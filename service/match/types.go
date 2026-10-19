// Package match is matchmaking: subjects queue for a mode, the service groups
// them, and a group becomes a match exactly once.
//
// This is a redesign, not a port. The implementation it replaces had ten
// severe defects and — decisively — no production caller for its queue half,
// so there was no migration pressure to preserve its shape. Each decision
// here answers one of those defects:
//
//   - Ticket ids came off the wire and were used verbatim as identity, with no
//     per-subject uniqueness check. One client could queue the same player
//     twice and have it committed into two matches, or into one match twice.
//     Here the service mints ids, and a subject may hold one live ticket.
//   - Committing a match popped the queue, then wrote the match, then flipped
//     each ticket in a loop. A failure anywhere after the pop lost those
//     players: gone from the queue, still "waiting", with no reaper. Here the
//     commit is a single atomic step and the queue is only mutated by it.
//   - The version used for optimistic writes was a process-local counter
//     starting at one, so versions from different replicas — and from the same
//     replica after a restart — were incomparable, and a stale write was
//     dropped while the caller was told it succeeded. Here versions come from
//     the store and a refused write is an error.
//   - Server-generated ids were "ticket-1", "match-1" process sequences, so
//     two replicas minted identical ids and an idempotent enqueue could return
//     another player's ticket.
//   - Cancel, read and close carried no caller identity, over guessable ids.
//     Here every operation naming a ticket also names its subject.
//   - ExpiresAt was written and never read: nothing enforced a timeout. Here
//     expiry is enforced on read and by an explicit sweep.
//   - 已结束的票、比赛与排队请求记录保留 Config.Retention 后移除（维护者 2026-10-19 方案 A）。
//     整个队列状态是一个整体读写的值，只增不删时每次操作随历史越来越慢；
//     代价是重放与读历史只在保留期内有效。
//   - 一个分区（Queue.Partition）是一个整体读写的值；大队列按段位档 / 地区 / sid 组拆成多个分区，
//     每次操作只碰一个分区。一局票来自同一逻辑队列的几个分区（放宽匹配）时用 Store.CommitAcross，
//     它在一次多键比较并设置里原子地改这几个分区（维护者 2026-10-19 批准）；键名的 hash tag 是逻辑队列，
//     Redis Cluster 下同一逻辑队列的分区同槽。
//
// This is the domain half of the match service: types, error codes, the
// ticket state machine behind Store, the Redis store and the Grouping
// policies. It imports nothing from wiring. The other half — the Mod that
// reads configuration and registers the capability, the Matchmaker RPC
// surface with its generated client and server, the sweep loop — lives in
// wiring/match and calls into this package.
package match

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/tjbdwanghaibo/roost-core/infra/base/errcode"
	"github.com/tjbdwanghaibo/roost-core/infra/storage/versionstore"
)

// Error codes.
const (
	CodeOK int32 = 0

	CodeQueueInvalid   int32 = 550101
	CodeSubjectInvalid int32 = 550102
	CodeTicketInvalid  int32 = 550103
	CodeTicketMissing  int32 = 550104
	CodeAlreadyQueued  int32 = 550105
	CodeNotPermitted   int32 = 550106
	CodeTicketMatched  int32 = 550107
	CodeConflict       int32 = 550108
	// CodeRequestInvalid is what a malformed request reports — a payload the
	// transport could not decode.
	//
	// It reuses 550109, which briefly held a catch-all "store failed" code.
	// Reuse is safe here and would not be in every segment: nothing ever
	// emitted 550109, because this package had no error-to-code mapping at
	// all until one was added, so no client can have matched on it. Contrast
	// global's 570111, which WAS observable and is therefore left vacant.
	CodeRequestInvalid int32 = 550109
)

var (
	ErrQueueInvalid   = errcode.Define(CodeQueueInvalid, "match: queue is invalid", "")
	ErrSubjectInvalid = errcode.Define(CodeSubjectInvalid, "match: subject is invalid", "")
	ErrTicketInvalid  = errcode.Define(CodeTicketInvalid, "match: ticket is invalid", "")
	ErrTicketMissing  = errcode.Define(CodeTicketMissing, "match: ticket not found", "")
	// ErrAlreadyQueued reports that the subject already holds a live ticket.
	// A subject with two live tickets is what allowed one player to be
	// committed into two matches.
	ErrAlreadyQueued = errcode.Define(CodeAlreadyQueued, "match: subject is already queued", "")
	// ErrNotPermitted reports that the caller does not own the ticket. Every
	// operation naming a ticket also names its subject, so a guessable id is
	// not enough to act on someone else's ticket.
	ErrNotPermitted  = errcode.Define(CodeNotPermitted, "match: caller does not own this ticket", "")
	ErrTicketMatched = errcode.Define(CodeTicketMatched, "match: ticket is already matched", "")
	ErrConflict      = errcode.Define(CodeConflict, "match: conflict", "")
	// ErrRequestInvalid reports a request the transport could not decode.
	//
	// The generated transport expects every service to have it: a malformed
	// payload is a category every wire surface needs an answer for, and
	// answering with a code that means something else would mislead a client
	// that switches on it.
	ErrRequestInvalid = errcode.Define(CodeRequestInvalid, "match: request is invalid", "")
)

