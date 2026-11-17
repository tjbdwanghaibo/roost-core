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
	// CredentialVersion 是渠道给的“凭证版本”（交接 C6 第 1 点，维护者 2026-11-18 选 B）：玩家的登录凭证
	// （密码、绑定的平台身份）每变一次，它就变一个值——密码版本号、最后改密时刻、凭证指纹都行，account
	// 只比较是否相等，不解释、不排序。空表示渠道不提供，登录的行为与没有这一项时相同。
	//
	// account 在 Login 时把它与账号记录上存的比较：存过、且这次不同，就把游戏账号 token 的代数加一
	// （与 RevokeGameTokens 同一个计数），此前签发的 gt2 全部失效；第一次见到只记录。所以它必须只在凭证
	// 真的变了时才变：每次登录都不同的值（会话 token、随机数）会让每次登录都吊销一次。它会落在账号记录上、
	// 经内部总线传给 Login 的调用方，不要放密码本身或能推出密码的东西。不超过 MaxCredentialVersionBytes。
	CredentialVersion string `json:"credential_version,omitempty"`
}

// MaxCredentialVersionBytes 是 Verified.CredentialVersion 的长度上限。超过的是渠道实现的错，登录答 ErrIdentityInvalid。
const MaxCredentialVersionBytes = 128

func (v Verified) Validate() error {
	if v.Channel == "" || v.OpenID == "" {
		return fmt.Errorf("%w: verifier returned an empty identity", ErrIdentityInvalid)
	}
	if len(v.CredentialVersion) > MaxCredentialVersionBytes {
		return fmt.Errorf("%w: verifier returned a credential version of %d bytes, limit %d", ErrIdentityInvalid, len(v.CredentialVersion), MaxCredentialVersionBytes)
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
