package nest

import (
	"fmt"
	"log/slog"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	flog "github.com/tjbdwanghaibo/roost-core/log"
	"github.com/tjbdwanghaibo/roost-core/metrics"
)

const (
	nestSlowDispatchThreshold      = 200 * time.Millisecond
	nestSlowDispatchTraceThreshold = nestSlowDispatchThreshold
	nestSlowDispatchStackLimit     = 256 * 1024
	nestSlowDispatchStackInterval  = 5 * time.Second
)

func observeDispatch(msg *Msg, err error, cost time.Duration) {
	if msg == nil {
		return
	}
	labels := metrics.Labels{
		"handler": msg.Name,
		"type":    msg.Type.String(),
	}
	if err != nil {
		labels["result"] = "error"
	} else {
		labels["result"] = "ok"
	}
	metrics.IncCounter("nest.dispatch.total", labels, 1)
	metrics.ObserveDuration("nest.dispatch.cost", labels, cost)
	if msg.HasRemote {
		metrics.IncCounter("nest.dispatch.remote.total", labels, 1)
	}
	if shouldLogSlowDispatch(cost) {
		flog.NewELog().Title("nest").Warn("slow dispatch",
			"handler", msg.Name,
			"type", msg.Type.String(),
			"key", msg.Key(),
			"tid", msg.Tid,
			"tids", len(msg.Tids),
			"groups", len(msg.GroupTIds),
			"cost_ms", cost.Milliseconds(),
			"cost", cost.String(),
			"result", labels["result"],
			"cost_pool", msg.Cost,
			"remote", msg.HasRemote,
		)
	}
}

func shouldLogSlowDispatch(cost time.Duration) bool {
	return cost >= nestSlowDispatchThreshold
}

func shouldTraceSlowDispatch(cost time.Duration) bool {
	return cost > nestSlowDispatchTraceThreshold
}

func dispatchResult(err error) string {
	if err != nil {
		return "error"
	}
	return "ok"
}

// watch 在 stop 确认计时回调结束后才可复用；完成信号只由计时回调发送。
// 快请求只 Stop 计时器，无需每次分配 channel、Timer 和回调闭包。
type slowDispatchTraceWatch struct {
	timer  *time.Timer
	msg    *Msg
	start  time.Time
	done   chan struct{}
	traced atomic.Bool
}

var slowDispatchWatchPool = sync.Pool{New: func() any {
	return &slowDispatchTraceWatch{done: make(chan struct{}, 1)}
}}

func startSlowDispatchTraceWatch(msg *Msg, start time.Time) *slowDispatchTraceWatch {
	if msg == nil {
		return nil
	}
	watch := slowDispatchWatchPool.Get().(*slowDispatchTraceWatch)
	watch.msg, watch.start = msg, start
	watch.traced.Store(false)
	if watch.timer == nil {
		watch.timer = time.AfterFunc(nestSlowDispatchTraceThreshold, func() {
			defer func() { watch.done <- struct{}{} }()
			watch.trace(time.Since(watch.start), "running")
		})
	} else {
		watch.timer.Reset(nestSlowDispatchTraceThreshold)
	}
	return watch
}

func (watch *slowDispatchTraceWatch) stop(cost time.Duration, result string) {
	if watch == nil {
		return
	}
	if !watch.timer.Stop() {
		if shouldTraceSlowDispatch(cost) {
			watch.trace(cost, result)
		}
		<-watch.done
	}
	watch.msg = nil
	watch.start = time.Time{}
	slowDispatchWatchPool.Put(watch)
}

func (watch *slowDispatchTraceWatch) trace(cost time.Duration, result string) {
	if !watch.traced.CompareAndSwap(false, true) {
		return
	}
	allowed, suppressed := slowDispatchStackGate.take(time.Now())
	if !allowed {
		metrics.IncCounter("nest.dispatch.slow_trace_suppressed", nil, 1)
		return
	}
	logSlowDispatchTrace(newNestMsgDebugInfo(watch.msg), cost, result, suppressed)
}

// 全 goroutine 堆栈是进程级诊断，按进程限频。慢日志和耗时指标仍逐请求记录；
// 依赖阻塞时不能让每个等待者都分配 256 KiB 并暂停运行时抓取相同堆栈。
type slowTraceGate struct {
	mu         sync.Mutex
	next       time.Time
	suppressed uint64
}

var slowDispatchStackGate slowTraceGate

