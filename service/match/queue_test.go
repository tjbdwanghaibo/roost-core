// 队列：键转义单射、同一逻辑队列的分区共用 hash tag（含真实 Redis Cluster 上的跨分区成局）、分数窗口饱和算术与真实距离、过期 sweep 失败计数与按配置扫描。

package match

import (
	"context"
	"errors"
	"math"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/infra/observe/servicemetrics"
	fredis "github.com/tjbdwanghaibo/roost-core/infra/storage/redis"
	redisdriver "github.com/tjbdwanghaibo/roost-core/infra/storage/redis/driver"
	"github.com/tjbdwanghaibo/roost-core/infra/storage/redis/redistest"
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
	// 键形状：花括号里是逻辑队列（hash tag），其后是分区（维护者 2026-10-19，旧键不迁移）。
	if got := (Queue{Mode: "duel", GroupSize: 2, Partition: "eu"}).Key(); got != "{duel:2}:eu" {
		t.Fatalf("plain key is %q, want {duel:2}:eu", got)
	}
	if got := (Queue{Mode: "duel", GroupSize: 2}).Key(); got != "{duel:2}:" {
		t.Fatalf("plain key with empty partition is %q, want {duel:2}:", got)
	}
	// Injective across the whole reserved set, including the escape character
	// and the hash-tag braces.
	seen := map[string]Queue{}
	for _, q := range []Queue{
		{Mode: "a%3A2", GroupSize: 3, Partition: "x"}, {Mode: "a:2", GroupSize: 3, Partition: "x"},
		{Mode: "a", GroupSize: 3, Partition: "%3Ax"}, {Mode: "a", GroupSize: 3, Partition: ":x"},
		{Mode: "a%", GroupSize: 3, Partition: "x"}, {Mode: "a%25", GroupSize: 3, Partition: "x"},
		{Mode: "a}", GroupSize: 3, Partition: "x"}, {Mode: "a%7D", GroupSize: 3, Partition: "x"},
		{Mode: "a{", GroupSize: 3, Partition: "x"}, {Mode: "a", GroupSize: 3, Partition: "{x}"},
		{Mode: "a}:3}:x", GroupSize: 3, Partition: ""}, {Mode: "a", GroupSize: 3, Partition: "}:x"},
	} {
		if prev, dup := seen[q.Key()]; dup {
			t.Fatalf("%+v and %+v share key %q", prev, q, q.Key())
		}
		seen[q.Key()] = q
	}
}

// 同一逻辑队列（Mode + GroupSize）的所有分区在 Redis Cluster 的同一个 slot，跨分区成局的多键脚本才能执行；
// 不同逻辑队列的 hash tag 不同。Mode 里的花括号转义，不能让 hash tag 提前结束而把分区算进去、或只剩 Mode 的一段。
// 真实 Redis 的 CLUSTER KEYSLOT 核对见 wiring/integration 的 TestMatchCrossPartitionKeysShareOneSlotOnRedis。
func TestPartitionsOfOneLogicalQueueShareAHashTag(t *testing.T) {
	const prefix = "roost:game:match"
	for _, mode := range []string{"ranked", "a:b", "x}y", "{x", "}", "%7D", "50%"} {
		gold := Queue{Mode: mode, GroupSize: 5, Partition: "gold"}
		tags := map[string]bool{}
		for _, partition := range []string{"gold", "silver", "", "{gold}", "}:", "east:1"} {
			queue := gold
			queue.Partition = partition
			tags[clusterHashTag(RedisKey(prefix, queue))] = true
		}
		if len(tags) != 1 {
			t.Fatalf("mode %q: the partitions of one logical queue hash on %d different tags: %v", mode, len(tags), tags)
		}
		other := gold
		other.GroupSize = 10
		if clusterHashTag(RedisKey(prefix, other)) == clusterHashTag(RedisKey(prefix, gold)) {
			t.Fatalf("mode %q: group sizes 5 and 10 hash on the same tag", mode)
		}
		if want := gold.LogicalKey(); clusterHashTag(RedisKey(prefix, gold)) != want {
			t.Fatalf("mode %q: hash tag is %q, want the logical key %q", mode, clusterHashTag(RedisKey(prefix, gold)), want)
		}
	}
	if got := RedisKey(prefix, Queue{Mode: "duel", GroupSize: 2, Partition: "eu"}); got != "roost:game:match:queue:{duel:2}:eu" {
		t.Fatalf("redis key is %q", got)
	}
}

