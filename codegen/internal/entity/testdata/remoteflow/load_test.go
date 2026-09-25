package remoteflow

import (
	"context"
	"encoding/json"
	"os"
	"runtime"
	"runtime/pprof"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/dataengine/engine"
	"github.com/tjbdwanghaibo/roost-core/entity"
	"github.com/tjbdwanghaibo/roost-core/metrics"
	"github.com/tjbdwanghaibo/roost-core/nest"
	"github.com/tjbdwanghaibo/roost-core/nestwal"
	"github.com/tjbdwanghaibo/roost-core/remoteentity"
)

func remoteNestWorkers() int {
	if remoteLoadEnabled() {
		return remoteInt("ROOST_REMOTE_WORKERS", 64)
	}
	return 4
}

func remoteLoadEnabled() bool { return os.Getenv("ROOST_REMOTE_LOAD") == "1" }
func remoteInt(name string, fallback int) int {
	if value := os.Getenv(name); value != "" {
		n, err := strconv.Atoi(value)
		if err != nil || n < 1 {
			panic("invalid " + name)
		}
		return n
	}
	return fallback
}
func remoteDuration() time.Duration {
	d, err := time.ParseDuration(os.Getenv("ROOST_REMOTE_DURATION"))
	if err != nil || d <= 0 {
		panic("positive ROOST_REMOTE_DURATION required")
	}
	return d
}
func remoteTestTimeout() time.Duration {
	if remoteLoadEnabled() {
		return remoteDuration() + 15*time.Minute
	}
	return 90 * time.Second
}
func remoteEntityCount() int {
	if remoteLoadEnabled() {
		n := remoteInt("ROOST_REMOTE_ENTITIES", 10000)
		if n < 2 || n%2 != 0 {
			panic("even entity count required")
		}
		return n
	}
	return 2
}
func markRemoteVerified(t *testing.T) {
	t.Helper()
	if err := os.WriteFile(os.Getenv("ROOST_REMOTE_OUTPUT")+".verified", []byte("all Mongo and NATS snapshots verified; rollback and outbox checks passed\n"), 0600); err != nil {
		t.Fatal(err)
	}
}

type remoteLoadSample struct {
	Seconds    float64               `json:"seconds"`
	Completed  uint64                `json:"completed"`
	Errors     uint64                `json:"errors"`
	Dropped    uint64                `json:"dropped"`
	HeapBytes  uint64                `json:"heap_bytes"`
	Goroutines int                   `json:"goroutines"`
	WAL        nestwal.Stats         `json:"wal"`
	Projection engine.ProjectorStats `json:"projection"`
	Nest       nest.DispatcherStats  `json:"nest"`
	Remote     remoteentity.Stats    `json:"remote"`
}
type remoteLoadReport struct {
	DurationSeconds                                        float64 `json:"duration_seconds"`
	Entities, Sessions, TargetRate                         int
	Completed, Errors, Dropped                             uint64
	CompletionTPS                                          float64
	LatencyP50MS, LatencyP95MS, LatencyP99MS, LatencyMaxMS int64
	FirstError                                             string
	ErrorDetails                                           []remoteLoadError
	Metrics                                                []metrics.Metric
	Samples                                                []remoteLoadSample
}

type remoteLoadError struct {
	Entities   []int64
	Error      string
	ElapsedMS  int64
	Projection engine.ProjectorStats
}

