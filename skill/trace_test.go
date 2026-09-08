package skill

import "testing"

type traceSinkForTest struct {
	events []TraceEvent
	err    error
}

func (sink *traceSinkForTest) Append(event TraceEvent) error {
	sink.events = append(sink.events, event)
	return sink.err
}

func TestTraceIsBoundedAndDoesNotChangeGameplay(t *testing.T) {
	program, environment := compileRuntimeFixture(t, "simple_damage.json")
	withoutHost := runtimeTestHost(environment)
	without := NewRuntime(withoutHost, RuntimeOptions{MatchSeed: fixedTestSeed(31)})
	if _, err := without.Activate(program, CastInput{Caster: 1, Target: 2}); err != nil {
		t.Fatal(err)
	}
	withHost := runtimeTestHost(environment)
	with := NewRuntime(withHost, RuntimeOptions{MatchSeed: fixedTestSeed(31), TraceLimits: TraceLimits{MaxBuffer: 1}})
	if _, err := with.Activate(program, CastInput{Caster: 1, Target: 2}); err != nil {
		t.Fatal(err)
	}
	if withoutHost.HealthForTest(2) != withHost.HealthForTest(2) {
		t.Fatal("trace changed gameplay")
	}
	trace := with.InspectTrace()
	if len(trace) != 2 || trace[0].Kind != TraceCastActivated || trace[1].Kind != TraceTruncated {
		t.Fatalf("trace = %#v, want activation followed by truncation", trace)
	}
}

func TestTraceSinkIsDrainedOutsideGameplayAndFailuresAreIgnored(t *testing.T) {
	program, environment := compileRuntimeFixture(t, "simple_damage.json")
	host := runtimeTestHost(environment)
	sink := &traceSinkForTest{}
	runtime := NewRuntime(host, RuntimeOptions{MatchSeed: fixedTestSeed(32), TraceSink: sink, TraceLimits: TraceLimits{MaxBuffer: 4}})
	if _, err := runtime.Activate(program, CastInput{Caster: 1, Target: 2}); err != nil {
		t.Fatal(err)
	}
	if len(sink.events) != 0 {
		t.Fatal("runtime must not synchronously call the trace sink")
	}
	runtime.FlushTrace()
	if len(sink.events) == 0 {
		t.Fatal("FlushTrace did not deliver buffered observations")
	}
	sink.err = assertTraceError{}
	runtime.recordTrace(TraceEvent{Kind: TraceCastPrepared})
	runtime.FlushTrace()
	if _, err := runtime.Activate(program, CastInput{Caster: 1, Target: 2}); err != nil {
		t.Fatalf("trace sink failure changed gameplay: %v", err)
	}
}

type assertTraceError struct{}

func (assertTraceError) Error() string { return "trace sink unavailable" }
