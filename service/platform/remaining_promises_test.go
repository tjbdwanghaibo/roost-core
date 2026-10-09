package platform

import (
	"context"
	"errors"
	"github.com/tjbdwanghaibo/roost-core/infra/storage/versionstore"
	"testing"
	"time"
)

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