func (gate *slowTraceGate) take(now time.Time) (bool, uint64) {
	gate.mu.Lock()
	defer gate.mu.Unlock()
	if now.Before(gate.next) {
		gate.suppressed++
		return false, 0
	}
	suppressed := gate.suppressed
	gate.suppressed = 0
	gate.next = now.Add(nestSlowDispatchStackInterval)
	return true, suppressed
}

// 阶段指标按引擎显式启用，避免给所有热请求添加细粒度计时和标签构造开销。
func startNestStage(enabled bool) time.Time {
	if enabled {
		return time.Now()
	}
	return time.Time{}
}

func observeNestStage(handler, stage string, start time.Time) {
	if !start.IsZero() {
		metrics.ObserveDuration("nest.stage.duration", metrics.Labels{"handler": handler, "stage": stage}, time.Since(start))
	}
}

type nestMsgDebugInfo struct {
	Handler      string
	Type         string
	Key          int64
	Tid          int64
	Tids         []int64
	Groups       [][]int64
	ParamsCount  int
	ParamTypes   []string
	ParamSamples []string
	RefCount     int
	HasRetChan   bool
	CostPool     bool
	Remote       bool
	ContextValid bool
	TraceID      string
	TraceEnabled bool
	TraceReason  string
	TraceTags    map[string]string
	MsgString    string
}

func newNestMsgDebugInfo(msg *Msg) nestMsgDebugInfo {
	if msg == nil {
		return nestMsgDebugInfo{}
	}
	return nestMsgDebugInfo{
		Handler:      msg.Name,
		Type:         msg.Type.String(),
		Key:          msg.Key(),
		Tid:          msg.Tid,
		Tids:         cloneInt64s(msg.Tids),
		Groups:       cloneInt64Groups(msg.GroupTIds),
		ParamsCount:  len(msg.Params),
		ParamTypes:   nestParamTypes(msg.Params),
		ParamSamples: nestParamSamples(msg.Params),
		RefCount:     msg.RefCount,
		HasRetChan:   msg.RetChan != nil,
		CostPool:     msg.Cost,
		Remote:       msg.HasRemote,
		ContextValid: msg.Context.Valid,
		TraceID:      msg.Context.Trace.TraceID,
		TraceEnabled: msg.Context.Trace.Active(),
		TraceReason:  msg.Context.Trace.Reason,
		TraceTags:    msg.Context.Trace.Tags,
		MsgString:    msg.String(),
	}
}

func (info nestMsgDebugInfo) fields() map[string]any {
	return map[string]any{
		"handler":       info.Handler,
		"type":          info.Type,
		"key":           info.Key,
		"tid":           info.Tid,
		"tids":          info.Tids,
		"tids_count":    len(info.Tids),
		"groups":        info.Groups,
		"groups_count":  len(info.Groups),
		"params_count":  info.ParamsCount,
		"param_types":   info.ParamTypes,
		"param_samples": info.ParamSamples,
		"ref_count":     info.RefCount,
		"has_ret_chan":  info.HasRetChan,
		"cost_pool":     info.CostPool,
		"remote":        info.Remote,
		"context_valid": info.ContextValid,
		"trace_id":      info.TraceID,
		"trace_enabled": info.TraceEnabled,
		"trace_reason":  info.TraceReason,
		"trace_tags":    info.TraceTags,
		"msg":           info.MsgString,
	}
}

func cloneInt64s(src []int64) []int64 {
	if len(src) == 0 {
		return nil
	}
	dst := make([]int64, len(src))
	copy(dst, src)
	return dst
}

func cloneInt64Groups(src [][]int64) [][]int64 {
	if len(src) == 0 {
		return nil
	}
	dst := make([][]int64, len(src))
	for i, group := range src {
		dst[i] = cloneInt64s(group)
	}
	return dst
}

func nestParamTypes(params []any) []string {
	if len(params) == 0 {
		return nil
	}
	ret := make([]string, len(params))
	for i, param := range params {
		if param == nil {
			ret[i] = "nil"
		} else {
			ret[i] = fmt.Sprintf("%T", param)
		}
	}
	return ret
}

func nestParamSamples(params []any) []string {
	if len(params) == 0 {
		return nil
	}
	ret := make([]string, len(params))
	for i, param := range params {
		ret[i] = truncateNestDebugText(fmt.Sprintf("%#v", param), 256)
	}
	return ret
}

func truncateNestDebugText(text string, limit int) string {
	if limit <= 0 || len(text) <= limit {
		return text
	}
	return text[:limit] + "...(truncated)"
}

