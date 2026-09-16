package match

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/tjbdwanghaibo/roost-core/versionstore"

	"github.com/tjbdwanghaibo/roost-kit/service/servicemetrics"
)

// queueState is one queue's entire mutable state, held as a single versioned
// value.
//
// Keeping it in one entry is the central decision. The implementation this
// replaces spread a queue across a waiting list, a ticket record per subject
// and a match record, and mutated them in sequence: pop the queue, write the
// match, then flip each ticket. Any failure after the pop lost those players.
// With one entry, a commit is one compare-and-set: either the queue, the
// tickets and the match all move, or none of them do. Two committers racing on
// the same head cannot both win, because the second one's version is stale.
//
// The cost is that a queue's throughput is bounded by contention on one key,
// which is why callers route by Queue.Key() — see servicerpc.KeyAffinityPicker.
type queueState struct {
	// Waiting is the ticket ids in queue order, oldest first.
	Waiting []string `json:"waiting"`
	// Tickets holds every ticket this queue knows, live and resolved.
	Tickets map[string]Ticket `json:"tickets"`
	// Matches holds committed matches.
	Matches map[string]Match `json:"matches"`
	// SubjectTickets maps a subject key to its live ticket id. This is what
	// makes "one live ticket per subject" a lookup rather than a scan, and
	// what stops the same player being committed into two matches.
	SubjectTickets map[string]string `json:"subject_tickets"`
	// Requests maps an enqueue idempotency key to the ticket it produced.
	Requests map[string]string `json:"requests"`
}

// clone copies the state before a mutation touches it.
//
// versionstore documents that a Mutate may run more than once and must be a
// pure function of its arguments, and its memory implementation hands out the
// stored value itself. The callbacks below resolve expired tickets by shifting
// Waiting in place before they decide whether to save; done on the stored
// slice, a mutation that then declined to save left the stored header over a
// shifted array with its tail duplicated, and Candidates handed out one ticket
// twice. Copying first is what makes "an aborted mutation changes nothing"
// hold for every implementation of the contract, not only the Redis one that
// happens to decode a fresh value per call.
func (s queueState) clone() queueState {
	next := queueState{
		Waiting:        append([]string(nil), s.Waiting...),
		Tickets:        make(map[string]Ticket, len(s.Tickets)+1),
		Matches:        make(map[string]Match, len(s.Matches)+1),
		SubjectTickets: make(map[string]string, len(s.SubjectTickets)+1),
		Requests:       make(map[string]string, len(s.Requests)+1),
	}
	for id, ticket := range s.Tickets {
		next.Tickets[id] = ticket
	}
	for id, match := range s.Matches {
		next.Matches[id] = match
	}
	for key, id := range s.SubjectTickets {
		next.SubjectTickets[key] = id
	}
	for key, id := range s.Requests {
		next.Requests[key] = id
	}
	return next
}

func (s *queueState) init() {
	if s.Tickets == nil {
		s.Tickets = map[string]Ticket{}
	}
	if s.Matches == nil {
		s.Matches = map[string]Match{}
	}
	if s.SubjectTickets == nil {
		s.SubjectTickets = map[string]string{}
	}
	if s.Requests == nil {
		s.Requests = map[string]string{}
	}
}

// Config configures a queue store.
type Config struct {
	// TicketTTL is how long a ticket waits before expiring; zero selects
	// DefaultTicketTTL. It must be positive: a ticket with no deadline is a
	// queue entry that leaks when its client disconnects, which is what
	// happened when the deadline was written but never read.
	TicketTTL time.Duration
	// Now is the clock; nil means time.Now.
	Now func() time.Time
	// SweepQueues is the queue set this process's server sweeps for expired
	// tickets, once per tick. A deployment knows its queues — from the modes
	// it runs — and this package cannot enumerate them without an unbounded
	// scan. Empty means the server sweeps nothing, which it logs at start.
	// Expiry is still enforced inline on every read and mutation, so tickets
	// do not wait forever without it; what the sweep adds is resolving
	// expired tickets nobody touches again.
	SweepQueues []Queue
	// NewID mints ticket and match ids; nil means a 128-bit random id.
	//
	// Ids must be globally unique and unguessable. The implementation this
	// replaces used "ticket-1" from a process-local counter, so two replicas
	// minted identical ids — and an "idempotent" enqueue could hand back
	// another player's ticket.
	NewID func() (string, error)
	// Metrics receives reports. A nil reporter means no reporting and never
	// fails an operation. Queue depth and expiry counts are the two signals
	// the implementation this replaces had no way to emit, which is why its
	// ticket loss and its unenforced deadline both went unnoticed.
	Metrics servicemetrics.Reporter
}

// queueStore implements Store over versioned per-queue state.
type queueStore struct {
	state  versionstore.Store[string, queueState]
	cfg    Config
	report servicemetrics.Sink
}

