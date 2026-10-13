// Cast 与消息事务：动态目标随事务回滚 / 提交，按实例释放，捕获失败整条回滚。

package nest

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/infra/base/fctx"
)

func castTransactionEntities(t *testing.T) (int64, *rollbackTestEntity, *rollbackTestEntity, *mockGetter) {
	t.Helper()
	id, primary := newAsyncPilotEntity(t, 9950, 10)
	otherID := mustBuildCastID(t, 9951, castAllianceCategory, castAllianceKind)
	other := &rollbackTestEntity{
		EntityBase: entity.NewEntityBase(otherID, castAllianceCategory, false, castAllianceKind),
		dao:        &rollbackTestDao{id: otherID, Value: 20},
	}
	getter := newMockGetter()
	getter.Add(primary)
	getter.Add(other)
	return id, primary, other, getter
}

// 动态实体属于 Nest 事务；是否安装客户端 Sync 不应改变回滚语义。
func TestCastTransactionRollbackWithoutSync(t *testing.T) {
	for _, rejectCommit := range []bool{false, true} {
		name := "handler_error"
		if rejectCommit {
			name = "commit_rejected"
		}
		t.Run(name, func(t *testing.T) {
			id, primary, other, getter := castTransactionEntities(t)
			rejection := errors.New("rejected")
			committer := &recordingCommitter{err: rejection}
			engine := NewEngine(NestOptionWithGetter(getter), NestOptionWithTransactionCommitter(committer))
			handler := NewHandlerName("cast_rollback_" + name)
			meta := HandlerMeta{Rollback: RollbackState, Durability: DurabilityMemory}
			if rejectCommit {
				meta.Durability = DurabilityStrict
			}
			engine.MustRegisterHandlerWithMeta(handler, func(_ []entity.IThreadSafeEntity, _ []any, _ ...HandlerOption) (any, error) {
				cast, err := guardFixtureCastOne[*rollbackTestEntity](other.ID())
				if err != nil {
					return nil, err
				}
				primary.dao.Value = 11
				cast.dao.Value = 99
				cast.dao.Tracker.MarkSync(1)
				if err := MarkPersist(cast.dao, 1); err != nil {
					return nil, err
				}
				ReleaseCast(cast)
				if !entity.CurrentGuardScope().Guard().Guarded(cast.GUId()) {
					t.Error("dynamic entity released before transaction outcome")
				}
				if rejectCommit {
					return nil, nil
				}
				return nil, rejection
			}, meta)
			if err := engine.Start(); err != nil {
				t.Fatal(err)
			}
			defer engine.Shutdown(context.Background())
			if _, err := engine.Request(context.Background(), handler, id, nil); !errors.Is(err, rejection) {
				t.Fatalf("request error = %v", err)
			}
			if primary.dao.Value != 10 || other.dao.Value != 20 {
				t.Errorf("rollback values = %d/%d, want 10/20", primary.dao.Value, other.dao.Value)
			}
			if mask := other.dao.Tracker.TakeEntitySyncDirty(); mask != 0 {
				t.Errorf("rolled-back dynamic entity retained sync dirty: %d", mask)
			}
		})
	}
}

type castBoundaryCommitter struct {
	*pipelinedTestCommitter
	beforeWait func()
}

func (c *castBoundaryCommitter) Enqueue(ctx context.Context, record CommitRecord) (CommitTicket, error) {
	ticket, err := c.pipelinedTestCommitter.Enqueue(ctx, record)
	if err != nil {
		return nil, err
	}
	return castBoundaryTicket{CommitTicket: ticket, beforeWait: c.beforeWait}, nil
}

type castBoundaryTicket struct {
	CommitTicket
	beforeWait func()
}

func (t castBoundaryTicket) Done() <-chan struct{} {
	t.beforeWait()
	return t.CommitTicket.Done()
}

