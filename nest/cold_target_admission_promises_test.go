package nest

import (
	"context"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/entity"
	"github.com/tjbdwanghaibo/roost-core/fctx"
)

// RR-20260926-25：生成 sender 与 demo 从不带 SendOptionSlow，冷实体上的正式业务固定失败。
//
// RR-02 让快阶段只读已加载实体后，进入慢池只剩显式 SendOptionSlow 和 Remote 两条路；
// 生成的 sender 不暴露 SendOpt，demo 没有一个 handler 用 _cost 后缀，于是进程重启或
// 闲置驱逐后的离线玩家（gift saga 扣减/补偿、GM 发放）一律得到 ErrColdLoadInLogic。
// 承诺：Nest 在统一准入时发现声明目标里有未加载、可由 loader 加载的实体，就自动走
// 慢阶段预加载；业务代码不改，同 ID 顺序仍在统一准入处建立，loader 只在慢 worker 上
// 执行。准入之后、handler 取得 Guard 之前目标被驱逐时，同一条已准入请求原位转到慢
// 阶段准备，不重新排到同 ID 后继之后，也不在快 worker 上冷加载。

func newColdTargetEngine(t *testing.T, fast, slow WorkerPoolConfig) (*NestMgr, *entity.EntityManager, *stageLoader) {
	t.Helper()
	manager := entity.NewEntityManager()
	access := entity.NewManagerAccess(manager)
	loader := &stageLoader{manager: manager}
	if _, err := access.ConfigureLoader(loader); err != nil {
		t.Fatal(err)
	}
	mgr := NewEngine(NestOptionWithGetter(access), NestOptionWithWorkerPools(fast, slow))
	return mgr, manager, loader
}

func startColdTargetEngine(t *testing.T, mgr *NestMgr) {
	t.Helper()
	if err := mgr.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := mgr.Shutdown(ctx); err != nil {
			t.Error(err)
		}
	})
}

func TestDefaultSenderPreparesColdDeclaredTargetsOnSlowWorker(t *testing.T) {
	mgr, manager, loader := newColdTargetEngine(t, WorkerPoolConfig{1, 8}, WorkerPoolConfig{2, 8})
	hot := mustBuildCastID(t, 8401, entity.EntityCategory(1), nestLocalKind)
	coldSingle := mustBuildCastID(t, 8402, entity.EntityCategory(1), nestLocalKind)
	coldMulti := mustBuildCastID(t, 8403, entity.EntityCategory(1), nestLocalKind)
	coldGroup := mustBuildCastID(t, 8404, entity.EntityCategory(1), nestLocalKind)
	if err := manager.TryAdd(newMockEntityWithKind(hot, entity.EntityCategory(1), nestLocalKind)); err != nil {
		t.Fatal(err)
	}
	mgr.MustRegisterHandlerWithMeta(NewHandlerName("grant"), func(es []entity.IThreadSafeEntity, _ []any, _ ...HandlerOption) (any, error) {
		if !fctx.InFastWorker() {
			return nil, errors.New("handler left the fast pool")
		}
		for _, e := range es {
			if e == nil {
				return nil, errors.New("declared target missing in handler")
			}
		}
		return len(es), nil
	}, HandlerMeta{Rollback: RollbackNone, Durability: DurabilityMemory})
	startColdTargetEngine(t, mgr)

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	// 生成 sender 的默认形态：不带任何 SendOpt。
	if ret, err := mgr.Request(ctx, NewHandlerName("grant"), coldSingle, nil); err != nil || ret != 1 {
		t.Fatalf("single request to cold target: ret=%v err=%v loads=%d", ret, err, loader.calls.Load())
	}
	if ret, err := mgr.RequestMulti(ctx, NewHandlerName("grant"), []int64{hot, coldMulti}, nil); err != nil || ret != 2 {
		t.Fatalf("multi request with a cold target: ret=%v err=%v", ret, err)
	}
	if ret, err := mgr.RequestMultiGroup(ctx, NewHandlerName("grant"), [][]int64{{hot}, {coldGroup}}, nil); err != nil || ret != 2 {
		t.Fatalf("multi-group request with a cold target: ret=%v err=%v", ret, err)
	}
	// stageLoader 在快 worker 上被调用会返回错误，所以三次成功即证明加载只在慢 worker。
	if loads := loader.calls.Load(); loads != 3 {
		t.Fatalf("loads=%d, want one per cold declared target", loads)
	}
	// 目标已加载后回到快池直接执行，不再经过慢阶段。
	slowBefore := mgr.Stats().Queue.Slow.Started
	if ret, err := mgr.Request(ctx, NewHandlerName("grant"), coldSingle, nil); err != nil || ret != 1 {
		t.Fatalf("warm request: ret=%v err=%v", ret, err)
	}
	if slowAfter := mgr.Stats().Queue.Slow.Started; slowAfter != slowBefore || loader.calls.Load() != 3 {
		t.Fatalf("loaded target still used the slow stage: slow started %d -> %d loads=%d", slowBefore, slowAfter, loader.calls.Load())
	}
}