// Limits bound what a queue may hold. Every one of these is a bound the
// implementation this replaces did not have: its queue length was unbounded
// and each enqueue read every queued ticket.
const (
	// MaxQueueLength caps one queue. Reaching it rejects new tickets rather
	// than degrading every enqueue.
	MaxQueueLength = 10000
	// MaxGroupSize caps how many subjects one match may hold.
	MaxGroupSize = 64
	// MaxPageSize caps a listing.
	MaxPageSize = 200
	// DefaultTicketTTL is how long a ticket waits before it expires. Unlike
	// the implementation this replaces, this is enforced.
	DefaultTicketTTL = 5 * time.Minute
	// DefaultRetention 是已结束的票、比赛与排队请求记录的缺省保留期（Config.Retention，配置键 match.retention）。
	//
	// 取 10 分钟的理由：
	//   - 它覆盖的读者都是短窗口：匹配器的 Commit 重试（单次调用超时 3s，按秒重试）、客户端的排队重试
	//     （同一 requestID，秒级）、玩家轮询自己的票与成局成员（game-demo 的 PollMatch）。10 分钟比重试窗口长两个数量级，
	//     是缺省票期限 DefaultTicketTTL 的两倍：排队后掉线的玩家在票的整个等待期之后再过 5 分钟回来，仍能读到结果。
	//   - 更长的“这一局是谁”不由匹配服务回答：开局后业务自己保存 Match ID → 房间宿主 → 结算实体的路由
	//     （见 docs/framework/MOBA-INTEGRATION.md），匹配记录不是对局档案。
	//   - 代价是每次读写的状态大小：实测每名成局玩家约 580 字节 JSON（票 + 请求记录 + 比赛里的成员，不含 Payload），
	//     每秒成局 1 人的队列在 10 分钟里约多 0.35 MB，小于满队列（MaxQueueLength 张等待票）本身的状态。
	//     吞吐更高的队列可调小 match.retention，但不要短于匹配器与客户端的重试窗口。
	DefaultRetention = 10 * time.Minute
)

// SubjectKind says what kind of entity is queueing. It is opaque to this
// service: a subject is whatever the caller says it is.
type SubjectKind string

// Subject is who is queueing.
type Subject struct {
	Kind SubjectKind `json:"kind"`
	ID   int64       `json:"id"`
	// Score is what the grouping policy ranks on; its meaning is the caller's.
	Score int64 `json:"score"`
	// Payload is opaque data carried through to the match, bounded by
	// MaxPayloadBytes.
	Payload []byte `json:"payload,omitempty"`
}

// MaxPayloadBytes bounds a subject's opaque payload.
const MaxPayloadBytes = 1024

func (s Subject) Validate() error {
	if s.Kind == "" {
		return fmt.Errorf("%w: kind is empty", ErrSubjectInvalid)
	}
	if s.ID == 0 {
		return fmt.Errorf("%w: id is zero", ErrSubjectInvalid)
	}
	if len(s.Payload) > MaxPayloadBytes {
		return fmt.Errorf("%w: payload is %d bytes, limit %d", ErrSubjectInvalid, len(s.Payload), MaxPayloadBytes)
	}
	return nil
}

// Key identifies one subject uniquely, and is what enforces "one live ticket
// per subject".
func (s Subject) Key() string { return fmt.Sprintf("%s:%d", s.Kind, s.ID) }

