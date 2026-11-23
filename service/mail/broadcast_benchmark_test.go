package mail

import (
	"fmt"
	"testing"
)

func benchBroadcastID(k int) string { return fmt.Sprintf("bench-broadcast-%06d", k) }

// 第 k 封固定在 base+k+1 到期；第 k+capacity 封恰在这一秒发布并取代它。
func benchBroadcast(k int, base int64) Envelope {
	createdAt := base + int64(max(0, k-DefaultMaxActiveBroadcasts+1))
	expiresAt := base + int64(k) + 1
	return Envelope{ID: benchBroadcastID(k), Audience: AudienceBroadcast, Scope: ServerScope(int32(1 + k%4)),
		Subject: "maintenance compensation", CreatedAtUnix: createdAt, ExpiresAtUnix: expiresAt}
}

func TestBroadcastLedgerAtCapacityPublishFixture(t *testing.T) {
	const capacity = DefaultMaxActiveBroadcasts
	index := NewMemoryBroadcastIndex()
	ctx := t.Context()
	// 预填后再完整换两批，跨过首次回收替换邮件的边界。
	for k := range 3 * capacity {
		mail := benchBroadcast(k, 1_900_000_000)
		if _, err := index.Publish(ctx, mail, mail.CreatedAtUnix, capacity); err != nil {
			t.Fatalf("publish k=%d: %v", k, err)
		}
		if k < capacity-1 {
			continue
		}
		ledger, err := index.Snapshot(ctx)
		if err != nil {
			t.Fatalf("snapshot k=%d: %v", k, err)
		}
		if len(ledger.Mails) != capacity || ledger.Head != uint64(k+1) {
			t.Fatalf("k=%d: got head=%d, size=%d; want head=%d, size=%d", k, ledger.Head, len(ledger.Mails), k+1, capacity)
		}
		if got, want := ledger.Mails[0].MailID, benchBroadcastID(k-capacity+1); got != want {
			t.Fatalf("k=%d: oldest mail=%s, want %s", k, got, want)
		}
	}
}
