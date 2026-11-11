// Package account is the account and role directory: it maps a channel
// identity to an account, owns the roles under it, and issues role session
// tokens.
//
// This is a rewrite. The implementation it replaces had, among others, these
// defects — and each one is why something here looks the way it does:
//
//   - Login verified nothing. It accepted {channel, open_id} and returned the
//     account, and selecting a role then returned a valid session token for
//     any player under it. The only gate was one repo-wide shared secret
//     whose signature covered the body with no timestamp or nonce, so a
//     captured request replayed forever — and in a non-production
//     configuration with no secret set the gate was off entirely. That is
//     full account takeover. Here identity verification is a required
//     interface with **no default**: a service that cannot verify does not
//     start.
//   - The default player-id allocator was a per-process counter starting at
//     one, selected whenever Redis was absent but Mongo was not, and role
//     creation wrote with a replace that matched and overwrote an existing
//     document. Restart the service, create a role, and an existing player's
//     record was gone. Here there is no default allocator, and creation is
//     insert-only.
//   - The per-server role limit was a read-count-write with a deliberately
//     non-unique index behind it, so two concurrent creates both passed.
//     Here one insert-only slot owns a durable creation plan.
//   - A separate upsert entry point bypassed name reservation, account
//     existence and the role limit, and would reparent a role to another
//     account or rename it while orphaning the old reservation. There is no
//     such entry point here; each mutation is a narrow, constrained operation.
//   - Name reservation and the role record were two collection writes with no
//     transaction, and the compensating release dropped its error, so a crash
//     between them burned the name permanently. Here reservation is the
//     two-phase directory claim, linked to a durable slot intent for recovery.
//   - Redis role updates compared a re-marshalled value against stored bytes,
//     so adding one field wedged every role write across a rolling deploy.
//     Here state goes through versionstore, which compares versions.
package account

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/tjbdwanghaibo/roost-core/infra/base/errcode"
	"github.com/tjbdwanghaibo/roost-core/infra/storage/versionstore"
	"github.com/tjbdwanghaibo/roost-core/service/directory"
)

// Error codes.
//
// Written out rather than derived with iota: in the implementation this
// replaces the error code evaluated to 2 because CodeOK occupied the first
// const spec, while every decode failure returned 1 — a value no constant
// named. A client matching on the error code never matched.
const (
	CodeOK int32 = 0

	CodeIdentityInvalid int32 = 560101
	CodeIdentityDenied  int32 = 560102
	CodeAccountMissing  int32 = 560103
	CodeServerInvalid   int32 = 560104
	CodeServerClosed    int32 = 560105
	CodeNameInvalid     int32 = 560106
	CodeNameTaken       int32 = 560107
	CodeRoleLimit       int32 = 560108
	CodeRoleMissing     int32 = 560109
	CodeNotPermitted    int32 = 560110
	CodeSessionInvalid  int32 = 560111
	CodeRangeInvalid    int32 = 560112
	CodeConflict        int32 = 560113
	// CodeRequestInvalid reports a request this service could not even read:
	// a wire frame that failed to decode. It is last in the segment because
	// it was added when the generated transport arrived — the generated
	// handler needs one coded error to answer a frame it cannot decode, and
	// answering that with CodeInternal would report a caller's malformed
	// request as a server fault.
	CodeRequestInvalid int32 = 560114
	// CodeNotResolvable and CodeAdminNoteRequired belong to the operator
	// surface in admin.go. They are separate codes because
	// they are separate answers: "this slot is not in a state a human may
	// release" and "you did not say why".
	CodeNotResolvable     int32 = 560115
	CodeAdminNoteRequired int32 = 560116
	// CodeServerMergeCycle belongs to server merge (merge.go): an alias
	// that would point at a server which is itself merged, or that already
	// does in stored data. Distinct from CodeServerInvalid because the
	// servers exist and the refusal is about their arrangement.
	CodeServerMergeCycle int32 = 560117
	// CodeTransferStale 与 CodeTransferInProgress 属于单个角色换服（transfer.go）。前者是“调用方手里的
	// 绑定或换服 ID 已经不是当前的”（epoch 对不上、发起的服不是承载服、这个 ID 已经了结），后者是
	// “这个角色正有另一次换服在途”。与 CodeConflict 分开：CodeConflict 是可以原样重试的存储竞争，这两个
	// 要调用方先重读绑定（RoleBinding）再决定。
	CodeTransferStale      int32 = 560118
	CodeTransferInProgress int32 = 560119
)

