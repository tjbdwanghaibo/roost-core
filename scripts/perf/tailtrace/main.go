// tailtrace 分析 nest-game 负载长尾采集（ROOST_NEST_GAME_TAIL=metrics|trace）的产物目录 sample-N.tail/。
//
// 两层证据：
//  1. runtime-metrics.jsonl × tail-events.jsonl：每个长尾事件窗口内的 GC 周期、STW 直方图、调度延迟
//     直方图、互斥等待、involuntary 上下文切换与 CPU 占用，并与“全部秒”的基线比例对照（富集比）。
//  2. tail-NN.trace（flight recorder）：用 trace.Log 的 roost.tail 事件把长尾对到 trace 时钟，统计窗口内
//     STW / GC 标记 / 标记辅助的重叠、记录事件的 goroutine 在各分段的状态（运行、可运行、等待原因、
//     系统调用）、全局可运行 goroutine 与运行中 P 的时间加权均值；base-NN.trace 给出同样指标的对照分布。
//
// 分类是启发式的，只给出“主要可见因素”；trace 看不到 OS 对线程的抢占（P 处于运行态但线程被宿主
// 调走时，表现为 goroutine 长时间 Running），这一类只能结合 involuntary 上下文切换旁证。
//
// 用法：cd scripts/perf/tailtrace && GOWORK=off go run . -dir <artifacts/perf/nest/LABEL/sample-1.tail> [-json out.json]
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/exp/trace"
)

type tailEvent struct {
	Stream        string             `json:"stream"`
	AtS           float64            `json:"at_s"`
	TotalMS       float64            `json:"total_ms"`
	Parts         map[string]float64 `json:"parts_ms"`
	Capture       int                `json:"capture"`
	DuringCapture bool               `json:"during_capture"`
}

type histogramSummary struct {
	Count  uint64  `json:"count"`
	Over1  uint64  `json:"over_1ms"`
	Over10 uint64  `json:"over_10ms"`
	Over50 uint64  `json:"over_50ms"`
	MaxMS  float64 `json:"max_ms"`
}

type runtimeSample struct {
	AtS        float64                     `json:"at_s"`
	TickLagMS  float64                     `json:"tick_lag_ms"`
	GCCycles   uint64                      `json:"gc_cycles"`
	Goroutines uint64                      `json:"goroutines"`
	Histograms map[string]histogramSummary `json:"histograms"`
	Counters   map[string]float64          `json:"counters"`
	Rusage     map[string]float64          `json:"rusage"`
}

type summaryFile struct {
	GOMAXPROCS     int     `json:"gomaxprocs"`
	ThresholdMS    float64 `json:"threshold_ms"`
	OriginUnixNano int64   `json:"origin_unix_nano"`
}

func main() {
	dir := flag.String("dir", "", "sample-N.tail 目录")
	jsonOut := flag.String("json", "", "可选：把完整结果写成 JSON")
	flag.IntVar(&printTop, "top", 20, "每个 trace 文本输出里最多列出的事件数（按总延迟降序；JSON 含全部）")
	flag.Parse()
	if *dir == "" {
		fmt.Fprintln(os.Stderr, "usage: tailtrace -dir <sample-N.tail> [-json out.json]")
		os.Exit(2)
	}
	report, err := analyze(*dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "tailtrace:", err)
		os.Exit(1)
	}
	printReport(os.Stdout, report)
	if *jsonOut != "" {
		raw, _ := json.MarshalIndent(report, "", "  ")
		if err := os.WriteFile(*jsonOut, append(raw, '\n'), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, "tailtrace:", err)
			os.Exit(1)
		}
	}
}

type report struct {
	Dir         string                  `json:"dir"`
	GOMAXPROCS  int                     `json:"gomaxprocs"`
	ThresholdMS float64                 `json:"threshold_ms"`
	Streams     map[string]streamCounts `json:"streams"`
	Metrics     metricsCorrelation      `json:"metrics"`
	Traces      []traceReport           `json:"traces"`
	Baselines   []traceReport           `json:"baselines"`
	Labels      map[string]int          `json:"trace_labels"`
	Episodes    []episode               `json:"episodes"`
	// MinutePhase 是各 episode 起点落在墙钟分钟内第几秒的计数；集中在同一秒说明与宿主按墙钟触发的周期任务同步。
	MinutePhase map[int]int `json:"episode_minute_phase_s,omitempty"`
}

