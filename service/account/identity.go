package account

import (
	"context"
	"errors"
	"fmt"
)

// IdentityVerifier checks a credential with whoever issued it.
//
// **There is no default implementation, and there must never be one.** In the
// implementation this replaces, verification was simply absent: login took a
// channel and an open id and returned the account, so anyone who could reach
// the endpoint could obtain a session token for any player. The absence was
// not a decision anybody made — it was a missing piece that looked like a
// default. Requiring the interface turns that into a startup failure instead
// of a silent hole.
//
// A verifier must fail closed. If it cannot reach the channel it returns an
// error; it must not return "verified" on a timeout.
type IdentityVerifier interface {
	// Verify checks the credential and returns the identity it attests to.
	//
	// The returned identity — not the submitted one — is what the account is
	// derived from, so a caller cannot claim someone else's open id by
	// presenting its own credential alongside another's identifier.
	Verify(ctx context.Context, identity Identity) (Verified, error)
}

// Verified is what a channel attests to.
type Verified struct {
	Channel Channel `json:"channel"`
	// OpenID is the identifier the channel confirms, which may differ from
	// the one submitted. The confirmed one wins.
	OpenID string `json:"open_id"`
}

func (v Verified) Validate() error {
	if v.Channel == "" || v.OpenID == "" {
		return fmt.Errorf("%w: verifier returned an empty identity", ErrIdentityInvalid)
	}
	return nil
}

// VerifierFunc adapts a function to IdentityVerifier.
type VerifierFunc func(ctx context.Context, identity Identity) (Verified, error)

func (f VerifierFunc) Verify(ctx context.Context, identity Identity) (Verified, error) {
	return f(ctx, identity)
}

// PlayerIDAllocator mints role ids.
//
// Also required with no default. The implementation this replaces defaulted to
// a per-process counter starting at one, and selected it whenever Redis was
// absent while Mongo was present — a durable store fed by ids that restarted
// from one on every boot, combined with a create that overwrote on collision.
// A restart followed by a role creation destroyed an existing player.
//
// An allocator must produce ids that are unique across every process and
// every restart. A counter is only acceptable if the counter itself is
// durable and shared.
type PlayerIDAllocator interface {
	// Allocate returns an unused player id for a role on serverID.
	Allocate(ctx context.Context, serverID int32) (int64, error)
}

// AllocatorFunc adapts a function to PlayerIDAllocator.
type AllocatorFunc func(ctx context.Context, serverID int32) (int64, error)

func (f AllocatorFunc) Allocate(ctx context.Context, serverID int32) (int64, error) {
	return f(ctx, serverID)
}

// NameValidator checks a proposed role name.
//
// Required with no default: length, character set and disallowed words are
// per-project policy, and the implementation this replaces compiled one
// project's rules in as constants.
type NameValidator interface {
	// Validate reports whether raw is acceptable as a display name. It runs
	// before normalization, so it sees exactly what the player typed.
	Validate(raw string) error
}

// NameValidatorFunc adapts a function to NameValidator.
type NameValidatorFunc func(raw string) error

func (f NameValidatorFunc) Validate(raw string) error { return f(raw) }

// ErrVerifierUnavailable is what a verifier returns when it cannot reach the
// channel. It exists so a caller can distinguish "the channel says no" from
// "we could not ask", which need different responses: the first is a client
// error, the second is a service outage.
var ErrVerifierUnavailable = errors.New("account: identity channel is unavailable")