var (
	ErrIdentityInvalid = errcode.Define(CodeIdentityInvalid, "account: identity is invalid", "")
	// ErrIdentityDenied reports that the channel refused the credential. It
	// is distinct from ErrIdentityInvalid so a malformed request and a
	// rejected one are not the same event to an operator.
	ErrIdentityDenied = errcode.Define(CodeIdentityDenied, "account: identity was denied by the channel", "")
	ErrAccountMissing = errcode.Define(CodeAccountMissing, "account: account not found", "")
	ErrServerInvalid  = errcode.Define(CodeServerInvalid, "account: server is invalid", "")
	ErrServerClosed   = errcode.Define(CodeServerClosed, "account: server is not open", "")
	ErrNameInvalid    = errcode.Define(CodeNameInvalid, "account: role name is invalid", "")
	ErrNameTaken      = errcode.Define(CodeNameTaken, "account: role name is taken", "")
	ErrRoleLimit      = errcode.Define(CodeRoleLimit, "account: role limit reached for this server", "")
	ErrRoleMissing    = errcode.Define(CodeRoleMissing, "account: role not found", "")
	ErrNotPermitted   = errcode.Define(CodeNotPermitted, "account: caller does not own this role", "")
	ErrSessionInvalid = errcode.Define(CodeSessionInvalid, "account: session token is invalid", "")
	ErrRangeInvalid   = errcode.Define(CodeRangeInvalid, "account: range is invalid", "")
	ErrConflict       = errcode.Define(CodeConflict, "account: conflict", "")
	// ErrRequestInvalid reports a request that could not be decoded. The
	// generated transport returns it for a frame it cannot read, which is the
	// one refusal the transport itself has to be able to make.
	ErrRequestInvalid = errcode.Define(CodeRequestInvalid, "account: request is invalid", "")
	// ErrNotResolvable reports that a role slot is not in a state an operator
	// may release: there is no pending plan, the plan already published its
	// role, the slot predates creation identities, or the plan still holds
	// its own name and can complete by an ordinary same-name CreateRole.
	ErrNotResolvable = errcode.Define(CodeNotResolvable, "account: slot is not in a resolvable state", "")
	// ErrAdminNoteRequired reports a missing or oversized operator note.
	ErrAdminNoteRequired = errcode.Define(CodeAdminNoteRequired, "account: an operator note is required", "")
	// ErrServerMergeCycle reports a merge alias with more than one hop: a
	// merge whose target is itself merged (or would form a cycle), or a stored
	// alias that already points at a merged server. Aliases are flattened on
	// write (MergeServer), so resolution only ever follows one hop; meeting a
	// second one means the records were edited by hand or two merges raced,
	// and the role is refused rather than routed to a guess.
	ErrServerMergeCycle = errcode.Define(CodeServerMergeCycle, "account: server merge alias would form a chain or cycle", "")
	// ErrTransferStale：调用方对这个角色绑定的认识已经过期——BeginRoleTransfer 的 ExpectEpoch 不是当前
	// BindEpoch、FromSID 不是当前承载服、换服 ID 已经了结；CommitRoleTransfer 遇到已中止或不认识的 ID；
	// AbortRoleTransfer 遇到已提交的 ID。返回它的接口同时带回当前的 RoleBinding，调用方按它处理。
	ErrTransferStale = errcode.Define(CodeTransferStale, "account: role binding or transfer id is stale", "")
	// ErrTransferInProgress：这个角色正有另一次换服在途（另一个 ID，或同一个 ID 但目标不同）。
	ErrTransferInProgress = errcode.Define(CodeTransferInProgress, "account: another role transfer is in progress", "")
)

