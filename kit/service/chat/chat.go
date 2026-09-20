// Package chat is the messaging service: a sender publishes a message into a
// channel, and a reader pages that channel's recent history.
//
// This is a redesign, not a port. Every defect below was confirmed by reading
// the implementation this replaces, and every decision here exists to make one
// of them unrepresentable rather than merely fixed:
//
//   - A client-supplied Trusted bool in the request body was the entire
//     authorization for a system message. Setting it disabled the
//     caller-equals-sender check, unlocked the system-template message type,
//     and bypassed both the channel policy and the history authorization. No
//     production code ever set it, so it was pure attack surface; two tests
//     asserted contradictory models and production took the vulnerable path.
//     Here there is no such field anywhere, PublishRequest carries no sender
//     either (the sender is the session's identity, passed as an argument, so
//     "caller equals sender" is not a check that can be switched off), and the
//     only way to speak as the game is Service.PublishSystem, which needs a
//     SystemToken — a struct whose only field is unexported, so no request
//     body, in any encoding, can ever produce one.
//   - Private history omitted everything the caller had sent, and could not be
//     scoped to a conversation: it filtered on {channel, target_id} with
//     target_id forced to equal the caller, while documents stored the
//     *recipient* there. No usable 1:1 chat could be built on it. Here a
//     pair-scoped channel's stream key is the unordered pair of the two role
//     ids (see ChannelRef), so both directions are one stream, the caller's own
//     messages are in it by construction, and "the conversation with peer X" is
//     the natural unit of a query rather than something to be reconstructed.
//   - There was no retention and no cursor. The timestamp was persisted as an
//     int64 of milliseconds rather than a date, so no TTL was even possible,
//     and history took only a Limit that was silently clamped to 200 — no
//     cursor, no since. On reconnect a client re-fetched what it already had
//     and permanently lost anything older, silently. Here history is
//     cursor-based in both directions, a page reports Gap when the cursor
//     predates what is retained, and retention is a bounded per-channel ring
//     plus an age-based Prune (see Store for the tradeoff, stated honestly:
//     versionstore has no TTL, so none is claimed).
//   - A global sequence document was a hot single-document bottleneck that was
//     not tied to the insert: every publish did a find-and-increment on one
//     document and then a separate insert. A failed insert burned the id, and a
//     client retry allocated a new one and stored a *second copy*, because
//     publish had no idempotency key at all — while the deployed transport was
//     at-least-once with max_deliver 5. Here the sequence is per channel and
//     lives in the same versioned entry as the messages, so allocating it and
//     storing the message are one compare-and-set, and a publish without an
//     idempotency key is refused.
//   - Ordering keyed off the publisher process's wall clock rather than the
//     monotonic sequence it had already allocated, so with more than one
//     replica clock skew reordered history. Here ordering is the sequence,
//     always; the stored timestamp exists only for retention and no read path
//     sorts on it.
//   - The storage seam validated with trusted = true unconditionally, so any
//     future direct user of it silently lost the trust check. Here the seam
//     itself holds the policy and the body registry and evaluates them on every
//     append; there is no trust parameter to pass, and the privileged append
//     demands the unforgeable token.
//   - There was no size limit on message text, and an unbounded args slice was
//     persisted verbatim. Here every body is bounded twice — by MaxBodyBytes
//     and by the per-type limit its BodySpec declares — and there is no args
//     field: a template and its arguments are the game's own encoding inside
//     those bounded bytes.
//   - Message body types were gameplay entities — position share, battle-report
//     share, guild invite — baked into the storage schema and into per-type
//     validation, so the messaging service could not be changed without
//     knowing the game. Here a body is opaque bytes with a registered
//     per-type validator, and an unregistered type is refused.
package chat

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/tjbdwanghaibo/roost-kit/service/servicemetrics"

	"github.com/tjbdwanghaibo/roost-core/versionstore"

	"github.com/tjbdwanghaibo/roost-core/errcode"
)

// Error codes.
//
// Written out rather than derived with iota, and every business failure has
// one: a client mistake must be answerable with a code the client can match
// on, never with the internal-failure code.
//
// There is no code of this package's for an internal failure. Code returns
// errcode.CodeInternal for an error it did not classify, which is the honest
// answer — a service-specific "store failed" code, which this block used to
// carry, claims a diagnosis nothing established and nothing could produce
// deliberately.
const (
	CodeOK int32 = 0

	CodeChannelInvalid   int32 = 580101
	CodeSenderInvalid    int32 = 580102
	CodeMessageInvalid   int32 = 580103
	CodeTypeUnknown      int32 = 580104
	CodeNotPermitted     int32 = 580105
	CodeRequestInvalid   int32 = 580106
	CodeCursorInvalid    int32 = 580107
	CodeRangeInvalid     int32 = 580108
	CodeSystemDenied     int32 = 580109
	CodeAlreadyPublished int32 = 580110
	CodeConflict         int32 = 580111
)

