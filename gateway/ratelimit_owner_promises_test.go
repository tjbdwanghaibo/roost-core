// RR-20261005-NC-82：RateLimit 以 {PlayerID, MessageID} 为 key，一个玩家变化 MessageID
// 不能占满全局 key 表让其他玩家被限流。
package gateway

import (
	"context"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/security"
)

type ownerSession struct{ principal Principal }

func (s ownerSession) Principal() Principal           { return s.principal }
func (ownerSession) Reply(context.Context, any) error { return nil }
func (ownerSession) Close(error) error                { return nil }

func TestOnePlayerSprayingMessageIDsDoesNotRateLimitOthers(t *testing.T) {
	limiter := security.NewRateLimiter(security.RateLimitConfig{Capacity: 5, MaxKeys: 1000, IdleTTL: time.Hour})
	handled := 0
	endpoint := Chain(EndpointFunc(func(context.Context, Session, Request) (any, error) { handled++; return nil, nil }),
		Recover(nil), RequireAuthenticated, RateLimit(limiter))
	attacker := ownerSession{Principal{PlayerID: 1, SessionID: "a"}}
	for id := uint32(1); id <= 1000; id++ {
		_, _ = endpoint.Handle(context.Background(), attacker, Request{MessageID: id})
	}
	victim := ownerSession{Principal{PlayerID: 2, SessionID: "v"}}
	if _, err := endpoint.Handle(context.Background(), victim, Request{MessageID: 1}); err != nil {
		t.Fatalf("victim's first request = %v after player 1 sprayed 1000 message IDs (stats %+v)", err, limiter.Stats())
	}
}