// Error maps an error to the code and reason a client sees.
//
// It matches the servicerpc.Error convention (infra/network/servicerpc), which is what an RPC
// envelope is filled from.
//
// It is short because the sentinels carry their own codes: errcode.ClientError
// finds the code through any depth of fmt.Errorf wrapping, so there is no
// per-sentinel table here to keep in step with the one above. A hand-written
// switch over every sentinel is the shape this replaces, and it is a second
// list that a newly added error silently falls off.
//
// Two behaviours are relied on rather than incidental:
//
//   - When an error wraps two coded errors with "%w: %w", the FIRST one wins.
//     That is what makes a refusal which wraps a caller's own reason report
//     the refusal, which is what the client has to be told.
//   - An error this package cannot classify reports errcode.CodeInternal, not
//     a code of its own. Answering "the store failed" for an unclassified bug
//     is a guess presented as a diagnosis — and a catch-all code of that shape
//     is what the previous constant block had, with nothing able to produce it
//     deliberately.
func Error(err error) (int32, string) {
	if err == nil {
		return CodeOK, ""
	}
	// versionstore.ErrConflict is a FOREIGN sentinel: it belongs to versionstore
	// and carries no code of this package's, so errcode.ClientError would
	// report it as CodeInternal. Compare-and-set exhaustion under contention
	// is a real, retryable outcome a caller can act on, and "server error" is
	// not an answer it can act on — so it is mapped deliberately here.
	//
	// This is the only kind of case a table is still needed for, and it is
	// why Error is a function rather than a bare call to errcode.
	if errors.Is(err, versionstore.ErrConflict) {
		return errcode.ClientError(ErrConflict)
	}
	return errcode.ClientError(err)
}

// Code is Error without the reason, for callers that only switch on the code.
func Code(err error) int32 {
	code, _ := Error(err)
	return code
}

// Bounds. Every listing is bounded, and no bound can be bypassed by leaving a
// limit at zero.
const (
	MaxPageSize = 100
	// MaxProfileBytes bounds the opaque per-role profile. Progression fields
	// are game concepts, so they are carried as an opaque blob rather than
	// baked into this schema the way level, power, alliance and avatar were.
	MaxProfileBytes = 4096
)

// Channel names an identity provider: a store, a platform, a self-hosted
// login. It is opaque here.
type Channel string

// Identity is a credential presented for login.
type Identity struct {
	Channel Channel `json:"channel"`
	// OpenID is the channel's identifier for the user.
	OpenID string `json:"open_id"`
	// Credential is what the channel verifies — a platform token, a receipt,
	// a signature. This service never interprets it; it hands it to the
	// IdentityVerifier. It is never logged and never stored.
	Credential string `json:"credential"`
}

func (i Identity) Validate() error {
	if strings.TrimSpace(string(i.Channel)) == "" {
		return fmt.Errorf("%w: channel is empty", ErrIdentityInvalid)
	}
	if strings.TrimSpace(i.OpenID) == "" {
		return fmt.Errorf("%w: open id is empty", ErrIdentityInvalid)
	}
	return nil
}

// AccountID is the stable account key, derived from the verified identity.
// Derivation is deterministic so the same verified identity always maps to the
// same account without a lookup table.
func (i Identity) AccountID() string {
	channel := strings.ToLower(strings.TrimSpace(string(i.Channel)))
	return strings.NewReplacer("%", "%25", ":", "%3A").Replace(channel) + ":" + strings.TrimSpace(i.OpenID)
}

// Account is one player's account across servers.
type Account struct {
	ID      string  `json:"id"`
	Channel Channel `json:"channel"`
	OpenID  string  `json:"open_id"`
	// Banned blocks login and role creation. It is here rather than in a
	// separate service because login is the only place it can be enforced
	// cheaply.
	Banned          bool  `json:"banned"`
	CreatedAtUnix   int64 `json:"created_at_unix"`
	LastLoginAtUnix int64 `json:"last_login_at_unix"`
	// AdminNote and AdminActionAtUnix record the last operator intervention
	// on this account (Admin.ResolvePendingCreation). They live here rather
	// than on the slot because the intervention removes the slot; the account
	// is the record that remains for the next reader.
	AdminNote         string `json:"admin_note,omitempty"`
	AdminActionAtUnix int64  `json:"admin_action_at_unix,omitempty"`
}

// ServerStatus says whether a server accepts new roles.
type ServerStatus string

const (
	ServerOpen        ServerStatus = "open"
	ServerMaintenance ServerStatus = "maintenance"
	ServerFull        ServerStatus = "full"
	ServerClosed      ServerStatus = "closed"
)

// acceptsNewRoles reports whether a role may be created on this server.
func (s ServerStatus) acceptsNewRoles() bool { return s == ServerOpen }

