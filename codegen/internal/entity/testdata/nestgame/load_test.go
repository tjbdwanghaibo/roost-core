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
	"github.com/tjbdwanghaibo/roost-core/infra/network/gateway"
	gatewiring "github.com/tjbdwanghaibo/roost-core/wiring/gate"
)

type gameConfig struct {
	Entities, Players, MessageTargets, MessagesPerPlayer, HeartbeatHz, DirtyPercent, Workers, IOWorkers, Queue int
	Duration                                                                                                   time.Duration
	// Frontend 是普通业务的入口：direct（ticker 直接投递 Nest）、gate（ROOST_NEST_GAME_GATE=1）、
	// embedded（ROOST_NEST_GAME_EMBEDDED=1）。SceneSync / SelfSync 对应 ROOST_NEST_GAME_SYNC / _SELF_SYNC。
	Frontend            string
	SceneSync, SelfSync bool
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
	admitLag, clientQueue, serverQueue, serverDone    timings
}

// streamResult 的 Completion / QueueWait 从计划时刻 / 投递时刻起算，混有压测端自身的等待，
// 保留它们是为了和历史样本同口径比较。Harness 与 Server 把同一段时间拆开：
//
//	planned ─Harness.AdmitLag─▶ admitted ─Harness.ClientQueue─▶ sent ─Server.QueueWait─▶ began ─handler─▶ released
//	                                                              └──────────── Server.Completion ────────────┘
//
// sent 是模拟客户端把请求写进 TCP 之前的时刻；不经 Gate 的直接投递没有客户端，sent 等于 admitted。
type streamResult struct {
	Planned, Accepted, Rejected, Completed, EntityChanges   uint64
	CompletedPerSecond                                      float64
	FirstError                                              string
	QueueWait, Handler, Completion, ChangedEntityCompletion latencyResult
	Harness                                                 harnessResult
	Server                                                  serverResult
}

// harnessResult 是压测框架自己引入的等待，不计入服务器延迟。
type harnessResult struct {
	// AdmitLag：计划时刻到 ticker 投递。发生器只在 Nest 10ms tick 里批量投递，天然 0～10ms，
	// 再加上同一 tick 内排在前面的投递耗时。
	AdmitLag latencyResult
	// ClientQueue：投递到模拟客户端写出请求。每个玩家连接一次只有一条在途请求，
	// 这段包含等前一条回包的时间，所以会间接受到前一条请求服务端耗时的影响。
	ClientQueue latencyResult
}

// serverResult 是请求离开模拟客户端之后的耗时：Gate、NATS、Game 接入、Nest 排队、handler 与释放。
type serverResult struct {
	QueueWait, Completion latencyResult
}

func (c *loadCounters) result(elapsed float64) streamResult {
	return streamResult{
		Planned: c.planned, Accepted: c.accepted, Rejected: c.rejected, Completed: c.completed.Load(),
		EntityChanges: c.changed.Load(), CompletedPerSecond: float64(c.completed.Load()) / elapsed, FirstError: c.firstError,
		QueueWait: c.queueWait.result(), Handler: c.handler.result(), Completion: c.completion.result(), ChangedEntityCompletion: c.changedCompletion.result(),
		Harness: harnessResult{AdmitLag: c.admitLag.result(), ClientQueue: c.clientQueue.result()},
		Server:  serverResult{QueueWait: c.serverQueue.result(), Completion: c.serverDone.result()},
	}
}

type invocation struct {
	planned, admitted, sent time.Time
	change, sample          bool
	heartbeat               bool
}
type gameSample struct {
	Seconds              float64
	Messages, Heartbeats uint64
	HeapBytes            uint64
	Goroutines           int
	Nest                 nest.DispatcherStats
	// Outbound 只在嵌入 TCP 模式下采（每秒一次 metrics 快照），其他模式不改变采样开销。
	Outbound *outboundCounters `json:",omitempty"`
}

// minuteSummary 是按负载分钟汇总的每秒采样（离线计算，不在被测区间内）。
// 速率与增量取本分钟最后一个采样减上一分钟最后一个采样；ShortAvgWorkerWaitUS = ΔWorkerWait / ΔStarted。
type minuteSummary struct {
	Minute, Samples                        int
	MessagesPerSecond, HeartbeatsPerSecond float64
	HeapMinMB, HeapAvgMB, HeapMaxMB        float64
	GoroutinesMin, GoroutinesMax           int
	ShortAvgWorkerWaitUS                   float64
	// ShortPeakWaiting / ShortMaxWorkerWaitMS 是 Nest 的累计峰值，在本分钟末的读数。
	ShortPeakWaiting     int
	ShortMaxWorkerWaitMS float64
	Slow200msMessages    uint64
	// Outbound 是本分钟的嵌入 TCP 出站计数增量（StrandedPlayers 取本分钟最大值）。
	Outbound *outboundCounters `json:",omitempty"`
}

