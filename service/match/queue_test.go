// 队列：键转义单射、分数窗口饱和算术与真实距离、过期 sweep 失败计数与按配置扫描。

package match

import (
	"context"
	"errors"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/infra/observe/servicemetrics"
	"github.com/tjbdwanghaibo/roost-core/infra/storage/versionstore"
)

// 两个都通过 Validate 的 Queue 不映射到同一个存储键：Mode / Partition 里的保留字符转义，键是单射；不含保留字符的键形状不变。
// 历史：RR-20260917-01。
func TestDistinctQueuesNeverShareAKey(t *testing.T) {
	a := Queue{Mode: "a:2", GroupSize: 3, Partition: "x"}
	b := Queue{Mode: "a", GroupSize: 2, Partition: "3:x"}
	for _, q := range []Queue{a, b} {
		if err := q.Validate(); err != nil {
			t.Fatal(err)
		}
	}
	if a.Key() == b.Key() {
		t.Fatalf("two valid queues share key %q", a.Key())
	}
	// Keys without the reserved characters are unchanged, so no deployed key
	// moves (the compatibility promise of this fix).
	if got := (Queue{Mode: "duel", GroupSize: 2, Partition: "eu"}).Key(); got != "duel:2:eu" {
		t.Fatalf("plain key changed to %q", got)
	}
	if got := (Queue{Mode: "duel", GroupSize: 2}).Key(); got != "duel:2:" {
		t.Fatalf("plain key with empty partition changed to %q", got)
	}
	// Injective across the whole reserved set, including the escape character.
	seen := map[string]Queue{}
	for _, q := range []Queue{
		{Mode: "a%3A2", GroupSize: 3, Partition: "x"}, {Mode: "a:2", GroupSize: 3, Partition: "x"},
		{Mode: "a", GroupSize: 3, Partition: "%3Ax"}, {Mode: "a", GroupSize: 3, Partition: ":x"},
		{Mode: "a%", GroupSize: 3, Partition: "x"}, {Mode: "a%25", GroupSize: 3, Partition: "x"},
	} {
		if prev, dup := seen[q.Key()]; dup {
			t.Fatalf("%+v and %+v share key %q", prev, q, q.Key())
		}
		seen[q.Key()] = q
	}
}

