// Package platform is the channel-facing edge: it turns a channel credential
// into a session, and a payment provider's callback into exactly one delivery.
//
// It is a small package guarding two of the highest-consequence paths in a
// game service, and the implementation it replaces got both wrong:
//
//   - Session minting had no authentication at all. `AuthSession` took a
//     player id out of the request body and signed a token for it. Anyone who
//     could reach the endpoint could obtain a valid session for any player.
//     There was no credential, no channel check, nothing to verify — so this
//     package requires a Verifier with no default, the same way account does:
//     the absence of a check must not be able to look like a default.
//
//   - Recharge delivery was stateless and not idempotent. The callback was
//     signature-verified and then handed straight to the deliverer. The
//     `RequestID` field was accepted, forwarded, and never used — not for
//     dedupe, not for correlation, not even in a log line. A signature proves
//     the payload came from the provider; it does not prove this is the first
//     time it arrived. So replaying a captured callback, or a redelivery from
//     the at-least-once transport in front of it, delivered the goods again.
//     The only dedupe was in the game process three hops away, keyed on an
//     order id it happened to also receive.
//
//     Here an order is a durable, insert-only record, and delivery is
//     reserve-before-deliver: the reservation is what makes the second arrival
//     a replay instead of a second grant. A signature that verifies is
//     necessary and not sufficient.
//
// And two smaller ones: the error from a failed delivery was discarded and
// replaced with a constant string, so a payment that failed to deliver left no
// trace on the server at all; and the callback response was the deliverer's
// response passed through unchanged, so a caller could not tell a platform
// refusal from a game refusal.
package platform

import (
	"errors"
	"fmt"
	"strings"

	"github.com/tjbdwanghaibo/roost-core/errcode"
	"github.com/tjbdwanghaibo/roost-core/versionstore"
)

// Error codes.
const (
	CodeOK int32 = 0

	CodeRequestInvalid   int32 = 600101
	CodeSignatureInvalid int32 = 600102
	CodeIdentityDenied   int32 = 600103
	CodeVerifierDown     int32 = 600104
	CodeOrderInvalid     int32 = 600105
	CodeOrderMismatch    int32 = 600106
	CodeDeliveryHeld     int32 = 600107
	CodeDeliveryFailed   int32 = 600108
	CodeDeliveryExpired  int32 = 600109
	CodeConflict         int32 = 600110
	// CodeNotResolvable and CodeAdminNoteRequired belong to the operator
	// surface in admin.go. They are separate codes because they are separate
	// answers: "this order is not in a state a human may change" and "you did
	// not say why".
	CodeNotResolvable     int32 = 600111
	CodeAdminNoteRequired int32 = 600112
	// CodeOrderSettled reports an order a human resolved out of band. It is
	// distinct from CodeDeliveryExpired (attempts ran out) and from
	// CodeDeliveryHeld (an attempt is in flight) because a caller acts on it
	// differently: stop, rather than retry later.
	CodeOrderSettled int32 = 600113
)

