package mail

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

// U-0165 · C8 · RR-20260910-02：容量淘汰不得让"这封邮件已被领取"这件事消失。
//
// evict 认为终态条目可以整条丢掉,deliver 认为"条目不在就是从未投递",两条路径对同一状态
// 的判据不同。于是:领取邮件 A 拿到 token-1、提交;邮箱涨过 MaxMailboxEntries,A 作为终态被
// 整条删除;异步 fanout 的重复消息再次投递 A,A 变回未读且没有 token,ReserveClaim 铸出
// token-2 并返回同一个附件。发奖侧若按 Claim.Token 去重,就会再发一次。
//
// 展示保留与领取身份保留是两件事,上限也该各自独立:条目数受 MaxMailboxEntries 约束,
// 而"已领取过"要一直记着,直到墓碑自己的上限把它挤掉。
func TestEvictionKeepsTheClaimIdentityOfAClaimedMail(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	envelope := mustSend(t, h, withAttachment(directTo(1), "reward"))
	first, err := h.service.ReserveClaim(ctx, 1, envelope.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.service.CommitClaim(ctx, 1, envelope.ID, first.Token); err != nil {
		t.Fatal(err)
	}

	// Push the mailbox past its bound so the claimed entry is evicted.
	for i := 0; i < MaxMailboxEntries; i++ {
		if err := h.service.Deliver(ctx, 1, fmt.Sprintf("filler-%d", i), 0); err != nil {
			t.Fatal(err)
		}
	}
	stored, found, err := h.mailboxes.Get(ctx, 1)
	if err != nil || !found {
		t.Fatalf("mailbox missing: found=%v err=%v", found, err)
	}
	if _, exists := stored.Value.Entries[envelope.ID]; exists {
		t.Fatal("control: the claimed entry was not evicted, so this test proves nothing")
	}

	// Free evictable room, then let a duplicate fanout message redeliver it.
	if _, err := h.service.Delete(ctx, 1, "filler-0"); err != nil {
		t.Fatal(err)
	}
	if err := h.service.Deliver(ctx, 1, envelope.ID, 0); err != nil {
		t.Fatalf("redelivering a settled mail must stay idempotent, not error: %v", err)
	}

	// It must not be claimable again, and above all must not mint a second token.
	again, err := h.service.ReserveClaim(ctx, 1, envelope.ID, "")
	if err == nil {
		t.Fatalf("claimed mail resurrected with a new delivery key: first=%q replay=%q", first.Token, again.Token)
	}
	if !errors.Is(err, ErrAlreadyClaimed) {
		t.Fatalf("ReserveClaim on a settled mail = %v, want ErrAlreadyClaimed", err)
	}

	// The redelivery did not make it unread again either.
	stored, found, err = h.mailboxes.Get(ctx, 1)
	if err != nil || !found {
		t.Fatalf("mailbox missing after redelivery: found=%v err=%v", found, err)
	}
	if entry, exists := stored.Value.Entries[envelope.ID]; exists && entry.Status == StatusUnread {
		t.Fatalf("a settled mail came back as unread: %+v", entry)
	}
}

// A mail that was never claimed is a different case: dropping it is what the
// retention bound is for, and a later redelivery legitimately shows it again.
func TestEvictionStillForgetsAMailThatWasNeverClaimed(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	envelope := mustSend(t, h, directTo(1))
	if _, err := h.service.MarkRead(ctx, 1, envelope.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.service.Delete(ctx, 1, envelope.ID); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < MaxMailboxEntries; i++ {
		if err := h.service.Deliver(ctx, 1, fmt.Sprintf("filler-%d", i), 0); err != nil {
			t.Fatal(err)
		}
	}
	stored, _, err := h.mailboxes.Get(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := stored.Value.Entries[envelope.ID]; exists {
		t.Skip("the deleted entry was not evicted; nothing to assert")
	}
	if _, err := h.service.Delete(ctx, 1, "filler-0"); err != nil {
		t.Fatal(err)
	}
	if err := h.service.Deliver(ctx, 1, envelope.ID, 0); err != nil {
		t.Fatal(err)
	}
	stored, _, err = h.mailboxes.Get(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	entry, exists := stored.Value.Entries[envelope.ID]
	if !exists || entry.Status != StatusUnread {
		t.Fatalf("an unclaimed mail must be deliverable again as unread, got exists=%v entry=%+v", exists, entry)
	}
}

// A late retry of the commit that already succeeded is still answerable after
// the entry was evicted, because the tombstone kept the token. A retry with
// the wrong token is still refused without saying what the right one is.
func TestCommitClaimReplaysAfterTheEntryWasEvicted(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	envelope := mustSend(t, h, withAttachment(directTo(1), "reward"))
	claim, err := h.service.ReserveClaim(ctx, 1, envelope.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.service.CommitClaim(ctx, 1, envelope.ID, claim.Token); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < MaxMailboxEntries; i++ {
		if err := h.service.Deliver(ctx, 1, fmt.Sprintf("filler-%d", i), 0); err != nil {
			t.Fatal(err)
		}
	}
	stored, _, err := h.mailboxes.Get(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := stored.Value.Entries[envelope.ID]; exists {
		t.Fatal("control: the claimed entry was not evicted")
	}

	replayed, err := h.service.CommitClaim(ctx, 1, envelope.ID, claim.Token)
	if err != nil {
		t.Fatalf("a late commit retry with the original token = %v, want the replay", err)
	}
	if replayed.Status != StatusClaimed {
		t.Fatalf("replayed commit status = %v, want claimed", replayed.Status)
	}
	if _, err := h.service.CommitClaim(ctx, 1, envelope.ID, "some-other-token"); !errors.Is(err, ErrClaimTokenWrong) {
		t.Fatalf("commit with a wrong token after eviction = %v, want ErrClaimTokenWrong", err)
	}
}

// Retention drops a settled claim when the envelope it protects can no longer
// be claimed. U-0165 bounded these by count instead; RR-20260911-01 showed a
// count cannot express a time window, so the count now only bounds records
// written before the window was recorded.
func TestSettledClaimsAgeOutWithTheirEnvelope(t *testing.T) {
	var box Mailbox
	box.init(1)
	const now int64 = 1_000_000
	box.SettledClaims["expired"] = SettledClaim{Token: "t1", SettledAtUnix: now - 100, EnvelopeExpiresAtUnix: now - 1}
	box.SettledClaims["claimable"] = SettledClaim{Token: "t2", SettledAtUnix: now - 5000, EnvelopeExpiresAtUnix: now + 1}
	box.evictSettledClaims(now)
	if _, ok := box.SettledClaims["expired"]; ok {
		t.Error("a record whose envelope can no longer be claimed was kept")
	}
	if _, ok := box.SettledClaims["claimable"]; !ok {
		t.Error("a record was dropped while its envelope was still claimable, even though it was the oldest")
	}

	// Records with no recorded window are the only ones the count bound may
	// drop, and only when the mailbox is over it.
	var legacy Mailbox
	legacy.init(1)
	for i := 0; i < MaxSettledClaims+50; i++ {
		legacy.SettledClaims[fmt.Sprintf("mail-%04d", i)] = SettledClaim{
			Token: fmt.Sprintf("token-%d", i), SettledAtUnix: int64(i),
		}
	}
	legacy.SettledClaims["known-window"] = SettledClaim{Token: "keep", SettledAtUnix: 0, EnvelopeExpiresAtUnix: now + 1}
	legacy.evictSettledClaims(now)
	if len(legacy.SettledClaims) != MaxSettledClaims {
		t.Fatalf("settled claims = %d, want the bound %d", len(legacy.SettledClaims), MaxSettledClaims)
	}
	if _, ok := legacy.SettledClaims["known-window"]; !ok {
		t.Error("the count bound dropped a record that knows its window")
	}
	if _, ok := legacy.SettledClaims["mail-0000"]; ok {
		t.Error("the oldest window-less record survived the bound")
	}
}