func TestCastPipelinedReleasesAndStampsWithoutSync(t *testing.T) {
	_, closeContext := fctx.NewContext()
	defer closeContext()
	_, primary, other, getter := castTransactionEntities(t)
	scope, closeScope := entity.NewGuardScope("cast-pipelined")
	defer closeScope()
	pop := pushCurrentNestDispatchMsg(&Msg{getter: getter})
	defer pop()
	if !scope.Guard().RequireEntity(primary) {
		t.Fatal("primary lock")
	}
	committer := &castBoundaryCommitter{pipelinedTestCommitter: newPipelinedTestCommitter(false)}
	committer.beforeWait = func() {
		// ticket.Done 在 inline 路径等待 WAL 前调用，无需 sleep。
		if scope.Guard().Guarded(other.GUId()) {
			t.Error("dynamic entity still locked during durable wait")
		}
		if got := other.LastCommitLSN(); got != 1 {
			t.Errorf("dynamic entity LSN = %d, want 1", got)
		}
		committer.resolveAll(nil)
	}
	_, err := invokeWithTransaction(HandlerMeta{Rollback: RollbackState, Durability: DurabilityPipelined},
		[]entity.IThreadSafeEntity{primary}, committer, "cast-pipelined",
		func() { scope.Guard().ReleaseEntity(primary.GUId()) }, nil, func() (any, error) {
			cast, err := guardFixtureCastOne[*rollbackTestEntity](other.ID())
			if err != nil {
				return nil, err
			}
			cast.dao.Value++
			return nil, MarkPersist(cast.dao, 1)
		})
	if err != nil {
		t.Fatal(err)
	}
}

// handler 内 Cast 取得实体后回滚捕获失败：捕获失败记在事务上，即使业务吞掉错误返回 nil，handler 结束时也整条回滚；
// 调用方拿到可 errors.Is 的捕获错误（不是锁超时、不重排），committer 不被调用，Cast 已取得的锁在事务结束时归还。
// 历史：RR-20260927-31。

// kind 199：nest 包测试已用 11/12/13、171、193～195、206/207、236、238、241、248～252；199 未用（进程级 builder 注册表按 kind 区分）。
const castCaptureFailingKind entity.EntityKind = 199

var errCastCaptureFailed = errors.New("test: cast entity rollback capture failed")

// castCaptureFailingEntity 的回滚捕获总是失败（RollbackState 下 CaptureEntities 调 RollbackParticipant.CaptureRollback）。
type castCaptureFailingEntity struct {
	*rollbackTestEntity
}

func (*castCaptureFailingEntity) CaptureRollback(*RollbackTx) error { return errCastCaptureFailed }

func init() {
	entity.RegisterEntityBuilder(&entity.EntityBuilderParam{
		Category: castOtherCategory,
		Kind:     castCaptureFailingKind,
		Builder: func(p *entity.EntityCreateParam) (entity.IThreadSafeEntity, error) {
			return &castCaptureFailingEntity{&rollbackTestEntity{
				EntityBase: entity.NewEntityBaseWithMutex(p.Id, p.Category, false, p.Mutex, p.Kind),
				dao:        &rollbackTestDao{id: p.Id},
			}}, nil
		},
	})
}

