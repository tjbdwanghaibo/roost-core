package nest

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/entity"
)

// RR-20260926-64：不能回滚的 handler（RollbackNone / memory 快路径）内新建实体遇到按锁序不能等待的锁冲突时，
// 冲突前的内存修改不会被撤销，所以整条消息不能自动重排——否则这些修改重复生效。冲突以可 errors.Is 的
// ErrCreatedEntityLockConflict 交给调用方（不带 ErrLockTimeout：它表示“未执行、已重排”）；锁序允许时仍有序等待；
// 可回滚的事务保持 RR-20260926-48 的“整条回滚后重排”（见 create_lock_order_promises_test.go）。

// higherGroupCreatedKind 的锁组高于 pilot（category 1），用于“锁序允许时有序等待”。
const higherGroupCreatedKind entity.EntityKind = 238

func init() {
	entity.RegisterEntityBuilder(&entity.EntityBuilderParam{
		Category: castOtherCategory,
		Kind:     higherGroupCreatedKind,
		Builder: func(p *entity.EntityCreateParam) (entity.IThreadSafeEntity, error) {
			return &rollbackTestEntity{
				EntityBase: entity.NewEntityBaseWithMutex(p.Id, p.Category, false, p.Mutex, p.Kind),
				dao:        &rollbackTestDao{id: p.Id},
			}, nil
		},
	})
}

// REPRO-2026-09-26-06 §1 P-B：creator 新建 X 后停在 strict 提交里（持有 X 的锁）；follower 先改声明实体、再新建 X。
// 修前 follower 冲突后被重排：每次重排都再加一次声明实体（attempts=36… declared.Value=36）；修后只执行一次、
// 调用方拿到 ErrCreatedEntityLockConflict，不必等 creator 提交。
// RollbackNone 只能与 DurabilityMemory 组合（validateHandlerMeta），所以不能回滚的本地 handler 只有 memory 快路径；
// 探针里的 none_strict 组合在注册时即被拒绝。
func TestNonRollbackHandlerCreateConflictIsNotRequeued(t *testing.T) {
	for i, tc := range []struct {
		name string
		meta HandlerMeta
	}{
		{"memory", HandlerMeta{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manager := entity.NewEntityManager()
			ids := addPilots(t, manager, 36000+int64(i)*10, 2)
			declared := manager.Get(ids[1]).(*rollbackTestEntity)
			access := entity.NewManagerAccess(manager)
			x := mustBuildCastID(t, 36005+int64(i)*10, entity.EntityCategory(1), createdInScopeKind)
			committer := newBlockingCommitter()
			mgr := NewEngine(NestOptionWithGetter(access), NestOptionWithTransactionCommitter(committer), NestOptionWithWorkerPools(WorkerPoolConfig{Workers: 2, QueueCap: 16}, WorkerPoolConfig{}))
			creator := NewHandlerName("rr64_creator_" + tc.name)
			mgr.MustRegisterHandlerWithMeta(creator, func([]entity.IThreadSafeEntity, []any, ...HandlerOption) (any, error) {
				v, err := access.Create(createParam(x))
				if err != nil {
					return nil, err
				}
				return "created", MarkPersist(v.(*rollbackTestEntity).dao, 1)
			}, HandlerMeta{Rollback: RollbackState, Durability: DurabilityStrict})
			var attempts atomic.Int64
			var firstCreateErr atomic.Pointer[error]
			follower := NewHandlerName("rr64_follower_" + tc.name)
			mgr.MustRegisterHandlerWithMeta(follower, func(es []entity.IThreadSafeEntity, _ []any, _ ...HandlerOption) (any, error) {
				n := attempts.Add(1)
				es[0].(*rollbackTestEntity).dao.Value++ // 业务修改：这条 handler 不回滚，重做就会重复生效
				_, err := access.Create(createParam(x))
				if n == 1 {
					firstCreateErr.Store(&err)
				}
				if errors.Is(err, entity.ErrEntityExists) {
					return "exists", nil
				}
				return "created", err
			}, tc.meta)
			if err := mgr.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = mgr.Shutdown(context.Background()) }()
			out := make(chan requestResult, 2)
			sendRequest(mgr, "creator", creator, ids[0], out)
			<-committer.entered // creator 持有 X 的锁，停在 strict 提交里
			sendRequest(mgr, "follower", follower, ids[1], out)
			var rf requestResult
			select {
			case rf = <-out:
			case <-time.After(10 * time.Second):
				close(committer.release)
				t.Fatalf("follower did not reply within 10s while X stayed locked (attempts=%d)", attempts.Load())
			}
			close(committer.release)
			ra := <-out
			if ra.tag != "creator" || ra.err != nil || ra.ret != "created" {
				t.Fatalf("creator: want created, got %+v", ra)
			}
			if rf.tag != "follower" {
				t.Fatalf("unexpected reply order: %+v", rf)
			}
			if n := attempts.Load(); n != 1 {
				t.Fatalf("non-rollback handler was executed %d times for one request (declared.Value=%d, reply=%v): a create conflict must not requeue a handler that cannot roll back", n, declared.dao.Value, rf.err)
			}
			if declared.dao.Value != 2 { // pilot 初值 1，只加一次
				t.Fatalf("declared.Value=%d, want 2", declared.dao.Value)
			}
			if first := firstCreateErr.Load(); first == nil || !errors.Is(*first, ErrCreatedEntityLockConflict) || errors.Is(*first, ErrLockTimeout) {
				t.Fatalf("first Create error=%v: want ErrCreatedEntityLockConflict without ErrLockTimeout", derefErr(first))
			}
			if !errors.Is(rf.err, ErrCreatedEntityLockConflict) || errors.Is(rf.err, ErrLockTimeout) {
				t.Fatalf("follower reply=%v/%v: want ErrCreatedEntityLockConflict without ErrLockTimeout", rf.ret, rf.err)
			}
		})
	}
}

