package mail

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"testing"
	"time"

	fredis "github.com/tjbdwanghaibo/roost-core/redis"
	driver "github.com/tjbdwanghaibo/roost-core/redis/driver"
)

func review3MailHarness(t *testing.T) *harness {
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
func TestReview3OldCancelCannotReleaseNewClaimAttempt(t *testing.T) {
	h := review3MailHarness(t)
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
func TestReview3ObserveExpiredCancelledClaimsRetainCapacity(t *testing.T) {
	h := review3MailHarness(t)
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
