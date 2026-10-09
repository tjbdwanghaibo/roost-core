//go:build integration

package saga

// 原生步骤 Reserve 的真实副本集吞吐（状态文档方案的前后对照，docs/feature/SAGA-OPERATION-STATE-DOC-2026-10-06.md）。
// g 个协程并发为新的操作实例 Reserve 一次（原生步骤每次尝试都先付这个 Reserve 事务）。文件只依赖 NewDataEngineStepInbox /
// Reserve 的公开签名与 mongo_step_latency_real_mongo_integration_test.go 的 openMongoLatencyDatabase，可以原样复制到修前的
// 源码树上同口径对照：
//
//	GOWORK=off go test -tags integration -run '^$' -bench 'BenchmarkRealMongoNativeReserveThroughput' -benchtime 1000x -count 6 ./framework/saga/

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func BenchmarkRealMongoNativeReserveThroughput(b *testing.B) {
	for _, workers := range []int{1, 8, 32} {
		b.Run(fmt.Sprintf("g=%d", workers), func(b *testing.B) {
			client, database, _ := openMongoLatencyDatabase(b)
			inbox, err := NewDataEngineStepInbox(client, database, DataEngineStepInboxOptions{Owner: "bench", LeaseDuration: time.Minute})
			if err != nil {
				b.Fatal(err)
			}
			ctx := context.Background()
			if err := inbox.EnsureInfrastructure(ctx); err != nil {
				b.Fatal(err)
			}
			run := fmt.Sprintf("%d", time.Now().UnixNano())
			command := func(index int) Command {
				now := time.Now().UTC()
				operation := fmt.Sprintf("bench-%s-%d:1:0", run, index)
				return Command{
					ID: operation + ":1", IdempotencyKey: operation, SagaID: fmt.Sprintf("bench-%s-%d", run, index), SagaType: "bench",
					DefinitionVersion: 1, BusinessKey: "b", StepName: "debit", Phase: PhaseForward, Attempt: 1,
					Topic: "bench.debit", Payload: []byte("state"), CreatedAt: now, DeadlineAt: now.Add(time.Minute),
				}
			}
			for index := range 20 {
				if _, err := inbox.Reserve(ctx, command(-1-index)); err != nil {
					b.Fatal(err)
				}
			}
			latencies := make([]time.Duration, b.N)
			var next atomic.Int64
			var failed atomic.Value
			var wg sync.WaitGroup
			b.ResetTimer()
			start := time.Now()
			for range workers {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for {
						index := int(next.Add(1) - 1)
						if index >= b.N {
							return
						}
						opStart := time.Now()
						if reservation, err := inbox.Reserve(ctx, command(index)); err != nil || reservation.Duplicate {
							failed.Store(fmt.Sprintf("reserve %d: duplicate=%v err=%v", index, reservation.Duplicate, err))
							return
						}
						latencies[index] = time.Since(opStart)
					}
				}()
			}
			wg.Wait()
			elapsed := time.Since(start)
			b.StopTimer()
			if message := failed.Load(); message != nil {
				b.Fatal(message)
			}
			slices.Sort(latencies)
			percentile := func(q float64) float64 {
				return float64(latencies[int(q*float64(len(latencies)-1))].Microseconds()) / 1000
			}
			b.ReportMetric(float64(b.N)/elapsed.Seconds(), "ops/s")
			b.ReportMetric(percentile(0.50), "p50-ms")
			b.ReportMetric(percentile(0.99), "p99-ms")
		})
	}
}