func TestCastCaptureFailureFailsTheTransactionEvenIfSwallowed(t *testing.T) {
	for _, returnIt := range []bool{false, true} {
		name := "swallowed"
		if returnIt {
			name = "returned"
		}
		t.Run(name, func(t *testing.T) {
			manager := entity.NewEntityManager()
			pilots := addPilots(t, manager, 49980, 1)
			access := entity.NewManagerAccess(manager)
			x := mustBuildCastID(t, 49985, castOtherCategory, castCaptureFailingKind)
			if _, err := access.Create(&entity.EntityCreateParam{IsCreate: true, Id: x, Category: castOtherCategory, Kind: castCaptureFailingKind}); err != nil {
				t.Fatal(err)
			}
			committer := &countingCommitter{}
			mgr := NewEngine(NestOptionWithGetter(access), NestOptionWithTransactionCommitter(committer), NestOptionWithWorkerPools(WorkerPoolConfig{Workers: 1, QueueCap: 16}, WorkerPoolConfig{}, WorkerPoolConfig{}))
			handler := NewHandlerName("rr20260927_31_cast_capture_failure_" + name)
			var castErr error
			mgr.MustRegisterHandlerWithMeta(handler, func(es []entity.IThreadSafeEntity, _ []any, _ ...HandlerOption) (any, error) {
				pilot := es[0].(*rollbackTestEntity)
				pilot.dao.Value = 55
				if err := MarkPersist(pilot.dao, 1); err != nil {
					return nil, err
				}
				_, castErr = guardFixtureCastOne[*castCaptureFailingEntity](x)
				if returnIt {
					return nil, castErr
				}
				return "ok", nil // 业务吞掉 Cast 的捕获失败
			}, HandlerMeta{Rollback: RollbackState, Durability: DurabilityStrict})
			if err := mgr.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = mgr.Shutdown(context.Background()) }()

			ret, err := mgr.Request(context.Background(), handler, pilots[0], nil)
			if !errors.Is(castErr, errCastCaptureFailed) {
				t.Fatalf("fixture: Cast in the handler = %v, want the capture failure", castErr)
			}
			if err == nil {
				t.Fatalf("the transaction committed although the Cast target failed to be captured and the handler swallowed it: ret=%v commits=%d pilot=%d",
					ret, committer.calls.Load(), manager.Get(pilots[0]).(*rollbackTestEntity).dao.Value)
			}
			if !errors.Is(err, errCastCaptureFailed) {
				t.Fatalf("reply err=%v, want errors.Is the capture failure", err)
			}
			if errors.Is(err, ErrLockTimeout) {
				t.Fatalf("a capture failure must not be requeued as a lock timeout: %v", err)
			}
			if n := committer.calls.Load(); n != 0 {
				t.Fatalf("committer called %d times for a transaction that must fail", n)
			}
			if v := manager.Get(pilots[0]).(*rollbackTestEntity).dao.Value; v != 1 {
				t.Fatalf("declared entity value=%d after the forced rollback, want 1", v)
			}
			target := manager.Get(x)
			if target == nil {
				t.Fatal("the Cast target disappeared")
			}
			if !target.GetMutex().TryLock() {
				t.Fatal("the Cast target's lock was not released when the transaction ended")
			}
			target.GetMutex().Unlock()
		})
	}
}

// Cast 等锁期间目标被 Destroy / 仅内存卸载：返回 ErrEntityNotFound，错误文本带目标 ID（不是清理后归零的 GUId）。
// 历史：RR-20260926-73。
func TestCastTargetRemovedWhileWaitingNamesTheTarget(t *testing.T) {
	for i, tc := range []struct {
		name   string
		remove func(*entity.ManagerAccess, entity.IThreadSafeEntity) error
	}{
		{"destroy", func(access *entity.ManagerAccess, e entity.IThreadSafeEntity) error {
			return access.Destroy(context.Background(), e, entity.EntityDestroyReason(0), false)
		}},
		{"unload", func(access *entity.ManagerAccess, e entity.IThreadSafeEntity) error {
			return access.Unload(context.Background(), e)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manager := entity.NewEntityManager()
			pID, p := newAsyncPilotEntity(t, 48600+int64(i)*10, 1)
			if err := manager.TryAdd(p); err != nil {
				t.Fatal(err)
			}
			zID := mustBuildCastID(t, 48601+int64(i)*10, castPlayerCategory, castPlayerKind)
			zMu := newLockEntrySignalMutex(zID)
			z := &rollbackTestEntity{EntityBase: entity.NewEntityBaseWithMutex(zID, castPlayerCategory, false, zMu, castPlayerKind), dao: &rollbackTestDao{id: zID, Value: 1}}
			if err := manager.TryAdd(z); err != nil {
				t.Fatal(err)
			}
			access := entity.NewManagerAccess(manager)
			mgr := NewEngine(NestOptionWithGetter(access), NestOptionWithTransactionCommitter(&recordingCommitter{}), NestOptionWithWorkerPools(WorkerPoolConfig{Workers: 2, QueueCap: 16}, WorkerPoolConfig{}, WorkerPoolConfig{}))
			holding, proceed := make(chan struct{}), make(chan struct{})
			holder, follower := NewHandlerName("rr73_id_holder_"+tc.name), NewHandlerName("rr73_id_follower_"+tc.name)
			var removeErr error
			mgr.MustRegisterHandlerWithMeta(holder, func(es []entity.IThreadSafeEntity, _ []any, _ ...HandlerOption) (any, error) {
				close(holding)
				<-proceed
				removeErr = tc.remove(access, es[0])
				return "removed", nil
			}, HandlerMeta{})
			var castErr error
			mgr.MustRegisterHandlerWithMeta(follower, func(es []entity.IThreadSafeEntity, _ []any, _ ...HandlerOption) (any, error) {
				if _, err := guardFixtureCastOne[*rollbackTestEntity](zID); err != nil {
					castErr = err
					return nil, err
				}
				return "ok", nil
			}, HandlerMeta{})
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
			if rh.err != nil || removeErr != nil {
				t.Fatalf("premise: holder reply=%v remove=%v", rh.err, removeErr)
			}
			if !errors.Is(castErr, ErrEntityNotFound) || errors.Is(castErr, ErrLockTimeout) {
				t.Fatalf("premise: Cast error=%v, want ErrEntityNotFound without ErrLockTimeout (reply=%v)", castErr, rf.err)
			}
			if want := fmt.Sprintf("id=%d ", zID); !strings.Contains(castErr.Error(), want) {
				t.Fatalf("Cast error %q does not name the target (want %q): the ID was read after the entity was cleared", castErr, strings.TrimSpace(want))
			}
		})
	}
}