// episode 是相邻长尾事件间隔小于 1s 的一簇（不含 tick 流）。
type episode struct {
	StartS  float64        `json:"start_s"`
	Wall    string         `json:"wall,omitempty"`
	SpanMS  float64        `json:"span_ms"`
	Events  int            `json:"events"`
	MaxMS   float64        `json:"max_ms"`
	Streams map[string]int `json:"streams"`
}

func groupEpisodes(events []tailEvent, originUnixNano int64) ([]episode, map[int]int) {
	sorted := slices.Clone(events)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].AtS < sorted[j].AtS })
	var out []episode
	var last float64
	for _, ev := range sorted {
		if ev.Stream == "tick" {
			continue
		}
		if len(out) == 0 || ev.AtS-last >= 1 {
			out = append(out, episode{StartS: ev.AtS, Streams: map[string]int{}})
		}
		ep := &out[len(out)-1]
		ep.SpanMS = (ev.AtS - ep.StartS) * 1000
		ep.Events++
		ep.MaxMS = max(ep.MaxMS, ev.TotalMS)
		ep.Streams[ev.Stream]++
		last = ev.AtS
	}
	var phase map[int]int
	if originUnixNano > 0 {
		phase = map[int]int{}
		for i := range out {
			wall := time.Unix(0, originUnixNano+int64(out[i].StartS*1e9))
			out[i].Wall = wall.Format("15:04:05.000")
			phase[wall.Second()]++
		}
	}
	return out, phase
}

type streamCounts struct {
	Events        int     `json:"events"`
	DuringCapture int     `json:"during_capture"`
	MaxMS         float64 `json:"max_ms"`
}

func analyze(dir string) (*report, error) {
	r := &report{Dir: dir, Streams: map[string]streamCounts{}, Labels: map[string]int{}}
	var summary summaryFile
	if raw, err := os.ReadFile(filepath.Join(dir, "summary.json")); err == nil {
		_ = json.Unmarshal(raw, &summary)
	}
	r.GOMAXPROCS, r.ThresholdMS = summary.GOMAXPROCS, summary.ThresholdMS
	events, err := readJSONL[tailEvent](filepath.Join(dir, "tail-events.jsonl"))
	if err != nil {
		return nil, err
	}
	samples, err := readJSONL[runtimeSample](filepath.Join(dir, "runtime-metrics.jsonl"))
	if err != nil {
		return nil, err
	}
	for _, ev := range events {
		c := r.Streams[ev.Stream]
		c.Events++
		if ev.DuringCapture {
			c.DuringCapture++
		}
		c.MaxMS = max(c.MaxMS, ev.TotalMS)
		r.Streams[ev.Stream] = c
	}
	r.Metrics = correlateMetrics(events, samples, r.GOMAXPROCS)
	r.Episodes, r.MinutePhase = groupEpisodes(events, summary.OriginUnixNano)
	traces, _ := filepath.Glob(filepath.Join(dir, "tail-*.trace"))
	baselines, _ := filepath.Glob(filepath.Join(dir, "base-*.trace"))
	sort.Strings(traces)
	sort.Strings(baselines)
	for _, path := range traces {
		tr, err := analyzeTrace(path, r.GOMAXPROCS, false)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		for _, ev := range tr.Events {
			r.Labels[ev.Label]++
		}
		r.Traces = append(r.Traces, *tr)
	}
	for _, path := range baselines {
		tr, err := analyzeTrace(path, r.GOMAXPROCS, true)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		r.Baselines = append(r.Baselines, *tr)
	}
	return r, nil
}

func readJSONL[T any](path string) ([]T, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []T
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1<<20), 16<<20)
	for scanner.Scan() {
		var v T
		if err := json.Unmarshal(scanner.Bytes(), &v); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		out = append(out, v)
	}
	return out, scanner.Err()
}

// ---------- 第 1 层：每秒 runtime 指标 ----------

// condition 是一个“这一秒发生了 X”的判定；比较它在长尾事件窗口里的出现比例与在全部秒中的比例。
type condition struct {
	name string
	hit  func(runtimeSample, int) bool
}