// GameServer is one game server a role can live on.
//
// It is not the generated Server in this package, which is the process host
// for the account service. The two would collide as type names, and a game
// server record and a service process are about as different as two things in
// one package get — so this one carries the longer name.
type GameServer struct {
	ID     int32        `json:"id"`
	Name   string       `json:"name"`
	Status ServerStatus `json:"status"`
	// Region groups servers for display; opaque here.
	Region        string `json:"region"`
	UpdatedAtUnix int64  `json:"updated_at_unix"`

	// 别名合服（Admin.MergeServer）。下面四个字段只由 MergeServer / UnmergeServer 写，UpsertServer
	// 原样保留它们。
	//
	// MergedInto 非 0 表示本服已并入该服：本服角色的身份（PlayerID、ServerID、slot）不变，由
	// MergedInto 的 game 进程承载（Role.ServingServerID）。0 表示没有合服。记录永远只有一跳：并入时
	// 把指向本服的别名一并改指新的承载服（拍平），承载服自己不能再有 MergedInto。
	MergedInto int32 `json:"merged_into,omitempty"`
	// MergedFrom 是并入本服、由本服承载的服（按 ID 排序），供拍平与 show-server 用。它是辅助索引：
	// 是否并入以对方记录的 MergedInto 为准，索引里多出的项（并发改动留下）在拍平时按 MergedInto 核对后跳过。
	MergedFrom []int32 `json:"merged_from,omitempty"`
	// MergedAtUnix 与 MergeNote 记录最近一次合服 / 解除合服的操作时间（系统时间）与操作者备注：
	// account 没有独立的审计日志，运维记录写在记录本身（与 ResolvePendingCreation 相同）。
	MergedAtUnix int64  `json:"merged_at_unix,omitempty"`
	MergeNote    string `json:"merge_note,omitempty"`
}

// clone 复制 MergedFrom，调用方拿到的记录不与存储共享切片。
func (g GameServer) clone() GameServer {
	g.MergedFrom = append([]int32(nil), g.MergedFrom...)
	return g
}

// Role is one character under an account on one server.
type Role struct {
	// CreationID links the role to its durable creation intent; every role
	// carries one, and a role without it is refused as malformed. It is not a
	// session token or a client request identity.
	CreationID string `json:"creation_id,omitempty"`
	// PlayerID is allocated by the injected PlayerIDAllocator, never by this
	// service. It is the role's identity everywhere else in the system.
	PlayerID  int64  `json:"player_id"`
	AccountID string `json:"account_id"`
	// ServerID 是当前绑定服：建角时写定，只有 CommitRoleTransfer（transfer.go，单个角色换服）会改它，
	// slot 按它（accountID@ServerID）。没有换过服的角色它就是建角服；account 不另存建角服（名字目录
	// 不分服，换服不改名）。它与别名合服无关：合服不改 ServerID，绑定服并入另一服时由 ServingServerID
	// 给出承载服。显示原服标识、日志、按服的业务数据归属用它；决定由哪个 game 进程承载、往哪里路由用
	// ServingServerID。
	ServerID int32 `json:"server_id"`
	// ServingServerID 是承载服：原服没有合服时等于 ServerID，原服已并入另一服（GameServer.MergedInto）
	// 时是那个服。它不是存储字段：存储里的角色记录总是 0，ValidateSession、SelectRole、CreateRole、
	// UpdateProfile 返回前由 account 唯一的解析点（servingServer）填入，经总线返回时随响应传递。
	// 消费者（Gate 与嵌入 TCP 的认证器）一律用它决定路由与驻留。
	ServingServerID int32 `json:"serving_server_id,omitempty"`
	// Name is the display name; uniqueness is enforced on its normalized form.
	Name string `json:"name"`
	// Profile is opaque game state — level, power, guild, avatar. It is a
	// blob because those are gameplay concepts: baking them into this schema
	// is what made the directory carry one game's progression model.
	Profile          []byte `json:"profile,omitempty"`
	CreatedAtUnix    int64  `json:"created_at_unix"`
	LastLoginAtUnix  int64  `json:"last_login_at_unix"`
	LastLogoutAtUnix int64  `json:"last_logout_at_unix"`

	// 单个角色换服（transfer.go）。下面四个字段只由 Begin / Commit / AbortRoleTransfer 写。
	//
	// BindEpoch 是绑定的代数：建角时 0，每次换服提交加 1（Begin 与 Abort 不改）。调用方凭它说明
	// “我看到的是哪一版绑定”（TransferRequest.ExpectEpoch），slot 也记着自己属于哪一代（Slot.BindEpoch）。
	BindEpoch uint64 `json:"bind_epoch,omitempty"`
	// TransferSeq 是换服尝试的序号：建角时 0，每受理一次发起加 1，每记下一次中止（含中止一个没发起过的
	// ID）再加 1，只增不减；提交、被拒绝的请求、同一个在途 ID 的重发不改它。发起必须带上读到的值
	// （TransferRequest.ExpectSeq）：一个请求发出之后只要有任何一次发起或中止落了地，它就不再能发起。
	// 这一条不靠 LastTransfer——那里只留最近一次了结的换服，更早中止的 ID 在记录上已经看不到。
	// 它与 BindEpoch 分开：BindEpoch 说的是绑定换没换（会话 claim、slot、游戏侧“换回来了”都按它判断），
	// 中止不改绑定，所以不能动它。
	TransferSeq uint64 `json:"transfer_seq,omitempty"`
	// Transfer 非空表示换服在途：它是“这个角色正在换服”的唯一持久事实。在途期间 ServerID 仍是源服，
	// 角色照常可登录（是否放行由 game 侧按 TransferClaim 决定）。
	Transfer *RoleTransfer `json:"transfer,omitempty"`
	// LastTransfer 是最近一次了结（提交或中止）的换服，Commit / Abort 重发时据此回放。
	LastTransfer *RoleTransferRecord `json:"last_transfer,omitempty"`
}