var (
	ErrRequestInvalid = errcode.Define(CodeRequestInvalid, "platform: request is invalid", "")
	// ErrSignatureInvalid reports that the payload's signature did not verify.
	// It is deliberately indistinguishable to the caller from a malformed
	// payload of the right shape: a provider integration that is misconfigured
	// and an attacker probing the endpoint should not get different answers.
	ErrSignatureInvalid = errcode.Define(CodeSignatureInvalid, "platform: payload signature is invalid", "")
	ErrIdentityDenied   = errcode.Define(CodeIdentityDenied, "platform: identity was denied by the channel", "")
	// ErrVerifierDown reports that the channel could not be reached. It is
	// distinct from ErrIdentityDenied on purpose: telling a player their
	// credential is bad when the channel is merely unreachable is a support
	// ticket, and it is the answer a service gives when it collapses the two.
	ErrVerifierDown = errcode.Define(CodeVerifierDown, "platform: identity channel is unavailable", "")

	ErrOrderInvalid = errcode.Define(CodeOrderInvalid, "platform: order is invalid", "")
	// ErrOrderMismatch reports that one order id arrived twice with different
	// contents. Neither answer is right and delivering both is the double
	// grant, so it is refused.
	ErrOrderMismatch = errcode.Define(CodeOrderMismatch, "platform: order id was reserved for different contents", "")
	// ErrDeliveryHeld reports that a delivery for this order is in flight.
	ErrDeliveryHeld    = errcode.Define(CodeDeliveryHeld, "platform: a delivery is in flight", "")
	ErrDeliveryFailed  = errcode.Define(CodeDeliveryFailed, "platform: delivery failed", "")
	ErrDeliveryExpired = errcode.Define(CodeDeliveryExpired, "platform: delivery attempts are exhausted", "")
	ErrConflict        = errcode.Define(CodeConflict, "platform: conflict", "")

	// ErrNotResolvable reports that an order is not in a state an operator may
	// change. It is what refuses reopening a DELIVERED order, which would
	// grant the goods a second time — the defect this package exists to make
	// unrepresentable, reached through the operator door instead of the
	// provider's.
	ErrNotResolvable = errcode.Define(CodeNotResolvable, "platform: order is not in a resolvable state", "")
	// ErrAdminNoteRequired reports a missing or oversized operator note.
	ErrAdminNoteRequired = errcode.Define(CodeAdminNoteRequired, "platform: an operator note is required", "")
	// ErrOrderSettled reports that this order was resolved outside the
	// service. Without it a settled order fell through to ErrDeliveryHeld —
	// "a delivery is in flight: order o1 until 0" — which the retry hook
	// treats as benign, so a refunded order would be retried forever and the
	// nonsense message would be the only clue.
	ErrOrderSettled = errcode.Define(CodeOrderSettled, "platform: order was settled out of band", "")
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

// Bounds.
const (
	// MaxPayloadBytes bounds a callback payload. A provider callback arrives
	// over HTTP from outside, so its size is not the caller's to decide.
	MaxPayloadBytes = 16 * 1024
	// MaxProductIDBytes and MaxOrderIDBytes bound the provider-supplied
	// identifiers, which become storage keys.
	MaxProductIDBytes = 128
	MaxOrderIDBytes   = 128
	// MaxOpenIDBytes bounds a channel account identifier.
	MaxOpenIDBytes = 256
	// MaxDeliveryAttempts is the default ceiling on delivery attempts per
	// order. An unbounded retry queue is a queue that never drains and never
	// reports that it is not draining.
	MaxDeliveryAttempts = 8
)

// DeliveryState is where one order's delivery stands.
//
//	reserved ──> delivered
//	    │
//	    └──────> exhausted
//
// There is no "failed" state that a retry moves out of: a failed attempt
// leaves the order reserved with a later deadline, so the only terminal states
// are the two that need no further action.
type DeliveryState string

const (
	// DeliveryReserved means the order is recorded and delivery has not
	// succeeded yet. This is the state that makes a second callback a replay:
	// it exists before the deliverer is called, not after it returns.
	DeliveryReserved DeliveryState = "reserved"
	// DeliveryDelivered is terminal and is what a replay is answered from.
	DeliveryDelivered DeliveryState = "delivered"
	// DeliveryExhausted means the attempt budget is spent. It is terminal and
	// needs an operator: the player paid and did not receive the goods.
	DeliveryExhausted DeliveryState = "exhausted"
	// DeliverySettled means a human resolved this order outside the service —
	// refunded, or granted by hand. It is distinct from DeliveryDelivered on
	// purpose: a reconciliation that counted it as delivered would report a
	// fulfilment that did not happen.
	DeliverySettled DeliveryState = "settled"
)

// Order is one paid purchase and its delivery state.
//
// It is the durable record whose absence made the replaced implementation
// stateless. Everything a replay needs to be answered without delivering
// again is in here.
type Order struct {
	// OrderID is the provider's order identifier and this record's key.
	OrderID string `json:"order_id"`
	// PlayerID is who the goods go to. It comes from the callback payload,
	// which the signature covers — this is the one place a request field is
	// authoritative, and it is authoritative because the provider signed it,
	// not because the client sent it.
	PlayerID int64 `json:"player_id"`
	// Channel and ProductID identify what was bought where.
	Channel   string `json:"channel"`
	ProductID string `json:"product_id"`
	// AmountMinor is the paid amount in the currency's minor unit, as an
	// integer. Money is never a float here.
	AmountMinor int64  `json:"amount_minor"`
	Currency    string `json:"currency,omitempty"`

	// PayloadDigest is a digest of what this order MEANS — who, what, how
	// much — not of the bytes it arrived as. A second callback for the same
	// order id whose digest differs is refused rather than delivered: one
	// order id with two contents is either a provider bug or an attack, and
	// picking one is how a service delivers the wrong goods.
	//
	// It digests meaning rather than bytes so that a provider which
	// re-serializes between retries is still recognized as retrying. See
	// contentDigest.
	PayloadDigest string `json:"payload_digest"`

	State DeliveryState `json:"state"`
	// Attempts counts delivery attempts. It bounds the retry queue and is the
	// signal that a payment is not getting through.
	Attempts int32 `json:"attempts"`
	// MaxAttempts is the ceiling this order was created under, so raising the
	// configured ceiling does not silently revive orders that already gave up.
	MaxAttempts int32 `json:"max_attempts"`
	// NextAttemptAtUnix is when the next attempt may run.
	NextAttemptAtUnix int64 `json:"next_attempt_at_unix,omitempty"`
	// LastError is the last delivery failure's message, kept because the
	// replaced implementation discarded it and substituted a constant, which
	// is how a payment that never delivered left no trace on the server.
	LastError string `json:"last_error,omitempty"`

	PaidAtUnix      int64 `json:"paid_at_unix"`
	CreatedAtUnix   int64 `json:"created_at_unix"`
	DeliveredAtUnix int64 `json:"delivered_at_unix,omitempty"`
	UpdatedAtUnix   int64 `json:"updated_at_unix"`

	// Reopens counts how many times an operator returned this order to the
	// retry queue. It is kept rather than reset because "we reopened this five
	// times and it still fails" is the fact that stops someone reopening it a
	// sixth time, and resetting Attempts alone would erase it.
	Reopens int32 `json:"reopens,omitempty"`
	// AdminNote and AdminActionAtUnix record the last operator intervention.
	// The note is required by admin.go: an intervention on a paid order with
	// no recorded reason cannot be reviewed.
	AdminNote         string `json:"admin_note,omitempty"`
	AdminActionAtUnix int64  `json:"admin_action_at_unix,omitempty"`
}

// Terminal reports whether the order needs no further delivery work.
func (o Order) Terminal() bool {
	return o.State == DeliveryDelivered || o.State == DeliveryExhausted ||
		o.State == DeliverySettled
}

// Due reports whether a delivery attempt may run now.
func (o Order) Due(nowUnix int64) bool {
	if o.Terminal() {
		return false
	}
	return o.NextAttemptAtUnix == 0 || nowUnix >= o.NextAttemptAtUnix
}

func (o Order) Validate() error {
	if strings.TrimSpace(o.OrderID) == "" {
		return fmt.Errorf("%w: order id is empty", ErrOrderInvalid)
	}
	if len(o.OrderID) > MaxOrderIDBytes {
		return fmt.Errorf("%w: order id is %d bytes, limit %d", ErrOrderInvalid, len(o.OrderID), MaxOrderIDBytes)
	}
	if o.PlayerID <= 0 {
		return fmt.Errorf("%w: player id must be positive", ErrOrderInvalid)
	}
	if strings.TrimSpace(o.ProductID) == "" {
		return fmt.Errorf("%w: product id is empty", ErrOrderInvalid)
	}
	if len(o.ProductID) > MaxProductIDBytes {
		return fmt.Errorf("%w: product id is %d bytes, limit %d", ErrOrderInvalid, len(o.ProductID), MaxProductIDBytes)
	}
	if o.AmountMinor <= 0 {
		// A zero-amount paid order is either a test payload that reached
		// production or a provider bug. Delivering goods for it is free money.
		return fmt.Errorf("%w: paid amount must be positive, got %d", ErrOrderInvalid, o.AmountMinor)
	}
	if o.MaxAttempts <= 0 {
		return fmt.Errorf("%w: max attempts must be positive", ErrOrderInvalid)
	}
	return nil
}
