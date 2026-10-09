package nest

import (
	"context"
	"fmt"
	"runtime"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/infra/base/fctx"
	"github.com/tjbdwanghaibo/roost-core/infra/base/goroutine"
	"github.com/tjbdwanghaibo/roost-core/infra/base/worker"
)

// 本文件只比较单 ID 的调度，不运行 Entity/Guard/事务/DAO/网络业务。
// id 分片是测试对照组，不具备正式队列的多 ID、慢准备、续行和取消契约。
type dispatchComparisonPool struct {
	admit func(*Msg) error
	stop  func(context.Context) error
}

func newDispatchComparisonPool(kind string, workers int, handler func(*Msg)) dispatchComparisonPool {
	if kind == "nest" {
		q := newDispatchQueue("dispatch_compare", WorkerPoolConfig{workers, DefaultFastQueueCapacity}, WorkerPoolConfig{}, handler, nil)
		q.start()
		return dispatchComparisonPool{func(m *Msg) error { return q.admit(m, false) }, q.stop}
	}
	queues := make([]chan *Msg, workers)
	var wg sync.WaitGroup
	for i := range queues {
		// 总等待容量与 Nest 相同（余数留给前几个分片）；测试窗口远小于任何分片容量。
		capacity := DefaultFastQueueCapacity / workers
		if i < DefaultFastQueueCapacity%workers {
			capacity++
		}
		queues[i] = make(chan *Msg, capacity)
		wg.Add(1)
		go func(queue <-chan *Msg) {
			defer wg.Done()
			for m := range queue {
				// 与 dispatchQueue.work 的快阶段保留相同的异常隔离、fctx 和 Msg 释放。
				goroutine.SafeFunc(func() {
					_, release := fctx.NewContext(fctx.WithSource("worker"), fctx.WithHandler("dispatch_compare"), fctx.WithFastWorker())
					defer release()
					defer m.OnRelease()
					handler(m)
				})
			}
		}(queues[i])
	}
	return dispatchComparisonPool{
		admit: func(m *Msg) error {
			select {
			case queues[uint64(m.Tid)%uint64(workers)] <- m:
				return nil
			default:
				return worker.ErrWorkerQueueFull
			}
		},
		// 只供测试：调用者先停止生产，再且仅再调用一次 stop；不冒充生产生命周期实现。
		stop: func(ctx context.Context) error {
			for _, q := range queues {
				close(q)
			}
			done := make(chan struct{})
			go func() { wg.Wait(); close(done) }()
			select {
			case <-done:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		},
	}
}

// 固定算术工作量，不能用“忙等到墙钟截止”冒充固定 CPU 工作；后者被抢占也算已完成。
//
//go:noinline
func dispatchComparisonBurn(n int) uint64 {
	x := uint64(0x12345678)
	for i := 0; i < n; i++ {
		x ^= x << 13
		x ^= x >> 7
		x ^= x << 17
	}
	return x
}

var dispatchComparisonCalibration struct {
	sync.Once
	iterations int
	checksum   uint64
}

func dispatchComparisonIterations() int {
	dispatchComparisonCalibration.Do(func() {
		n := 10000
		for {
			start := time.Now()
			dispatchComparisonCalibration.checksum = dispatchComparisonBurn(n)
			if time.Since(start) >= 20*time.Millisecond {
				break
			}
			n *= 2
		}
		samples := make([]float64, 5)
		for i := range samples {
			start := time.Now()
			dispatchComparisonCalibration.checksum = dispatchComparisonBurn(n)
			samples[i] = float64(n) * float64(time.Millisecond) / float64(time.Since(start))
		}
		slices.Sort(samples)
		dispatchComparisonCalibration.iterations = max(1, int(samples[2]))
	})
	return dispatchComparisonCalibration.iterations
}

type dispatchComparisonSample struct {
	wait, handler, total time.Duration
	completed            bool
}

type dispatchComparisonJob struct {
	index    int
	start    time.Time
	done     chan *dispatchComparisonJob
	checksum uint64
}

func dispatchComparisonID(index, workers int, distribution string) int64 {
	switch distribution {
	case "hot_id":
		return 1
	case "colliding_ids":
		return int64(index%10000*workers + 1)
	default:
		return int64(index%10000 + 1)
	}
}

// BenchmarkDispatchCompare 测固定窗口的完成吞吐，非固定速率压测。
// 4 个生产者各自维持 16 条未完成消息；满队列立即报错，不忙重试、不丢弃。
// cpu_* 是同进程一次校准的固定工作量；wait_* 是故意占住 worker 的等待模拟，非业务建议。
func BenchmarkDispatchCompare(b *testing.B) {
	iterations := dispatchComparisonIterations()
	for _, workload := range []string{"noop", "cpu_1ms", "cpu_200ms", "wait_1ms", "wait_200ms", "cpu_mixed_1pct"} {
		for _, distribution := range []string{"uniform", "colliding_ids", "hot_id"} {
			for _, kind := range []string{"nest", "id"} {
				b.Run(workload+"/"+distribution+"/"+kind, func(b *testing.B) {
					runDispatchComparison(b, kind, workload, distribution, 4, iterations)
				})
			}
		}
	}
}

// 单生产者/多生产者配对，用于检查空 handler 时的准入竞争。
func BenchmarkDispatchCompareProducers(b *testing.B) {
	for _, producers := range []int{1, 4, 16} {
		for _, kind := range []string{"nest", "id"} {
			b.Run(fmt.Sprintf("p%d/%s", producers, kind), func(b *testing.B) {
				runDispatchComparison(b, kind, "noop", "uniform", producers, 0)
			})
		}
	}
}

func runDispatchComparison(b *testing.B, kind, workload, distribution string, producers, iterations int) {
	b.StopTimer()
	workers := runtime.GOMAXPROCS(0)
	samples := make([]dispatchComparisonSample, b.N)
	var rejected atomic.Int64
	pool := newDispatchComparisonPool(kind, workers, func(m *Msg) {
		job := m.Params[0].(*dispatchComparisonJob)
		start := time.Now()
		switch workload {
		case "cpu_1ms":
			job.checksum = dispatchComparisonBurn(iterations)
		case "cpu_200ms":
			job.checksum = dispatchComparisonBurn(iterations * 200)
		case "cpu_mixed_1pct":
			n := iterations
			if job.index%100 == 0 {
				n *= 200
			}
			job.checksum = dispatchComparisonBurn(n)
		case "wait_1ms":
			time.Sleep(time.Millisecond)
		case "wait_200ms":
			time.Sleep(200 * time.Millisecond)
		}
		end := time.Now()
		samples[job.index] = dispatchComparisonSample{start.Sub(job.start), end.Sub(start), end.Sub(job.start), true}
		// 反馈表示 handler 已完成，吞吐计时还会包含最后的队列排空/消息释放。
		job.done <- job
	})
	var producersDone sync.WaitGroup
	start := make(chan struct{})
	for producer := 0; producer < producers; producer++ {
		producersDone.Add(1)
		go func(p int) {
			defer producersDone.Done()
			done := make(chan *dispatchComparisonJob, 64/producers)
			next := p
			submit := func(job *dispatchComparisonJob) bool {
				if next >= b.N {
					return false
				}
				job.index = next
				next += producers
				m := GenMsg(MsgTypeSingle)
				m.Tid = dispatchComparisonID(job.index, workers, distribution)
				m.Params = []any{job}
				m.OnSend()
				job.start = time.Now()
				if err := pool.admit(m); err != nil {
					m.OnRelease()
					rejected.Add(1)
					return false
				}
				return true
			}
			<-start
			pending := 0
			for range cap(done) {
				if submit(&dispatchComparisonJob{done: done}) {
					pending++
				}
			}
			for pending > 0 {
				job := <-done
				pending--
				if submit(job) {
					pending++
				}
			}
		}(producer)
	}
	b.ReportAllocs()
	b.ResetTimer()
	b.StartTimer()
	close(start)
	producersDone.Wait()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	err := pool.stop(ctx)
	cancel()
	b.StopTimer()
	if err != nil || rejected.Load() != 0 {
		b.Fatalf("drain=%v rejected=%d", err, rejected.Load())
	}
	waits, totals := make([]time.Duration, b.N), make([]time.Duration, b.N)
	var handlerTotal time.Duration
	zeroDurations := 0
	for i, sample := range samples {
		// Windows 上极短调用可能落在同一时钟刻度；零耗时不等于没有完成。
		if !sample.completed || sample.wait < 0 || sample.handler < 0 || sample.total < 0 {
			b.Fatalf("message %d invalid completion: %+v", i, sample)
		}
		waits[i], totals[i] = sample.wait, sample.total
		handlerTotal += sample.handler
		if sample.total == 0 {
			zeroDurations++
		}
	}
	slices.Sort(waits)
	slices.Sort(totals)
	p99 := (b.N*99+99)/100 - 1
	b.ReportMetric(float64(b.N)/b.Elapsed().Seconds(), "completed/s")
	b.ReportMetric(float64(waits[p99])/float64(time.Millisecond), "wait-p99-ms")
	b.ReportMetric(float64(totals[p99])/float64(time.Millisecond), "total-p99-ms")
	b.ReportMetric(float64(totals[b.N-1])/float64(time.Millisecond), "total-max-ms")
	b.ReportMetric(float64(handlerTotal)/float64(b.N)/float64(time.Millisecond), "handler-avg-ms")
	b.ReportMetric(float64(iterations), "iterations/1ms")
	b.ReportMetric(0, "rejected")
	b.ReportMetric(100*float64(zeroDurations)/float64(b.N), "zero-duration-pct")
}

// 比较前验证单 ID 串行、FIFO 和完整排空，避免“丢任务/破坏顺序”换来假吞吐。
func TestDispatchComparisonSingleIDContract(t *testing.T) {
	for _, kind := range []string{"nest", "id"} {
		t.Run(kind, func(t *testing.T) {
			var counts [16]atomic.Int64
			var active [16]atomic.Int64
			var invalid atomic.Bool
			pool := newDispatchComparisonPool(kind, 4, func(m *Msg) {
				index := int(m.Tid - 1)
				if active[index].Add(1) != 1 {
					invalid.Store(true)
				}
				runtime.Gosched()
				if counts[index].Add(1) != int64(m.Params[0].(int)) {
					invalid.Store(true)
				}
				active[index].Add(-1)
			})
			defer func() {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				if err := pool.stop(ctx); err != nil {
					t.Fatal(err)
				}
				if invalid.Load() {
					t.Error("same ID ran concurrently or out of order")
				}
				for i := range counts {
					if n := counts[i].Load(); n != 100 {
						t.Errorf("id=%d completed=%d want=100", i+1, n)
					}
				}
			}()
			for sequence := 1; sequence <= 100; sequence++ {
				for id := 1; id <= 16; id++ {
					m := GenMsg(MsgTypeSingle)
					m.Tid, m.Params = int64(id), []any{sequence}
					m.OnSend()
					if err := pool.admit(m); err != nil {
						m.OnRelease()
						t.Fatal(err)
					}
				}
			}
		})
	}
}
