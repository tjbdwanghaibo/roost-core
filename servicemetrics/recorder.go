package servicemetrics

import (
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Recorder is a Reporter that remembers what it was told, for tests.
//
// It lives in the production package rather than in each package's test files
// because every service package needs the same thing: proof that its report
// calls are actually reached. Six near-identical hand-written recorders is six
// places for one to quietly stop asserting, which is the failure mode this
// whole seam exists to prevent.
//
// It is safe for concurrent use, because the paths worth asserting on —
// compare-and-set conflicts, replays — are the concurrent ones.
type Recorder struct {
	mu     sync.Mutex
	events map[string]int
}

// NewRecorder returns an empty Recorder.
func NewRecorder() *Recorder { return &Recorder{events: map[string]int{}} }

func (r *Recorder) add(event string, count int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.events == nil {
		r.events = map[string]int{}
	}
	r.events[event] += count
}

func (r *Recorder) Accepted(op string)         { r.add("accepted:"+op, 1) }
func (r *Recorder) Refused(op, reason string)  { r.add("refused:"+op+":"+reason, 1) }
func (r *Recorder) Replayed(op string)         { r.add("replayed:"+op, 1) }
func (r *Recorder) Dropped(op string, n int)   { r.add("dropped:"+op, n) }
func (r *Recorder) Conflict(op string)         { r.add("conflict:"+op, 1) }
func (r *Recorder) Depth(name string, v int64) { r.set("depth:"+name, v) }

// set replaces rather than accumulates: a depth is a gauge, and summing
// successive readings of a gauge produces a number that means nothing.
func (r *Recorder) set(event string, value int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.events == nil {
		r.events = map[string]int{}
	}
	r.events[event] = int(value)
}

// Count returns how many times one event was reported. Event names are the
// same strings Events renders, so a failing assertion and the dump beside it
// speak the same language.
func (r *Recorder) Count(event string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.events[event]
}

// Snapshot returns a copy of every recorded event keyed by the same names
// Count takes, so a package with its own assertion vocabulary can derive it
// without reimplementing the Reporter.
func (r *Recorder) Snapshot() map[string]int {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]int, len(r.events))
	for event, count := range r.events {
		out[event] = count
	}
	return out
}

// Events renders every recorded event, sorted, for a test failure message.
func (r *Recorder) Events() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.events) == 0 {
		return "(nothing reported)"
	}
	keys := make([]string, 0, len(r.events))
	for key := range r.events {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var out strings.Builder
	for i, key := range keys {
		if i > 0 {
			out.WriteString(", ")
		}
		out.WriteString(key)
		out.WriteString("=")
		out.WriteString(strconv.Itoa(r.events[key]))
	}
	return out.String()
}

var _ Reporter = (*Recorder)(nil)
