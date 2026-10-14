package nestgame

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"runtime/metrics"
	"runtime/trace"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// 长尾采集（诊断用，默认关闭）。ROOST_NEST_GAME_TAIL 选择模式：
//   - metrics：每秒采 runtime/metrics 与进程 rusage，记录每个超过阈值的完成事件；不开 runtime trace。
//   - trace：在 metrics 之上开 Go flight recorder；出现长尾事件后延迟 POST 保存最近窗口的 trace，
//     并按 BASELINE_EVERY 固定间隔保存对照窗口。长尾事件同时以 trace.Log（类别 roost.tail）写进
//     trace，分析工具 scripts/perf/tailtrace 用它把事件对到同一时钟上的 GC / STW / 调度 / 阻塞。
//
// 采集有观测开销（trace 模式尤其明显），结果只用于归因，不能与无采集样本的延迟数字混用。
// 热路径只多一次 nil 判断；只有超过阈值的事件才格式化消息并非阻塞投递给采集 goroutine，
// 采集 goroutine 落后时丢弃并计数，绝不反压业务。
//
// 其余变量：ROOST_NEST_GAME_TAIL_THRESHOLD（默认 50ms）、_TAIL_WINDOW（flight recorder 最小
// 保留时长，默认 5s）、_TAIL_MAX_BYTES（默认 128MiB）、_TAIL_POST（事件后再等多久保存，默认 1s）、
// _TAIL_MAX_CAPTURES（长尾 trace 上限，默认 12）、_TAIL_BASELINE_EVERY（对照 trace 间隔，默认 0 关闭）。
// 产物写在 ROOST_NEST_GAME_OUTPUT 去掉 .json 后加 .tail/ 的目录。

// tail 由 TestNestGameLoad 在负载开始前设置；用原子指针是因为部分读取方 goroutine 先于它创建。
var tail atomic.Pointer[tailCollector]

type tailEvent struct {
	Stream string `json:"stream"`
	// AtS 是完成时刻，相对 loadClockOrigin 的单调秒数；与 runtime-metrics.jsonl 的 at_s 同一时钟。
	AtS     float64            `json:"at_s"`
	TotalMS float64            `json:"total_ms"`
	Parts   map[string]float64 `json:"parts_ms"`
	// Capture 是覆盖该事件的长尾 trace 编号；-1 表示已达上限或未开 trace。
	Capture int `json:"capture"`
	// DuringCapture 表示事件窗口与某次 trace 保存重叠，归因时应排除（保存本身可能造成抖动）。
	DuringCapture bool `json:"during_capture,omitempty"`
}

type tailPart struct {
	name string
	d    time.Duration
}

type tailCollector struct {
	mode      string
	dir       string
	threshold time.Duration
	post      time.Duration
	maxCaps   int
	baseline  time.Duration
	recorder  *trace.FlightRecorder

	events  chan tailEvent
	dropped atomic.Uint64
	seen    atomic.Uint64
	stop    chan struct{}
	done    sync.WaitGroup

	// 以下只由采集 goroutine 访问。
	captures     int
	pending      int       // 待保存的长尾 trace 编号，-1 表示没有
	lastTick     time.Time // 只由调度 ticker goroutine 访问
	saving       [][2]float64
	saveFailures []string
}

