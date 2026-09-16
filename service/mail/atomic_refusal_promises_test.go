package mail

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// U-0182 · C2 · RR-20260911-05:被拒绝的投递不能留下任何痕迹。
//
// Update 回调拿到的 Mailbox 是结构体浅拷贝,Entries / SettledClaims 两张 map 仍指向存储对象。
// Deliver 先 insert、再 evict,最后才做容量拒绝:回调返回 save=false 加错误,MemoryStore 不保存
// 新结构体,但 map 上的修改已经发生 —— Unread 与 Version 维持旧值,条目和墓碑却多了一个,
// "拒绝"与实际保存状态矛盾。match 的 Update 回调第一行就是 current = current.clone(),
// mail 漏了这一行。
func TestRefusedDeliveryLeavesTheMailboxUntouched(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	mailID := settleOneClaim(t, h)

	// Drive the settled claims to the bound so the next settling delivery is
	// refused with ErrClaimHistoryFull.
	refused := false
	for i := 0; i < MaxSettledClaims+5 && !refused; i++ {
		if err := deleteOneUnread(h, mailID); err != nil {
			t.Fatalf("free a slot before churn %d: %v", i, err)
		}
		h.clock.advance(time.Second)
		req := withAttachment(directTo(1), "reward")
		req.RequestID = fmt.Sprintf("atomic-%d", i)
		other, err := h.service.Send(ctx, req)
		if errors.Is(err, ErrClaimHistoryFull) {
			refused = true
			break
		}
		if err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
		claim, err := h.service.ReserveClaim(ctx, 1, other.ID, "")
		if err != nil {
			t.Fatalf("reserve %d: %v", i, err)
		}
		if _, err := h.service.CommitClaim(ctx, 1, other.ID, claim.Token); err != nil {
			t.Fatalf("commit %d: %v", i, err)
		}
		if err := evictUntilSettled(h, other.ID); errors.Is(err, ErrClaimHistoryFull) {
			refused = true
		} else if err != nil {
			t.Fatalf("settle %d: %v", i, err)
		}
	}
	if !refused {
		t.Fatal("control: never reached the claim-history bound")
	}

	before, _, err := h.mailboxes.Get(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	// One more delivery that would have to evict a claimed entry and settle a
	// new claim: it must be refused, and refusing must change nothing.
	h.clock.advance(time.Second)
	err = h.service.Deliver(ctx, 1, "one-too-many", 0)
	if !errors.Is(err, ErrClaimHistoryFull) && !errors.Is(err, ErrMailboxFull) {
		t.Fatalf("control: the extra delivery was not refused: %v", err)
	}
	after, _, err := h.mailboxes.Get(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, inserted := after.Value.Entries["one-too-many"]; inserted {
		t.Error("the refused delivery's entry is in the stored mailbox")
	}
	if len(after.Value.Entries) != len(before.Value.Entries) {
		t.Errorf("entries %d -> %d across a refused delivery", len(before.Value.Entries), len(after.Value.Entries))
	}
	if len(after.Value.SettledClaims) != len(before.Value.SettledClaims) {
		t.Errorf("settled claims %d -> %d across a refused delivery", len(before.Value.SettledClaims), len(after.Value.SettledClaims))
	}
	if after.Version != before.Version {
		t.Errorf("version %d -> %d across a refused delivery", before.Version, after.Version)
	}
	unread := int32(0)
	for _, entry := range after.Value.Entries {
		if entry.Status == StatusUnread {
			unread++
		}
	}
	if unread != after.Value.Unread {
		t.Errorf("Unread field %d disagrees with %d actual unread entries", after.Value.Unread, unread)
	}
}
