package chat

import "context"

// The transport for Messaging is generated from the interface below.
//
//go:generate go run github.com/tjbdwanghaibo/roost-codegen/cmd/servicerpc -dir .

// Messaging is the cross-process contract: what ANOTHER process may ask of the
// chat service.
//
// Named Messaging rather than Chat because chat.Chat stutters at every call
// site, and because "Chat" would read as the noun this package already has
// several of — a channel, a message, a conversation.
//
// Three of the eight Service methods are not here, and one of the three is
// worth reading closely:
//
//   - Resolve maps a channel and a participant to a ChannelRef. It takes no
//     context and does no I/O; it is a key derivation. More importantly,
//     ChannelRef's identifying field is UNEXPORTED — that is deliberate, so a
//     ref can only come from Resolve — which means a ChannelRef cannot cross a
//     bus at all: every codec would drop the key silently and hand the far
//     side a ref that resolves to nothing. The generator refuses it for
//     exactly that reason, and the refusal is correct rather than an obstacle.
//   - Prune and Stats take a ChannelRef, so they inherit that. They are also
//     maintenance rather than requests: pruning is the owning process's own
//     periodic work and lives in the Server's run hook.
//
// PublishSystem IS here, and it is the interesting inclusion.
//
// This package exists largely to remove a client-supplied Trusted bool that
// was the entire authorization for a system message. So putting the system
// path on a bus deserves a precise answer, and the answer is that nothing
// about the trust decision travels: SystemPublishRequest carries a channel, an
// actor label, a type, a body and an idempotency key, and no token. The token
// is minted at the OWNER, by the deployment's SystemAuthenticator, from the
// transport identity in the handler's own context.
//
// That makes the bus case fail CLOSED rather than fail open. If a deployment's
// bus carries no caller identity the authenticator can attest, it returns no
// grant and PublishSystem refuses with ErrSystemDenied. A missing piece
// produces a refusal, not permission — which is the property the Trusted bool
// got backwards, and the reason this method can be exposed at all.
//
// One thing IS trusted, and it should be said rather than left implicit:
// Publish, History, Conversation and Scrollback all take the caller's identity
// as a parameter, so over the bus the CALLING PROCESS asserts who the sender
// or viewer is. In-process that argument comes from the session; across
// processes it comes from whichever process holds the session — a gateway that
// authenticated the player. chat believes it. That is the trust model of an
// internal bus and it is not specific to this method or this service; it is
// why the bus must not be reachable from outside the deployment. What this
// design does buy is that the identity is a separate ARGUMENT rather than a
// field of the request body, so a player packet forwarded verbatim cannot
// carry one.
//
// No method carries an affinity key. A channel's messages and its sequence
// live in one versioned entry, so a publish is one compare-and-set and
// contention is per channel — which looks like a case for affinity. It is not,
// because the routing key would have to be the channel's resolved stream key,
// and that is what ChannelRef's unexported field holds: not available to a
// caller, by design. Ordering and idempotency come from the per-channel
// sequence inside the CAS, not from routing.
//
//roost:rpc service_type=chat capability=service.chat
type Messaging interface {
	// Publish appends a player's message. The sender is an argument, not a
	// request field, so "the caller is the sender" is not a check that can be
	// switched off.
	Publish(ctx context.Context, from Sender, req PublishRequest) (message Message, err error)

	// PublishSystem appends a message from the game itself. The request
	// carries no proof of anything; the grant is minted at the owner from
	// transport identity, and its absence is a refusal.
	PublishSystem(ctx context.Context, req SystemPublishRequest) (message Message, err error)

	// History pages a channel the viewer may read.
	History(ctx context.Context, viewer Sender, query HistoryQuery) (page Page, err error)

	// Conversation pages the 1:1 conversation between viewer and peer,
	// forwards from a reconnect cursor.
	Conversation(ctx context.Context, viewer Sender, peer int64, afterSeq uint64, limit int) (page Page, err error)

	// Scrollback pages the same conversation backwards, for a client
	// scrolling up.
	Scrollback(ctx context.Context, viewer Sender, peer int64, beforeSeq uint64, limit int) (page Page, err error)
}

var _ Messaging = (*Service)(nil)
