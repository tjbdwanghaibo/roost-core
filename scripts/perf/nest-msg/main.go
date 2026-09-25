// Command nest-msg measures completed messages through the production Nest API.
// It is a load fixture, not a game/demo or a replacement dispatch implementation.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math"
	"os"
	"runtime"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tjbdwanghaibo/roost-core/entity"
	"github.com/tjbdwanghaibo/roost-core/nest"
)

type config struct {
	Mode         string        `json:"mode"`
	Distribution string        `json:"distribution"`
	Messages     int           `json:"messages"`
	Warmup       int           `json:"warmup"`
	Entities     int           `json:"entities"`
	Workers      int           `json:"workers"`
	Producers    int           `json:"producers"`
	Window       int           `json:"window_per_producer"`
	Queue        int           `json:"queue_per_worker"`
	SampleEvery  int           `json:"sample_every"`
	Timeout      time.Duration `json:"timeout_ns"`
}

func (c config) validate() error {
	if c.Mode != "dispatch" && c.Mode != "request" {
		return errors.New("mode must be dispatch or request")
	}
	if c.Distribution != "hot" && c.Distribution != "spread" {
		return errors.New("distribution must be hot or spread")
	}
	if c.Messages < 1 || c.Warmup < 0 || c.Entities < 1 || c.Entities > 100000 || c.Workers < 1 || c.Workers > 256 || c.Producers < 1 || c.Producers > 1024 || c.Window < 1 || c.Queue < 1 || c.SampleEvery < 1 || c.Timeout <= 0 {
		return errors.New("invalid load counts or timeout")
	}
	// 确保即使全部消息哈希到一个 worker，也不靠无界积压撑高准入数字。
	if c.Mode == "dispatch" && c.Window > c.Queue/c.Producers {
		return errors.New("producers * window must not exceed one worker queue capacity")
	}
	return nil
}

type loadEntity struct {
	*entity.EntityBase
	value uint64
}

func (e *loadEntity) Base() *entity.EntityBase { return e.EntityBase }

// 适配正式 EntityManager 的已加载对象；此压测不包含数据库 miss / load。
type loadedGetter struct{ manager *entity.EntityManager }

func (g loadedGetter) Get(_ context.Context, id int64, category entity.EntityCategory) (entity.IThreadSafeEntity, error) {
	return g.manager.GetWithCategory(id, category), nil
}
func (g loadedGetter) GetMany(ctx context.Context, ids []int64, categories []entity.EntityCategory) ([]entity.IThreadSafeEntity, error) {
	result := make([]entity.IThreadSafeEntity, len(ids))
	for i, id := range ids {
		result[i], _ = g.Get(ctx, id, categories[i])
	}
	return result, nil
}

type samples struct {
	mu        sync.Mutex
	durations []time.Duration
}

func (s *samples) record(start time.Time) {
	if start.IsZero() {
		return
	}
	duration := time.Since(start)
	s.mu.Lock()
	s.durations = append(s.durations, duration)
	s.mu.Unlock()
}

type producer struct {
	credits    chan struct{}
	accepted   uint64
	rejected   uint64
	failures   uint64
	firstError string
	completed  atomic.Uint64
}

type delivery struct {
	start   time.Time
	owner   *producer
	samples *samples
}

func (d *delivery) complete() {
	d.samples.record(d.start)
	d.owner.completed.Add(1)
	d.owner.credits <- struct{}{}
}

type result struct {
	Config             config  `json:"config"`
	GoVersion          string  `json:"go_version"`
	Platform           string  `json:"platform"`
	CPUCount           int     `json:"cpu_count"`
	GOMAXPROCS         int     `json:"gomaxprocs"`
	ElapsedSeconds     float64 `json:"elapsed_seconds"`
	CompletedPerSecond float64 `json:"completed_per_second"`
	Accepted           uint64  `json:"accepted"`
	Completed          uint64  `json:"completed"`
	Processed          uint64  `json:"processed"`
	QueueFull          uint64  `json:"queue_full"`
	Errors             uint64  `json:"errors"`
	FirstError         string  `json:"first_error,omitempty"`
	LatencySamples     int     `json:"latency_samples"`
	P50US              float64 `json:"p50_us"`
	P95US              float64 `json:"p95_us"`
	P99US              float64 `json:"p99_us"`
	MaxSampleUS        float64 `json:"max_sample_us"`
	BytesPerMessage    float64 `json:"bytes_per_message"`
	AllocsPerMessage   float64 `json:"allocs_per_message"`
	GCs                uint32  `json:"gc_cycles"`
	GCPauseMS          float64 `json:"gc_pause_ms"`
	Slow200MS          uint64  `json:"slow_dispatch_200ms"`
	Correct            bool    `json:"correct"`
}