func envDuration(t *testing.T, key string, fallback time.Duration) time.Duration {
	t.Helper()
	raw := os.Getenv(key)
	if raw == "" {
		return fallback
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d < 0 {
		t.Fatalf("%s=%q: %v", key, raw, err)
	}
	return d
}

// startTailCollector 按环境变量启动采集；未开启时返回 nil，负载代码路径不变。
func startTailCollector(t *testing.T, output string) *tailCollector {
	t.Helper()
	mode := os.Getenv("ROOST_NEST_GAME_TAIL")
	if mode == "" {
		return nil
	}
	if mode != "metrics" && mode != "trace" {
		t.Fatalf("ROOST_NEST_GAME_TAIL=%q: want metrics or trace", mode)
	}
	c := &tailCollector{
		mode:      mode,
		dir:       strings.TrimSuffix(output, ".json") + ".tail",
		threshold: envDuration(t, "ROOST_NEST_GAME_TAIL_THRESHOLD", 50*time.Millisecond),
		post:      envDuration(t, "ROOST_NEST_GAME_TAIL_POST", time.Second),
		maxCaps:   envInt(t, "ROOST_NEST_GAME_TAIL_MAX_CAPTURES", 12),
		baseline:  envDuration(t, "ROOST_NEST_GAME_TAIL_BASELINE_EVERY", 0),
		events:    make(chan tailEvent, 4096),
		stop:      make(chan struct{}),
		pending:   -1,
	}
	window := envDuration(t, "ROOST_NEST_GAME_TAIL_WINDOW", 5*time.Second)
	if c.threshold <= 0 || window <= c.post || c.maxCaps < 0 {
		t.Fatal("invalid tail collector configuration: need threshold>0, window>post, max captures>=0")
	}
	if err := os.MkdirAll(c.dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if mode == "trace" {
		c.recorder = trace.NewFlightRecorder(trace.FlightRecorderConfig{
			MinAge:   window,
			MaxBytes: uint64(envInt(t, "ROOST_NEST_GAME_TAIL_MAX_BYTES", 128<<20)),
		})
		if err := c.recorder.Start(); err != nil {
			t.Fatal(err)
		}
	}
	c.done.Add(2)
	go c.sampleRuntime()
	go c.collect()
	return c
}

func tailNow() float64 { return time.Since(loadClockOrigin).Seconds() }

// observe 在业务完成路径上调用；未超过阈值时只做一次比较。
func (c *tailCollector) observe(stream string, total time.Duration, parts ...tailPart) {
	if c == nil || total <= c.threshold {
		return
	}
	c.observeAbove(stream, total, parts...)
}

func (c *tailCollector) observeAbove(stream string, total time.Duration, parts ...tailPart) {
	c.seen.Add(1)
	ev := tailEvent{Stream: stream, AtS: tailNow(), TotalMS: ms(total), Parts: make(map[string]float64, len(parts))}
	if c.recorder != nil {
		var b strings.Builder
		fmt.Fprintf(&b, "stream=%s total_us=%d", stream, total.Microseconds())
		for _, p := range parts {
			fmt.Fprintf(&b, " %s_us=%d", p.name, p.d.Microseconds())
		}
		trace.Log(context.Background(), "roost.tail", b.String())
	}
	for _, p := range parts {
		ev.Parts[p.name] = ms(p.d)
	}
	select {
	case c.events <- ev:
	default:
		c.dropped.Add(1)
	}
}

// observeTick 由唯一的调度 ticker goroutine 调用：相邻两次回调间隔超过阈值一半（正常 10ms）时
// 记为 tick 流事件，让分析能在 trace 中认出 ticker goroutine，并区分“投递晚了”与“排队 / 执行慢”。
func (c *tailCollector) observeTick(now time.Time) {
	last := c.lastTick
	c.lastTick = now
	if last.IsZero() {
		return
	}
	if gap := now.Sub(last); gap > c.threshold/2 {
		c.observeAbove("tick", gap)
	}
}

func ms(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }

// collect 写事件索引并按需保存 trace；保存发生在这里，不在业务 goroutine。
func (c *tailCollector) collect() {
	defer c.done.Done()
	f, err := os.Create(filepath.Join(c.dir, "tail-events.jsonl"))
	if err != nil {
		c.saveFailures = append(c.saveFailures, err.Error())
		return
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	var postTimer <-chan time.Time
	var baseTimer <-chan time.Time
	baseCount := 0
	if c.recorder != nil && c.baseline > 0 {
		baseTimer = time.After(c.baseline)
	}
	for {
		select {
		case ev := <-c.events:
			ev.Capture = -1
			for _, w := range c.saving {
				if ev.AtS-ev.TotalMS/1000 <= w[1] && ev.AtS >= w[0] {
					ev.DuringCapture = true
				}
			}
			if c.recorder != nil {
				switch {
				case c.pending >= 0:
					ev.Capture = c.pending
				case ev.Stream != "tick" && c.captures < c.maxCaps: // tick 只附着在已有窗口上，不单独占用名额
					c.pending = c.captures
					c.captures++
					ev.Capture = c.pending
					postTimer = time.After(c.post)
				}
			}
			_ = enc.Encode(ev)
		case <-postTimer:
			postTimer = nil
			c.save(fmt.Sprintf("tail-%02d.trace", c.pending))
			c.pending = -1
		case <-baseTimer:
			c.save(fmt.Sprintf("base-%02d.trace", baseCount))
			baseCount++
			baseTimer = time.After(c.baseline)
		case <-c.stop:
			// 收尾：排空已投递事件；未到保存时刻的长尾 trace 立即保存。
			for len(c.events) > 0 {
				ev := <-c.events
				ev.Capture = c.pending
				_ = enc.Encode(ev)
			}
			if c.pending >= 0 {
				c.save(fmt.Sprintf("tail-%02d.trace", c.pending))
			}
			return
		}
	}
}

func (c *tailCollector) save(name string) {
	began := tailNow()
	f, err := os.Create(filepath.Join(c.dir, name))
	if err == nil {
		_, err = c.recorder.WriteTo(f)
		if closeErr := f.Close(); err == nil {
			err = closeErr
		}
	}
	if err != nil {
		c.saveFailures = append(c.saveFailures, name+": "+err.Error())
	}
	c.saving = append(c.saving, [2]float64{began, tailNow()})
}

// runtimeSample 是每秒一行的 runtime/metrics 与 rusage 增量；直方图只保留超过各档的样本数与最大非空桶上界。
type runtimeSample struct {
	AtS        float64                     `json:"at_s"`
	TickLagMS  float64                     `json:"tick_lag_ms"`
	GCCycles   uint64                      `json:"gc_cycles"`
	Goroutines uint64                      `json:"goroutines"`
	Histograms map[string]histogramSummary `json:"histograms"`
	Counters   map[string]float64          `json:"counters"`
	Rusage     map[string]float64          `json:"rusage"`
}

type histogramSummary struct {
	Count  uint64  `json:"count"`
	Over1  uint64  `json:"over_1ms"`
	Over10 uint64  `json:"over_10ms"`
	Over50 uint64  `json:"over_50ms"`
	MaxMS  float64 `json:"max_ms"`
}

var tailHistogramMetrics = []string{
	"/sched/latencies:seconds",
	"/sched/pauses/total/gc:seconds",
	"/sched/pauses/total/other:seconds",
	"/sched/pauses/stopping/gc:seconds",
	"/sched/pauses/stopping/other:seconds",
	"/gc/pauses:seconds",
}

var tailCounterMetrics = []string{
	"/sync/mutex/wait/total:seconds",
	"/cpu/classes/gc/pause:cpu-seconds",
	"/cpu/classes/gc/mark/assist:cpu-seconds",
	"/cpu/classes/gc/total:cpu-seconds",
	"/cpu/classes/idle:cpu-seconds",
	"/cpu/classes/user:cpu-seconds",
	"/cpu/classes/scavenge/total:cpu-seconds",
	"/gc/heap/allocs:bytes",
}

func (c *tailCollector) sampleRuntime() {
	defer c.done.Done()
	f, err := os.Create(filepath.Join(c.dir, "runtime-metrics.jsonl"))
	if err != nil {
		c.saveFailures = append(c.saveFailures, err.Error())
		return
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	supported := map[string]bool{}
	for _, d := range metrics.All() {
		supported[d.Name] = true
	}
	var samples []metrics.Sample
	for _, name := range append(append(append([]string{}, tailHistogramMetrics...), tailCounterMetrics...), "/gc/cycles/total:gc-cycles", "/sched/goroutines:goroutines") {
		if supported[name] {
			samples = append(samples, metrics.Sample{Name: name})
		}
	}
	prevHist := map[string][]uint64{}
	prevCounter := map[string]float64{}
	var prevCycles uint64
	var prevUsage syscall.Rusage
	_ = syscall.Getrusage(syscall.RUSAGE_SELF, &prevUsage)
	metrics.Read(samples)
	for _, s := range samples {
		switch s.Value.Kind() {
		case metrics.KindFloat64Histogram:
			prevHist[s.Name] = append([]uint64(nil), s.Value.Float64Histogram().Counts...)
		case metrics.KindFloat64:
			prevCounter[s.Name] = s.Value.Float64()
		case metrics.KindUint64:
			if s.Name == "/gc/cycles/total:gc-cycles" {
				prevCycles = s.Value.Uint64()
			} else {
				prevCounter[s.Name] = float64(s.Value.Uint64())
			}
		}
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		var planned time.Time
		select {
		case <-c.stop:
			return
		case planned = <-ticker.C:
		}
		now := time.Now()
		metrics.Read(samples)
		row := runtimeSample{AtS: tailNow(), TickLagMS: ms(now.Sub(planned)), Histograms: map[string]histogramSummary{}, Counters: map[string]float64{}, Rusage: map[string]float64{}}
		for _, s := range samples {
			switch s.Value.Kind() {
			case metrics.KindFloat64Histogram:
				h := s.Value.Float64Histogram()
				prev := prevHist[s.Name]
				var sum histogramSummary
				for i, n := range h.Counts {
					if i < len(prev) {
						n -= prev[i]
					}
					if n == 0 {
						continue
					}
					sum.Count += n
					upper := h.Buckets[i+1] // 桶 i 的上界；最后一桶上界可能是 +Inf，取下界
					if upper > 1e9 {
						upper = h.Buckets[i]
					}
					if h.Buckets[i] >= 0.001 {
						sum.Over1 += n
					}
					if h.Buckets[i] >= 0.010 {
						sum.Over10 += n
					}
					if h.Buckets[i] >= 0.050 {
						sum.Over50 += n
					}
					sum.MaxMS = max(sum.MaxMS, upper*1000)
				}
				prevHist[s.Name] = append(prevHist[s.Name][:0], h.Counts...)
				row.Histograms[s.Name] = sum
			case metrics.KindFloat64:
				v := s.Value.Float64()
				row.Counters[s.Name] = v - prevCounter[s.Name]
				prevCounter[s.Name] = v
			case metrics.KindUint64:
				v := s.Value.Uint64()
				switch s.Name {
				case "/gc/cycles/total:gc-cycles":
					row.GCCycles = v - prevCycles
					prevCycles = v
				case "/sched/goroutines:goroutines":
					row.Goroutines = v
				default:
					row.Counters[s.Name] = float64(v) - prevCounter[s.Name]
					prevCounter[s.Name] = float64(v)
				}
			}
		}
		var usage syscall.Rusage
		if syscall.Getrusage(syscall.RUSAGE_SELF, &usage) == nil {
			row.Rusage["user_s"] = tvSeconds(usage.Utime) - tvSeconds(prevUsage.Utime)
			row.Rusage["sys_s"] = tvSeconds(usage.Stime) - tvSeconds(prevUsage.Stime)
			row.Rusage["voluntary_ctxsw"] = float64(usage.Nvcsw - prevUsage.Nvcsw)
			row.Rusage["involuntary_ctxsw"] = float64(usage.Nivcsw - prevUsage.Nivcsw)
			row.Rusage["major_faults"] = float64(usage.Majflt - prevUsage.Majflt)
			prevUsage = usage
		}
		_ = enc.Encode(row)
	}
}

func tvSeconds(tv syscall.Timeval) float64 { return float64(tv.Sec) + float64(tv.Usec)/1e6 }

// finish 停止采集并写 summary.json；在负载统计完成后调用，不影响被测区间。
func (c *tailCollector) finish(t *testing.T) {
	if c == nil {
		return
	}
	close(c.stop)
	c.done.Wait()
	if c.recorder != nil {
		c.recorder.Stop()
	}
	summary := map[string]any{
		"mode": c.mode, "threshold_ms": ms(c.threshold), "post_ms": ms(c.post), "max_captures": c.maxCaps,
		"baseline_every_s": c.baseline.Seconds(), "tail_events": c.seen.Load(), "dropped_events": c.dropped.Load(),
		"tail_captures": c.captures, "save_intervals_s": c.saving, "save_failures": c.saveFailures,
		"gomaxprocs": runtime.GOMAXPROCS(0), "go": runtime.Version(),
	}
	raw, _ := json.MarshalIndent(summary, "", "  ")
	if err := os.WriteFile(filepath.Join(c.dir, "summary.json"), append(raw, '\n'), 0o644); err != nil {
		t.Errorf("tail summary: %v", err)
	}
	t.Logf("tail collector: mode=%s events=%d dropped=%d captures=%d failures=%d dir=%s", c.mode, c.seen.Load(), c.dropped.Load(), c.captures, len(c.saveFailures), c.dir)
}
