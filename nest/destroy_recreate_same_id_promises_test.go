package nest

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/entity"
)

// RR-20260926-67：handler 内先 Destroy 声明目标 X，再新建同 ID 的 X。Destroy 在 handler 中途从 LockManager 摘掉了 X 的锁，
// 新实例拿到新的 mutex；Guard 以实例判断是否已持有，新实例按 RR-48 的锁序规则取锁（同组 → try-lock，新锁无人持有即成功），
// 持锁到 handler 结束并纳入本次提交边界；回滚时新实例按 RR-35 撤销发布，旧实例的销毁不回滚（Destroy 本就不是事务操作）。

type destroyRecreateRun struct {
	old, fresh             entity.IThreadSafeEntity
	createErr              error
	sameMutex              bool
	freshLockableElsewhere bool
}

func registerDestroyThenRecreate(t *testing.T, mgr *NestMgr, access *entity.ManagerAccess, name HandlerName, meta HandlerMeta, x int64, run *destroyRecreateRun, fail error) {
	t.Helper()
	mgr.MustRegisterHandlerWithMeta(name, func(es []entity.IThreadSafeEntity, _ []any, _ ...HandlerOption) (any, error) {
		if err := access.Destroy(context.Background(), es[0], entity.EntityDestroyReason(0), false); err != nil {
			return nil, err
		}
		run.fresh, run.createErr = access.Create(createParam(x))
		if run.createErr != nil {
			return nil, run.createErr
		}
		run.sameMutex = run.fresh.GetMutex() == run.old.GetMutex()
		run.freshLockableElsewhere = tryLockElsewhere(run.fresh)
		if err := MarkPersist(run.fresh.(*rollbackTestEntity).dao, 1); err != nil && meta.Rollback != RollbackNone {
			return nil, err
		}
		return "ok", fail
	}, meta)
}

// REPRO-2026-09-26-06 §3 P-D：修前 strict 与 memory 均 sameMutex=false freshLockableByOtherGoroutineMidHandler=true。
func TestDestroyThenRecreateSameIDInHandlerLocksNewInstance(t *testing.T) {
	for i, tc := range []struct {
		name string
		meta HandlerMeta
	}{
		{"state_strict", HandlerMeta{Rollback: RollbackState, Durability: DurabilityStrict}},
		{"memory", HandlerMeta{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manager := entity.NewEntityManager()
			access := entity.NewManagerAccess(manager)
			x := mustBuildCastID(t, 36705+int64(i)*10, entity.EntityCategory(1), createdInScopeKind)
			run := &destroyRecreateRun{}
			var err error
			if run.old, err = access.Create(createParam(x)); err != nil { // Nest 之外创建并发布
				t.Fatal(err)
			}
			committer := &recordingCommitter{}
			mgr := NewEngine(NestOptionWithGetter(access), NestOptionWithTransactionCommitter(committer), NestOptionWithWorkerPools(WorkerPoolConfig{Workers: 1, QueueCap: 16}, WorkerPoolConfig{}))
			name := NewHandlerName("rr67_destroy_recreate_" + tc.name)
			registerDestroyThenRecreate(t, mgr, access, name, tc.meta, x, run, nil)
			if err := mgr.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = mgr.Shutdown(context.Background()) }()
			ret, err := mgr.Request(context.Background(), name, x, nil)
			if err != nil || ret != "ok" || run.createErr != nil {
				t.Fatalf("ret=%v err=%v createErr=%v", ret, err, run.createErr)
			}
			if run.sameMutex {
				t.Fatal("fixture: Destroy must have released the old mutex from the LockManager")
			}
			if run.freshLockableElsewhere {
				t.Fatalf("%s: the re-created instance was published unlocked mid-handler (another goroutine could lock it): the guard treated it as held because the destroyed instance had the same id", tc.name)
			}
			if published := manager.Get(x); published != run.fresh {
				t.Fatalf("published=%p want the re-created instance %p", published, run.fresh)
			}
			if !tryLockElsewhere(run.fresh) || !tryLockElsewhere(run.old) {
				t.Fatalf("locks leaked after the handler: fresh free=%v old free=%v", tryLockElsewhere(run.fresh), tryLockElsewhere(run.old))
			}
			if tc.meta.Rollback != RollbackNone && len(committer.record.Mutations) == 0 {
				t.Fatalf("the re-created entity is outside the transaction's commit boundary: record=%+v", committer.record)
			}
		})
	}
}

