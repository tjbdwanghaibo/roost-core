package chat

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/tjbdwanghaibo/roost-core/versionstore"

	"github.com/tjbdwanghaibo/roost-kit/service/servicemetrics"
)

// Store is chat persistence, and it is also where authorization happens.
//
// That placement is deliberate. In the implementation this replaces the
// storage seam validated with trusted = true unconditionally — the trust
// decision was an argument the caller passed, and the only caller passed the
// value that skipped every check. Any future direct user of that seam would
// have silently lost the check. Here there is no trust argument to pass: the
// seam holds the ChannelPolicy and the BodyRegistry and evaluates them on
// every append and every history read, and the privileged append demands a
// SystemToken, which cannot be constructed outside this package.
//
// The retention tradeoff, stated plainly: versionstore has no TTL in its
// contract, and a TTL on versioned state would drop the version along with the
// value. So this package does not claim one. A channel keeps a bounded ring of
// its most recent messages (ChannelRule.Retain, at most MaxRetain), enforced on
// every append with no background job required, plus an age-based Prune an
// operator or a sweep loop calls. The cost is real and is the honest part:
// history older than the ring is gone, and gone permanently — chat here is a
// live feed with a replay window, not an archive. A game that needs an archive
// must tee messages to a log or a warehouse, which is a different service.
// What the ring does buy is that the retained window is also a bound on how
// large one versioned entry can grow, and that a client which fell outside the
// window is *told* so (Page.Gap) instead of silently believing it is current.
type Store interface {
	// Append stores a role's message. It validates the channel against its
	// rule, requires the message type to be registered, bounds the body,
	// requires an idempotency key, and evaluates the channel policy — always,
	// with no way for a caller to ask it not to.
	//
	// from is the session's identity, not something the request carried. A
	// replay of req.RequestID returns the message already stored.
	Append(ctx context.Context, from Sender, req PublishRequest) (Message, error)

	// AppendSystem stores a message from the game itself. It needs a
	// SystemToken, which only GrantSystem mints and no request body can carry.
	//
	// It skips exactly one thing the role path does — the per-role
	// ChannelPolicy, because there is no role — and skips nothing else: the
	// type must still be registered, the body is still bounded, the
	// idempotency key is still required, retention still applies.
	AppendSystem(ctx context.Context, token SystemToken, req SystemPublishRequest) (Message, error)

	// History pages a channel, ascending by sequence, bounded by
	// HistoryQuery.Limit. The viewer's read permission is evaluated every time.
	History(ctx context.Context, viewer Sender, query HistoryQuery) (Page, error)

	// Resolve maps a channel plus one participant to the single stream they
	// share. For a pair-scoped channel it is symmetric, which is what makes a
	// 1:1 conversation one stream instead of two half-views.
	Resolve(ch Channel, participant int64) (ChannelRef, error)

	// Prune drops messages older than the configured retention age, up to
	// limit, and returns how many it dropped. Bounded: a non-positive limit is
	// an error.
	Prune(ctx context.Context, ref ChannelRef, limit int) (int, error)

	// Stats reports one channel's depth and drop counters.
	Stats(ctx context.Context, ref ChannelRef) (Stats, error)
}

// channelState is one channel's entire mutable state, held as a single
// versioned entry.
//
// Keeping the sequence in the same entry as the messages is the central
// decision, and it answers two defects at once. The implementation this
// replaces did a find-and-increment on one *global* sequence document and then
// a separate insert: one hot document for the whole service, and an id that
// was allocated by a write which the insert could then fail to use — burning
// the id, or, on a client retry with no idempotency key, storing a second copy
// of the same message under a new id. Here the allocation, the append and the
// idempotency record are one compare-and-set on one channel's entry: either all
// of them happen or none do, and contention is per channel rather than global.
//
// The type is unexported so nothing outside this package can write chat state
// and thereby skip the policy, the type registry and the bounds the seam
// applies. Wire a backend for it with NewRedisStateStore.
type channelState struct {
	// LastSeq is the highest sequence ever issued for this channel. It only
	// increases, and retention never resets it, so a cursor a client is holding
	// stays comparable even after everything it named has been pruned.
	LastSeq uint64 `json:"last_seq"`
	// Ring holds the retained messages, oldest first — which is also sequence
	// order, because a sequence is only ever assigned by appending here.
	Ring []Message `json:"ring"`
	// Requests maps an idempotency key to the sequence it produced. It is the
	// deduplication ledger for an at-least-once transport.
	Requests map[string]uint64 `json:"requests,omitempty"`
	// Evicted counts messages retention has dropped, ever. A drop count that
	// nobody keeps is a silent path, which is how the absence of retention went
	// unnoticed in the first place.
	Evicted uint64 `json:"evicted"`
}