func run(c config) (r result, err error) {
	if err = c.validate(); err != nil {
		return r, err
	}
	const kind = entity.EntityKind(30)
	const category = entity.EntityCategory(1)
	entity.MustRegisterEntityKindCategory(kind, category)
	manager := entity.NewEntityManager()
	entities := make([]*loadEntity, c.Entities)
	for i := range entities {
		id, buildErr := entity.BuildEntityID(int64(i+1), kind)
		if buildErr != nil {
			return r, buildErr
		}
		ent := &loadEntity{EntityBase: entity.NewEntityBase(id, category, true, kind)}
		if addErr := manager.TryAdd(ent); addErr != nil {
			return r, addErr
		}
		entities[i] = ent
	}
	engine := nest.NewEngine(nest.NestOptionWithGetter(loadedGetter{manager}), nest.NestOptionWithWorkerNumAndMsgCap(c.Workers, 0, c.Queue))
	name := nest.NewHandlerName("perf.message")
	engine.MustRegisterHandlerWithMeta(name, func(es []entity.IThreadSafeEntity, params []any, _ ...nest.HandlerOption) (any, error) {
		es[0].(*loadEntity).value++ // 真实 Entity 锁下的简单业务写入。
		if len(params) != 0 {
			d := params[0].(*delivery)
			entity.CurrentGuardScope().Guard().AppendPostRelease(d.complete)
		}
		return nil, nil
	}, nest.HandlerMeta{Rollback: nest.RollbackNone, Durability: nest.DurabilityMemory})
	if err = engine.Start(); err != nil {
		return r, err
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		err = errors.Join(err, engine.Shutdown(ctx))
	}()
	ctx, cancel := context.WithTimeout(context.Background(), c.Timeout)
	defer cancel()
	for i := range c.Warmup {
		if _, err = engine.Request(ctx, name, entities[i%len(entities)].ID(), nil); err != nil {
			return r, err
		}
	}
	// Request 的返回可能略早于 dispatch 统计收尾；正式计时前确认预热已经完全结束。
	for engine.Stats().Work.ProcessedMessages != uint64(c.Warmup) {
		if ctx.Err() != nil {
			return r, ctx.Err()
		}
		runtime.Gosched()
	}
	latency := &samples{durations: make([]time.Duration, 0, c.Messages/c.SampleEvery+c.Producers)}
	producers := make([]*producer, c.Producers)
	startGate := make(chan struct{})
	var wg sync.WaitGroup
	for p := range producers {
		state := &producer{credits: make(chan struct{}, c.Window)}
		for range c.Window {
			state.credits <- struct{}{}
		}
		producers[p] = state
		wg.Go(func() {
			<-startGate
			for sequence := p; sequence < c.Messages; sequence += c.Producers {
				if ctx.Err() != nil {
					state.failures++
					state.firstError = ctx.Err().Error()
					return
				}
				index := 0
				if c.Distribution == "spread" {
					index = sequence % len(entities)
				}
				id := entities[index].ID()
				var began time.Time
				// 各 producer 使用本地序号采样，避免 sampleEvery 整除 producer 数时只采到一个线程。
				sampled := (sequence/c.Producers)%c.SampleEvery == 0
				var callErr error
				if c.Mode == "dispatch" {
					select {
					case <-state.credits:
					case <-ctx.Done():
						state.failures++
						state.firstError = ctx.Err().Error()
						return
					}
					if sampled {
						began = time.Now()
					}
					d := &delivery{start: began, owner: state, samples: latency}
					callErr = engine.Dispatch(ctx, name, id, nest.NewParams(d))
					if callErr != nil {
						state.credits <- struct{}{}
					}
				} else {
					if sampled {
						began = time.Now()
					}
					_, callErr = engine.Request(ctx, name, id, nil)
					if callErr == nil {
						latency.record(began)
						state.completed.Add(1)
					}
				}
				if callErr == nil {
					state.accepted++
				} else if errors.Is(callErr, nest.ErrQueueFull) {
					state.rejected++
				} else {
					state.failures++
					if state.firstError == "" {
						state.firstError = callErr.Error()
					}
				}
			}
		})
	}
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	start := time.Now()
	close(startGate)
	wg.Wait()
	drainCtx, drainCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer drainCancel()
	if err = engine.Shutdown(drainCtx); err != nil {
		return r, err
	}
	elapsed := time.Since(start)
	runtime.ReadMemStats(&after)
	r = result{Config: c, GoVersion: runtime.Version(), Platform: runtime.GOOS + "/" + runtime.GOARCH, CPUCount: runtime.NumCPU(), GOMAXPROCS: runtime.GOMAXPROCS(0), ElapsedSeconds: elapsed.Seconds()}
	for _, p := range producers {
		r.Accepted += p.accepted
		r.Completed += p.completed.Load()
		r.QueueFull += p.rejected
		r.Errors += p.failures
		if r.FirstError == "" {
			r.FirstError = p.firstError
		}
	}
	var mutations uint64
	for _, e := range entities {
		mutations += e.value
	}
	stats := engine.Stats()
	r.Processed = stats.Work.ProcessedMessages - uint64(c.Warmup)
	r.Slow200MS = stats.Work.Slow200msMessages
	r.Correct = r.Errors == 0 && r.QueueFull == 0 && r.Accepted == uint64(c.Messages) && r.Completed == r.Accepted && r.Processed == r.Accepted && mutations == uint64(c.Warmup)+r.Accepted
	r.CompletedPerSecond = float64(r.Completed) / r.ElapsedSeconds
	if r.Completed > 0 {
		r.BytesPerMessage = float64(after.TotalAlloc-before.TotalAlloc) / float64(r.Completed)
		r.AllocsPerMessage = float64(after.Mallocs-before.Mallocs) / float64(r.Completed)
	}
	r.GCs = after.NumGC - before.NumGC
	r.GCPauseMS = float64(after.PauseTotalNs-before.PauseTotalNs) / 1e6
	slices.Sort(latency.durations)
	r.LatencySamples = len(latency.durations)
	quantile := func(q float64) float64 {
		if len(latency.durations) == 0 {
			return 0
		}
		return float64(latency.durations[int(math.Ceil(float64(len(latency.durations))*q))-1]) / 1e3
	}
	r.P50US, r.P95US, r.P99US, r.MaxSampleUS = quantile(.5), quantile(.95), quantile(.99), quantile(1)
	if !r.Correct {
		return r, errors.New("message count, mutation count or completion validation failed")
	}
	return r, nil
}

func main() {
	var c config
	flag.StringVar(&c.Mode, "mode", "dispatch", "dispatch or request")
	flag.StringVar(&c.Distribution, "distribution", "spread", "hot or spread")
	flag.IntVar(&c.Messages, "messages", 1000000, "measured messages")
	flag.IntVar(&c.Warmup, "warmup", 10000, "excluded warmup requests")
	flag.IntVar(&c.Entities, "entities", 10000, "loaded entities")
	flag.IntVar(&c.Workers, "workers", 4, "normal workers")
	flag.IntVar(&c.Producers, "producers", 32, "concurrent callers")
	flag.IntVar(&c.Window, "window", 32, "async in-flight messages per caller")
	flag.IntVar(&c.Queue, "queue", 4096, "queue capacity per worker")
	flag.IntVar(&c.SampleEvery, "sample-every", 64, "sample one latency per N messages per caller")
	flag.DurationVar(&c.Timeout, "timeout", 2*time.Minute, "workload deadline")
	flag.Parse()
	r, err := run(c)
	if encodeErr := json.NewEncoder(os.Stdout).Encode(r); encodeErr != nil {
		fmt.Fprintln(os.Stderr, encodeErr)
		os.Exit(1)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