// 前缀自带的 hash tag 可以（整个匹配服务落在一个 slot，仍满足同槽）；第一对花括号为空时 Redis 对整个键求槽，
// 同一逻辑队列的分区会散开，构造时拒绝。
func TestNewRedisStoreRefusesAPrefixThatBreaksTheHashTag(t *testing.T) {
	for _, prefix := range []string{"roost:match", "{roost}:match", "roost{:match", "roost}:match"} {
		if _, err := NewRedisStore(&noRedis{}, prefix, Config{}); err != nil {
			t.Fatalf("prefix %q was refused: %v", prefix, err)
		}
	}
	for _, prefix := range []string{"roost{}:match", "{}"} {
		if _, err := NewRedisStore(&noRedis{}, prefix, Config{}); err == nil || !strings.Contains(err.Error(), "first hash tag is empty") {
			t.Fatalf("prefix %q = %v, want a refusal naming the empty hash tag", prefix, err)
		}
	}
}

// noRedis 只用来构造 store，任何调用都失败。
type noRedis struct{}

func (*noRedis) Eval(context.Context, string, []string, ...any) (any, error) {
	return nil, errors.New("no redis")
}
func (*noRedis) Get(context.Context, string) ([]byte, error)   { return nil, errors.New("no redis") }
func (*noRedis) Del(context.Context, ...string) (int64, error) { return 0, errors.New("no redis") }

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

// 真实 Redis Cluster（ROOST_REVIEW_CLUSTER，入口 wiring/scripts/integration/redis-cluster-suites.sh）：
// 同一逻辑队列的分区键 CLUSTER KEYSLOT 相同，跨分区成局的多键脚本在集群上执行成功；一张票被取消时整体失败。
// 前缀自带 hash tag 时（整个匹配服务一个 slot）同样成立。
func TestCrossPartitionCommitRunsOnRedisCluster(t *testing.T) {
	addrs := os.Getenv("ROOST_REVIEW_CLUSTER")
	if addrs == "" {
		t.Skip("ROOST_REVIEW_CLUSTER is not set")
	}
	cfg := fredis.DefaultConfig("")
	cfg.ClusterAddrs = strings.Split(addrs, ",")
	client, err := redisdriver.NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	raw := client.(*redisdriver.Client).Raw()
	ctx := context.Background()
	for _, name := range []string{"match-cluster", "{match-cluster}"} {
		keyPrefix := redistest.Prefix(t, addrs, name)
		store, err := NewRedisStore(client, keyPrefix, Config{TicketTTL: time.Hour})
		if err != nil {
			t.Fatal(err)
		}
		partitions := []Queue{tiered("gold"), tiered("silver"), tiered("bronze"), tiered("x}y{z")}
		slots := map[int64]bool{}
		for _, queue := range partitions {
			slot, err := raw.ClusterKeySlot(ctx, RedisKey(keyPrefix, queue)).Result()
			if err != nil {
				t.Fatal(err)
			}
			slots[slot] = true
		}
		if len(slots) != 1 {
			t.Fatalf("prefix %q: the partitions of one logical queue are in slots %v", keyPrefix, slots)
		}
		var tickets []Ticket
		for i, queue := range partitions[:3] {
			for j := 0; j < 2; j++ {
				ticket, err := store.Enqueue(ctx, queue, player(int64(10*i+j+1), 0), "")
				if err != nil {
					t.Fatal(err)
				}
				tickets = append(tickets, ticket)
			}
		}
		formed, err := store.CommitAcross(ctx, refsOf(tickets[0], tickets[2], tickets[4]))
		if err != nil {
			t.Fatalf("prefix %q: CommitAcross on Redis Cluster: %v", keyPrefix, err)
		}
		for _, queue := range partitions[:3] {
			if _, found, err := store.Match(ctx, queue, formed.ID); err != nil || !found {
				t.Fatalf("prefix %q: the match read through %s: found=%v err=%v", keyPrefix, queue.Partition, found, err)
			}
		}
		if _, err := store.Cancel(ctx, tickets[5].Queue, tickets[5].ID, tickets[5].Subject); err != nil {
			t.Fatal(err)
		}
		before, err := raw.Get(ctx, RedisKey(keyPrefix, partitions[0])).Result()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.CommitAcross(ctx, refsOf(tickets[1], tickets[3], tickets[5])); !errors.Is(err, ErrConflict) {
			t.Fatalf("prefix %q: a set with a cancelled ticket on Redis Cluster = %v, want ErrConflict", keyPrefix, err)
		}
		if after, _ := raw.Get(ctx, RedisKey(keyPrefix, partitions[0])).Result(); after != before {
			t.Fatalf("prefix %q: a refused cross-partition commit changed partition gold on Redis Cluster", keyPrefix)
		}
	}
}