// evictingEntity 在被武装后的第一次 Touch 里把自己从 EntityManager 驱逐，模拟闲置回收恰好
// 落在“读取之后、引用之前”或“引用之后、取锁之前”两个窗口。
type evictingEntity struct {
	*mockEntity
	evict      atomic.Pointer[func()]
	afterTouch bool
}

func (e *evictingEntity) Touch() bool {
	evict := e.evict.Swap(nil)
	if evict != nil && !e.afterTouch {
		(*evict)()
	}
	ok := e.mockEntity.Touch()
	if evict != nil && e.afterTouch {
		(*evict)()
	}
	return ok
}

func TestDeclaredTargetEvictedAfterAdmissionMovesToSlowKeepingOrder(t *testing.T) {
	for _, window := range []string{"before lookup", "between lookup and touch", "between touch and lock"} {
		t.Run(window, func(t *testing.T) {
			// 1 个快 worker：先用 blocker 占住，保证两条请求都在目标仍加载时准入并走快池，
			// 然后在 handler 取 Guard 之前把目标驱逐。第二条请求用来证明第一条没有被重新
			// 准入到同 ID 队尾。
			mgr, manager, loader := newColdTargetEngine(t, WorkerPoolConfig{1, 8}, WorkerPoolConfig{2, 8})
			blockerID := mustBuildCastID(t, 8411, entity.EntityCategory(1), nestLocalKind)
			targetID := mustBuildCastID(t, 8412, entity.EntityCategory(1), nestLocalKind)
			target := &evictingEntity{mockEntity: newMockEntityWithKind(targetID, entity.EntityCategory(1), nestLocalKind), afterTouch: window == "between touch and lock"}
			for _, e := range []entity.IThreadSafeEntity{newMockEntityWithKind(blockerID, entity.EntityCategory(1), nestLocalKind), target} {
				if err := manager.TryAdd(e); err != nil {
					t.Fatal(err)
				}
			}
			entered, gate := make(chan struct{}), make(chan struct{})
			mgr.MustRegisterHandlerWithMeta(NewHandlerName("block"), func(_ []entity.IThreadSafeEntity, _ []any, _ ...HandlerOption) (any, error) {
				close(entered)
				<-gate
				return "released", nil
			}, HandlerMeta{Rollback: RollbackNone, Durability: DurabilityMemory})
			var orderMu sync.Mutex
			var order []string
			mgr.MustRegisterHandlerWithMeta(NewHandlerName("record"), func(es []entity.IThreadSafeEntity, params []any, _ ...HandlerOption) (any, error) {
				if !fctx.InFastWorker() || len(es) != 1 || es[0] == nil || es[0] == entity.IThreadSafeEntity(target) {
					return nil, errors.New("handler did not run on the fast pool with the reloaded target")
				}
				orderMu.Lock()
				order = append(order, params[0].(string))
				orderMu.Unlock()
				return "ok", nil
			}, HandlerMeta{Rollback: RollbackNone, Durability: DurabilityMemory})
			startColdTargetEngine(t, mgr)

			send := func(name string, id int64, params ...any) <-chan any {
				t.Helper()
				msg, ch := GenSyncMsg(MsgTypeSingle)
				msg.Name, msg.Tid, msg.Params = name, id, params
				if err := mgr.dispatcher.TrySendMsg(msg); err != nil {
					t.Fatal(err)
				}
				return ch
			}
			blocked := send("block", blockerID)
			stagedSignal(t, entered)
			first := send("record", targetID, "first")
			second := send("record", targetID, "second")
			if fast := mgr.Stats().Queue.Fast; fast.Ready+fast.BlockedOnPredecessor != 2 {
				t.Fatalf("both requests should be admitted to the fast lane while the target is loaded: %+v", fast)
			}
			evict := func() {
				if err := manager.Destroy(context.Background(), target, entity.DestroyReasonCommon, false); err != nil {
					t.Errorf("evict: %v", err)
				}
			}
			if window == "before lookup" {
				evict()
			} else {
				target.evict.Store(&evict)
			}
			close(gate)
			if got := stagedWait(t, blocked); got != "released" {
				t.Fatalf("blocker: %v", got)
			}
			if got := stagedWait(t, first); got != "ok" {
				t.Fatalf("first request after eviction: %v (loads=%d)", got, loader.calls.Load())
			}
			if got := stagedWait(t, second); got != "ok" {
				t.Fatalf("second request: %v", got)
			}
			orderMu.Lock()
			defer orderMu.Unlock()
			if !slices.Equal(order, []string{"first", "second"}) {
				t.Fatalf("same-ID order changed after moving to the slow stage: %v", order)
			}
			if loads := loader.calls.Load(); loads != 1 {
				t.Fatalf("loads=%d, want one slow-stage reload", loads)
			}
			if manager.Get(targetID) == nil {
				t.Fatal("reloaded target is not published")
			}
		})
	}
}
