package mail

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func bugfix4FillMail(t *testing.T, h *harness, count int) {
	t.Helper()
	for i := 0; i < count; i++ {
		req := directTo(1)
		req.RequestID = fmt.Sprintf("review4-filler-%d", i)
		mustSend(t, h, req)
	}
}

func TestBugfix4DeletedUnclaimedMailMustNotResurrect(t *testing.T) {
	h := review3MailHarness(t)
	ctx := context.Background()
	mail := mustSend(t, h, withAttachment(directTo(1), "reward"))
	if _, err := h.service.Delete(ctx, 1, mail.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.service.ReserveClaim(ctx, 1, mail.ID, ""); !errors.Is(err, ErrMailMissing) {
		t.Fatal(err)
	}
	h.clock.advance(time.Second)
	bugfix4FillMail(t, h, MaxMailboxEntries)
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
