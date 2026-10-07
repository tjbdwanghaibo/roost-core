package chat

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// D-L3 第八轮（维护者决定）：展示给玩家的消息时间（SentAtUnix）读业务时钟（Config.Now）；
// 保留期清理（StoredAtUnix 与 Prune 的截止线）读系统时钟（Config.SystemNow）。
// 业务时钟比系统时钟快一天时，展示时间前移一天，保留期仍按真实年龄算。
func TestDisplayTimeIsBusinessTimeAndRetentionIsSystemTime(t *testing.T) {
	ctx := context.Background()
	system := newClock()
	business := &clock{now: system.Now().Add(24 * time.Hour)}
	h := newHarness(t, withRetentionAge(time.Hour), func(cfg *Config, _ *ServiceConfig, _ *harness) {
		cfg.Now, cfg.SystemNow = business.Now, system.Now
	})

	message := mustPublish(t, h, role(1), text("hello", "r1", world()))
	if message.SentAtUnix != business.Now().Unix() {
		t.Fatalf("SentAtUnix = %d, want the business clock %d", message.SentAtUnix, business.Now().Unix())
	}
	if message.StoredAtUnix != system.Now().Unix() {
		t.Fatalf("StoredAtUnix = %d, want the system clock %d", message.StoredAtUnix, system.Now().Unix())
	}
	ref, err := h.store.Resolve(world(), 0)
	if err != nil {
		t.Fatal(err)
	}
	// 业务时钟再前拨一周：保留期 1h 按真实年龄算，刚存的消息不清理。
	business.advance(7 * 24 * time.Hour)
	if pruned, err := h.service.Prune(ctx, ref, 10); err != nil || pruned != 0 {
		t.Fatalf("Prune after only the business clock moved = %d, %v; want 0: retention is real age", pruned, err)
	}
	// 真实时间过了保留期才清理。
	system.advance(2 * time.Hour)
	if pruned, err := h.service.Prune(ctx, ref, 10); err != nil || pruned != 1 {
		t.Fatalf("Prune after the system clock passed the retention age = %d, %v; want 1", pruned, err)
	}
}

// 持久格式只增不改：SentAtUnix 出现之前存下的消息没有这个字段，读出时用 StoredAtUnix 兜底
// （那时偏移为 0，两个钟一致），Publish 的重放应答与 History 都一样。存着的消息不改写。
func TestAMessageStoredBeforeSentAtUnixShowsItsStoredTime(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	ref, err := h.store.Resolve(world(), 0)
	if err != nil {
		t.Fatal(err)
	}
	const storedAt = 1_699_000_000
	missingTimestamp := []byte(`{"last_seq":1,"ring":[{"seq":1,"channel":{"kind":"world","target":7},` +
		`"from":{"role_id":1,"name":"role-1"},"origin":"role","type":"text","body":"aGk=",` +
		`"request_id":"old","stored_at_unix":1699000000}],"requests":{"old":{"sequence":1,"origin":"role","role_id":1}}}`)
	var state channelState
	if err := json.Unmarshal(missingTimestamp, &state); err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.state.Update(ctx, ref.key, func(channelState, bool) (channelState, bool, error) {
		return state, true, nil
	}); err != nil {
		t.Fatal(err)
	}

	page, err := h.service.History(ctx, role(2), HistoryQuery{Channel: world(), Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Messages) != 1 || page.Messages[0].SentAtUnix != storedAt {
		t.Fatalf("History of a missingTimestamp message = %+v, want SentAtUnix filled from StoredAtUnix %d", page.Messages, storedAt)
	}
	replay := mustPublish(t, h, role(1), text("hi", "old", world()))
	if replay.Seq != 1 || replay.SentAtUnix != storedAt {
		t.Fatalf("replay of a missingTimestamp message = seq %d SentAtUnix %d, want seq 1 SentAtUnix %d", replay.Seq, replay.SentAtUnix, storedAt)
	}
	stored, _, err := h.state.Get(ctx, ref.key)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Value.Ring[0].SentAtUnix != 0 {
		t.Fatalf("the stored missingTimestamp message was rewritten with SentAtUnix %d; the fallback is a read-side view", stored.Value.Ring[0].SentAtUnix)
	}
}
