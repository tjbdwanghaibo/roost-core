// 业务池不阻塞：业务池拒绝直接冷访问与自派发，io 池 handler 的 RunLocal 不等自己的池，RunLocal 入口的执行位置与生命周期。

package nest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/infra/base/fctx"
)

type stageLoader struct {
	manager *entity.EntityManager
	calls   atomic.Int32
}

func (l *stageLoader) LoadEntity(ctx context.Context, id int64, kind entity.EntityKind) (entity.IThreadSafeEntity, error) {
	l.calls.Add(1)
	if fctx.InShortWorker() {
		return nil, errors.New("loader ran in business worker")
	}
	var value entity.IThreadSafeEntity
	var createErr error
	err := entity.RunLocal(ctx, func() {
		if !fctx.InShortWorker() {
			createErr = errors.New("publication did not run in business worker")
			return
		}
		value = newMockEntityWithKind(id, entity.ResolveEntityID(id).Category, kind)
		createErr = l.manager.TryAdd(value)
	})
	return value, errors.Join(err, createErr)
}

func TestFastStageRejectsDirectColdAccessBeforeIO(t *testing.T) {
	manager := entity.NewEntityManager()
	access := entity.NewManagerAccess(manager)
	loader := &stageLoader{manager: manager}
	if _, err := access.ConfigureLoader(loader); err != nil {
		t.Fatal(err)
	}
	hot := mustBuildCastID(t, 8201, entity.EntityCategory(1), nestLocalKind)
	cold := mustBuildCastID(t, 8202, entity.EntityCategory(1), nestLocalKind)
	if err := manager.TryAdd(newMockEntityWithKind(hot, entity.EntityCategory(1), nestLocalKind)); err != nil {
		t.Fatal(err)
	}
	mgr := NewEngine(NestOptionWithGetter(access), NestOptionWithWorkerPools(WorkerPoolConfig{1, 8}, WorkerPoolConfig{}, WorkerPoolConfig{2, 8}))
	mgr.MustRegisterHandlerWithMeta(NewHandlerName("direct_access"), func(_ []entity.IThreadSafeEntity, params []any, _ ...HandlerOption) (any, error) {
		ctx := nestBaseContext()
		if params[1].(bool) {
			ctx = context.Background()
		}
		if params[0].(bool) {
			_, err := access.GetMany(ctx, []int64{hot, cold}, nil)
			return nil, err
		}
		return access.Get(ctx, cold, entity.EntityCategoryNone)
	}, HandlerMeta{Rollback: RollbackNone, Durability: DurabilityMemory})
	mgr.MustRegisterHandlerWithMeta(NewHandlerName("ready"), func(_ []entity.IThreadSafeEntity, _ []any, _ ...HandlerOption) (any, error) {
		if !fctx.InShortWorker() {
			return nil, errors.New("handler not marked fast")
		}
		return "ok", nil
	}, HandlerMeta{Rollback: RollbackNone, Durability: DurabilityMemory})
	if err := mgr.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := mgr.Shutdown(ctx); err != nil {
			t.Error(err)
		}
	})
	for _, slow := range []bool{false, true} {
		for _, many := range []bool{false, true} {
			for _, detached := range []bool{false, true} {
				var opts []SendOpt
				if slow {
					opts = append(opts, SendOptionIO())
				}
				ctx, cancel := context.WithTimeout(t.Context(), time.Second)
				_, err := mgr.Request(ctx, NewHandlerName("direct_access"), hot, []any{many, detached}, opts...)
				cancel()
				if !errors.Is(err, entity.ErrColdLoadInLogic) || !errors.Is(err, fctx.ErrBlockingInBusinessWorker) || !strings.Contains(err.Error(), "direct_access") {
					t.Fatalf("slow=%v many=%v detached=%v: %v", slow, many, detached, err)
				}
			}
		}
	}
	if loader.calls.Load() != 0 {
		t.Fatalf("loads=%d", loader.calls.Load())
	}
	// 相同热ID仍可执行，证明错误路径没有遗留 Guard；声明冷目标后 IO 正常加载/发布。
	for _, id := range []int64{hot, cold} {
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		ret, err := mgr.Request(ctx, NewHandlerName("ready"), id, nil, SendOptionIO())
		cancel()
		if err != nil || ret != "ok" {
			t.Fatalf("%v %v", ret, err)
		}
	}
	if loader.calls.Load() != 1 {
		t.Fatalf("prepared loads=%d", loader.calls.Load())
	}
}

