// 不能回滚的 handler 开始执行后不自动重排：新建冲突、锁超时 / 组迁移错误、等锁期间目标被摘除都只执行一次。

package nest

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/infra/base/lock"
)

// 不能回滚的 handler（RollbackNone / memory 快路径）内新建实体遇到按锁序不能等待的锁冲突时，冲突前的内存修改不会被撤销，
// 所以整条消息不自动重排；冲突以可 errors.Is 的 ErrCreatedEntityLockConflict 交给调用方（不带 ErrLockTimeout：它表示
// “未执行、已重排”）；锁序允许时仍有序等待；可回滚的事务保持“整条回滚后重排”（见 create_lock_conflict_test.go）。
// 历史：RR-20260926-64。

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

// creator 新建 X 后停在 strict 提交里（持有 X 的锁）；follower 先改声明实体、再新建 X：只执行一次，调用方拿到
// ErrCreatedEntityLockConflict，不必等 creator 提交。
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
			mgr := NewEngine(NestOptionWithGetter(access), NestOptionWithTransactionCommitter(committer), NestOptionWithWorkerPools(WorkerPoolConfig{Workers: 2, QueueCap: 16}, WorkerPoolConfig{}, WorkerPoolConfig{}))
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
	mgr := NewEngine(NestOptionWithGetter(access), NestOptionWithTransactionCommitter(committer), NestOptionWithWorkerPools(WorkerPoolConfig{Workers: 2, QueueCap: 16}, WorkerPoolConfig{}, WorkerPoolConfig{}))
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
	mgr := NewEngine(NestOptionWithGetter(access), NestOptionWithTransactionCommitter(committer), NestOptionWithWorkerPools(WorkerPoolConfig{Workers: 2, QueueCap: 16}, WorkerPoolConfig{}, WorkerPoolConfig{}))
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

// 不能回滚的 handler（memory 快路径，或消息自己的事务是 RollbackNone）一旦开始执行，锁超时 / 组迁移类暂时性错误不再让框架
// 自动重排——重排会让 handler 从头再执行，失败前已做且不撤销的修改重复生效；回复带 ErrNonRollbackNotRequeued（原因链保留）。
// handler 开始执行之前的准入失败照常重排。Cast 取得锁后发现目标已被摘除，返回 ErrEntityNotFound，不伪装成锁超时。
// 历史：RR-20260926-73。

// lockEntrySignalMutex 在 armed 之后的第一次 Lock 入口关闭 entered：调用方此时已通过 Touch、正要（或正在）等这把锁。
// 测试据此把“目标被摘除”排在等待者取得锁之前，不靠 sleep。
type lockEntrySignalMutex struct {
	lock.Mutex
	armed   atomic.Bool
	entered chan struct{}
}

func newLockEntrySignalMutex(id int64) *lockEntrySignalMutex {
	return &lockEntrySignalMutex{Mutex: lock.NewReentrantMutex(id), entered: make(chan struct{})}
}

func (m *lockEntrySignalMutex) Lock() {
	if m.armed.CompareAndSwap(true, false) {
		close(m.entered)
	}
	m.Mutex.Lock()
}

// recordingRemoteBatch 记录 FinalizeLocked 收到的 handler 与 Commit / Abort / Close 次数。
type recordingRemoteBatch struct {
	entity.RemoteWriteBatch
	mu        sync.Mutex
	finalized []string
	commits   atomic.Int32
	aborts    atomic.Int32
	closed    atomic.Int32
}

func (b *recordingRemoteBatch) FinalizeLocked(o entity.RemoteTransactionOutcome) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.finalized = append(b.finalized, o.Handler)
	return nil
}
func (b *recordingRemoteBatch) Commits() []entity.RemoteCommit { return nil }
func (b *recordingRemoteBatch) Commit(context.Context) ([]entity.RemoteCommitReceipt, error) {
	b.commits.Add(1)
	return nil, nil
}
func (b *recordingRemoteBatch) Abort(context.Context, error) error { b.aborts.Add(1); return nil }
func (b *recordingRemoteBatch) Close(context.Context) error        { b.closed.Add(1); return nil }