// clone deep-copies the state before mutation.
//
// versionstore documents that a Mutate may run more than once and must be a
// pure function of its arguments. The memory implementation hands out the
// stored value directly, so mutating the slice or the map in place would edit
// stored state even on a mutation that then declines to save. Copying is the
// only way to keep "an aborted mutation changes nothing" true for every
// implementation of the contract.
func (s channelState) clone() channelState {
	next := channelState{LastSeq: s.LastSeq, Evicted: s.Evicted}
	if len(s.Ring) > 0 {
		next.Ring = make([]Message, len(s.Ring))
		for i, msg := range s.Ring {
			next.Ring[i] = msg.clone()
		}
	}
	next.Requests = make(map[string]uint64, len(s.Requests)+1)
	for key, seq := range s.Requests {
		next.Requests[key] = seq
	}
	return next
}

// clone copies a message including its body, so a stored body and a body a
// caller still holds a reference to can never be the same array.
func (m Message) clone() Message {
	out := m
	if len(m.Body) > 0 {
		out.Body = append([]byte(nil), m.Body...)
	}
	return out
}

// find returns the retained message with this sequence. The ring is ascending,
// so this is a binary search rather than a scan.
func (s channelState) find(seq uint64) (Message, bool) {
	index := sort.Search(len(s.Ring), func(i int) bool { return s.Ring[i].Seq >= seq })
	if index < len(s.Ring) && s.Ring[index].Seq == seq {
		return s.Ring[index], true
	}
	return Message{}, false
}

func (s channelState) oldestSeq() uint64 {
	if len(s.Ring) == 0 {
		return 0
	}
	return s.Ring[0].Seq
}

// trim enforces the retained ring and bounds the deduplication ledger,
// returning how many messages it evicted.
//
// The ledger is kept wider than the ring (up to requestKeyBudget) on purpose:
// inside that window a redelivery is either answered with the stored message or
// refused with ErrAlreadyPublished, so it can never become a second copy. That
// window is finite, and saying so is the honest form of this design — with a
// ring of hundreds of messages and a transport that redelivers within seconds
// at max_deliver 5, a redelivery falling outside it is not a case that occurs,
// but it is a case that exists.
func (s *channelState) trim(retain int) int {
	evicted := 0
	if retain > 0 && len(s.Ring) > retain {
		drop := len(s.Ring) - retain
		// Re-slice into a fresh array so the evicted messages are actually
		// released rather than kept alive by the backing array.
		kept := make([]Message, retain)
		copy(kept, s.Ring[drop:])
		s.Ring = kept
		evicted = drop
	}
	s.Evicted += uint64(evicted)

	budget := requestKeyBudget(retain)
	if len(s.Requests) > budget {
		type entry struct {
			key string
			seq uint64
		}
		entries := make([]entry, 0, len(s.Requests))
		for key, seq := range s.Requests {
			entries = append(entries, entry{key: key, seq: seq})
		}
		// Oldest sequences go first: the deduplication window follows the ring
		// forward instead of expiring keys at random.
		sort.Slice(entries, func(i, j int) bool { return entries[i].seq < entries[j].seq })
		for _, item := range entries[:len(s.Requests)-budget] {
			delete(s.Requests, item.key)
		}
	}
	return evicted
}

// requestKeyBudget is how many idempotency keys a channel remembers: twice its
// retained ring, so the deduplication window is never narrower than the
// replayable window, capped by MaxRequestKeys so one channel's entry stays
// bounded.
func requestKeyBudget(retain int) int {
	budget := retain * 2
	if budget < DefaultRetain {
		budget = DefaultRetain
	}
	if budget > MaxRequestKeys {
		budget = MaxRequestKeys
	}
	return budget
}