// clone transfers the mutable profile to the caller without exposing storage.
// 换服的两条记录也复制一份：存储里的值从不原地改，调用方拿到的也不与存储共享。
func (r Role) clone() Role {
	r.Profile = append([]byte(nil), r.Profile...)
	r.Transfer = cloneOf(r.Transfer)
	r.LastTransfer = cloneOf(r.LastTransfer)
	return r
}

func cloneOf[T any](value *T) *T {
	if value == nil {
		return nil
	}
	copied := *value
	return &copied
}

// RoleTransfer 是一次在途的换服（Role.Transfer）。
type RoleTransfer struct {
	// ID 由发起方给出，一次换服一个，了结之后不要复用。account 拒绝重新发起刚了结的那个 ID；更早的
	// 请求由 Role.TransferSeq 挡住（它们带的序号已经过期），不靠记住每一个用过的 ID。
	ID string `json:"id"`
	// From 是发起时的绑定服，To 是目标服。
	From int32 `json:"from"`
	To   int32 `json:"to"`
	// Epoch 是发起时的 BindEpoch；提交之后角色的 BindEpoch 是 Epoch+1。
	Epoch         uint64 `json:"epoch"`
	StartedAtUnix int64  `json:"started_at_unix"`
	// DeadlineUnix 是发起方给的期限（系统时间）。一期 account 只记录它：不自动中止（account 不知道源服
	// 卸载到了哪一步），也不拒绝迟到的提交（中途停下一律向前恢复）；过期的换服由运维或游戏侧模块中止。
	DeadlineUnix int64  `json:"deadline_unix"`
	Note         string `json:"note,omitempty"`
}

// RoleTransferRecord 是最近一次了结的换服（Role.LastTransfer）。
type RoleTransferRecord struct {
	ID   string `json:"id"`
	From int32  `json:"from"`
	To   int32  `json:"to"`
	// Committed 为 true 是已提交（角色绑定到 To），false 是已中止（仍绑定 From）。
	Committed bool `json:"committed"`
	// Epoch 是了结之后角色的 BindEpoch：提交时是新的一代，中止时不变。提交的收尾按它认旧 slot。
	Epoch  uint64 `json:"epoch"`
	AtUnix int64  `json:"at_unix"`
	// Note 是了结时的说明：中止的原因；提交时是发起时的备注，或人工提交时操作者的备注。
	Note string `json:"note,omitempty"`
	// Settled 为 true 表示这次换服在 slot 上的收尾已经做过（提交：清掉新服 slot 的占位标记、删掉旧服的
	// slot；中止：删掉目标服的占位）。角色记录先定结果、slot 后收尾，两步之间停下时它是 false：重发的
	// Commit / Abort、下一次 SelectRole 或 Begin 读到就补完。
	//
	// 对已中止的换服，它只说明“中止当时撤过一次”，不说明以后不会再冒出占位：一个停在占位写入之前的
	// Begin 可能在收尾之后才把占位写下去。所以上面那几个入口对已中止的记录每次都再看一眼目标服
	// （settleTransfer），不因为 Settled 跳过。
	Settled bool `json:"settled,omitempty"`
}

