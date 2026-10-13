package skill

// TraceSink 是可选的观测消费者。Runtime 执行过程中从不调用它；调用方经 FlushTrace 排空有界缓冲。
type TraceSink interface{ Append(TraceEvent) error }

type TraceLimits struct {
	MaxEventsPerCast int
	MaxEventsPerRoot int
	MaxBuffer        int
}

// TraceEvent 是有界、被动的运行期观测记录，刻意不含随机密钥、种子与可变的 Host 载荷。
type TraceEvent struct {
	Sequence      uint64
	Kind          TraceEventKind
	Tick          Tick
	WorldRevision WorldRevision
	CastID        CastID
}

type TraceEventKind string

const (
	TraceCastActivated TraceEventKind = "cast_activated"
	TraceCastPrepared  TraceEventKind = "cast_prepared"
	TraceTruncated     TraceEventKind = "trace_truncated"
)

func (runtime *Runtime) recordTrace(event TraceEvent) {
	limit := runtime.traceBufferLimit()
	if limit <= 0 || runtime.traceTruncated {
		return
	}
	if event.WorldRevision == 0 {
		event.WorldRevision = runtime.host.CurrentRevision()
	}
	if len(runtime.trace) >= limit {
		runtime.traceSequence++
		runtime.trace = append(runtime.trace, TraceEvent{Sequence: runtime.traceSequence, Kind: TraceTruncated, Tick: runtime.currentTick})
		runtime.traceTruncated = true
		return
	}
	runtime.traceSequence++
	event.Sequence = runtime.traceSequence
	runtime.trace = append(runtime.trace, event)
}

func (runtime *Runtime) traceBufferLimit() int {
	return runtime.options.TraceLimits.MaxBuffer
}

// InspectTrace 返回缓冲中的 trace 事件副本。
func (runtime *Runtime) InspectTrace() []TraceEvent {
	runtime.mutex.Lock()
	defer runtime.mutex.Unlock()
	return append([]TraceEvent(nil), runtime.trace...)
}

// FlushTrace 在玩法执行路径之外投递缓冲中的观测记录。Sink 失败时不报错，事件留在队列里等之后重试。
func (runtime *Runtime) FlushTrace() {
	runtime.mutex.Lock()
	sink := runtime.options.TraceSink
	if sink == nil || runtime.traceFlushed >= len(runtime.trace) {
		runtime.mutex.Unlock()
		return
	}
	start := runtime.traceFlushed
	events := append([]TraceEvent(nil), runtime.trace[start:]...)
	runtime.mutex.Unlock()
	for index, event := range events {
		if sink.Append(event) != nil {
			return
		}
		runtime.mutex.Lock()
		if runtime.traceFlushed < start+index+1 {
			runtime.traceFlushed = start + index + 1
		}
		runtime.mutex.Unlock()
	}
}