// StateStore is the versioned store chat state lives in. It is named so an
// assembler can talk about the dependency without naming the persisted value
// type, which stays unexported.
type StateStore = versionstore.Store[string, channelState]

// NewRedisStateStore builds the durable backend for chat state.
//
// It exists so a deployment can wire this package without being able to name
// channelState: state is reachable only through Store, which is where the
// policy and the bounds are applied.
func NewRedisStateStore(client versionstore.RedisClient, prefix string) (StateStore, error) {
	return versionstore.NewRedisStore(client, versionstore.RedisConfig[string, channelState]{
		Prefix: prefix,
		KeyOf:  func(key string) string { return key },
		Codec:  versionstore.JSONCodec[channelState]{},
	})
}

// DefaultRetentionAge is how long Prune keeps a message. Chat is a live feed:
// three days covers a client that was offline for a weekend.
const DefaultRetentionAge = 72 * time.Hour

// Config wires a Store. Every field without a safe default is required and the
// constructor refuses an incomplete one, because a chat service that starts
// with no policy is a chat service where everyone may say anything anywhere.
type Config struct {
	// Policy decides publish and read permission. Required: there is no
	// production default, permissive or otherwise. A permissive policy is what
	// the Trusted bool amounted to.
	Policy ChannelPolicy
	// Bodies is the message-type registry. Required, and an empty one is
	// legitimate — it simply refuses every publish until a game registers its
	// types.
	Bodies *BodyRegistry
	// Rules replaces or extends DefaultChannelRules, matched by kind. Channel
	// behaviour is data, so a game adds a kind here instead of editing a
	// switch.
	Rules []ChannelRule
	// RetentionAge is how old a message must be for Prune to drop it; zero
	// selects DefaultRetentionAge. It must be positive: an age of zero would
	// make Prune delete the channel.
	RetentionAge time.Duration
	// Now is the clock; nil means time.Now. Injected because a stored
	// timestamp must come from one place — and because the ordering test
	// requires a clock that runs backwards.
	Now func() time.Time
	// Metrics is optional. Nil means no reporting.
	Metrics Metrics
}

type channelStore struct {
	state   StateStore
	rules   ruleTable
	policy  ChannelPolicy
	bodies  *BodyRegistry
	now     func() time.Time
	age     time.Duration
	metrics servicemetrics.Sink
}

