package mail

import (
	"context"

	"github.com/tjbdwanghaibo/roost-core/versionstore"
)

// EnvelopeStore holds envelopes. Envelopes are written once and never
// rewritten, so the only write this contract needs is an insert.
//
// It is deliberately not a versionstore: there is no read-modify-write on an
// envelope, and offering an Update would invite one.
type EnvelopeStore interface {
	// Create inserts an envelope. It reports created=false when the id is
	// already taken, and must not overwrite. That is what makes a retried
	// send safe without a transaction.
	Create(ctx context.Context, envelope Envelope) (created bool, err error)
	// Get reads one envelope.
	Get(ctx context.Context, id string) (Envelope, bool, error)
	// GetMany reads a bounded set of envelopes in ONE round trip.
	//
	// This method exists because of a confirmed defect: the implementation
	// this replaces read one envelope page and then issued one state read per
	// envelope, in a loop with no bound on the number of iterations. A
	// contract that can only fetch one envelope at a time makes that the
	// natural thing to write. Callers pass at most MaxPageSize ids.
	GetMany(ctx context.Context, ids []string) (map[string]Envelope, error)
}

// MailboxStore holds per-player state. It is versioned, so there is no
// unconditional write: a status change and the unread count it moves are one
// compare-and-set.
type MailboxStore = versionstore.Store[int64, Mailbox]

// SendLedger records accepted sends by their idempotency key, so a retried
// send returns the envelope it already produced instead of creating a second
// one.
//
// It is insert-only for the same reason account's per-server slot is: an
// idempotency claim that a second racer can share is not a claim. See
// versionstore.Create.
type SendLedger = versionstore.Store[string, SentRecord]

// SentRecord is what a send ledger entry holds.
type SentRecord struct {
	RequestID     string `json:"request_id"`
	MailID        string `json:"mail_id"`
	CreatedAtUnix int64  `json:"created_at_unix"`
	// DeliveredAtUnix is stamped once the envelope's delivery has succeeded.
	// Zero means the first attempt never finished delivering, and a replay of
	// this request id must deliver before it answers "sent". Records written
	// before this field existed read as zero and cost one redundant,
	// idempotent delivery attempt on their next replay.
	DeliveredAtUnix int64 `json:"delivered_at_unix,omitempty"`
}

// Deliverer fans a broadcast envelope out into individual mailboxes.
//
// It is an interface and has no default, because "how does one server-wide
// mail reach a hundred thousand mailboxes" is a deployment decision — a job
// queue, a lazy backfill on first read, a nightly sweep — and a library that
// picked one would be wrong for the others.
//
// What this package does guarantee is that whatever the caller picks, Deliver
// is idempotent per (mailbox, mail): a redelivered mail does not become unread
// twice and does not double the count. So a Deliverer may retry freely.
type Deliverer interface {
	// Deliver hands one envelope to the mailboxes in its scope. It may return
	// before delivery completes; this package makes no claim about when a
	// broadcast becomes visible.
	Deliver(ctx context.Context, envelope Envelope) error
}

// DelivererFunc adapts a function to Deliverer.
type DelivererFunc func(context.Context, Envelope) error

// Deliver implements Deliverer.
func (f DelivererFunc) Deliver(ctx context.Context, envelope Envelope) error {
	return f(ctx, envelope)
}

var _ Deliverer = DelivererFunc(nil)