// 固定速率输入独立于完成速度；有界队列满时计入 dropped，不能静默降速掩盖过载。
// 每个业务会话为一个 worker；这里不模拟客户端 TCP，也不把 Remote 写入当作 AOI 广播。
func runRemoteLoad(t *testing.T, ctx context.Context, scheduler *nest.NestMgr, name, warmup nest.HandlerName, ids []int64, keys []entity.RemoteSnapshotKey, receiver, owner *remoteentity.Manager, projector *engine.Projector, wal *nestwal.WAL) map[int64]int64 {
	t.Helper()
	sessions, rate := remoteInt("ROOST_REMOTE_SESSIONS", 1000), remoteInt("ROOST_REMOTE_RATE", 20)
	duration := remoteDuration()
	output := os.Getenv("ROOST_REMOTE_OUTPUT")
	if output == "" {
		t.Fatal("ROOST_REMOTE_OUTPUT required")
	}
	progress, err := os.Create(output + ".jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer progress.Close()
	encoder := json.NewEncoder(progress)
	counts := make([]atomic.Int64, len(ids))
	var completed, failed, dropped atomic.Uint64
	var firstError string
	var errorDetails []remoteLoadError
	var errorMu sync.Mutex
	recordError := func(err error) {
		failed.Add(1)
		errorMu.Lock()
		if firstError == "" {
			firstError = err.Error()
		}
		errorMu.Unlock()
	}
	// 长稳期间继续续订，而不是人为延长 Interest TTL 到整个测试时长。
	renewCtx, cancelRenew := context.WithCancel(ctx)
	renewDone := make(chan struct{})
	go func() {
		defer close(renewDone)
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-renewCtx.Done():
				return
			case <-ticker.C:
				for _, key := range keys {
					if err := receiver.RenewRemoteSnapshotInterest(renewCtx, key); err != nil {
						if renewCtx.Err() == nil {
							recordError(err)
							t.Errorf("renew Remote Interest: %v", err)
						}
						return
					}
				}
			}
		}
	}()
	// 输入结束不等于投影排空；继续续订到最终快照校验结束，不能在 async 积压期间退订。
	t.Cleanup(func() { cancelRenew(); <-renewDone })
	type job struct {
		pair     int
		due      time.Time
		measured bool
	}
	jobs := make(chan job, sessions)
	var workers sync.WaitGroup
	var histogram [60001]atomic.Uint64
	var maxLatency atomic.Int64
	warmSlots := make(chan struct{}, remoteInt("ROOST_REMOTE_WARM_CONCURRENCY", 16))
	request := func(j job) {
		if !j.measured {
			defer func() { <-warmSlots }()
		}
		pair := ids[j.pair*2 : j.pair*2+2]
		handler := name
		if !j.measured {
			handler = warmup
		}
		callCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		requestStarted := time.Now()
		_, err := scheduler.RequestMulti(callCtx, handler, pair, nil)
		cancel()
		if err != nil {
			recordError(err)
			errorMu.Lock()
			if len(errorDetails) < 32 {
				errorDetails = append(errorDetails, remoteLoadError{append([]int64(nil), pair...), err.Error(), time.Since(requestStarted).Milliseconds(), projector.Stats()})
			}
			errorMu.Unlock()
			return
		}
		counts[j.pair*2].Add(1)
		counts[j.pair*2+1].Add(1)
		if j.measured {
			completed.Add(1)
			ms := time.Since(j.due).Milliseconds()
			histogram[min(ms, 60000)].Add(1)
			for old := maxLatency.Load(); ms > old && !maxLatency.CompareAndSwap(old, ms); old = maxLatency.Load() {
			}
		}
	}
	for range sessions {
		workers.Go(func() {
			for j := range jobs {
				request(j)
			}
		})
	}
	// 先把所有实体实际走过一次完整事务；预热不计入持续负载吞吐。
	for i := 0; i < len(ids)/2; i++ {
		warmSlots <- struct{}{}
		jobs <- job{pair: i}
	}
	for {
		total := int64(0)
		for i := range counts {
			total += counts[i].Load()
		}
		if total == int64(len(ids)) || failed.Load() > 0 {
			break
		}
		select {
		case <-time.After(20 * time.Millisecond):
		case <-ctx.Done():
			recordError(ctx.Err())
		}
	}
	if failed.Load() > 0 {
		close(jobs)
		workers.Wait()
		errorMu.Lock()
		message := firstError
		errorMu.Unlock()
		t.Fatalf("warmup: %s", message)
	}
	runtime.GC()
	report := remoteLoadReport{Entities: len(ids), Sessions: sessions, TargetRate: rate}
	started := time.Now()
	sample := func() {
		var mem runtime.MemStats
		runtime.ReadMemStats(&mem)
		s := remoteLoadSample{Seconds: time.Since(started).Seconds(), Completed: completed.Load(), Errors: failed.Load(), Dropped: dropped.Load(), HeapBytes: mem.HeapAlloc, Goroutines: runtime.NumGoroutine(), WAL: wal.Stats(), Projection: projector.Stats(), Remote: owner.Stats(), Nest: scheduler.Stats()}
		report.Samples = append(report.Samples, s)
		if err := encoder.Encode(s); err != nil {
			recordError(err)
		}
		t.Logf("load %.0fs completed=%d errors=%d dropped=%d heap=%d goroutines=%d unacked=%d", s.Seconds, s.Completed, s.Errors, s.Dropped, s.HeapBytes, s.Goroutines, s.Projection.WALUnacked)
	}
	sample()
	if os.Getenv("ROOST_REMOTE_PROFILE") == "1" {
		file, err := os.Create(output + ".cpu.pprof")
		if err != nil {
			t.Fatal(err)
		}
		if err := pprof.StartCPUProfile(file); err != nil {
			t.Fatal(err)
		}
		defer func() { pprof.StopCPUProfile(); file.Close() }()
	}
	progressTick := time.NewTicker(10 * time.Second)
	defer progressTick.Stop()
	interval := time.Second / time.Duration(rate)
	if interval <= 0 {
		t.Fatal("rate too large")
	}
	tick := time.NewTicker(interval)
	defer tick.Stop()
	finish := time.NewTimer(duration)
	defer finish.Stop()
	sequence := 0
	emitUntil := func(until time.Time) {
		count := int(min(until.Sub(started), duration) / interval)
		for sequence < count {
			pairs := len(ids) / 2
			if os.Getenv("ROOST_REMOTE_SHAPE") == "hot" {
				pairs = min(pairs, 100)
			}
			j := job{pair: sequence % pairs, due: started.Add(time.Duration(sequence+1) * interval), measured: true}
			sequence++
			select {
			case jobs <- j:
			default:
				dropped.Add(1)
			}
		}
	}