// 回滚语义保持：state_strict handler 在 Destroy + 新建后报错——新实例按 RR-35 撤销发布（摘除、removed、不持久），
// 旧实例的销毁不回滚；两把锁都释放，同一 ID 之后可以在 Nest 之外重新创建。
func TestDestroyThenRecreateSameIDInHandlerRollsBack(t *testing.T) {
	manager := entity.NewEntityManager()
	access := entity.NewManagerAccess(manager)
	x := mustBuildCastID(t, 36725, entity.EntityCategory(1), createdInScopeKind)
	run := &destroyRecreateRun{}
	var err error
	if run.old, err = access.Create(createParam(x)); err != nil {
		t.Fatal(err)
	}
	committer := &recordingCommitter{}
	mgr := NewEngine(NestOptionWithGetter(access), NestOptionWithTransactionCommitter(committer), NestOptionWithWorkerPools(WorkerPoolConfig{Workers: 1, QueueCap: 16}, WorkerPoolConfig{}))
	name := NewHandlerName("rr67_destroy_recreate_rollback")
	boom := errors.New("business failed after re-creating")
	registerDestroyThenRecreate(t, mgr, access, name, HandlerMeta{Rollback: RollbackState, Durability: DurabilityStrict}, x, run, boom)
	if err := mgr.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = mgr.Shutdown(context.Background()) }()
	if _, err := mgr.Request(context.Background(), name, x, nil); !errors.Is(err, boom) {
		t.Fatalf("err=%v, want the business error", err)
	}
	if run.freshLockableElsewhere {
		t.Fatal("the re-created instance was unlocked mid-handler")
	}
	if manager.Get(x) != nil || !run.fresh.IsRemoved() || !run.old.IsRemoved() {
		t.Fatalf("after rollback: published=%v fresh removed=%v old removed=%v; want nothing published", manager.Get(x), run.fresh.IsRemoved(), run.old.IsRemoved())
	}
	if len(committer.record.Mutations) != 0 {
		t.Fatalf("a rolled-back transaction reached the committer: %+v", committer.record)
	}
	if !tryLockElsewhere(run.fresh) || !tryLockElsewhere(run.old) {
		t.Fatal("locks leaked after the rolled-back handler")
	}
	again, err := access.Create(createParam(x))
	if err != nil || manager.Get(x) != again {
		t.Fatalf("re-create after rollback: %v", err)
	}
}

// handler 内 Destroy 声明目标 X 后，Nest 之外有人重建了 X（新锁、立即发布并解锁），handler 再 Cast X：修前 Cast 按 ID
// 认定“已持有”直接返回未加锁的新实例；修后按实例判断，同组新实例不满足锁序，返回 ErrCastDeadlockRisk。
func TestCastAfterDestroyDoesNotReturnUnlockedRecreatedInstance(t *testing.T) {
	manager := entity.NewEntityManager()
	access := entity.NewManagerAccess(manager)
	x := mustBuildCastID(t, 36735, entity.EntityCategory(1), createdInScopeKind)
	if _, err := access.Create(createParam(x)); err != nil {
		t.Fatal(err)
	}
	mgr := NewEngine(NestOptionWithGetter(access), NestOptionWithTransactionCommitter(&recordingCommitter{}), NestOptionWithWorkerPools(WorkerPoolConfig{Workers: 1, QueueCap: 16}, WorkerPoolConfig{}))
	name := NewHandlerName("rr67_cast_after_destroy")
	var castErr error
	var castLockableElsewhere bool
	mgr.MustRegisterHandlerWithMeta(name, func(es []entity.IThreadSafeEntity, _ []any, _ ...HandlerOption) (any, error) {
		if err := access.Destroy(context.Background(), es[0], entity.EntityDestroyReason(0), false); err != nil {
			return nil, err
		}
		recreated := make(chan error, 1)
		go func() { // Nest 之外、另一个 goroutine
			_, err := access.Create(createParam(x))
			recreated <- err
		}()
		if err := <-recreated; err != nil {
			return nil, err
		}
		var cast *rollbackTestEntity
		cast, castErr = CastOne[*rollbackTestEntity](x)
		if castErr == nil {
			castLockableElsewhere = tryLockElsewhere(cast)
		}
		return "ok", nil
	}, HandlerMeta{Rollback: RollbackState, Durability: DurabilityStrict})
	if err := mgr.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = mgr.Shutdown(context.Background()) }()
	if _, err := mgr.Request(context.Background(), name, x, nil); err != nil {
		t.Fatal(err)
	}
	if castErr == nil {
		t.Fatalf("Cast returned the re-created instance (unlocked by this handler: lockable elsewhere=%v); want ErrCastDeadlockRisk", castLockableElsewhere)
	}
	if !errors.Is(castErr, ErrCastDeadlockRisk) {
		t.Fatalf("Cast err=%v, want ErrCastDeadlockRisk", castErr)
	}
}

// remoteRecreatableKind 是带真实 builder 的 remote-managed kind（nestRemoteManagedKind 的 builder 返回 nil，不能重建）。
const remoteRecreatableKind entity.EntityKind = 252

type remoteRecreatableEntity struct {
	*entity.RemoteEntityBase
}

func (e *remoteRecreatableEntity) Base() *entity.EntityBase { return &e.RemoteEntityBase.EntityBase }

