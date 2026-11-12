package account

import "context"

// The transport for Accounts is generated from the interface below.
//
//go:generate go run github.com/tjbdwanghaibo/roost-core/codegen/cmd/servicerpc -dir . -emit transport

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

	// GetServer returns one game server's record, merge fields included
	// (ErrServerInvalid when there is none). It is read-only and the record is
	// public listing information, so it needs no caller identity. A game
	// process asks it at start-up whether its own server was merged away
	// (wiring/account ServingGuardMod).
	GetServer(ctx context.Context, serverID int32) (server GameServer, err error)

	// ListServers 返回登记过的全部区服记录，按服号升序，至多 MaxServers 条（到了上限 UpsertServer 拒绝
	// 登记新的服号，所以这里从不截断）。只读；区服记录是登录前就公开的列表信息，不需要调用方身份。
	// 登录入口与清单服务（manifest）用它知道“有哪些服”；连接地址、负载与推荐不在 account 里。
	// 索引与记录的一致性见 servers.go。
	ListServers(ctx context.Context) (servers []GameServer, err error)

	// RoleOnServer 返回 accountID 在 serverID 上的角色（一个账号在一个服至多一个）；只读。没有角色、角色还
	// 没发布、角色已经换到别的服，都答 ErrRoleMissing（各种 slot 状态的回答见 Service.RoleOnServer）。
	// 登录入口用它告诉客户端“你在这个服上的角色是谁”：换设备、重装之后客户端不知道自己的 player_id，
	// 拿另一个名字 CreateRole 只会得到 ErrRoleLimit。信任模型与 SelectRole 相同：可信调用方提供 accountID。
	// 没有“列出账号的全部角色 / 全部区服”：存储是键值的，没有扫描；区服清单由登录入口的配置给出。
	RoleOnServer(ctx context.Context, accountID string, serverID int32) (role Role, err error)

	// 单个角色换服（transfer.go）。四个接口都在总线上：发起与提交的是源服的 game 进程（它知道自己什么
	// 时候停写、卸载完），冷准入时任何 game 进程都要读绑定。它们改的是一个角色自己的绑定，不是
	// UpsertServer / MergeServer 那样改变所有玩家能登录到哪里的控制面写；调用方可信的前提与本接口其余
	// 方法相同。运维的人工兜底走 accountctl（Admin.CommitRoleTransferWithNote）。

	// RoleBinding returns where a role is bound right now: its server, the
	// server that serves it, the bind epoch, the transfer seq, and the
	// transfer in flight or last settled. Read-only.
	RoleBinding(ctx context.Context, playerID int64) (binding RoleBinding, err error)

	// BeginRoleTransfer records a transfer of the role to request.To and
	// holds the account's slot there. The role stays bound to, and playable
	// on, its current server until CommitRoleTransfer. Idempotent per
	// request.ID. The request presents the bind epoch and the transfer seq
	// the caller read (RoleBinding); once any transfer was begun or aborted
	// after that read it is stale, so a replayed Begin cannot revive a
	// transfer that was aborted. A refusal with ErrTransferStale or
	// ErrTransferInProgress carries the current binding — also when this
	// transfer was aborted or committed while Begin was placing the hold.
	BeginRoleTransfer(ctx context.Context, playerID int64, request TransferRequest) (binding RoleBinding, err error)

	// CommitRoleTransfer rebinds the role to the transfer's target and bumps
	// its bind epoch. The caller guarantees the source no longer writes the
	// player. Idempotent per transferID; ErrTransferStale (with the current
	// binding) when that transfer was aborted or is not the one in flight.
	CommitRoleTransfer(ctx context.Context, playerID int64, transferID string) (binding RoleBinding, err error)

	// AbortRoleTransfer abandons a transfer: the role stays on its server,
	// the hold is released, and the id can neither commit nor begin again.
	// Every recorded abort advances the transfer seq, so no Begin sent
	// before it can be accepted afterwards. Idempotent per transferID;
	// ErrTransferStale (with the committed binding) when that transfer
	// already committed.
	AbortRoleTransfer(ctx context.Context, playerID int64, transferID string, reason string) (binding RoleBinding, err error)
}

var _ Accounts = (*Service)(nil)