var (
	ErrChannelInvalid = errcode.Define(CodeChannelInvalid, "chat: channel is invalid", "")
	ErrSenderInvalid  = errcode.Define(CodeSenderInvalid, "chat: sender is invalid", "")
	ErrMessageInvalid = errcode.Define(CodeMessageInvalid, "chat: message is invalid", "")
	// ErrTypeUnknown reports that no BodyValidator is registered for the
	// message type. Refusing an unregistered type is what keeps gameplay out
	// of this package: the service does not know what a "battle report share"
	// is, it only knows that the game registered a validator for that type.
	ErrTypeUnknown = errcode.Define(CodeTypeUnknown, "chat: message type is not registered", "")
	// ErrNotPermitted reports that the channel policy, or a system-only
	// channel or message type, refused this sender. It wraps the policy's own
	// error, so a game can carry its reason ("muted", "not a guild member")
	// through without this package knowing any of them.
	ErrNotPermitted = errcode.Define(CodeNotPermitted, "chat: sender may not do that on this channel", "")
	// ErrRequestInvalid reports a missing or malformed idempotency key. Publish
	// requires one because the transport is at-least-once: the implementation
	// this replaces had no key and stored a second copy on every redelivery.
	ErrRequestInvalid = errcode.Define(CodeRequestInvalid, "chat: idempotency key is invalid", "")
	ErrCursorInvalid  = errcode.Define(CodeCursorInvalid, "chat: cursor is invalid", "")
	ErrRangeInvalid   = errcode.Define(CodeRangeInvalid, "chat: range is invalid", "")
	// ErrSystemDenied reports that the privileged entry point refused the
	// caller — either the service is wired without a system authenticator, or
	// the transport identity is not trusted infrastructure. This is the error a
	// client-side Trusted bool used to bypass.
	ErrSystemDenied = errcode.Define(CodeSystemDenied, "chat: caller is not trusted infrastructure", "")
	// ErrAlreadyPublished reports that the idempotency key was already used and
	// the message it produced is no longer retained, so it cannot be returned.
	// The publish happened: a consumer must ack, not retry. It exists so a
	// redelivery outside the dedup window is refused rather than silently
	// stored a second time.
	ErrAlreadyPublished = errcode.Define(CodeAlreadyPublished, "chat: idempotency key was already published and its message is no longer retained", "")
	ErrConflict         = errcode.Define(CodeConflict, "chat: conflict", "")
)

// Error maps an error to the code and reason a client sees.
//
// It matches roost-kit's servicerpc.Error convention, which is what an RPC
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
	// versionstore.ErrConflict is a FOREIGN sentinel: it belongs to roost-kit
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

// Bounds. Every one of these is a limit the implementation this replaces did
// not have, and none of them can be bypassed by leaving a field at zero: a
// zero limit is an error where it is a request parameter, and selects the
// documented default where it is configuration.
const (
	// MaxBodyBytes is the hard ceiling on one message body, whatever its type
	// declares. The implementation this replaces had no limit on message text
	// at all, and persisted an unbounded args slice alongside it.
	MaxBodyBytes = 4096
	// MaxSenderNameBytes bounds the display-name snapshot carried with a
	// message.
	MaxSenderNameBytes = 64
	// MaxActorBytes bounds the subsystem label a privileged publish declares.
	MaxActorBytes = 64
	// MaxRequestIDBytes bounds an idempotency key. The key is a map key in
	// stored state, so an unbounded one is an unbounded write.
	MaxRequestIDBytes = 128
	// MaxPageSize bounds one history page.
	MaxPageSize = 200
	// DefaultRetain is how many recent messages a channel keeps when its rule
	// does not say.
	DefaultRetain = 200
	// MaxRetain bounds a channel's retained ring. The whole channel is one
	// versioned entry, so this is also the bound on how large that entry can
	// grow — retention here is a size decision, not only a policy one.
	MaxRetain = 1000
	// MaxRequestKeys bounds how many idempotency keys one channel remembers.
	MaxRequestKeys = 2048
)

// ChannelKind names a family of channels. It is opaque to this package: what a
// kind means is the game's business, and what its rules are is data (see
// ChannelRule), not a switch statement.
type ChannelKind string

