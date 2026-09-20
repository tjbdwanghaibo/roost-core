package platform

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/tjbdwanghaibo/roost-core/security"
	"github.com/tjbdwanghaibo/roost-core/versionstore"

	"github.com/tjbdwanghaibo/roost-core/kit/service/servicemetrics"
)

// OrderStore holds orders. It is versioned, so a delivery attempt cannot be
// recorded with an unconditional write — the defect that made the replaced
// implementation's retry counters meaningless wherever it appeared.
type OrderStore = versionstore.Store[string, Order]

// Deliverer hands the goods for one paid order to whatever grants them.
//
// It is called at most once per attempt, always AFTER the order is durably
// reserved. That order is the point: the reservation is what turns a second
// callback into a replay. Calling the deliverer first and recording afterwards
// — which is what the replaced implementation did, having nothing to record —
// means a crash between the two delivers the goods with no record that it did.
//
// An implementation may additionally dedupe on OrderID. It should not have to:
// this package guarantees at-most-one successful delivery per order. But
// belt-and-braces here is cheap, and the replaced implementation's ONLY
// protection was a dedupe three hops away, so the habit is worth keeping.
type Deliverer interface {
	Deliver(ctx context.Context, order Order) error
}

// DelivererFunc adapts a function to Deliverer.
type DelivererFunc func(context.Context, Order) error

// Deliver implements Deliverer.
func (f DelivererFunc) Deliver(ctx context.Context, order Order) error { return f(ctx, order) }

// Config wires a Service.
type Config struct {
	// Orders holds the durable order records.
	Orders OrderStore
	// Pending names the paid orders this deployment is still waiting to
	// deliver, so the Server's background loop has something to retry.
	// Optional and deliberately not defaulted: enumerating every order on a
	// timer is the unbounded scan this package refuses to do, and only the
	// deployment knows what index it keeps.
	//
	// Leaving it nil turns background recovery OFF — a real choice for a
	// deployment whose channel re-delivers callbacks, and one the service
	// states rather than mimes: the loop used to call a private method that
	// always returned nil, so a recoverable failure could hang forever with
	// nothing in the logs (RR-20260917-04).
	Pending PendingOrders
	// Deliver grants the goods. Required.
	Deliver Deliverer

	// Verifier and Players authenticate a session request. Both are required
	// and neither has a default; see their interfaces for why.
	Verifier Verifier
	Players  PlayerResolver

	// SessionSecret signs session tokens. Required and non-empty.
	SessionSecret string
	// SessionTTL bounds a token's life; zero selects DefaultSessionTTL.
	SessionTTL time.Duration

	// PaymentSecret verifies provider callback signatures. Required and
	// non-empty: an empty secret in the replaced implementation was checked
	// at call time and turned every callback into an invalid-signature
	// refusal, which is a silent outage rather than a startup failure.
	PaymentSecret string

	// DeliveryAttempts is the per-order attempt ceiling; zero selects
	// MaxDeliveryAttempts. It must be positive.
	DeliveryAttempts int
	// DeliveryBackoff is the base delay between attempts; the delay grows
	// with the attempt count so a game that is down is not hammered. Zero
	// selects DefaultDeliveryBackoff.
	DeliveryBackoff time.Duration

	// Now is the clock; nil means time.Now.
	Now func() time.Time
	// Metrics receives reports. A nil reporter means no reporting and never
	// fails an operation.
	//
	// This package had zero observability: no slog line, no counter, and the
	// error from a failed delivery was discarded and replaced with a constant
	// string. A payment that failed to deliver left no trace on the server.
	Metrics servicemetrics.Reporter
}

// PendingOrders is the deployment's index of paid-but-undelivered orders.
//
// The contract is bounded and fallible on purpose: limit is how many ids the
// caller can use this tick (so the source pages rather than loading
// everything), and an error is reported and retried next tick rather than
// taking the process down. Ids the service has already settled may appear —
// the loop skips them and says so, which is the signal that the index needs
// to retire them.
type PendingOrders interface {
	PendingOrders(ctx context.Context, limit int) ([]string, error)
}

// PendingOrdersFunc adapts a function to PendingOrders.
type PendingOrdersFunc func(context.Context, int) ([]string, error)