func init() {
	entity.RegisterEntityBuilder(&entity.EntityBuilderParam{
		Category:     entity.EntityCategoryRemote,
		Kind:         remoteRecreatableKind,
		RemotePolicy: entity.RemotePolicyManaged,
		NoPersist:    true,
		Lifetime:     entity.EntityLifetimeRemoteManaged,
		Builder: func(p *entity.EntityCreateParam) (entity.IThreadSafeEntity, error) {
			return &remoteRecreatableEntity{RemoteEntityBase: entity.NewRemoteEntityBaseWithMutex(p.Id, p.Category, false, p.Mutex, p.Kind)}, nil
		},
	})
}

// OPEN-ITEMS B17：RR-67 对 Remote 实体同样成立。声明目标是 remote-managed 实体 X（慢阶段经受控批次准备，快续行执行 memory handler），
// handler 内 Destroy X 再新建同 ID 的 X：新实例持锁到 handler 结束（执行中别的 goroutine 取不到它的锁）、成为发布的实例，
// Remote 批次照常 Commit，handler 结束后新旧两把锁都释放。
func TestDestroyThenRecreateSameIDRemoteEntityLocksNewInstance(t *testing.T) {
	manager := entity.NewEntityManager()
	access := entity.NewManagerAccess(manager)
	x := mustBuildCastID(t, 36745, entity.EntityCategoryRemote, remoteRecreatableKind)
	remoteParam := func() *entity.EntityCreateParam {
		return &entity.EntityCreateParam{IsCreate: true, Id: x, Category: entity.EntityCategoryRemote, Kind: remoteRecreatableKind}
	}
	run := &destroyRecreateRun{}
	var err error
	if run.old, err = access.Create(remoteParam()); err != nil { // Nest 之外创建并发布
		t.Fatal(err)
	}
	var batchCommits atomic.Int32
	batch := &stagedRemoteBatch{commit: func() { batchCommits.Add(1) }}
	remote := stagedRemoteManager{prepare: func(context.Context) (entity.RemoteWriteBatch, error) { return batch, nil }}
	mgr := NewEngine(NestOptionWithGetter(access), NestOptionWithRemoteEntityManager(remote), NestOptionWithTransactionCommitter(&recordingCommitter{}), NestOptionWithWorkerPools(WorkerPoolConfig{Workers: 1, QueueCap: 16}, WorkerPoolConfig{}))
	name := NewHandlerName("b17_remote_destroy_recreate")
	var remoteBatch bool
	mgr.MustRegisterHandlerWithMeta(name, func(es []entity.IThreadSafeEntity, _ []any, _ ...HandlerOption) (any, error) {
		remoteBatch = currentNestDispatchMsg() != nil && currentNestDispatchMsg().RemoteWriteBatch != nil
		if err := access.Destroy(context.Background(), es[0], entity.EntityDestroyReason(0), false); err != nil {
			return nil, err
		}
		run.fresh, run.createErr = access.Create(remoteParam())
		if run.createErr != nil {
			return nil, run.createErr
		}
		run.sameMutex = run.fresh.GetMutex() == run.old.GetMutex()
		run.freshLockableElsewhere = tryLockElsewhere(run.fresh)
		return "ok", nil
	}, HandlerMeta{Rollback: RollbackNone, Durability: DurabilityMemory})
	if err := mgr.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = mgr.Shutdown(context.Background()) }()
	msg, ch := GenSyncMsg(MsgTypeMulti)
	msg.Name, msg.Tids, msg.Cost, msg.HasRemote = name.String(), []int64{x}, true, true
	if err := mgr.dispatcher.TrySendMsg(msg); err != nil {
		t.Fatal(err)
	}
	var reply any
	select {
	case reply = <-ch:
	case <-time.After(10 * time.Second):
		t.Fatal("remote destroy/re-create handler did not reply within 10s")
	}
	if reply != "ok" || run.createErr != nil {
		t.Fatalf("reply=%v createErr=%v", reply, run.createErr)
	}
	if !remoteBatch || !entity.IsEntityKindRemoteManaged(run.fresh.GetEntityKind()) {
		t.Fatalf("premise: handler ran with a Remote batch=%v on a remote-managed entity=%v", remoteBatch, entity.IsEntityKindRemoteManaged(run.fresh.GetEntityKind()))
	}
	if run.sameMutex {
		t.Fatal("fixture: Destroy must have released the old mutex from the LockManager")
	}
	if run.freshLockableElsewhere {
		t.Fatal("the re-created Remote instance was published unlocked mid-handler (another goroutine could lock it)")
	}
	if published := manager.Get(x); published != run.fresh {
		t.Fatalf("published=%p want the re-created instance %p", published, run.fresh)
	}
	if batchCommits.Load() != 1 || batch.aborted.Load() || batch.closed.Load() != 1 {
		t.Fatalf("Remote batch commits=%d aborted=%v closed=%d: want one Commit and one Close", batchCommits.Load(), batch.aborted.Load(), batch.closed.Load())
	}
	if !tryLockElsewhere(run.fresh) || !tryLockElsewhere(run.old) {
		t.Fatalf("locks leaked after the handler: fresh free=%v old free=%v", tryLockElsewhere(run.fresh), tryLockElsewhere(run.old))
	}
}