// The kinds DefaultChannelRules describes. A game may register others.
const (
	// ChannelWorld is a broadcast channel scoped to a world, zone or server.
	ChannelWorld ChannelKind = "world"
	// ChannelGroup is a channel shared by the members of some group — a guild,
	// a party, a team. Which group, and who is a member, is the policy's
	// business.
	ChannelGroup ChannelKind = "group"
	// ChannelPrivate is a 1:1 conversation between two roles.
	ChannelPrivate ChannelKind = "private"
	// ChannelSystem is a broadcast channel only the privileged entry point may
	// publish to.
	ChannelSystem ChannelKind = "system"
)

// ChannelScope says how a channel's storage stream is keyed, which is the same
// thing as saying how its history is scoped.
type ChannelScope string

const (
	// ScopeShared means every participant reads one stream, named by the
	// channel's target: a world id, a guild id.
	ScopeShared ChannelScope = "shared"
	// ScopePair means the stream is the unordered pair of the participant and
	// the target, so {a,b} and {b,a} are the same stream.
	//
	// This is the structural answer to the private-history defect. The
	// implementation this replaces stored the recipient in target_id and then
	// queried target_id == caller, which by construction returned only what the
	// caller had received and never what it had sent. With a symmetric key
	// there is no such filter to get wrong: one conversation is one stream.
	ScopePair ChannelScope = "pair"
)

// ChannelRule is one kind's rules, held as data.
//
// The implementation this replaces answered "does this kind need a target?"
// and "how is its history scoped?" in switch statements spread across
// validation, the publish path and the history path — which is how gameplay
// kinds ended up baked into the storage schema. Here a kind is a row: its
// rules travel with it, and a game registers its own rows through Config.
type ChannelRule struct {
	Kind ChannelKind `json:"kind"`
	// RequiresTarget refuses a channel whose Target is zero. A shared channel
	// without a target would be one global stream for every world at once.
	RequiresTarget bool `json:"requires_target"`
	// Scope selects the stream keying, and therefore the history scoping.
	Scope ChannelScope `json:"scope"`
	// SystemOnly means only the privileged entry point may publish here.
	// Reading is still subject to the policy: a system broadcast has to be
	// readable to be of any use.
	SystemOnly bool `json:"system_only"`
	// Retain is how many recent messages one channel of this kind keeps; zero
	// selects DefaultRetain and it may not exceed MaxRetain.
	Retain int `json:"retain"`
}

func (r ChannelRule) validate() error {
	if strings.TrimSpace(string(r.Kind)) == "" {
		return fmt.Errorf("%w: rule has an empty kind", ErrChannelInvalid)
	}
	switch r.Scope {
	case ScopeShared:
	case ScopePair:
		// A pair needs two participants, so the peer is not optional. Without
		// this, a pair-scoped kind with RequiresTarget false would key every
		// conversation of one role into a single stream.
		if !r.RequiresTarget {
			return fmt.Errorf("%w: kind %q is pair-scoped and must require a target", ErrChannelInvalid, r.Kind)
		}
	default:
		return fmt.Errorf("%w: kind %q has unknown scope %q", ErrChannelInvalid, r.Kind, r.Scope)
	}
	if r.Retain < 0 {
		return fmt.Errorf("%w: kind %q retains %d messages", ErrChannelInvalid, r.Kind, r.Retain)
	}
	if r.Retain > MaxRetain {
		return fmt.Errorf("%w: kind %q retains %d messages, limit %d", ErrChannelInvalid, r.Kind, r.Retain, MaxRetain)
	}
	return nil
}

// DefaultChannelRules is the built-in rule table. A game may replace any row
// by kind and add its own; see Config.Rules.
func DefaultChannelRules() []ChannelRule {
	return []ChannelRule{
		{Kind: ChannelWorld, RequiresTarget: true, Scope: ScopeShared, Retain: DefaultRetain},
		{Kind: ChannelGroup, RequiresTarget: true, Scope: ScopeShared, Retain: DefaultRetain},
		{Kind: ChannelPrivate, RequiresTarget: true, Scope: ScopePair, Retain: DefaultRetain},
		{Kind: ChannelSystem, RequiresTarget: false, Scope: ScopeShared, SystemOnly: true, Retain: 100},
	}
}

// ruleTable resolves a kind to its rule.
type ruleTable map[ChannelKind]ChannelRule

// newRuleTable merges overrides over the defaults, by kind.
func newRuleTable(overrides []ChannelRule) (ruleTable, error) {
	table := ruleTable{}
	for _, rule := range append(DefaultChannelRules(), overrides...) {
		if err := rule.validate(); err != nil {
			return nil, err
		}
		if rule.Retain == 0 {
			rule.Retain = DefaultRetain
		}
		table[rule.Kind] = rule
	}
	return table, nil
}

