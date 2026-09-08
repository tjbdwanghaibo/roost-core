package gateway

import (
	"context"
	"errors"
	"github.com/tjbdwanghaibo/roost-core/security"
	"testing"
	"time"
)

type session struct{ principal Principal }

func (s *session) Principal() Principal             { return s.principal }
func (s *session) Reply(context.Context, any) error { return nil }
func (s *session) Close(error) error                { return nil }

func TestRateLimitByPlayerAndMessage(t *testing.T) {
	limiter := security.NewRateLimiter(security.RateLimitConfig{Capacity: 1, Refill: 1, Interval: time.Hour})
	endpoint := Chain(EndpointFunc(func(context.Context, Session, Request) (any, error) {
		return "ok", nil
	}), RequireAuthenticated, RateLimit(limiter))
	s := &session{principal: Principal{PlayerID: 1, SessionID: "s"}}
	request := Request{MessageID: 10}
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
	endpoint := Chain(EndpointFunc(func(context.Context, Session, Request) (any, error) {
		return "ok", nil
	}), RateLimit(nil))
	request := Request{MessageID: 10}
	if _, err := endpoint.Handle(context.Background(), nil, request); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("nil session error=%v want ErrUnauthenticated", err)
	}
	if _, err := endpoint.Handle(context.Background(), &session{}, request); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("anonymous principal error=%v want ErrUnauthenticated", err)
	}
	authed := &session{principal: Principal{PlayerID: 1, SessionID: "s"}}
	if ret, err := endpoint.Handle(context.Background(), authed, request); err != nil || ret != "ok" {
		t.Fatalf("authenticated request: ret=%v err=%v", ret, err)
	}
}

func TestTimeoutAndRecover(t *testing.T) {
	var reported bool
	endpoint := Chain(EndpointFunc(func(ctx context.Context, _ Session, request Request) (any, error) {
		if request.MessageID == 1 {
			panic("boom")
		}
		<-ctx.Done()
		return nil, ctx.Err()
	}), Recover(func(context.Context, any) { reported = true }), Timeout(5*time.Millisecond))
	if _, err := endpoint.Handle(context.Background(), nil, Request{MessageID: 1}); err == nil || !reported {
		t.Fatalf("panic error=%v reported=%v", err, reported)
	}
	if _, err := endpoint.Handle(context.Background(), nil, Request{MessageID: 2}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout error=%v", err)
	}
}