// PendingOrders implements PendingOrders.
func (f PendingOrdersFunc) PendingOrders(ctx context.Context, limit int) ([]string, error) {
	return f(ctx, limit)
}

// Defaults.
const (
	// RetryBatch is how many pending orders one tick asks for. The bound is
	// the point: a deployment with a large backlog pages through it instead
	// of building one giant slice per tick.
	RetryBatch = 128

	DefaultSessionTTL      = 30 * time.Minute
	DefaultDeliveryBackoff = 5 * time.Second

	// maxBackoffMultiplier caps the growth of the retry delay, so a
	// long-lived order does not compute a delay measured in hours.
	maxBackoffMultiplier = 8
)

// Service is the channel-facing edge.
type Service struct {
	cfg    Config
	report servicemetrics.Sink
}

// New validates the configuration and returns a Service.
func New(cfg Config) (*Service, error) {
	missing := []string{}
	if cfg.Orders == nil {
		missing = append(missing, "order store")
	}
	if cfg.Deliver == nil {
		missing = append(missing, "deliverer")
	}
	if cfg.Verifier == nil {
		missing = append(missing, "identity verifier")
	}
	if cfg.Players == nil {
		missing = append(missing, "player resolver")
	}
	if strings.TrimSpace(cfg.SessionSecret) == "" {
		missing = append(missing, "session secret")
	}
	if strings.TrimSpace(cfg.PaymentSecret) == "" {
		missing = append(missing, "payment secret")
	}
	if len(missing) > 0 {
		// Refusing at construction rather than at call time is the point. An
		// empty payment secret in the replaced implementation turned every
		// callback into an invalid-signature refusal — an outage that looked
		// like an attack.
		return nil, fmt.Errorf("platform: incomplete configuration: %s", strings.Join(missing, ", "))
	}
	if cfg.SessionTTL <= 0 {
		cfg.SessionTTL = DefaultSessionTTL
	}
	if cfg.DeliveryAttempts < 0 {
		return nil, fmt.Errorf("platform: delivery attempts must not be negative")
	}
	if cfg.DeliveryAttempts == 0 {
		cfg.DeliveryAttempts = MaxDeliveryAttempts
	}
	if cfg.DeliveryBackoff < 0 {
		return nil, fmt.Errorf("platform: delivery backoff must not be negative")
	}
	if cfg.DeliveryBackoff == 0 {
		cfg.DeliveryBackoff = DefaultDeliveryBackoff
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Service{cfg: cfg, report: servicemetrics.Wrap(cfg.Metrics)}, nil
}

// Session is a signed session for one player.
type Session struct {
	PlayerID      int64
	Channel       string
	Token         string
	ExpiresAtUnix int64
}

// AuthSession verifies a credential with its channel and returns a session
// for the player that channel identifies.
//
// Verification happens first and its result — not anything the caller sent —
// decides the player. The replaced implementation had no verification step at
// all: it read a player id out of the request body and signed a token for it.
func (s *Service) AuthSession(ctx context.Context, credential Credential) (Session, error) {
	if err := credential.Validate(); err != nil {
		return Session{}, err
	}
	verified, err := s.cfg.Verifier.Verify(ctx, credential)
	if err != nil {
		if errors.Is(err, ErrVerifierDown) {
			// Propagated as-is: a channel outage is not a client error, and
			// answering "denied" would tell the player their credential is
			// bad when it is not.
			s.report.Refused("auth_session", "verifier_down")
			return Session{}, err
		}
		s.report.Refused("auth_session", "denied")
		return Session{}, fmt.Errorf("%w: %s", ErrIdentityDenied, err)
	}
	if err := verified.Validate(); err != nil {
		return Session{}, err
	}
	// A verifier that answers about a different channel than the one asked is
	// either misrouted or lying; either way the answer is not about this
	// request.
	if verified.Channel != credential.Channel {
		s.report.Refused("auth_session", "channel_mismatch")
		return Session{}, fmt.Errorf("%w: verifier answered for channel %q, asked about %q",
			ErrIdentityDenied, verified.Channel, credential.Channel)
	}

	playerID, err := s.cfg.Players.Resolve(ctx, verified)
	if err != nil {
		return Session{}, err
	}
	if playerID <= 0 {
		return Session{}, fmt.Errorf("platform: player resolver returned a non-positive id %d", playerID)
	}

	now := s.cfg.Now()
	token, err := security.SignSessionToken(playerID, s.cfg.SessionSecret, s.cfg.SessionTTL, now)
	if err != nil {
		return Session{}, err
	}
	s.report.Accepted("auth_session")
	return Session{
		PlayerID: playerID, Channel: verified.Channel, Token: token,
		ExpiresAtUnix: now.Add(s.cfg.SessionTTL).Unix(),
	}, nil
}

// ValidateSession verifies a session token.
func (s *Service) ValidateSession(playerID int64, token string) error {
	if _, err := security.VerifySessionToken(token, s.cfg.SessionSecret, playerID, s.cfg.Now()); err != nil {
		s.report.Refused("validate_session", "bad_token")
		return fmt.Errorf("platform: session is invalid: %w", err)
	}
	return nil
}

// callbackPayload is the provider's signed callback, as it arrives on the
// wire. It is a separate type from Order on purpose: the replaced
// implementation converted between two identically-shaped structs with a
// struct cast, so either side gaining a field or reordering one would have
// silently misaligned the values.
type callbackPayload struct {
	OrderID     string `json:"order_id"`
	PlayerID    int64  `json:"player_id"`
	Channel     string `json:"channel"`
	ProductID   string `json:"product_id"`
	AmountMinor int64  `json:"amount_minor"`
	Currency    string `json:"currency,omitempty"`
	PaidAtUnix  int64  `json:"paid_at_unix,omitempty"`
}

// Receipt is what a callback produced.
type Receipt struct {
	Order Order
	// Replayed reports that this callback had already been recorded. A
	// provider that retries — they all do — gets a truthful answer rather
	// than a second delivery.
	Replayed bool
	// Delivered reports whether the goods are now granted. A callback may be
	// accepted (durably recorded) without being delivered yet, and saying so
	// is what lets a caller answer the provider "received" while the delivery
	// is still being retried.
	Delivered bool
}

// HandleCallback records a provider callback and attempts delivery once.
//
// The order of operations is the fix, and it is worth stating plainly because
// the replaced implementation had the other one:
//
//  1. verify the signature — necessary, and NOT sufficient;
//  2. decode and validate;
//  3. reserve the order, insert-only. A second arrival loses here and is
//     answered as a replay;
//  4. only then call the deliverer.
//
// Steps 1 and 4 were the whole of the replaced implementation. A signature
// proves the payload came from the provider; it says nothing about whether
// this is the first time it arrived. So a captured callback replayed later, or
// a redelivery from the at-least-once transport in front of it, granted the
// goods again.
func (s *Service) HandleCallback(ctx context.Context, raw []byte, signature string) (Receipt, error) {
	if len(raw) == 0 {
		return Receipt{}, fmt.Errorf("%w: payload is empty", ErrRequestInvalid)
	}
	if len(raw) > MaxPayloadBytes {
		// Bounded before anything parses it: this input arrives from outside.
		s.report.Refused("callback", "payload_too_large")
		return Receipt{}, fmt.Errorf("%w: payload is %d bytes, limit %d",
			ErrRequestInvalid, len(raw), MaxPayloadBytes)
	}
	if !security.VerifyPayloadSignature(raw, signature, s.cfg.PaymentSecret) {
		s.report.Refused("callback", "bad_signature")
		return Receipt{}, ErrSignatureInvalid
	}

	var payload callbackPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		// The signature verified, so this is a provider whose payload shape
		// changed, not an attacker. Distinguishing it from a bad signature in
		// the counters is how that gets noticed.
		s.report.Refused("callback", "malformed_payload")
		return Receipt{}, fmt.Errorf("%w: %s", ErrRequestInvalid, err)
	}

	now := s.cfg.Now()
	paidAt := payload.PaidAtUnix
	if paidAt == 0 {
		paidAt = now.Unix()
	}
	order := Order{
		OrderID: strings.TrimSpace(payload.OrderID), PlayerID: payload.PlayerID,
		Channel: payload.Channel, ProductID: strings.TrimSpace(payload.ProductID),
		AmountMinor: payload.AmountMinor, Currency: payload.Currency,
		PayloadDigest: "",
		State:         DeliveryReserved,
		MaxAttempts:   int32(s.cfg.DeliveryAttempts),
		PaidAtUnix:    paidAt,
		CreatedAtUnix: now.Unix(),
		UpdatedAtUnix: now.Unix(),
	}
	if err := order.Validate(); err != nil {
		s.report.Refused("callback", "order_invalid")
		return Receipt{}, err
	}
	order.PayloadDigest = contentDigest(order)

	// Insert-only. This is what makes the second arrival a replay rather than
	// a second grant, and it is insert-only rather than read-then-write
	// because two concurrent callbacks both pass a read-then-write.
	_, created, err := s.cfg.Orders.Create(ctx, order.OrderID, order)
	if err != nil {
		return Receipt{}, err
	}
	if !created {
		existing, found, err := s.cfg.Orders.Get(ctx, order.OrderID)
		if err != nil {
			return Receipt{}, err
		}
		if !found {
			return Receipt{}, fmt.Errorf("%w: order %s vanished during create", ErrConflict, order.OrderID)
		}
		// One order id, two different payloads. Neither answer is right and
		// delivering both is the double grant.
		if subtle.ConstantTimeCompare(
			[]byte(existing.Value.PayloadDigest), []byte(order.PayloadDigest)) != 1 {
			s.report.Refused("callback", "order_mismatch")
			return Receipt{}, fmt.Errorf("%w: order %s", ErrOrderMismatch, order.OrderID)
		}
		s.report.Replayed("callback")
		receipt := Receipt{Order: existing.Value, Replayed: true,
			Delivered: existing.Value.State == DeliveryDelivered}
		if receipt.Delivered {
			return receipt, nil
		}
		// The order is recorded but undelivered: drive one more attempt, so a
		// provider retry does useful work instead of only being told "known".
		attempt, err := s.AttemptDelivery(ctx, order.OrderID)
		if err != nil {
			return receipt, err
		}
		receipt.Order, receipt.Delivered = attempt.Order, attempt.Delivered
		return receipt, nil
	}
	s.report.Accepted("callback")

	attempt, err := s.AttemptDelivery(ctx, order.OrderID)
	if err != nil {
		// The order is durably recorded, so this is a delivery that has not
		// happened YET, not a payment that was lost. Returning both the
		// record and the error is what lets a caller answer the provider
		// "received" and still see that delivery is outstanding.
		return Receipt{Order: order}, err
	}
	// Replayed travels with the receipt: when this attempt was overtaken by a
	// retry that committed first, the caller must be able to tell "I granted
	// the goods" from "they were already granted" — the distinction this
	// function's own contract insists on. Dropping the flag here erased it
	// for every callback-driven delivery (U-0018).
	return Receipt{Order: attempt.Order, Delivered: attempt.Delivered, Replayed: attempt.Replayed}, nil
}