func TestFastContinuationRejectsSelfDispatchAndSavedSlowExecutorRunsInline(t *testing.T) {
	var manager *NestMgr
	executorCalls, localCalls := 0, 0
	saved := entity.WithLocalExecutor(context.Background(), func(fn func()) error { executorCalls++; fn(); return nil })
	_, release := fctx.NewContext(fctx.WithShortWorker(), fctx.WithHandler("self_dispatch"))
	defer release()
	if err := entity.RunLocal(saved, func() { localCalls++ }); err != nil {
		t.Fatal(err)
	}
	if executorCalls != 0 || localCalls != 1 {
		t.Fatalf("executor=%d local=%d", executorCalls, localCalls)
	}
	defer func() {
		err, ok := recover().(error)
		if !ok || !errors.Is(err, fctx.ErrBlockingInBusinessWorker) || !strings.Contains(err.Error(), "dispatchBusinessContinuation") {
			t.Fatalf("panic=%v", err)
		}
	}()
	_, _ = manager.dispatchBusinessContinuation(nil, nil)
}

func TestFastBroadcastReportsColdTargetAndContinues(t *testing.T) {
	manager := entity.NewEntityManager()
	access := entity.NewManagerAccess(manager)
	loader := &stageLoader{manager: manager}
	if _, err := access.ConfigureLoader(loader); err != nil {
		t.Fatal(err)
	}
	hot := newDispatchLifetimeEntity(t, 8291)
	if err := manager.TryAdd(hot); err != nil {
		t.Fatal(err)
	}
	cold := mustBuildCastID(t, 8290, entity.EntityCategory(1), nestLocalKind)
	mgr := NewEngine(NestOptionWithGetter(access))
	name := NewHandlerName("broadcast_cold")
	calls := 0
	mgr.MustRegisterHandlerWithMeta(name, func(es []entity.IThreadSafeEntity, _ []any, _ ...HandlerOption) (any, error) {
		calls++
		return nil, nil
	}, HandlerMeta{})
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	defer slog.SetDefault(old)
	_, release := fctx.NewContext(fctx.WithShortWorker(), fctx.WithHandler(name.String()))
	defer release()
	_, end := entity.NewGuardScope("broadcast-cold")
	defer end()
	mgr.broadcastDispatch(name.String(), []int64{cold, hot.ID()}, nil)
	if calls != 1 || loader.calls.Load() != 0 || hot.touches != hot.untouches {
		t.Fatalf("calls=%d loads=%d touches=%d/%d", calls, loader.calls.Load(), hot.touches, hot.untouches)
	}
	if !strings.Contains(logs.String(), "nest broadcast target load failed") || !strings.Contains(logs.String(), name.String()) {
		t.Fatalf("missing actionable log: %s", logs.String())
	}
}

