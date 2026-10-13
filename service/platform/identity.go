package platform

import (
	"context"
	"fmt"
	"strings"
)

// Credential is what a player presents to obtain a session.
//
// It carries no player id. The player id is the OUTPUT of verification, not an
// input to it — that inversion is the whole fix. The implementation this
// replaces took a player id from the request and signed a token for it, with
// no credential anywhere in the request type, so anyone who could reach the
// endpoint could mint a session for any player.
type Credential struct {
	// Channel names the identity provider (a store, a platform SDK, a guest
	// flow). It selects which verifier runs.
	Channel string
	// OpenID is the channel's account identifier, as the client claims it.
	// It is claimed, not trusted: a verifier that echoes it back without
	// checking it with the channel is the vulnerability, not the design.
	OpenID string
	// Secret is the channel-issued proof — a token, a ticket, a signature.
	// Opaque here.
	Secret string
}

func (c Credential) Validate() error {
	if strings.TrimSpace(c.Channel) == "" {
		return fmt.Errorf("%w: channel is empty", ErrRequestInvalid)
	}
	if strings.TrimSpace(c.OpenID) == "" {
		return fmt.Errorf("%w: open id is empty", ErrRequestInvalid)
	}
	if len(c.OpenID) > MaxOpenIDBytes {
		return fmt.Errorf("%w: open id is %d bytes, limit %d", ErrRequestInvalid, len(c.OpenID), MaxOpenIDBytes)
	}
	if strings.TrimSpace(c.Secret) == "" {
		// A credential with no secret is the shape the replaced
		// implementation accepted. Refusing it here means the vulnerable call
		// does not typecheck.
		return fmt.Errorf("%w: credential secret is empty", ErrRequestInvalid)
	}
	return nil
}

// Verified is what a channel says about a credential.
//
// The player id comes from here and nowhere else.
type Verified struct {
	Channel string
	OpenID  string
}

func (v Verified) Validate() error {
	if strings.TrimSpace(v.Channel) == "" {
		return fmt.Errorf("%w: verifier returned an empty channel", ErrIdentityDenied)
	}
	if strings.TrimSpace(v.OpenID) == "" {
		return fmt.Errorf("%w: verifier returned an empty open id", ErrIdentityDenied)
	}
	return nil
}

// Verifier checks a credential with its channel.
//
// It is REQUIRED and has no default implementation, deliberately. A default
// that accepted anything would be indistinguishable, at the call site, from a
// configured verifier — and that is exactly how the replaced implementation
// came to have no authentication: the check was simply absent, and absence
// looks like a default.
//
// An implementation that cannot reach the channel must return a wrapped
// ErrVerifierDown rather than a denial, so an outage is not reported to
// players as a bad credential.
type Verifier interface {
	Verify(ctx context.Context, credential Credential) (Verified, error)
}

// VerifierFunc adapts a function to Verifier.
type VerifierFunc func(context.Context, Credential) (Verified, error)

// Verify implements Verifier.
func (f VerifierFunc) Verify(ctx context.Context, credential Credential) (Verified, error) {
	return f(ctx, credential)
}

// PlayerResolver maps a verified channel identity to a player id.
//
// Required and with no default, for the same reason as Verifier: the mapping
// from "this channel account" to "this player" is the account service's
// business, and a default here would either invent player ids or read them
// from the request.
type PlayerResolver interface {
	// Resolve returns the player id for a verified identity, creating it on
	// first sight if that is the caller's policy.
	Resolve(ctx context.Context, verified Verified) (int64, error)
}

// PlayerResolverFunc adapts a function to PlayerResolver.
type PlayerResolverFunc func(context.Context, Verified) (int64, error)

// Resolve implements PlayerResolver.
func (f PlayerResolverFunc) Resolve(ctx context.Context, verified Verified) (int64, error) {
	return f(ctx, verified)
}

var (
	_ Verifier       = VerifierFunc(nil)
	_ PlayerResolver = PlayerResolverFunc(nil)
)
