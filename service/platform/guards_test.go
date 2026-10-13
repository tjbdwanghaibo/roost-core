// 订单入参守卫：未记录订单、空输入、非正玩家、回调与交付的竞争拒绝、字段校验、未知领取不重复外部发放。

package platform

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/infra/storage/versionstore"
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

// Every "the order moved under us" branch of the callback and delivery paths is a refusal the caller sees; none of them grants goods.
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

func TestValidateSessionRejectsZeroPlayer(t *testing.T) {
	h := newHarness(t)
	session, err := h.service.AuthSession(context.Background(), Credential{Channel: "store", OpenID: "u1", Secret: "good"})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.service.ValidateSession(0, session.Token); err == nil {
		t.Fatal("zero player accepted another player's valid token")
	}
}

// Provide 只装配能力；若构造期间访问 Redis，这个无连接替身会直接失败。

// 模拟认领已持久、核对仍不确定；不能将未确认的认领当作可再次发货。
type lostClaimReply struct {
	OrderStore
	lose bool
}

func (s *lostClaimReply) Update(ctx context.Context, key string, mutate versionstore.Mutate[Order]) (versionstore.Versioned[Order], bool, error) {
	v, changed, err := s.OrderStore.Update(ctx, key, mutate)
	if err == nil && changed && s.lose {
		s.lose = false
		return versionstore.Versioned[Order]{}, false, versionstore.ErrOutcomeUnknown
	}
	return v, changed, err
}
func TestUnknownClaimDoesNotCallOrRepeatExternalGrant(t *testing.T) {
	calls := 0
	h := bugfix4Platform(t, 3, DelivererFunc(func(context.Context, Order) error { calls++; return nil }))
	h.service.cfg.Orders = &lostClaimReply{OrderStore: h.orders, lose: true}
	if _, err := h.service.AttemptDelivery(context.Background(), "paid"); !errors.Is(err, versionstore.ErrOutcomeUnknown) || calls != 0 {
		t.Fatalf("unknown claim granted: calls=%d err=%v", calls, err)
	}
	h.clock.advance(time.Hour)
	if _, err := h.service.AttemptDelivery(context.Background(), "paid"); !errors.Is(err, ErrDeliveryExpired) || calls != 0 {
		t.Fatalf("unknown claim retried grant: calls=%d err=%v", calls, err)
	}
}

func validOrder() Order {
	return Order{OrderID: "o-1", PlayerID: 7, Channel: "appstore", ProductID: "gems.100", AmountMinor: 499, Currency: "USD", MaxAttempts: 3}
}

// Order.Validate stands between a provider callback and goods delivery. The
// amount rule in particular is the "free money" guard; each rule is pinned by
// message so a dropped one turns exactly one case red.
func TestOrderValidateRefusesEachBrokenField(t *testing.T) {
	if err := validOrder().Validate(); err != nil {
		t.Fatalf("baseline rejected: %v", err)
	}
	cases := []struct {
		name   string
		mutate func(*Order)
		text   string
	}{
		{"blank order id", func(o *Order) { o.OrderID = " " }, "order id is empty"},
		{"order id too long", func(o *Order) { o.OrderID = strings.Repeat("o", MaxOrderIDBytes+1) }, "order id is"},
		{"player id not positive", func(o *Order) { o.PlayerID = 0 }, "player id must be positive"},
		{"blank product id", func(o *Order) { o.ProductID = "" }, "product id is empty"},
		{"product id too long", func(o *Order) { o.ProductID = strings.Repeat("p", MaxProductIDBytes+1) }, "product id is"},
		{"zero amount", func(o *Order) { o.AmountMinor = 0 }, "paid amount must be positive, got 0"},
		{"negative amount", func(o *Order) { o.AmountMinor = -1 }, "paid amount must be positive, got -1"},
		{"no attempt budget", func(o *Order) { o.MaxAttempts = 0 }, "max attempts must be positive"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o := validOrder()
			tc.mutate(&o)
			err := o.Validate()
			if !errors.Is(err, ErrOrderInvalid) || !strings.Contains(err.Error(), tc.text) {
				t.Fatalf("Validate = %v, want ErrOrderInvalid containing %q", err, tc.text)
			}
		})
	}
}

// A credential without a secret is exactly the shape the replaced login
// accepted; the verifier's answer must name a channel and an open id or the
// player id derived from it is meaningless.
func TestCredentialAndVerifiedValidateRefuseEachBlankField(t *testing.T) {
	good := Credential{Channel: "wechat", OpenID: "open-1", Secret: "ticket"}
	if err := good.Validate(); err != nil {
		t.Fatalf("baseline rejected: %v", err)
	}
	for name, tc := range map[string]struct {
		mutate func(*Credential)
		text   string
	}{
		"blank channel":    {func(c *Credential) { c.Channel = " " }, "channel is empty"},
		"blank open id":    {func(c *Credential) { c.OpenID = "" }, "open id is empty"},
		"open id too long": {func(c *Credential) { c.OpenID = strings.Repeat("x", MaxOpenIDBytes+1) }, "open id is"},
		"blank secret":     {func(c *Credential) { c.Secret = "  " }, "credential secret is empty"},
	} {
		t.Run(name, func(t *testing.T) {
			c := good
			tc.mutate(&c)
			err := c.Validate()
			if !errors.Is(err, ErrRequestInvalid) || !strings.Contains(err.Error(), tc.text) {
				t.Fatalf("Validate = %v, want ErrRequestInvalid containing %q", err, tc.text)
			}
		})
	}
	if err := (Verified{Channel: "wechat", OpenID: "open-1"}).Validate(); err != nil {
		t.Fatalf("verified baseline rejected: %v", err)
	}
	if err := (Verified{OpenID: "open-1"}).Validate(); !errors.Is(err, ErrIdentityDenied) || !strings.Contains(err.Error(), "verifier returned an empty channel") {
		t.Fatalf("verified without channel = %v", err)
	}
	if err := (Verified{Channel: "wechat"}).Validate(); !errors.Is(err, ErrIdentityDenied) || !strings.Contains(err.Error(), "verifier returned an empty open id") {
		t.Fatalf("verified without open id = %v", err)
	}
}