func (b *recordingRemoteBatch) finalizedHandlers() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.finalized...)
}

// follower 先改声明实体，再 Cast 更高锁组的 z（锁序允许，等待）；等待期间持有者 Destroy z：Cast 返回 ErrEntityNotFound，
// 消息不重排，声明实体只改一次。
func TestNonRollbackHandlerCastTargetRemovedWhileWaiting(t *testing.T) {
	for i, tc := range []struct {
		name      string
		meta      HandlerMeta
		wantValue int // pilot 初值 1
	}{
		{"memory", HandlerMeta{}, 2}, // 不撤销：只加一次
		{"state_strict", HandlerMeta{Rollback: RollbackState, Durability: DurabilityStrict}, 1}, // 整条回滚
	} {
		t.Run(tc.name, func(t *testing.T) {
			manager := entity.NewEntityManager()
			pID, p := newAsyncPilotEntity(t, 41000+int64(i)*10, 1)
			if err := manager.TryAdd(p); err != nil {
				t.Fatal(err)
			}
			zID := mustBuildCastID(t, 41001+int64(i)*10, castPlayerCategory, castPlayerKind)
			zMu := newLockEntrySignalMutex(zID)
			z := &rollbackTestEntity{EntityBase: entity.NewEntityBaseWithMutex(zID, castPlayerCategory, false, zMu, castPlayerKind), dao: &rollbackTestDao{id: zID, Value: 1}}
			if err := manager.TryAdd(z); err != nil {
				t.Fatal(err)
			}
			access := entity.NewManagerAccess(manager)
			mgr := NewEngine(NestOptionWithGetter(access), NestOptionWithTransactionCommitter(&recordingCommitter{}), NestOptionWithWorkerPools(WorkerPoolConfig{Workers: 2, QueueCap: 16}, WorkerPoolConfig{}, WorkerPoolConfig{}))
			holding, proceed := make(chan struct{}), make(chan struct{})
			holder, follower := NewHandlerName("rr73_holder_"+tc.name), NewHandlerName("rr73_follower_"+tc.name)
			mgr.MustRegisterHandlerWithMeta(holder, func(es []entity.IThreadSafeEntity, _ []any, _ ...HandlerOption) (any, error) {
				close(holding)
				<-proceed
				return "destroyed", access.Destroy(context.Background(), es[0], entity.EntityDestroyReason(0), false)
			}, HandlerMeta{})
			var attempts atomic.Int64
			var firstCastErr atomic.Pointer[error]
			mgr.MustRegisterHandlerWithMeta(follower, func(es []entity.IThreadSafeEntity, _ []any, _ ...HandlerOption) (any, error) {
				n := attempts.Add(1)
				e := es[0].(*rollbackTestEntity)
				old := e.dao.Value
				RecordUndo(e.dao, 1, func() error { e.dao.Value = old; return nil })
				e.dao.Value++ // memory：失败也不撤销
				if _, err := guardFixtureCastOne[*rollbackTestEntity](zID); err != nil {
					if n == 1 {
						firstCastErr.Store(&err)
					}
					return nil, err
				}
				return "ok", MarkPersist(e.dao, 1)
			}, tc.meta)
			if err := mgr.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = mgr.Shutdown(context.Background()) }()

			hOut, fOut := make(chan requestResult, 1), make(chan requestResult, 1)
			sendRequest(mgr, "holder", holder, zID, hOut)
			<-holding // holder 持有 z 的锁
			zMu.armed.Store(true)
			sendRequest(mgr, "follower", follower, pID, fOut)
			select {
			case <-zMu.entered: // follower 已通过 Touch，正在等 z 的锁
			case <-time.After(10 * time.Second):
				t.Fatal("follower never reached z's lock")
			}
			close(proceed)
			rh := <-hOut
			var rf requestResult
			select {
			case rf = <-fOut:
			case <-time.After(10 * time.Second):
				t.Fatal("follower did not reply within 10s")
			}
			if rh.err != nil {
				t.Fatalf("holder: %v", rh.err)
			}
			if a := attempts.Load(); a != 1 || p.dao.Value != tc.wantValue {
				t.Fatalf("one request: attempts=%d declared.Value=%d (want 1/%d) reply=%v/%v: a handler that already ran must not be requeued after its cast target disappeared",
					a, p.dao.Value, tc.wantValue, rf.ret, rf.err)
			}
			first := firstCastErr.Load()
			if first == nil || !errors.Is(*first, ErrEntityNotFound) || errors.Is(*first, ErrLockTimeout) {
				t.Fatalf("Cast error for a target removed while waiting = %v: want ErrEntityNotFound, not a lock timeout", derefErr(first))
			}
			if !errors.Is(rf.err, ErrEntityNotFound) || errors.Is(rf.err, ErrLockTimeout) {
				t.Fatalf("follower reply=%v: want ErrEntityNotFound without ErrLockTimeout", rf.err)
			}
		})
	}
}

