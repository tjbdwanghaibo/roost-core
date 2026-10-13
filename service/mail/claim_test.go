// 邮件领取：预留 / 提交 / 取消的令牌与代际、淘汰保留领取身份与删除墓碑、结算记录随信封窗口过期、读取副本不共享墓碑。

package mail

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"testing"
	"time"

	fredis "github.com/tjbdwanghaibo/roost-core/infra/storage/redis"
	"github.com/tjbdwanghaibo/roost-core/infra/storage/redis/driver"
)

func TestReserveClaimReportsWhenTheMailStopsBeingClaimable(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	envelope := mustSend(t, h, withAttachment(directTo(1), "reward"))
	claim, err := h.service.ReserveClaim(ctx, 1, envelope.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if claim.ExpiresAtUnix != envelope.ExpiresAtUnix {
		t.Fatalf("the reservation reports expiry %d, want the envelope's %d — a caller that has to remember this claim until it can no longer happen has no other way to know when that is",
			claim.ExpiresAtUnix, envelope.ExpiresAtUnix)
	}
	if claim.ExpiresAtUnix <= 0 {
		t.Fatal("the envelope has no expiry, so nothing can bound a dedupe ledger against it")
	}
	// The lease deadline is a different, much shorter thing: it says when
	// somebody else may re-reserve, not when the mail stops being claimable.
	if claim.DeadlineUnix >= claim.ExpiresAtUnix {
		t.Fatalf("the lease deadline %d is not shorter than the expiry %d; the two would be confusable", claim.DeadlineUnix, claim.ExpiresAtUnix)
	}
}

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
		if err := deliverMailboxFixture(h, ctx, 1, fmt.Sprintf("filler-%d", i), 0); err != nil {
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

// Deletion remains authoritative after display retention, even without a token.
func TestEvictionPreservesUnclaimedDeletion(t *testing.T) {
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
		if err := deliverMailboxFixture(h, ctx, 1, fmt.Sprintf("filler-%d", i), 0); err != nil {
			t.Fatal(err)
		}
	}
	stored, _, err := h.mailboxes.Get(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := stored.Value.Entries[envelope.ID]; exists {
		t.Fatal("the deleted entry was not evicted")
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
	if exists || !stored.Value.SettledClaims[envelope.ID].Deleted {
		t.Fatalf("deleted identity was lost: exists=%v entry=%+v", exists, entry)
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
		if err := deliverMailboxFixture(h, ctx, 1, fmt.Sprintf("filler-%d", i), 0); err != nil {
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

// Retention drops a settled claim when the envelope it protects can no longer be claimed, and never by count: a record
// without a window is kept and makes delivery refuse rather than being guessed away.
// 历史：此前按条数淘汰无窗口的旧记录；维护者 2026-10-13 决定不兼容旧数据。
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

	// A record without a window (never written by this package: every envelope
	// expires) is not dropped by count, however old: retention keeps it, and
	// settledClaimsOverflow then makes delivery refuse with ErrClaimHistoryFull.
	var windowless Mailbox
	windowless.init(1)
	for i := 0; i < MaxSettledClaims+50; i++ {
		windowless.SettledClaims[fmt.Sprintf("mail-%04d", i)] = SettledClaim{
			Token: fmt.Sprintf("token-%d", i), SettledAtUnix: int64(i),
		}
	}
	windowless.evictSettledClaims(now)
	if len(windowless.SettledClaims) != MaxSettledClaims+50 || !windowless.settledClaimsOverflow() {
		t.Fatalf("window-less records were dropped by count: %d left, overflow=%v", len(windowless.SettledClaims), windowless.settledClaimsOverflow())
	}
}

func claimHarness(t *testing.T) *harness {
	t.Helper()
	if os.Getenv("ROOST_REVIEW3_BACKEND") != "redis" {
		return newHarness(t)
	}
	addr := os.Getenv("ROOST_REVIEW_REDIS")
	if addr == "" {
		t.Fatal("Redis backend requires ROOST_REVIEW_REDIS")
	}
	client, err := driver.NewClient(fredis.DefaultConfig(addr))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	return newHarness(t, func(c *Config) {
		stores, e := NewRedisStores(client, RedisConfig{Prefix: fmt.Sprintf("mail-review3:%d", time.Now().UnixNano()), SendTTL: time.Hour, Now: c.Now})
		if e != nil {
			t.Fatal(e)
		}
		c.Envelopes = stores.Envelopes
		c.Mailboxes = stores.Mailboxes
		c.Sends = stores.Sends
	})
}
func TestOldCancelCannotReleaseNewClaimAttempt(t *testing.T) {
	h := claimHarness(t)
	ctx := context.Background()
	envelope := mustSend(t, h, withAttachment(directTo(1), "reward"))
	a, e := h.service.ReserveClaim(ctx, 1, envelope.ID, "")
	if e != nil {
		t.Fatal(e)
	}
	h.clock.advance(31 * time.Second)
	b, e := h.service.ReserveClaim(ctx, 1, envelope.ID, "")
	if e != nil {
		t.Fatal(e)
	}
	if a.Token != b.Token || b.Attempts != 2 {
		t.Fatal("fixture lost stable delivery identity", a, b)
	}
	cancelled, e := h.service.CancelClaim(ctx, 1, envelope.ID, a.Token, a.Attempts)
	if e != nil {
		t.Fatal(e)
	}
	c, thirdErr := h.service.ReserveClaim(ctx, 1, envelope.ID, "")
	if cancelled || !errors.Is(thirdErr, ErrClaimHeld) {
		t.Fatalf("old A cancel released B: cancelled=%v Bdeadline=%d C=%+v err=%v", cancelled, b.DeadlineUnix, c, thirdErr)
	}
}
func TestObserveExpiredCancelledClaimsRetainCapacity(t *testing.T) {
	h := claimHarness(t)
	ctx := context.Background()
	firstID := ""
	for i := 0; i < MaxMailboxEntries; i++ {
		req := withAttachment(directTo(1), "reward")
		req.RequestID = fmt.Sprintf("old-%d", i)
		req.ExpiresInSeconds = 60
		envelope := mustSend(t, h, req)
		if firstID == "" {
			firstID = envelope.ID
		}
		claim, e := h.service.ReserveClaim(ctx, 1, envelope.ID, "")
		if e != nil {
			t.Fatal(e)
		}
		if _, e = h.service.CancelClaim(ctx, 1, envelope.ID, claim.Token, claim.Attempts); e != nil {
			t.Fatal(e)
		}
	}
	h.clock.advance(61 * time.Second)
	req := directTo(1)
	req.RequestID = "new"
	_, e := h.service.Send(ctx, req)
	if !errors.Is(e, ErrMailboxFull) {
		t.Fatalf("observation changed: expected retained pending proof, err=%v", e)
	}
	box, found, e := h.service.Mailbox(ctx, 1)
	if e != nil || !found || len(box.Entries) != MaxMailboxEntries || box.Entries[firstID].ClaimDeadlineUnix != 0 {
		t.Fatal(box, found, e)
	}
	// Cancel alone does not certify that the external attempt never granted.
	// Explicit disposal is a business decision, not automatic expiration.
	if _, e = h.service.Delete(ctx, 1, firstID); e != nil {
		t.Fatal(e)
	}
	if _, e = h.service.Send(ctx, req); e != nil {
		t.Fatalf("explicit disposal cannot restore capacity: %v", e)
	}
	t.Log("observation: expired cancelled claims pin capacity; explicit Delete and same-request Send replay recover")
}

func TestCancelClaimGenerationCrossesLocalAndBusTransports(t *testing.T) {
	for _, transport := range []string{"local", "bus"} {
		t.Run(transport, func(t *testing.T) {
			mail, clock := newTransport(t, transport)
			ctx := context.Background()
			envelope, err := mail.Send(ctx, withAttachment(directTo(7), "reward"))
			if err != nil {
				t.Fatal(err)
			}
			a, err := mail.ReserveClaim(ctx, 7, envelope.ID, "")
			if err != nil {
				t.Fatal(err)
			}
			clock.advance(31 * time.Second)
			b, err := mail.ReserveClaim(ctx, 7, envelope.ID, "")
			if err != nil {
				t.Fatal(err)
			}
			if released, err := mail.CancelClaim(ctx, 7, envelope.ID, a.Token, a.Attempts); err != nil || released {
				t.Fatalf("old lease cancelled new one: %v %v", released, err)
			}
			if _, err := mail.ReserveClaim(ctx, 7, envelope.ID, ""); !errors.Is(err, ErrClaimHeld) {
				t.Fatal(err)
			}
			if _, err := mail.CancelClaim(ctx, 7, envelope.ID, b.Token, 0); !errors.Is(err, ErrRequestInvalid) {
				t.Fatalf("legacy attempt omitted: %v", err)
			}
			if released, err := mail.CancelClaim(ctx, 7, envelope.ID, b.Token, b.Attempts); err != nil || !released {
				t.Fatalf("current lease cannot cancel: %v %v", released, err)
			}
			if released, err := mail.CancelClaim(ctx, 7, envelope.ID, b.Token, b.Attempts); err != nil || released {
				t.Fatalf("repeated cancel not idempotent: %v %v", released, err)
			}
			c, err := mail.ReserveClaim(ctx, 7, envelope.ID, "")
			if err != nil || c.Token != a.Token || c.Attempts != 3 {
				t.Fatalf("delivery identity changed: %+v %v", c, err)
			}
			// An older successful grant can still confirm under its stable token.
			if _, err := mail.CommitClaim(ctx, 7, envelope.ID, a.Token); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestCancelClaimLegacyWireIsRejected(t *testing.T) {
	h := newHarness(t)
	fake := newFakeBus()
	if err := RegisterHandlers(fake, h.service); err != nil {
		t.Fatal(err)
	}
	envelope := mustSend(t, h, withAttachment(directTo(7), "reward"))
	claim, err := h.service.ReserveClaim(context.Background(), 7, envelope.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	handler, _ := fake.handler(MethodCancelClaim)
	// Old JSON has no attempts field; it decodes as zero and must fail closed.
	response, err := invoke(t, handler, MethodCancelClaim, map[string]any{"player_id": int64(7), "mail_id": envelope.ID, "token": claim.Token})
	if err != nil {
		t.Fatal(err)
	}
	wire, ok := response.(rpcCancelClaimResponse)
	if !ok || wire.Code != CodeRequestInvalid || wire.Released {
		t.Fatalf("old wire cancelled a lease: %+v", response)
	}
	if _, err := h.service.ReserveClaim(context.Background(), 7, envelope.ID, ""); !errors.Is(err, ErrClaimHeld) {
		t.Fatal(err)
	}
}
func TestClaimAttemptGenerationNeverWraps(t *testing.T) {
	for _, attempts := range []int32{-1, math.MaxInt32} {
		t.Run(fmt.Sprint(attempts), func(t *testing.T) {
			h := newHarness(t)
			ctx := context.Background()
			envelope := mustSend(t, h, withAttachment(directTo(1), "reward"))
			claim, err := h.service.ReserveClaim(ctx, 1, envelope.ID, "")
			if err != nil {
				t.Fatal(err)
			}
			h.clock.advance(31 * time.Second)
			if _, _, err = h.mailboxes.Update(ctx, 1, func(box Mailbox, found bool) (Mailbox, bool, error) {
				box = box.clone()
				entry := box.Entries[envelope.ID]
				entry.ClaimAttempts = attempts
				box.Entries[envelope.ID] = entry
				return box, true, nil
			}); err != nil {
				t.Fatal(err)
			}
			before, _, err := h.mailboxes.Get(ctx, 1)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = h.service.ReserveClaim(ctx, 1, envelope.ID, ""); !errors.Is(err, ErrRequestInvalid) {
				t.Fatalf("wrapped generation: %v", err)
			}
			after, _, err := h.mailboxes.Get(ctx, 1)
			if err != nil {
				t.Fatal(err)
			}
			if after.Version != before.Version || after.Value.Entries[envelope.ID].ClaimToken != claim.Token || after.Value.Entries[envelope.ID].ClaimAttempts != attempts {
				t.Fatal("refusal changed retained claim proof")
			}
		})
	}
}

func fillMailbox(t *testing.T, h *harness, count int) {
	t.Helper()
	for i := 0; i < count; i++ {
		req := directTo(1)
		req.RequestID = fmt.Sprintf("review4-filler-%d", i)
		mustSend(t, h, req)
	}
}

func TestDeletedUnclaimedMailMustNotResurrect(t *testing.T) {
	h := claimHarness(t)
	ctx := context.Background()
	mail := mustSend(t, h, withAttachment(directTo(1), "reward"))
	if _, err := h.service.Delete(ctx, 1, mail.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.service.ReserveClaim(ctx, 1, mail.ID, ""); !errors.Is(err, ErrMailMissing) {
		t.Fatal(err)
	}
	h.clock.advance(time.Second)
	fillMailbox(t, h, MaxMailboxEntries)
	box, _, err := h.service.Mailbox(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, retained := box.Entries[mail.ID]; retained {
		t.Fatal("fixture did not evict deleted entry")
	}
	if _, err := h.service.Delete(ctx, 1, "mail-2"); err != nil {
		t.Fatal(err)
	}
	if err := h.service.Deliver(ctx, 1, mail.ID, h.clock.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	claim, err := h.service.ReserveClaim(ctx, 1, mail.ID, "")
	if !errors.Is(err, ErrMailMissing) {
		t.Fatalf("deleted mail revived after retention: token=%q attachment=%q err=%v", claim.Token, claim.Attachment, err)
	}
}

func TestDeletedTombstonesAreBoundedWithoutForgettingUnknownExpiry(t *testing.T) {
	m := Mailbox{}
	m.init(1)
	for i := 0; i <= MaxSettledClaims; i++ {
		m.SettledClaims[fmt.Sprint(i)] = SettledClaim{Deleted: true, SettledAtUnix: int64(i)}
	}
	m.evict(1000)
	if !m.settledClaimsOverflow() || len(m.SettledClaims) != MaxSettledClaims+1 {
		t.Fatal("unknown deletion proof was aged out", len(m.SettledClaims))
	}
	m.SettledClaims["0"] = SettledClaim{Deleted: true, EnvelopeExpiresAtUnix: 1000}
	m.evict(1000)
	if m.settledClaimsOverflow() || len(m.SettledClaims) != MaxSettledClaims {
		t.Fatal("expired proof did not release capacity", len(m.SettledClaims))
	}
}

// Mailbox 的读取副本是真副本：clone 同时复制 SettledClaims，读出来的快照删掉一个墓碑不影响存储里的领取记录。
// 历史：RR-20260911-02。
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

// 结算身份的保留期限盖住信封还能被领取的那段时间：条数上限保证不了时间期限，墓碑按信封的可领取窗口过期，而不是按条数；窗口内挤不下时
// 明确拒绝投递，而不是悄悄忘掉一条领取身份（与 full() 对“腾不出位置”的处理同一个态度）。
// 历史：RR-20260911-01。
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
	if err := deliverMailboxFixture(h, ctx, 1, "after-expiry", 0); err != nil {
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
		err := deliverMailboxFixture(h, ctx, 1, fmt.Sprintf("filler-%s-%d", mailID, i), 0)
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