var conditions = []condition{
	{"gc_cycle", func(s runtimeSample, _ int) bool { return s.GCCycles > 0 }},
	{"gc_stw_over_1ms", func(s runtimeSample, _ int) bool { return s.Histograms["/sched/pauses/total/gc:seconds"].Over1 > 0 }},
	{"other_stw_any", func(s runtimeSample, _ int) bool { return s.Histograms["/sched/pauses/total/other:seconds"].Count > 0 }},
	{"sched_latency_over_10ms", func(s runtimeSample, _ int) bool { return s.Histograms["/sched/latencies:seconds"].Over10 > 0 }},
	{"sched_latency_over_50ms", func(s runtimeSample, _ int) bool { return s.Histograms["/sched/latencies:seconds"].Over50 > 0 }},
	{"cpu_over_90pct", func(s runtimeSample, procs int) bool {
		return procs > 0 && (s.Rusage["user_s"]+s.Rusage["sys_s"]) > 0.9*float64(procs)
	}},
	{"mark_assist_over_50ms_cpu", func(s runtimeSample, _ int) bool { return s.Counters["/cpu/classes/gc/mark/assist:cpu-seconds"] > 0.05 }},
	{"ticker_lag_over_10ms", func(s runtimeSample, _ int) bool { return s.TickLagMS > 10 }},
}

type conditionResult struct {
	Name          string  `json:"name"`
	SecondsShare  float64 `json:"share_of_all_seconds"`
	EventsShare   float64 `json:"share_of_tail_events"`
	Enrichment    float64 `json:"enrichment"`
	EventsCovered int     `json:"events_with_condition"`
}

type metricsCorrelation struct {
	Seconds         int               `json:"seconds"`
	EventsUsed      int               `json:"events_used"`
	Conditions      []conditionResult `json:"conditions"`
	InvoluntaryCtx  [2]float64        `json:"involuntary_ctxsw_per_s_all_vs_tail"`
	SchedMaxMSTail  float64           `json:"sched_latency_max_ms_in_tail_windows"`
	StopOtherMaxMS  float64           `json:"other_stw_max_ms_all"`
	GCStopMaxMS     float64           `json:"gc_stw_max_ms_all"`
	SchedMaxMSAll   float64           `json:"sched_latency_max_ms_all"`
	UnmatchedEvents int               `json:"events_without_metrics_second"`
}

// correlateMetrics 把事件窗口 [at-total, at] 对到覆盖它的每秒样本（样本 i 覆盖 (at_s(i)-1, at_s(i)]）。
func correlateMetrics(events []tailEvent, samples []runtimeSample, procs int) metricsCorrelation {
	var out metricsCorrelation
	out.Seconds = len(samples)
	if len(samples) == 0 {
		return out
	}
	var ctxAll float64
	for _, s := range samples {
		ctxAll += s.Rusage["involuntary_ctxsw"]
		out.StopOtherMaxMS = max(out.StopOtherMaxMS, s.Histograms["/sched/pauses/total/other:seconds"].MaxMS)
		out.GCStopMaxMS = max(out.GCStopMaxMS, s.Histograms["/sched/pauses/total/gc:seconds"].MaxMS)
		out.SchedMaxMSAll = max(out.SchedMaxMSAll, s.Histograms["/sched/latencies:seconds"].MaxMS)
	}
	out.InvoluntaryCtx[0] = ctxAll / float64(len(samples))
	hits := make([]int, len(conditions))
	secondsHit := make([]int, len(conditions))
	for _, s := range samples {
		for i, c := range conditions {
			if c.hit(s, procs) {
				secondsHit[i]++
			}
		}
	}
	var ctxTail float64
	var ctxTailN int
	for _, ev := range events {
		if ev.Stream == "tick" || ev.DuringCapture {
			continue
		}
		from, to := ev.AtS-ev.TotalMS/1000, ev.AtS
		lo := sort.Search(len(samples), func(i int) bool { return samples[i].AtS > from })
		var covered []runtimeSample
		for i := lo; i < len(samples) && samples[i].AtS-1 < to; i++ {
			covered = append(covered, samples[i])
		}
		if len(covered) == 0 {
			out.UnmatchedEvents++
			continue
		}
		out.EventsUsed++
		for i, c := range conditions {
			if slices.ContainsFunc(covered, func(s runtimeSample) bool { return c.hit(s, procs) }) {
				hits[i]++
			}
		}
		for _, s := range covered {
			ctxTail += s.Rusage["involuntary_ctxsw"]
			ctxTailN++
			out.SchedMaxMSTail = max(out.SchedMaxMSTail, s.Histograms["/sched/latencies:seconds"].MaxMS)
		}
	}
	if ctxTailN > 0 {
		out.InvoluntaryCtx[1] = ctxTail / float64(ctxTailN)
	}
	for i, c := range conditions {
		res := conditionResult{Name: c.name, SecondsShare: float64(secondsHit[i]) / float64(len(samples)), EventsCovered: hits[i]}
		if out.EventsUsed > 0 {
			res.EventsShare = float64(hits[i]) / float64(out.EventsUsed)
		}
		if res.SecondsShare > 0 {
			res.Enrichment = res.EventsShare / res.SecondsShare
		}
		out.Conditions = append(out.Conditions, res)
	}
	return out
}