// Queue identifies one matchmaking pool.
type Queue struct {
	// Mode is the gameplay mode, opaque here.
	Mode string `json:"mode"`
	// GroupSize is how many subjects a match holds. A queue is defined by it:
	// a 2-player and a 5-player pool of the same mode are different queues.
	GroupSize int `json:"group_size"`
	// Partition scopes a queue, e.g. a region, a rank band or a server group.
	// It is part of the routing and storage key, so operations on one queue
	// land on one instance and one stored value — the part the implementation
	// this replaces lacked.
	//
	// 分区是大队列的推荐拆法：一个队列的全部状态是一个整体读写的值，按段位档 / 地区 / sid 组拆成多个分区后，
	// 每个分区各自一个键，Enqueue / Cancel / Commit / Sweep 只读写自己的分区。Commit 只从一个分区取票；
	// 需要“放宽匹配”、一局票来自同一逻辑队列（Mode + GroupSize 相同）的几个分区时用 Store.CommitAcross，
	// 它原子地改这几个分区。不同 Mode / GroupSize 的票永不进同一局。
	Partition string `json:"partition"`
}

func (q Queue) Validate() error {
	if q.Mode == "" {
		return fmt.Errorf("%w: mode is empty", ErrQueueInvalid)
	}
	if q.GroupSize < 2 {
		return fmt.Errorf("%w: group size must be at least 2, got %d", ErrQueueInvalid, q.GroupSize)
	}
	if q.GroupSize > MaxGroupSize {
		return fmt.Errorf("%w: group size %d exceeds %d", ErrQueueInvalid, q.GroupSize, MaxGroupSize)
	}
	return nil
}

// Key renders the queue identity: "{<Mode>:<GroupSize>}:<Partition>". It is
// the queue's storage key (the Redis store prefixes it with "<prefix>:queue:")
// and also the affinity key: passing it to servicerpc.WithAffinityKey routes
// every operation on one queue to one instance, so the compare-and-set below
// contends with itself rather than with a different replica reading the same
// head.
//
// 花括号里是 LogicalKey（Mode + GroupSize，不含 Partition），它是 Redis Cluster 的 hash tag：同一个逻辑队列的
// 所有分区落在同一个 slot，跨分区成局（Store.CommitAcross）才能用一个多键脚本原子地改几个分区的状态；
// 不同逻辑队列仍分散到不同 slot。单分区的 Enqueue / Cancel / Commit / Sweep 仍只碰自己这一个键。
// 键名加 hash tag 是维护者 2026-10-19 批准的改动，旧键不迁移（尚未上线，CHANGELOG 写明）。
//
// The rendering is injective: Mode and Partition are opaque strings that may
// themselves contain ':', and {Mode "a:2", GroupSize 3, Partition "x"} and
// {Mode "a", GroupSize 2, Partition "3:x"} both used to render as "a:2:3:x" —
// two valid pools sharing one queueState, so Candidates read across them and
// Commit took a three-player ticket into a two-player match.
// The two free fields are escaped (escapeKeyField) so a ':' inside them can no
// longer be read as the separator, and a '{' or '}' inside them can neither end
// the hash tag early nor open a second one.
func (q Queue) Key() string {
	return "{" + q.LogicalKey() + "}:" + escapeKeyField(q.Partition)
}

// LogicalKey 是逻辑队列的身份 "<Mode>:<GroupSize>"：同一个 Mode 与 GroupSize、只差 Partition 的队列属于同一个
// 逻辑队列。它是 Key 的 hash tag，也是跨分区成局（CommitAcross）的路由键。
func (q Queue) LogicalKey() string {
	return fmt.Sprintf("%s:%d", escapeKeyField(q.Mode), q.GroupSize)
}

// SameLogicalQueue 报告两个队列是否同属一个逻辑队列（Mode 与 GroupSize 相同，Partition 可以不同）。
func (q Queue) SameLogicalQueue(other Queue) bool {
	return q.Mode == other.Mode && q.GroupSize == other.GroupSize
}

// escapeKeyField makes a free-form field safe to join with ':' and to sit
// inside or after a Redis hash tag. '%' is the escape character and is escaped
// first, so the mapping is injective.
func escapeKeyField(field string) string {
	if !strings.ContainsAny(field, ":%{}") {
		return field
	}
	field = strings.ReplaceAll(field, "%", "%25")
	field = strings.ReplaceAll(field, ":", "%3A")
	field = strings.ReplaceAll(field, "{", "%7B")
	return strings.ReplaceAll(field, "}", "%7D")
}

// TicketRef 指名跨分区成局里的一张票：它等待所在的分区队列与票 ID。匹配器从各分区的 Candidates 拿到的
// Ticket 已带 Queue 与 ID，直接组成 TicketRef。
type TicketRef struct {
	Queue    Queue  `json:"queue"`
	TicketID string `json:"ticket_id"`
}

// TicketRefs 是一次跨分区成局的票集合，按成员顺序排列。
type TicketRefs []TicketRef

