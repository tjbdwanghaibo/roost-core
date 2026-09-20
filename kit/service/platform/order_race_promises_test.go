package platform

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/versionstore"
)

// vanishingOrders loses the insert to a racer whose row is gone by the time it
// is read back.
type vanishingOrders struct {
	versionstore.Store[string, Order]
}

func (vanishingOrders) Create(context.Context, string, Order) (versionstore.Versioned[Order], bool, error) {
	return versionstore.Versioned[Order]{}, false, nil
}
func (vanishingOrders) Get(context.Context, string) (versionstore.Versioned[Order], bool, error) {
	return versionstore.Versioned[Order]{}, false, nil
}

// mismatchedOrders loses the insert to a racer that recorded the same order id
// with a different payload.
type mismatchedOrders struct {
	versionstore.Store[string, Order]
}

func (mismatchedOrders) Create(context.Context, string, Order) (versionstore.Versioned[Order], bool, error) {
	return versionstore.Versioned[Order]{}, false, nil
}
func (mismatchedOrders) Get(_ context.Context, orderID string) (versionstore.Versioned[Order], bool, error) {
	return versionstore.Versioned[Order]{Value: Order{OrderID: orderID, PayloadDigest: "someone-else's-payload", State: DeliveryDelivered}, Version: 1}, true, nil
}

// ordersLosingRowAt behaves normally until the n-th Update, which then finds
// the row gone — the shape of a record deleted between the claim and the
// write that should have followed it.
type ordersLosingRowAt struct {
	versionstore.Store[string, Order]
	at    int
	calls int
}

func (o *ordersLosingRowAt) Update(ctx context.Context, key string, mutate versionstore.Mutate[Order]) (versionstore.Versioned[Order], bool, error) {
	o.calls++
	if o.calls == o.at {
		_, _, err := mutate(Order{}, false)
		return versionstore.Versioned[Order]{}, false, err
	}
	return o.Store.Update(ctx, key, mutate)
}

// U-0097 (C2): every "the order moved under us" branch of the callback and
// delivery paths is a refusal the caller sees; none of them grants goods.
func TestCallbackReportsEachOrderRaceAndGrantsNothing(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name    string
		mutate  func(*Config)
		want    error
		message string
	}{
		{"insert lost and racer row vanished", func(cfg *Config) { cfg.Orders = vanishingOrders{Store: cfg.Orders} },
			ErrConflict, "order order-1 vanished during create"},
		{"insert lost to a different payload", func(cfg *Config) { cfg.Orders = mismatchedOrders{Store: cfg.Orders} },
			ErrOrderMismatch, "order order-1"},
		{"row gone before the delivered mark", func(cfg *Config) { cfg.Orders = &ordersLosingRowAt{Store: cfg.Orders, at: 2} },
			ErrConflict, "order order-1 vanished"},
		{"row gone before the failure record", func(cfg *Config) {
			cfg.Deliver.(*recordingDeliverer).failFor["order-1"] = 1
			cfg.Orders = &ordersLosingRowAt{Store: cfg.Orders, at: 2}
		}, ErrConflict, "order order-1 vanished"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, tc.mutate)
			raw, signature := callback(t, "order-1", 1001, 499)
			_, err := h.service.HandleCallback(ctx, raw, signature)
			if !errors.Is(err, tc.want) || !strings.Contains(err.Error(), tc.message) {
				t.Fatalf("HandleCallback = %v; want %v containing %q", err, tc.want, tc.message)
			}
			if strings.HasPrefix(tc.name, "insert lost") && h.deliverer.grantsFor("order-1") != 0 {
				t.Fatalf("goods were granted %d time(s) for an order that was never reserved", h.deliverer.grantsFor("order-1"))
			}
			if got := h.metrics.Count("accepted:deliver"); got != 0 {
				t.Fatalf("a refused delivery was counted as accepted (%d); %s", got, h.metrics.Events())
			}
		})
	}
}