func summarizeMinutes(samples []gameSample, duration time.Duration, outboundStart *outboundCounters) []minuteSummary {
	if len(samples) == 0 {
		return nil
	}
	minutes := int((duration + time.Minute - 1) / time.Minute)
	result := make([]minuteSummary, 0, minutes)
	previous := samples[0]
	var previousOutbound outboundCounters
	if outboundStart != nil {
		previousOutbound = *outboundStart
	}
	for begin := 0; begin < len(samples); {
		minute := min(int(samples[begin].Seconds/60), minutes-1)
		end := begin
		for end < len(samples) && min(int(samples[end].Seconds/60), minutes-1) == minute {
			end++
		}
		group := samples[begin:end]
		last := group[len(group)-1]
		m := minuteSummary{Minute: minute, Samples: len(group), HeapMinMB: math.MaxFloat64, GoroutinesMin: math.MaxInt}
		var heap float64
		stranded := int64(0)
		for _, sample := range group {
			mb := float64(sample.HeapBytes) / (1 << 20)
			heap += mb
			m.HeapMinMB, m.HeapMaxMB = min(m.HeapMinMB, mb), max(m.HeapMaxMB, mb)
			m.GoroutinesMin, m.GoroutinesMax = min(m.GoroutinesMin, sample.Goroutines), max(m.GoroutinesMax, sample.Goroutines)
			if sample.Outbound != nil {
				stranded = max(stranded, sample.Outbound.StrandedPlayers)
			}
		}
		m.HeapAvgMB = heap / float64(len(group))
		if seconds := last.Seconds - previous.Seconds; seconds > 0 {
			m.MessagesPerSecond = float64(last.Messages-previous.Messages) / seconds
			m.HeartbeatsPerSecond = float64(last.Heartbeats-previous.Heartbeats) / seconds
		}
		short, previousShort := last.Nest.Queue.Short, previous.Nest.Queue.Short
		if started := short.Started - previousShort.Started; started > 0 {
			m.ShortAvgWorkerWaitUS = float64(short.WorkerWait-previousShort.WorkerWait) / float64(started) / float64(time.Microsecond)
		}
		m.ShortPeakWaiting = short.PeakWaiting
		m.ShortMaxWorkerWaitMS = float64(short.MaxWorkerWait) / float64(time.Millisecond)
		m.Slow200msMessages = last.Nest.Work.Slow200msMessages - previous.Nest.Work.Slow200msMessages
		if last.Outbound != nil {
			delta := last.Outbound.sub(previousOutbound)
			delta.StrandedPlayers = stranded
			m.Outbound = &delta
			previousOutbound = *last.Outbound
		}
		result = append(result, m)
		previous = last
		begin = end
	}
	return result
}

