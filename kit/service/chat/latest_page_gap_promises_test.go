package chat

// RR-20261001-08：Page.Gap 只报告真正的洞。承诺：无游标取最新页、或 BeforeSeq 落在
// 保留窗口内向前翻到保留边缘，只是正常容量淘汰（ring 头部之前的序号被按容量丢掉）
// 时 Gap=false，history.gap.<kind> 指标不计；游标落在已淘汰区间（AfterSeq 早于
// 最旧保留序号、BeforeSeq 要的前一条已不在）或页内序号不连续（页内 / 尾部洞）时
// 才 Gap=true 并计一次指标。旧行为：pageOf 把"页到达 ring 头部且头部序号 > 1"也当
// 洞，任何曾按容量淘汰过的频道，客户端每拉一次完整最新页都 Gap=true、指标每次 +1。

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"testing"
	"time"

	fredis "github.com/tjbdwanghaibo/roost-core/redis"
	"github.com/tjbdwanghaibo/roost-core/redis/driver"
)

// rr08Chat builds a world channel retaining four messages, on Memory by
// default and on the isolated Redis under the same gate as the RR-27 tests
// (ROOST_BUGFIX5_BACKEND=redis + ROOST_REVIEW_REDIS), so one Redis run covers
// both retention-gap promises.
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
