package nest

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
)

// 正式 Client→队列→Guard→Handler 链路；Mock Getter 不代表数据库或网络容量。
// Dispatch 使用固定在途窗口，计时包含最终排空；Request 的回复成本单独比较。
func BenchmarkNestHotPath(b *testing.B) {
	iterations := 0
	if configured := os.Getenv("ROOST_NEST_BENCH_ITERATIONS"); configured != "" {
		var err error
		iterations, err = strconv.Atoi(configured)
		if err != nil || iterations <= 0 {
			b.Fatal("invalid ROOST_NEST_BENCH_ITERATIONS")
		}
	} else {
		iterations = dispatchComparisonIterations() / 10
	}
	for _, cpu := range []bool{false, true} {
		for _, workers := range []int{1, 8} {
			for _, independent := range []bool{false, true} {
				for _, shape := range []string{"single", "multi2", "multi4", "groups1", "groups8", "groups32"} {
					for _, async := range []bool{false, true} {
						name := fmt.Sprintf("cpu=%t/workers=%d/independent=%t/%s/async=%t", cpu, workers, independent, shape, async)
						b.Run(name, func(b *testing.B) { benchmarkNestHotPath(b, shape, workers, independent, async, cpu, iterations) })
					}
				}
			}
		}
	}
}

func benchmarkNestHotPath(b *testing.B, shape string, workers int, independent, async, cpu bool, iterations int) {
	entity.MustRegisterEntityKindCategory(nestLocalKind, entity.EntityCategory(1))
	getter := newMockGetter()
	const window = 64
	count, groupsN := 1, 0
	switch shape {
	case "multi2":
		count = 2
	case "multi4":
		count = 4
	case "groups1":
		count, groupsN = 4, 1
	case "groups8":
		count, groupsN = 32, 8
	case "groups32":
		count, groupsN = 128, 32
	}
	targets := make([][]int64, window)
	groups := make([][][]int64, window)
	for slot := range targets {
		targets[slot] = make([]int64, count)
		for i := range targets[slot] {
			unique := 100000 + slot*count + i
			if !independent {
				unique = 100000 + i
			}
			id, err := entity.BuildEntityID(int64(unique), nestLocalKind)
			if err != nil {
				b.Fatal(err)
			}
			targets[slot][i] = id
			getter.Add(newMockEntity(id, entity.EntityCategory(1)))
		}
		if groupsN > 0 {
			groups[slot] = make([][]int64, groupsN)
			for g := range groups[slot] {
				groups[slot][g] = targets[slot][g*4 : (g+1)*4 : (g+1)*4]
			}
		}
	}
	engine := NewEngine(NestOptionWithGetter(getter), NestOptionWithWorkerPools(WorkerPoolConfig{Workers: workers, QueueCap: 1024}, WorkerPoolConfig{}, WorkerPoolConfig{}), NestOptionWithStageMetrics(false))
	name := NewHandlerName("bench_hot_path")
	slots := make(chan int, window)
	for slot := range window {
		slots <- slot
	}
	var completed atomic.Int64
	engine.MustRegisterHandlerWithMeta(name, func(_ []entity.IThreadSafeEntity, params []any, _ ...HandlerOption) (any, error) {
		var checksum uint64
		if cpu {
			checksum = dispatchComparisonBurn(iterations)
		}
		completed.Add(1)
		if async {
			slots <- params[0].(int)
		}
		return checksum, nil
	}, HandlerMeta{})
	if err := engine.Start(); err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = engine.Shutdown(context.Background()) })
	ctx := context.Background()
	send := func(slot int) error {
		ids := targets[slot]
		params := Params{slot}
		if async {
			if groupsN > 0 {
				return engine.DispatchMultiGroup(ctx, name, groups[slot], params)
			}
			if count > 1 {
				return engine.DispatchMulti(ctx, name, ids, params)
			}
			return engine.Dispatch(ctx, name, ids[0], params)
		}
		var err error
		if groupsN > 0 {
			_, err = engine.RequestMultiGroup(ctx, name, groups[slot], params)
		} else if count > 1 {
			_, err = engine.RequestMulti(ctx, name, ids, params)
		} else {
			_, err = engine.Request(ctx, name, ids[0], params)
		}
		return err
	}
	b.ReportAllocs()
	b.ResetTimer()
	if async {
		for range b.N {
			if err := send(<-slots); err != nil {
				b.Fatal(err)
			}
		}
	} else {
		var next atomic.Uint64
		b.SetParallelism(1)
		b.RunParallel(func(pb *testing.PB) {
			slot := int(next.Add(1)-1) % window
			for pb.Next() {
				if err := send(slot); err != nil {
					b.Error(err)
					return
				}
			}
		})
	}
	if err := engine.Shutdown(ctx); err != nil {
		b.Fatal(err)
	}
	b.StopTimer()
	if completed.Load() != int64(b.N) {
		b.Fatalf("completed=%d want=%d", completed.Load(), b.N)
	}
	if cpu {
		b.ReportMetric(float64(iterations), "cpu-iterations/op")
	}
}