// 冲突发生后业务改返回别的锁超时类错误（例如嵌套派发的 ErrLockTimeout）：消息同样不重排，回复补上
// ErrCreatedEntityLockConflict，调用方仍能判别。
func TestNonRollbackHandlerCreateConflictSuppressesLaterLockTimeoutRequeue(t *testing.T) {
	manager := entity.NewEntityManager()
	ids := addPilots(t, manager, 36100, 2)
	declared := manager.Get(ids[1]).(*rollbackTestEntity)
	access := entity.NewManagerAccess(manager)
	x := mustBuildCastID(t, 36105, entity.EntityCategory(1), createdInScopeKind)
	committer := newBlockingCommitter()
	mgr := NewEngine(NestOptionWithGetter(access), NestOptionWithTransactionCommitter(committer), NestOptionWithWorkerPools(WorkerPoolConfig{Workers: 2, QueueCap: 16}, WorkerPoolConfig{}))
	creator := NewHandlerName("rr64_retag_creator")
	mgr.MustRegisterHandlerWithMeta(creator, func([]entity.IThreadSafeEntity, []any, ...HandlerOption) (any, error) {
		v, err := access.Create(createParam(x))
		if err != nil {
			return nil, err
		}
		return "created", MarkPersist(v.(*rollbackTestEntity).dao, 1)
	}, HandlerMeta{Rollback: RollbackState, Durability: DurabilityStrict})
	var attempts atomic.Int64
	follower := NewHandlerName("rr64_retag_follower")
	mgr.MustRegisterHandlerWithMeta(follower, func(es []entity.IThreadSafeEntity, _ []any, _ ...HandlerOption) (any, error) {
		attempts.Add(1)
		es[0].(*rollbackTestEntity).dao.Value++
		if _, err := access.Create(createParam(x)); err != nil {
			return nil, ErrLockTimeout // 业务丢掉原错误，返回一个普通锁超时
		}
		return "created", nil
	}, HandlerMeta{})
	if err := mgr.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = mgr.Shutdown(context.Background()) }()
	out := make(chan requestResult, 2)
	sendRequest(mgr, "creator", creator, ids[0], out)
	<-committer.entered
	sendRequest(mgr, "follower", follower, ids[1], out)
	var rf requestResult
	select {
	case rf = <-out:
	case <-time.After(10 * time.Second):
		close(committer.release)
		t.Fatalf("follower did not reply within 10s (attempts=%d)", attempts.Load())
	}
	close(committer.release)
	<-out
	if n := attempts.Load(); n != 1 || declared.dao.Value != 2 { // pilot 初值 1，只加一次
		t.Fatalf("attempts=%d declared.Value=%d, want 1/2 (reply=%v)", n, declared.dao.Value, rf.err)
	}
	if !errors.Is(rf.err, ErrCreatedEntityLockConflict) || !errors.Is(rf.err, ErrLockTimeout) {
		t.Fatalf("reply=%v: want ErrCreatedEntityLockConflict added to the business error", rf.err)
	}
}