// NewStore wires a Store over versioned per-channel state.
func NewStore(state StateStore, cfg Config) (Store, error) {
	missing := []string{}
	if state == nil {
		missing = append(missing, "state store")
	}
	if cfg.Policy == nil {
		missing = append(missing, "channel policy")
	}
	if cfg.Bodies == nil {
		missing = append(missing, "body registry")
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("chat: store config is incomplete: %v", missing)
	}
	if cfg.RetentionAge < 0 {
		return nil, fmt.Errorf("chat: retention age must not be negative, got %s", cfg.RetentionAge)
	}
	if cfg.RetentionAge == 0 {
		cfg.RetentionAge = DefaultRetentionAge
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	rules, err := newRuleTable(cfg.Rules)
	if err != nil {
		return nil, err
	}
	return &channelStore{
		state:   state,
		rules:   rules,
		policy:  cfg.Policy,
		bodies:  cfg.Bodies,
		now:     cfg.Now,
		age:     cfg.RetentionAge,
		metrics: servicemetrics.Wrap(cfg.Metrics),
	}, nil
}

func (s *channelStore) Resolve(ch Channel, participant int64) (ChannelRef, error) {
	rule, err := s.rules.lookup(ch.Kind)
	if err != nil {
		return ChannelRef{}, err
	}
	return resolveRef(ch, rule, participant)
}

func (s *channelStore) Append(ctx context.Context, from Sender, req PublishRequest) (Message, error) {
	rule, err := s.rules.lookup(req.Channel.Kind)
	if err != nil {
		return Message{}, err
	}
	if err := from.validate(); err != nil {
		return Message{}, err
	}
	if err := validateRequestID(req.RequestID); err != nil {
		return Message{}, err
	}
	if rule.SystemOnly {
		// A role cannot publish here at all. In the implementation this
		// replaces a client-set boolean was enough to reach exactly this path.
		s.metrics.Refused(publishOp(req.Channel.Kind), "system_only_channel")
		return Message{}, denied(fmt.Errorf("kind %q accepts only privileged publishes", req.Channel.Kind))
	}
	spec, err := s.bodies.lookup(req.Type)
	if err != nil {
		return Message{}, err
	}
	if spec.SystemOnly {
		// The system-template type is the one the Trusted bool unlocked. There
		// is no flag that reaches it now — only PublishSystem.
		s.metrics.Refused(publishOp(req.Channel.Kind), "system_only_type")
		return Message{}, denied(fmt.Errorf("type %q accepts only privileged publishes", req.Type))
	}
	if err := validateBody(req.Type, spec, req.Body); err != nil {
		return Message{}, err
	}
	ref, err := resolveRef(req.Channel, rule, from.RoleID)
	if err != nil {
		return Message{}, err
	}
	// The policy runs on every append. There is no argument that skips it.
	if err := s.policy.CanPublish(ctx, from, req.Channel); err != nil {
		s.metrics.Refused(publishOp(req.Channel.Kind), "policy")
		return Message{}, denied(err)
	}

	return s.store(ctx, ref, rule, Message{
		Channel:   req.Channel,
		From:      from,
		Origin:    OriginRole,
		Type:      req.Type,
		Body:      req.Body,
		RequestID: req.RequestID,
	})
}

func (s *channelStore) AppendSystem(ctx context.Context, token SystemToken, req SystemPublishRequest) (Message, error) {
	// The token is the whole authorization, and it cannot be forged from a
	// request: SystemToken's only field is unexported.
	if !token.valid() {
		return Message{}, fmt.Errorf("%w: a privileged publish needs a granted system token", ErrSystemDenied)
	}
	rule, err := s.rules.lookup(req.Channel.Kind)
	if err != nil {
		return Message{}, err
	}
	if err := validateActor(req.Actor); err != nil {
		return Message{}, err
	}
	if err := validateRequestID(req.RequestID); err != nil {
		return Message{}, err
	}
	if rule.Scope == ScopePair {
		// A pair-scoped stream is keyed by two role ids and the game is not a
		// role. Refusing is better than inventing role 0 as a participant: a
		// game-to-player notice is mail, which is a different service with a
		// different lifetime.
		return Message{}, fmt.Errorf("%w: kind %q is pair-scoped and has no privileged publisher", ErrChannelInvalid, req.Channel.Kind)
	}
	spec, err := s.bodies.lookup(req.Type)
	if err != nil {
		return Message{}, err
	}
	if err := validateBody(req.Type, spec, req.Body); err != nil {
		return Message{}, err
	}
	// participant zero: a shared scope does not use it, and a pair scope was
	// refused above.
	ref, err := resolveRef(req.Channel, rule, 0)
	if err != nil {
		return Message{}, err
	}
	return s.store(ctx, ref, rule, Message{
		Channel:   req.Channel,
		From:      Sender{Name: req.Actor},
		Origin:    OriginSystem,
		Type:      req.Type,
		Body:      req.Body,
		RequestID: req.RequestID,
	})
}

// store is the one write path: allocate the sequence, append, record the
// idempotency key and enforce retention, all inside a single compare-and-set.
func (s *channelStore) store(ctx context.Context, ref ChannelRef, rule ChannelRule, draft Message) (Message, error) {
	draft = draft.clone()
	storedAt := s.now().Unix()

	var (
		result    Message
		replayed  bool
		keyReused bool
		evicted   int
	)
	_, applied, err := s.state.Update(ctx, ref.key, func(current channelState, _ bool) (channelState, bool, error) {
		next := current.clone()
		replayed, keyReused, evicted = false, false, 0

		if seq, ok := next.Requests[draft.RequestID]; ok {
			if existing, found := next.find(seq); found {
				if !sameSender(existing, draft) {
					// Keys are scoped per channel and clients mint them on
					// their own, so two senders can collide on one by accident.
					// Answering with the first sender's message would report
					// the second publish as a success while dropping it — so
					// the collision is refused, and the message is not lost
					// silently.
					keyReused = true
					return current, false, fmt.Errorf("%w: idempotency key %q was already used by another sender on this channel",
						ErrConflict, draft.RequestID)
				}
				// The replay answer: return what was stored, do not store a
				// second copy. This is what the at-least-once transport needed
				// and never had.
				result = existing
				replayed = true
				return current, false, nil
			}
			return current, false, fmt.Errorf("%w: key %q produced sequence %d", ErrAlreadyPublished, draft.RequestID, seq)
		}

		message := draft
		// The sequence is derived from the state being written, in the same
		// compare-and-set. Two publishers cannot get the same number, and a
		// failed write burns nothing: nobody has seen the number yet.
		message.Seq = next.LastSeq + 1
		message.StoredAtUnix = storedAt
		next.LastSeq = message.Seq
		next.Ring = append(next.Ring, message)
		next.Requests[message.RequestID] = message.Seq
		evicted = next.trim(rule.Retain)
		result = message
		return next, true, nil
	})
	if err != nil {
		if errors.Is(err, versionstore.ErrConflict) {
			s.metrics.Conflict("append")
		}
		if keyReused {
			s.metrics.Refused(publishOp(ref.Kind), "key_reused")
		}
		return Message{}, err
	}
	switch {
	case replayed:
		s.metrics.Replayed(publishOp(ref.Kind))
	case applied:
		s.metrics.Accepted(publishOp(ref.Kind))
		s.metrics.Dropped("message.evicted."+string(ref.Kind), evicted)
	}
	return result.clone(), nil
}

// sameSender reports whether a stored message and a draft come from the same
// publisher, which is what makes a repeated idempotency key a replay rather
// than a collision. A role is identified by its id; the game, publishing
// through the privileged path, by its origin and the actor label it declared.
func sameSender(stored, draft Message) bool {
	if stored.Origin != draft.Origin {
		return false
	}
	if stored.Origin == OriginSystem {
		return stored.From.Name == draft.From.Name
	}
	return stored.From.RoleID == draft.From.RoleID
}

func (s *channelStore) History(ctx context.Context, viewer Sender, query HistoryQuery) (Page, error) {
	rule, err := s.rules.lookup(query.Channel.Kind)
	if err != nil {
		return Page{}, err
	}
	if err := viewer.validate(); err != nil {
		return Page{}, err
	}
	if err := query.validate(); err != nil {
		return Page{}, err
	}
	ref, err := resolveRef(query.Channel, rule, viewer.RoleID)
	if err != nil {
		return Page{}, err
	}
	// Read permission is evaluated every time, for the same reason publish
	// permission is: the implementation this replaces let one request field
	// bypass the history authorization entirely.
	if err := s.policy.CanRead(ctx, viewer, query.Channel); err != nil {
		return Page{}, denied(err)
	}
	current, found, err := s.state.Get(ctx, ref.key)
	if err != nil {
		return Page{}, err
	}
	state := current.Value
	if !found {
		state = channelState{}
	}
	if query.AfterSeq > state.LastSeq {
		// A cursor beyond anything this channel ever issued is a cursor from
		// somewhere else. Answering "you are up to date" would strand the
		// client forever.
		return Page{}, fmt.Errorf("%w: after_seq %d is beyond sequence %d", ErrCursorInvalid, query.AfterSeq, state.LastSeq)
	}
	page := pageOf(state, query)
	if page.Gap {
		s.metrics.Dropped("history.gap."+string(ref.Kind), 1)
	}
	return page, nil
}

// pageOf slices the retained ring. The ring is in sequence order by
// construction, so this never sorts — and in particular never sorts on a
// timestamp, which is the defect that let clock skew between two publishing
// replicas reorder history.
func pageOf(state channelState, query HistoryQuery) Page {
	page := Page{OldestSeq: state.oldestSeq(), LatestSeq: state.LastSeq}
	ring := state.Ring

	var selected []Message
	switch {
	case query.AfterSeq > 0:
		start := 0
		for start < len(ring) && ring[start].Seq <= query.AfterSeq {
			start++
		}
		end := start + query.Limit
		if end > len(ring) {
			end = len(ring)
		}
		selected = ring[start:end]
		page.HasMore = end < len(ring)
		// The cursor names a message older than the oldest one retained, so
		// everything between them is gone. Report it rather than pretend the
		// client is current.
		if len(ring) == 0 {
			page.Gap = query.AfterSeq < state.LastSeq
		} else {
			page.Gap = query.AfterSeq+1 < page.OldestSeq
		}
	case query.BeforeSeq > 0:
		end := 0
		for end < len(ring) && ring[end].Seq < query.BeforeSeq {
			end++
		}
		start := end - query.Limit
		if start < 0 {
			start = 0
		}
		selected = ring[start:end]
		page.HasMore = start > 0
	default:
		// No cursor: the newest page, which is what a client opening a channel
		// wants. Paging forward from zero would hand it the oldest retained
		// messages instead.
		start := len(ring) - query.Limit
		if start < 0 {
			start = 0
		}
		selected = ring[start:]
		page.HasMore = start > 0
	}

	page.Messages = make([]Message, 0, len(selected))
	for _, message := range selected {
		page.Messages = append(page.Messages, message.clone())
	}
	if len(page.Messages) > 0 {
		page.NextCursor = page.Messages[len(page.Messages)-1].Seq
		page.PrevCursor = page.Messages[0].Seq
		return page
	}
	// An empty page must not move a client's cursor backwards.
	switch {
	case query.AfterSeq > 0:
		page.NextCursor = query.AfterSeq
	case query.BeforeSeq > 0:
		page.NextCursor = state.LastSeq
	default:
		page.NextCursor = state.LastSeq
	}
	return page
}

func (s *channelStore) Prune(ctx context.Context, ref ChannelRef, limit int) (int, error) {
	if !ref.valid() {
		return 0, fmt.Errorf("%w: ref is empty, resolve a channel first", ErrChannelInvalid)
	}
	if limit <= 0 {
		return 0, fmt.Errorf("%w: limit must be positive, got %d", ErrRangeInvalid, limit)
	}
	if limit > MaxRetain {
		return 0, fmt.Errorf("%w: limit %d exceeds %d", ErrRangeInvalid, limit, MaxRetain)
	}
	cutoff := s.now().Add(-s.age).Unix()

	pruned := 0
	_, _, err := s.state.Update(ctx, ref.key, func(current channelState, found bool) (channelState, bool, error) {
		pruned = 0
		if !found {
			return current, false, nil
		}
		next := current.clone()
		// The ring is oldest first, so retention only ever removes a prefix.
		// The age comes from the stored timestamp, which is the only thing that
		// timestamp is for — it never decides order.
		for len(next.Ring) > 0 && pruned < limit && next.Ring[0].StoredAtUnix <= cutoff {
			next.Ring = next.Ring[1:]
			pruned++
		}
		if pruned == 0 {
			return current, false, nil
		}
		kept := make([]Message, len(next.Ring))
		copy(kept, next.Ring)
		next.Ring = kept
		next.Evicted += uint64(pruned)
		// Idempotency keys are deliberately NOT dropped with their messages: a
		// replay of a pruned message is then refused with ErrAlreadyPublished
		// instead of being stored again. They are bounded by count in trim.
		return next, true, nil
	})
	if err != nil {
		if errors.Is(err, versionstore.ErrConflict) {
			s.metrics.Conflict("prune")
		} else {
			// Counted as well as returned: the retention loop only logs this,
			// and a prune that fails on every tick must be distinguishable, in
			// the metrics, from a channel with nothing to evict (U-0122).
			s.metrics.Dropped("prune.failed", 1)
		}
		return 0, err
	}
	s.metrics.Dropped("message.evicted."+string(ref.Kind), pruned)
	return pruned, nil
}

func (s *channelStore) Stats(ctx context.Context, ref ChannelRef) (Stats, error) {
	if !ref.valid() {
		return Stats{}, fmt.Errorf("%w: ref is empty, resolve a channel first", ErrChannelInvalid)
	}
	current, found, err := s.state.Get(ctx, ref.key)
	if err != nil || !found {
		return Stats{}, err
	}
	state := current.Value
	return Stats{
		Retained:    len(state.Ring),
		OldestSeq:   state.oldestSeq(),
		LatestSeq:   state.LastSeq,
		Evicted:     state.Evicted,
		RequestKeys: len(state.Requests),
	}, nil
}

var _ Store = (*channelStore)(nil)
