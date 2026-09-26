package remoteentity

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/entity"
	"github.com/tjbdwanghaibo/roost-core/fctx"
	"github.com/tjbdwanghaibo/roost-core/nest"
	"github.com/tjbdwanghaibo/roost-core/sync/entitysync"
)

// RR-20260926-37 跨层：真实 Nest + 正式 Remote Manager（MongoCommitter 权威、ManagerAccess loader）。Durability 0 的
// Remote 写结果未知时，请求带“结果未知”错误回复；提交后工作经 Nest → remoteWriteBatch → finalizer 交接，
// 拿到持久结论后执行一次：回复丢失但已写入 → AfterCommit 在快池执行一次、Sync 放行；从未到达 → 持久拒绝、卸载、
// AfterCommit 不执行。
func TestNestDeferredPostCommitFollowsFinalizerOutcome(t *testing.T) {
	for _, tc := range []struct {
		name      string
		rawID     int64
		committed bool
	}{
		{name: "lost_reply_commits", rawID: 1971, committed: true},
		{name: "never_reached_is_rejected", rawID: 1972},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, live := newReloadFixture(t, tc.rawID)
			steps := make(chan string, 16)
			f.mgr.remote.finalizeTrace = func(_ entity.RemoteTransactionID, step string) { steps <- step }
			syncMgr, err := entitysync.NewManager(entitysync.ManagerConfig{Mode: entitysync.ModePeriodic, Interval: time.Hour, Transport: entitysync.TransportFunc(func(context.Context, entitysync.SessionID, []byte) error { return nil })})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = syncMgr.Close(context.Background()) })
			packed := func(entity.SyncProfile) (entity.FrozenSyncPayload, error) {
				return entity.CopyFrozenSyncPayload(1, []byte("v")), nil
			}
			live.EnableSync(entity.EntitySyncCreateParam{Enabled: true, EntityID: live.ID(), Namespace: "test", Packer: entity.SubjectSyncPackFunc{Snapshot: packed, Delta: func(p entity.SyncProfile, _ uint64) (entity.FrozenSyncPayload, error) { return packed(p) }}})
			if err = syncMgr.Register(live.Sync()); err != nil {
				t.Fatal(err)
			}
			engine := nest.NewEngine(nest.NestOptionWithGetter(f.access), nest.NestOptionWithRemoteEntityManager(f.mgr), nest.NestOptionWithEntitySync(syncMgr), nest.NestOptionWithWorkerNumAndMsgCap(1, 1, 16))
			var afterCommit, afterCommitOnFast atomic.Int32
			name := nest.NewHandlerName("deferred_outcome_" + tc.name)
			engine.MustRegisterHandlerWithMeta(name, func(_ []entity.IThreadSafeEntity, _ []any, _ ...nest.HandlerOption) (any, error) {
				live.set("rejected", "")
				live.MarkSyncDirty(1)
				nest.AfterCommit(func() {
					afterCommit.Add(1)
					if fctx.InFastWorker() {
						afterCommitOnFast.Add(1)
					}
				})
				return "ok", nil
			}, nest.HandlerMeta{Rollback: nest.RollbackNone, Durability: nest.DurabilityMemory})
			if err = engine.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = engine.Shutdown(context.Background()) })
			if tc.committed {
				f.storage.loseReply.Store(true)
			} else {
				f.storage.unreachable.Store(true)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			reply, err := engine.Request(ctx, name, live.ID(), nil)
			if !errors.Is(err, entity.ErrRemotePersistenceIndeterminate) || errors.Is(err, nest.ErrAfterCommitFailed) {
				t.Fatalf("reply=%v err=%v, want an unknown outcome without the committed sentinel", reply, err)
			}
			select {
			case step := <-steps:
				if step != "released" {
					t.Fatalf("finalizer step=%s, want released", step)
				}
			case <-ctx.Done():
				t.Fatal("finalizer did not conclude the transaction")
			}
			if tc.committed {
				if got, fast := afterCommit.Load(), afterCommitOnFast.Load(); got != 1 || fast != 1 {
					t.Fatalf("AfterCommit after the durable commit ran=%d onFast=%d, want exactly once on a fast worker", got, fast)
				}
				if !live.Sync().SyncCommitReady() {
					t.Fatal("Sync still frozen after the durable commit")
				}
				if doc := f.storedDocument(t); doc != "a=rejected;b=" {
					t.Fatalf("Mongo document=%q, want the committed write", doc)
				}
				return
			}
			if got := afterCommit.Load(); got != 0 {
				t.Fatalf("AfterCommit ran %d time(s) for a rejected transaction", got)
			}
			if !live.IsRemoved() {
				t.Fatal("rejected instance was not unloaded")
			}
			if doc := f.storedDocument(t); doc != "" {
				t.Fatalf("rejected transaction reached Mongo: %q", doc)
			}
		})
	}
}