func TestSlowHandlerRunLocalDoesNotWaitForOwnPool(t *testing.T) {
	for _, workers := range []int{1, 2} {
		t.Run(fmt.Sprint(workers), func(t *testing.T) {
			getter := newMockGetter()
			ids := make([]int64, workers+1)
			for i := range ids {
				ids[i] = mustBuildCastID(t, int64(8100+i), entity.EntityCategory(1), nestLocalKind)
				getter.Add(newMockEntity(ids[i], entity.EntityCategory(1)))
			}
			mgr := NewEngine(NestOptionWithGetter(getter), NestOptionWithWorkerPools(WorkerPoolConfig{workers, 8}, WorkerPoolConfig{}, WorkerPoolConfig{workers, 8}))
			entered := make(chan struct{}, workers)
			release := make(chan struct{})
			var ran atomic.Int32
			mgr.MustRegisterHandlerWithMeta(NewHandlerName("local_step"), func(_ []entity.IThreadSafeEntity, _ []any, _ ...HandlerOption) (any, error) {
				entered <- struct{}{}
				<-release // 受控地让每个业务 worker 都进入同一边界，不靠 sleep。
				return "ok", entity.RunLocal(nestBaseContext(), func() { ran.Add(1) })
			}, HandlerMeta{Rollback: RollbackNone, Durability: DurabilityMemory})
			mgr.MustRegisterHandlerWithMeta(NewHandlerName("plain"), func(_ []entity.IThreadSafeEntity, _ []any, _ ...HandlerOption) (any, error) {
				return "ok", nil
			}, HandlerMeta{Rollback: RollbackNone, Durability: DurabilityMemory})
			if err := mgr.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				if err := mgr.Shutdown(ctx); err != nil {
					t.Error(err)
				}
			})
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			results := make(chan error, workers)
			for i := range workers {
				go func() {
					_, err := mgr.Request(ctx, NewHandlerName("local_step"), ids[i], nil, SendOptionIO())
					results <- err
				}()
			}
			for range workers {
				stagedSignal(t, entered)
			}
			close(release)
			for range workers {
				if err := <-results; err != nil {
					t.Fatal(err)
				}
			}
			if ran.Load() != int32(workers) {
				t.Fatalf("local steps=%d", ran.Load())
			}
			if _, err := mgr.Request(ctx, NewHandlerName("plain"), ids[workers], nil); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// NestMgr.RunLocal 是框架后台 goroutine 在业务池持锁执行的正式入口：fn 在业务 worker 上运行、调用方同步等它；
// 业务 worker 上调用 fail-fast；未运行的引擎拒绝且不执行 fn；它不排在同 ID 链上，同一实体正在 I/O 准备时也能完成。
// 历史：RR-20260926-30。

type binderCommitter struct {
	run func(func()) error
}

func (*binderCommitter) Commit(context.Context, CommitRecord) error { return nil }
func (committer *binderCommitter) BindLocalExecutor(run func(func()) error) {
	committer.run = run
}

func TestRunLocalExecutesOnTheShortPoolAndIsBoundToTheCommitter(t *testing.T) {
	committer := &binderCommitter{}
	mgr := NewEngine(NestOptionWithGetter(newMockGetter()), NestOptionWithTransactionCommitter(committer))
	if committer.run == nil {
		t.Fatal("NewEngine did not hand RunLocal to a LocalExecutorBinder committer")
	}
	ran := false
	if err := committer.run(func() { ran = true }); !errors.Is(err, ErrNestStopped) || ran {
		t.Fatalf("RunLocal before Start=%v ran=%v, want ErrNestStopped without running", err, ran)
	}
	if err := mgr.Start(); err != nil {
		t.Fatal(err)
	}
	onFast := false
	if err := committer.run(func() { onFast = fctx.InShortWorker() }); err != nil || !onFast {
		t.Fatalf("RunLocal=%v on business worker=%v", err, onFast)
	}
	if err := mgr.RunLocal(context.Background(), func() { panic("injected") }); err == nil {
		t.Fatal("a panicking task reported success")
	}
	if err := mgr.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	ran = false
	if err := committer.run(func() { ran = true }); !errors.Is(err, ErrNestStopped) || ran {
		t.Fatalf("RunLocal after Shutdown=%v ran=%v, want ErrNestStopped without running", err, ran)
	}
}

func TestRunLocalFromAFastWorkerFailsFast(t *testing.T) {
	getter := newMockGetter()
	id := mustBuildCastID(t, 9920, entity.EntityCategory(1), nestLocalKind)
	getter.Add(newMockEntity(id, entity.EntityCategory(1)))
	mgr := NewEngine(NestOptionWithGetter(getter), NestOptionWithWorkerPools(WorkerPoolConfig{1, 8}, WorkerPoolConfig{}, WorkerPoolConfig{1, 4}))
	mgr.MustRegisterHandlerWithMeta(NewHandlerName("self_run_local"), func(_ []entity.IThreadSafeEntity, _ []any, _ ...HandlerOption) (any, error) {
		return nil, mgr.RunLocal(context.Background(), func() {})
	}, HandlerMeta{Rollback: RollbackNone, Durability: DurabilityMemory})
	if err := mgr.Start(); err != nil {
		t.Fatal(err)
	}
	defer mgr.Shutdown(context.Background())
	if _, err := mgr.Request(context.Background(), NewHandlerName("self_run_local"), id, nil); !errors.Is(err, fctx.ErrBlockingInBusinessWorker) {
		t.Fatalf("RunLocal on a business worker=%v, want ErrBlockingInBusinessWorker", err)
	}
}

func TestRunLocalIsNotOrderedBehindTheEntitysSlowPreparation(t *testing.T) {
	getter := newMockGetter()
	id := mustBuildCastID(t, 9921, entity.EntityCategory(1), nestLocalKind)
	getter.Add(newMockEntity(id, entity.EntityCategory(1)))
	slow := &slowTestGetter{Getter: getter, entered: make(chan struct{}), release: make(chan struct{})}
	mgr := NewEngine(NestOptionWithGetter(slow), NestOptionWithWorkerPools(WorkerPoolConfig{1, 8}, WorkerPoolConfig{}, WorkerPoolConfig{1, 4}))
	mgr.MustRegisterHandlerWithMeta(NewHandlerName("cold"), func(_ []entity.IThreadSafeEntity, _ []any, _ ...HandlerOption) (any, error) {
		return "ok", nil
	}, HandlerMeta{Rollback: RollbackNone, Durability: DurabilityMemory})
	if err := mgr.Start(); err != nil {
		t.Fatal(err)
	}
	defer mgr.Shutdown(context.Background())
	msg, ch := GenSyncMsg(MsgTypeSingle)
	msg.Name, msg.Tid, msg.Cost = "cold", id, true
	if err := mgr.dispatcher.TrySendMsg(msg); err != nil {
		t.Fatal(err)
	}
	stagedSignal(t, slow.entered) // 同一实体的消息正卡在I/O 准备里
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := mgr.RunLocal(context.Background(), func() {}); err != nil {
			t.Error(err)
		}
	}()
	stagedSignal(t, done)
	close(slow.release)
	if got := stagedWait(t, ch); got != "ok" {
		t.Fatal(got)
	}
}
