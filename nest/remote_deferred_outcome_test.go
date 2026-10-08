package nest

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/entity"
	"github.com/tjbdwanghaibo/roost-core/fctx"
	"github.com/tjbdwanghaibo/roost-core/sync/entitysync"
)

// outcomeDeferringBatch 模拟 remoteentity 在 strict 确认超时后的批次：Commit 返回“结果未知”，
// 批次接手提交后回调，finalizer 拿到持久结论后调用一次。
type outcomeDeferringBatch struct {
	stagedRemoteBatch
	mu      sync.Mutex
	outcome func(bool)
	ids     []int64
}

// EntityIDs 是批次里的 Remote 实体（被拒绝时只丢弃它们的 Sync 内容与事实，RR-20260926-58）。
func (b *outcomeDeferringBatch) EntityIDs() []int64 { return append([]int64(nil), b.ids...) }

func (b *outcomeDeferringBatch) Commit(context.Context) ([]entity.RemoteCommitReceipt, error) {
	return nil, fmt.Errorf("%w: %w", entity.ErrRemotePersistenceIndeterminate, context.DeadlineExceeded)
}

func (b *outcomeDeferringBatch) DeferUntilDurableOutcome(fn func(bool)) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.outcome = fn
	return true
}

func (b *outcomeDeferringBatch) takeOutcome() func(bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	fn := b.outcome
	b.outcome = nil
	return fn
}

// RR-20260926-37：本地已持久提交、strict 远端确认超时时，Sync Confirm 与 AfterCommit 不能丢，也不能在没有持久结论时执行。
// 它们随批次交给 finalizer：Committed 时经 Nest 本地执行入口在快池执行一次；Rejected 时丢弃 Sync 门（不阻塞后续提交、
// 不执行 AfterCommit）；停机排空拿不到结论时门保持冻结。REPRO-2026-09-26-04 §5 探针改写。
func TestStrictRemoteConfirmTimeoutDefersPostCommitToDurableOutcome(t *testing.T) {
	for _, mode := range []entitysync.SyncMode{entitysync.ModePeriodic, entitysync.ModeOnChange} {
		for _, conclusion := range []string{"committed", "rejected", "none"} {
			t.Run(mode.String()+"/"+conclusion, func(t *testing.T) {
				unique := int64(9960)
				switch conclusion {
				case "rejected":
					unique = 9964
				case "none":
					unique = 9968
				}
				if mode == entitysync.ModeOnChange {
					unique += 20
				}
				localID, e := newAsyncPilotEntity(t, unique, 10)
				m, frames, _ := syncTestManager(t, e, mode)
				getter := newMockGetter()
				remoteID := mustBuildCastID(t, unique+1, entity.EntityCategoryRemote, nestRemoteManagedKind)
				getter.Add(newMockEntity(remoteID, entity.EntityCategoryRemote))
				getter.Add(e)
				first := &outcomeDeferringBatch{ids: []int64{remoteID}}
				useFirst := true
				manager := &bindingRemoteManager{stagedRemoteManager: stagedRemoteManager{prepare: func(context.Context) (entity.RemoteWriteBatch, error) {
					if useFirst {
						return first, nil
					}
					return &stagedRemoteBatch{}, nil
				}}}
				mgr := NewEngine(NestOptionWithGetter(getter), NestOptionWithRemoteEntityManager(manager), NestOptionWithEntitySync(m), NestOptionWithWorkerPools(WorkerPoolConfig{Workers: 1, QueueCap: 16}, WorkerPoolConfig{}))
				name := NewHandlerName(fmt.Sprintf("strict_confirm_outcome_%d", unique))
				var hookMu sync.Mutex
				afterCommit, afterCommitOnFast := 0, 0
				mgr.MustRegisterHandlerWithMeta(name, func([]entity.IThreadSafeEntity, []any, ...HandlerOption) (any, error) {
					e.dao.Value++
					e.MarkSyncDirty(1)
					AfterCommit(func() {
						hookMu.Lock()
						afterCommit++
						if fctx.InFastWorker() {
							afterCommitOnFast++
						}
						hookMu.Unlock()
					})
					return "ok", nil
				}, HandlerMeta{Rollback: RollbackNone, Durability: DurabilityMemory})
				if err := m.Start(context.Background()); err != nil {
					t.Fatal(err)
				}
				if err := mgr.Start(); err != nil {
					t.Fatal(err)
				}
				defer mgr.Shutdown(context.Background())
				counts := func() (int, int) {
					hookMu.Lock()
					defer hookMu.Unlock()
					return afterCommit, afterCommitOnFast
				}
				send := func() any {
					msg, ch := GenSyncMsg(MsgTypeMulti)
					msg.Name = name.String()
					msg.Tids = []int64{remoteID, localID}
					msg.Cost = true
					msg.HasRemote = true
					if err := mgr.dispatcher.TrySendMsg(msg); err != nil {
						t.Fatal(err)
					}
					return stagedWait(t, ch)
				}
				if reply, ok := send().(error); !ok || reply == nil {
					t.Fatalf("tx1 reply=%v, want the unknown remote confirmation", reply)
				}
				if e.Sync().SyncCommitReady() {
					t.Fatal("Sync released without a durable remote conclusion")
				}
				if n, _ := counts(); n != 0 {
					t.Fatalf("AfterCommit ran %d time(s) without a durable remote conclusion", n)
				}
				outcome := first.takeOutcome()
				if outcome == nil {
					t.Fatalf("post-commit work (Sync Confirm, AfterCommit) was not handed to the remote outcome: local entity %d stays frozen forever", localID)
				}
				run := manager.localExecutor()
				if run == nil {
					t.Fatal("Nest did not bind its local runtime")
				}
				switch conclusion {
				case "committed":
					// finalizer 拿到 Applied/Committed：经 Nest 本地执行入口调用一次（非快 worker 发起）。
					done := make(chan error, 1)
					go func() { done <- run(func() { outcome(true) }) }()
					if err := <-done; err != nil {
						t.Fatal(err)
					}
				case "rejected":
					done := make(chan error, 1)
					go func() { done <- run(func() { outcome(false) }) }()
					if err := <-done; err != nil {
						t.Fatal(err)
					}
				}
				n, onFast := counts()
				switch conclusion {
				case "committed":
					if n != 1 || onFast != 1 {
						t.Fatalf("AfterCommit after durable commit: ran=%d onFast=%d, want exactly once on a fast worker", n, onFast)
					}
					if !e.Sync().SyncCommitReady() {
						t.Fatal("Sync not confirmed after the durable commit")
					}
				default:
					if n != 0 {
						t.Fatalf("AfterCommit ran %d time(s) for a transaction without a durable commit", n)
					}
				}
				useFirst = false
				if reply := send(); reply != "ok" {
					t.Fatalf("tx2 reply=%v", reply)
				}
				if conclusion == "none" {
					if e.Sync().SyncCommitReady() {
						t.Fatal("Sync gate released during shutdown drain without a durable conclusion")
					}
					return
				}
				if !e.Sync().SyncCommitReady() {
					t.Fatalf("later transaction still frozen behind tx1 (%s)", conclusion)
				}
				// on_change 按提交逐笔冻结：先交付 tx1 冻结的 11（本地内存值），再交付 tx2 的 12；periodic 直接交付最新值。
				want := byte(e.dao.Value)
				deadline := time.After(2 * time.Second)
				for delivered := false; !delivered; {
					_ = m.Flush(context.Background())
					select {
					case raw := <-frames:
						delivered = syncValue(t, raw) == want
					case <-deadline:
						t.Fatalf("value %d never delivered after a later successful transaction (%s)", want, conclusion)
					}
				}
			})
		}
	}
}