// AttemptDelivery drives one delivery attempt for one order.
//
// It is exported so a sweep can retry orders that are due — a payment that
// failed to deliver has to be retried by something, and in the replaced
// implementation there was nothing: the error was discarded, no record
// existed, and the only path was the provider calling back again.
//
// At most one attempt is in flight at a time, and that is enforced INSIDE the
// compare-and-set that claims the attempt, not by a check before it. The
// difference is not academic: with the check outside, a sweep and a provider
// retry that arrive together both read a due order, both pass, and both call
// the deliverer — so the granting side is asked to grant twice and the only
// thing that saves it is a dedupe of its own. Pushing the decision into the
// CAS is what makes "the deliverer must dedupe" unnecessary rather than
// load-bearing. Getting this wrong is the read-decide-write pattern this whole
// repository exists to remove, and the first version of this function had it.
//
// Idempotent: an order that is already delivered returns without calling the
// deliverer — and says so. It returns a Receipt rather than an Order because
// "I granted the goods" and "they were already granted" must be
// distinguishable to the caller. Two success branches returning identical
// values is what made the replaced implementation's cancel path unusable, and
// here the stakes are a payment.
func (s *Service) AttemptDelivery(ctx context.Context, orderID string) (Receipt, error) {
	if strings.TrimSpace(orderID) == "" {
		return Receipt{}, fmt.Errorf("%w: order id is empty", ErrRequestInvalid)
	}
	now := s.cfg.Now()
	nowUnix := now.Unix()

	// The claim and every decision that gates it happen in one compare-and-set.
	var (
		claimed   Order
		outcome   string
		exhausted bool
	)
	_, _, err := s.cfg.Orders.Update(ctx, orderID, func(current Order, found bool) (Order, bool, error) {
		outcome, exhausted = "", false
		_ = found
		if !found {
			return current, false, fmt.Errorf("%w: order %s is not recorded", ErrOrderInvalid, orderID)
		}
		switch {
		case current.State == DeliveryDelivered:
			claimed, outcome = current, "already_delivered"
			return current, false, nil
		case current.State == DeliveryExhausted:
			claimed, outcome = current, "exhausted"
			return current, false, nil
		case current.State == DeliverySettled:
			// Terminal, and NOT the same answer as "held". A settled order has
			// Due() false because it is Terminal, so without this case it fell
			// through to the not-due branch and reported ErrDeliveryHeld —
			// which the retry hook treats as benign, so a refunded order would
			// be retried on every tick forever.
			claimed, outcome = current, "settled"
			return current, false, nil
		case !current.Due(nowUnix):
			// Either a backoff that has not elapsed or another attempt in
			// flight. Both mean "not now", and both are the same refusal.
			claimed, outcome = current, "held"
			return current, false, nil
		case current.Attempts >= current.MaxAttempts:
			// The budget was already spent; record the terminal state rather
			// than leaving an order that looks retryable forever.
			current.State = DeliveryExhausted
			current.NextAttemptAtUnix = 0
			current.UpdatedAtUnix = nowUnix
			claimed, outcome, exhausted = current, "exhausted", true
			return current, true, nil
		}
		// The attempt is claimed BEFORE the deliverer runs, so a crash
		// mid-flight consumes an attempt rather than being invisible. An
		// attempt counter incremented after a successful call counts
		// successes, which is not what a retry budget is for.
		current.Attempts++
		current.NextAttemptAtUnix = now.Add(s.backoff(int(current.Attempts))).Unix()
		current.UpdatedAtUnix = nowUnix
		claimed, outcome = current, "claimed"
		return current, true, nil
	})
	if err != nil {
		return Receipt{}, err
	}
	switch outcome {
	case "settled":
		return Receipt{Order: claimed}, fmt.Errorf("%w: order %s", ErrOrderSettled, orderID)
	case "already_delivered":
		s.report.Replayed("deliver")
		return Receipt{Order: claimed, Replayed: true, Delivered: true}, nil
	case "exhausted":
		if exhausted {
			s.report.Dropped("order.exhausted", 1)
		} else {
			s.report.Refused("deliver", "exhausted")
		}
		return Receipt{Order: claimed}, fmt.Errorf("%w: order %s after %d attempts",
			ErrDeliveryExpired, orderID, claimed.Attempts)
	case "held":
		s.report.Refused("deliver", "held")
		return Receipt{Order: claimed}, fmt.Errorf("%w: order %s until %d",
			ErrDeliveryHeld, orderID, claimed.NextAttemptAtUnix)
	}

	deliverErr := s.cfg.Deliver.Deliver(ctx, claimed)
	if deliverErr != nil {
		// The error is RECORDED, not discarded and replaced with a constant.
		// A payment that failed to deliver leaving no trace on the server is
		// the confirmed defect this answers.
		failed, err := s.recordFailure(ctx, orderID, deliverErr, nowUnix)
		if err != nil {
			return Receipt{}, err
		}
		if failed.State == DeliveryExhausted {
			s.report.Dropped("order.exhausted", 1)
			return Receipt{Order: failed}, fmt.Errorf("%w: order %s after %d attempts: %w",
				ErrDeliveryExpired, orderID, failed.Attempts, deliverErr)
		}
		s.report.Refused("deliver", "failed")
		return Receipt{Order: failed}, fmt.Errorf("%w: order %s attempt %d: %w",
			ErrDeliveryFailed, orderID, failed.Attempts, deliverErr)
	}

	var (
		result Order
		raced  bool
	)
	_, _, err = s.cfg.Orders.Update(ctx, orderID, func(current Order, found bool) (Order, bool, error) {
		raced = false
		if !found {
			return current, false, fmt.Errorf("%w: order %s vanished", ErrConflict, orderID)
		}
		if current.State == DeliveryDelivered {
			// Idempotent: the delivery time is not moved, or "when was order
			// X delivered" becomes whenever it was last retried.
			result, raced = current, true
			return current, false, nil
		}
		current.State = DeliveryDelivered
		current.DeliveredAtUnix = nowUnix
		current.UpdatedAtUnix = nowUnix
		current.NextAttemptAtUnix = 0
		current.LastError = ""
		result = current
		return current, true, nil
	})
	if err != nil {
		return Receipt{}, err
	}
	if raced {
		// Another attempt committed while this one was in the deliverer. The
		// goods were granted twice by two callers, which the in-CAS claim
		// above is there to prevent — so if this is ever reached it is a bug
		// worth seeing, not a routine replay.
		s.report.Conflict("deliver")
		return Receipt{Order: result, Replayed: true, Delivered: true}, nil
	}
	s.report.Accepted("deliver")
	return Receipt{Order: result, Delivered: true}, nil
}