type gameResult struct {
	SyncReport *syncLoadReport
	// Embedded 只在嵌入 TCP 模式下有，见 embedded_load_test.go。
	Embedded                                            *embeddedReport `json:",omitempty"`
	Minutes                                             []minuteSummary
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
		DirtyPercent: envInt(t, "ROOST_NEST_GAME_DIRTY", 1), Workers: envInt(t, "ROOST_NEST_GAME_SHORT_WORKERS", 4), Queue: envInt(t, "ROOST_NEST_GAME_SHORT_QUEUE", nest.DefaultShortQueueCapacity),
	}
	c.IOWorkers = envInt(t, "ROOST_NEST_GAME_IO_WORKERS", 32)
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
	c.Frontend = "direct"
	switch gate, embedded := os.Getenv("ROOST_NEST_GAME_GATE") == "1", os.Getenv("ROOST_NEST_GAME_EMBEDDED") == "1"; {
	case gate && embedded:
		t.Fatal("ROOST_NEST_GAME_GATE and ROOST_NEST_GAME_EMBEDDED are two deployments of the player access; pick one")
	case gate:
		c.Frontend = "gate"
	case embedded:
		c.Frontend = "embedded"
	}
	c.SceneSync, c.SelfSync = os.Getenv("ROOST_NEST_GAME_SYNC") == "1", os.Getenv("ROOST_NEST_GAME_SELF_SYNC") == "1"
	switch {
	case c.SceneSync && c.Frontend == "direct":
		t.Fatal("Sync mixed load requires real Gate or embedded TCP")
	case c.SelfSync && c.Frontend != "embedded":
		t.Fatal("self_sync load runs over embedded TCP only (ROOST_NEST_GAME_EMBEDDED=1)")
	case c.SelfSync && c.SceneSync:
		// 一个 SubjectSyncState 只能登记在一个 Manager（entitysync.ErrSubjectRegisteredElsewhere）；生产同样不支持
		// 场景与自身视图同时覆盖玩家实体（PLAYER-ACCESS §7）。
		t.Fatal("ROOST_NEST_GAME_SELF_SYNC and ROOST_NEST_GAME_SYNC cannot both cover the player units: a subject feeds one entity sync manager")
	}
	if c.Entities < 1 || c.Entities > 100000 || c.Players < 1 || c.Players > c.Entities || c.MessageTargets < 1 || c.MessageTargets > c.Entities || c.MessagesPerPlayer < 0 || c.MessagesPerPlayer > 1000 || c.HeartbeatHz < 0 || c.HeartbeatHz > 1000 || c.DirtyPercent < 0 || c.DirtyPercent > 100 || c.Workers < 1 || c.IOWorkers < 1 || c.Queue < 1 || c.Duration < time.Second || c.Duration > time.Hour || c.Duration%time.Second != 0 {
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
	options := []nest.NestOption{nest.NestOptionWithGetter(access), nest.NestOptionWithWorkerPools(nest.WorkerPoolConfig{Workers: c.Workers, QueueCap: c.Queue}, nest.WorkerPoolConfig{}, nest.WorkerPoolConfig{Workers: c.IOWorkers}), nest.NestOptionWithTickDuration(10 * time.Millisecond)}
	// 嵌入模式的 TCPRuntime 先于 Sync 创建：场景 Sync 的 Transport 与自身视图都推给它（生产由 player TCP Mod 在 Provide 时注册）。
	var tcpRuntime *gateway.TCPRuntime
	if c.Frontend == "embedded" {
		tcpRuntime = gateway.NewTCPRuntime(nil, gateway.DefaultTCPConfig().LoginTimeout)
	}
	switch {
	case c.SelfSync:
		stateSync = newSelfSyncGameLoad(t, units, c, access)
	case c.SceneSync && c.Frontend == "gate":
		lane := &gatewiring.Transport{SyncMessageID: 501}
		stateSync = newSyncGameLoad(t, units, c, lane)
		stateSync.gateTransport = lane
	case c.SceneSync:
		stateSync = newSyncGameLoad(t, units, c, embeddedSceneLane{players: tcpRuntime})
	}
	if stateSync != nil {
		options = append(options, nest.NestOptionWithEntitySync(stateSync.manager))
	}
	engine := nest.NewEngine(options...)
	var messages, heartbeats loadCounters
	var network loadFrontend
	var gateNetwork *gateFrontend
	var embeddedNetwork *embeddedFrontend
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
			counters.admitLag.add(in.admitted.Sub(in.planned), in.sample)
			counters.clientQueue.add(in.sent.Sub(in.admitted), in.sample)
			counters.serverQueue.add(began.Sub(in.sent), in.sample)
			counters.serverDone.add(completed.Sub(in.sent), in.sample)
			if c := tail.Load(); c != nil {
				stream := "business"
				if in.heartbeat {
					stream = "heartbeat"
				}
				c.observe(stream, completed.Sub(in.planned), tailPart{"admit", in.admitted.Sub(in.planned)}, tailPart{"client", in.sent.Sub(in.admitted)}, tailPart{"queue", began.Sub(in.sent)}, tailPart{"handler", ended.Sub(began)}, tailPart{"release", completed.Sub(ended)})
			}
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
	// 唯一正式 ticker 只负责投递；组件、DAO 与 Guard 全部由业务池处理。
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
		if c := tail.Load(); c != nil {
			c.observeTick(now)
		}
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
			admitted := time.Now()
			in := invocation{planned: planned, admitted: admitted, sent: admitted, change: change, sample: sequence%16 == 0, heartbeat: k == 1}
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
			sample := gameSample{elapsed.Seconds(), messages.completed.Load(), heartbeats.completed.Load(), m.HeapAlloc, runtime.NumGoroutine(), engine.Stats(), nil}
			if embeddedNetwork != nil {
				outbound := readOutboundCounters()
				sample.Outbound = &outbound
			}
			samples = append(samples, sample)
			lastSample = second
		}
		if !now.Before(end) {
			stopped = true
			close(done)
		}
	}); err != nil {
		t.Fatal(err)
	}
	collector := startTailCollector(t, output)
	tail.Store(collector)
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	if c.Frontend != "direct" && c.MessageTargets != c.Players {
		t.Fatal("Gate / embedded TCP load requires one declared player target per connection")
	}
	switch c.Frontend {
	case "gate":
		gateNetwork = newGateFrontend(t, engine, name, units, c, stateSync)
		network = gateNetwork
	case "embedded":
		embeddedNetwork = newEmbeddedFrontend(t, engine, name, units, c, tcpRuntime, stateSync)
		network = embeddedNetwork
	}
	if stateSync != nil {
		stateSync.start(t, network)
	}
	var embedded *embeddedReport
	if embeddedNetwork != nil {
		embedded = &embeddedReport{SceneSync: c.SceneSync, SelfSync: c.SelfSync, MaxPayloadBytes: embeddedNetwork.tcp.MaxPayloadBytes,
			OutboundResidentBytes: embeddedNetwork.tcp.OutboundResidentBytes, WriteTimeoutMS: float64(embeddedNetwork.tcp.WriteTimeout) / float64(time.Millisecond),
			GoroutinesAtStart: countTCPGoroutines(t), CountersAtStart: readOutboundCounters(),
			QueuePeak: "not exported: per-connection queue depth lives in unexported gateway.tcpSession and there is no resident-bytes gauge (KNOWN-LIMITS embedded TCP outbound)"}
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
	if embedded != nil {
		embedded.GoroutinesAtEnd = countTCPGoroutines(t)
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
	collector.finish(t)
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
	var outboundStart *outboundCounters
	if embedded != nil {
		embedded.CountersDelta = readOutboundCounters().sub(embedded.CountersAtStart)
		delta := embedded.CountersDelta
		embedded.OutboundClean = delta.BudgetExhausted == 0 && delta.Overflow == 0 && delta.PushClosed == 0 && delta.WriteErrors == 0 && delta.PushRefusedRetried == 0 && delta.PushRefusedClosed == 0
		embedded.Completed = embeddedNetwork.completed.Load()
		embedded.RoundTrip = embeddedNetwork.roundTrip.result()
		if embedded.Completed != r.Messages.Completed {
			r.CountsVerified = false
		}
		if embedded.RoundTrip.P99MS > 50 {
			r.LatencyPassed = false
		}
		r.Embedded = embedded
		outboundStart = &embedded.CountersAtStart
	}
	r.Minutes = summarizeMinutes(samples, c.Duration, outboundStart)
	if syncReport != nil {
		r.EntityValuesVerified = r.EntityValuesVerified && syncReport.ValuesVerified
		r.LatencyPassed = r.LatencyPassed && syncReport.LatencyPassed
	}
	if gateNetwork != nil {
		r.GateEnabled = true
		r.GateCompleted = gateNetwork.completed.Load()
		r.GateRoundTrip = gateNetwork.roundTrip.result()
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
		if stream.Completed > 0 && (stream.Completion.P99MS > 50 || stream.ChangedEntityCompletion.P99MS > 50 || stream.Completion.NegativeDurations > 0 || stream.ChangedEntityCompletion.NegativeDurations > 0 || stream.QueueWait.NegativeDurations > 0 ||
			stream.Harness.AdmitLag.NegativeDurations > 0 || stream.Harness.ClientQueue.NegativeDurations > 0 || stream.Server.QueueWait.NegativeDurations > 0 || stream.Server.Completion.NegativeDurations > 0) {
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
	fmt.Printf("messages=%.1f/s heartbeat=%.1f/s changes=%.1f/s p99=%.3f/%.3fms server_p99=%.3f/%.3fms counts=%v values=%v latency=%v\n", r.Messages.CompletedPerSecond, r.Heartbeats.CompletedPerSecond, r.EntityChangesPerSecond, r.Messages.Completion.P99MS, r.Heartbeats.Completion.P99MS, r.Messages.Server.Completion.P99MS, r.Heartbeats.Server.Completion.P99MS, r.CountsVerified, r.EntityValuesVerified, r.LatencyPassed)
	if e := r.Embedded; e != nil {
		d := e.CountersDelta
		fmt.Printf("embedded scene_sync=%v self_sync=%v roundtrip_p99=%.3fms budget_exhausted=%d push_refused=retried:%d/closed:%d overflow=%d push_closed=%d write_errors=%d write_loops=%d->%d outbound_clean=%v\n",
			e.SceneSync, e.SelfSync, e.RoundTrip.P99MS, d.BudgetExhausted, d.PushRefusedRetried, d.PushRefusedClosed, d.Overflow, d.PushClosed, d.WriteErrors, e.GoroutinesAtStart.WriteLoops, e.GoroutinesAtEnd.WriteLoops, e.OutboundClean)
	}
	if !r.CountsVerified || !r.EntityValuesVerified || !r.LatencyPassed {
		t.Fatal("business load gate failed; full result retained")
	}
}