// ---------- 第 2 层：flight recorder trace ----------

type segment struct {
	Name     string  `json:"name"`
	Start    int64   `json:"-"`
	End      int64   `json:"-"`
	MS       float64 `json:"ms"`
	Running  float64 `json:"running_ms"`
	Runnable float64 `json:"runnable_ms"`
	Syscall  float64 `json:"syscall_ms"`
	// Waiting 按进入等待时的原因分组（如 sync.Mutex.Lock、chan receive、select）。
	Waiting map[string]float64 `json:"waiting_ms,omitempty"`
}

type windowStats struct {
	STWMS          float64  `json:"stw_overlap_ms"`
	STWNames       []string `json:"stw_names,omitempty"`
	GCMarkMS       float64  `json:"gc_mark_overlap_ms"`
	MarkAssistMS   float64  `json:"mark_assist_ms_this_goroutine"`
	MarkAssistAll  float64  `json:"mark_assist_ms_all_goroutines"`
	RunnableMean   float64  `json:"runnable_goroutines_mean"`
	RunnableMax    int      `json:"runnable_goroutines_max"`
	RunningPMean   float64  `json:"running_procs_mean"`
	IdleWithQueued float64  `json:"idle_proc_with_runnable_ms"`
}

type traceEvent struct {
	Stream    string         `json:"stream"`
	Goroutine int64          `json:"goroutine"`
	TotalMS   float64        `json:"total_ms"`
	Parts     map[string]int `json:"parts_us"`
	Segments  []segment      `json:"segments"`
	Window    windowStats    `json:"window"`
	Label     string         `json:"label"`
	start     int64
	end       int64
}

type traceReport struct {
	File     string       `json:"file"`
	Span     float64      `json:"span_ms"`
	Events   []traceEvent `json:"events,omitempty"`
	STW      []stwRange   `json:"stw_ranges_over_1ms,omitempty"`
	Baseline *baseline    `json:"baseline,omitempty"`
}

type stwRange struct {
	Name string  `json:"name"`
	AtMS float64 `json:"at_ms"`
	MS   float64 `json:"ms"`
}

// baseline 是对照 trace 上 50ms 滑窗（步长 50ms）的分布，用于判断长尾窗口里的条件是否“异常”。
type baseline struct {
	Windows          int     `json:"windows"`
	STWShare         float64 `json:"windows_with_stw"`
	STWOver5Share    float64 `json:"windows_with_stw_over_5ms"`
	GCMarkShare      float64 `json:"windows_with_gc_mark"`
	RunnableMeanP50  float64 `json:"runnable_mean_p50"`
	RunnableMeanP99  float64 `json:"runnable_mean_p99"`
	IdleWithQueuedMS float64 `json:"idle_proc_with_runnable_ms_p99"`
}

type interval struct {
	name       string
	start, end int64
}

