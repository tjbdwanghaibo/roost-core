package mail

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// U-0170 · C2 · RR-20260911-02：Mailbox 的读取副本必须是真副本。
//
// clone 只复制了 Entries,`out := m` 让 U-0165 新增的 SettledClaims 继续和存储里的 map 共享
// 底层数组。于是拿 Service.Mailbox 读出来的快照删掉一个墓碑,存储里的领取记录也没了 ——
// 绕过 Store.Update 改到了权威状态。
func TestMailboxSnapshotDoesNotShareTheSettledClaims(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	mailID := settleOneClaim(t, h)

	snapshot, found, err := h.service.Mailbox(ctx, 1)
	if err != nil || !found {
		t.Fatalf("mailbox: found=%v err=%v", found, err)
	}
	if _, ok := snapshot.SettledClaims[mailID]; !ok {
		t.Fatalf("control: the snapshot has no settled claim for %s", mailID)
	}
	delete(snapshot.SettledClaims, mailID)

	again, found, err := h.service.Mailbox(ctx, 1)
	if err != nil || !found {
		t.Fatalf("mailbox again: found=%v err=%v", found, err)
	}
	if _, ok := again.SettledClaims[mailID]; !ok {
		t.Fatal("mutating the returned mailbox deleted the stored settled claim")
	}
	// And the record still does its job.
	if err := h.service.Deliver(ctx, 1, mailID, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := h.service.ReserveClaim(ctx, 1, mailID, ""); !errors.Is(err, ErrAlreadyClaimed) {
		t.Fatalf("ReserveClaim after mutating a snapshot = %v, want ErrAlreadyClaimed", err)
	}
}

// U-0171 · C8 · RR-20260911-01：结算身份的保留期限必须盖住信封还能被领取的那段时间。
//
// U-0165 用条数给墓碑收界,并论证"ReserveClaim 本来就拒绝过期信封,所以墓碑只需活得比信封长"。
// 这个论证是错的:条数上限保证不了时间期限。信封还有一周有效期,只要再有 MaxSettledClaims 条
// 更新的已领取邮件被淘汰,A 的墓碑就成了最旧项被挤掉,A 重投后又能领出一个新 token。
//
// 所以墓碑按信封的可领取窗口过期,而不是按条数;窗口内挤不下时明确拒绝投递,而不是悄悄忘掉
// 一条领取身份 —— 这和 full() 对"腾不出位置"的处理是同一个态度。
func TestSettledClaimsOutliveTheEnvelopeTheyProtect(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	mailID := settleOneClaim(t, h)

	// Churn past the old count bound while the original envelope is nowhere
	// near expiry. Two outcomes are correct: the identity is kept, or the
	// mailbox refuses the delivery because it cannot keep one more. What must
	// never happen is quietly forgetting one.
	refusedAt := -1
	rounds := 0
	for i := 0; i < MaxSettledClaims+2; i++ {
		if err := deleteOneUnread(h, mailID); err != nil {
			t.Fatalf("free a slot before churn %d: %v", i, err)
		}
		h.clock.advance(time.Second)
		req := withAttachment(directTo(1), "reward")
		req.RequestID = fmt.Sprintf("churn-%d", i)
		other, err := h.service.Send(ctx, req)
		if errors.Is(err, ErrClaimHistoryFull) {
			refusedAt = i
			break
		}
		if err != nil {
			t.Fatalf("send churn %d: %v", i, err)
		}
		claim, err := h.service.ReserveClaim(ctx, 1, other.ID, "")
		if err != nil {
			t.Fatalf("reserve %d: %v", i, err)
		}
		if _, err := h.service.CommitClaim(ctx, 1, other.ID, claim.Token); err != nil {
			t.Fatalf("commit %d: %v", i, err)
		}
		if err := evictUntilSettled(h, other.ID); err != nil {
			if errors.Is(err, ErrClaimHistoryFull) {
				refusedAt = i
				break
			}
			t.Fatalf("settle %d: %v", i, err)
		}
		rounds = i + 1
	}
	if rounds <= 1 && refusedAt < 0 {
		t.Fatalf("control: the churn did nothing (rounds=%d)", rounds)
	}
	h.clock.advance(400 * time.Second)

	// Whatever happened above, the original envelope has not expired, so its
	// claim identity must still refuse a second token.
	stored, _, err := h.mailboxes.Get(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := stored.Value.SettledClaims[mailID]; !ok {
		t.Fatalf("the settled claim was dropped while its envelope was still claimable (churn rounds=%d, refused at %d)", rounds, refusedAt)
	}
	if err := h.service.Deliver(ctx, 1, mailID, 0); err != nil && !errors.Is(err, ErrMailboxFull) && !errors.Is(err, ErrClaimHistoryFull) {
		t.Fatal(err)
	}
	again, err := h.service.ReserveClaim(ctx, 1, mailID, "")
	if err == nil {
		t.Fatalf("a second token %q was minted for a mail that was already claimed", again.Token)
	}
	if !errors.Is(err, ErrAlreadyClaimed) {
		t.Fatalf("ReserveClaim = %v, want ErrAlreadyClaimed", err)
	}
}

// Once the envelope can no longer be claimed, the tombstone has nothing left
// to protect and retention may drop it.
func TestSettledClaimsAreDroppedOnceTheEnvelopeExpires(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	mailID := settleOneClaim(t, h)

	stored, _, err := h.mailboxes.Get(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	settled, ok := stored.Value.SettledClaims[mailID]
	if !ok {
		t.Fatal("control: no settled claim to expire")
	}
	if settled.EnvelopeExpiresAtUnix <= h.clock.Now().Unix() {
		t.Fatalf("control: the envelope is already expired at %d", settled.EnvelopeExpiresAtUnix)
	}

	h.clock.advance(time.Duration(settled.EnvelopeExpiresAtUnix-h.clock.Now().Unix()+1) * time.Second)
	// A delivery runs retention, which is when expired tombstones go. Free an
	// evictable slot first so the delivery is not refused by the entry bound.
	for id, entry := range stored.Value.Entries {
		if entry.Status != StatusDeleted {
			if _, err := h.service.Delete(ctx, 1, id); err != nil {
				t.Fatal(err)
			}
			break
		}
	}
	if err := h.service.Deliver(ctx, 1, "after-expiry", 0); err != nil {
		t.Fatal(err)
	}
	stored, _, err = h.mailboxes.Get(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := stored.Value.SettledClaims[mailID]; ok {
		t.Fatal("a tombstone whose envelope can no longer be claimed was kept forever")
	}
}

// settleOneClaim delivers an attachment mail, claims it, and evicts its entry
// so only the settled-claim record remains. It returns the mail id.
func settleOneClaim(t *testing.T, h *harness) string {
	t.Helper()
	ctx := context.Background()
	envelope := mustSend(t, h, withAttachment(directTo(1), "reward"))
	claim, err := h.service.ReserveClaim(ctx, 1, envelope.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.service.CommitClaim(ctx, 1, envelope.ID, claim.Token); err != nil {
		t.Fatal(err)
	}
	if err := evictUntilSettled(h, envelope.ID); err != nil {
		t.Fatal(err)
	}
	return envelope.ID
}

// evictUntilSettled fills the mailbox until the named entry is evicted and
// only its settled-claim record remains.
//
// Filler mails arrive unread and unread mail is never evicted, so the claimed
// entry is the only evictable one and goes first. When the mailbox is so full
// that a filler cannot even be delivered, one unread filler is deleted to make
// a slot; a freshly deleted entry is NEWER than the claimed one, so the target
// still leaves first.
func evictUntilSettled(h *harness, mailID string) error {
	ctx := context.Background()
	for i := 0; i < MaxMailboxEntries*4; i++ {
		h.clock.advance(time.Second)
		err := h.service.Deliver(ctx, 1, fmt.Sprintf("filler-%s-%d", mailID, i), 0)
		if errors.Is(err, ErrClaimHistoryFull) {
			return err
		}
		if errors.Is(err, ErrMailboxFull) {
			if freeErr := deleteOneUnread(h, mailID); freeErr != nil {
				return freeErr
			}
			continue
		}
		if err != nil {
			return err
		}
		stored, found, err := h.mailboxes.Get(ctx, 1)
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("mailbox vanished")
		}
		if _, exists := stored.Value.Entries[mailID]; !exists {
			return nil
		}
	}
	return fmt.Errorf("entry %s was never evicted", mailID)
}

func deleteOneUnread(h *harness, except string) error {
	ctx := context.Background()
	stored, found, err := h.mailboxes.Get(ctx, 1)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("mailbox vanished")
	}
	for id, entry := range stored.Value.Entries {
		if id == except || entry.Status == StatusDeleted || entry.Status == StatusClaimed {
			continue
		}
		_, err := h.service.Delete(ctx, 1, id)
		return err
	}
	return fmt.Errorf("no unread entry left to delete")
}
