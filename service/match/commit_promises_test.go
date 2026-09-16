package match

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func expectErr(t *testing.T, err error, sentinel error, text string) {
	t.Helper()
	if err == nil || !errors.Is(err, sentinel) || !strings.Contains(err.Error(), text) {
		t.Fatalf("error = %v, want %v containing %q", err, sentinel, text)
	}
}

// Commit is one compare-and-set over the whole queue: every refusal must fire
// before anything is mutated, so the queue is exactly as it was afterwards.
// Each rule is pinned by sentinel and message.
func TestCommitRefusesEachInvalidTicketSet(t *testing.T) {
	store, _ := newStore(t)
	ctx := context.Background()
	q := ranked()
	a := enqueue(t, store, q, player(1, 10), "r-1")
	b := enqueue(t, store, q, player(2, 12), "r-2")

	cases := []struct {
		name     string
		tickets  []string
		sentinel error
		text     string
	}{
		{"wrong count", []string{a.ID}, ErrTicketInvalid, "1 tickets for a group of 2"},
		{"empty id", []string{a.ID, ""}, ErrTicketInvalid, "empty ticket id"},
		// "ticket X appears twice" is the id-level rule; the subject-level rule
		// inside the CAS says "subject X appears twice" — pin the id one.
		{"same ticket twice", []string{a.ID, a.ID}, ErrTicketInvalid, "ticket " + a.ID + " appears twice"},
		{"unknown ticket", []string{a.ID, "ghost"}, ErrTicketMissing, "ghost"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := store.Commit(ctx, q, tc.tickets)
			expectErr(t, err, tc.sentinel, tc.text)
		})
	}
	if _, err := store.Commit(ctx, Queue{Mode: "ranked", GroupSize: 2, Partition: "empty"}, []string{a.ID, b.ID}); !errors.Is(err, ErrTicketMissing) || !strings.Contains(err.Error(), "queue is empty") {
		t.Fatalf("commit on a queue nobody entered = %v", err)
	}
	// Both tickets still waiting: nothing above touched the queue.
	for _, held := range []struct {
		ticket Ticket
		owner  Subject
	}{{a, player(1, 10)}, {b, player(2, 12)}} {
		got, found, err := store.Ticket(ctx, q, held.ticket.ID, held.owner)
		if err != nil || !found || got.State != TicketWaiting {
			t.Fatalf("ticket %s after refused commits = %+v found=%v err=%v", held.ticket.ID, got, found, err)
		}
	}
	match, err := store.Commit(ctx, q, []string{a.ID, b.ID})
	if err != nil {
		t.Fatalf("legal commit: %v", err)
	}
	if _, err := store.Commit(ctx, q, []string{a.ID, b.ID}); !errors.Is(err, ErrConflict) {
		t.Fatalf("committing already matched tickets = %v, want ErrConflict", err)
	}
	if len(match.Members) != 2 {
		t.Fatalf("match = %+v", match)
	}
}

// The "subject appears twice" rule inside Commit is unreachable through the
// public API: Enqueue already refuses a second live ticket for the same
// subject. It stays as defence in depth and is not pinned here.

func TestCancelRefusesBlankAndUnknownTickets(t *testing.T) {
	store, _ := newStore(t)
	ctx := context.Background()
	q := ranked()
	_, err := store.Cancel(ctx, q, "", player(1, 10))
	expectErr(t, err, ErrTicketInvalid, "id is empty")
	_, err = store.Cancel(ctx, q, "ghost", player(1, 10))
	expectErr(t, err, ErrTicketMissing, "ghost")
}
