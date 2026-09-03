// Package gateway contains reusable middleware for core/gateway boundaries
// (rate limiting, authentication requirements, request guards).
//
// Scope: middleware only. This is not a gateway server — no listener, no
// protocol termination, no routing, no session management; those live in the
// application's gateway service, which composes these middlewares.
package gateway

import (
	"context"
	"errors"
	"time"

	coregateway "github.com/tjbdwanghaibo/roost-core/gateway"
	"github.com/tjbdwanghaibo/roost-core/security"
)

var (
	ErrRateLimited   = errors.New("gateway: rate limited")
	ErrEndpointPanic = errors.New("gateway: endpoint panic")
)

// RateLimit applies the core token bucket by authenticated player and message
// ID. Authentication middleware should be placed before it in the chain; the
// principal checks here are a defense-in-depth backstop for the limit key and
// therefore run regardless of whether a limiter is configured, so disabling
// rate limiting can never widen the authentication surface.
func RateLimit(limiter *security.RateLimiter) coregateway.Middleware {
	return func(next coregateway.Endpoint) coregateway.Endpoint {
		return coregateway.EndpointFunc(func(ctx context.Context, session coregateway.Session, request coregateway.Request) (any, error) {
			if session == nil {
				return nil, coregateway.ErrUnauthenticated
			}
			principal := session.Principal()
			if !principal.Authenticated() {
				return nil, coregateway.ErrUnauthenticated
			}
			if limiter != nil && !limiter.Allow(security.RateLimitKey{OwnerID: principal.PlayerID, Action: request.MessageID}) {
				return nil, ErrRateLimited
			}
			return next.Handle(ctx, session, request)
		})
	}
}

// Timeout adds a boundary deadline only when the caller did not already set a
// stricter deadline.
func Timeout(timeout time.Duration) coregateway.Middleware {
	return func(next coregateway.Endpoint) coregateway.Endpoint {
		return coregateway.EndpointFunc(func(ctx context.Context, session coregateway.Session, request coregateway.Request) (any, error) {
			if ctx == nil {
				ctx = context.Background()
			}
			if timeout <= 0 {
				return next.Handle(ctx, session, request)
			}
			if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) <= timeout {
				return next.Handle(ctx, session, request)
			}
			callCtx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			return next.Handle(callCtx, session, request)
		})
	}
}

// Recover converts a boundary panic to an error and reports it without
// exposing panic details to the transport response.
func Recover(report func(context.Context, any)) coregateway.Middleware {
	return func(next coregateway.Endpoint) coregateway.Endpoint {
		return coregateway.EndpointFunc(func(ctx context.Context, session coregateway.Session, request coregateway.Request) (ret any, err error) {
			defer func() {
				if recovered := recover(); recovered != nil {
					if report != nil {
						report(ctx, recovered)
					}
					ret = nil
					err = ErrEndpointPanic
				}
			}()
			return next.Handle(ctx, session, request)
		})
	}
}
