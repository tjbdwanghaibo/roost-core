package platform

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestReviewSettledOrderSurvivesLateDelivery(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	h := newHarness(t, func(c *Config) {
		c.DeliveryAttempts = 1
		c.Deliver = DelivererFunc(func(context.Context, Order) error { close(started); <-release; return nil })
	})
	ctx := context.Background()
	_, created, err := h.orders.Create(ctx, "late", Order{OrderID: "late", PlayerID: 1, ProductID: "gem", AmountMinor: 1, State: DeliveryReserved, MaxAttempts: 1})
	if err != nil || !created {
		t.Fatal(err)
	}
	go func() { _, err := h.service.AttemptDelivery(ctx, "late"); done <- err }()
	<-started
	h.clock.advance(6 * time.Second)
	_, err = h.service.AttemptDelivery(ctx, "late")
	if !errors.Is(err, ErrDeliveryExpired) {
		close(release)
		<-done
		t.Fatal(err)
	}
	_, err = h.service.SettleOutOfBand(ctx, "late", "refunded")
	close(release)
	lateErr := <-done
	if !errors.Is(err, ErrNotResolvable) || lateErr != nil {
		t.Fatal(err, lateErr)
	}
	order, _, err := h.service.Order(ctx, "late")
	if err != nil {
		t.Fatal(err)
	}
	if order.State != DeliveryDelivered || order.AdminNote != "" {
		t.Fatalf("pending delivery was incorrectly settled: state=%s note=%s", order.State, order.AdminNote)
	}
}