// memory handler 已开始执行后返回锁超时 / 组迁移类错误：只执行一次，修改只生效一次，回复带 ErrNonRollbackNotRequeued 且保留原因。
func TestNonRollbackHandlerTransientFailureIsNotRequeued(t *testing.T) {
	for i, cause := range []error{
		fmt.Errorf("%w: simulated nested lock conflict", ErrLockTimeout),
		fmt.Errorf("%w: simulated", ErrEntityLockGroupChanged),
		fmt.Errorf("%w: simulated", ErrEntityGroupTransitionPending),
	} {
		t.Run(fmt.Sprintf("cause_%d", i), func(t *testing.T) {
			manager := entity.NewEntityManager()
			ids := addPilots(t, manager, 41100+int64(i)*10, 1)
			access := entity.NewManagerAccess(manager)
			mgr := NewEngine(NestOptionWithGetter(access), NestOptionWithWorkerPools(WorkerPoolConfig{Workers: 1, QueueCap: 16}, WorkerPoolConfig{}, WorkerPoolConfig{}))
			var attempts atomic.Int64
			var declared *rollbackTestEntity
			name := NewHandlerName(fmt.Sprintf("rr73_memory_transient_%d", i))
			mgr.MustRegisterHandlerWithMeta(name, func(es []entity.IThreadSafeEntity, _ []any, _ ...HandlerOption) (any, error) {
				declared = es[0].(*rollbackTestEntity)
				declared.dao.Value++
				if attempts.Add(1) == 1 {
					return nil, cause
				}
				return "ok", nil
			}, HandlerMeta{})
			if err := mgr.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = mgr.Shutdown(context.Background()) }()
			r := requestWithin(t, mgr, name, ids[0])
			if a := attempts.Load(); a != 1 || declared.dao.Value != 2 {
				t.Fatalf("one request: attempts=%d declared.Value=%d (want 1/2) reply=%v/%v: a memory handler that already ran must not be requeued", a, declared.dao.Value, r.ret, r.err)
			}
			if !errors.Is(r.err, ErrNonRollbackNotRequeued) || !errors.Is(r.err, cause) {
				t.Fatalf("reply=%v: want ErrNonRollbackNotRequeued wrapping %v", r.err, cause)
			}
		})
	}
}