func (t ruleTable) lookup(kind ChannelKind) (ChannelRule, error) {
	rule, ok := t[kind]
	if !ok {
		// An unknown kind is a client mistake, not an internal failure: it gets
		// its own code and names what is wrong.
		return ChannelRule{}, fmt.Errorf("%w: no rule is registered for kind %q", ErrChannelInvalid, kind)
	}
	return rule, nil
}

// Channel addresses a channel.
type Channel struct {
	Kind ChannelKind `json:"kind"`
	// Target is what the channel is about: the world or zone id for a world
	// channel, the group id for a group channel, the peer's role id for a
	// pair-scoped one.
	//
	// For a pair-scoped kind this is the *recipient as addressed by the
	// sender*. It is deliberately not what history filters on — that filter is
	// precisely the defect that hid the caller's own messages — it only takes
	// part in deriving the symmetric stream key.
	Target int64 `json:"target"`
}

func (c Channel) String() string { return fmt.Sprintf("%s:%d", c.Kind, c.Target) }

// validate checks the channel against its rule and the participant it is being
// used by.
func (c Channel) validate(rule ChannelRule, participant int64) error {
	if c.Kind != rule.Kind {
		return fmt.Errorf("%w: channel kind %q does not match rule %q", ErrChannelInvalid, c.Kind, rule.Kind)
	}
	if c.Target < 0 {
		return fmt.Errorf("%w: target %d is negative", ErrChannelInvalid, c.Target)
	}
	if rule.RequiresTarget && c.Target == 0 {
		return fmt.Errorf("%w: kind %q requires a target", ErrChannelInvalid, c.Kind)
	}
	if rule.Scope == ScopePair {
		if participant <= 0 {
			return fmt.Errorf("%w: kind %q needs a participant", ErrChannelInvalid, c.Kind)
		}
		if c.Target == participant {
			return fmt.Errorf("%w: a pair-scoped channel's target is the peer, not the caller", ErrChannelInvalid)
		}
	}
	return nil
}

// ChannelRef is a channel resolved against one participant: the single storage
// stream their messages live in.
//
// Its key is unexported and only resolveRef builds one, for two reasons. A
// stream cannot be named from outside this package, so no caller can prune or
// inspect a conversation it is not part of by constructing a key. And the
// symmetry of a pair-scoped key — Resolve(peer=b, viewer=a) and
// Resolve(peer=a, viewer=b) are the same ref — is a property of the type
// rather than of every call site that happens to remember it.
type ChannelRef struct {
	Kind ChannelKind
	key  string
}

// Key renders the stream key, for logs and metrics.
func (r ChannelRef) Key() string { return r.key }

func (r ChannelRef) valid() bool { return r.key != "" }

func resolveRef(ch Channel, rule ChannelRule, participant int64) (ChannelRef, error) {
	if err := ch.validate(rule, participant); err != nil {
		return ChannelRef{}, err
	}
	switch rule.Scope {
	case ScopeShared:
		return ChannelRef{Kind: ch.Kind, key: fmt.Sprintf("%s:%d", ch.Kind, ch.Target)}, nil
	case ScopePair:
		low, high := participant, ch.Target
		if low > high {
			low, high = high, low
		}
		return ChannelRef{Kind: ch.Kind, key: fmt.Sprintf("%s:%d:%d", ch.Kind, low, high)}, nil
	default:
		return ChannelRef{}, fmt.Errorf("%w: kind %q has unknown scope %q", ErrChannelInvalid, ch.Kind, rule.Scope)
	}
}

// Sender is who is speaking, as established by the session — never as claimed
// by a request body.
type Sender struct {
	RoleID int64 `json:"role_id"`
	// Name is a display-name snapshot, so a reader can render history without
	// resolving every role id. It is a snapshot on purpose: renaming a role
	// does not rewrite what it already said.
	Name string `json:"name,omitempty"`
}

func (s Sender) validate() error {
	if s.RoleID <= 0 {
		return fmt.Errorf("%w: role id must be positive, got %d", ErrSenderInvalid, s.RoleID)
	}
	if len(s.Name) > MaxSenderNameBytes {
		return fmt.Errorf("%w: name is %d bytes, limit %d", ErrSenderInvalid, len(s.Name), MaxSenderNameBytes)
	}
	if !utf8.ValidString(s.Name) {
		return fmt.Errorf("%w: name is not valid utf-8", ErrSenderInvalid)
	}
	return nil
}