// 锁序允许（新实体锁组高于已持有的全部锁组）时，不能回滚的 handler 仍有序等待，不报冲突、不重排：
// creator 提交释放后 follower 取得锁，得到 ErrEntityExists。
func TestNonRollbackHandlerCreateInLockOrderStillWaits(t *testing.T) {
	manager := entity.NewEntityManager()
	ids := addPilots(t, manager, 36200, 2)
	access := entity.NewManagerAccess(manager)
	z := mustBuildCastID(t, 36205, castOtherCategory, higherGroupCreatedKind)
	if entity.GetEntityGroup(z) <= entity.GetEntityGroup(ids[0]) {
		t.Fatal("fixture: the created kind must rank above the pilot group")
	}
	committer := newBlockingCommitter()
	mgr := NewEngine(NestOptionWithGetter(access), NestOptionWithTransactionCommitter(committer), NestOptionWithWorkerPools(WorkerPoolConfig{Workers: 2, QueueCap: 16}, WorkerPoolConfig{}))
	zParam := func() *entity.EntityCreateParam {
		return &entity.EntityCreateParam{IsCreate: true, Id: z, Category: castOtherCategory, Kind: higherGroupCreatedKind}
	}
	creator := NewHandlerName("rr64_order_creator")
	mgr.MustRegisterHandlerWithMeta(creator, func([]entity.IThreadSafeEntity, []any, ...HandlerOption) (any, error) {
		v, err := access.Create(zParam())
		if err != nil {
			return nil, err
		}
		return "created", MarkPersist(v.(*rollbackTestEntity).dao, 1)
	}, HandlerMeta{Rollback: RollbackState, Durability: DurabilityStrict})
	var attempts atomic.Int64
	entered := make(chan struct{}, 1)
	follower := NewHandlerName("rr64_order_follower")
	mgr.MustRegisterHandlerWithMeta(follower, func([]entity.IThreadSafeEntity, []any, ...HandlerOption) (any, error) {
		attempts.Add(1)
		entered <- struct{}{}
		_, err := access.Create(zParam())
		if errors.Is(err, entity.ErrEntityExists) {
			return "exists", nil
		}
		return "created", err
	}, HandlerMeta{})
	if err := mgr.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = mgr.Shutdown(context.Background()) }()
	out := make(chan requestResult, 2)
	sendRequest(mgr, "creator", creator, ids[0], out)
	<-committer.entered
	sendRequest(mgr, "follower", follower, ids[1], out)
	<-entered
	select {
	case r := <-out:
		close(committer.release)
		t.Fatalf("follower replied while the creator still held Z; it must wait in lock order: %+v", r)
	case <-time.After(100 * time.Millisecond):
	}
	close(committer.release)
	results := map[string]requestResult{}
	for len(results) < 2 {
		select {
		case r := <-out:
			results[r.tag] = r
		case <-time.After(5 * time.Second):
			t.Fatalf("requests did not finish: %v", results)
		}
	}
	if r := results["follower"]; r.err != nil || r.ret != "exists" || attempts.Load() != 1 {
		t.Fatalf("follower: want exists after an ordered wait in one attempt, got %+v attempts=%d", r, attempts.Load())
	}
}

func derefErr(p *error) error {
	if p == nil {
		return nil
	}
	return *p
}
