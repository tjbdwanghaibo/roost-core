package platform

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// blockingDeliverer parks its first call until released, so a second attempt
// can claim and deliver the same order while the first is still inside the
// deliverer — the one window the in-CAS claim cannot close, because the
// backoff that guards it can elapse while a slow deliverer is running.
type blockingDeliverer struct {
	mu      sync.Mutex
	calls   int
	entered chan struct{}
	release chan struct{}
}

func (d *blockingDeliverer) Deliver(_ context.Context, _ Order) error {
	d.mu.Lock()
	d.calls++
	first := d.calls == 1
	d.mu.Unlock()
	if first {
		close(d.entered)
		<-d.release
	}
	return nil
}

// When two attempts both grant, the SECOND commit finds the order delivered
// and must leave the recorded delivery time alone — "when was order X
// delivered" is the first commit, not whichever retry finished last — and
// must report the double grant as a conflict, because it is a bug worth
// seeing rather than a routine replay.
func TestALateAttemptDoesNotMoveTheDeliveryTimeAndIsCountedAsAConflict(t *testing.T) {
	d := &blockingDeliverer{entered: make(chan struct{}), release: make(chan struct{})}
	h := newHarness(t, func(cfg *Config) { cfg.Deliver = d })
	ctx := context.Background()
	raw, signature := callback(t, "order-1", 1001, 499)

	var (
		firstReceipt Receipt
		firstErr     error
		done         = make(chan struct{})
	)
	go func() {
		defer close(done)
		firstReceipt, firstErr = h.service.HandleCallback(ctx, raw, signature)
	}()
	<-d.entered

	// The first attempt's backoff elapses while it is still delivering, so a
	// retry is admitted and commits first.
	h.clock.advance(time.Hour)
	secondAt := h.clock.Now().Unix()
	second, err := h.service.AttemptDelivery(ctx, "order-1")
	if err != nil || !second.Delivered || second.Replayed {
		t.Fatalf("the retry that overtook a slow delivery: err=%v delivered=%v replayed=%v", err, second.Delivered, second.Replayed)
	}
	close(d.release)
	<-done
	if firstErr != nil {
		t.Fatalf("the overtaken attempt failed: %v", firstErr)
	}
	if !firstReceipt.Replayed || !firstReceipt.Delivered {
		t.Fatalf("the overtaken attempt reported replayed=%v delivered=%v; it must read as a replay of the commit that beat it",
			firstReceipt.Replayed, firstReceipt.Delivered)
	}
	order, _, err := h.service.Order(ctx, "order-1")
	if err != nil {
		t.Fatal(err)
	}
	if order.DeliveredAtUnix != secondAt {
		t.Fatalf("delivered_at moved to %d; the first commit was at %d", order.DeliveredAtUnix, secondAt)
	}
	if got := h.metrics.Count("conflict:deliver"); got != 1 {
		t.Fatalf("a double grant reported %d conflicts; %s", got, h.metrics.Events())
	}
}

// An order whose budget is already spent when an attempt claims it — a
// stored record that says "reserved, 3 of 3 attempts" — is moved to
// exhausted and counted, not refused in place: a refusal leaves an order
// that looks retryable to every sweep forever.
func TestClaimingASpentBudgetRecordsExhaustionRatherThanRefusingInPlace(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	now := h.clock.Now().Unix()
	if _, _, err := h.orders.Create(ctx, "order-9", Order{
		OrderID: "order-9", PlayerID: 1001, Channel: "test", ProductID: "gems", AmountMinor: 499,
		State: DeliveryReserved, Attempts: 3, MaxAttempts: 3,
		PaidAtUnix: now, CreatedAtUnix: now, UpdatedAtUnix: now,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.service.AttemptDelivery(ctx, "order-9"); !errors.Is(err, ErrDeliveryExpired) {
		t.Fatalf("claiming a spent budget produced %v, want ErrDeliveryExpired", err)
	}
	stored, found, err := h.orders.Get(ctx, "order-9")
	if err != nil || !found {
		t.Fatalf("order vanished: found=%v err=%v", found, err)
	}
	if stored.Value.State != DeliveryExhausted {
		t.Fatalf("the stored order is %s after its budget was found spent, want exhausted", stored.Value.State)
	}
	if got := h.metrics.Count("dropped:order.exhausted"); got != 1 {
		t.Fatalf("exhaustion at claim reported %d drops; %s", got, h.metrics.Events())
	}
	if h.deliverer.grantsFor("order-9") != 0 {
		t.Fatal("the deliverer ran for an order with no budget")
	}
}
