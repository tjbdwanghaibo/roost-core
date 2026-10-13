// 聊天留存：按年龄清理的页内与尾部洞、最新页不误报洞、清理失败计数、副本时钟偏差下的翻页、留存循环只清枚举的频道。

package chat

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"sort"
	"sync"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/servicemetrics"
	fredis "github.com/tjbdwanghaibo/roost-core/infra/storage/redis"
	"github.com/tjbdwanghaibo/roost-core/infra/storage/redis/driver"
	"github.com/tjbdwanghaibo/roost-core/infra/storage/versionstore"
)

func retentionHarness(t *testing.T) *harness {
	t.Helper()
	if os.Getenv("ROOST_BUGFIX5_BACKEND") != "redis" {
		return newHarness(t)
	}
	addr := os.Getenv("ROOST_REVIEW_REDIS")
	if addr == "" {
		t.Fatal("isolated Redis address required")
	}
	client, err := driver.NewClient(fredis.DefaultConfig(addr))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	state, err := NewRedisStateStore(client, fmt.Sprintf("bugfix5:chat:%d", time.Now().UnixNano()))
	if err != nil {
		t.Fatal(err)
	}
	return newHarness(t, withState(state))
}

func TestAgePruneHandlesInteriorAndTailGaps(t *testing.T) {
	h := retentionHarness(t)
	ctx := context.Background()
	mustPublish(t, h, role(1), text("fresh-1", "1", world()))
	h.clock.advance(-2 * time.Second)
	mustPublish(t, h, role(1), text("expired-2", "2", world()))
	h.clock.advance(2 * time.Second)
	mustPublish(t, h, role(1), text("fresh-3", "3", world()))
	h.clock.advance(-2 * time.Second)
	mustPublish(t, h, role(1), text("expired-4", "4", world()))
	h.clock.advance(DefaultRetentionAge + time.Second)
	ref, err := h.service.Resolve(world(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := h.service.Prune(ctx, ref, 1); err != nil || n != 1 {
		t.Fatal(n, err)
	}
	if n, err := h.service.Prune(ctx, ref, 1); err != nil || n != 1 {
		t.Fatal(n, err)
	}
	if n, err := h.service.Prune(ctx, ref, 1); err != nil || n != 0 {
		t.Fatal(n, err)
	}
	first, err := h.service.History(ctx, role(1), HistoryQuery{Channel: world(), AfterSeq: 1, Limit: 1})
	if err != nil || !first.Gap || first.HasMore || len(first.Messages) != 1 || first.Messages[0].Seq != 3 || first.NextCursor != 3 || first.LatestSeq != 4 {
		t.Fatal(first, err)
	}
	empty, err := h.service.History(ctx, role(1), HistoryQuery{Channel: world(), AfterSeq: 3, Limit: 1})
	if err != nil || !empty.Gap || empty.NextCursor != 3 || len(empty.Messages) != 0 {
		t.Fatal(empty, err)
	}
	back, err := h.service.History(ctx, role(1), HistoryQuery{Channel: world(), BeforeSeq: 4, Limit: 2})
	if err != nil || !back.Gap || back.HasMore || len(back.Messages) != 2 || back.Messages[0].Seq != 1 || back.Messages[1].Seq != 3 || back.PrevCursor != 1 {
		t.Fatal(back, err)
	}
	if _, err := h.service.Publish(ctx, role(1), text("expired-2", "2", world())); !errors.Is(err, ErrAlreadyPublished) {
		t.Fatal(err)
	}
	stats, err := h.service.Stats(ctx, ref)
	if err != nil || stats.Evicted != 2 || stats.Retained != 2 {
		t.Fatal(stats, err)
	}
}

func TestGapOnlyReportsTheRangeActuallyPaged(t *testing.T) {
	state := channelState{LastSeq: 5, Ring: []Message{{Seq: 1}, {Seq: 2}, {Seq: 4}, {Seq: 5}}}
	first := pageOf(state, HistoryQuery{AfterSeq: 1, Limit: 1})
	if first.Gap || !first.HasMore || first.NextCursor != 2 {
		t.Fatal(first)
	}
	second := pageOf(state, HistoryQuery{AfterSeq: 2, Limit: 1})
	if !second.Gap || !second.HasMore || second.NextCursor != 4 {
		t.Fatal(second)
	}
	last := pageOf(state, HistoryQuery{AfterSeq: 4, Limit: 1})
	if last.Gap || last.HasMore || last.NextCursor != 5 {
		t.Fatal(last)
	}
	back := pageOf(state, HistoryQuery{BeforeSeq: 5, Limit: 1})
	if back.Gap || !back.HasMore || back.PrevCursor != 4 {
		t.Fatal(back)
	}
	gapBack := pageOf(state, HistoryQuery{BeforeSeq: 4, Limit: 1})
	if !gapBack.Gap || !gapBack.HasMore || gapBack.PrevCursor != 2 {
		t.Fatal(gapBack)
	}
}

// Page.Gap 只报告真正的洞：无游标取最新页、或 BeforeSeq 落在保留窗口内向前翻到保留边缘，只是正常容量淘汰时 Gap=false、不计
// history.gap.<kind>；游标落在已淘汰区间或页内序号不连续（页内 / 尾部洞）时才 Gap=true 并计一次。
// 历史：RR-20261001-08。

// rr08Chat builds a world channel retaining four messages, on Memory by default and on the isolated Redis under
// ROOST_BUGFIX5_BACKEND=redis + ROOST_REVIEW_REDIS, so one Redis run covers both retention-gap promises.
func rr08Chat(t *testing.T) *harness {
	t.Helper()
	rule := ChannelRule{Kind: ChannelWorld, RequiresTarget: true, Scope: ScopeShared, Retain: 4}
	if os.Getenv("ROOST_BUGFIX5_BACKEND") != "redis" {
		return newHarness(t, withRules(rule))
	}
	addr := os.Getenv("ROOST_REVIEW_REDIS")
	if addr == "" {
		t.Fatal("isolated Redis address required")
	}
	client, err := driver.NewClient(fredis.DefaultConfig(addr))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	state, err := NewRedisStateStore(client, fmt.Sprintf("rr08:chat:%d", time.Now().UnixNano()))
	if err != nil {
		t.Fatal(err)
	}
	return newHarness(t, withState(state), withRules(rule))
}

func TestLatestPageAfterCapacityEvictionIsNotAGap(t *testing.T) {
	ctx := context.Background()
	h := rr08Chat(t)
	// Capacity + 1: exactly one message (seq 1) is evicted by the ring.
	for i := 1; i <= 5; i++ {
		mustPublish(t, h, role(1), text(fmt.Sprintf("m%d", i), fmt.Sprintf("k%d", i), world()))
	}

	latest, err := h.service.History(ctx, role(1), HistoryQuery{Channel: world(), Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := bodies(latest), []string{"m2", "m3", "m4", "m5"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("latest page = %v, want %v", got, want)
	}
	if latest.Gap {
		t.Fatalf("the latest page after plain capacity eviction reported a gap: %+v", latest)
	}
	if latest.HasMore || latest.OldestSeq != 2 || latest.LatestSeq != 5 {
		t.Fatalf("latest page shape: %+v", latest)
	}
	if gaps := h.metrics.snapshot().gaps; gaps != 0 {
		t.Fatalf("the gap metric counted %d for a page with no hole, want 0; %s", gaps, h.metrics.Events())
	}

	// Scrolling back inside the window and reaching the retention edge is
	// the same situation: nothing the client asked for is missing.
	back, err := h.service.History(ctx, role(1), HistoryQuery{Channel: world(), BeforeSeq: 5, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := bodies(back), []string{"m2", "m3", "m4"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("scrollback page = %v, want %v", got, want)
	}
	if back.Gap || back.HasMore {
		t.Fatalf("scrollback to the retention edge reported a gap: %+v", back)
	}
	if gaps := h.metrics.snapshot().gaps; gaps != 0 {
		t.Fatalf("the gap metric counted %d after scrollback with no hole, want 0; %s", gaps, h.metrics.Events())
	}

	// One more message evicts seq 2 as well. A cursor whose successor is gone
	// is a cursor in the evicted range: a gap, counted once per page.
	mustPublish(t, h, role(1), text("m6", "k6", world()))
	stale, err := h.service.History(ctx, role(1), HistoryQuery{Channel: world(), AfterSeq: 1, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if !stale.Gap {
		t.Fatalf("a cursor whose next message (seq 2) was evicted did not report a gap: %+v", stale)
	}
	if got, want := bodies(stale), []string{"m3", "m4", "m5", "m6"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("page after the evicted cursor = %v, want %v", got, want)
	}
	if gaps := h.metrics.snapshot().gaps; gaps != 1 {
		t.Fatalf("the gap metric counted %d after one evicted cursor, want 1; %s", gaps, h.metrics.Events())
	}
	// The last evicted sequence as a cursor is the boundary: the client has
	// seen it, and the next retained message follows it directly.
	edge, err := h.service.History(ctx, role(1), HistoryQuery{Channel: world(), AfterSeq: 2, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if edge.Gap {
		t.Fatalf("a cursor on the last evicted sequence reported a gap: %+v", edge)
	}
	if gaps := h.metrics.snapshot().gaps; gaps != 1 {
		t.Fatalf("the gap metric counted %d after the boundary cursor, want still 1; %s", gaps, h.metrics.Events())
	}
	// Asking for what lies before the oldest retained message is asking for
	// the evicted range too.
	beyond, err := h.service.History(ctx, role(1), HistoryQuery{Channel: world(), BeforeSeq: 3, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if !beyond.Gap || len(beyond.Messages) != 0 {
		t.Fatalf("scrollback into the evicted range did not report a gap: %+v", beyond)
	}
	if gaps := h.metrics.snapshot().gaps; gaps != 2 {
		t.Fatalf("the gap metric counted %d after two evicted cursors, want 2; %s", gaps, h.metrics.Events())
	}
}

// Without a cursor, only holes inside or at the end of the retained ring are
// gaps; the ring simply not starting at sequence 1 is not one.
func TestLatestPageReportsOnlyInteriorAndTailHoles(t *testing.T) {
	cases := []struct {
		label string
		ring  []uint64
		gap   bool
	}{
		{label: "contiguous ring after capacity eviction", ring: []uint64{2, 3, 4, 5}, gap: false},
		{label: "single retained message", ring: []uint64{5}, gap: false},
		{label: "interior hole", ring: []uint64{2, 4, 5}, gap: true},
		{label: "tail hole", ring: []uint64{2, 3}, gap: true},
		{label: "ring emptied by age pruning", ring: nil, gap: true},
	}
	for _, testCase := range cases {
		state := channelState{LastSeq: 5}
		for _, seq := range testCase.ring {
			state.Ring = append(state.Ring, Message{Seq: seq})
		}
		page := pageOf(state, HistoryQuery{Limit: 10})
		if page.Gap != testCase.gap {
			t.Fatalf("%s: Gap = %v, want %v (%+v)", testCase.label, page.Gap, testCase.gap, page)
		}
		if page.HasMore {
			t.Fatalf("%s: a page holding the whole ring claims more", testCase.label)
		}
	}
}

// 留存循环对 Prune 的失败计指标（不只打日志）；冲突（ErrConflict）不算 prune 失败，它由 versionstore 统一计数（versionstore.conflict.total）。

type failingChannelState struct {
	versionstore.Store[string, channelState]
	err error
}

func (s failingChannelState) Update(context.Context, string, versionstore.Mutate[channelState]) (versionstore.Versioned[channelState], bool, error) {
	return versionstore.Versioned[channelState]{}, false, s.err
}

func TestPruneFailureIsCountedNotJustReturned(t *testing.T) {
	recorder := servicemetrics.NewRecorder()
	wire := errors.New("channel state: connection reset")
	store, err := NewStore(failingChannelState{Store: versionstore.NewMemoryStore[string, channelState](), err: wire}, Config{
		RetentionAge: DefaultRetentionAge, Policy: allowAllPolicy{}, Bodies: testRegistry(t), Metrics: recorder,
	})
	if err != nil {
		t.Fatal(err)
	}
	world, err := store.Resolve(Channel{Kind: ChannelWorld, Target: 1}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Prune(context.Background(), world, PruneBatch); !errors.Is(err, wire) {
		t.Fatalf("Prune = %v, want the store error", err)
	}
	if got := recorder.Count("dropped:prune.failed"); got != 1 {
		t.Fatalf("prune.failed counted %d, want 1 (events: %s)", got, recorder.Events())
	}
	if got := recorder.Count("conflict:prune"); got != 0 {
		t.Fatalf("a wire error was counted as a conflict %d times", got)
	}

	// 冲突不算失败；冲突本身由 versionstore 统一计数（第十二轮决定），chat 不再另报 Conflict。
	conflicting, err := NewStore(failingChannelState{Store: versionstore.NewMemoryStore[string, channelState](), err: versionstore.ErrConflict}, Config{
		RetentionAge: DefaultRetentionAge, Policy: allowAllPolicy{}, Bodies: testRegistry(t), Metrics: recorder,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conflicting.Prune(context.Background(), world, PruneBatch); !errors.Is(err, versionstore.ErrConflict) {
		t.Fatalf("Prune under conflict = %v", err)
	}
	if recorder.Count("dropped:prune.failed") != 1 || recorder.Count("conflict:prune") != 0 {
		t.Fatalf("conflict accounting: %s", recorder.Events())
	}
}

// 两个时钟相差两小时的副本并发写同一频道、一个副本同时按年龄 Prune，之后客户端按 AfterSeq 翻页：单键 CAS 下序号唯一且单调、保留 /
// 淘汰计数守恒、年龄清理留下的页内洞与尾部洞都报 Gap、连续的页不报 Gap，返回的消息体与存储隔离。
// 默认 Memory；ROOST_BUGFIX5_BACKEND=redis 且 ROOST_REVIEW_REDIS 指向隔离 Redis 时状态在真实 Redis 上，键前缀 revn06:chat:<纳秒>（测试结束删除）。

func retentionStateStore(t *testing.T) StateStore {
	t.Helper()
	addr := os.Getenv("ROOST_REVIEW_REDIS")
	if os.Getenv("ROOST_BUGFIX5_BACKEND") != "redis" || addr == "" {
		return versionstore.NewMemoryStore[string, channelState]()
	}
	client, err := driver.NewClient(fredis.DefaultConfig(addr))
	if err != nil {
		t.Fatal(err)
	}
	prefix := fmt.Sprintf("revn06:chat:%d:", time.Now().UnixNano())
	t.Cleanup(func() {
		_ = client.Close()
		c := goredis.NewClient(&goredis.Options{Addr: addr})
		defer c.Close()
		ctx := context.Background()
		keys, err := c.Keys(ctx, prefix+"*").Result()
		if err != nil {
			t.Errorf("cleanup scan: %v", err)
			return
		}
		if len(keys) > 0 {
			if err := c.Del(ctx, keys...).Err(); err != nil {
				t.Errorf("cleanup delete: %v", err)
			}
		}
	})
	state, err := NewRedisStateStore(client, prefix)
	if err != nil {
		t.Fatal(err)
	}
	return state
}

func TestSkewedReplicasPruneAndPageWithHonestGaps(t *testing.T) {
	const (
		perWriter = 10
		writers   = 3 // per replica
		total     = 2 * writers * perWriter
	)
	state := retentionStateStore(t)
	registry := testRegistry(t)
	rule := ChannelRule{Kind: ChannelWorld, RequiresTarget: true, Scope: ScopeShared, Retain: total}
	fresh := newClock()
	skewed := &clock{now: fresh.Now().Add(-2 * time.Hour)}
	newReplica := func(c *clock) Store {
		store, err := NewStore(state, Config{Policy: allowAllPolicy{}, Bodies: registry, Rules: []ChannelRule{rule},
			RetentionAge: time.Hour, Now: c.Now, SystemNow: c.Now})
		if err != nil {
			t.Fatal(err)
		}
		return store
	}
	freshStore, skewedStore := newReplica(fresh), newReplica(skewed)
	ctx := context.Background()
	ref, err := freshStore.Resolve(world(), 0)
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	errs := make(chan error, total+1)
	stop := make(chan struct{})
	for w := 0; w < writers; w++ {
		for replica, store := range map[int64]Store{1: freshStore, 2: skewedStore} {
			wg.Add(1)
			go func(roleID int64, store Store) {
				defer wg.Done()
				for i := 0; i < perWriter; i++ {
					req := text(fmt.Sprintf("r%d-%d", roleID, i), fmt.Sprintf("k-%d-%d", roleID, i), world())
					if _, err := store.Append(ctx, role(roleID), req); err != nil {
						errs <- fmt.Errorf("append %s: %w", req.RequestID, err)
						return
					}
				}
			}(replica*100+int64(w), store)
		}
	}
	pruned := 0
	var pruneWG sync.WaitGroup
	pruneWG.Add(1)
	go func() {
		defer pruneWG.Done()
		for {
			n, err := freshStore.Prune(ctx, ref, MaxRetain)
			if err != nil && !isRetryableConflict(err) {
				errs <- fmt.Errorf("prune: %w", err)
				return
			}
			pruned += n
			// A retention loop runs on a period; a tight loop here would only
			// measure this test's own contention.
			select {
			case <-stop:
				return
			case <-time.After(time.Millisecond):
			}
		}
	}()
	wg.Wait()
	close(stop)
	pruneWG.Wait()
	n, err := freshStore.Prune(ctx, ref, MaxRetain)
	if err != nil {
		t.Fatal(err)
	}
	pruned += n
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}

	stats, err := freshStore.Stats(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	if stats.LatestSeq != total || stats.Retained != total/2 || stats.Evicted != total/2 || pruned != total/2 {
		t.Fatalf("conservation broken: %+v pruned=%d, want latest %d retained/evicted/pruned %d", stats, pruned, total, total/2)
	}

	// Walk forward from before the first sequence, seven at a time.
	seen := map[uint64]bool{}
	cursor := uint64(0)
	first := true
	for {
		query := HistoryQuery{Channel: world(), AfterSeq: cursor, Limit: 7}
		if first {
			// AfterSeq 0 means "latest page"; start the walk with a cursor
			// below every issued sequence by scrolling back to the oldest.
			query = HistoryQuery{Channel: world(), BeforeSeq: total + 1, Limit: MaxPageSize}
		}
		page, err := freshStore.History(ctx, role(9), query)
		if err != nil {
			t.Fatal(err)
		}
		if first {
			first = false
			if len(page.Messages) != total/2 {
				t.Fatalf("full scrollback returned %d messages, want %d", len(page.Messages), total/2)
			}
			cursor = 1 // seq 1 may or may not be retained; it is a valid cursor either way
			if page.Messages[0].Seq == 1 {
				seen[1] = true
			}
			continue
		}
		expectGap := false
		prev := cursor
		for _, m := range page.Messages {
			if m.From.RoleID < 100 || m.From.RoleID >= 200 {
				t.Fatalf("a skewed replica's expired message %d (role %d) survived the prune", m.Seq, m.From.RoleID)
			}
			if m.Seq <= prev || seen[m.Seq] {
				t.Fatalf("sequence %d out of order or repeated after %d", m.Seq, prev)
			}
			if m.Seq-prev > 1 {
				expectGap = true
			}
			seen[m.Seq] = true
			prev = m.Seq
		}
		if !page.HasMore && prev < page.LatestSeq {
			expectGap = true
		}
		if page.Gap != expectGap {
			t.Fatalf("after %d: Gap=%v, want %v (page %v, has_more %v, latest %d)", cursor, page.Gap, expectGap, seqs(page), page.HasMore, page.LatestSeq)
		}
		// Isolation: editing a returned body must not edit the stored one.
		if len(page.Messages) > 0 {
			page.Messages[0].Body[0] ^= 0xff
		}
		if !page.HasMore {
			break
		}
		cursor = page.NextCursor
	}
	if len(seen) != total/2 {
		t.Fatalf("the forward walk saw %d messages, want %d", len(seen), total/2)
	}
	again, err := freshStore.History(ctx, role(9), HistoryQuery{Channel: world(), BeforeSeq: total + 1, Limit: MaxPageSize})
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range again.Messages {
		if want := fmt.Sprintf("r%d-", m.From.RoleID); len(m.Body) < len(want) || string(m.Body[:len(want)]) != want {
			t.Fatalf("a caller's edit reached stored message %d: %q", m.Seq, m.Body)
		}
	}
	if !sort.SliceIsSorted(again.Messages, func(i, j int) bool { return again.Messages[i].Seq < again.Messages[j].Seq }) {
		t.Fatalf("scrollback is not in sequence order: %v", seqs(again))
	}
}

func isRetryableConflict(err error) bool {
	return err != nil && (errors.Is(err, versionstore.ErrConflict) || errors.Is(err, versionstore.ErrVersionMismatch))
}

func seqs(page Page) []uint64 {
	out := make([]uint64, 0, len(page.Messages))
	for _, m := range page.Messages {
		out = append(out, m.Seq)
	}
	return out
}

// pruneRecorder is a Store that only knows how to Resolve and Prune — the
// two calls the retention loop makes. Anything else is a bug in the loop and
// panics on the nil embedded interface.
type pruneRecorder struct {
	Store
	mu     sync.Mutex
	pruned []string
	seen   chan struct{}
}

func (p *pruneRecorder) Resolve(ch Channel, participant int64) (ChannelRef, error) {
	if ch.Kind == ChannelPrivate {
		return ChannelRef{}, fmt.Errorf("%w: kind %q needs a participant", ErrChannelInvalid, ch.Kind)
	}
	return ChannelRef{Kind: ch.Kind, key: fmt.Sprintf("%s:%d", ch.Kind, ch.Target)}, nil
}

func (p *pruneRecorder) Prune(_ context.Context, ref ChannelRef, limit int) (int, error) {
	p.mu.Lock()
	p.pruned = append(p.pruned, ref.Key())
	p.mu.Unlock()
	select {
	case p.seen <- struct{}{}:
	default:
	}
	return 1, nil
}

// The retention loop prunes what the deployment enumerated — and nothing when nothing was enumerated.
func TestTheRetentionLoopPrunesTheEnumeratedChannels(t *testing.T) {
	previous := pruneEvery
	pruneEvery = 5 * time.Millisecond
	t.Cleanup(func() { pruneEvery = previous })

	recorder := &pruneRecorder{seen: make(chan struct{}, 1)}
	world, _ := recorder.Resolve(Channel{Kind: ChannelWorld, Target: 1}, 0)
	service, err := NewService(ServiceConfig{Store: recorder, PruneChannels: func(context.Context) []ChannelRef {
		return []ChannelRef{world}
	}})
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{service: service}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.run(ctx) }()
	select {
	case <-recorder.seen:
	case <-time.After(5 * time.Second):
		t.Fatal("the retention loop never pruned the enumerated channel")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	if len(recorder.pruned) == 0 || recorder.pruned[0] != "world:1" {
		t.Fatalf("pruned %v, want world:1", recorder.pruned)
	}
}
