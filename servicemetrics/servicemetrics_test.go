package servicemetrics

import (
	"sync"
	"testing"
)

type recorder struct {
	mu       sync.Mutex
	accepted []string
	refused  [][2]string
	replayed []string
	dropped  map[string]int
	conflict []string
	depth    map[string]int64
}

func newRecorder() *recorder {
	return &recorder{dropped: map[string]int{}, depth: map[string]int64{}}
}

func (r *recorder) Accepted(op string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.accepted = append(r.accepted, op)
}

func (r *recorder) Refused(op, reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.refused = append(r.refused, [2]string{op, reason})
}

func (r *recorder) Replayed(op string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.replayed = append(r.replayed, op)
}

func (r *recorder) Dropped(op string, count int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.dropped[op] += count
}

func (r *recorder) Conflict(op string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.conflict = append(r.conflict, op)
}

func (r *recorder) Depth(name string, value int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.depth[name] = value
}

// A nil reporter must never be a reason for an operation to fail. Every call
// site is unconditional precisely so a report cannot be forgotten, which only
// works if the nil case is safe.
func TestANilReporterIsSafeOnEveryMethod(t *testing.T) {
	sink := Wrap(nil)
	sink.Accepted("op")
	sink.Refused("op", "why")
	sink.Replayed("op")
	sink.Dropped("op", 3)
	sink.Conflict("op")
	sink.Depth("queue", 7)
	if sink.Enabled() {
		t.Fatal("a nil reporter reports itself as enabled")
	}
}

func TestEveryEventReachesTheReporter(t *testing.T) {
	recorder := newRecorder()
	sink := Wrap(recorder)
	if !sink.Enabled() {
		t.Fatal("a wrapped reporter reports itself as disabled")
	}
	sink.Accepted("publish")
	sink.Refused("publish", "denied")
	sink.Replayed("publish")
	sink.Dropped("retention", 4)
	sink.Conflict("append")
	sink.Depth("queue", 12)

	if len(recorder.accepted) != 1 || recorder.accepted[0] != "publish" {
		t.Fatalf("accepted = %v", recorder.accepted)
	}
	if len(recorder.refused) != 1 || recorder.refused[0] != [2]string{"publish", "denied"} {
		t.Fatalf("refused = %v", recorder.refused)
	}
	if len(recorder.replayed) != 1 {
		t.Fatalf("replayed = %v", recorder.replayed)
	}
	if recorder.dropped["retention"] != 4 {
		t.Fatalf("dropped = %v", recorder.dropped)
	}
	if len(recorder.conflict) != 1 {
		t.Fatalf("conflict = %v", recorder.conflict)
	}
	if recorder.depth["queue"] != 12 {
		t.Fatalf("depth = %v", recorder.depth)
	}
}

// A zero or negative drop count is not an event. Reporting it would put noise
// in the one signal that says data was lost.
func TestAZeroDropCountIsNotReported(t *testing.T) {
	recorder := newRecorder()
	sink := Wrap(recorder)
	sink.Dropped("retention", 0)
	sink.Dropped("retention", -1)
	if len(recorder.dropped) != 0 {
		t.Fatalf("a non-positive drop count was reported: %v", recorder.dropped)
	}
	sink.Dropped("retention", 1)
	if recorder.dropped["retention"] != 1 {
		t.Fatal("a real drop was not reported")
	}
}

// A Sink is copied by value into services, so it must be safe to share across
// goroutines — every service reports from concurrent request paths.
func TestASinkIsSafeToShareAcrossGoroutines(t *testing.T) {
	recorder := newRecorder()
	sink := Wrap(recorder)
	var wait sync.WaitGroup
	for writer := 0; writer < 8; writer++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for i := 0; i < 50; i++ {
				sink.Accepted("op")
				sink.Conflict("op")
			}
		}()
	}
	wait.Wait()
	if len(recorder.accepted) != 400 {
		t.Fatalf("accepted %d events, want 400", len(recorder.accepted))
	}
	if len(recorder.conflict) != 400 {
		t.Fatalf("conflict %d events, want 400", len(recorder.conflict))
	}
}

// Depth is a gauge, so the Recorder must replace rather than accumulate.
// Summing successive readings of a gauge produces a number that means nothing,
// and a test asserting on that number would pass for the wrong reason.
func TestRecorderTreatsDepthAsAGauge(t *testing.T) {
	recorder := NewRecorder()
	sink := Wrap(recorder)
	sink.Depth("queue.a", 3)
	sink.Depth("queue.a", 5)
	if got := recorder.Count("depth:queue.a"); got != 5 {
		t.Fatalf("depth reported %d after readings of 3 then 5, want 5; %s", got, recorder.Events())
	}
}

// Dropped accumulates, because a drop count is a rate: two sweeps that each
// lost three tickets lost six tickets.
func TestRecorderAccumulatesDrops(t *testing.T) {
	recorder := NewRecorder()
	sink := Wrap(recorder)
	sink.Dropped("ticket.expired", 3)
	sink.Dropped("ticket.expired", 3)
	if got := recorder.Count("dropped:ticket.expired"); got != 6 {
		t.Fatalf("two drops of 3 reported %d, want 6; %s", got, recorder.Events())
	}
}

// A refusal's reason is part of its identity: "a client is probing" and "the
// policy rejects everyone" have nothing in common but the operation name.
func TestRecorderKeepsRefusalReasonsApart(t *testing.T) {
	recorder := NewRecorder()
	sink := Wrap(recorder)
	sink.Refused("publish", "policy")
	sink.Refused("publish", "system_only")
	if got := recorder.Count("refused:publish:policy"); got != 1 {
		t.Fatalf("policy refusals reported %d, want 1; %s", got, recorder.Events())
	}
	if got := recorder.Count("refused:publish:system_only"); got != 1 {
		t.Fatalf("system-only refusals reported %d, want 1; %s", got, recorder.Events())
	}
}

// An empty Recorder has to render something a failure message can print,
// because "reported: " followed by nothing reads as a truncated log line.
func TestAnEmptyRecorderRendersReadably(t *testing.T) {
	if got := NewRecorder().Events(); got == "" {
		t.Fatal("an empty recorder rendered an empty string")
	}
}
