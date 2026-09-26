package nest

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/entity"
	"github.com/tjbdwanghaibo/roost-core/sync/entitysync"
)

// RR-20260926-58：本地 + Remote 混合事务，本地部分已持久提交、Remote 确认无结论，之后权威持久拒绝 Remote 部分。
// Nest 交给批次的提交后工作只能丢弃批次内 Remote 实体的 Sync 事实与内容；本地实体的提交事实与冻结内容照常生效，
// 不需要等下一笔提交才恢复。AfterCommit 仍不执行（结论为拒绝）。
func TestRemoteRejectKeepsCommittedLocalEntitySync(t *testing.T) {
	for _, mode := range []entitysync.SyncMode{entitysync.ModePeriodic, entitysync.ModeOnChange} {
		t.Run(mode.String(), func(t *testing.T) {
			unique := int64(9940)
			if mode == entitysync.ModeOnChange {
				unique = 9950
			}
			localID, local := newAsyncPilotEntity(t, unique, 10)
			m, frames, _ := syncTestManager(t, local, mode)
			getter := newMockGetter()
			remoteID := mustBuildCastID(t, unique+1, entity.EntityCategoryRemote, nestRemoteManagedKind)
			remote := newMockEntity(remoteID, entity.EntityCategoryRemote)
			packed := func(entity.SyncProfile) (entity.FrozenSyncPayload, error) {
				return entity.CopyFrozenSyncPayload(1, []byte{0xEE}), nil
			}
			remote.EnableSync(entity.EntitySyncCreateParam{Enabled: true, EntityID: remoteID, Namespace: "test", Packer: entity.SubjectSyncPackFunc{Snapshot: packed, Delta: func(p entity.SyncProfile, _ uint64) (entity.FrozenSyncPayload, error) { return packed(p) }}})
			getter.Add(remote)
			getter.Add(local)
			batch := &outcomeDeferringBatch{ids: []int64{remoteID}}
			manager := &bindingRemoteManager{stagedRemoteManager: stagedRemoteManager{prepare: func(context.Context) (entity.RemoteWriteBatch, error) {
				return batch, nil
			}}}
			mgr := NewEngine(NestOptionWithGetter(getter), NestOptionWithRemoteEntityManager(manager), NestOptionWithEntitySync(m), NestOptionWithWorkerNumAndMsgCap(1, 1, 16))
			name := NewHandlerName(fmt.Sprintf("mixed_remote_reject_%d", unique))
			var localFacts, remoteFacts func() (bool, bool)
			afterCommit := 0
			mgr.MustRegisterHandlerWithMeta(name, func([]entity.IThreadSafeEntity, []any, ...HandlerOption) (any, error) {
				local.dao.Value++
				local.MarkSyncDirty(1)
				remote.MarkSyncDirty(1)
				// 与 Interest 事实队列相同的入口：锁内固定本提交的条件。
				localFacts = entity.SyncConditionFor(local)
				remoteFacts = entity.SyncConditionFor(remote)
				AfterCommit(func() { afterCommit++ })
				return "ok", nil
			}, HandlerMeta{Rollback: RollbackNone, Durability: DurabilityMemory})
			if err := m.Start(context.Background()); err != nil {
				t.Fatal(err)
			}
			if err := mgr.Start(); err != nil {
				t.Fatal(err)
			}
			defer mgr.Shutdown(context.Background())
			msg, ch := GenSyncMsg(MsgTypeMulti)
			msg.Name = name.String()
			msg.Tids = []int64{remoteID, localID}
			msg.Cost = true
			msg.HasRemote = true
			if err := mgr.dispatcher.TrySendMsg(msg); err != nil {
				t.Fatal(err)
			}
			if reply, ok := stagedWait(t, ch).(error); !ok || reply == nil {
				t.Fatalf("reply=%v, want the unknown remote confirmation", reply)
			}
			outcome := batch.takeOutcome()
			if outcome == nil {
				t.Fatal("post-commit work was not handed to the remote outcome")
			}
			run := manager.localExecutor()
			done := make(chan error, 1)
			go func() { done <- run(func() { outcome(false) }) }()
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if ready, discarded := localFacts(); !ready || discarded {
				t.Fatalf("durably committed local entity %d: facts ready=%v discarded=%v after the Remote part was rejected, want ready (only the rejected Remote entity's facts may be dropped)", localID, ready, discarded)
			}
			if ready, discarded := remoteFacts(); ready || !discarded {
				t.Fatalf("rejected Remote entity %d: facts ready=%v discarded=%v, want discarded", remoteID, ready, discarded)
			}
			if !local.Sync().SyncCommitReady() || !remote.Sync().SyncCommitReady() {
				t.Fatalf("gates still frozen after the rejection: local=%v remote=%v", local.Sync().SyncCommitReady(), remote.Sync().SyncCommitReady())
			}
			if afterCommit != 0 {
				t.Fatalf("AfterCommit ran %d time(s) for a rejected transaction", afterCommit)
			}
			// 本地实体已提交的内容（11）直接交付，不需要等下一笔提交。
			deadline := time.After(2 * time.Second)
			for delivered := false; !delivered; {
				_ = m.Flush(context.Background())
				select {
				case raw := <-frames:
					delivered = syncValue(t, raw) == 11
				case <-deadline:
					t.Fatal("committed local content 11 never delivered after the Remote rejection")
				}
			}
		})
	}
}