func logSlowDispatchTrace(info nestMsgDebugInfo, cost time.Duration, result string, suppressed uint64) {
	stack, truncated := captureSlowDispatchStack()
	flog.NewELog().Title("nest").Warn("slow dispatch trace",
		"handler", info.Handler,
		"type", info.Type,
		"key", info.Key,
		"cost_ms", cost.Milliseconds(),
		"cost", cost.String(),
		"result", result,
		"msg_info", info.fields(),
		"stack_truncated", truncated,
		"suppressed_since_last_trace", suppressed,
		"stack", stack,
	)
}

type nestTraceEventInfo struct {
	Active      bool
	Handler     string
	Type        string
	Key         int64
	Tid         int64
	TidsCount   int
	GroupsCount int
	Remote      bool
	TraceID     string
	Reason      string
	Tags        map[string]string
	Source      string
	PlayerID    int64
	MsgID       uint32
	Seq         uint32
}

func newNestTraceEventInfo(msg *Msg) nestTraceEventInfo {
	if msg == nil || !msg.Context.Trace.Active() {
		return nestTraceEventInfo{}
	}
	return nestTraceEventInfo{
		Active:      true,
		Handler:     msg.Name,
		Type:        msg.Type.String(),
		Key:         msg.Key(),
		Tid:         msg.Tid,
		TidsCount:   len(msg.Tids),
		GroupsCount: len(msg.GroupTIds),
		Remote:      msg.HasRemote,
		TraceID:     msg.Context.Trace.TraceID,
		Reason:      msg.Context.Trace.Reason,
		Tags:        msg.Context.Trace.Clone().Tags,
		Source:      msg.Context.Meta.Source,
		PlayerID:    msg.Context.Meta.PlayerID,
		MsgID:       msg.Context.Meta.MsgID,
		Seq:         msg.Context.Meta.Seq,
	}
}

func emitNestTraceEvent(msg *Msg, event string, result string, cost time.Duration) {
	emitNestTraceEventInfo(newNestTraceEventInfo(msg), event, result, cost)
}

func emitNestTraceEventInfo(info nestTraceEventInfo, event string, result string, cost time.Duration) {
	if !info.Active {
		return
	}
	labels := metrics.Labels{
		"handler": info.Handler,
		"type":    info.Type,
		"event":   event,
		"result":  result,
	}
	metrics.IncCounter("nest.trace.events.total", labels, 1)
	if cost > 0 {
		metrics.ObserveDuration("nest.trace.cost", labels, cost)
	}
	flog.NewELog().Title("nest_trace").Info("nest trace event",
		"trace_id", info.TraceID,
		"reason", info.Reason,
		"event", event,
		"result", result,
		"handler", info.Handler,
		"type", info.Type,
		"key", info.Key,
		"tid", info.Tid,
		"tids", info.TidsCount,
		"groups", info.GroupsCount,
		"cost_ms", cost.Milliseconds(),
		"cost", cost.String(),
		"remote", info.Remote,
		"source", info.Source,
		"player", info.PlayerID,
		"msg_id", info.MsgID,
		"seq", info.Seq,
		"tags", info.Tags,
	)
}

func captureSlowDispatchStack() (string, bool) {
	buf := make([]byte, nestSlowDispatchStackLimit)
	n := runtime.Stack(buf, true)
	return string(buf[:n]), n == len(buf)
}

func logAsyncDispatchError(msg *Msg, err error) {
	if msg == nil || err == nil {
		return
	}
	flog.NewELog().Title("nest").Warn("async handler failed",
		"handler", msg.Name,
		"type", msg.Type.String(),
		"key", msg.Key(),
		"tid", msg.Tid,
		"tids", len(msg.Tids),
		"groups", len(msg.GroupTIds),
		"cost", msg.Cost,
		"remote", msg.HasRemote,
		"err", err,
	)
}

// observeLockHold records the per-handler entity-lock hold time and flags
// holds beyond the configured threshold — the operational signal for "which
// handler is doing slow work inside its entity locks" (the exact cost class
// DurabilityPipelined exists to remove).
func (mgr *NestMgr) observeLockHold(handler string, hold time.Duration) {
	metrics.ObserveDuration("nest.handler.lock_hold", metrics.Labels{"handler": handler}, hold)
	if threshold := mgr.slowLockThreshold; threshold > 0 && hold >= threshold {
		metrics.IncCounter("nest.handler.lock_hold.slow.total", metrics.Labels{"handler": handler}, 1)
		slog.Warn("nest handler held entity locks beyond threshold", "handler", handler, "hold", hold, "threshold", threshold)
	}
}