// pass1 收集 roost.tail 日志与全局区间；pass2 在已知窗口上累计状态。
func analyzeTrace(path string, procs int, isBaseline bool) (*traceReport, error) {
	out := &traceReport{File: filepath.Base(path)}
	var events []*traceEvent
	var stw, gcMark []interval
	assist := map[int64][]interval{}
	var first, last int64 = -1, 0
	open := map[string]int64{}
	err := readTrace(path, func(e trace.Event) {
		t := int64(e.Time())
		if first < 0 {
			first = t
		}
		last = t
		switch e.Kind() {
		case trace.EventLog:
			l := e.Log()
			if l.Category != "roost.tail" || isBaseline {
				return
			}
			ev := parseTailLog(l.Message)
			ev.Goroutine = int64(e.Goroutine())
			ev.end = t
			ev.start = t - int64(ev.TotalMS*1e6)
			events = append(events, ev)
		case trace.EventRangeBegin, trace.EventRangeActive, trace.EventRangeEnd:
			rg := e.Range()
			key := rg.Name + "@" + rg.Scope.String()
			if e.Kind() != trace.EventRangeEnd {
				begin := t
				if e.Kind() == trace.EventRangeActive {
					begin = first
				}
				open[key] = begin
				return
			}
			begin, ok := open[key]
			if !ok {
				begin = first
			}
			delete(open, key)
			iv := interval{rg.Name, begin, t}
			switch {
			case strings.HasPrefix(rg.Name, "stop-the-world"):
				stw = append(stw, iv)
			case rg.Name == "GC concurrent mark phase":
				gcMark = append(gcMark, iv)
			case rg.Name == "GC mark assist":
				g := int64(rg.Scope.Goroutine())
				assist[g] = append(assist[g], iv)
			}
		}
	})
	if err != nil {
		return nil, err
	}
	for key, begin := range open {
		name := key[:strings.LastIndex(key, "@")]
		iv := interval{name, begin, last}
		if strings.HasPrefix(name, "stop-the-world") {
			stw = append(stw, iv)
		} else if name == "GC concurrent mark phase" {
			gcMark = append(gcMark, iv)
		}
	}
	out.Span = float64(last-first) / 1e6
	for _, iv := range stw {
		if d := iv.end - iv.start; d > int64(time.Millisecond) {
			out.STW = append(out.STW, stwRange{iv.name, float64(iv.start-first) / 1e6, float64(d) / 1e6})
		}
	}
	// 窗口超出 trace 起点的部分无法观察，截到 trace 起点并在分段里体现。
	for _, ev := range events {
		ev.start = max(ev.start, first)
		ev.Segments = buildSegments(ev)
	}
	var windows []*windowAcc
	if isBaseline {
		const step = int64(50 * time.Millisecond)
		for s := first; s+step <= last; s += step {
			windows = append(windows, &windowAcc{start: s, end: s + step})
		}
	} else {
		for _, ev := range events {
			windows = append(windows, &windowAcc{start: ev.start, end: ev.end, ev: ev})
		}
	}
	if err := pass2(path, windows, procs); err != nil {
		return nil, err
	}
	for _, w := range windows {
		w.finish(stw, gcMark, assist)
	}
	if isBaseline {
		out.Baseline = summarizeBaseline(windows)
		return out, nil
	}
	for _, ev := range events {
		ev.Label = classify(ev)
		out.Events = append(out.Events, *ev)
	}
	return out, nil
}

func readTrace(path string, fn func(trace.Event)) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	r, err := trace.NewReader(bufio.NewReaderSize(f, 1<<20))
	if err != nil {
		return err
	}
	for {
		e, err := r.ReadEvent()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		fn(e)
	}
}

func parseTailLog(msg string) *traceEvent {
	ev := &traceEvent{Parts: map[string]int{}}
	for _, field := range strings.Fields(msg) {
		key, value, ok := strings.Cut(field, "=")
		if !ok {
			continue
		}
		if key == "stream" {
			ev.Stream = value
			continue
		}
		n, err := strconv.Atoi(value)
		if err != nil {
			continue
		}
		name := strings.TrimSuffix(key, "_us")
		if name == "total" {
			ev.TotalMS = float64(n) / 1000
		} else {
			ev.Parts[name] = n
		}
	}
	return ev
}

// partOrder 是各流分段的时间顺序（与夹具里 observe 的参数顺序一致）。
var partOrder = map[string][]string{
	"business":  {"admit", "client", "queue", "handler", "release"},
	"heartbeat": {"admit", "client", "queue", "handler", "release"},
	"sync":      {"business", "commit", "deliver"},
}

func buildSegments(ev *traceEvent) []segment {
	segs := []segment{{Name: "window", Start: ev.start, End: ev.end}}
	at := ev.end - int64(ev.TotalMS*1e6)
	for _, name := range partOrder[ev.Stream] {
		d := int64(ev.Parts[name]) * 1000
		segs = append(segs, segment{Name: name, Start: max(at, ev.start), End: max(at+d, ev.start)})
		at += d
	}
	for i := range segs {
		segs[i].MS = float64(segs[i].End-segs[i].Start) / 1e6
		segs[i].Waiting = map[string]float64{}
	}
	return segs
}

type windowAcc struct {
	start, end int64
	ev         *traceEvent
	// 时间加权累计
	runnableNS, runningPNS, idleQueuedNS float64
	runnableMax                          int
	stats                                windowStats
}

type goState struct {
	state  trace.GoState
	since  int64
	reason string
}

