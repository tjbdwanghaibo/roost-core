package nest

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/infra/base/fctx"
)

// RR-20260926-54（Nest 正式链路）：Nest 慢阶段准备以请求自己的截止时间领头一次冷加载，截止后
// 请求结束、Msg 回收；同一 flight 里预算更长的等待方仍要拿到结果。加载不能再用领头方的 ctx
// ——既不能随它的截止被取消，也不能在它结束后还经它的快续行（已回收的 Msg）发布实体：发布改走
// Nest 绑定给 ManagerAccess 的 RunLocal，仍在快 worker 上执行。

type gatedStageLoader struct {
	manager      *entity.EntityManager
	entered      chan struct{}
	release      chan struct{}
	loads        atomic.Int32
	publishedOff atomic.Bool
}

func (l *gatedStageLoader) LoadEntity(ctx context.Context, id int64, kind entity.EntityKind) (entity.IThreadSafeEntity, error) {
	l.loads.Add(1)
	if fctx.InFastWorker() {
		return nil, errors.New("loader ran in fast worker")
	}
	l.entered <- struct{}{}
	select {
	case <-l.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	var value entity.IThreadSafeEntity
	var createErr error
	err := entity.RunLocal(ctx, func() {
		if !fctx.InFastWorker() {
			l.publishedOff.Store(true)
		}
		value = newMockEntityWithKind(id, entity.ResolveEntityID(id).Category, kind)
		createErr = l.manager.TryAdd(value)
	})
	return value, errors.Join(err, createErr)
}

func TestSlowPreparationLeaderTimeoutDoesNotCutTheSharedLoad(t *testing.T) {
	manager := entity.NewEntityManager()
	access := entity.NewManagerAccess(manager)
	loader := &gatedStageLoader{manager: manager, entered: make(chan struct{}, 4), release: make(chan struct{})}
	if _, err := access.ConfigureLoader(loader); err != nil {
		t.Fatal(err)
	}
	mgr := NewEngine(NestOptionWithGetter(access), NestOptionWithWorkerPools(WorkerPoolConfig{1, 8}, WorkerPoolConfig{2, 8}))
	mgr.MustRegisterHandlerWithMeta(NewHandlerName("touch"), func(es []entity.IThreadSafeEntity, _ []any, _ ...HandlerOption) (any, error) {
		return len(es), nil
	}, HandlerMeta{Rollback: RollbackNone, Durability: DurabilityMemory})
	startColdTargetEngine(t, mgr)
	cold := mustBuildCastID(t, 8501, entity.EntityCategory(1), nestLocalKind)

	// 领头方：Nest 请求，预算很短（代表 RR-36 的登录预算）。
	leaderCtx, cancelLeader := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancelLeader()
	leader := make(chan error, 1)
	go func() {
		_, err := mgr.Request(leaderCtx, NewHandlerName("touch"), cold, nil)
		leader <- err
	}()
	select {
	case <-loader.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("the slow preparation never reached the loader")
	}
	// 等待方：同一实体、预算更长，直接经 ManagerAccess 加入同一 flight。
	waiterCtx, cancelWaiter := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancelWaiter()
	waiter := make(chan error, 1)
	go func() {
		value, err := access.Get(waiterCtx, cold, entity.EntityCategoryNone)
		if err == nil && value == nil {
			err = errors.New("nil entity")
		}
		waiter <- err
	}()
	select {
	case err := <-leader:
		if err == nil {
			t.Fatal("the leader finished although its load is still gated")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the leader did not return when its budget expired")
	}
	// 等领头请求在慢池、快池都彻底结束（Msg 已回收），再放行加载：发布只能走绑定的快池入口。
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(time.Millisecond) {
		queue := mgr.Stats().Queue
		if queue.Slow.Running == 0 && queue.Fast.Running == 0 && queue.ContinuationRunning == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the leading request is still running: %+v", queue)
		}
	}
	close(loader.release)
	select {
	case err := <-waiter:
		if err != nil {
			t.Fatalf("the waiter with a longer budget failed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the waiter never got the shared load's result")
	}
	if loader.publishedOff.Load() {
		t.Fatal("the loaded entity was published off the fast pool")
	}
	if n := loader.loads.Load(); n != 1 {
		t.Fatalf("loads=%d, want the waiter to share the leader's flight", n)
	}
	// 实体已发布，后续请求直接在快池执行。
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if ret, err := mgr.Request(ctx, NewHandlerName("touch"), cold, nil); err != nil || ret != 1 {
		t.Fatalf("request after the shared load: ret=%v err=%v", ret, err)
	}
}
