package nestwal_test

// 热点单例实体的同步请求吞吐（KNOWN-LIMITS“热点单例实体的写吞吐”）：真实 Nest（8 个 short worker）+ 正式 engine.Projector
// + 真实 WAL（默认组提交参数、真实 fsync），32 个闭环客户端同时 Request 同一个实体。
//
//   - strict / pipelined：每条请求都在同 ID 准入链上等前一条持久并回复，预期约“每次 fsync 一条”；
//   - pipelined_async_completion：handler 声明 HandlerMeta.AsyncCompletion（//roost:nest completion=async），Enqueue 后作业
//     结束、下一条随即开始，一次 fsync 覆盖一批；
//   - async_under_strict_load：async 持久的热点实体，另有 4 个客户端在别的实体上持续做 strict 提交（只占 8 个 short worker 里的 4 个，热点请求不缺 worker）——nestwal 在 fsync 时
//     不再挡写入之后，async 记录不排在 strict 的 fsync 后面。
//
// 报告热点实体的 ops/s 与回复延迟 p50 / p99。前后对照：两侧各 go test -c，交替运行 -test.bench BenchmarkHotEntityRequest，
// benchstat 比较（docs/performance/benchmarks/hot-entity-commit-20261115/）。

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	engine "github.com/tjbdwanghaibo/roost-core/framework/dataengine/engine"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	corenest "github.com/tjbdwanghaibo/roost-core/framework/nest"
	"github.com/tjbdwanghaibo/roost-core/framework/nestwal"
)

func BenchmarkHotEntityRequest(b *testing.B) {
	const clients, background = 32, 4
	strict := corenest.HandlerMeta{Rollback: corenest.RollbackUndo, Durability: corenest.DurabilityStrict}
	for _, tc := range []struct {
		name       string
		meta       corenest.HandlerMeta
		background bool
	}{
		{"strict", strict, false},
		{"pipelined", corenest.HandlerMeta{Rollback: corenest.RollbackUndo, Durability: corenest.DurabilityPipelined}, false},
		{"pipelined_async_completion", hotEntityAsyncCompletionMeta(), false},
		{"async", corenest.HandlerMeta{Rollback: corenest.RollbackUndo, Durability: corenest.DurabilityAsync}, false},
		{"async_under_strict_load", corenest.HandlerMeta{Rollback: corenest.RollbackUndo, Durability: corenest.DurabilityAsync}, true},
	} {
		b.Run(tc.name, func(b *testing.B) {
			opts := nestwal.DefaultOptions(b.TempDir())
			opts.SegmentBytes = 1 << 30
			wal, err := nestwal.Open(opts)
			if err != nil {
				b.Fatal(err)
			}
			projector, err := engine.NewProjector(wal, fallbackBenchStore{}, engine.ProjectorOptions{CloseWAL: true})
			if err != nil {
				b.Fatal(err)
			}
			defer func() { _ = projector.Close(context.Background()) }()
			entity.MustRegisterEntityKindCategory(fallbackBenchKind, entity.EntityCategoryWorld)
			manager := entity.NewEntityManager()
			ids := make([]int64, 1+background)
			for i := range ids {
				id, err := entity.BuildEntityID(70000+fallbackBenchUnique.Add(1), fallbackBenchKind)
				if err != nil {
					b.Fatal(err)
				}
				e := &fallbackBenchEntity{EntityBase: entity.NewEntityBase(id, entity.EntityCategoryWorld, false, fallbackBenchKind), dao: &fallbackBenchDao{id: id}}
				if err := manager.TryAdd(e); err != nil {
					b.Fatal(err)
				}
				ids[i] = id
			}
			mgr := corenest.NewEngine(corenest.NestOptionWithGetter(entity.NewManagerAccess(manager)), corenest.NestOptionWithTransactionCommitter(projector),
				corenest.NestOptionWithWorkerPools(corenest.WorkerPoolConfig{Workers: 8, QueueCap: 1024}, corenest.WorkerPoolConfig{}, corenest.WorkerPoolConfig{}))
			increment := func(es []entity.IThreadSafeEntity, _ []any, _ ...corenest.HandlerOption) (any, error) {
				e := es[0].(*fallbackBenchEntity)
				old := e.dao.Value
				corenest.RecordUndo(e.dao, 1, func() error { e.dao.Value = old; return nil })
				e.dao.Value++
				return e.dao.Value, corenest.MarkPersist(e.dao, 1)
			}
			suffix := fallbackBenchUnique.Add(1)
			hot := corenest.NewHandlerName(fmt.Sprintf("hot_entity_%s_%d", tc.name, suffix))
			load := corenest.NewHandlerName(fmt.Sprintf("hot_entity_load_%d", suffix))
			mgr.MustRegisterHandlerWithMeta(hot, increment, tc.meta)
			mgr.MustRegisterHandlerWithMeta(load, increment, strict)
			if err := mgr.Start(); err != nil {
				b.Fatal(err)
			}
			defer func() { _ = mgr.Shutdown(context.Background()) }()

			stop := make(chan struct{})
			var loadWG sync.WaitGroup
			if tc.background {
				for i := range background {
					loadWG.Go(func() {
						for {
							select {
							case <-stop:
								return
							default:
							}
							ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
							_, err := mgr.Request(ctx, load, ids[1+i], nil)
							cancel()
							if err != nil {
								b.Error(err)
								return
							}
						}
					})
				}
				time.Sleep(50 * time.Millisecond) // 让 strict 负载先跑起来
			}

			latencies := make([]int64, b.N)
			var next atomic.Int64
			var wg sync.WaitGroup
			b.ResetTimer()
			start := time.Now()
			for range clients {
				wg.Go(func() {
					for {
						op := next.Add(1) - 1
						if op >= int64(b.N) {
							return
						}
						began := time.Now()
						ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
						_, err := mgr.Request(ctx, hot, ids[0], nil)
						cancel()
						if err != nil {
							b.Error(err)
							return
						}
						latencies[op] = time.Since(began).Nanoseconds()
					}
				})
			}
			wg.Wait()
			elapsed := time.Since(start)
			b.StopTimer()
			close(stop)
			loadWG.Wait()
			slices.Sort(latencies)
			b.ReportMetric(float64(b.N)/elapsed.Seconds(), "ops/s")
			b.ReportMetric(float64(latencies[(b.N-1)*50/100]), "p50-ns")
			b.ReportMetric(float64(latencies[(b.N-1)*99/100]), "p99-ns")
		})
	}
}
