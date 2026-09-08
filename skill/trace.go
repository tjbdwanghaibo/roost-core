package skill

// TraceSink is an optional observation consumer. Runtime execution never
// calls it; callers drain the bounded buffer through FlushTrace.
type TraceSink interface{ Append(TraceEvent) error }

type TraceLimits struct {
	MaxEventsPerCast int
	MaxEventsPerRoot int
	MaxBuffer        int
}

// TraceEvent is a bounded, passive runtime observation. It deliberately
// excludes random keys, seeds, and mutable host payloads.
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

func (runtime *Runtime) InspectTrace() []TraceEvent {
	runtime.mutex.Lock()
	defer runtime.mutex.Unlock()
	return append([]TraceEvent(nil), runtime.trace...)
}

// FlushTrace delivers buffered observations outside the gameplay execution
// path. Sink failures are deliberately ignored and leave the event queued for
// a later retry.
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