// LogicalKey 是这组票所属逻辑队列的身份（第一张票的 Queue.LogicalKey），用作 CommitAcross 的路由键；
// 空集合返回空串（CommitAcross 会以 ErrTicketInvalid 拒绝它）。
func (refs TicketRefs) LogicalKey() string {
	if len(refs) == 0 {
		return ""
	}
	return refs[0].Queue.LogicalKey()
}

// TicketState is where a ticket is in its lifecycle.
//
//	waiting ─┬─> matched    (committed into a match)
//	         ├─> cancelled  (withdrawn by its subject)
//	         └─> expired    (waited past its deadline)
//
// matched, cancelled and expired are terminal.
type TicketState string

const (
	TicketWaiting   TicketState = "waiting"
	TicketMatched   TicketState = "matched"
	TicketCancelled TicketState = "cancelled"
	TicketExpired   TicketState = "expired"
)

func (s TicketState) terminal() bool {
	return s == TicketMatched || s == TicketCancelled || s == TicketExpired
}

// Ticket is one subject's place in a queue.
type Ticket struct {
	// ID is minted by the service, never accepted from a caller. A
	// caller-supplied id was what let one client queue a subject twice and
	// what let an "idempotent" enqueue return another player's ticket.
	ID      string      `json:"id"`
	Queue   Queue       `json:"queue"`
	Subject Subject     `json:"subject"`
	State   TicketState `json:"state"`
	// MatchID is set when State is matched.
	MatchID string `json:"match_id,omitempty"`
	// RequestID is the caller's idempotency key for the enqueue that created
	// this ticket. A repeat enqueue with the same key returns this ticket
	// instead of creating a second one.
	RequestID      string `json:"request_id,omitempty"`
	CreatedAtUnix  int64  `json:"created_at_unix"`
	ExpiresAtUnix  int64  `json:"expires_at_unix"`
	ResolvedAtUnix int64  `json:"resolved_at_unix,omitempty"`
}

// Expired reports whether a waiting ticket has passed its deadline. A resolved
// ticket never expires — its state is already terminal.
func (t Ticket) Expired(now time.Time) bool {
	if t.State != TicketWaiting || t.ExpiresAtUnix == 0 {
		return false
	}
	return now.Unix() >= t.ExpiresAtUnix
}

// Match is a committed group.
type Match struct {
	ID    string `json:"id"`
	Queue Queue  `json:"queue"`
	// Members are the subjects, in the order the grouping policy placed them.
	Members []Subject `json:"members"`
	// TicketIDs are the tickets this match consumed, in member order. They
	// are recorded so a match can be reconciled against its tickets.
	TicketIDs []string `json:"ticket_ids"`
	// TicketPartitions 只在跨分区成局（CommitAcross）时填写，与 TicketIDs 一一对应：TicketPartitions[i] 是
	// TicketIDs[i] 等待所在的分区（Queue.Partition）。此时 Queue 是第一张票的分区队列；比赛在每个涉及的分区里
	// 各存一份，用其中任何一个分区的 Queue 调 Match 都能读到。单分区成局时为空。
	TicketPartitions []string `json:"ticket_partitions,omitempty"`
	CreatedAtUnix    int64    `json:"created_at_unix"`
}

// Error maps an error to the code and reason a client sees.
//
// It matches servicerpc.Error's convention, which is what an RPC envelope is
// filled from.
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

// clone deep-copies the one reference field, Payload. The memory store hands
// values back and forth by copy, and a []byte inside a copied struct is still
// the same bytes: a caller that edited a returned Payload — or its own input
// after Enqueue — was editing the store.
func (s Subject) clone() Subject {
	if s.Payload != nil {
		s.Payload = append([]byte(nil), s.Payload...)
	}
	return s
}

// clone deep-copies a ticket at the store boundary; see Subject.clone.
func (t Ticket) clone() Ticket {
	t.Subject = t.Subject.clone()
	return t
}

// clone deep-copies a match at the store boundary: Members (each with its
// Payload) and TicketIDs are the fields a caller could otherwise write through.
func (m Match) clone() Match {
	if m.Members != nil {
		members := make([]Subject, len(m.Members))
		for i, member := range m.Members {
			members[i] = member.clone()
		}
		m.Members = members
	}
	if m.TicketIDs != nil {
		m.TicketIDs = append([]string(nil), m.TicketIDs...)
	}
	if m.TicketPartitions != nil {
		m.TicketPartitions = append([]string(nil), m.TicketPartitions...)
	}
	return m
}