func (s *Service) recordFailure(ctx context.Context, orderID string, cause error, nowUnix int64) (Order, error) {
	var result Order
	_, _, err := s.cfg.Orders.Update(ctx, orderID, func(current Order, found bool) (Order, bool, error) {
		if !found {
			return current, false, fmt.Errorf("%w: order %s vanished", ErrConflict, orderID)
		}
		if current.State == DeliveryDelivered {
			result = current
			return current, false, nil
		}
		current.LastError = truncate(cause.Error(), 512)
		current.UpdatedAtUnix = nowUnix
		if current.Attempts >= current.MaxAttempts {
			current.State = DeliveryExhausted
			current.NextAttemptAtUnix = 0
		}
		result = current
		return current, true, nil
	})
	if err != nil {
		return Order{}, err
	}
	return result, nil
}

// backoff grows the retry delay with the attempt count, capped so a long-lived
// order does not compute a delay measured in hours.
func (s *Service) backoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	multiplier := attempt
	if multiplier > maxBackoffMultiplier {
		multiplier = maxBackoffMultiplier
	}
	return s.cfg.DeliveryBackoff * time.Duration(multiplier)
}

// Order reads one order. Operator-facing, and the answer to "did this payment
// deliver" that the replaced implementation could not give at all.
func (s *Service) Order(ctx context.Context, orderID string) (Order, bool, error) {
	stored, found, err := s.cfg.Orders.Get(ctx, orderID)
	if err != nil || !found {
		return Order{}, false, err
	}
	return stored.Value, true, nil
}

