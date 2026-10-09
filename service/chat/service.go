package chat

import (
	"context"
	"fmt"
)

// ServiceConfig wires a Service.
type ServiceConfig struct {
	// Store is the persistence and authorization seam. Required.
	Store Store
	// System authenticates the privileged entry point from transport identity.
	//
	// Nil is a valid, and safe, configuration: it means this deployment has no
	// system path at all and PublishSystem always refuses. That is the shape
	// the defect demanded — trust is a capability a deployment grants, not a
	// field a request sets — and it fails closed, unlike the boolean it
	// replaces, whose absence from a request was the only thing keeping the
	// hole shut.
	System SystemAuthenticator
	// PruneChannels enumerates the channels this process prunes, once per
	// retention tick. A deployment knows its channels — a world channel, its
	// group channels, the pair channels of its online players — and this
	// package cannot enumerate them without an unbounded keyspace scan. Nil
	// means this process prunes nothing, which the server logs at start:
	// retention that nobody runs is the "looks present, does nothing"
	// mechanism this package criticises in the implementation it replaced.
	PruneChannels func(context.Context) []ChannelRef
	// ConversationKind is the pair-scoped kind Conversation queries; zero
	// selects ChannelPrivate.
	ConversationKind ChannelKind
}

// Service is the chat service.
//
// It is deliberately thin over Store: the authorization, the type registry and
// the bounds live in the seam, so a caller that reaches the store directly
// cannot lose them. What the service adds is the privileged entry point, which
// is the only way to speak as the game, and the conversation helper.
type Service struct {
	cfg ServiceConfig
}

func NewService(cfg ServiceConfig) (*Service, error) {
	if cfg.Store == nil {
		return nil, fmt.Errorf("chat: store is required")
	}
	if cfg.ConversationKind == "" {
		cfg.ConversationKind = ChannelPrivate
	}
	return &Service{cfg: cfg}, nil
}

// Publish stores a role's message.
//
// from is the session's identity, established by the transport, and the request
// has no sender field for it to disagree with. "The caller is the sender" is
// therefore not a check that can be disabled — it is the only representable
// state.
func (s *Service) Publish(ctx context.Context, from Sender, req PublishRequest) (Message, error) {
	return s.cfg.Store.Append(ctx, from, req)
}

// PublishSystem stores a message from the game itself. This is the *only* way
// a system message can be produced.
//
// The trust decision is made here, from the transport identity in ctx, by an
// authenticator the deployment supplied — never from anything the request
// carried. A service with no authenticator refuses outright rather than
// treating the missing piece as permission.
func (s *Service) PublishSystem(ctx context.Context, req SystemPublishRequest) (Message, error) {
	if s.cfg.System == nil {
		return Message{}, fmt.Errorf("%w: this service is wired without a system authenticator", ErrSystemDenied)
	}
	token, err := s.cfg.System.AuthenticateSystem(ctx)
	if err != nil {
		return Message{}, fmt.Errorf("%w: %w", ErrSystemDenied, err)
	}
	if !token.valid() {
		// An authenticator that returns no error and no grant has not decided
		// anything. Fail closed: a verifier that cannot say yes says no.
		return Message{}, fmt.Errorf("%w: the authenticator granted no token", ErrSystemDenied)
	}
	return s.cfg.Store.AppendSystem(ctx, token, req)
}

// History pages a channel the viewer may read.
func (s *Service) History(ctx context.Context, viewer Sender, query HistoryQuery) (Page, error) {
	return s.cfg.Store.History(ctx, viewer, query)
}

// Conversation pages the 1:1 conversation between viewer and peer.
//
// It exists because this was impossible before. Private history filtered on
// {channel, target_id} with target_id forced to equal the caller, while
// documents recorded the *recipient* there — so the query returned only what
// the caller had received, never what it had sent, and could not be narrowed to
// one peer at all. Here the pair is the stream: one query, both directions,
// including the viewer's own messages.
//
// afterSeq is the reconnect cursor (zero for the newest page); limit is
// required and bounded like any other listing.
func (s *Service) Conversation(ctx context.Context, viewer Sender, peer int64, afterSeq uint64, limit int) (Page, error) {
	if peer <= 0 {
		return Page{}, fmt.Errorf("%w: peer role id must be positive, got %d", ErrChannelInvalid, peer)
	}
	return s.cfg.Store.History(ctx, viewer, HistoryQuery{
		Channel:  Channel{Kind: s.cfg.ConversationKind, Target: peer},
		AfterSeq: afterSeq,
		Limit:    limit,
	})
}

// Scrollback pages the same conversation backwards, for a client scrolling up.
func (s *Service) Scrollback(ctx context.Context, viewer Sender, peer int64, beforeSeq uint64, limit int) (Page, error) {
	if peer <= 0 {
		return Page{}, fmt.Errorf("%w: peer role id must be positive, got %d", ErrChannelInvalid, peer)
	}
	return s.cfg.Store.History(ctx, viewer, HistoryQuery{
		Channel:   Channel{Kind: s.cfg.ConversationKind, Target: peer},
		BeforeSeq: beforeSeq,
		Limit:     limit,
	})
}

// Resolve maps a channel and a participant to their stream.
func (s *Service) Resolve(ch Channel, participant int64) (ChannelRef, error) {
	return s.cfg.Store.Resolve(ch, participant)
}

// pruneTargets is what the retention loop prunes on one tick.
func (s *Service) pruneTargets(ctx context.Context) []ChannelRef {
	if s.cfg.PruneChannels == nil {
		return nil
	}
	return s.cfg.PruneChannels(ctx)
}

// Prune drops messages past the retention age, up to limit. It is the sweep
// half of retention: the ring bounds a channel's size on every append, and this
// bounds a message's age. Neither is a TTL, and neither is claimed to be.
func (s *Service) Prune(ctx context.Context, ref ChannelRef, limit int) (int, error) {
	return s.cfg.Store.Prune(ctx, ref, limit)
}

// Stats reports a channel's depth and drop counters.
func (s *Service) Stats(ctx context.Context, ref ChannelRef) (Stats, error) {
	return s.cfg.Store.Stats(ctx, ref)
}