loop:
	for {
		select {
		case <-tick.C:
			emitUntil(time.Now())
		case <-progressTick.C:
			sample()
		case <-finish.C:
			emitUntil(started.Add(duration))
			break loop
		case <-ctx.Done():
			recordError(ctx.Err())
			break loop
		}
	}
	close(jobs)
	workers.Wait()
	sample()
	report.DurationSeconds = time.Since(started).Seconds()
	report.Completed = completed.Load()
	report.Errors = failed.Load()
	report.Dropped = dropped.Load()
	report.CompletionTPS = float64(report.Completed) / report.DurationSeconds
	report.LatencyMaxMS = maxLatency.Load()
	errorMu.Lock()
	report.FirstError = firstError
	report.ErrorDetails = errorDetails
	errorMu.Unlock()
	report.Metrics = metrics.Snapshot()
	percentile := func(p uint64) int64 {
		target := (report.Completed*p + 99) / 100
		var n uint64
		for i := range histogram {
			n += histogram[i].Load()
			if n >= target {
				return int64(i)
			}
		}
		return 60000
	}
	report.LatencyP50MS = percentile(50)
	report.LatencyP95MS = percentile(95)
	report.LatencyP99MS = percentile(99)
	raw, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(output, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if report.Errors != 0 || report.Dropped != 0 {
		// 超时后事务可能仍会提交，不能再用成功回复次数充当实体状态的精确期望值。
		t.Fatalf("load errors=%d dropped=%d first=%s; final consistency not verified", report.Errors, report.Dropped, report.FirstError)
	}
	if p := projector.Stats(); p.FatalProjectionConflicts != 0 {
		t.Errorf("fatal projection conflicts: %+v", p)
	}
	expected := make(map[int64]int64, len(ids))
	for i, id := range ids {
		expected[id] = counts[i].Load()
	}
	t.Logf("REMOTE_LOAD %s", raw)
	return expected
}