// SignPayload signs a payload with a secret. Exported for provider
// integrations and for tests that need to produce a valid callback.
func SignPayload(raw []byte, secret string) string { return security.SignPayload(raw, secret) }

// contentDigest is what distinguishes two callbacks that claim the same order
// id.
//
// It digests the order's MEANING — who, what, how much — and not the raw
// signed bytes. Digesting the bytes would be simpler and would be wrong: a
// provider that re-serializes its payload between retries (different field
// order, a whitespace change, an added field) would produce a different digest
// for the same purchase, and this service would answer its retry with
// ErrOrderMismatch instead of recognizing a replay. The provider would then
// retry forever against a payment that is already recorded.
//
// So the comparison is on the fields that decide what gets granted. Two
// callbacks that agree on all of them are the same purchase however they were
// serialized; two that disagree on any of them are refused, because delivering
// either one would be picking.
func contentDigest(order Order) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{
		order.OrderID,
		strconv.FormatInt(order.PlayerID, 10),
		order.Channel,
		order.ProductID,
		strconv.FormatInt(order.AmountMinor, 10),
		order.Currency,
	}, "\x00")))
	return hex.EncodeToString(sum[:])
}

func truncate(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	return text[:limit]
}

// pendingOrderIDs asks the deployment's index for the orders to retry this
// tick. No source configured is not an error: it is background recovery
// switched off, which BackgroundRetryEnabled reports.
func (s *Service) pendingOrderIDs(ctx context.Context) ([]string, error) {
	if s == nil || s.cfg.Pending == nil {
		return nil, nil
	}
	return s.cfg.Pending.PendingOrders(ctx, RetryBatch)
}

// BackgroundRetryEnabled reports whether this service has a source of paid
// orders to retry. A deployment that expects recovery and reads false here
// has forgotten to wire its index.
func (s *Service) BackgroundRetryEnabled() bool { return s != nil && s.cfg.Pending != nil }