// Origin records where a stored message's authority came from. It is written by
// this package and never read from a request: the whole defect was that a
// request could assert it.
type Origin string

const (
	// OriginRole is a message a role published, authorized by ChannelPolicy.
	OriginRole Origin = "role"
	// OriginSystem is a message the game itself published through the
	// privileged entry point.
	OriginSystem Origin = "system"
)

// MessageType selects the body validator. It is opaque to this package.
type MessageType string

// Message is one stored message.
type Message struct {
	// Seq is the per-channel sequence, allocated inside the same
	// compare-and-set that stored this message. It is monotonic, never reused,
	// and it is the *only* ordering: no read path sorts on a timestamp, because
	// sorting on the publisher's wall clock is what let clock skew between
	// replicas reorder history.
	Seq uint64 `json:"seq"`
	// Channel is the channel as the sender addressed it.
	Channel Channel `json:"channel"`
	// From is the sender. For a system message its RoleID is zero and its Name
	// is the subsystem label the privileged caller declared, so an audit can
	// say which subsystem spoke.
	From   Sender      `json:"from"`
	Origin Origin      `json:"origin"`
	Type   MessageType `json:"type"`
	// Body is opaque bytes. This package never interprets it — the gameplay
	// entities the implementation this replaces had in its schema are the
	// game's own encoding inside these bytes, checked by the validator the game
	// registered for this type.
	Body []byte `json:"body,omitempty"`
	// RequestID is the idempotency key that produced this message. A replay of
	// it returns this message instead of storing a second copy.
	RequestID string `json:"request_id"`
	// StoredAtUnix is when the service stored it, from the injected clock. It
	// exists for retention and for display. Nothing orders by it. Retention is
	// the only reason it is a date at all: the implementation this replaces
	// persisted milliseconds in an int64, which no TTL and no age-based prune
	// can use.
	StoredAtUnix int64 `json:"stored_at_unix"`
}

// PublishRequest is what a role's publish carries.
//
// Note what is absent, because that absence is the fix:
//
//   - No Trusted flag, or anything else a client could set to change how it is
//     authorized. Trust is not expressible in this type.
//   - No sender. The sender is the session identity, passed to Publish as a
//     separate argument, so a request cannot claim to be from someone else and
//     "caller equals sender" is not a check that could be skipped.
//   - No Origin, and no client timestamp. Both are decided by the service.
//   - No args slice. A system template's arguments are the game's encoding
//     inside Body, which is bounded.
type PublishRequest struct {
	Channel Channel     `json:"channel"`
	Type    MessageType `json:"type"`
	Body    []byte      `json:"body,omitempty"`
	// RequestID is the idempotency key, and it is required. The transport that
	// carried the implementation this replaces was at-least-once with
	// max_deliver 5, and a publish with no key stored a copy per delivery.
	RequestID string `json:"request_id"`
}

// SystemPublishRequest is what the privileged entry point carries. It is a
// different type from PublishRequest on purpose: "the game is speaking" is a
// different operation with a different authorization, not a variant of a
// player's publish distinguished by a boolean.
type SystemPublishRequest struct {
	Channel Channel `json:"channel"`
	// Actor is the subsystem that is speaking — "world-boss", "gm-console",
	// "mail-daemon". It is required and recorded, so a system message in
	// history says which part of the game produced it.
	Actor     string      `json:"actor"`
	Type      MessageType `json:"type"`
	Body      []byte      `json:"body,omitempty"`
	RequestID string      `json:"request_id"`
}

// HistoryQuery pages a channel's history.
//
// A cursor in both directions is the point. The implementation this replaces
// took a Limit and nothing else, silently clamped it to 200, and so a client
// reconnecting re-received what it already had and could never reach anything
// older.
type HistoryQuery struct {
	Channel Channel `json:"channel"`
	// AfterSeq pages forward, exclusive: give me what I have not seen. This is
	// the reconnect case.
	AfterSeq uint64 `json:"after_seq,omitempty"`
	// BeforeSeq pages backward, exclusive: scrollback. Setting both cursors is
	// an error rather than a silent preference.
	BeforeSeq uint64 `json:"before_seq,omitempty"`
	// Limit is required, must be positive and may not exceed MaxPageSize. A
	// non-positive limit is an error, never "unlimited" and never silently
	// replaced by a default.
	Limit int `json:"limit"`
}

