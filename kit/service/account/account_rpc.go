package account

import "context"

// The transport for Accounts is generated from the interface below.
//
//go:generate go run github.com/tjbdwanghaibo/roost-codegen/cmd/servicerpc -dir .

// Accounts is the cross-process contract: what ANOTHER process may ask of the
// account service.
//
// The name shares a word with Config.Accounts, which is the versioned store of
// Account records. They are not the same kind of thing and Go keeps them in
// different namespaces — one is this file's exported interface, the other a
// field reached as s.cfg.Accounts — but it is worth saying out loud, because
// the sibling match package renamed its interface to Matchmaker precisely to
// avoid a collision, and that case was a genuine one: there, both names were
// types.
//
// UpsertServer is deliberately not here. Registering a game server, opening
// it, closing it and renaming it are control-plane writes: they change what
// every player can log in to. A game process has no business making them, and
// exposing them on the same bus subject as Login would mean any process that
// can reach the account service can close a server. It belongs with the
// operator surface.
//
// ValidateSession IS here, unlike platform's method of the same name, and the
// difference is not arbitrary: this one takes a context and reads the role,
// because it answers "is this token valid AND does this player still exist on
// this server" — a question about durable state that only the owner has.
// platform's validates a MAC and touches nothing, so it stays local.
//
// No method carries an affinity key. This package has TWO contention domains
// with different keys — an account's role list, keyed by account id, and a
// server's name index, keyed by server id — and CreateRole writes into both.
// No single routing key makes both contention-free, so routing on one would
// advertise a guarantee that does not hold for the other. What actually makes
// concurrent role creation safe is a versioned compare-and-set on the account
// and an insert-only name reservation, neither of which needs affinity.
//
//roost:rpc service_type=account capability=service.account
type Accounts interface {
	// Login verifies a channel identity and returns the account it maps to,
	// creating it on first sight. The channel is asked; the account id is
	// never read from the request.
	Login(ctx context.Context, identity Identity) (account Account, err error)

	// CreateRole creates one role for an account on a server. The name is
	// reserved insert-only, so two concurrent creations of one name cannot
	// both succeed.
	CreateRole(ctx context.Context, accountID string, serverID int32, name string) (role Role, err error)

	// SelectRole mints a session for a role the account owns. Ownership is
	// checked against the stored role rather than taken from the request.
	SelectRole(ctx context.Context, accountID string, playerID int64) (session Session, err error)

	// ValidateSession checks a session token and returns the role it belongs
	// to.
	ValidateSession(ctx context.Context, playerID int64, token string) (role Role, err error)

	// UpdateProfile replaces a role's opaque profile blob. The account id is
	// required, so a caller cannot write another account's role.
	UpdateProfile(ctx context.Context, accountID string, playerID int64, profile []byte) (role Role, err error)

	// MarkLogout records a logout time. The account id is required for the
	// same reason.
	MarkLogout(ctx context.Context, accountID string, playerID int64) (err error)
}

var _ Accounts = (*Service)(nil)
