package mail

import (
	"context"
	"testing"
	"time"
)

func TestClaimLeaseRejectsSubsecond(t *testing.T) {
	h := newHarness(t)
	cfg := h.service.cfg
	cfg.ClaimLease = time.Millisecond
	if _, err := New(cfg); err == nil {
		t.Fatal("subsecond claim lease accepted despite Unix-second deadline")
	}
}
func TestDeliverRefusesMissingEnvelope(t *testing.T) {
	h := newHarness(t)
	if err := h.service.Deliver(context.Background(), 1, "missing", h.clock.Now().Unix()); err == nil {
		t.Fatal("missing envelope created immortal unread entry")
	}
	if _, found, err := h.mailboxes.Get(context.Background(), 1); err != nil || found {
		t.Fatalf("missing envelope changed mailbox: found=%v err=%v", found, err)
	}
}
