package nest

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/entity"
	"github.com/tjbdwanghaibo/roost-core/metrics"
)

func TestNestStageMetricsFollowExecutionPaths(t *testing.T) {
	for _, mode := range []string{"disabled", "memory", "strict", "pipeline", "async", "rollback"} {
		t.Run(mode, func(t *testing.T) {
			metrics.DefaultRegistry().Reset()
			t.Cleanup(func() { metrics.DefaultRegistry().Reset() })
			id, e := newAsyncPilotEntity(t, 9990, 10)
			getter := newMockGetter()
			getter.Add(e)
			opts := []NestOption{NestOptionWithGetter(getter), NestOptionWithTransactionCommitter(newPipelinedTestCommitter(true))}
			if mode != "disabled" {
				opts = append(opts, NestOptionWithStageMetrics(true))
			}
			if mode == "async" {
				opts = append(opts, NestOptionWithPipelinedAsyncCompletion(1, 4))
			}
			engine := NewEngine(opts...)
			name := NewHandlerName("stages_" + mode)
			meta := HandlerMeta{}
			if mode == "strict" {
				meta = HandlerMeta{Rollback: RollbackUndo, Durability: DurabilityStrict}
			}
			if mode == "pipeline" || mode == "async" {
				meta = HandlerMeta{Rollback: RollbackUndo, Durability: DurabilityPipelined}
			}
			if mode == "rollback" {
				meta.Rollback = RollbackState
			}
			failure := errors.New("business rejected")
			engine.MustRegisterHandlerWithMeta(name, func(_ []entity.IThreadSafeEntity, _ []any, _ ...HandlerOption) (any, error) {
				if mode == "rollback" {
					return nil, failure
				}
				if meta.Durability != DurabilityMemory {
					return nil, MarkPersist(e.dao, 1)
				}
				return nil, nil
			}, meta)
			if err := engine.Start(); err != nil {
				t.Fatal(err)
			}
			defer engine.Shutdown(context.Background())
			_, err := engine.Request(context.Background(), name, id, nil)
			if mode == "rollback" {
				if !errors.Is(err, failure) {
					t.Fatal(err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if err := engine.Shutdown(context.Background()); err != nil {
				t.Fatal(err)
			}
			seen := map[string]bool{}
			for _, metric := range metrics.Snapshot() {
				if metric.Name == "nest.stage.duration" && metric.Labels["handler"] == name.String() {
					seen[metric.Labels["stage"]] = true
				}
			}
			if mode == "disabled" {
				if len(seen) != 0 {
					t.Fatalf("default emitted stages: %v", seen)
				}
				return
			}
			want := []string{"queue", "load", "lock", "handler", "release", "cleanup"}
			if mode == "rollback" {
				want = append(want, "capture", "rollback")
				if seen["admission"] {
					t.Error("rejected transaction recorded successful admission")
				}
			} else {
				want = append(want, "admission")
			}
			if mode == "strict" {
				want = append(want, "capture", "prepare", "durable_commit")
			}
			if mode == "pipeline" || mode == "async" {
				want = append(want, "capture", "prepare", "enqueue", "durable_wait")
			}
			if mode == "async" {
				want = append(want, "commit_queue", "completion_queue", "completion_unlock", "completion_order", "completion")
			}
			for _, stage := range want {
				if !seen[stage] {
					t.Errorf("missing stage %q: %v", stage, seen)
				}
			}
		})
	}
}

func TestTickRegistrationInsideCallbackUsesNextSnapshot(t *testing.T) {
	resetTickCallbacksForTest()
	t.Cleanup(resetTickCallbacksForTest)
	var calls []string
	var once sync.Once
	MustRegisterTickCallback(NewTickCallbackName("register"), func(TickMsg) {
		calls = append(calls, "first")
		once.Do(func() {
			MustRegisterTickCallback(NewTickCallbackName("later"), func(TickMsg) { calls = append(calls, "later") })
		})
	})
	MustRegisterTickCallback(NewTickCallbackName("peer"), func(TickMsg) { calls = append(calls, "peer") })
	ticker := NewTicker(time.Hour)
	ticker.doTick()
	if !slices.Equal(calls, []string{"first", "peer"}) {
		t.Fatal(calls)
	}
	ticker.doTick()
	if !slices.Equal(calls, []string{"first", "peer", "first", "peer", "later"}) {
		t.Fatal(calls)
	}
}

func TestSlowDispatchWatchReuseAfterTimerExpiry(t *testing.T) {
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 1000 {
				watch := startSlowDispatchTraceWatch(&Msg{Name: "watch-reuse"}, time.Now())
				// 只验证过期/Stop 竞争的所有权交接，避免为压力回归打印数千份堆栈。
				watch.traced.Store(true)
				watch.timer.Reset(0)
				watch.stop(0, "ok")
			}
		})
	}
	wg.Wait()
}
