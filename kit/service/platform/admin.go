package platform

import (
	"context"
	"fmt"
	"strings"
)

// Admin is the operator surface: the two things a human can do about an order
// that the automatic paths have given up on.
//
// # Why this exists at all
//
// An order whose delivery attempts run out reaches DeliveryExhausted, and
// before this file that state was a DEAD END in the strict sense: the player
// paid, the money is recorded, the goods are not delivered, and no code path
// anywhere could change that. AttemptDelivery refuses an exhausted order —
// correctly, because a retry loop that ignored the budget would not have a
// budget — so the only trace was one slog.Error line, which is not a work
// queue and does not survive log rotation.
//
// This is the highest-consequence dead end in the repository. It is also the
// one an operator is most certain to hit, because a deliverer that is down for
// longer than MaxDeliveryAttempts × the backoff will exhaust every order that
// arrives during the outage.
//
// # What this deliberately does NOT do
//
// It does not enumerate exhausted orders, and that is a correction to a claim
// made while planning this step. "We cannot find the exhausted orders" sounds
// like the problem and is not: the PAYMENT PROVIDER has the authoritative list
// of what it charged for, every provider publishes a reconciliation report,
// and the question an operator actually asks is "for each order the provider
// says it charged, did we deliver?" — which Service.Order already answers per
// id. Building an index here would duplicate a source of truth this service
// does not own, and it would need to be written outside the compare-and-set
// that sets the terminal state, so it could disagree with the order records it
// indexes.
//
// The real gap was narrower and is what this file closes: once an operator has
// the id, there was no way to ACT.
//
// # No bus transport
//
// There is no //roost:rpc marker here, on purpose. Both methods are more
// dangerous than anything on the Platform interface — one can cause a second
// grant, the other declares a debt settled — and the bus carries no caller
// identity this service can verify. A plain bus method would mean any process
// on the bus can reopen any order. So Admin is reachable only in the process
// that OWNS platform, and how a human reaches that process — an internal
// listener, a CLI against the same Redis, an operator RPC the deployment
// authenticates itself — is a deployment decision, made where the credentials
// live.
//
// This is the same shape as chat.SystemAuthenticator: the trust decision
// happens at the owner, from something the deployment supplies, and its
// absence is a refusal rather than permission.
type Admin interface {
	// ReopenDelivery returns an exhausted order to the retry queue with a
	// fresh attempt budget, so the Server's run hook picks it up again. Use
	// it after fixing whatever the deliverer was failing on.
	ReopenDelivery(ctx context.Context, orderID string, note string) (order Order, err error)

	// SettleOutOfBand records that an exhausted order was resolved outside
	// this service — refunded, or granted by hand — so it stops being owed
	// without being counted as a delivery.
	SettleOutOfBand(ctx context.Context, orderID string, note string) (order Order, err error)
}

// MaxAdminNoteBytes bounds an operator note. Notes are stored on the order and
// read back by whoever looks at it next, so they are bounded like any other
// caller-controlled string in this package.
const MaxAdminNoteBytes = 512

