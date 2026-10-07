package nest

import (
	"context"
	"sync"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/entity"
)

// RR-20260930-14（REMAINING §3 N28，维护者 2026-09-30 拍板）：广播路径按实例释放，并且一个目标结束时释放它取得的全部锁。
// 广播 handler 里 Destroy 目标 X 再新建同 ID 的 X：旧实例被广播自己 Touch 着（ID 不清零），新实例由 handler 内的 Create 在同一 Guard
// 上取锁。旧行为：broadcastDispatch 用一把跨全部目标的 Guard、目标结束时按 ID 释放——放掉的是新实例的锁，旧实例回到 eMap，
// 它的锁一直持有到整轮广播结束，后续目标的 handler 期间别的 goroutine 锁不到旧 X（等它的等待者被拖到广播结束）。承诺：X 这个目标
// 结束后（下一个目标的 handler 里观察）旧 X 与新 X 的锁都已释放；广播结束后新 X 是已发布实例且两把锁都空闲。
func TestBroadcastReleasesDestroyedAndRecreatedLocksPerTarget(t *testing.T) {
	for i, tc := range []struct {
		name string
		meta HandlerMeta
	}{
		{"memory", HandlerMeta{}},
		{"state_strict", HandlerMeta{Rollback: RollbackState, Durability: DurabilityStrict}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manager := entity.NewEntityManager()
			pilots := addPilots(t, manager, 49800+int64(i)*10, 1)
			access := entity.NewManagerAccess(manager)
			x := mustBuildCastID(t, 49805+int64(i)*10, entity.EntityCategory(1), createdInScopeKind)
			old, err := access.Create(createParam(x))
			if err != nil {
				t.Fatal(err)
			}
			mgr := NewEngine(NestOptionWithGetter(access), NestOptionWithTransactionCommitter(&recordingCommitter{}), NestOptionWithWorkerNumAndMsgCap(1, 16))
			name := NewHandlerName("rr14_broadcast_destroy_recreate_" + tc.name)
			type observed struct {
				fresh                entity.IThreadSafeEntity
				createErr            error
				freshLockedInHandler bool // X 目标内：新 X 被本 Guard 持有
				oldFreeInNextTarget  bool // Y 目标内：旧 X 的锁已释放
				freshFreeInNext      bool // Y 目标内：新 X 的锁已释放
				targets              []int64
			}
			var (
				mu  sync.Mutex
				got observed
			)
			mgr.MustRegisterHandlerWithMeta(name, func(es []entity.IThreadSafeEntity, _ []any, _ ...HandlerOption) (any, error) {
				mu.Lock()
				defer mu.Unlock()
				got.targets = append(got.targets, es[0].GUId())
				if es[0] == old {
					if err := access.Destroy(context.Background(), es[0], entity.EntityDestroyReason(0), false); err != nil {
						return nil, err
					}
					got.fresh, got.createErr = access.Create(createParam(x))
					if got.createErr != nil {
						return nil, got.createErr
					}
					got.freshLockedInHandler = !tryLockElsewhere(got.fresh)
					if err := MarkPersist(got.fresh.(*rollbackTestEntity).dao, 1); err != nil && tc.meta.Rollback != RollbackNone {
						return nil, err
					}
					return "recreated", nil
				}
				// 后续目标：X 目标已经结束，它取得的锁都应已释放。
				got.oldFreeInNextTarget = tryLockElsewhere(old)
				got.freshFreeInNext = got.fresh != nil && tryLockElsewhere(got.fresh)
				return "observed", nil
			}, tc.meta)
			if err := mgr.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = mgr.Shutdown(context.Background()) }()
			msg, ch := GenSyncMsg(MsgTypeBroadcast)
			msg.Name, msg.Tids = name.String(), []int64{x, pilots[0]}
			if err := mgr.dispatcher.TrySendMsg(msg); err != nil {
				t.Fatal(err)
			}
			reply := stagedWait(t, ch)
			mu.Lock()
			defer mu.Unlock()
			t.Logf("reply=%v targets=%v %+v", reply, got.targets, got)
			if err, _ := reply.(error); err != nil || got.createErr != nil || len(got.targets) != 2 || got.targets[0] != x {
				t.Fatalf("fixture: reply=%v createErr=%v targets=%v (want X first, then the pilot)", reply, got.createErr, got.targets)
			}
			if !got.freshLockedInHandler {
				t.Fatal("fixture: the re-created X must be held by the broadcast target's guard while its handler runs")
			}
			if !got.oldFreeInNextTarget {
				t.Fatal("the destroyed X's lock was still held while the next broadcast target ran: the broadcast released by id and kept the superseded instance until the whole broadcast ended")
			}
			if !got.freshFreeInNext {
				t.Fatal("the re-created X's lock was still held while the next broadcast target ran")
			}
			if published := manager.Get(x); published != got.fresh {
				t.Fatalf("published=%p want the re-created instance %p", published, got.fresh)
			}
			if !tryLockElsewhere(old) || !tryLockElsewhere(got.fresh) {
				t.Fatalf("locks leaked after the broadcast: old free=%v fresh free=%v", tryLockElsewhere(old), tryLockElsewhere(got.fresh))
			}
		})
	}
}