// pass2 第二次顺序读 trace：维护全局可运行 goroutine 数与运行中 P 数，并对关心的 goroutine 记录状态区间。
func pass2(path string, windows []*windowAcc, procs int) error {
	sort.Slice(windows, func(i, j int) bool { return windows[i].start < windows[j].start })
	interest := map[int64][]*traceEvent{}
	for _, w := range windows {
		if w.ev != nil {
			interest[w.ev.Goroutine] = append(interest[w.ev.Goroutine], w.ev)
		}
	}
	gStates := map[int64]*goState{}
	procRunning := map[int64]bool{}
	runnable, runningP := 0, 0
	var prev int64 = -1
	next := 0
	var active []*windowAcc
	advance := func(t int64) {
		if prev < 0 {
			prev = t
			return
		}
		for next < len(windows) && windows[next].start < t {
			active = append(active, windows[next])
			next++
		}
		kept := active[:0]
		for _, w := range active {
			lo, hi := max(prev, w.start), min(t, w.end)
			if hi > lo {
				d := float64(hi - lo)
				w.runnableNS += d * float64(runnable)
				w.runningPNS += d * float64(runningP)
				if procs > 0 && runningP < procs && runnable > 0 {
					w.idleQueuedNS += d
				}
			}
			if runnable > w.runnableMax && t > w.start && prev < w.end {
				w.runnableMax = runnable
			}
			if w.end > t {
				kept = append(kept, w)
			}
		}
		active = kept
		prev = t
	}
	err := readTrace(path, func(e trace.Event) {
		if e.Kind() != trace.EventStateTransition {
			return
		}
		t := int64(e.Time())
		advance(t)
		st := e.StateTransition()
		switch st.Resource.Kind {
		case trace.ResourceGoroutine:
			from, to := st.Goroutine()
			if from == trace.GoRunnable {
				runnable--
			}
			if to == trace.GoRunnable {
				runnable++
			}
			runnable = max(runnable, 0)
			g := int64(st.Resource.Goroutine())
			if evs, ok := interest[g]; ok {
				s := gStates[g]
				if s == nil {
					s = &goState{state: from, since: t}
					gStates[g] = s
				}
				account(evs, s, t)
				s.state, s.since, s.reason = to, t, st.Reason
			}
		case trace.ResourceProc:
			_, to := st.Proc()
			p := int64(st.Resource.Proc())
			if procRunning[p] {
				runningP--
			}
			procRunning[p] = to == trace.ProcRunning
			if procRunning[p] {
				runningP++
			}
		}
	})
	if err != nil {
		return err
	}
	for g, s := range gStates {
		account(interest[g], s, prev)
	}
	return nil
}

// account 把 goroutine 在 [s.since, t) 的状态计入各事件分段。
func account(evs []*traceEvent, s *goState, t int64) {
	for _, ev := range evs {
		for i := range ev.Segments {
			seg := &ev.Segments[i]
			lo, hi := max(s.since, seg.Start), min(t, seg.End)
			if hi <= lo {
				continue
			}
			d := float64(hi-lo) / 1e6
			switch s.state {
			case trace.GoRunning:
				seg.Running += d
			case trace.GoRunnable:
				seg.Runnable += d
			case trace.GoSyscall:
				seg.Syscall += d
			case trace.GoWaiting:
				reason := s.reason
				if reason == "" {
					reason = "unknown"
				}
				seg.Waiting[reason] += d
			}
		}
	}
}

func (w *windowAcc) finish(stw, gcMark []interval, assist map[int64][]interval) {
	span := float64(w.end - w.start)
	if span <= 0 {
		return
	}
	w.stats.RunnableMean = w.runnableNS / span
	w.stats.RunningPMean = w.runningPNS / span
	w.stats.RunnableMax = w.runnableMax
	w.stats.IdleWithQueued = w.idleQueuedNS / 1e6
	for _, iv := range stw {
		if o := overlap(iv, w.start, w.end); o > 0 {
			w.stats.STWMS += o
			w.stats.STWNames = append(w.stats.STWNames, iv.name)
		}
	}
	for _, iv := range gcMark {
		w.stats.GCMarkMS += overlap(iv, w.start, w.end)
	}
	for g, ivs := range assist {
		for _, iv := range ivs {
			o := overlap(iv, w.start, w.end)
			w.stats.MarkAssistAll += o
			if w.ev != nil && g == w.ev.Goroutine {
				w.stats.MarkAssistMS += o
			}
		}
	}
	if w.ev != nil {
		w.ev.Window = w.stats
	}
}

func overlap(iv interval, start, end int64) float64 {
	lo, hi := max(iv.start, start), min(iv.end, end)
	if hi <= lo {
		return 0
	}
	return float64(hi-lo) / 1e6
}

