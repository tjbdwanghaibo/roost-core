package account

import "context"

// The transport for Accounts is generated from the interface below.
//
//go:generate go run github.com/tjbdwanghaibo/roost-core/codegen/cmd/servicerpc -dir .

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
// 不声明 affinity：账号角色列表按 accountID 竞争，全局名字目录按 name 竞争。
// CreateRole 以 account/server slot 的 insert-only 和名字预留共同保证唯一性。
// 总线调用方必须可信：accountID/gameSID 等由已认证的入口绑定，服务端校验关联关系，
// 不能把调用方自行填写的 accountID 当作身份认证。
//
//roost:rpc service_type=account capability=service.account
type Accounts interface {
	// Login verifies a channel identity and returns the account it maps to,
	// creating it on first sight. The channel is asked; the account id is
	// never read from the request.
	Login(ctx context.Context, identity Identity) (account Account, err error)

	// CreateRole starts or resumes one durable role plan for an account/server.
	// Same-name retries share its ID, including a lost success response;
	// a different name cannot consume a second role allowance.
	CreateRole(ctx context.Context, accountID string, serverID int32, name string) (role Role, err error)

	// SelectRole mints a session for a role the account owns. Ownership is
	// checked against the stored role; the trusted caller supplies accountID.
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
