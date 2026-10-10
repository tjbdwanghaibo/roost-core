package nestgame

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/nest"
)

type gameConfig struct {
	Entities, Players, MessageTargets, MessagesPerPlayer, HeartbeatHz, DirtyPercent, Workers, SlowWorkers, Queue int
	Duration                                                                                                     time.Duration
}

func envInt(t *testing.T, key string, fallback int) int {
	t.Helper()
	if os.Getenv(key) == "" {
		return fallback
	}
	v, err := strconv.Atoi(os.Getenv(key))
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// 延迟直方图固定容量，每 16 次按序采样；超 50ms 的数量和最大值覆盖全部完成事件。
type timings struct {
	mu       sync.Mutex
	buckets  [10001]uint64 // 10us 一个桶，量化向上取整，最后一桶包含触顶及溢出样本。
	samples  uint64
	over50   atomic.Uint64
	negative atomic.Uint64
	max      atomic.Int64
}

func (h *timings) add(d time.Duration, sample bool) {
	if d < 0 {
		h.negative.Add(1)
	}
	for old := h.max.Load(); int64(d) > old; old = h.max.Load() {
		if h.max.CompareAndSwap(old, int64(d)) {
			break
		}
	}
	if d > 50*time.Millisecond {
		h.over50.Add(1)
	}
	if !sample {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	i := min(int((max(d, 0)+10*time.Microsecond-1)/(10*time.Microsecond)), len(h.buckets)-1)
	h.buckets[i]++
	h.samples++
}

type latencyResult struct {
	Samples, Over50MS, NegativeDurations uint64
	P50MS, P95MS, P99MS, MaxMS           float64
	SamplesAtCeiling                     uint64
}

func (h *timings) result() latencyResult {
	h.mu.Lock()
	defer h.mu.Unlock()
	q := func(p float64) float64 {
		var sum uint64
		want := uint64(math.Ceil(float64(h.samples) * p))
		for i, n := range h.buckets {
			sum += n
			if sum >= want {
				return float64(i) / 100
			}
		}
		return 100
	}
	return latencyResult{
		Samples: h.samples, Over50MS: h.over50.Load(), NegativeDurations: h.negative.Load(),
		P50MS: q(.5), P95MS: q(.95), P99MS: q(.99), MaxMS: float64(h.max.Load()) / float64(time.Millisecond),
		SamplesAtCeiling: h.buckets[len(h.buckets)-1],
	}
}

type loadCounters struct {
	planned, accepted, rejected                       uint64 // 只由 ticker 调度者写；调度完后读取。
	completed, changed                                atomic.Uint64
	firstError                                        string
	queueWait, handler, completion, changedCompletion timings
}
type streamResult struct {
	Planned, Accepted, Rejected, Completed, EntityChanges   uint64
	CompletedPerSecond                                      float64
	FirstError                                              string
	QueueWait, Handler, Completion, ChangedEntityCompletion latencyResult
}

func (c *loadCounters) result(elapsed float64) streamResult {
	return streamResult{
		Planned: c.planned, Accepted: c.accepted, Rejected: c.rejected, Completed: c.completed.Load(),
		EntityChanges: c.changed.Load(), CompletedPerSecond: float64(c.completed.Load()) / elapsed, FirstError: c.firstError,
		QueueWait: c.queueWait.result(), Handler: c.handler.result(), Completion: c.completion.result(), ChangedEntityCompletion: c.changedCompletion.result(),
	}
}

type invocation struct {
	planned, admitted time.Time
	change, sample    bool
	heartbeat         bool
}
type gameSample struct {
	Seconds              float64
	Messages, Heartbeats uint64
	HeapBytes            uint64
	Goroutines           int
	Nest                 nest.DispatcherStats
}
type gameResult struct {
	SyncReport                                          *syncLoadReport
	GateEnabled                                         bool
	GateCompleted                                       uint64
	GateRoundTrip                                       latencyResult
	Config                                              gameConfig
	ClockWallDriftNS                                    int64
	GoVersion, Platform                                 string
	GOMAXPROCS                                          int
	ElapsedSeconds                                      float64
	Messages, Heartbeats                                streamResult
	EntityChangesPerSecond                              float64
	AllocBytes, Allocs                                  uint64
	GCs                                                 uint32
	GCPauseMS                                           float64
	EntityValuesVerified, CountsVerified, LatencyPassed bool
	Samples                                             []gameSample
	FinalNest                                           nest.DispatcherStats
}

// 用商和余数计算，避免长时间高频负载在纳秒乘法中溢出。
func eventOffset(sequence uint64, rate int) time.Duration {
	return time.Duration(sequence/uint64(rate))*time.Second + time.Duration(sequence%uint64(rate))*time.Second/time.Duration(rate)
}

func TestNestGameLoad(t *testing.T) {
	output := os.Getenv("ROOST_NEST_GAME_OUTPUT")
	if output == "" {
		t.Skip("run scripts/perf/nest-game.sh")
	}
	c := gameConfig{
		Entities: envInt(t, "ROOST_NEST_GAME_ENTITIES", 10000), Players: envInt(t, "ROOST_NEST_GAME_PLAYERS", 1000),
		MessagesPerPlayer: envInt(t, "ROOST_NEST_GAME_MESSAGES_PER_PLAYER", 10), HeartbeatHz: envInt(t, "ROOST_NEST_GAME_HZ", 1),
		DirtyPercent: envInt(t, "ROOST_NEST_GAME_DIRTY", 1), Workers: envInt(t, "ROOST_NEST_GAME_WORKERS", 4), Queue: envInt(t, "ROOST_NEST_GAME_QUEUE", nest.DefaultFastQueueCapacity),
	}
	c.SlowWorkers = envInt(t, "ROOST_NEST_GAME_SLOW_WORKERS", 32)
	c.MessageTargets = envInt(t, "ROOST_NEST_GAME_MESSAGE_TARGETS", c.Players)
	var err error
	duration := os.Getenv("ROOST_NEST_GAME_DURATION")
	if duration == "" {
		duration = "1m"
	}
	c.Duration, err = time.ParseDuration(duration)
	if err != nil {
		t.Fatal(err)
	}
	if c.Entities < 1 || c.Entities > 100000 || c.Players < 1 || c.Players > c.Entities || c.MessageTargets < 1 || c.MessageTargets > c.Entities || c.MessagesPerPlayer < 0 || c.MessagesPerPlayer > 1000 || c.HeartbeatHz < 0 || c.HeartbeatHz > 1000 || c.DirtyPercent < 0 || c.DirtyPercent > 100 || c.Workers < 1 || c.SlowWorkers < 1 || c.Queue < 1 || c.Duration < time.Second || c.Duration > time.Hour || c.Duration%time.Second != 0 {
		t.Fatal("invalid load configuration")
	}
	RegisterEntity()
	access := entity.NewManagerAccess(entity.NewEntityManager())
	units := make([]*Unit, c.Entities)
	for i := range units {
		id, e := entity.BuildEntityID(int64(i+1), kindUnit)
		if e != nil {
			t.Fatal(e)
		}
		v, e := access.Create(&entity.EntityCreateParam{IsCreate: true, Kind: kindUnit, Id: id})
		if e != nil {
			t.Fatal(e)
		}
		units[i] = v.(*Unit)
		if units[i].heartbeat == nil || units[i].state == nil {
			t.Fatal("generated component/DAO missing")
		}
	}
	var stateSync *syncGameLoad
	options := []nest.NestOption{nest.NestOptionWithGetter(access), nest.NestOptionWithWorkerPools(nest.WorkerPoolConfig{Workers: c.Workers, QueueCap: c.Queue}, nest.WorkerPoolConfig{Workers: c.SlowWorkers}), nest.NestOptionWithTickDuration(10 * time.Millisecond)}
	if os.Getenv("ROOST_NEST_GAME_SYNC") == "1" {
		if os.Getenv("ROOST_NEST_GAME_GATE") != "1" {
			t.Fatal("Sync mixed load requires real Gate")
		}
		stateSync = newSyncGameLoad(t, units, c)
		options = append(options, nest.NestOptionWithEntitySync(stateSync.manager))
	}
	engine := nest.NewEngine(options...)
	var messages, heartbeats loadCounters
	var network *gateFrontend
	name := nest.NewHandlerName("perf.game.business")
	engine.MustRegisterHandlerWithMeta(name, func(es []entity.IThreadSafeEntity, params []any, _ ...nest.HandlerOption) (any, error) {
		in := params[0].(invocation)
		counters := &messages
		if in.heartbeat {
			counters = &heartbeats
		}
		began := time.Now()
		u := es[0].(*Unit)
		if in.heartbeat {
			u.heartbeat.Tick(in.change)
		} else {
			u.HandleMessage(in.change)
		}

		if in.change && stateSync != nil {
			if err := stateSync.changed(u, in); err != nil {
				return nil, err
			}
		}
		ended := time.Now()
		entity.CurrentGuardScope().Guard().AppendPostRelease(func() {
			completed := time.Now()
			counters.queueWait.add(began.Sub(in.admitted), in.sample)
			counters.handler.add(ended.Sub(began), in.sample)
			counters.completion.add(completed.Sub(in.planned), in.sample)
			if in.change {
				counters.changedCompletion.add(completed.Sub(in.planned), true)
				counters.changed.Add(1)
			}
			counters.completed.Add(1)
		})
		return nil, nil
	}, nest.HandlerMeta{Rollback: nest.RollbackUndo, Durability: nest.DurabilityMemory})
	ctx, cancel := context.WithTimeout(context.Background(), c.Duration+30*time.Second)
	defer cancel()
	// 唯一正式 ticker 只负责投递；组件、DAO 与 Guard 全部由快池处理。
	// 各实体的到期相位均匀分散，用绝对计划时间计算应投数量，ticker 变慢不能偷偷降低输入率。
	rates := [2]int{c.Players * c.MessagesPerPlayer, c.Entities * c.HeartbeatHz}
	counters := [2]*loadCounters{&messages, &heartbeats}
	expected := make([][2]int64, c.Entities)
	var start, end time.Time
	done := make(chan struct{})
	var stopped bool
	var samples []gameSample
	lastSample := -1
	if err := nest.RegisterTickCallback(nest.NewTickCallbackName("perf.game.schedule"), func(_ nest.TickMsg) {
		if stopped || time.Now().Before(start) {
			return
		}
		now := time.Now()
		elapsed := min(now.Sub(start), c.Duration)
		var due [2]uint64
		for k, rate := range rates {
			due[k] = uint64(elapsed/time.Second)*uint64(rate) + uint64(elapsed%time.Second)*uint64(rate)/uint64(time.Second)
		}
		for messages.planned < due[0] || heartbeats.planned < due[1] {
			k := 0
			if messages.planned >= due[0] {
				k = 1
			} else if heartbeats.planned < due[1] && eventOffset(heartbeats.planned+1, rates[1]) < eventOffset(messages.planned+1, rates[0]) {
				k = 1
			}
			counter := counters[k]
			sequence := counter.planned
			counter.planned++
			targets := c.MessageTargets
			if k == 1 {
				targets = c.Entities
			}
			index := int(sequence % uint64(targets))
			round := sequence / uint64(targets)
			change := (uint64(index)+round)%100 < uint64(c.DirtyPercent)
			planned := start.Add(eventOffset(sequence+1, rates[k]))
			in := invocation{planned: planned, admitted: time.Now(), change: change, sample: sequence%16 == 0, heartbeat: k == 1}
			var dispatchErr error
			if k == 0 && network != nil {
				dispatchErr = network.dispatch(index, in)
			} else {
				dispatchErr = engine.Dispatch(ctx, name, units[index].ID(), nest.NewParams(in))
			}
			if err := dispatchErr; err != nil {
				counter.rejected++
				if counter.firstError == "" {
					counter.firstError = err.Error()
					// 只在首个已发生的拒绝后采诊断，不能给健康样本持续加栈采样开销。
					t.Logf("first admission failure: heartbeat=%v elapsed=%s planned_lag=%s due=%v planned=[%d %d] completed=[%d %d] nest=%+v err=%v", k == 1, now.Sub(start), time.Since(planned), due, messages.planned, heartbeats.planned, messages.completed.Load(), heartbeats.completed.Load(), engine.Stats(), err)
					stack := make([]byte, 16<<20)
					n := runtime.Stack(stack, true)
					if saveErr := os.WriteFile(fmt.Sprintf("%s.failure-%d.stack", output, k), stack[:n], 0600); saveErr != nil {
						t.Logf("save first failure stack: %v", saveErr)
					}
				}
			} else {
				counter.accepted++
				if change {
					expected[index][k]++
				}
			}
		}
		second := int(elapsed / time.Second)
		if second != lastSample {
			var m runtime.MemStats
			runtime.ReadMemStats(&m)
			samples = append(samples, gameSample{elapsed.Seconds(), messages.completed.Load(), heartbeats.completed.Load(), m.HeapAlloc, runtime.NumGoroutine(), engine.Stats()})
			lastSample = second
		}
		if !now.Before(end) {
			stopped = true
			close(done)
		}
	}); err != nil {
		t.Fatal(err)
	}
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	if os.Getenv("ROOST_NEST_GAME_GATE") == "1" {
		if c.MessageTargets != c.Players {
			t.Fatal("Gate load requires one declared player target per connection")
		}
		network = newGateFrontend(t, engine, name, units, c, stateSync)
		if stateSync != nil {
			stateSync.start(t, network)
		}
	}
	start = time.Now().Add(100 * time.Millisecond)
	end = start.Add(c.Duration)
	if err := engine.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		stop, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = engine.Shutdown(stop)
	})
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if network != nil {
		if err := network.drain(); err != nil {
			t.Error(err)
		}
	}
	stop, stopCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer stopCancel()
	if err := engine.Shutdown(stop); err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(start).Seconds()
	var syncReport *syncLoadReport
	if stateSync != nil {
		syncReport = stateSync.finish(t, network)
	}
	runtime.ReadMemStats(&after)
	r := gameResult{
		Config: c, GoVersion: runtime.Version(), Platform: runtime.GOOS + "/" + runtime.GOARCH, GOMAXPROCS: runtime.GOMAXPROCS(0),
		ElapsedSeconds: elapsed, Messages: messages.result(elapsed), Heartbeats: heartbeats.result(elapsed),
		EntityValuesVerified: true, CountsVerified: true, LatencyPassed: true, Samples: samples, FinalNest: engine.Stats(),
		AllocBytes: after.TotalAlloc - before.TotalAlloc, Allocs: after.Mallocs - before.Mallocs,
		GCs: after.NumGC - before.NumGC, GCPauseMS: float64(after.PauseTotalNs-before.PauseTotalNs) / 1e6,
	}
	clockNow := time.Now()
	r.ClockWallDriftNS = clockNow.UnixNano() - loadClockOrigin.UnixNano() - loadTimestamp(clockNow)
	r.SyncReport = syncReport
	if syncReport != nil {
		r.EntityValuesVerified = r.EntityValuesVerified && syncReport.ValuesVerified
		r.LatencyPassed = r.LatencyPassed && syncReport.LatencyPassed
	}
	if network != nil {
		r.GateEnabled = true
		r.GateCompleted = network.completed.Load()
		r.GateRoundTrip = network.roundTrip.result()
		if r.GateCompleted != r.Messages.Completed {
			r.CountsVerified = false
		}
		if r.GateRoundTrip.P99MS > 50 {
			r.LatencyPassed = false
		}
	}
	for i, u := range units {
		if u.state.GetMessages() != expected[i][0] || u.state.GetHeartbeats() != expected[i][1] || u.state.GetX() != expected[i][0] || u.state.GetHP() != expected[i][1]%100 {
			r.EntityValuesVerified = false
		}
	}
	for k, stream := range []streamResult{r.Messages, r.Heartbeats} {
		want := uint64(c.Duration/time.Second) * uint64(rates[k])
		if stream.Planned != want || stream.Accepted != want || stream.Completed != want || stream.Rejected != 0 {
			r.CountsVerified = false
		}
		if stream.Completed > 0 && (stream.Completion.P99MS > 50 || stream.ChangedEntityCompletion.P99MS > 50 || stream.Completion.NegativeDurations > 0 || stream.ChangedEntityCompletion.NegativeDurations > 0 || stream.QueueWait.NegativeDurations > 0) {
			r.LatencyPassed = false
		}
	}
	if r.FinalNest.Work.ProcessedMessages != r.Messages.Completed+r.Heartbeats.Completed {
		r.CountsVerified = false
	}
	r.EntityChangesPerSecond = float64(r.Messages.EntityChanges+r.Heartbeats.EntityChanges) / elapsed
	raw, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(output, append(raw, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	fmt.Printf("messages=%.1f/s heartbeat=%.1f/s changes=%.1f/s p99=%.3f/%.3fms counts=%v values=%v latency=%v\n", r.Messages.CompletedPerSecond, r.Heartbeats.CompletedPerSecond, r.EntityChangesPerSecond, r.Messages.Completion.P99MS, r.Heartbeats.Completion.P99MS, r.CountsVerified, r.EntityValuesVerified, r.LatencyPassed)
	if !r.CountsVerified || !r.EntityValuesVerified || !r.LatencyPassed {
		t.Fatal("business load gate failed; full result retained")
	}
}