// ReleaseCast 按实例释放：handler 内 Destroy X → 新建同 ID 的 X → ReleaseCast(旧 X) 只释放旧实例自己的锁
// （正在等旧锁的访问者随即拿到它、看到已销毁退出），新 X 的锁保持到 handler 结束。
// 历史：RR-20260927-26。
func TestReleaseCastOfDestroyedInstanceKeepsRecreatedLock(t *testing.T) {
	manager := entity.NewEntityManager()
	pilots := addPilots(t, manager, 37200, 1)
	access := entity.NewManagerAccess(manager)
	x := mustBuildCastID(t, 37205, castOtherCategory, higherGroupCreatedKind)
	param := func() *entity.EntityCreateParam {
		return &entity.EntityCreateParam{IsCreate: true, Id: x, Category: castOtherCategory, Kind: higherGroupCreatedKind}
	}
	if _, err := access.Create(param()); err != nil {
		t.Fatal(err)
	}
	mgr := NewEngine(NestOptionWithGetter(access), NestOptionWithTransactionCommitter(&recordingCommitter{}), NestOptionWithWorkerPools(WorkerPoolConfig{Workers: 1, QueueCap: 16}, WorkerPoolConfig{}, WorkerPoolConfig{}))
	name := NewHandlerName("rr26_release_cast_destroyed_instance")
	type observed struct {
		oldGUID                   int64
		freshLockedAfterCreate    bool
		waiterReleasedMidHandler  bool
		waiterLockedDestroyed     bool
		freshLockedAfterRelease   bool
		freshReleasedByOwnRelease bool
	}
	var got observed
	var fresh entity.IThreadSafeEntity
	mgr.MustRegisterHandlerWithMeta(name, func([]entity.IThreadSafeEntity, []any, ...HandlerOption) (any, error) {
		old, err := guardFixtureCastOne[*rollbackTestEntity](x)
		if err != nil {
			return nil, err
		}
		waiterTouched := make(chan struct{})
		waiterDone := make(chan bool, 1)
		go func() { // Nest 之外的另一个访问者，按 Cast 的顺序：Touch → 等锁
			if !old.Touch() {
				close(waiterTouched)
				waiterDone <- false
				return
			}
			close(waiterTouched)
			var locked bool
			_ = entity.WithGuardScope("rr26_waiter", func(scope *entity.GuardScope) error {
				locked = scope.Guard().RequireEntity(old)
				return nil
			})
			old.UnTouch()
			waiterDone <- locked
		}()
		<-waiterTouched
		if err := access.Destroy(context.Background(), old, entity.EntityDestroyReason(0), false); err != nil {
			return nil, err
		}
		if fresh, err = access.Create(param()); err != nil {
			return nil, err
		}
		got.oldGUID = old.GUId()
		got.freshLockedAfterCreate = !tryLockElsewhere(fresh)
		ReleaseCast(old)
		got.freshLockedAfterRelease = !tryLockElsewhere(fresh)
		select { // 旧 X 自己的锁已释放：等待者拿到它、看到已销毁（RequireEntity=false）退出
		case got.waiterLockedDestroyed = <-waiterDone:
			got.waiterReleasedMidHandler = true
		case <-time.After(2 * time.Second):
		}
		ReleaseCast(fresh) // 当前实例照常按实例释放
		got.freshReleasedByOwnRelease = tryLockElsewhere(fresh)
		return "ok", nil
	}, HandlerMeta{})
	if err := mgr.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = mgr.Shutdown(context.Background()) }()
	if _, err := mgr.Request(context.Background(), name, pilots[0], nil); err != nil {
		t.Fatal(err)
	}
	t.Logf("%+v", got)
	if got.oldGUID == 0 || !got.freshLockedAfterCreate {
		t.Fatalf("fixture: the waiter's Touch must defer the old instance's cleanup (oldGUID=%d) and the handler must hold the re-created X (%v)", got.oldGUID, got.freshLockedAfterCreate)
	}
	if !got.freshLockedAfterRelease {
		t.Fatal("ReleaseCast(old X) released the re-created X's lock mid-handler")
	}
	if !got.waiterReleasedMidHandler || got.waiterLockedDestroyed {
		t.Fatalf("ReleaseCast(old X) must release the old instance's own lock: waiter released=%v lockedDestroyed=%v", got.waiterReleasedMidHandler, got.waiterLockedDestroyed)
	}
	if !got.freshReleasedByOwnRelease {
		t.Fatal("ReleaseCast(fresh X) did not release the current instance")
	}
	if !tryLockElsewhere(fresh) {
		t.Fatal("the re-created X is still locked after the handler")
	}
}

