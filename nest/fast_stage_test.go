package nest

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/entity"
	"github.com/tjbdwanghaibo/roost-core/fctx"
)

type stageLoader struct {
	manager *entity.EntityManager
	calls   atomic.Int32
}

func (l *stageLoader) LoadEntity(ctx context.Context, id int64, kind entity.EntityKind) (entity.IThreadSafeEntity, error) {
	l.calls.Add(1)
	if fctx.InFastWorker() {
		return nil, errors.New("loader ran in fast worker")
	}
	var value entity.IThreadSafeEntity
	var createErr error
	err := entity.RunLocal(ctx, func() {
		if !fctx.InFastWorker() {
			createErr = errors.New("publication did not run in fast worker")
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
	mgr := NewEngine(NestOptionWithGetter(access), NestOptionWithWorkerPools(WorkerPoolConfig{1, 8}, WorkerPoolConfig{2, 8}))
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
		if !fctx.InFastWorker() {
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
					opts = append(opts, SendOptionSlow())
				}
				ctx, cancel := context.WithTimeout(t.Context(), time.Second)
				_, err := mgr.Request(ctx, NewHandlerName("direct_access"), hot, []any{many, detached}, opts...)
				cancel()
				if !errors.Is(err, entity.ErrColdLoadInLogic) || !errors.Is(err, fctx.ErrBlockingInFastWorker) || !strings.Contains(err.Error(), "direct_access") {
					t.Fatalf("slow=%v many=%v detached=%v: %v", slow, many, detached, err)
				}
			}
		}
	}
	if loader.calls.Load() != 0 {
		t.Fatalf("loads=%d", loader.calls.Load())
	}
	// 相同热ID仍可执行，证明错误路径没有遗留 Guard；声明冷目标后 Slow 正常加载/发布。
	for _, id := range []int64{hot, cold} {
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		ret, err := mgr.Request(ctx, NewHandlerName("ready"), id, nil, SendOptionSlow())
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
	_, release := fctx.NewContext(fctx.WithFastWorker(), fctx.WithHandler("self_dispatch"))
	defer release()
	if err := entity.RunLocal(saved, func() { localCalls++ }); err != nil {
		t.Fatal(err)
	}
	if executorCalls != 0 || localCalls != 1 {
		t.Fatalf("executor=%d local=%d", executorCalls, localCalls)
	}
	defer func() {
		err, ok := recover().(error)
		if !ok || !errors.Is(err, fctx.ErrBlockingInFastWorker) || !strings.Contains(err.Error(), "dispatchFastContinuation") {
			t.Fatalf("panic=%v", err)
		}
	}()
	_, _ = manager.dispatchFastContinuation(nil, nil)
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
	_, release := fctx.NewContext(fctx.WithFastWorker(), fctx.WithHandler(name.String()))
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