func (q HistoryQuery) validate() error {
	if q.AfterSeq > 0 && q.BeforeSeq > 0 {
		return fmt.Errorf("%w: after_seq %d and before_seq %d are both set", ErrCursorInvalid, q.AfterSeq, q.BeforeSeq)
	}
	if q.Limit <= 0 {
		return fmt.Errorf("%w: limit must be positive, got %d", ErrRangeInvalid, q.Limit)
	}
	if q.Limit > MaxPageSize {
		return fmt.Errorf("%w: limit %d exceeds %d", ErrRangeInvalid, q.Limit, MaxPageSize)
	}
	return nil
}

// Page is one history page, always in ascending sequence order.
type Page struct {
	Messages []Message `json:"messages"`
	// NextCursor is what to pass as AfterSeq next time. It is unchanged when
	// the page is empty, so a client that is up to date does not lose its
	// place.
	NextCursor uint64 `json:"next_cursor"`
	// PrevCursor is what to pass as BeforeSeq to scroll further back.
	PrevCursor uint64 `json:"prev_cursor"`
	// HasMore reports that more retained messages exist in the direction this
	// page was paging.
	HasMore bool `json:"has_more"`
	// Gap reports that the forward cursor predates the oldest retained
	// message: messages between them were dropped by retention and this client
	// will never receive them. Saying so is the difference from an
	// implementation where the loss was silent — a client can resynchronise
	// instead of believing it is up to date.
	Gap bool `json:"gap"`
	// OldestSeq is the oldest sequence still retained, zero when nothing is.
	OldestSeq uint64 `json:"oldest_seq"`
	// LatestSeq is the highest sequence ever issued for this channel. It never
	// decreases, not even when retention empties the ring, so a cursor stays
	// comparable across a prune.
	LatestSeq uint64 `json:"latest_seq"`
}

// Stats is one channel's depth and drop counters — the signals constraint 6
// exists for. A silent path stays broken for as long as nothing measures it.
type Stats struct {
	// Retained is how many messages the channel currently holds.
	Retained int `json:"retained"`
	// OldestSeq and LatestSeq bracket what a reader can still fetch.
	OldestSeq uint64 `json:"oldest_seq"`
	LatestSeq uint64 `json:"latest_seq"`
	// Evicted counts messages retention has dropped from this channel, by ring
	// overflow or by Prune. It is the drop count: without it, retention loss is
	// invisible.
	Evicted uint64 `json:"evicted"`
	// RequestKeys is how many idempotency keys the channel remembers, which is
	// the width of its deduplication window.
	RequestKeys int `json:"request_keys"`
}

// ChannelPolicy decides whether a role may publish to or read a channel. The
// game supplies it; this package has no opinion and no production default.
//
// It is an interface, not a bool: membership of a guild, being muted, being on
// a block list and being in the same world are all the game's knowledge, and
// the implementation this replaces made all of it bypassable by setting one
// field in a request body.
//
// A policy must fail closed. Returning nil means permitted, so a policy that
// cannot reach the data it needs must return an error, not nil.
type ChannelPolicy interface {
	// CanPublish reports whether from may publish into ch. The returned error
	// is wrapped in ErrNotPermitted and travels to the caller, so a game can
	// carry its own reason without this package knowing any of them.
	CanPublish(ctx context.Context, from Sender, ch Channel) error
	// CanRead reports whether viewer may read ch's history. It is a separate
	// decision from CanPublish: a muted player still reads, and a world channel
	// a player has left is still readable to nobody.
	CanRead(ctx context.Context, viewer Sender, ch Channel) error
}

// PolicyFuncs adapts two functions to ChannelPolicy. A nil function denies,
// rather than permitting: an unset half of a policy is a missing decision, and
// the implementation this replaces is what a permissive default looks like in
// production.
type PolicyFuncs struct {
	Publish func(ctx context.Context, from Sender, ch Channel) error
	Read    func(ctx context.Context, viewer Sender, ch Channel) error
}

func (p PolicyFuncs) CanPublish(ctx context.Context, from Sender, ch Channel) error {
	if p.Publish == nil {
		return fmt.Errorf("%w: no publish policy is configured", ErrNotPermitted)
	}
	return p.Publish(ctx, from, ch)
}

func (p PolicyFuncs) CanRead(ctx context.Context, viewer Sender, ch Channel) error {
	if p.Read == nil {
		return fmt.Errorf("%w: no read policy is configured", ErrNotPermitted)
	}
	return p.Read(ctx, viewer, ch)
}

var _ ChannelPolicy = PolicyFuncs{}

// BodyValidator checks one message body.
//
// It receives opaque bytes and decides for itself what they must be. That is
// how gameplay stays out of this package: the implementation this replaces had
// a position share, a battle-report share and a guild invite in its storage
// schema and a switch over them in its validation, so the messaging service
// could not be reused by a game with different entities — or changed without
// touching them.
type BodyValidator interface {
	Validate(body []byte) error
}