// SyncMutation.Reject：已准入提交的持久结论为拒绝时，门不再阻塞后续提交，本提交的兴趣事实按丢弃处理，
// 后续提交的事实不受影响（RR-20260926-37）。
func TestSyncMutationRejectReleasesLaterCommits(t *testing.T) {
	_, e := newAsyncPilotEntity(t, 9990, 10)
	m, _, _ := syncTestManager(t, e, entitysync.ModePeriodic)
	es := []entity.IThreadSafeEntity{e}
	first := entity.BeginSyncMutation(es, m)
	firstFacts := e.Sync().SyncCommitCondition()
	e.MarkSyncDirty(1)
	first.Admit()
	first.Release()
	first.Finish(false)
	second := entity.BeginSyncMutation(es, m)
	secondFacts := e.Sync().SyncCommitCondition()
	e.MarkSyncDirty(1)
	second.Admit()
	second.Release()
	second.Confirm()
	second.Finish(false)
	if e.Sync().SyncCommitReady() {
		t.Fatal("second commit passed the unconfirmed first gate")
	}
	first.Reject()
	if !e.Sync().SyncCommitReady() {
		t.Fatal("rejected first commit still blocks the second")
	}
	if ready, discarded := firstFacts(); ready || !discarded {
		t.Fatalf("facts of the rejected commit ready=%v discarded=%v, want dropped", ready, discarded)
	}
	if ready, discarded := secondFacts(); !ready || discarded {
		t.Fatalf("facts of the later commit ready=%v discarded=%v, want ready", ready, discarded)
	}
	third := entity.BeginSyncMutation(es, m)
	thirdFacts := e.Sync().SyncCommitCondition()
	third.Admit()
	third.Release()
	third.Confirm()
	third.Finish(false)
	if ready, discarded := thirdFacts(); !ready || discarded {
		t.Fatalf("a commit that began after the rejection ready=%v discarded=%v, want ready", ready, discarded)
	}
}
