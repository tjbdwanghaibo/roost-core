package chat

import (
	"context"
	"errors"
	"fmt"
	fredis "github.com/tjbdwanghaibo/roost-core/redis"
	"github.com/tjbdwanghaibo/roost-core/redis/driver"
	"os"
	"testing"
	"time"
)

func bugfix5Chat(t *testing.T) *harness {
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

func TestBugfix5AgePruneHandlesInteriorAndTailGaps(t *testing.T) {
	h := bugfix5Chat(t)
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

func TestBugfix5GapOnlyReportsTheRangeActuallyPaged(t *testing.T) {
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
