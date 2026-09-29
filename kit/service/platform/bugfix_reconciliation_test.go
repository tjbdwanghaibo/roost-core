package platform

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestReconciledSettlementRejectsStaleDeliveryCompletion(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	h := newHarness(t, func(c *Config) {
		c.Deliver = DelivererFunc(func(context.Context, Order) error { close(started); <-release; return nil })
	})
	ctx := context.Background()
	if _, _, err := h.orders.Create(ctx, "reconciled", Order{OrderID: "reconciled", PlayerID: 1, State: DeliveryReserved, MaxAttempts: 1}); err != nil {
		t.Fatal(err)
	}
	go func() { _, err := h.service.AttemptDelivery(ctx, "reconciled"); done <- err }()
	<-started
	h.clock.advance(6 * time.Second)
	if _, err := h.service.AttemptDelivery(ctx, "reconciled"); !errors.Is(err, ErrDeliveryExpired) {
		close(release)
		<-done
		t.Fatal(err)
	}
	if _, err := h.service.ReopenDelivery(ctx, "reconciled", "retry"); !errors.Is(err, ErrNotResolvable) {
		close(release)
		<-done
		t.Fatal(err)
	}
	if _, err := h.service.ResolvePendingAttempts(ctx, "reconciled", "delivery stopped; external result reconciled"); err != nil {
		close(release)
		<-done
		t.Fatal(err)
	}
	if _, err := h.service.SettleOutOfBand(ctx, "reconciled", "refunded"); err != nil {
		close(release)
		<-done
		t.Fatal(err)
	}
	close(release)
	if err := <-done; !errors.Is(err, ErrOrderSettled) {
		t.Fatal(err)
	}
	got, _, err := h.service.Order(ctx, "reconciled")
	if err != nil || got.State != DeliverySettled || got.AdminNote != "refunded" {
		t.Fatal(got, err)
	}
}