// BodyValidatorFunc adapts a function to BodyValidator.
type BodyValidatorFunc func(body []byte) error

func (f BodyValidatorFunc) Validate(body []byte) error { return f(body) }

// BodySpec is what a game registers for one message type.
type BodySpec struct {
	// Validator is required: a type with no validator would be an unchecked
	// body, which is the state the implementation this replaces was in for
	// message text.
	Validator BodyValidator
	// MaxBytes bounds this type's body; zero selects MaxBodyBytes and it may
	// not exceed MaxBodyBytes. The ceiling is enforced at registration, so a
	// type cannot be registered with a bound that is not a bound.
	MaxBytes int
	// SystemOnly means only the privileged entry point may publish this type.
	// The implementation this replaces had exactly such a type — a system
	// template — and a client-set boolean unlocked it.
	SystemOnly bool
	// AllowEmpty permits a zero-length body, for types that are a pure signal.
	// Off by default: an empty body was accepted everywhere before.
	AllowEmpty bool
}

// BodyRegistry maps a message type to its spec. An unregistered type is
// refused, which is what keeps the set of types a deployment decision rather
// than a compiled-in list.
type BodyRegistry struct {
	mu    sync.RWMutex
	specs map[MessageType]BodySpec
}

func NewBodyRegistry() *BodyRegistry {
	return &BodyRegistry{specs: map[MessageType]BodySpec{}}
}

// Register adds a type. It refuses a redefinition rather than overwriting:
// two packages registering the same type with different bounds is a
// configuration fault, and the last writer winning makes it invisible.
func (r *BodyRegistry) Register(messageType MessageType, spec BodySpec) error {
	if strings.TrimSpace(string(messageType)) == "" {
		return fmt.Errorf("%w: message type is empty", ErrTypeUnknown)
	}
	if spec.Validator == nil {
		return fmt.Errorf("%w: type %q has no validator", ErrMessageInvalid, messageType)
	}
	if spec.MaxBytes < 0 || spec.MaxBytes > MaxBodyBytes {
		return fmt.Errorf("%w: type %q declares max %d bytes, limit %d", ErrMessageInvalid, messageType, spec.MaxBytes, MaxBodyBytes)
	}
	if spec.MaxBytes == 0 {
		spec.MaxBytes = MaxBodyBytes
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.specs[messageType]; exists {
		return fmt.Errorf("%w: type %q is already registered", ErrConflict, messageType)
	}
	r.specs[messageType] = spec
	return nil
}

// MustRegister is Register for wiring code that cannot proceed without the
// type. It panics at startup rather than losing the registration silently.
func (r *BodyRegistry) MustRegister(messageType MessageType, spec BodySpec) {
	if err := r.Register(messageType, spec); err != nil {
		panic(err)
	}
}

func (r *BodyRegistry) lookup(messageType MessageType) (BodySpec, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	spec, ok := r.specs[messageType]
	if !ok {
		return BodySpec{}, fmt.Errorf("%w: %q", ErrTypeUnknown, messageType)
	}
	return spec, nil
}

// Types lists the registered types. Bounded by construction — a registry is
// wiring, not client input — and used by tests and admin output.
func (r *BodyRegistry) Types() []MessageType {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]MessageType, 0, len(r.specs))
	for messageType := range r.specs {
		out = append(out, messageType)
	}
	return out
}

// validateBody applies the two bounds and then the game's validator. Order
// matters: the size check runs first so a validator is never handed a body
// larger than the type admits.
func validateBody(messageType MessageType, spec BodySpec, body []byte) error {
	if len(body) == 0 && !spec.AllowEmpty {
		return fmt.Errorf("%w: type %q requires a body", ErrMessageInvalid, messageType)
	}
	if len(body) > spec.MaxBytes {
		return fmt.Errorf("%w: body is %d bytes, type %q allows %d", ErrMessageInvalid, len(body), messageType, spec.MaxBytes)
	}
	if len(body) > MaxBodyBytes {
		return fmt.Errorf("%w: body is %d bytes, limit %d", ErrMessageInvalid, len(body), MaxBodyBytes)
	}
	if err := spec.Validator.Validate(body); err != nil {
		return fmt.Errorf("%w: type %q: %w", ErrMessageInvalid, messageType, err)
	}
	return nil
}

