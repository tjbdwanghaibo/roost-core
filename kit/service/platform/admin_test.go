package platform

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// exhaustStep is comfortably longer than any backoff the harness configures,
// so each AttemptDelivery in the loop below is actually Due rather than held.
const exhaustStep = time.Hour

// exhaust drives an order all the way to DeliveryExhausted, the way a
// deliverer that is down for longer than the retry budget would.
func exhaust(t *testing.T, h *harness, orderID string) Order {
	t.Helper()
	ctx := context.Background()
	h.deliverer.failFor[orderID] = 99
	raw, sig := callback(t, orderID, 1001, 499)
	if _, err := h.service.HandleCallback(ctx, raw, sig); err == nil {
		t.Fatal("the first attempt succeeded against a failing deliverer")
	}
	// Burn the budget. DeliveryAttempts is 3 in the harness.
	for i := 0; i < 8; i++ {
		h.clock.advance(exhaustStep)
		_, _ = h.service.AttemptDelivery(ctx, orderID)
	}
	order, found, err := h.service.Order(ctx, orderID)
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatalf("order %s is not recorded", orderID)
	}
	if order.State != DeliveryExhausted {
		t.Fatalf("order %s is %s after burning the budget, want exhausted", orderID, order.State)
	}
	return order
}

// The dead end this file exists for: before ReopenDelivery, an exhausted order
// could not be changed by any code path in the service. The player paid, the
// money was recorded, the goods were not delivered, and AttemptDelivery
// refused it forever.
func TestAnExhaustedOrderCanBeReopenedAndThenDelivers(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	exhaust(t, h, "o1")

	// Confirm the dead end is real rather than assumed.
	if _, err := h.service.AttemptDelivery(ctx, "o1"); !errors.Is(err, ErrDeliveryExpired) {
		t.Fatalf("AttemptDelivery on an exhausted order returned %v, want ErrDeliveryExpired", err)
	}
	if got := h.deliverer.grantsFor("o1"); got != 0 {
		t.Fatalf("the deliverer granted %d times while failing", got)
	}

	// The operator fixes the deliverer and reopens.
	h.deliverer.failFor["o1"] = 0
	reopened, err := h.service.ReopenDelivery(ctx, "o1", "game cluster was down 03:00-04:10, fixed")
	if err != nil {
		t.Fatal(err)
	}
	if reopened.State != DeliveryReserved {
		t.Fatalf("a reopened order is %s, want reserved", reopened.State)
	}
	if reopened.Attempts != 0 {
		t.Fatalf("a reopened order has %d attempts, want a fresh budget", reopened.Attempts)
	}
	if reopened.Reopens != 1 {
		t.Fatalf("Reopens is %d, want 1", reopened.Reopens)
	}
	if !strings.Contains(reopened.AdminNote, "cluster was down") {
		t.Fatalf("the operator note was not recorded: %q", reopened.AdminNote)
	}
	if reopened.AdminActionAtUnix == 0 {
		t.Fatal("the intervention has no timestamp, so it cannot be correlated with anything")
	}

	// And it is due immediately: an operator who just fixed the cause should
	// not have to wait out a backoff they did not set.
	receipt, err := h.service.AttemptDelivery(ctx, "o1")
	if err != nil {
		t.Fatalf("a reopened order did not deliver: %v", err)
	}
	if !receipt.Delivered {
		t.Fatal("the receipt does not report a delivery")
	}
	if got := h.deliverer.grantsFor("o1"); got != 1 {
		t.Fatalf("the deliverer granted %d times, want exactly 1", got)
	}
}