// 广播按实例释放，一个目标结束时释放它取得的全部锁：广播 handler 里 Destroy 目标 X 再新建同 ID 的 X，
// 下一个目标的 handler 里旧 X 与新 X 的锁都已释放；广播结束后新 X 是已发布实例且两把锁都空闲。
// 历史：RR-20260930-14。
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
			mgr := NewEngine(NestOptionWithGetter(access), NestOptionWithTransactionCommitter(&recordingCommitter{}), NestOptionWithWorkerPools(WorkerPoolConfig{Workers: 1, QueueCap: 16}, WorkerPoolConfig{}, WorkerPoolConfig{}))
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

// 声明的 remote-managed 目标由 prepareRemoteWriteBatch 准备、随消息自己的事务提交 / 撤销，不经 Cast 取锁。
// 带 Remote 批次的消息里 handler 对它调用 ReleaseCast 是空操作：锁保持到消息结束，由 Guard 作用域统一释放。
func TestReleaseCastOnDeclaredRemoteTargetIsNoopUntilMessageEnds(t *testing.T) {
	localID, e := newAsyncPilotEntity(t, 49830, 10)
	getter := newMockGetter()
	remoteID := mustBuildCastID(t, 49831, entity.EntityCategoryRemote, nestRemoteManagedKind)
	remote := newMockEntity(remoteID, entity.EntityCategoryRemote)
	getter.Add(remote)
	getter.Add(e)
	recording := &recordingRemoteBatch{}
	manager := stagedRemoteManager{prepare: func(context.Context) (entity.RemoteWriteBatch, error) { return recording, nil }}
	mgr := NewEngine(NestOptionWithGetter(getter), NestOptionWithRemoteEntityManager(manager), NestOptionWithTransactionCommitter(&recordsCommitter{}), NestOptionWithWorkerPools(WorkerPoolConfig{Workers: 1, QueueCap: 16}, WorkerPoolConfig{}, WorkerPoolConfig{}))
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