// RoleBinding 是给 game 的窄读模型：这个角色现在绑定在哪个服、由哪个服承载、是第几代绑定、是否正在换服。
// 换服的四个接口都返回它；返回 ErrTransferStale / ErrTransferInProgress 时也带回当前值。
type RoleBinding struct {
	PlayerID int64 `json:"player_id"`
	// ServerID 是当前绑定服，ServingServerID 是承载服，含义同 Role 的同名字段。
	ServerID        int32  `json:"server_id"`
	ServingServerID int32  `json:"serving_server_id"`
	BindEpoch       uint64 `json:"bind_epoch"`
	// TransferSeq 是换服尝试的序号（Role.TransferSeq）。发起时原样带回去（TransferRequest.ExpectSeq）。
	TransferSeq uint64 `json:"transfer_seq"`
	// Transfer 非空表示换服在途（此时 ServerID 仍是源服）。
	Transfer     *RoleTransfer       `json:"transfer,omitempty"`
	LastTransfer *RoleTransferRecord `json:"last_transfer,omitempty"`
}

// TransferRequest 是 BeginRoleTransfer 的参数。
type TransferRequest struct {
	// ID 是这次换服的标识，必填，同一个 ID 重发幂等。
	ID string `json:"id"`
	// FromSID 是发起方认为的承载服（源服进程自己的 sid），To 是目标服。
	FromSID int32 `json:"from_sid"`
	To      int32 `json:"to"`
	// ExpectEpoch 是发起方看到的 BindEpoch。
	ExpectEpoch uint64 `json:"expect_epoch"`
	// ExpectSeq 是发起方看到的 TransferSeq（RoleBinding.TransferSeq）。与 ExpectEpoch 一样必须是当前值，
	// 否则答 ErrTransferStale 并带回当前绑定：读到绑定之后有别的发起或中止落了地，这个请求已经过期，
	// 重读绑定再发起。0 不是“不检查”。同一个在途 ID 的重发不看这两个字段。
	ExpectSeq uint64 `json:"expect_seq"`
	// TTLSeconds 决定 RoleTransfer.DeadlineUnix；0 取 DefaultTransferTTL，负数或超过 MaxTransferTTL 拒绝。
	TTLSeconds int64 `json:"ttl_seconds,omitempty"`
	// Note 可空，长度上限同 MaxAdminNoteBytes。
	Note string `json:"note,omitempty"`
}

// Session is what a verified role selection produces.
type Session struct {
	PlayerID  int64  `json:"player_id"`
	AccountID string `json:"account_id"`
	// ServerID 是角色的绑定服，ServingServerID 是承载服，含义同 Role 的同名字段。
	ServerID        int32  `json:"server_id"`
	ServingServerID int32  `json:"serving_server_id"`
	Token           string `json:"token"`
	ExpiresAtUnix   int64  `json:"expires_at_unix"`
	// Transfer 非空表示这个角色换服在途（Role.Transfer）。会话照常签发：握手被拒时客户端拿不到原因，
	// 是否放行、怎么提示由登录入口与 game 的登录端点决定。
	Transfer *RoleTransfer `json:"transfer,omitempty"`
}

// ServerIDClaim 是游戏网关的认证器把 ValidateSession 返回的承载服（Role.ServingServerID）记进
// gateway.Principal.Claims 用的键（值是十进制 sid）。
//
// 写的一方是认证器（game-demo 的 internal/access/player/tcp/auth.go），读的一方是登录端点
// （game/controllers/player/enter_game.go 的 BoundServerID），后者据此判断这个玩家是不是由本服承载，
// 比较逻辑与合服前相同：没有合服时承载服就是原服。两边分属传输包与控制器包，后者不能引用前者，
// 所以键放在两边本来就依赖的 account 包里：它说的正是 account 返回的那个承载服。
const ServerIDClaim = "server_id"

// OriginServerIDClaim 是认证器把角色的绑定服（Role.ServerID；别名合服里说的“原服”）记进 Principal.Claims
// 用的键（十进制 sid），供业务显示原服标识（例如名字后缀“[A]”，显示规则由业务决定）与按原服归属的
// 数据使用。路由与驻留不读它，读 ServerIDClaim。没有合服时两个值相同。
const OriginServerIDClaim = "origin_server_id"