// ReopenDelivery implements Admin.
//
// Every decision is inside the compare-and-set, for the reason the rest of
// this package is: two operators clicking the same button, or one clicking
// twice, must not both succeed at reopening.
//
// The state check is the load-bearing one. Reopening is the only operation in
// this service that can cause a SECOND GRANT — it hands a paid order back to
// the deliverer — so it is allowed from DeliveryExhausted and from nothing
// else. In particular:
//
//   - A DeliveryDelivered order is refused. Reopening it would deliver the
//     goods twice, which is the defect this whole package was rewritten to
//     make unrepresentable, reintroduced through the operator door.
//   - A DeliverySettled order is refused. Someone already decided this debt
//     was paid another way; delivering as well means paying twice.
//   - A DeliveryReserved order is refused, and is told so distinctly: it is
//     already retryable, so an operator whose call timed out and retried gets
//     an answer it can act on rather than a generic failure.
func (s *Service) ReopenDelivery(ctx context.Context, orderID string, note string) (Order, error) {
	note, err := validateAdminNote(note)
	if err != nil {
		return Order{}, err
	}
	nowUnix := s.cfg.Now().Unix()
	var reopened Order
	_, _, err = s.cfg.Orders.Update(ctx, orderID, func(current Order, found bool) (Order, bool, error) {
		if !found {
			return current, false, fmt.Errorf("%w: order %s is not recorded", ErrOrderInvalid, orderID)
		}
		switch current.State {
		case DeliveryExhausted:
			// The only reopenable state.
		case DeliveryReserved:
			return current, false, fmt.Errorf("%w: order %s is already retryable (attempt %d of %d)",
				ErrNotResolvable, orderID, current.Attempts, current.MaxAttempts)
		default:
			return current, false, fmt.Errorf("%w: order %s is %s; reopening it would grant the "+
				"goods a second time", ErrNotResolvable, orderID, current.State)
		}
		current.State = DeliveryReserved
		current.Attempts = 0
		// Due immediately: the operator reopened it because the cause is
		// fixed, and making them wait out a backoff they did not set is a
		// reason to reach past this API into the store.
		//
		// This assignment is currently REDUNDANT — the path that sets
		// DeliveryExhausted already zeroes it, and exhausted is the only state
		// reopening accepts — and it is kept rather than deleted because the
		// intent belongs where the state transition is, not two hundred lines
		// away in a branch of AttemptDelivery. Deleting it today changes
		// nothing; a future change to the exhaust path that leaves a backoff
		// behind would otherwise silently make a reopened order un-due, which
		// looks exactly like "reopening does not work". The property is pinned
		// by a test rather than the assignment: a mutation that sets a FUTURE
		// time here fails, a mutation that removes the line does not.
		current.NextAttemptAtUnix = 0
		current.LastError = ""
		current.Reopens++
		current.AdminNote = note
		current.AdminActionAtUnix = nowUnix
		current.UpdatedAtUnix = nowUnix
		reopened = current
		return current, true, nil
	})
	if err != nil {
		return Order{}, err
	}
	s.report.Accepted("admin.reopen")
	return reopened, nil
}

// SettleOutOfBand implements Admin.
//
// DeliverySettled is a THIRD terminal state rather than a reuse of
// DeliveryDelivered, and the distinction is the point: a reconciliation that
// counted a hand-refunded order as delivered would report that this service
// fulfilled something it did not. "The deliverer succeeded", "we gave up" and
// "a human resolved it elsewhere" are three different facts about the money,
// and collapsing any two of them is how a payment ledger stops being auditable.
//
// It is allowed from DeliveryExhausted only. A DeliveryReserved order is
// refused because the retry loop is still live: settling it would race an
// attempt that may yet succeed, and the pair of outcomes is a refund AND the
// goods.
func (s *Service) SettleOutOfBand(ctx context.Context, orderID string, note string) (Order, error) {
	note, err := validateAdminNote(note)
	if err != nil {
		return Order{}, err
	}
	nowUnix := s.cfg.Now().Unix()
	var settled Order
	_, _, err = s.cfg.Orders.Update(ctx, orderID, func(current Order, found bool) (Order, bool, error) {
		if !found {
			return current, false, fmt.Errorf("%w: order %s is not recorded", ErrOrderInvalid, orderID)
		}
		switch current.State {
		case DeliveryExhausted:
			// The only settleable state.
		case DeliveryReserved:
			return current, false, fmt.Errorf("%w: order %s is still being retried; settling it "+
				"now races an attempt that may succeed, and the pair of outcomes is a refund and "+
				"the goods", ErrNotResolvable, orderID)
		default:
			return current, false, fmt.Errorf("%w: order %s is %s", ErrNotResolvable, orderID, current.State)
		}
		current.State = DeliverySettled
		current.NextAttemptAtUnix = 0
		current.AdminNote = note
		current.AdminActionAtUnix = nowUnix
		current.UpdatedAtUnix = nowUnix
		settled = current
		return current, true, nil
	})
	if err != nil {
		return Order{}, err
	}
	s.report.Accepted("admin.settle")
	return settled, nil
}

// validateAdminNote requires a reason and bounds it.
//
// Required, with no default. An intervention on a paid order that records no
// reason is the "zero observability" pattern applied to the highest-value
// operation in the repository: the next person to look at the order sees that
// someone changed it and cannot find out why or whether it was deliberate.
func validateAdminNote(note string) (string, error) {
	trimmed := strings.TrimSpace(note)
	if trimmed == "" {
		return "", fmt.Errorf("%w: an operator note is required; an intervention on a paid "+
			"order with no recorded reason cannot be reviewed", ErrAdminNoteRequired)
	}
	if len(trimmed) > MaxAdminNoteBytes {
		return "", fmt.Errorf("%w: note is %d bytes, limit %d",
			ErrAdminNoteRequired, len(trimmed), MaxAdminNoteBytes)
	}
	return trimmed, nil
}

var _ Admin = (*Service)(nil)