// Reopening is the only operation in this service that can cause a SECOND
// GRANT, so the state check is the load-bearing one.
//
// This is the defect the whole package was rewritten to make unrepresentable,
// reintroduced through the operator door instead of the provider's.
func TestReopeningRefusesAnyStateThatWouldGrantTwice(t *testing.T) {
	ctx := context.Background()

	t.Run("delivered", func(t *testing.T) {
		h := newHarness(t)
		raw, sig := callback(t, "o1", 1001, 499)
		if _, err := h.service.HandleCallback(ctx, raw, sig); err != nil {
			t.Fatal(err)
		}
		if got := h.deliverer.grantsFor("o1"); got != 1 {
			t.Fatalf("setup: granted %d times, want 1", got)
		}
		_, err := h.service.ReopenDelivery(ctx, "o1", "please send it again")
		if !errors.Is(err, ErrNotResolvable) {
			t.Fatalf("reopening a DELIVERED order returned %v; it would grant the goods a "+
				"second time", err)
		}
		if got := h.deliverer.grantsFor("o1"); got != 1 {
			t.Fatalf("the deliverer granted %d times after a refused reopen, want 1", got)
		}
	})

	t.Run("settled", func(t *testing.T) {
		h := newHarness(t)
		exhaust(t, h, "o1")
		if _, err := h.service.SettleOutOfBand(ctx, "o1", "refunded via provider console"); err != nil {
			t.Fatal(err)
		}
		h.deliverer.failFor["o1"] = 0
		_, err := h.service.ReopenDelivery(ctx, "o1", "second thoughts")
		if !errors.Is(err, ErrNotResolvable) {
			t.Fatalf("reopening a SETTLED order returned %v; the debt was already paid another "+
				"way, so delivering as well pays twice", err)
		}
	})

	t.Run("reserved is refused distinctly", func(t *testing.T) {
		h := newHarness(t)
		h.deliverer.failFor["o1"] = 1
		raw, sig := callback(t, "o1", 1001, 499)
		if _, err := h.service.HandleCallback(ctx, raw, sig); err == nil {
			t.Fatal("setup: the callback succeeded against a failing deliverer")
		}
		_, err := h.service.ReopenDelivery(ctx, "o1", "impatient")
		if !errors.Is(err, ErrNotResolvable) {
			t.Fatalf("reopening a RESERVED order returned %v", err)
		}
		// The message must say it is ALREADY retryable, so an operator whose
		// call timed out and retried gets an answer it can act on.
		if !strings.Contains(err.Error(), "already retryable") {
			t.Fatalf("the refusal does not say the order is already retryable: %v", err)
		}
	})
}

// Settling is allowed only from exhausted, because the retry loop is still
// live in every other non-terminal state: settling then races an attempt that
// may yet succeed, and the pair of outcomes is a refund AND the goods.
func TestSettlingRefusesAnOrderTheRetryLoopStillOwns(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.deliverer.failFor["o1"] = 1
	raw, sig := callback(t, "o1", 1001, 499)
	if _, err := h.service.HandleCallback(ctx, raw, sig); err == nil {
		t.Fatal("setup: the callback succeeded")
	}
	_, err := h.service.SettleOutOfBand(ctx, "o1", "refunding")
	if !errors.Is(err, ErrNotResolvable) {
		t.Fatalf("settling a RESERVED order returned %v; it races the retry loop", err)
	}
	// And a delivered order cannot be settled either: it would report a
	// fulfilment as a refund.
	h2 := newHarness(t)
	raw2, sig2 := callback(t, "o2", 1001, 499)
	if _, err := h2.service.HandleCallback(ctx, raw2, sig2); err != nil {
		t.Fatal(err)
	}
	if _, err := h2.service.SettleOutOfBand(ctx, "o2", "refunding"); !errors.Is(err, ErrNotResolvable) {
		t.Fatalf("settling a DELIVERED order returned %v", err)
	}
}

// DeliverySettled is a third terminal state, not a reuse of DeliveryDelivered.
//
// A reconciliation that counted a hand-refunded order as delivered would
// report a fulfilment this service did not perform. "The deliverer succeeded",
// "we gave up" and "a human resolved it elsewhere" are three different facts
// about the money.
func TestSettledIsTerminalAndDistinctFromDelivered(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	exhaust(t, h, "o1")
	settled, err := h.service.SettleOutOfBand(ctx, "o1", "refunded, ticket OPS-4471")
	if err != nil {
		t.Fatal(err)
	}
	if settled.State == DeliveryDelivered {
		t.Fatal("a settled order reports DeliveryDelivered; a refund would be counted as a " +
			"fulfilment in every reconciliation")
	}
	if settled.State != DeliverySettled {
		t.Fatalf("a settled order is %s", settled.State)
	}
	if !settled.Terminal() {
		t.Fatal("a settled order is not Terminal, so the retry loop still considers it due")
	}
	if settled.Due(h.clock.now.Unix()) {
		t.Fatal("a settled order is still Due; the run hook would keep retrying a refunded order")
	}
	// The retry loop must refuse it, not deliver it — and refuse it with the
	// RIGHT answer. Adding DeliverySettled without this case made a settled
	// order fall through to the not-due branch, which reported "a delivery is
	// in flight: order o1 until 0": the retry hook treats ErrDeliveryHeld as
	// benign, so a refunded order would be retried on every tick forever and
	// that nonsense message would be the only clue.
	h.deliverer.failFor["o1"] = 0
	_, err = h.service.AttemptDelivery(ctx, "o1")
	if err == nil {
		t.Fatal("AttemptDelivery accepted a settled order")
	}
	if errors.Is(err, ErrDeliveryHeld) {
		t.Fatalf("a settled order reports ErrDeliveryHeld (%v); the retry hook treats that as "+
			"benign and would retry a refunded order forever", err)
	}
	if !errors.Is(err, ErrOrderSettled) {
		t.Fatalf("a settled order reports %v, want ErrOrderSettled", err)
	}
	if got := h.deliverer.grantsFor("o1"); got != 0 {
		t.Fatalf("the deliverer granted %d times for a refunded order", got)
	}
}