// classify 给出主要可见因素。阈值取“占该事件超出 10ms 正常水平部分的一半以上”。
func classify(ev *traceEvent) string {
	excess := max(ev.TotalMS-10, 1)
	big := func(ms float64) bool { return ms >= excess/2 }
	if big(ev.Window.STWMS) {
		return "stw"
	}
	segs := map[string]segment{}
	for _, s := range ev.Segments {
		segs[s.Name] = s
	}
	part := func(name string) float64 { return float64(ev.Parts[name]) / 1000 }
	switch ev.Stream {
	case "business", "heartbeat":
		// planned→admitted 天然最多一个 ticker 周期（10ms）：投递按 10ms tick 批量进行。
		if big(part("admit") - 10) {
			return "admit-late(ticker)"
		}
		// admitted→sent 是模拟客户端单连接串行发送的排队（直接投递恒为 0），属于压测端。
		if big(part("client")) {
			return "client-queue(harness)"
		}
		if q := segs["queue"]; big(part("queue")) {
			// 排队期间记录事件的 worker 若一直在运行，说明它在处理排在前面的任务（同 worker 队头阻塞）；
			// 若在等待，说明任务在队列里而该 worker 空闲，属于调度 / 唤醒延迟。
			switch {
			case big(q.Runnable):
				return "queue:worker-runnable(sched-delay)"
			case big(q.Running):
				return "queue:worker-busy(head-of-line)"
			case big(sum(q.Waiting)):
				return "queue:worker-waiting(" + topReason(q.Waiting) + ")"
			default:
				return "queue:mixed"
			}
		}
		for _, name := range []string{"handler", "release"} {
			s := segs[name]
			if !big(part(name)) {
				continue
			}
			switch {
			case big(s.Runnable):
				return name + ":runnable(sched-delay)"
			case big(sum(s.Waiting)):
				return name + ":waiting(" + topReason(s.Waiting) + ")"
			case big(s.Running):
				return name + ":running(cpu-or-os-preempt)"
			default:
				return name + ":mixed"
			}
		}
	case "sync":
		for _, name := range []string{"business", "commit", "deliver"} {
			if big(part(name)) {
				w := segs["window"]
				if name == "deliver" && big(w.Runnable) {
					return "deliver:client-runnable(sched-delay)"
				}
				return "sync:" + name
			}
		}
	case "gate", "embedded", "tick":
		w := segs["window"]
		switch {
		case big(w.Runnable):
			return ev.Stream + ":runnable(sched-delay)"
		case big(w.Running):
			return ev.Stream + ":running"
		case big(sum(w.Waiting)):
			return ev.Stream + ":waiting(" + topReason(w.Waiting) + ")"
		}
	}
	if ev.Window.IdleWithQueued >= excess/2 {
		return "idle-proc-with-runnable(wakeup)"
	}
	if ev.Window.RunningPMean > 0 && ev.Window.MarkAssistAll >= excess {
		return "gc-mark-assist-pressure"
	}
	return "unattributed"
}

func sum(m map[string]float64) float64 {
	var s float64
	for _, v := range m {
		s += v
	}
	return s
}

func topReason(m map[string]float64) string {
	best, bestV := "", -1.0
	for k, v := range m {
		if v > bestV {
			best, bestV = k, v
		}
	}
	return best
}

func summarizeBaseline(windows []*windowAcc) *baseline {
	b := &baseline{Windows: len(windows)}
	if len(windows) == 0 {
		return b
	}
	var means, idle []float64
	for _, w := range windows {
		if w.stats.STWMS > 0 {
			b.STWShare++
		}
		if w.stats.STWMS > 5 {
			b.STWOver5Share++
		}
		if w.stats.GCMarkMS > 0 {
			b.GCMarkShare++
		}
		means = append(means, w.stats.RunnableMean)
		idle = append(idle, w.stats.IdleWithQueued)
	}
	n := float64(len(windows))
	b.STWShare /= n
	b.STWOver5Share /= n
	b.GCMarkShare /= n
	slices.Sort(means)
	slices.Sort(idle)
	b.RunnableMeanP50 = quantile(means, .5)
	b.RunnableMeanP99 = quantile(means, .99)
	b.IdleWithQueuedMS = quantile(idle, .99)
	return b
}

func quantile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	return sorted[min(len(sorted)-1, int(math.Ceil(p*float64(len(sorted))))-1)]
}

var printTop int