// 带 Remote 批次的 memory handler（消息自己的 RollbackTx policy 为 none）：同一规则，批次 Abort、不 Commit。
func TestRemoteNonRollbackHandlerTransientFailureIsNotRequeued(t *testing.T) {
	localID, e := newAsyncPilotEntity(t, 41200, 1)
	getter := newMockGetter()
	remoteID := mustBuildCastID(t, 41201, entity.EntityCategoryRemote, nestRemoteManagedKind)
	getter.Add(newMockEntity(remoteID, entity.EntityCategoryRemote))
	getter.Add(e)
	var batches []*recordingRemoteBatch
	var batchesMu sync.Mutex
	manager := stagedRemoteManager{prepare: func(context.Context) (entity.RemoteWriteBatch, error) {
		b := &recordingRemoteBatch{}
		batchesMu.Lock()
		batches = append(batches, b)
		batchesMu.Unlock()
		return b, nil
	}}
	mgr := NewEngine(NestOptionWithGetter(getter), NestOptionWithRemoteEntityManager(manager), NestOptionWithWorkerPools(WorkerPoolConfig{Workers: 1, QueueCap: 16}, WorkerPoolConfig{}, WorkerPoolConfig{}))
	name := NewHandlerName("rr73_remote_memory_transient")
	var attempts atomic.Int64
	mgr.MustRegisterHandlerWithMeta(name, func([]entity.IThreadSafeEntity, []any, ...HandlerOption) (any, error) {
		e.dao.Value++
		if attempts.Add(1) == 1 {
			return nil, fmt.Errorf("%w: simulated", ErrLockTimeout)
		}
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
	err, _ := reply.(error)
	batchesMu.Lock()
	n := len(batches)
	batchesMu.Unlock()
	if a := attempts.Load(); a != 1 || e.dao.Value != 2 || n != 1 {
		t.Fatalf("one request: attempts=%d value=%d batches=%d (want 1/2/1) reply=%v", a, e.dao.Value, n, reply)
	}
	if b := batches[0]; b.commits.Load() != 0 || b.aborts.Load() != 1 {
		t.Fatalf("batch Commit=%d Abort=%d: the failed message's batch must abort", b.commits.Load(), b.aborts.Load())
	}
	if !errors.Is(err, ErrNonRollbackNotRequeued) || !errors.Is(err, ErrLockTimeout) {
		t.Fatalf("reply=%v: want ErrNonRollbackNotRequeued wrapping the lock timeout", reply)
	}
}

// 对照：可回滚的事务（undo_strict）返回锁超时时整条回滚后照旧重排，第二次执行成功，值只加一次。
func TestRollbackHandlerTransientFailureStillRequeues(t *testing.T) {
	manager := entity.NewEntityManager()
	ids := addPilots(t, manager, 41300, 1)
	access := entity.NewManagerAccess(manager)
	mgr := NewEngine(NestOptionWithGetter(access), NestOptionWithTransactionCommitter(&isolatedCountingCommitter{}), NestOptionWithWorkerPools(WorkerPoolConfig{Workers: 1, QueueCap: 16}, WorkerPoolConfig{}, WorkerPoolConfig{}))
	var attempts atomic.Int64
	var declared *rollbackTestEntity
	name := NewHandlerName("rr73_undo_transient_requeues")
	mgr.MustRegisterHandlerWithMeta(name, func(es []entity.IThreadSafeEntity, _ []any, _ ...HandlerOption) (any, error) {
		declared = es[0].(*rollbackTestEntity)
		old := declared.dao.Value
		if !RecordUndo(declared.dao, 1, func() error { declared.dao.Value = old; return nil }) {
			return nil, errors.New("missing undo transaction")
		}
		declared.dao.Value++
		if attempts.Add(1) == 1 {
			return nil, fmt.Errorf("%w: simulated", ErrLockTimeout)
		}
		return "ok", MarkPersist(declared.dao, 1)
	}, HandlerMeta{Rollback: RollbackUndo, Durability: DurabilityStrict})
	if err := mgr.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = mgr.Shutdown(context.Background()) }()
	r := requestWithin(t, mgr, name, ids[0])
	if r.err != nil || r.ret != "ok" || attempts.Load() != 2 || declared.dao.Value != 2 {
		t.Fatalf("reply=%v/%v attempts=%d value=%d: a rolled-back transaction must still be requeued (want ok, 2 attempts, value 2)", r.ret, r.err, attempts.Load(), declared.dao.Value)
	}
}

// 带 Remote 批次的 memory handler 内新建实体冲突：只执行一次、只带冲突哨兵、批次 Abort。这是 RollbackTx policy 为 none 的
// 唯一形态（纯本地 memory 走没有 RollbackTx 的快路径），用例同时核对这个前提。
func TestRemoteNonRollbackHandlerCreateConflictIsNotRequeued(t *testing.T) {
	manager := entity.NewEntityManager()
	aID, a := newAsyncPilotEntity(t, 45000, 1)
	pID, p := newAsyncPilotEntity(t, 45001, 1)
	for _, e := range []*rollbackTestEntity{a, p} {
		if err := manager.TryAdd(e); err != nil {
			t.Fatal(err)
		}
	}
	remoteID := mustBuildCastID(t, 45002, entity.EntityCategoryRemote, nestRemoteManagedKind)
	if err := manager.TryAdd(newMockEntity(remoteID, entity.EntityCategoryRemote)); err != nil {
		t.Fatal(err)
	}
	access := entity.NewManagerAccess(manager)
	x := mustBuildCastID(t, 45005, entity.EntityCategory(1), createdInScopeKind)
	committer := newBlockingCommitter()
	batch := &recordingRemoteBatch{}
	rm := stagedRemoteManager{prepare: func(context.Context) (entity.RemoteWriteBatch, error) { return batch, nil }}
	mgr := NewEngine(NestOptionWithGetter(access), NestOptionWithRemoteEntityManager(rm), NestOptionWithTransactionCommitter(committer), NestOptionWithWorkerPools(WorkerPoolConfig{Workers: 2, QueueCap: 16}, WorkerPoolConfig{}, WorkerPoolConfig{}))
	creator, follower := NewHandlerName("rr73_rm_creator"), NewHandlerName("rr73_rm_follower")
	mgr.MustRegisterHandlerWithMeta(creator, func([]entity.IThreadSafeEntity, []any, ...HandlerOption) (any, error) {
		v, err := access.Create(createParam(x))
		if err != nil {
			return nil, err
		}
		return "created", MarkPersist(v.(*rollbackTestEntity).dao, 1)
	}, HandlerMeta{Rollback: RollbackState, Durability: DurabilityStrict})
	var attempts atomic.Int64
	var noneTx atomic.Bool
	mgr.MustRegisterHandlerWithMeta(follower, func([]entity.IThreadSafeEntity, []any, ...HandlerOption) (any, error) {
		if attempts.Add(1) == 1 {
			tx := CurrentRollbackTx()
			noneTx.Store(tx != nil && tx.policy == RollbackNone)
		}
		p.dao.Value++
		_, err := access.Create(createParam(x))
		return "created", err
	}, HandlerMeta{Rollback: RollbackNone, Durability: DurabilityMemory})
	if err := mgr.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = mgr.Shutdown(context.Background()) }()
	ra := make(chan requestResult, 1)
	sendRequest(mgr, "creator", creator, aID, ra)
	<-committer.entered // creator 新建 X 后停在 strict 提交里，持有 X 的锁
	msg, ch := GenSyncMsg(MsgTypeMulti)
	msg.Name, msg.Tids, msg.Cost, msg.HasRemote = follower.String(), []int64{remoteID, pID}, true, true
	if err := mgr.dispatcher.TrySendMsg(msg); err != nil {
		t.Fatal(err)
	}
	var reply any
	select {
	case reply = <-ch:
	case <-time.After(10 * time.Second):
		close(committer.release)
		t.Fatal("remote memory follower did not reply while the creator's commit was held")
	}
	close(committer.release)
	<-ra
	err, _ := reply.(error)
	if !noneTx.Load() {
		t.Fatal("premise: the follower must run inside a RollbackTx with policy none (memory handler with a Remote batch)")
	}
	if attempts.Load() != 1 || p.dao.Value != 2 {
		t.Fatalf("attempts=%d value=%d (want 1/2) reply=%v", attempts.Load(), p.dao.Value, reply)
	}
	if !errors.Is(err, ErrCreatedEntityLockConflict) || errors.Is(err, ErrLockTimeout) {
		t.Fatalf("reply=%v: want ErrCreatedEntityLockConflict without ErrLockTimeout", reply)
	}
	if batch.commits.Load() != 0 || batch.aborts.Load() != 1 {
		t.Fatalf("batch Commit=%d Abort=%d: want the failed message's batch aborted", batch.commits.Load(), batch.aborts.Load())
	}
}
