package gateway

import (
	"context"
	"errors"
	"log"
	"log/slog"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/infra/base/security"
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

// 观察回调（reporter / 诊断）失败不能让 endpoint panic 突破稳定错误边界。
func TestRecoverIsolatesReporterFailure(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value any
	}{{"string", "reporter failed"}, {"error", errors.New("reporter failed")}, {"nil", nil}} {
		t.Run(tc.name, func(t *testing.T) {
			reports := 0
			endpoint := Recover(func(context.Context, any) { reports++; panic(tc.value) })(EndpointFunc(func(context.Context, Session, Request) (any, error) { panic("private endpoint detail") }))
			var escaped any
			var ret any
			var err error
			func() {
				defer func() { escaped = recover() }()
				ret, err = endpoint.Handle(context.Background(), nil, Request{})
			}()
			if escaped != nil || ret != nil || !errors.Is(err, ErrEndpointPanic) || reports != 1 {
				t.Fatalf("ret=%v err=%v escaped=%v reports=%d", ret, err, escaped, reports)
			}
		})
	}
}

type panicDiagnosticWriter struct{}

func (panicDiagnosticWriter) Write([]byte) (int, error) { panic("diagnostic sink failed") }

func TestRecoverIsolatesDiagnosticFailure(t *testing.T) {
	previous := slog.Default()
	previousOutput := log.Default().Writer()
	slog.SetDefault(slog.New(slog.NewTextHandler(panicDiagnosticWriter{}, nil)))
	// slog.SetDefault 同时替换标准 log 的 writer，恢复旧 defaultHandler 时不会自动恢复它。
	// 恶意诊断 sink 只属于本回归，不能泄漏给后续 TCP 启动与生命周期测试。
	t.Cleanup(func() { slog.SetDefault(previous); log.SetOutput(previousOutput) })
	endpoint := Recover(func(context.Context, any) { panic("report failed") })(EndpointFunc(func(context.Context, Session, Request) (any, error) { panic("endpoint") }))
	_, err := endpoint.Handle(context.Background(), nil, Request{})
	if !errors.Is(err, ErrEndpointPanic) {
		t.Fatalf("error=%v", err)
	}
}

func TestRecoverPreservesNormalContract(t *testing.T) {
	t.Run("nil_reporter", func(t *testing.T) {
		_, err := Recover(nil)(EndpointFunc(func(context.Context, Session, Request) (any, error) { panic(nil) })).Handle(context.Background(), nil, Request{})
		if !errors.Is(err, ErrEndpointPanic) {
			t.Fatalf("error=%v", err)
		}
	})
	t.Run("original_context_and_panic", func(t *testing.T) {
		type key struct{}
		ctx := context.WithValue(context.Background(), key{}, "request")
		reports := 0
		_, err := Recover(func(got context.Context, v any) {
			reports++
			if got != ctx || v != "endpoint" {
				t.Error("report context or panic lost")
			}
		})(EndpointFunc(func(context.Context, Session, Request) (any, error) { panic("endpoint") })).Handle(ctx, nil, Request{})
		if !errors.Is(err, ErrEndpointPanic) || reports != 1 {
			t.Fatalf("error=%v reports=%d", err, reports)
		}
	})
	t.Run("healthy", func(t *testing.T) {
		businessErr := errors.New("business")
		reports := 0
		ret, err := Recover(func(context.Context, any) { reports++ })(EndpointFunc(func(context.Context, Session, Request) (any, error) { return 42, businessErr })).Handle(context.Background(), nil, Request{})
		if ret != 42 || err != businessErr || reports != 0 {
			t.Fatalf("ret=%v err=%v reports=%d", ret, err, reports)
		}
	})
}

// RateLimit 以 {PlayerID, MessageID} 为 key：一个玩家变化 MessageID 不能占满全局 key 表让其他玩家被限流。

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