// RR-20260930-14 补的 Remote 回归（RR-20260927-26 §未验证项）：声明的 remote-managed 目标由 prepareRemoteWriteBatch 准备、
// 随消息自己的事务提交 / 撤销，不经 Cast 取锁。带 Remote 批次的消息里 handler 对它调用 ReleaseCast 是空操作（事务上下文内保留锁，
// 与 ReleaseCast 的既有契约一致）：锁保持到消息结束，由 Guard 作用域统一释放。修前修后行为相同，这里只是钉住。
func TestReleaseCastOnDeclaredRemoteTargetIsNoopUntilMessageEnds(t *testing.T) {
	localID, e := newAsyncPilotEntity(t, 49830, 10)
	getter := newMockGetter()
	remoteID := mustBuildCastID(t, 49831, entity.EntityCategoryRemote, nestRemoteManagedKind)
	remote := newMockEntity(remoteID, entity.EntityCategoryRemote)
	getter.Add(remote)
	getter.Add(e)
	recording := &recordingRemoteBatch{}
	manager := stagedRemoteManager{prepare: func(context.Context) (entity.RemoteWriteBatch, error) { return recording, nil }}
	mgr := NewEngine(NestOptionWithGetter(getter), NestOptionWithRemoteEntityManager(manager), NestOptionWithTransactionCommitter(&recordsCommitter{}), NestOptionWithWorkerNumAndMsgCap(1, 16))
	name := NewHandlerName("rr14_release_cast_declared_remote")
	type observed struct {
		heldBefore, heldAfter, freeAfter bool
	}
	var got observed
	mgr.MustRegisterHandlerWithMeta(name, func(es []entity.IThreadSafeEntity, _ []any, _ ...HandlerOption) (any, error) {
		guard := entity.GetEntityGuard()
		got.heldBefore = guard.GuardedEntity(remote) && !tryLockElsewhere(remote)
		ReleaseCast(remote)
		got.heldAfter = guard.GuardedEntity(remote)
		got.freeAfter = tryLockElsewhere(remote)
		return "ok", nil
	}, HandlerMeta{Rollback: RollbackNone, Durability: DurabilityMemory})
	if err := mgr.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = mgr.Shutdown(context.Background()) }()
	msg, ch := GenSyncMsg(MsgTypeMulti)
	msg.Name, msg.Tids, msg.Cost, msg.HasRemote = name.String(), []int64{remoteID, localID}, true, true
	if err := mgr.dispatcher.TrySendMsg(msg); err != nil {
		t.Fatal(err)
	}
	reply := stagedWait(t, ch)
	t.Logf("reply=%v %+v", reply, got)
	if err, _ := reply.(error); err != nil {
		t.Fatalf("reply=%v", reply)
	}
	if !got.heldBefore {
		t.Fatal("fixture: the declared remote target must be held by the dispatch guard when the handler starts")
	}
	if !got.heldAfter || got.freeAfter {
		t.Fatalf("ReleaseCast on a declared remote target inside a remote-batch message must be a no-op: held=%v free=%v", got.heldAfter, got.freeAfter)
	}
	if !tryLockElsewhere(remote) {
		t.Fatal("the declared remote target is still locked after the message")
	}
	if c := recording.commits.Load(); c != 1 {
		t.Fatalf("Remote batch commits=%d, want 1", c)
	}
}