// RoleCreatedAtClaim 是认证器把 ValidateSession 返回的 Role.CreatedAtUnix（十进制 Unix 秒）记进
// gateway.Principal.Claims 用的键。登录端点用它作为 mail.BroadcastProfile.RegisteredAtUnix 调
// Mail.PullBroadcasts：全服邮件的内置条件是“注册时间早于发送时间”，建角之后才发出的补偿才属于这个
// 角色。与 ServerIDClaim 一样放在两边都依赖的 account 包里。
const RoleCreatedAtClaim = "role_created_at"

// BindEpochClaim 是认证器把 ValidateSession 返回的 Role.BindEpoch（十进制）记进 Principal.Claims 用的键。
// 没换过服的角色是 "0"。game 侧拿它与自己记下的绑定代数比较：claim 是认证那一刻的快照，代数落后说明
// 这个会话是换服之前签发的。
const BindEpochClaim = "bind_epoch"

// TransferClaim 是认证器在角色换服在途（Role.Transfer 非空）时记进 Principal.Claims 的键，值由
// TransferClaimValue 给出、ParseTransferClaim 读回；没有换服时不写这个键。认证器只负责记下，拒绝由登录
// 端点给出（握手失败客户端得不到原因）。
const TransferClaim = "transfer"

// TransferClaimValue 是 TransferClaim 的值："<目标 sid>:<换服 ID>"。sid 在前：ID 由发起方给出，可以含冒号。
func TransferClaimValue(transfer RoleTransfer) string {
	return strconv.FormatInt(int64(transfer.To), 10) + ":" + transfer.ID
}

// ParseTransferClaim 读回 TransferClaimValue 写的值；格式不对时 ok 为 false。
func ParseTransferClaim(value string) (to int32, id string, ok bool) {
	head, id, found := strings.Cut(value, ":")
	sid, err := strconv.ParseInt(head, 10, 32)
	if !found || err != nil || sid <= 0 || id == "" {
		return 0, "", false
	}
	return int32(sid), id, true
}

// Slot records that an account occupies its role allowance on one server.
//
// It exists as its own record — rather than being derived by counting roles —
// because the limit has to be enforced by a single atomic claim. Counting is
// what let two concurrent creates both read zero and both insert.
type Slot struct {
	AccountID string `json:"account_id"`
	ServerID  int32  `json:"server_id"`
	// PlayerID is filled once the role exists. It is zero for the window
	// between claiming the slot and inserting the role, which is what makes
	// an abandoned claim identifiable.
	PlayerID int64 `json:"player_id,omitempty"`
	// Creation is retained through publication and identifies retries after
	// an uncertain write. A slot without it is malformed and refused.
	//
	// A pending plan leaves the slot by one of three doors, all identity- and
	// version-fenced deletes: a definite pre-admission refusal (name taken,
	// foreign allocator id), a create request — same name or another — that
	// finds the name COMMITTED to another owner (the plan can never commit
	// it), or an operator's
	// Admin.ResolvePendingCreation. Which door applies is the creation table
	// (decideCreation).
	Creation RoleCreation `json:"creation,omitempty"`

	// 单个角色换服（transfer.go）。
	//
	// TransferID 非空表示这个 slot 是某次换服在目标服的占位（BeginRoleTransfer 建），或者那次换服已经
	// 提交、收尾还没做完。占位带着角色的 PlayerID 与建角计划（照抄源服 slot），提交之后清掉 TransferID
	// 就是角色在新服的正式 slot。带着 TransferID 的 slot 对建角一律按“名额已占”回答（建角判定表的
	// slotTransferHold）。
	TransferID string `json:"transfer_id,omitempty"`
	// BindEpoch 是这个 slot 属于角色的哪一代绑定：建角的 slot 是 0，换服占位是提交之后的那一代。
	// roleReady 要求它与 Role.BindEpoch 相等，所以同一时刻只有一个 slot 可用；提交的收尾只删代数更旧的
	// slot，一个迟到的收尾不会删掉角色后来换回来的 slot。
	BindEpoch uint64 `json:"bind_epoch,omitempty"`
}

// RoleCreation is an immutable role plan plus the reservation admitted to
// insert it. After Admitted, storage errors are recovered forward, never by
// deleting a role whose Create may already have committed.
type RoleCreation struct {
	ID            string          `json:"id"`
	PlayerID      int64           `json:"player_id"`
	Name          string          `json:"name"`
	CreatedAtUnix int64           `json:"created_at_unix"`
	Claim         directory.Claim `json:"claim"`
	Admitted      bool            `json:"admitted"`
}
