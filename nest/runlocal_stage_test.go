package nest

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/entity"
)

func TestSlowHandlerRunLocalDoesNotWaitForOwnPool(t *testing.T) {
	for _, workers := range []int{1, 2} {
		t.Run(fmt.Sprint(workers), func(t *testing.T) {
			getter := newMockGetter()
			ids := make([]int64, workers+1)
			for i := range ids {
				ids[i] = mustBuildCastID(t, int64(8100+i), entity.EntityCategory(1), nestLocalKind)
				getter.Add(newMockEntity(ids[i], entity.EntityCategory(1)))
			}
			mgr := NewEngine(NestOptionWithGetter(getter), NestOptionWithWorkerPools(WorkerPoolConfig{workers, 8}, WorkerPoolConfig{workers, 8}))
			entered := make(chan struct{}, workers)
			release := make(chan struct{})
			var ran atomic.Int32
			mgr.MustRegisterHandlerWithMeta(NewHandlerName("local_step"), func(_ []entity.IThreadSafeEntity, _ []any, _ ...HandlerOption) (any, error) {
				entered <- struct{}{}
				<-release // 受控地让每个快 worker 都进入同一边界，不靠 sleep。
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
					_, err := mgr.Request(ctx, NewHandlerName("local_step"), ids[i], nil, SendOptionSlow())
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