// An intervention with no recorded reason cannot be reviewed. Both methods
// require a note, with no default.
func TestBothOperationsRequireANote(t *testing.T) {
	ctx := context.Background()
	for _, blank := range []string{"", "   ", "\t\n"} {
		h := newHarness(t)
		exhaust(t, h, "o1")
		if _, err := h.service.ReopenDelivery(ctx, "o1", blank); !errors.Is(err, ErrAdminNoteRequired) {
			t.Fatalf("ReopenDelivery with note %q returned %v", blank, err)
		}
		if _, err := h.service.SettleOutOfBand(ctx, "o1", blank); !errors.Is(err, ErrAdminNoteRequired) {
			t.Fatalf("SettleOutOfBand with note %q returned %v", blank, err)
		}
		// The order must be untouched by a refused call.
		order, _, err := h.service.Order(ctx, "o1")
		if err != nil {
			t.Fatal(err)
		}
		if order.State != DeliveryExhausted || order.AdminNote != "" {
			t.Fatalf("a refused intervention changed the order: %+v", order)
		}
	}
	// And an oversized note is refused rather than stored, because the note is
	// read back by whoever looks at the order next.
	h := newHarness(t)
	exhaust(t, h, "o2")
	if _, err := h.service.ReopenDelivery(ctx, "o2", strings.Repeat("x", MaxAdminNoteBytes+1)); !errors.Is(err, ErrAdminNoteRequired) {
		t.Fatalf("an oversized note was accepted: %v", err)
	}
}

// Two operators clicking the same button, or one clicking twice, must not both
// reopen: the second would reset a budget the first already reset, and a third
// would keep an order retrying forever with Reopens telling nobody.
func TestConcurrentReopensGrantOneFreshBudget(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	exhaust(t, h, "o1")
	h.deliverer.failFor["o1"] = 0

	const racers = 8
	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		succeeded int
	)
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := h.service.ReopenDelivery(ctx, "o1", "concurrent operators"); err == nil {
				mu.Lock()
				succeeded++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if succeeded != 1 {
		t.Fatalf("%d of %d concurrent reopens succeeded, want exactly 1", succeeded, racers)
	}
	order, _, err := h.service.Order(ctx, "o1")
	if err != nil {
		t.Fatal(err)
	}
	if order.Reopens != 1 {
		t.Fatalf("Reopens is %d after %d concurrent attempts, want 1", order.Reopens, racers)
	}
	// The order delivers exactly once afterwards.
	if _, err := h.service.AttemptDelivery(ctx, "o1"); err != nil {
		t.Fatal(err)
	}
	if got := h.deliverer.grantsFor("o1"); got != 1 {
		t.Fatalf("the deliverer granted %d times, want 1", got)
	}
}

// Reopens accumulates rather than resetting, because "we reopened this five
// times and it still fails" is the fact that stops someone reopening it a
// sixth time. Resetting Attempts alone would erase it.
func TestReopensAccumulateAcrossInterventions(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	exhaust(t, h, "o1")
	for round := int32(1); round <= 3; round++ {
		if _, err := h.service.ReopenDelivery(ctx, "o1", "trying again"); err != nil {
			t.Fatal(err)
		}
		order, _, err := h.service.Order(ctx, "o1")
		if err != nil {
			t.Fatal(err)
		}
		if order.Reopens != round {
			t.Fatalf("after %d reopens Reopens is %d", round, order.Reopens)
		}
		// Burn the fresh budget again.
		for i := 0; i < 8; i++ {
			h.clock.advance(exhaustStep)
			_, _ = h.service.AttemptDelivery(ctx, "o1")
		}
	}
}

// Admin is deliberately not on the bus, so the Platform interface must not
// have grown these methods. A generated transport for them would mean any
// process on the bus can reopen any paid order.
func TestTheOperatorSurfaceIsNotOnTheCrossProcessInterface(t *testing.T) {
	var asPlatform any = Capability(&Service{})
	if _, ok := asPlatform.(Admin); ok {
		t.Fatal("the capability published to other processes satisfies Admin; a caller over the " +
			"bus could reopen a paid order, and the bus carries no identity this service can " +
			"verify")
	}
}