func printReport(w io.Writer, r *report) {
	fmt.Fprintf(w, "dir=%s GOMAXPROCS=%d threshold=%gms\n", r.Dir, r.GOMAXPROCS, r.ThresholdMS)
	streams := make([]string, 0, len(r.Streams))
	for s := range r.Streams {
		streams = append(streams, s)
	}
	sort.Strings(streams)
	for _, s := range streams {
		c := r.Streams[s]
		fmt.Fprintf(w, "  stream %-9s events=%d during_capture=%d max=%.3fms\n", s, c.Events, c.DuringCapture, c.MaxMS)
	}
	m := r.Metrics
	fmt.Fprintf(w, "runtime metrics: seconds=%d events_used=%d unmatched=%d gc_stw_max=%.3fms other_stw_max=%.3fms sched_latency_max=%.3fms (tail windows %.3fms) involuntary_ctxsw/s all=%.0f tail=%.0f\n",
		m.Seconds, m.EventsUsed, m.UnmatchedEvents, m.GCStopMaxMS, m.StopOtherMaxMS, m.SchedMaxMSAll, m.SchedMaxMSTail, m.InvoluntaryCtx[0], m.InvoluntaryCtx[1])
	for _, c := range m.Conditions {
		fmt.Fprintf(w, "  %-28s seconds=%6.2f%% tail_events=%6.2f%% enrichment=%.2f\n", c.Name, 100*c.SecondsShare, 100*c.EventsShare, c.Enrichment)
	}
	fmt.Fprintf(w, "episodes (gap>=1s): %d; minute phase of episode starts (second-of-minute:count): %v\n", len(r.Episodes), r.MinutePhase)
	for _, ep := range r.Episodes {
		fmt.Fprintf(w, "  episode at=%.2fs wall=%s span=%.1fms events=%d max=%.1fms %v\n", ep.StartS, ep.Wall, ep.SpanMS, ep.Events, ep.MaxMS, ep.Streams)
	}
	for _, tr := range r.Traces {
		fmt.Fprintf(w, "trace %s span=%.0fms events=%d stw>1ms=%d\n", tr.File, tr.Span, len(tr.Events), len(tr.STW))
		for _, st := range tr.STW {
			fmt.Fprintf(w, "  STW %-50s at=%.1fms dur=%.3fms\n", st.Name, st.AtMS, st.MS)
		}
		top := slices.Clone(tr.Events)
		sort.SliceStable(top, func(i, j int) bool { return top[i].TotalMS > top[j].TotalMS })
		if len(top) > printTop {
			top = top[:printTop]
		}
		for _, ev := range top {
			fmt.Fprintf(w, "  %-9s total=%7.2fms g=%-6d label=%-36s stw=%.2f gcmark=%.1f assist(g/all)=%.2f/%.1f runnable(mean/max)=%.1f/%d runningP=%.1f idle+queued=%.2fms parts=%v\n",
				ev.Stream, ev.TotalMS, ev.Goroutine, ev.Label, ev.Window.STWMS, ev.Window.GCMarkMS, ev.Window.MarkAssistMS, ev.Window.MarkAssistAll,
				ev.Window.RunnableMean, ev.Window.RunnableMax, ev.Window.RunningPMean, ev.Window.IdleWithQueued, ev.Parts)
			for _, s := range ev.Segments[1:] {
				fmt.Fprintf(w, "      %-8s %7.2fms run=%.2f runnable=%.2f syscall=%.2f wait=%v\n", s.Name, s.MS, s.Running, s.Runnable, s.Syscall, roundMap(s.Waiting))
			}
		}
	}
	for _, tr := range r.Baselines {
		b := tr.Baseline
		fmt.Fprintf(w, "baseline %s span=%.0fms windows(50ms)=%d stw=%.2f%% stw>5ms=%.2f%% gcmark=%.2f%% runnable_mean p50/p99=%.1f/%.1f idle+queued p99=%.2fms stw>1ms=%d\n",
			tr.File, tr.Span, b.Windows, 100*b.STWShare, 100*b.STWOver5Share, 100*b.GCMarkShare, b.RunnableMeanP50, b.RunnableMeanP99, b.IdleWithQueuedMS, len(tr.STW))
	}
	if len(r.Labels) > 0 {
		labels := make([]string, 0, len(r.Labels))
		for l := range r.Labels {
			labels = append(labels, l)
		}
		sort.Slice(labels, func(i, j int) bool { return r.Labels[labels[i]] > r.Labels[labels[j]] })
		fmt.Fprintln(w, "trace labels:")
		for _, l := range labels {
			fmt.Fprintf(w, "  %-40s %d\n", l, r.Labels[l])
		}
	}
}

func roundMap(m map[string]float64) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = strconv.FormatFloat(v, 'f', 2, 64)
	}
	return out
}
