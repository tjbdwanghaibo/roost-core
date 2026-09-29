package platform

import (
	"context"
	"errors"
	"fmt"
	fredis "github.com/tjbdwanghaibo/roost-core/redis"
	"github.com/tjbdwanghaibo/roost-core/redis/driver"
	"os"
	"testing"
	"time"
)

func bugfix4Platform(t *testing.T, attempts int, deliver Deliverer) *harness {
	t.Helper()
	h := newHarness(t, func(c *Config) {
		c.DeliveryAttempts = attempts
		c.Deliver = deliver
		if os.Getenv("ROOST_REVIEW4_BACKEND") == "redis" {
			addr := os.Getenv("ROOST_REVIEW_REDIS")
			if addr == "" {
				t.Fatal("isolated Redis address is required")
			}
			client, err := driver.NewClient(fredis.DefaultConfig(addr))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = client.Close() })
			store, err := NewRedisOrders(client, fmt.Sprintf("review4:platform:%d", time.Now().UnixNano()))
			if err != nil {
				t.Fatal(err)
			}
			c.Orders = store
		}
	})
	h.orders = h.service.cfg.Orders
	order := Order{OrderID: "paid", PlayerID: 1, Channel: "store", ProductID: "gems-100", AmountMinor: 100, Currency: "USD", State: DeliveryReserved, MaxAttempts: int32(attempts), PaidAtUnix: h.clock.Now().Unix(), CreatedAtUnix: h.clock.Now().Unix(), UpdatedAtUnix: h.clock.Now().Unix()}
	if err := order.Validate(); err != nil {
		t.Fatal(err)
	}
	order.PayloadDigest = contentDigest(order)
	_, created, err := h.orders.Create(context.Background(), "paid", order)
	if err != nil || !created {
		t.Fatal(created, err)
	}
	return h
}

// The goods commit at the external boundary; only the response is lost.
func TestBugfix4UnknownGrantMustBlockSettlement(t *testing.T) {
	grants := 0
	h := bugfix4Platform(t, 1, DelivererFunc(func(context.Context, Order) error { grants++; return context.DeadlineExceeded }))
	ctx := context.Background()
	_, firstErr := h.service.AttemptDelivery(ctx, "paid")
	if !errors.Is(firstErr, context.DeadlineExceeded) || grants != 1 {
		t.Fatal(firstErr, grants)
	}
	before, found, err := h.service.Order(ctx, "paid")
	if err != nil || !found {
		t.Fatal(err, found)
	}
	settled, settleErr := h.service.SettleOutOfBand(ctx, "paid", "refund after exhausted delivery")
	if len(before.PendingAttempts) == 0 || !errors.Is(settleErr, ErrNotResolvable) {
		t.Fatalf("external grant=%d timeout=%v pending=%v state=%s; refund admitted: state=%s err=%v", grants, firstErr, before.PendingAttempts, before.State, settled.State, settleErr)
	}
}

func TestBugfix4UnknownGrantMustNotGrantAgain(t *testing.T) {
	grants := 0
	h := bugfix4Platform(t, 2, DelivererFunc(func(context.Context, Order) error {
		grants++
		if grants == 1 {
			return context.DeadlineExceeded
		}
		return nil
	}))
	ctx := context.Background()
	if _, err := h.service.AttemptDelivery(ctx, "paid"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	h.clock.advance(6 * time.Second)
	receipt, err := h.service.AttemptDelivery(ctx, "paid")
	if grants != 1 {
		t.Fatalf("lost external grant reply caused %d grants; receipt=%+v err=%v", grants, receipt, err)
	}
}

func TestUnknownGrantProofSurvivesServiceReconstruction(t *testing.T) {
	grants := 0
	h := bugfix4Platform(t, 3, DelivererFunc(func(context.Context, Order) error {
		grants++
		return context.DeadlineExceeded
	}))
	ctx := context.Background()
	if _, err := h.service.AttemptDelivery(ctx, "paid"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	rebuilt, err := New(h.service.cfg)
	if err != nil {
		t.Fatal(err)
	}
	h.clock.advance(time.Hour)
	if _, err := rebuilt.AttemptDelivery(ctx, "paid"); !errors.Is(err, ErrDeliveryExpired) || grants != 1 {
		t.Fatal("reconstruction retried an unknown grant", grants, err)
	}
	if _, err := rebuilt.ReopenDelivery(ctx, "paid", "retry before reconciliation"); !errors.Is(err, ErrNotResolvable) {
		t.Fatal(err)
	}
	if _, err := rebuilt.ResolvePendingAttempts(ctx, "paid", "external record inspected; refund chosen"); err != nil {
		t.Fatal(err)
	}
	if got, err := rebuilt.SettleOutOfBand(ctx, "paid", "refunded after reconciliation"); err != nil || got.State != DeliverySettled {
		t.Fatal(got, err)
	}
}

// The collaborator explicitly proves that the refusal performed no grant.
func TestBugfix4KnownNoGrantControl(t *testing.T) {
	h := bugfix4Platform(t, 1, DelivererFunc(func(context.Context, Order) error {
		return fmt.Errorf("%w: rejected before grant", ErrDeliveryNotApplied)
	}))
	ctx := context.Background()
	if _, err := h.service.AttemptDelivery(ctx, "paid"); !errors.Is(err, ErrDeliveryExpired) {
		t.Fatal(err)
	}
	got, err := h.service.SettleOutOfBand(ctx, "paid", "verified no grant; refunded")
	if err != nil || got.State != DeliverySettled {
		t.Fatal(got, err)
	}
}

func TestBugfix4CallbackReceiptMustOwnPendingProof(t *testing.T) {
	started, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	h := bugfix4Platform(t, 2, DelivererFunc(func(context.Context, Order) error { close(started); <-release; return nil }))
	raw, sig := callback(t, "alias-order", 1, 100)
	ctx := context.Background()
	go func() { _, err := h.service.HandleCallback(ctx, raw, sig); done <- err }()
	<-started
	defer func() {
		close(release)
		if err := <-done; err != nil {
			t.Errorf("original callback completion: %v", err)
		}
	}()
	receipt, err := h.service.HandleCallback(ctx, raw, sig)
	if !errors.Is(err, ErrDeliveryHeld) || len(receipt.Order.PendingAttempts) != 1 {
		t.Fatal(receipt, err)
	}
	before, _, err := h.orders.Get(ctx, "alias-order")
	if err != nil {
		t.Fatal(err)
	}
	expected := before.Value.PendingAttempts[0]
	receipt.Order.PendingAttempts[0] = expected + 100
	after, _, err := h.orders.Get(ctx, "alias-order")
	if err != nil {
		t.Fatal(err)
	}
	if after.Value.PendingAttempts[0] != expected || after.Version != before.Version {
		t.Fatalf("receipt changed authoritative pending proof without CAS: before=%d after=%v version=%d/%d", expected, after.Value.PendingAttempts, before.Version, after.Version)
	}
}