// TextValidator returns a validator for human-typed text: valid UTF-8, at most
// maxRunes runes, and no C0 control characters other than tab and newline.
//
// It is a helper, not a default: a game still has to register the type it
// wants to use it for. Its rune bound exists because the implementation this
// replaces had no limit on message text, so one client could store an
// arbitrarily large document in a channel every other player then fetched.
func TextValidator(maxRunes int) BodyValidator {
	return BodyValidatorFunc(func(body []byte) error {
		if maxRunes <= 0 {
			return fmt.Errorf("text validator: max runes must be positive, got %d", maxRunes)
		}
		if !utf8.Valid(body) {
			return errors.New("text is not valid utf-8")
		}
		if count := utf8.RuneCount(body); count > maxRunes {
			return fmt.Errorf("text is %d runes, limit %d", count, maxRunes)
		}
		for _, r := range string(body) {
			if r < 0x20 && r != '\n' && r != '\t' {
				return fmt.Errorf("text contains control character %#U", r)
			}
		}
		return nil
	})
}

// SystemToken is proof that a call came from trusted infrastructure.
//
// Its only field is unexported, so no request body — JSON, protobuf, anything —
// can produce one, and no zero value grants anything. That is the entire
// difference from the boolean this package exists to remove: trust is a value
// that can only be minted inside the server process by GrantSystem, from
// transport identity, and it is required by the privileged append.
type SystemToken struct {
	granted bool
}

func (t SystemToken) valid() bool { return t.granted }

// GrantSystem mints a token. It is exported because the code that establishes
// transport identity — an internal-only listener, mTLS peer identity, a
// service-to-service credential — lives in the assembling repo, not here.
//
// Calling it is a decision written in server code. It is never reachable from
// a request field, which is the property that matters.
func GrantSystem() SystemToken { return SystemToken{granted: true} }

// SystemAuthenticator turns the transport identity of the current call into a
// SystemToken.
//
// There is no default, and a Service wired without one has no system path at
// all: PublishSystem then fails closed with ErrSystemDenied. An absent
// authenticator must not look like a permissive one — that is exactly how the
// Trusted bool came to be the only gate.
type SystemAuthenticator interface {
	AuthenticateSystem(ctx context.Context) (SystemToken, error)
}

// SystemAuthenticatorFunc adapts a function to SystemAuthenticator.
type SystemAuthenticatorFunc func(ctx context.Context) (SystemToken, error)

func (f SystemAuthenticatorFunc) AuthenticateSystem(ctx context.Context) (SystemToken, error) {
	return f(ctx)
}

var _ SystemAuthenticator = SystemAuthenticatorFunc(nil)

// Metrics is the reporting seam, shared with every other service package so
// there is one vocabulary rather than one per package.
//
// It carries the counters constraint 6 exists for: depth, contention and
// drops. Every method is a named event rather than a string-keyed Count, so a
// misspelled event name is a compile error instead of a metric nobody sees.
//
// A nil Metrics is allowed and means "no reporting"; it is never a reason for
// a publish to fail. Channel kind is part of the operation name — "publish.pair"
// against "publish.group" — because a per-kind breakdown is what an operator
// needs and a separate typed parameter would not survive being shared.
type Metrics = servicemetrics.Reporter

func publishOp(kind ChannelKind) string { return "publish." + string(kind) }

func validateRequestID(requestID string) error {
	if strings.TrimSpace(requestID) == "" {
		return fmt.Errorf("%w: an idempotency key is required", ErrRequestInvalid)
	}
	if len(requestID) > MaxRequestIDBytes {
		return fmt.Errorf("%w: key is %d bytes, limit %d", ErrRequestInvalid, len(requestID), MaxRequestIDBytes)
	}
	if !utf8.ValidString(requestID) {
		return fmt.Errorf("%w: key is not valid utf-8", ErrRequestInvalid)
	}
	return nil
}

func validateActor(actor string) error {
	if strings.TrimSpace(actor) == "" {
		return fmt.Errorf("%w: a privileged publish must name its actor", ErrSenderInvalid)
	}
	if len(actor) > MaxActorBytes {
		return fmt.Errorf("%w: actor is %d bytes, limit %d", ErrSenderInvalid, len(actor), MaxActorBytes)
	}
	if !utf8.ValidString(actor) {
		return fmt.Errorf("%w: actor is not valid utf-8", ErrSenderInvalid)
	}
	return nil
}

// denied wraps a policy's own error so both sentinels match: the caller can
// test for ErrNotPermitted, and a game can test for its own reason.
func denied(cause error) error {
	if cause == nil {
		return fmt.Errorf("%w", ErrNotPermitted)
	}
	return fmt.Errorf("%w: %w", ErrNotPermitted, cause)
}
