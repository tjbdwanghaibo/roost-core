package platform

import (
	"context"
	"errors"
	"github.com/tjbdwanghaibo/roost-core/infra/storage/versionstore"
	"strings"
	"testing"
)

func TestOrderOperationsRefuseAnUnrecordedOrder(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	if _, err := h.service.ReopenDelivery(ctx, "ghost", "ops reviewed, cluster was down"); !errors.Is(err, ErrOrderInvalid) || !strings.Contains(err.Error(), "ghost is not recorded") {
		t.Fatalf("ReopenDelivery of an unrecorded order = %v", err)
	}
	if _, err := h.service.SettleOutOfBand(ctx, "ghost", "refunded via provider console"); !errors.Is(err, ErrOrderInvalid) || !strings.Contains(err.Error(), "ghost is not recorded") {
		t.Fatalf("SettleOutOfBand of an unrecorded order = %v", err)
	}
	if _, err := h.service.AttemptDelivery(ctx, "ghost"); !errors.Is(err, ErrOrderInvalid) || !strings.Contains(err.Error(), "ghost is not recorded") {
		t.Fatalf("AttemptDelivery of an unrecorded order = %v", err)
	}
	if _, found, _ := h.orders.Get(ctx, "ghost"); found {
		t.Fatal("a refused operation recorded the order")
	}
	if h.deliverer.grantsFor("ghost") != 0 {
		t.Fatal("a refused delivery attempt reached the deliverer")
	}
}

func TestCallbackAndDeliveryRefuseEmptyInputs(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	if _, err := h.service.HandleCallback(ctx, nil, "sig"); !errors.Is(err, ErrRequestInvalid) || !strings.Contains(err.Error(), "payload is empty") {
		t.Fatalf("HandleCallback with an empty payload = %v", err)
	}
	if _, err := h.service.AttemptDelivery(ctx, "  "); !errors.Is(err, ErrRequestInvalid) || !strings.Contains(err.Error(), "order id is empty") {
		t.Fatalf("AttemptDelivery with a blank order id = %v", err)
	}
	if _, err := NewRedisOrders(nil, "  "); err == nil || !strings.Contains(err.Error(), "key prefix is required") {
		t.Fatalf("NewRedisOrders with a blank prefix = %v", err)
	}
}

func TestAuthSessionRefusesANonPositivePlayerID(t *testing.T) {
	ctx := context.Background()
	for _, id := range []int64{0, -5} {
		h := newHarness(t, func(cfg *Config) {
			cfg.Players = PlayerResolverFunc(func(context.Context, Verified) (int64, error) { return id, nil })
		})
		session, err := h.service.AuthSession(ctx, Credential{Channel: "store", OpenID: "u1", Secret: "good"})
		if err == nil || !strings.Contains(err.Error(), "non-positive id") || session.Token != "" {
			t.Fatalf("AuthSession with the resolver answering %d = (%+v, %v)", id, session, err)
		}
	}
	h := newHarness(t)
	if session, err := h.service.AuthSession(ctx, Credential{Channel: "store", OpenID: "u1", Secret: "good"}); err != nil || session.Token == "" {
		t.Fatalf("AuthSession with a real resolver = (%+v, %v)", session, err)
	}
}

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