func TestACommitCannotTakeTicketsFromAnotherQueue(t *testing.T) {
	a := Queue{Mode: "a:2", GroupSize: 3, Partition: "x"}
	b := Queue{Mode: "a", GroupSize: 2, Partition: "3:x"}
	store, err := NewMemoryStore(Config{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	x, err := store.Enqueue(ctx, a, Subject{Kind: "player", ID: 1}, "")
	if err != nil {
		t.Fatal(err)
	}
	y, err := store.Enqueue(ctx, b, Subject{Kind: "player", ID: 2}, "")
	if err != nil {
		t.Fatal(err)
	}
	candidates, err := store.Candidates(ctx, b, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 || candidates[0].ID != y.ID {
		t.Fatalf("Candidates of queue b returned %d tickets (%v); a ticket from queue a leaked in", len(candidates), candidates)
	}
	if _, err := store.Commit(ctx, b, []string{x.ID, y.ID}); err == nil || !errors.Is(err, ErrTicketMissing) {
		t.Fatalf("Commit on queue b accepted a ticket from queue a: err=%v", err)
	}
}

// ScoreWindowGrouping 的距离与窗口算术不溢出（饱和算术）：有符号减法回绕不能把远端分数判进窗口，Initial + Widen*waited 不先溢出为负绕过
// MaxWindow；Score 的语义归调用方，API 没限制区间。
// 历史：RR-20260917-02。
func TestScoreWindowArithmeticDoesNotOverflow(t *testing.T) {
	queue := Queue{Mode: "ranked", GroupSize: 2}
	ticket := func(id string, score int64) Ticket {
		return Ticket{ID: id, Queue: queue, State: TicketWaiting, ExpiresAtUnix: 2000, CreatedAtUnix: 1000,
			Subject: Subject{Kind: "player", ID: int64(len(id)), Score: score}}
	}
	for _, tc := range []struct {
		label      string
		grouping   ScoreWindowGrouping
		now        int64
		a, b       int64
		wantFormed bool
	}{
		{"zero vs MinInt64 outside a window of 10", ScoreWindowGrouping{InitialWindow: 10}, 1000, 0, math.MinInt64, false},
		{"MaxInt64 vs MinInt64 outside a window of 10", ScoreWindowGrouping{InitialWindow: 10}, 1000, math.MaxInt64, math.MinInt64, false},
		{"window addition saturates then is capped", ScoreWindowGrouping{InitialWindow: math.MaxInt64 - 5, WidenPerSecond: 10, MaxWindow: 100}, 1001, 0, 5, true},
		{"window multiplication saturates then is capped", ScoreWindowGrouping{InitialWindow: 10, WidenPerSecond: math.MaxInt64/2 + 1, MaxWindow: 100}, 1002, 0, 5, true},
		{"ordinary near scores still group", ScoreWindowGrouping{InitialWindow: 10}, 1000, 100, 105, true},
		{"ordinary far scores still wait", ScoreWindowGrouping{InitialWindow: 10}, 1000, 100, 200, false},
		{"negative neighbours group", ScoreWindowGrouping{InitialWindow: 10}, 1000, -100, -95, true},
		{"a clock that went backwards widens nothing", ScoreWindowGrouping{InitialWindow: 10, WidenPerSecond: 100}, 500, 100, 150, false},
	} {
		g := tc.grouping
		now := tc.now
		g.NowUnix = func() int64 { return now }
		_, formed, err := g.Group(queue, []Ticket{ticket("a", tc.a), ticket("bb", tc.b)})
		if err != nil {
			t.Fatalf("%s: %v", tc.label, err)
		}
		if formed != tc.wantFormed {
			t.Fatalf("%s: formed=%v, want %v", tc.label, formed, tc.wantFormed)
		}
	}
}

// Sorting must agree with the filter: with an overflowing distance the far
// candidate could sort ahead of a near one.
func TestScoreWindowOrdersByTrueDistance(t *testing.T) {
	queue := Queue{Mode: "ranked", GroupSize: 2}
	mk := func(id string, playerID, score, created int64) Ticket {
		return Ticket{ID: id, Queue: queue, State: TicketWaiting, ExpiresAtUnix: 9000, CreatedAtUnix: created,
			Subject: Subject{Kind: "player", ID: playerID, Score: score}}
	}
	g := ScoreWindowGrouping{InitialWindow: 0, NowUnix: func() int64 { return 1000 }}
	g.MaxWindow = 0
	// An uncapped window that has widened to "everything" makes ordering the
	// only thing that decides who plays.
	g.WidenPerSecond = math.MaxInt64
	group, formed, err := g.Group(queue, []Ticket{mk("anchor", 1, 0, 1), mk("far", 2, math.MinInt64, 2), mk("near", 3, 5, 3)})
	if err != nil || !formed {
		t.Fatalf("formed=%v err=%v", formed, err)
	}
	if group[1].ID != "near" {
		t.Fatalf("the far candidate sorted ahead of the near one: %v %v", group[0].ID, group[1].ID)
	}
}

func TestScoreWindowRefusesNegativeConfiguration(t *testing.T) {
	queue := Queue{Mode: "ranked", GroupSize: 2}
	for _, g := range []ScoreWindowGrouping{
		{InitialWindow: -1}, {WidenPerSecond: -1}, {MaxWindow: -1},
	} {
		g.NowUnix = func() int64 { return 1000 }
		if _, _, err := g.Group(queue, []Ticket{{ID: "a"}, {ID: "b"}}); !errors.Is(err, ErrQueueInvalid) {
			t.Fatalf("%+v accepted: %v", g, err)
		}
	}
}

// 过期 sweep 的失败计指标，不只记日志（否则一直失败的 sweep 看起来就像一个没有过期票的队列）。

type failingQueueState struct {
	versionstore.Store[string, queueState]
	err error
}

func (s failingQueueState) Update(context.Context, string, versionstore.Mutate[queueState]) (versionstore.Versioned[queueState], bool, error) {
	return versionstore.Versioned[queueState]{}, false, s.err
}

func TestSweepFailureIsCountedNotJustReturned(t *testing.T) {
	recorder := servicemetrics.NewRecorder()
	wire := errors.New("queue state: connection reset")
	store, err := NewStore(failingQueueState{Store: versionstore.NewMemoryStore[string, queueState](), err: wire}, Config{
		Now: func() time.Time { return time.Unix(1_700_000_000, 0) }, Metrics: recorder,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Sweep(context.Background(), ranked(), 10); !errors.Is(err, wire) {
		t.Fatalf("Sweep = %v, want the store error", err)
	}
	if got := recorder.Count("dropped:sweep.failed"); got != 1 {
		t.Fatalf("sweep.failed counted %d, want 1 (events: %s)", got, recorder.Events())
	}
	if got := recorder.Count("dropped:ticket.expired"); got != 0 {
		t.Fatalf("a failed sweep reported %d expired tickets", got)
	}
}

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

// The expiry loop sweeps the queues the deployment configured.
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
