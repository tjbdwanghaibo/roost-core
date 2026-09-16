package match

import (
	"context"
	"fmt"
	"github.com/tjbdwanghaibo/roost-core/versionstore"
)

// Store is matchmaking persistence.
//
// The shape that matters: Commit is one call that takes tickets off the queue,
// creates the match and resolves those tickets **atomically**. There is no way
// to pop the queue without committing, and no way to write a match without
// consuming its tickets. In the implementation this replaces those were three
// separate writes with the queue already truncated, so a failure between them
// lost the players — removed from the queue, still marked waiting, with
// nothing to put them back.
type Store interface {
	// Enqueue adds a waiting ticket. It refuses a subject that already holds
	// a live ticket (ErrAlreadyQueued), and is idempotent per requestID: a
	// repeat returns the existing ticket rather than queueing twice.
	Enqueue(ctx context.Context, queue Queue, subject Subject, requestID string) (Ticket, error)

	// Cancel withdraws a waiting ticket. subject must own it.
	Cancel(ctx context.Context, queue Queue, ticketID string, subject Subject) (Ticket, error)

	// Ticket reads one ticket. subject must own it, so a guessable id does not
	// expose another subject's score or payload.
	Ticket(ctx context.Context, queue Queue, ticketID string, subject Subject) (Ticket, bool, error)

	// Candidates returns up to limit waiting, unexpired tickets in queue
	// order, oldest first. It is bounded: the implementation this replaces
	// read every queued ticket on every enqueue.
	Candidates(ctx context.Context, queue Queue, limit int) ([]Ticket, error)

	// Commit turns the named tickets into one match, atomically. It fails
	// without side effects if any ticket is no longer waiting — the check and
	// the write are one step, so two committers cannot both win.
	Commit(ctx context.Context, queue Queue, ticketIDs []string) (Match, error)

	// Match reads a committed match.
	Match(ctx context.Context, queue Queue, matchID string) (Match, bool, error)

	// Sweep resolves tickets whose deadline has passed, up to limit, and
	// returns how many it resolved. Nothing in the implementation this
	// replaces ever read the deadline it wrote.
	Sweep(ctx context.Context, queue Queue, limit int) (int, error)

	// QueueLength reports how many waiting tickets a queue holds.
	QueueLength(ctx context.Context, queue Queue) (int, error)
}

func validateEnqueue(queue Queue, subject Subject) error {
	if err := queue.Validate(); err != nil {
		return err
	}
	return subject.Validate()
}

func validateOwnership(ticket Ticket, subject Subject) error {
	if ticket.Subject.Key() != subject.Key() {
		// Deliberately does not say who does own it: the caller has already
		// shown it does not, and naming the owner would turn a guessable id
		// into an enumeration oracle.
		return fmt.Errorf("%w: ticket %s", ErrNotPermitted, ticket.ID)
	}
	return nil
}

func validateLimit(limit int) error {
	if limit <= 0 {
		return fmt.Errorf("%w: limit must be positive, got %d", ErrQueueInvalid, limit)
	}
	if limit > MaxPageSize {
		return fmt.Errorf("%w: limit %d exceeds %d", ErrQueueInvalid, limit, MaxPageSize)
	}
	return nil
}

// NewMemoryStore is NewStore over an in-process versionstore: what a test or a
// single-process tool uses when there is no Redis to share the queue with.
// The queue state type is this package's own, so callers outside it cannot
// name the versionstore themselves; this is the constructor they use instead.
func NewMemoryStore(cfg Config) (Store, error) {
	return NewStore(versionstore.NewMemoryStore[string, queueState](), cfg)
}
