package platform

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// U-0143 · C2（空洞测试）· nightly gap map kit `service/platform` 7/20。
//
// 对未记录订单的运维重开 / 场外结算与自动补发都要以 ErrOrderInvalid 拒绝且不
// 凭空创建订单；回调空载荷、空订单 id 在解析或读存储之前拒绝；玩家解析器交出
// 非正 id 时 AuthSession 不能签出 token；Redis 订单存储缺键前缀不能构造。

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
