package match

import (
	"context"
	"sync"
	"testing"
	"time"

)

// newSweepStore builds the in-process store the sweep tests drive. The domain and
// its own tests live in roost-core/service/match (M-06); what this package
// tests is the loop around it.
func newSweepStore(t *testing.T, options ...func(*Config)) (Store, struct{}) {
	t.Helper()
	cfg := Config{}
	for _, option := range options {
		option(&cfg)
	}
	store, err := NewMemoryStore(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return store, struct{}{}
}

// sweepRecorder wraps a real store and records which queues the background
// loop asked it to sweep.
type sweepRecorder struct {
	Store
	mu    sync.Mutex
	swept []string
	seen  chan struct{}
}

func (r *sweepRecorder) Sweep(_ context.Context, queue Queue, _ int) (int, error) {
	r.mu.Lock()
	r.swept = append(r.swept, queue.Key())
	r.mu.Unlock()
	select {
	case r.seen <- struct{}{}:
	default:
	}
	return 0, nil
}

// The wrapper forwards the configured queue set, as a deployment's store does.
func (r *sweepRecorder) SweepQueues() []Queue { return r.Store.(queueSet).SweepQueues() }

// The expiry loop sweeps the queues the deployment configured. Until U-0022
// it iterated over a method returning a hardcoded nil, so no process ever
// swept anything in the background: expired tickets were resolved only when
// something touched them.
func TestTheExpiryLoopSweepsTheConfiguredQueues(t *testing.T) {
	previous := sweepEvery
	sweepEvery = 5 * time.Millisecond
	t.Cleanup(func() { sweepEvery = previous })

	ranked := Queue{Mode: "ranked", GroupSize: 2, Partition: "asia"}
	store, _ := newSweepStore(t, func(cfg *Config) { cfg.SweepQueues = []Queue{ranked} })
	recorder := &sweepRecorder{Store: store, seen: make(chan struct{}, 1)}
	server := &Server{service: recorder}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.run(ctx) }()
	select {
	case <-recorder.seen:
	case <-time.After(5 * time.Second):
		t.Fatal("the expiry loop never swept the configured queue")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	if len(recorder.swept) == 0 || recorder.swept[0] != ranked.Key() {
		t.Fatalf("swept %v, want %s", recorder.swept, ranked.Key())
	}
}

// A store that does not expose a queue set sweeps nothing — and says so at
// start rather than pretending; the loop must still run and stop cleanly.
func TestAStoreWithoutAQueueSetSweepsNothing(t *testing.T) {
	previous := sweepEvery
	sweepEvery = 5 * time.Millisecond
	t.Cleanup(func() { sweepEvery = previous })
	store, _ := newSweepStore(t)
	if got := sweepQueuesOf(store); len(got) != 0 {
		t.Fatalf("an unconfigured store sweeps %v", got)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := (&Server{service: store}).run(ctx); err != nil {
		t.Fatal(err)
	}
}

