package gateway

import (
	"context"
	"errors"
	"testing"
	"time"

	coregateway "github.com/tjbdwanghaibo/cube-core/gateway"
	"github.com/tjbdwanghaibo/cube-core/security"
)

type session struct{ principal coregateway.Principal }

func (s *session) Principal() coregateway.Principal { return s.principal }
func (s *session) Reply(context.Context, any) error { return nil }
func (s *session) Close(error) error                { return nil }

func TestRateLimitByPlayerAndMessage(t *testing.T) {
	limiter := security.NewRateLimiter(security.RateLimitConfig{Capacity: 1, Refill: 1, Interval: time.Hour})
	endpoint := coregateway.Chain(coregateway.EndpointFunc(func(context.Context, coregateway.Session, coregateway.Request) (any, error) {
		return "ok", nil
	}), coregateway.RequireAuthenticated, RateLimit(limiter))
	s := &session{principal: coregateway.Principal{PlayerID: 1, SessionID: "s"}}
	request := coregateway.Request{MessageID: 10}
	if _, err := endpoint.Handle(context.Background(), s, request); err != nil {
		t.Fatal(err)
	}
	if _, err := endpoint.Handle(context.Background(), s, request); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("error=%v want ErrRateLimited", err)
	}
}

func TestRateLimitBackstopHoldsWithoutLimiter(t *testing.T) {
	// Regression: with a nil limiter the middleware used to skip its
	// defense-in-depth principal checks together with the rate limit, so a
	// chain missing RequireAuthenticated lost its only authentication
	// backstop. Disabling rate limiting must not widen the auth surface.
	endpoint := coregateway.Chain(coregateway.EndpointFunc(func(context.Context, coregateway.Session, coregateway.Request) (any, error) {
		return "ok", nil
	}), RateLimit(nil))
	request := coregateway.Request{MessageID: 10}
	if _, err := endpoint.Handle(context.Background(), nil, request); !errors.Is(err, coregateway.ErrUnauthenticated) {
		t.Fatalf("nil session error=%v want ErrUnauthenticated", err)
	}
	if _, err := endpoint.Handle(context.Background(), &session{}, request); !errors.Is(err, coregateway.ErrUnauthenticated) {
		t.Fatalf("anonymous principal error=%v want ErrUnauthenticated", err)
	}
	authed := &session{principal: coregateway.Principal{PlayerID: 1, SessionID: "s"}}
	if ret, err := endpoint.Handle(context.Background(), authed, request); err != nil || ret != "ok" {
		t.Fatalf("authenticated request: ret=%v err=%v", ret, err)
	}
}

func TestTimeoutAndRecover(t *testing.T) {
	var reported bool
	endpoint := coregateway.Chain(coregateway.EndpointFunc(func(ctx context.Context, _ coregateway.Session, request coregateway.Request) (any, error) {
		if request.MessageID == 1 {
			panic("boom")
		}
		<-ctx.Done()
		return nil, ctx.Err()
	}), Recover(func(context.Context, any) { reported = true }), Timeout(5*time.Millisecond))
	if _, err := endpoint.Handle(context.Background(), nil, coregateway.Request{MessageID: 1}); err == nil || !reported {
		t.Fatalf("panic error=%v reported=%v", err, reported)
	}
	if _, err := endpoint.Handle(context.Background(), nil, coregateway.Request{MessageID: 2}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout error=%v", err)
	}
}