// SweepQueues is the configured queue set the server's expiry sweep covers.
func (s *queueStore) SweepQueues() []Queue { return append([]Queue(nil), s.cfg.SweepQueues...) }

// NewStore returns a Store over versioned state.
func NewStore(state versionstore.Store[string, queueState], cfg Config) (Store, error) {
	if state == nil {
		return nil, fmt.Errorf("match: state store is nil")
	}
	if cfg.TicketTTL < 0 {
		return nil, fmt.Errorf("match: ticket ttl must not be negative")
	}
	if cfg.TicketTTL == 0 {
		cfg.TicketTTL = DefaultTicketTTL
	}
	for _, queue := range cfg.SweepQueues {
		if err := queue.Validate(); err != nil {
			return nil, fmt.Errorf("match: sweep queue %s: %w", queue.Key(), err)
		}
	}
	cfg.SweepQueues = append([]Queue(nil), cfg.SweepQueues...)
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.NewID == nil {
		cfg.NewID = randomID
	}
	return &queueStore{state: state, cfg: cfg, report: servicemetrics.Wrap(cfg.Metrics)}, nil
}

// randomID mints an unguessable id. Guessable ids are what let any client
// cancel another player's ticket and read its score.
func randomID() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("match: mint id: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

func (s *queueStore) Enqueue(ctx context.Context, queue Queue, subject Subject, requestID string) (Ticket, error) {
	if err := validateEnqueue(queue, subject); err != nil {
		return Ticket{}, err
	}
	now := s.cfg.Now()
	id, err := s.cfg.NewID()
	if err != nil {
		return Ticket{}, err
	}

	var (
		result   Ticket
		replayed bool
	)
	_, _, err = s.state.Update(ctx, queue.Key(), func(current queueState, _ bool) (queueState, bool, error) {
		current = current.clone()
		replayed = false
		s.expireLocked(&current, now)

		// Idempotent per request: a retried enqueue returns the ticket it
		// already produced instead of queueing the subject a second time.
		if requestID != "" {
			if existing, ok := current.Requests[requestID]; ok {
				if ticket, ok := current.Tickets[existing]; ok {
					// Whose retry is this? The request id comes from the
					// caller, so two subjects in one queue can collide on it,
					// and replaying without checking handed the second one the
					// first one's ticket and score while dropping its own
					// enqueue entirely — no request record, no queue entry.
					// The read path has always checked this
					// (validateOwnership); the replay branch had not
					// (RR-20260909-05).
					if err := validateOwnership(ticket, subject); err != nil {
						s.report.Refused("enqueue", "request_subject_mismatch")
						return current, false, err
					}
					result = ticket
					replayed = true
					return current, false, nil
				}
			}
		}
		// One live ticket per subject. This is the check whose absence let a
		// single client queue the same player twice and have both committed.
		if live, ok := current.SubjectTickets[subject.Key()]; ok {
			if ticket, ok := current.Tickets[live]; ok && !ticket.State.terminal() {
				s.report.Refused("enqueue", "already_queued")
				return current, false, fmt.Errorf("%w: %s holds ticket %s", ErrAlreadyQueued, subject.Key(), ticket.ID)
			}
		}
		if len(current.Waiting) >= MaxQueueLength {
			s.report.Refused("enqueue", "queue_full")
			return current, false, fmt.Errorf("%w: queue holds %d tickets, limit %d", ErrQueueInvalid, len(current.Waiting), MaxQueueLength)
		}

		ticket := Ticket{
			ID: id, Queue: queue, Subject: subject, State: TicketWaiting,
			RequestID: requestID, CreatedAtUnix: now.Unix(),
			ExpiresAtUnix: now.Add(s.cfg.TicketTTL).Unix(),
		}
		current.Tickets[id] = ticket
		current.Waiting = append(current.Waiting, id)
		current.SubjectTickets[subject.Key()] = id
		if requestID != "" {
			current.Requests[requestID] = id
		}
		result = ticket
		return current, true, nil
	})
	if err != nil {
		return Ticket{}, err
	}
	if replayed {
		// A retried enqueue is not a second player joining the queue. Counting
		// it as one makes the accept rate a measure of client retries.
		s.report.Replayed("enqueue")
	} else {
		s.report.Accepted("enqueue")
	}
	// Queue depth is the signal the implementation this replaces could not
	// emit: its tickets lived in per-subject records with no aggregate, so
	// "how many players are waiting" had no answer.
	if length, err := s.QueueLength(ctx, queue); err == nil {
		s.report.Depth("queue."+queue.Key(), int64(length))
	}
	return result, nil
}

func (s *queueStore) Cancel(ctx context.Context, queue Queue, ticketID string, subject Subject) (Ticket, error) {
	if err := queue.Validate(); err != nil {
		return Ticket{}, err
	}
	if ticketID == "" {
		return Ticket{}, fmt.Errorf("%w: id is empty", ErrTicketInvalid)
	}
	now := s.cfg.Now()

	var result Ticket
	_, _, err := s.state.Update(ctx, queue.Key(), func(current queueState, found bool) (queueState, bool, error) {
		current = current.clone()
		if !found {
			return current, false, fmt.Errorf("%w: %s", ErrTicketMissing, ticketID)
		}
		s.expireLocked(&current, now)
		ticket, ok := current.Tickets[ticketID]
		if !ok {
			return current, false, fmt.Errorf("%w: %s", ErrTicketMissing, ticketID)
		}
		if err := validateOwnership(ticket, subject); err != nil {
			return current, false, err
		}
		if ticket.State == TicketMatched {
			// A matched ticket cannot be withdrawn. Reporting this rather
			// than answering OK is the difference from an implementation that
			// told the caller "cancelled" while the store said "matched".
			return current, false, fmt.Errorf("%w: %s is in match %s", ErrTicketMatched, ticketID, ticket.MatchID)
		}
		if ticket.State.terminal() {
			// Already cancelled or expired: idempotent.
			result = ticket
			return current, false, nil
		}
		ticket.State = TicketCancelled
		ticket.ResolvedAtUnix = now.Unix()
		current.Tickets[ticketID] = ticket
		removeWaiting(&current, ticketID)
		delete(current.SubjectTickets, ticket.Subject.Key())
		result = ticket
		return current, true, nil
	})
	if err != nil {
		return Ticket{}, err
	}
	return result, nil
}

func (s *queueStore) Ticket(ctx context.Context, queue Queue, ticketID string, subject Subject) (Ticket, bool, error) {
	if err := queue.Validate(); err != nil {
		return Ticket{}, false, err
	}
	current, found, err := s.state.Get(ctx, queue.Key())
	if err != nil || !found {
		return Ticket{}, false, err
	}
	state := current.Value
	state.init()
	ticket, ok := state.Tickets[ticketID]
	if !ok {
		return Ticket{}, false, nil
	}
	if err := validateOwnership(ticket, subject); err != nil {
		return Ticket{}, false, err
	}
	// Report an elapsed deadline as expired even before a sweep runs, so a
	// reader never sees a ticket that is waiting only because nothing has
	// gotten round to resolving it.
	if ticket.Expired(s.cfg.Now()) {
		ticket.State = TicketExpired
	}
	return ticket, true, nil
}

func (s *queueStore) Candidates(ctx context.Context, queue Queue, limit int) ([]Ticket, error) {
	if err := queue.Validate(); err != nil {
		return nil, err
	}
	if err := validateLimit(limit); err != nil {
		return nil, err
	}
	current, found, err := s.state.Get(ctx, queue.Key())
	if err != nil || !found {
		return nil, err
	}
	state := current.Value
	state.init()
	now := s.cfg.Now()
	out := make([]Ticket, 0, limit)
	for _, id := range state.Waiting {
		ticket, ok := state.Tickets[id]
		if !ok || ticket.State != TicketWaiting || ticket.Expired(now) {
			continue
		}
		out = append(out, ticket)
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

func (s *queueStore) Commit(ctx context.Context, queue Queue, ticketIDs []string) (Match, error) {
	if err := queue.Validate(); err != nil {
		return Match{}, err
	}
	if len(ticketIDs) != queue.GroupSize {
		return Match{}, fmt.Errorf("%w: %d tickets for a group of %d", ErrTicketInvalid, len(ticketIDs), queue.GroupSize)
	}
	seen := make(map[string]bool, len(ticketIDs))
	for _, id := range ticketIDs {
		if id == "" {
			return Match{}, fmt.Errorf("%w: empty ticket id", ErrTicketInvalid)
		}
		if seen[id] {
			return Match{}, fmt.Errorf("%w: ticket %s appears twice", ErrTicketInvalid, id)
		}
		seen[id] = true
	}
	now := s.cfg.Now()
	matchID, err := s.cfg.NewID()
	if err != nil {
		return Match{}, err
	}

	var result Match
	_, _, err = s.state.Update(ctx, queue.Key(), func(current queueState, found bool) (queueState, bool, error) {
		current = current.clone()
		if !found {
			return current, false, fmt.Errorf("%w: queue is empty", ErrTicketMissing)
		}
		s.expireLocked(&current, now)

		// Validate every ticket before mutating anything. Because this whole
		// function is one compare-and-set, a failure here leaves the queue
		// exactly as it was — the property the three-write sequence it
		// replaces could not offer.
		members := make([]Subject, 0, len(ticketIDs))
		subjects := make(map[string]bool, len(ticketIDs))
		for _, id := range ticketIDs {
			ticket, ok := current.Tickets[id]
			if !ok {
				return current, false, fmt.Errorf("%w: %s", ErrTicketMissing, id)
			}
			if ticket.State != TicketWaiting {
				return current, false, fmt.Errorf("%w: %s is %s", ErrConflict, id, ticket.State)
			}
			// The same subject must not appear twice in one match, which is
			// what a duplicate live ticket used to produce.
			if subjects[ticket.Subject.Key()] {
				return current, false, fmt.Errorf("%w: subject %s appears twice", ErrTicketInvalid, ticket.Subject.Key())
			}
			subjects[ticket.Subject.Key()] = true
			members = append(members, ticket.Subject)
		}

		match := Match{
			ID: matchID, Queue: queue, Members: members,
			TicketIDs: append([]string(nil), ticketIDs...), CreatedAtUnix: now.Unix(),
		}
		for _, id := range ticketIDs {
			ticket := current.Tickets[id]
			ticket.State = TicketMatched
			ticket.MatchID = matchID
			ticket.ResolvedAtUnix = now.Unix()
			current.Tickets[id] = ticket
			removeWaiting(&current, id)
			delete(current.SubjectTickets, ticket.Subject.Key())
		}
		current.Matches[matchID] = match
		result = match
		return current, true, nil
	})
	if err != nil {
		return Match{}, err
	}
	s.report.Accepted("commit")
	return result, nil
}

func (s *queueStore) Match(ctx context.Context, queue Queue, matchID string) (Match, bool, error) {
	if err := queue.Validate(); err != nil {
		return Match{}, false, err
	}
	current, found, err := s.state.Get(ctx, queue.Key())
	if err != nil || !found {
		return Match{}, false, err
	}
	state := current.Value
	state.init()
	match, ok := state.Matches[matchID]
	return match, ok, nil
}

func (s *queueStore) Sweep(ctx context.Context, queue Queue, limit int) (int, error) {
	if err := queue.Validate(); err != nil {
		return 0, err
	}
	if err := validateLimit(limit); err != nil {
		return 0, err
	}
	now := s.cfg.Now()
	resolved := 0
	_, _, err := s.state.Update(ctx, queue.Key(), func(current queueState, found bool) (queueState, bool, error) {
		current = current.clone()
		if !found {
			return current, false, nil
		}
		resolved = s.expireLockedLimit(&current, now, limit)
		return current, resolved > 0, nil
	})
	if err != nil {
		// Counted as well as returned: the background sweep only logs this,
		// and a sweep that fails on every tick must show up somewhere other
		// than a log line (U-0121).
		s.report.Dropped("sweep.failed", 1)
		return 0, err
	}
	// Expired tickets are dropped data: a player who waited and got nothing.
	// The implementation this replaces wrote a deadline nothing read, so this
	// count was structurally unobtainable.
	s.report.Dropped("ticket.expired", resolved)
	return resolved, nil
}

func (s *queueStore) QueueLength(ctx context.Context, queue Queue) (int, error) {
	if err := queue.Validate(); err != nil {
		return 0, err
	}
	current, found, err := s.state.Get(ctx, queue.Key())
	if err != nil || !found {
		return 0, err
	}
	state := current.Value
	state.init()
	now := s.cfg.Now()
	live := 0
	for _, id := range state.Waiting {
		if ticket, ok := state.Tickets[id]; ok && ticket.State == TicketWaiting && !ticket.Expired(now) {
			live++
		}
	}
	return live, nil
}

// expireLocked resolves every lapsed waiting ticket. It runs at the start of
// every mutation, so an expired ticket can never be committed into a match
// even if no sweep has run — the deadline is enforced by the code that would
// otherwise use the ticket, not only by a background job.
func (s *queueStore) expireLocked(state *queueState, now time.Time) int {
	return s.expireLockedLimit(state, now, len(state.Waiting))
}

func (s *queueStore) expireLockedLimit(state *queueState, now time.Time, limit int) int {
	resolved := 0
	for _, id := range append([]string(nil), state.Waiting...) {
		if resolved >= limit {
			break
		}
		ticket, ok := state.Tickets[id]
		if !ok {
			removeWaiting(state, id)
			continue
		}
		if ticket.State != TicketWaiting || !ticket.Expired(now) {
			continue
		}
		ticket.State = TicketExpired
		ticket.ResolvedAtUnix = now.Unix()
		state.Tickets[id] = ticket
		removeWaiting(state, id)
		delete(state.SubjectTickets, ticket.Subject.Key())
		resolved++
	}
	return resolved
}

func removeWaiting(state *queueState, ticketID string) {
	for index, id := range state.Waiting {
		if id == ticketID {
			state.Waiting = append(state.Waiting[:index], state.Waiting[index+1:]...)
			return
		}
	}
}

var _ Store = (*queueStore)(nil)
