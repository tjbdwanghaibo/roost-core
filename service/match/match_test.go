package match

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/infra/storage/versionstore"

	"github.com/tjbdwanghaibo/roost-core/infra/observe/servicemetrics"
)

type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func newStore(t *testing.T, options ...func(*Config)) (Store, *clock) {
	t.Helper()
	c := &clock{now: time.Unix(1_700_000_000, 0)}
	cfg := Config{Now: c.Now}
	for _, option := range options {
		option(&cfg)
	}
	store, err := NewStore(versionstore.NewMemoryStore[string, queueState](), cfg)
	if err != nil {
		t.Fatal(err)
	}
	return store, c
}

func ranked() Queue { return Queue{Mode: "ranked", GroupSize: 2, Partition: "eu"} }

func player(id int64, score int64) Subject {
	return Subject{Kind: "player", ID: id, Score: score}
}

func enqueue(t *testing.T, s Store, q Queue, subject Subject, requestID string) Ticket {
	t.Helper()
	ticket, err := s.Enqueue(context.Background(), q, subject, requestID)
	if err != nil {
		t.Fatalf("enqueue %s: %v", subject.Key(), err)
	}
	return ticket
}

// A subject may hold one live ticket. Two live tickets for one subject is what
// let a single client have the same player committed into two matches.
func TestSubjectMayHoldOnlyOneLiveTicket(t *testing.T) {
	store, _ := newStore(t)
	ctx := context.Background()

	first := enqueue(t, store, ranked(), player(1, 100), "")
	_, err := store.Enqueue(ctx, ranked(), player(1, 100), "")
	if !errors.Is(err, ErrAlreadyQueued) {
		t.Fatalf("a second live ticket was allowed: %v", err)
	}
	// Once the first is withdrawn the subject may queue again.
	if _, err := store.Cancel(ctx, ranked(), first.ID, player(1, 100)); err != nil {
		t.Fatal(err)
	}
	second := enqueue(t, store, ranked(), player(1, 100), "")
	if second.ID == first.ID {
		t.Fatal("the second enqueue reused the cancelled ticket id")
	}
	if length, _ := store.QueueLength(ctx, ranked()); length != 1 {
		t.Fatalf("queue holds %d live tickets, want 1", length)
	}
}

// Ticket ids are minted by the service and unguessable. A caller-supplied id
// was what let an "idempotent" enqueue return another player's ticket.
func TestTicketIDsAreServerMintedAndDistinct(t *testing.T) {
	store, _ := newStore(t)
	seen := map[string]bool{}
	for id := int64(1); id <= 32; id++ {
		ticket := enqueue(t, store, ranked(), player(id, 0), "")
		if ticket.ID == "" {
			t.Fatal("the service minted an empty ticket id")
		}
		if len(ticket.ID) < 16 {
			t.Fatalf("ticket id %q is short enough to guess", ticket.ID)
		}
		if seen[ticket.ID] {
			t.Fatalf("ticket id %q was minted twice", ticket.ID)
		}
		seen[ticket.ID] = true
	}
}

// Enqueue is idempotent per request id: a retry returns the ticket it already
// produced rather than queueing the subject twice.
func TestEnqueueIsIdempotentPerRequest(t *testing.T) {
	store, _ := newStore(t)
	first := enqueue(t, store, ranked(), player(1, 100), "req-1")
	for attempt := 0; attempt < 4; attempt++ {
		again := enqueue(t, store, ranked(), player(1, 100), "req-1")
		if again.ID != first.ID {
			t.Fatalf("attempt %d produced a second ticket %s (first %s)", attempt, again.ID, first.ID)
		}
	}
	if length, _ := store.QueueLength(context.Background(), ranked()); length != 1 {
		t.Fatalf("queue holds %d tickets after four retries, want 1", length)
	}
}

// Every operation naming a ticket also names its subject, so a guessable id is
// not enough to cancel someone else's ticket or read their score.
func TestTicketOperationsRequireOwnership(t *testing.T) {
	store, _ := newStore(t)
	ctx := context.Background()
	victim := enqueue(t, store, ranked(), player(1, 100), "")

	attacker := player(2, 0)
	if _, err := store.Cancel(ctx, ranked(), victim.ID, attacker); !errors.Is(err, ErrNotPermitted) {
		t.Fatalf("another subject cancelled the ticket: %v", err)
	}
	if _, _, err := store.Ticket(ctx, ranked(), victim.ID, attacker); !errors.Is(err, ErrNotPermitted) {
		t.Fatalf("another subject read the ticket: %v", err)
	}
	// The ticket is untouched.
	ticket, found, err := store.Ticket(ctx, ranked(), victim.ID, player(1, 100))
	if err != nil || !found {
		t.Fatalf("owner read: found=%v err=%v", found, err)
	}
	if ticket.State != TicketWaiting {
		t.Fatalf("the refused cancel changed the state to %s", ticket.State)
	}
}

// The deadline is enforced. In the implementation this replaces it was
// written, shipped to the client, and never read by anything.
func TestTicketDeadlineIsEnforcedOnReadAndBySweep(t *testing.T) {
	store, c := newStore(t, func(cfg *Config) { cfg.TicketTTL = time.Minute })
	ctx := context.Background()
	ticket := enqueue(t, store, ranked(), player(1, 100), "")

	c.advance(61 * time.Second)

	// A reader must not see a ticket that is only "waiting" because nothing
	// has gotten round to resolving it.
	read, found, err := store.Ticket(ctx, ranked(), ticket.ID, player(1, 100))
	if err != nil || !found {
		t.Fatalf("read: found=%v err=%v", found, err)
	}
	if read.State != TicketExpired {
		t.Fatalf("an elapsed ticket reads as %s", read.State)
	}
	if length, _ := store.QueueLength(ctx, ranked()); length != 0 {
		t.Fatalf("an expired ticket still counts toward the queue length")
	}
	// It must not be a match candidate either.
	candidates, err := store.Candidates(ctx, ranked(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 0 {
		t.Fatalf("an expired ticket is still a candidate: %+v", candidates)
	}
	resolved, err := store.Sweep(ctx, ranked(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if resolved != 1 {
		t.Fatalf("sweep resolved %d tickets, want 1", resolved)
	}
	// And the subject may queue again, because expiry released its slot.
	if _, err := store.Enqueue(ctx, ranked(), player(1, 100), ""); err != nil {
		t.Fatalf("the expired ticket did not release the subject: %v", err)
	}
}

// A mutation that declines to save must leave stored state exactly as it
// found it. versionstore's Mutate contract says so — the callback may run more
// than once and must be pure — and the memory implementation hands out the
// stored value itself, so an in-place edit followed by "do not save" is a
// write that bypassed the version. The shape that made this visible: a
// replayed enqueue first resolved an expired ticket by shifting Waiting in
// place, then returned the existing ticket without saving. The stored slice
// kept its old length over the shifted array, so its tail was duplicated and
// Candidates handed out one ticket twice — which Commit then refused as a
// duplicate, stalling the queue until the next successful write.
func TestAnAbortedMutationLeavesStoredStateUntouched(t *testing.T) {
	store, c := newStore(t, func(cfg *Config) { cfg.TicketTTL = time.Minute })
	ctx := context.Background()
	queue := Queue{Mode: "ranked", GroupSize: 4, Partition: "eu"}

	enqueue(t, store, queue, player(1, 0), "")
	c.advance(30 * time.Second)
	second := enqueue(t, store, queue, player(2, 0), "req-2")
	// Player 1 has expired, player 2 has not.
	c.advance(45 * time.Second)

	// The replay: the mutation expires player 1 and then declines to save.
	again := enqueue(t, store, queue, player(2, 0), "req-2")
	if again.ID != second.ID {
		t.Fatalf("the replay returned ticket %s, want %s", again.ID, second.ID)
	}

	candidates, err := store.Candidates(ctx, queue, 10)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]int{}
	for _, candidate := range candidates {
		seen[candidate.ID]++
	}
	if len(candidates) != 1 || seen[second.ID] != 1 {
		t.Fatalf("candidates after an aborted mutation = %d entries, ticket %s seen %d times; "+
			"an in-place edit leaked into stored state", len(candidates), second.ID, seen[second.ID])
	}
	if length, _ := store.QueueLength(ctx, queue); length != 1 {
		t.Fatalf("queue length = %d, want 1", length)
	}
}

// Commit is one atomic step: on any failure the queue is exactly as it was.
// The implementation this replaces popped the queue first, so a failure lost
// those players — removed from the queue, still marked waiting, with nothing
// to put them back.
func TestFailedCommitLeavesTheQueueUntouched(t *testing.T) {
	store, _ := newStore(t)
	ctx := context.Background()
	first := enqueue(t, store, ranked(), player(1, 100), "")
	second := enqueue(t, store, ranked(), player(2, 100), "")

	// A commit naming a ticket that is not waiting must fail without effect.
	if _, err := store.Cancel(ctx, ranked(), second.ID, player(2, 100)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Commit(ctx, ranked(), []string{first.ID, second.ID}); !errors.Is(err, ErrConflict) {
		t.Fatalf("committing a cancelled ticket returned %v, want ErrConflict", err)
	}
	// The surviving ticket is still queued and still waiting.
	ticket, found, err := store.Ticket(ctx, ranked(), first.ID, player(1, 100))
	if err != nil || !found {
		t.Fatalf("read: found=%v err=%v", found, err)
	}
	if ticket.State != TicketWaiting {
		t.Fatalf("the failed commit resolved a surviving ticket to %s", ticket.State)
	}
	if length, _ := store.QueueLength(ctx, ranked()); length != 1 {
		t.Fatalf("the failed commit changed the queue length to %d, want 1", length)
	}
	// A commit naming a missing ticket likewise has no effect.
	if _, err := store.Commit(ctx, ranked(), []string{first.ID, "does-not-exist"}); !errors.Is(err, ErrTicketMissing) {
		t.Fatalf("committing a missing ticket returned %v", err)
	}
	if length, _ := store.QueueLength(ctx, ranked()); length != 1 {
		t.Fatal("a commit naming a missing ticket changed the queue")
	}
}

func TestCommitResolvesTicketsAndRecordsTheMatch(t *testing.T) {
	store, _ := newStore(t)
	ctx := context.Background()
	first := enqueue(t, store, ranked(), player(1, 100), "")
	second := enqueue(t, store, ranked(), player(2, 200), "")

	match, err := store.Commit(ctx, ranked(), []string{first.ID, second.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(match.Members) != 2 || len(match.TicketIDs) != 2 {
		t.Fatalf("match = %+v", match)
	}
	if match.ID == "" {
		t.Fatal("the match has no id")
	}
	for _, ticket := range []Ticket{first, second} {
		read, found, err := store.Ticket(ctx, ranked(), ticket.ID, ticket.Subject)
		if err != nil || !found {
			t.Fatalf("read %s: found=%v err=%v", ticket.ID, found, err)
		}
		if read.State != TicketMatched {
			t.Fatalf("ticket %s is %s after commit", ticket.ID, read.State)
		}
		if read.MatchID != match.ID {
			t.Fatalf("ticket %s points at match %q, want %q", ticket.ID, read.MatchID, match.ID)
		}
	}
	if length, _ := store.QueueLength(ctx, ranked()); length != 0 {
		t.Fatalf("queue holds %d tickets after the commit", length)
	}
	stored, found, err := store.Match(ctx, ranked(), match.ID)
	if err != nil || !found {
		t.Fatalf("match read: found=%v err=%v", found, err)
	}
	if len(stored.Members) != 2 {
		t.Fatalf("stored match = %+v", stored)
	}
	// A matched ticket cannot be withdrawn, and saying so beats answering OK
	// while the store still says matched.
	if _, err := store.Cancel(ctx, ranked(), first.ID, player(1, 100)); !errors.Is(err, ErrTicketMatched) {
		t.Fatalf("cancelling a matched ticket returned %v, want ErrTicketMatched", err)
	}
}

// The property the redesign exists for: concurrent committers on one queue
// must produce exactly one match, and no subject may appear in two.
func TestConcurrentCommitsProduceExactlyOneMatch(t *testing.T) {
	store, _ := newStore(t)
	ctx := context.Background()
	first := enqueue(t, store, ranked(), player(1, 100), "")
	second := enqueue(t, store, ranked(), player(2, 100), "")
	ids := []string{first.ID, second.ID}

	const committers = 12
	var wait sync.WaitGroup
	var mu sync.Mutex
	matches := map[string]bool{}
	conflicts := 0
	for i := 0; i < committers; i++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			match, err := store.Commit(ctx, ranked(), ids)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				matches[match.ID] = true
			case errors.Is(err, ErrConflict):
				conflicts++
			default:
				t.Errorf("unexpected error %v", err)
			}
		}()
	}
	wait.Wait()

	if len(matches) != 1 {
		t.Fatalf("%d distinct matches were created, want exactly 1", len(matches))
	}
	if conflicts != 0 {
		t.Fatalf("%d exact ticket-set retries were refused, want 0", conflicts)
	}
	if length, _ := store.QueueLength(ctx, ranked()); length != 0 {
		t.Fatalf("queue holds %d tickets after the race", length)
	}
}

// Concurrent enqueues for one subject must produce one ticket.
func TestConcurrentEnqueuesForOneSubjectProduceOneTicket(t *testing.T) {
	store, _ := newStore(t)
	ctx := context.Background()

	const racers = 12
	var wait sync.WaitGroup
	var mu sync.Mutex
	tickets := map[string]bool{}
	rejected := 0
	for i := 0; i < racers; i++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			ticket, err := store.Enqueue(ctx, ranked(), player(1, 100), "")
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				tickets[ticket.ID] = true
			case errors.Is(err, ErrAlreadyQueued):
				rejected++
			default:
				t.Errorf("unexpected error %v", err)
			}
		}()
	}
	wait.Wait()
	if len(tickets) != 1 {
		t.Fatalf("%d tickets were created for one subject, want 1", len(tickets))
	}
	if rejected != racers-1 {
		t.Fatalf("%d enqueues were rejected, want %d", rejected, racers-1)
	}
	length, _ := store.QueueLength(ctx, ranked())
	if length != 1 {
		t.Fatalf("queue length = %d, want 1", length)
	}
}

// A commit must refuse a group containing the same subject twice, whatever the
// caller passes.
func TestCommitRefusesDuplicateTicketsAndSubjects(t *testing.T) {
	store, _ := newStore(t)
	ctx := context.Background()
	ticket := enqueue(t, store, ranked(), player(1, 100), "")

	if _, err := store.Commit(ctx, ranked(), []string{ticket.ID, ticket.ID}); !errors.Is(err, ErrTicketInvalid) {
		t.Fatalf("a duplicated ticket id was accepted: %v", err)
	}
	if _, err := store.Commit(ctx, ranked(), []string{ticket.ID}); !errors.Is(err, ErrTicketInvalid) {
		t.Fatalf("a group of the wrong size was accepted: %v", err)
	}
	if _, err := store.Commit(ctx, ranked(), []string{ticket.ID, ""}); !errors.Is(err, ErrTicketInvalid) {
		t.Fatalf("an empty ticket id was accepted: %v", err)
	}
}

// Candidates is bounded, in queue order, and excludes resolved tickets. The
// implementation this replaces read every queued ticket on every enqueue.
func TestCandidatesAreBoundedAndInQueueOrder(t *testing.T) {
	store, _ := newStore(t)
	ctx := context.Background()
	queue := Queue{Mode: "ranked", GroupSize: 4, Partition: "eu"}
	for id := int64(1); id <= 10; id++ {
		enqueue(t, store, queue, player(id, id*10), "")
	}
	candidates, err := store.Candidates(ctx, queue, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 3 {
		t.Fatalf("got %d candidates, want 3", len(candidates))
	}
	for index, candidate := range candidates {
		if candidate.Subject.ID != int64(index+1) {
			t.Fatalf("position %d is subject %d; candidates must be oldest first", index, candidate.Subject.ID)
		}
	}
	for _, limit := range []int{0, -1, MaxPageSize + 1} {
		if _, err := store.Candidates(ctx, queue, limit); !errors.Is(err, ErrQueueInvalid) {
			t.Fatalf("limit %d returned %v, want ErrQueueInvalid", limit, err)
		}
	}
}

// Queues are isolated: mode, group size and partition all separate pools.
func TestQueuesAreIsolatedByModeSizeAndPartition(t *testing.T) {
	store, _ := newStore(t)
	ctx := context.Background()
	base := Queue{Mode: "ranked", GroupSize: 2, Partition: "eu"}
	variants := []Queue{
		{Mode: "casual", GroupSize: 2, Partition: "eu"},
		{Mode: "ranked", GroupSize: 4, Partition: "eu"},
		{Mode: "ranked", GroupSize: 2, Partition: "us"},
	}
	enqueue(t, store, base, player(1, 100), "")
	for _, variant := range variants {
		// The same subject may queue in a different pool.
		if _, err := store.Enqueue(ctx, variant, player(1, 100), ""); err != nil {
			t.Fatalf("queue %s rejected the subject: %v", variant.Key(), err)
		}
		if length, _ := store.QueueLength(ctx, variant); length != 1 {
			t.Fatalf("queue %s holds %d tickets", variant.Key(), length)
		}
	}
	if length, _ := store.QueueLength(ctx, base); length != 1 {
		t.Fatalf("the base queue holds %d tickets", length)
	}
}

func TestInvalidInputIsRejectedWithABusinessError(t *testing.T) {
	store, _ := newStore(t)
	ctx := context.Background()
	for _, testCase := range []struct {
		label string
		queue Queue
	}{
		{"empty mode", Queue{GroupSize: 2}},
		{"group size below two", Queue{Mode: "ranked", GroupSize: 1}},
		{"group size above the cap", Queue{Mode: "ranked", GroupSize: MaxGroupSize + 1}},
	} {
		if _, err := store.Enqueue(ctx, testCase.queue, player(1, 0), ""); !errors.Is(err, ErrQueueInvalid) {
			t.Fatalf("%s: %v", testCase.label, err)
		}
	}
	for _, testCase := range []struct {
		label   string
		subject Subject
	}{
		{"empty kind", Subject{ID: 1}},
		{"zero id", Subject{Kind: "player"}},
		{"oversized payload", Subject{Kind: "player", ID: 1, Payload: make([]byte, MaxPayloadBytes+1)}},
	} {
		if _, err := store.Enqueue(ctx, ranked(), testCase.subject, ""); !errors.Is(err, ErrSubjectInvalid) {
			t.Fatalf("%s: %v", testCase.label, err)
		}
	}
}

func TestNewStoreRejectsAnUnsafeConfig(t *testing.T) {
	state := versionstore.NewMemoryStore[string, queueState]()
	if _, err := NewStore(nil, Config{}); err == nil {
		t.Fatal("a nil state store was accepted")
	}
	if _, err := NewStore(state, Config{TicketTTL: -time.Second}); err == nil {
		t.Fatal("a negative ticket ttl was accepted")
	}
	store, err := NewStore(state, Config{})
	if err != nil {
		t.Fatal(err)
	}
	if store == nil {
		t.Fatal("NewStore returned nil with no error")
	}
}

func TestQueueLengthCountsOnlyLiveTickets(t *testing.T) {
	store, c := newStore(t, func(cfg *Config) { cfg.TicketTTL = time.Minute })
	ctx := context.Background()
	queue := Queue{Mode: "ranked", GroupSize: 4, Partition: "eu"}
	live := enqueue(t, store, queue, player(1, 0), "")
	cancelled := enqueue(t, store, queue, player(2, 0), "")
	expiring := enqueue(t, store, queue, player(3, 0), "")
	_ = expiring

	if _, err := store.Cancel(ctx, queue, cancelled.ID, player(2, 0)); err != nil {
		t.Fatal(err)
	}
	if length, _ := store.QueueLength(ctx, queue); length != 2 {
		t.Fatalf("length = %d, want 2 after one cancel", length)
	}
	c.advance(61 * time.Second)
	if length, _ := store.QueueLength(ctx, queue); length != 0 {
		t.Fatalf("length = %d, want 0 once everything expired", length)
	}
	_ = live
}

// Sweep is bounded so a large backlog cannot make one call unbounded work.
func TestSweepIsBounded(t *testing.T) {
	store, c := newStore(t, func(cfg *Config) { cfg.TicketTTL = time.Minute })
	ctx := context.Background()
	queue := Queue{Mode: "ranked", GroupSize: 8, Partition: "eu"}
	for id := int64(1); id <= 10; id++ {
		enqueue(t, store, queue, player(id, 0), "")
	}
	c.advance(61 * time.Second)
	resolved, err := store.Sweep(ctx, queue, 4)
	if err != nil {
		t.Fatal(err)
	}
	if resolved != 4 {
		t.Fatalf("a bounded sweep resolved %d, want 4", resolved)
	}
	total := resolved
	for {
		more, err := store.Sweep(ctx, queue, 4)
		if err != nil {
			t.Fatal(err)
		}
		if more == 0 {
			break
		}
		total += more
	}
	if total != 10 {
		t.Fatalf("repeated sweeps resolved %d of 10", total)
	}
}

// Grouping is the seam for game policy, and the default must not pretend to do
// more than it does. The implementation this replaces advertised
// score-adjacent matching while always taking the head of the queue.
func TestFirstComeGroupingTakesTheOldest(t *testing.T) {
	queue := Queue{Mode: "ranked", GroupSize: 2}
	candidates := []Ticket{
		{ID: "a", Subject: player(1, 500), CreatedAtUnix: 10},
		{ID: "b", Subject: player(2, 100), CreatedAtUnix: 20},
		{ID: "c", Subject: player(3, 110), CreatedAtUnix: 30},
	}
	group, ok, err := FirstComeGrouping{}.Group(queue, candidates)
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if group[0].ID != "a" || group[1].ID != "b" {
		t.Fatalf("first-come grouping returned %s,%s", group[0].ID, group[1].ID)
	}
	if _, ok, _ := (FirstComeGrouping{}).Group(queue, candidates[:1]); ok {
		t.Fatal("a group was formed from too few candidates")
	}
}

// The score window widens with the oldest candidate's wait, so an outlier
// eventually matches instead of starving — the property a fixed window lacks.
func TestScoreWindowWidensSoAnOutlierEventuallyMatches(t *testing.T) {
	now := int64(1000)
	grouping := ScoreWindowGrouping{
		InitialWindow:  10,
		WidenPerSecond: 10,
		NowUnix:        func() int64 { return now },
	}
	queue := Queue{Mode: "ranked", GroupSize: 2}
	candidates := []Ticket{
		{ID: "outlier", Subject: player(1, 0), CreatedAtUnix: 1000},
		{ID: "far", Subject: player(2, 500), CreatedAtUnix: 1000},
	}
	if _, ok, err := grouping.Group(queue, candidates); err != nil || ok {
		t.Fatalf("a far candidate matched immediately: ok=%v err=%v", ok, err)
	}
	// After fifty seconds the window is 10 + 10*50 = 510, wide enough.
	now = 1050
	group, ok, err := grouping.Group(queue, candidates)
	if err != nil || !ok {
		t.Fatalf("the window did not widen: ok=%v err=%v", ok, err)
	}
	if group[0].ID != "outlier" {
		t.Fatalf("the group is not anchored on the oldest candidate: %s", group[0].ID)
	}
	// The cap is honoured.
	capped := grouping
	capped.MaxWindow = 20
	if _, ok, _ := capped.Group(queue, candidates); ok {
		t.Fatal("MaxWindow was ignored")
	}
	// And it needs a clock rather than silently assuming one.
	if _, _, err := (ScoreWindowGrouping{InitialWindow: 10}).Group(queue, candidates); err == nil {
		t.Fatal("score window grouping ran without a clock")
	}
}

// Closest scores to the anchor come first, deterministically.
func TestScoreWindowPrefersTheClosestScores(t *testing.T) {
	grouping := ScoreWindowGrouping{
		InitialWindow: 1000,
		NowUnix:       func() int64 { return 1000 },
	}
	queue := Queue{Mode: "ranked", GroupSize: 3}
	candidates := []Ticket{
		{ID: "anchor", Subject: player(1, 100), CreatedAtUnix: 1000},
		{ID: "far", Subject: player(2, 900), CreatedAtUnix: 1001},
		{ID: "near", Subject: player(3, 110), CreatedAtUnix: 1002},
		{ID: "mid", Subject: player(4, 300), CreatedAtUnix: 1003},
	}
	group, ok, err := grouping.Group(queue, candidates)
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	want := []string{"anchor", "near", "mid"}
	for index, id := range want {
		if group[index].ID != id {
			t.Fatalf("position %d is %s, want %s (full: %v)", index, group[index].ID, id,
				[]string{group[0].ID, group[1].ID, group[2].ID})
		}
	}
	// Deterministic across repeats.
	for i := 0; i < 8; i++ {
		again, _, _ := grouping.Group(queue, candidates)
		for index := range want {
			if again[index].ID != group[index].ID {
				t.Fatal("grouping is not deterministic")
			}
		}
	}
}

func TestQueueKeyIsStableAndDistinct(t *testing.T) {
	seen := map[string]bool{}
	for _, queue := range []Queue{
		{Mode: "ranked", GroupSize: 2, Partition: "eu"},
		{Mode: "ranked", GroupSize: 2, Partition: "us"},
		{Mode: "ranked", GroupSize: 4, Partition: "eu"},
		{Mode: "casual", GroupSize: 2, Partition: "eu"},
	} {
		key := queue.Key()
		if seen[key] {
			t.Fatalf("two distinct queues share the key %q", key)
		}
		seen[key] = true
		if again := queue.Key(); again != key {
			t.Fatal("Queue.Key is not stable")
		}
	}
	_ = fmt.Sprint()
}

// A resolved ticket must leave the waiting list. Its absence is not a
// correctness break — Candidates filters on state — but the list would then
// grow without bound for the life of the queue, and every mutation carries the
// whole queue state in one versioned value, so unbounded growth there is a
// slow failure rather than a leak in a corner.
//
// This asserts the internal invariant directly. The observable behaviour
// (queue length, candidates) cannot see it, which is why the mutation that
// removed the cleanup passed the behavioural assertions.
func TestResolvedTicketsLeaveTheWaitingList(t *testing.T) {
	state := versionstore.NewMemoryStore[string, queueState]()
	c := &clock{now: time.Unix(1_700_000_000, 0)}
	store, err := NewStore(state, Config{Now: c.Now, TicketTTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	queue := ranked()

	waitingLen := func() int {
		t.Helper()
		current, found, err := state.Get(ctx, queue.Key())
		if err != nil || !found {
			t.Fatalf("state: found=%v err=%v", found, err)
		}
		return len(current.Value.Waiting)
	}
	subjectMappings := func() int {
		t.Helper()
		current, _, _ := state.Get(ctx, queue.Key())
		return len(current.Value.SubjectTickets)
	}

	// Matched tickets.
	first := enqueue(t, store, queue, player(1, 100), "")
	second := enqueue(t, store, queue, player(2, 100), "")
	if waitingLen() != 2 {
		t.Fatalf("waiting list holds %d before the commit, want 2", waitingLen())
	}
	if _, err := store.Commit(ctx, queue, []string{first.ID, second.ID}); err != nil {
		t.Fatal(err)
	}
	if got := waitingLen(); got != 0 {
		t.Fatalf("waiting list holds %d matched tickets after the commit; resolved tickets must be removed", got)
	}
	if got := subjectMappings(); got != 0 {
		t.Fatalf("%d live-subject mappings survived the commit", got)
	}

	// Cancelled tickets.
	third := enqueue(t, store, queue, player(3, 100), "")
	if _, err := store.Cancel(ctx, queue, third.ID, player(3, 100)); err != nil {
		t.Fatal(err)
	}
	if got := waitingLen(); got != 0 {
		t.Fatalf("waiting list holds %d cancelled tickets", got)
	}
	if got := subjectMappings(); got != 0 {
		t.Fatalf("%d live-subject mappings survived a cancel", got)
	}

	// Expired tickets.
	enqueue(t, store, queue, player(4, 100), "")
	c.advance(61 * time.Second)
	if _, err := store.Sweep(ctx, queue, 10); err != nil {
		t.Fatal(err)
	}
	if got := waitingLen(); got != 0 {
		t.Fatalf("waiting list holds %d expired tickets", got)
	}
	if got := subjectMappings(); got != 0 {
		t.Fatalf("%d live-subject mappings survived expiry", got)
	}
}

// The three signals the implementation this replaces could not emit: queue
// depth (its tickets lived in per-subject records with no aggregate), expired
// tickets (it wrote a deadline nothing read), and retried enqueues. Each is
// asserted rather than assumed, because a report call nothing reaches is
// indistinguishable from no report call at all.
func TestQueueDepthDropsAndReplaysAreReported(t *testing.T) {
	sink := servicemetrics.NewRecorder()
	store, c := newStore(t, func(cfg *Config) {
		cfg.Metrics = sink
		cfg.TicketTTL = time.Minute
	})
	ctx := context.Background()

	enqueue(t, store, ranked(), player(1, 100), "req-1")
	enqueue(t, store, ranked(), player(2, 110), "req-2")
	if got := sink.Count("accepted:enqueue"); got != 2 {
		t.Fatalf("two enqueues reported %d accepts; %s", got, sink.Events())
	}
	// C6：队列深度是固定名 queue 加 key 标签，队列标识不进指标名（N12 O1）。
	if got := sink.Count("depth:queue{" + ranked().Key() + "}"); got != 2 {
		t.Fatalf("queue depth reported %d under the fixed name queue keyed by the queue, want 2; %s", got, sink.Events())
	}
	if got := sink.Count("depth:queue." + ranked().Key()); got != 0 {
		t.Fatalf("the queue key is in a depth name (queue.%s); %s", ranked().Key(), sink.Events())
	}

	// A retry is not a third player.
	enqueue(t, store, ranked(), player(1, 100), "req-1")
	if got := sink.Count("replayed:enqueue"); got != 1 {
		t.Fatalf("a retried enqueue reported %d replays; %s", got, sink.Events())
	}
	if got := sink.Count("accepted:enqueue"); got != 2 {
		t.Fatalf("a retried enqueue was also counted as accepted (%d); %s", got, sink.Events())
	}

	// A second live ticket for the same subject is refused, with a reason.
	if _, err := store.Enqueue(ctx, ranked(), player(1, 100), "req-3"); !errors.Is(err, ErrAlreadyQueued) {
		t.Fatalf("a subject queued twice: %v", err)
	}
	if got := sink.Count("refused:enqueue:already_queued"); got != 1 {
		t.Fatalf("a double enqueue reported %d refusals; %s", got, sink.Events())
	}

	// Expired tickets are dropped data: players who waited and got nothing.
	c.advance(2 * time.Minute)
	resolved, err := store.Sweep(ctx, ranked(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if resolved != 2 {
		t.Fatalf("the sweep resolved %d tickets, want 2", resolved)
	}
	if got := sink.Count("dropped:ticket.expired"); got != 2 {
		t.Fatalf("the sweep reported %d expired tickets, want 2; %s", got, sink.Events())
	}
}

// A committed match is the accepted outcome the whole queue exists to reach.
func TestACommittedMatchIsReported(t *testing.T) {
	sink := servicemetrics.NewRecorder()
	store, _ := newStore(t, func(cfg *Config) { cfg.Metrics = sink })
	ctx := context.Background()

	first := enqueue(t, store, ranked(), player(1, 100), "req-1")
	second := enqueue(t, store, ranked(), player(2, 110), "req-2")
	if _, err := store.Commit(ctx, ranked(), []string{first.ID, second.ID}); err != nil {
		t.Fatal(err)
	}
	if got := sink.Count("accepted:commit"); got != 1 {
		t.Fatalf("a commit reported %d accepts; %s", got, sink.Events())
	}
}

// A nil reporter must never change behaviour.
func TestANilReporterChangesNothing(t *testing.T) {
	store, _ := newStore(t)
	ctx := context.Background()
	if _, err := store.Enqueue(ctx, ranked(), player(1, 100), "req-1"); err != nil {
		t.Fatalf("a store with no reporter failed to enqueue: %v", err)
	}
	if _, err := store.Sweep(ctx, ranked(), 10); err != nil {
		t.Fatal(err)
	}
}

// --- 已结束记录的保留期 ---

// newStoreWithState 与 newStore 相同，另外交出底层状态，让用例能核对队列状态里实际留了什么。
func newStoreWithState(t *testing.T, options ...func(*Config)) (Store, *clock, versionstore.Store[string, queueState]) {
	t.Helper()
	c := &clock{now: time.Unix(1_700_000_000, 0)}
	cfg := Config{Now: c.Now}
	for _, option := range options {
		option(&cfg)
	}
	state := versionstore.NewMemoryStore[string, queueState]()
	store, err := NewStore(state, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return store, c, state
}

// stateSize 是队列状态编码后的字节数与三张只增表的条目数：Redis 存储每次 Enqueue / Commit / Sweep 都整体读写这一个值。
func stateSize(t *testing.T, state versionstore.Store[string, queueState], queue Queue) (bytes, tickets, matches, requests int) {
	t.Helper()
	current, found, err := state.Get(context.Background(), queue.Key())
	if err != nil || !found {
		t.Fatalf("state: found=%v err=%v", found, err)
	}
	encoded, err := versionstore.JSONCodec[queueState]{}.Encode(current.Value)
	if err != nil {
		t.Fatal(err)
	}
	return len(encoded), len(current.Value.Tickets), len(current.Value.Matches), len(current.Value.Requests)
}

// 状态有界：已结束的票（成局 / 取消 / 过期）、已完成的比赛与它们的排队请求记录在保留期过后由 Sweep 移除，
// 整个队列状态回落到只剩进行中的记录。修前三张表只增不删，每次操作整体读写的值随历史线性增长。
func TestResolvedRecordsArePrunedAfterRetention(t *testing.T) {
	store, c, state := newStoreWithState(t, func(cfg *Config) { cfg.TicketTTL = time.Minute })
	ctx := context.Background()
	queue := ranked()

	const players = 400
	tickets := make([]Ticket, 0, players)
	for id := int64(1); id <= players; id++ {
		tickets = append(tickets, enqueue(t, store, queue, player(id, 100), fmt.Sprintf("req-%d", id)))
	}
	// 前 300 人两两成局，接下来 50 人取消，最后 50 人等到过期。
	for i := 0; i < 300; i += 2 {
		if _, err := store.Commit(ctx, queue, []string{tickets[i].ID, tickets[i+1].ID}); err != nil {
			t.Fatal(err)
		}
	}
	for _, ticket := range tickets[300:350] {
		if _, err := store.Cancel(ctx, queue, ticket.ID, ticket.Subject); err != nil {
			t.Fatal(err)
		}
	}
	c.advance(61 * time.Second)
	if _, err := store.Sweep(ctx, queue, MaxPageSize); err != nil {
		t.Fatal(err)
	}
	grown, _, _, _ := stateSize(t, state, queue)

	// 最晚结束的是刚过期的 50 张票；从它们算起满保留期（加秒取整的 1s）后清理。
	c.advance(DefaultRetention + time.Second)
	if _, err := store.Sweep(ctx, queue, MaxPageSize); err != nil {
		t.Fatal(err)
	}
	bytes, ticketCount, matchCount, requestCount := stateSize(t, state, queue)
	if ticketCount != 0 || matchCount != 0 || requestCount != 0 {
		t.Fatalf("after retention the queue state still holds %d tickets, %d matches, %d requests (%d bytes, was %d); "+
			"resolved records must be pruned so the state stays bounded", ticketCount, matchCount, requestCount, bytes, grown)
	}
	if bytes*20 > grown {
		t.Fatalf("the queue state is %d bytes after pruning, was %d", bytes, grown)
	}
	current, _, _ := state.Get(ctx, queue.Key())
	if len(current.Value.Waiting) != 0 || len(current.Value.SubjectTickets) != 0 {
		t.Fatalf("pruning left %d waiting ids and %d subject mappings", len(current.Value.Waiting), len(current.Value.SubjectTickets))
	}
}

// 超过保留期的同一票集合重放：比赛与它的票已一起移除，Commit 报 ErrTicketMissing，不创建第二局；
// 读票、读比赛都报不存在。
func TestCommitReplayAfterRetentionReportsTheTicketsMissing(t *testing.T) {
	store, c := newStore(t)
	ctx := context.Background()
	a, b := enqueue(t, store, ranked(), player(1, 100), "a"), enqueue(t, store, ranked(), player(2, 100), "b")
	first, err := store.Commit(ctx, ranked(), []string{a.ID, b.ID})
	if err != nil {
		t.Fatal(err)
	}
	c.advance(DefaultRetention + time.Second)
	if _, err := store.Sweep(ctx, ranked(), MaxPageSize); err != nil {
		t.Fatal(err)
	}
	replay, err := store.Commit(ctx, ranked(), []string{b.ID, a.ID})
	if !errors.Is(err, ErrTicketMissing) {
		t.Fatalf("replay after retention = (%s, %v), want ErrTicketMissing (original match %s)", replay.ID, err, first.ID)
	}
	if _, found, err := store.Ticket(ctx, ranked(), a.ID, player(1, 100)); err != nil || found {
		t.Fatalf("a pruned ticket reads as found=%v err=%v", found, err)
	}
	if _, found, err := store.Match(ctx, ranked(), first.ID); err != nil || found {
		t.Fatalf("a pruned match reads as found=%v err=%v", found, err)
	}
}

// 保留期内（含边界）重放返回原 Match，读票、读比赛照旧；越过边界后同一重放报 ErrTicketMissing。
// 中间插入的 Sweep 与另一名玩家的 Enqueue 都是会触发清理的写入，用来证明清理不提前。
func TestCommitReplayInsideRetentionReturnsTheOriginalMatch(t *testing.T) {
	const retention = 10 * time.Minute
	store, c := newStore(t, func(cfg *Config) { cfg.Retention = retention; cfg.TicketTTL = time.Hour })
	ctx := context.Background()
	a, b := enqueue(t, store, ranked(), player(1, 100), "a"), enqueue(t, store, ranked(), player(2, 100), "b")
	first, err := store.Commit(ctx, ranked(), []string{a.ID, b.ID})
	if err != nil {
		t.Fatal(err)
	}
	c.advance(retention)
	if _, err := store.Sweep(ctx, ranked(), MaxPageSize); err != nil {
		t.Fatal(err)
	}
	enqueue(t, store, ranked(), player(3, 100), "c")
	again, err := store.Commit(ctx, ranked(), []string{b.ID, a.ID})
	if err != nil || again.ID != first.ID {
		t.Fatalf("replay at the retention boundary = (%s, %v), want the original %s", again.ID, err, first.ID)
	}
	ticket, found, err := store.Ticket(ctx, ranked(), a.ID, player(1, 100))
	if err != nil || !found || ticket.State != TicketMatched || ticket.MatchID != first.ID {
		t.Fatalf("ticket inside retention = (%+v, %v, %v)", ticket, found, err)
	}
	if _, found, err := store.Match(ctx, ranked(), first.ID); err != nil || !found {
		t.Fatalf("match inside retention: found=%v err=%v", found, err)
	}

	c.advance(time.Second)
	if replay, err := store.Commit(ctx, ranked(), []string{a.ID, b.ID}); !errors.Is(err, ErrTicketMissing) {
		t.Fatalf("replay past retention = (%s, %v), want ErrTicketMissing", replay.ID, err)
	}
}

// 清理只碰终态：等得比保留期还久的等待票、保留期内的比赛与票都不动；等待票的请求记录与“一人一张活票”映射也保留。
func TestPruningNeverTouchesLiveOrRecentRecords(t *testing.T) {
	store, c, state := newStoreWithState(t, func(cfg *Config) {
		cfg.Retention = time.Minute
		cfg.TicketTTL = time.Hour
	})
	ctx := context.Background()
	queue := ranked()
	waiting := enqueue(t, store, queue, player(1, 100), "waiting")
	b, cc := enqueue(t, store, queue, player(2, 100), "b"), enqueue(t, store, queue, player(3, 100), "c")
	old, err := store.Commit(ctx, queue, []string{b.ID, cc.ID})
	if err != nil {
		t.Fatal(err)
	}
	cancelled := enqueue(t, store, queue, player(4, 100), "d")
	if _, err := store.Cancel(ctx, queue, cancelled.ID, cancelled.Subject); err != nil {
		t.Fatal(err)
	}

	c.advance(30 * time.Minute)
	e, f := enqueue(t, store, queue, player(5, 100), "e"), enqueue(t, store, queue, player(6, 100), "f")
	recent, err := store.Commit(ctx, queue, []string{e.ID, f.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Sweep(ctx, queue, MaxPageSize); err != nil {
		t.Fatal(err)
	}

	// 等了 30 分钟（保留期 1 分钟）的等待票还在：可读、是候选、占着主体、请求记录可重放。
	read, found, err := store.Ticket(ctx, queue, waiting.ID, player(1, 100))
	if err != nil || !found || read.State != TicketWaiting {
		t.Fatalf("the long-waiting ticket = (%+v, %v, %v)", read, found, err)
	}
	candidates, err := store.Candidates(ctx, queue, 10)
	if err != nil || len(candidates) != 1 || candidates[0].ID != waiting.ID {
		t.Fatalf("candidates = %+v %v, want only %s", candidates, err, waiting.ID)
	}
	if _, err := store.Enqueue(ctx, queue, player(1, 100), "another"); !errors.Is(err, ErrAlreadyQueued) {
		t.Fatalf("the waiting subject enqueued again: %v", err)
	}
	if replay, err := store.Enqueue(ctx, queue, player(1, 100), "waiting"); err != nil || replay.ID != waiting.ID {
		t.Fatalf("the waiting ticket's request replay = (%s, %v)", replay.ID, err)
	}
	// 保留期内的比赛与票都在。
	if _, found, _ := store.Match(ctx, queue, recent.ID); !found {
		t.Fatal("a match inside retention was pruned")
	}
	if read, found, _ := store.Ticket(ctx, queue, e.ID, player(5, 100)); !found || read.MatchID != recent.ID {
		t.Fatalf("a matched ticket inside retention = (%+v, %v)", read, found)
	}
	// 到期的比赛与它的票一起走，已取消的票也走。
	if _, found, _ := store.Match(ctx, queue, old.ID); found {
		t.Fatal("a match past retention survived the sweep")
	}
	for _, ticket := range []Ticket{b, cc, cancelled} {
		if _, found, _ := store.Ticket(ctx, queue, ticket.ID, ticket.Subject); found {
			t.Fatalf("ticket %s past retention survived the sweep", ticket.ID)
		}
	}
	current, _, _ := state.Get(ctx, queue.Key())
	for request := range current.Value.Requests {
		if request != "waiting" && request != "e" && request != "f" {
			t.Fatalf("request %q outlived its ticket", request)
		}
	}
	// 等待票照常能成局。
	g := enqueue(t, store, queue, player(7, 100), "g")
	if _, err := store.Commit(ctx, queue, []string{waiting.ID, g.ID}); err != nil {
		t.Fatalf("the long-waiting ticket cannot be committed after pruning: %v", err)
	}
}

// 排队请求记录与票同寿：保留期内同一 requestID 重试返回原票（即使已取消），超期后是一次新的排队。
func TestEnqueueRetryAfterRetentionQueuesAgain(t *testing.T) {
	store, c := newStore(t, func(cfg *Config) { cfg.Retention = time.Minute })
	ctx := context.Background()
	first := enqueue(t, store, ranked(), player(1, 100), "req")
	if _, err := store.Cancel(ctx, ranked(), first.ID, player(1, 100)); err != nil {
		t.Fatal(err)
	}
	c.advance(time.Minute)
	if replay := enqueue(t, store, ranked(), player(1, 100), "req"); replay.ID != first.ID || replay.State != TicketCancelled {
		t.Fatalf("retry inside retention = %+v, want the cancelled ticket %s", replay, first.ID)
	}
	c.advance(time.Second)
	fresh := enqueue(t, store, ranked(), player(1, 100), "req")
	if fresh.ID == first.ID || fresh.State != TicketWaiting {
		t.Fatalf("retry past retention = %+v, want a new waiting ticket", fresh)
	}
}

// 清理与 Commit 共用同一次比较并设置：并发的 Commit 与 Sweep 在保留期内只指向一场比赛，
// 超期后全部报 ErrTicketMissing、也不生成新比赛。
func TestConcurrentCommitsAndSweepsAgreeAcrossRetention(t *testing.T) {
	store, c, state := newStoreWithState(t, func(cfg *Config) { cfg.Retention = time.Minute })
	ctx := context.Background()
	queue := ranked()
	a, b := enqueue(t, store, queue, player(1, 100), ""), enqueue(t, store, queue, player(2, 100), "")
	ids := []string{a.ID, b.ID}

	race := func() (map[string]bool, int) {
		var wait sync.WaitGroup
		var mu sync.Mutex
		matches, missing := map[string]bool{}, 0
		for i := 0; i < 12; i++ {
			wait.Add(2)
			go func() {
				defer wait.Done()
				match, err := store.Commit(ctx, queue, ids)
				mu.Lock()
				defer mu.Unlock()
				switch {
				case err == nil:
					matches[match.ID] = true
				case errors.Is(err, ErrTicketMissing):
					missing++
				default:
					t.Errorf("unexpected commit error %v", err)
				}
			}()
			go func() {
				defer wait.Done()
				if _, err := store.Sweep(ctx, queue, MaxPageSize); err != nil {
					t.Errorf("sweep: %v", err)
				}
			}()
		}
		wait.Wait()
		return matches, missing
	}

	matches, missing := race()
	if len(matches) != 1 || missing != 0 {
		t.Fatalf("inside retention: %d distinct matches, %d missing; want 1 and 0", len(matches), missing)
	}
	c.advance(time.Minute + time.Second)
	matches, missing = race()
	if len(matches) != 0 || missing != 12 {
		t.Fatalf("past retention: %d matches, %d missing; want 0 and 12", len(matches), missing)
	}
	if _, tickets, matchCount, _ := stateSize(t, state, queue); tickets != 0 || matchCount != 0 {
		t.Fatalf("past retention the state holds %d tickets and %d matches", tickets, matchCount)
	}
}

func TestRetentionConfigIsValidated(t *testing.T) {
	state := versionstore.NewMemoryStore[string, queueState]()
	for _, bad := range []time.Duration{-time.Second, 500 * time.Millisecond} {
		if _, err := NewStore(state, Config{Retention: bad}); err == nil {
			t.Errorf("retention %v accepted", bad)
		}
	}
	store, err := NewStore(state, Config{})
	if err != nil {
		t.Fatal(err)
	}
	if got := store.(*queueStore).cfg.Retention; got != DefaultRetention {
		t.Fatalf("zero retention selected %v, want DefaultRetention %v", got, DefaultRetention)
	}
}

// --- 跨分区成局（CommitAcross）---

// tiered 是同一逻辑队列（ranked / 3 人一局）按段位档拆出的分区。
func tiered(partition string) Queue { return Queue{Mode: "ranked", GroupSize: 3, Partition: partition} }

func refsOf(tickets ...Ticket) TicketRefs {
	refs := make(TicketRefs, 0, len(tickets))
	for _, ticket := range tickets {
		refs = append(refs, TicketRef{Queue: ticket.Queue, TicketID: ticket.ID})
	}
	return refs
}

// stateVersion 是一个分区状态的当前版本，用来证明失败的跨分区提交一个分区也没写。
func stateVersion(t *testing.T, state versionstore.Store[string, queueState], queue Queue) uint64 {
	t.Helper()
	current, _, err := state.Get(context.Background(), queue.Key())
	if err != nil {
		t.Fatal(err)
	}
	return current.Version
}

// 跨分区成局：三张票来自两个分区，一次提交在两个分区同时生效——每张票在自己的分区里已匹配并指向同一场比赛，
// 比赛在两个分区各有一份（TicketPartitions 记每张票的分区），两个分区的等待队列都清空。
func TestCommitAcrossFormsOneMatchVisibleFromEveryPartition(t *testing.T) {
	store, _ := newStore(t)
	ctx := context.Background()
	gold, silver := tiered("gold"), tiered("silver")
	a := enqueue(t, store, gold, player(1, 1500), "a")
	b := enqueue(t, store, gold, player(2, 1490), "b")
	c := enqueue(t, store, silver, player(3, 1300), "c")

	match, err := store.CommitAcross(ctx, refsOf(a, c, b))
	if err != nil {
		t.Fatal(err)
	}
	if match.ID == "" || len(match.Members) != 3 || match.Queue != gold {
		t.Fatalf("match = %+v", match)
	}
	if want := []string{a.ID, c.ID, b.ID}; fmt.Sprint(match.TicketIDs) != fmt.Sprint(want) {
		t.Fatalf("ticket ids = %v, want member order %v", match.TicketIDs, want)
	}
	if want := []string{"gold", "silver", "gold"}; fmt.Sprint(match.TicketPartitions) != fmt.Sprint(want) {
		t.Fatalf("ticket partitions = %v, want %v", match.TicketPartitions, want)
	}
	for _, ticket := range []Ticket{a, b, c} {
		read, found, err := store.Ticket(ctx, ticket.Queue, ticket.ID, ticket.Subject)
		if err != nil || !found || read.State != TicketMatched || read.MatchID != match.ID {
			t.Fatalf("ticket %s in %s = (%+v, %v, %v), want matched into %s", ticket.ID, ticket.Queue.Partition, read, found, err, match.ID)
		}
	}
	for _, queue := range []Queue{gold, silver} {
		stored, found, err := store.Match(ctx, queue, match.ID)
		if err != nil || !found || fmt.Sprint(stored.TicketIDs) != fmt.Sprint(match.TicketIDs) {
			t.Fatalf("match read through %s = (%+v, %v, %v)", queue.Partition, stored, found, err)
		}
		if length, _ := store.QueueLength(ctx, queue); length != 0 {
			t.Fatalf("%s still holds %d waiting tickets", queue.Partition, length)
		}
	}
	// 已匹配的票不能取消，也不能再进别的局。
	if _, err := store.Cancel(ctx, silver, c.ID, c.Subject); !errors.Is(err, ErrTicketMatched) {
		t.Fatalf("cancelling a cross-partition matched ticket = %v, want ErrTicketMatched", err)
	}
}

// 原子性：任何一张票不再等待（被取消 / 被单分区 Commit 拿走 / 不存在）时整次失败，其他分区一个字节也不改
// （版本不变、票仍在等待）。
func TestCommitAcrossIsAllOrNothing(t *testing.T) {
	store, _, state := newStoreWithState(t)
	ctx := context.Background()
	gold, silver := tiered("gold"), tiered("silver")
	a := enqueue(t, store, gold, player(1, 1500), "a")
	b := enqueue(t, store, gold, player(2, 1490), "b")
	c := enqueue(t, store, silver, player(3, 1300), "c")
	d := enqueue(t, store, silver, player(4, 1310), "d")
	e := enqueue(t, store, silver, player(5, 1320), "e")

	untouched := func(why string, queue Queue, version uint64, waiting ...Ticket) {
		t.Helper()
		if got := stateVersion(t, state, queue); got != version {
			t.Fatalf("%s: partition %s moved from version %d to %d", why, queue.Partition, version, got)
		}
		for _, ticket := range waiting {
			read, _, _ := store.Ticket(ctx, queue, ticket.ID, ticket.Subject)
			if read.State != TicketWaiting {
				t.Fatalf("%s: ticket %s in %s is %s, want waiting", why, ticket.ID, queue.Partition, read.State)
			}
		}
	}

	// 一张票被取消。
	if _, err := store.Cancel(ctx, silver, c.ID, c.Subject); err != nil {
		t.Fatal(err)
	}
	goldVersion, silverVersion := stateVersion(t, state, gold), stateVersion(t, state, silver)
	if _, err := store.CommitAcross(ctx, refsOf(a, b, c)); !errors.Is(err, ErrConflict) {
		t.Fatalf("a set with a cancelled ticket = %v, want ErrConflict", err)
	}
	untouched("cancelled ticket", gold, goldVersion, a, b)
	untouched("cancelled ticket", silver, silverVersion, d, e)

	// 一张票被别的单分区 Commit 拿走。
	if _, err := store.Commit(ctx, silver, []string{d.ID, e.ID, enqueue(t, store, silver, player(6, 1330), "f").ID}); err != nil {
		t.Fatal(err)
	}
	goldVersion, silverVersion = stateVersion(t, state, gold), stateVersion(t, state, silver)
	if _, err := store.CommitAcross(ctx, refsOf(a, b, d)); !errors.Is(err, ErrConflict) {
		t.Fatalf("a set with a ticket taken by another commit = %v, want ErrConflict", err)
	}
	untouched("taken ticket", gold, goldVersion, a, b)
	untouched("taken ticket", silver, silverVersion)

	// 一张票不存在；一个分区从没写过。
	missing := TicketRefs{{Queue: gold, TicketID: a.ID}, {Queue: gold, TicketID: b.ID}, {Queue: silver, TicketID: "does-not-exist"}}
	if _, err := store.CommitAcross(ctx, missing); !errors.Is(err, ErrTicketMissing) {
		t.Fatalf("a set naming a missing ticket = %v, want ErrTicketMissing", err)
	}
	empty := TicketRefs{{Queue: gold, TicketID: a.ID}, {Queue: gold, TicketID: b.ID}, {Queue: tiered("bronze"), TicketID: "x"}}
	if _, err := store.CommitAcross(ctx, empty); !errors.Is(err, ErrTicketMissing) {
		t.Fatalf("a set naming a partition that holds nothing = %v, want ErrTicketMissing", err)
	}
	untouched("missing ticket", gold, goldVersion, a, b)
	if _, found, _ := state.Get(ctx, tiered("bronze").Key()); found {
		t.Fatal("a failed cross-partition commit created the empty partition")
	}
}

// 重放：保留期内同一票集合（顺序可不同）返回原 Match、不成第二局；超期后报 ErrTicketMissing，
// 两个分区的比赛与票都已移除。
func TestCommitAcrossReplayFollowsRetention(t *testing.T) {
	const retention = 10 * time.Minute
	store, clk, state := newStoreWithState(t, func(cfg *Config) { cfg.Retention = retention; cfg.TicketTTL = time.Hour })
	ctx := context.Background()
	gold, silver := tiered("gold"), tiered("silver")
	a := enqueue(t, store, gold, player(1, 1500), "a")
	b := enqueue(t, store, silver, player(2, 1300), "b")
	c := enqueue(t, store, silver, player(3, 1310), "c")

	first, err := store.CommitAcross(ctx, refsOf(a, b, c))
	if err != nil {
		t.Fatal(err)
	}
	clk.advance(retention)
	for _, queue := range []Queue{gold, silver} {
		if _, err := store.Sweep(ctx, queue, MaxPageSize); err != nil {
			t.Fatal(err)
		}
	}
	again, err := store.CommitAcross(ctx, refsOf(c, a, b))
	if err != nil || again.ID != first.ID {
		t.Fatalf("replay at the retention boundary = (%s, %v), want the original %s", again.ID, err, first.ID)
	}
	// 部分重叠不是重放。
	d := enqueue(t, store, gold, player(4, 1500), "d")
	if _, err := store.CommitAcross(ctx, refsOf(a, b, d)); !errors.Is(err, ErrConflict) {
		t.Fatalf("a set overlapping a cross-partition match = %v, want ErrConflict", err)
	}

	clk.advance(time.Second)
	if replay, err := store.CommitAcross(ctx, refsOf(a, b, c)); !errors.Is(err, ErrTicketMissing) {
		t.Fatalf("replay past retention = (%s, %v), want ErrTicketMissing", replay.ID, err)
	}
	for _, queue := range []Queue{gold, silver} {
		if _, err := store.Sweep(ctx, queue, MaxPageSize); err != nil {
			t.Fatal(err)
		}
		current, _, _ := state.Get(ctx, queue.Key())
		if _, kept := current.Value.Matches[first.ID]; kept {
			t.Fatalf("partition %s kept the match past retention", queue.Partition)
		}
		if _, found, _ := store.Match(ctx, queue, first.ID); found {
			t.Fatalf("the match still reads through %s past retention", queue.Partition)
		}
	}
	for _, ticket := range []Ticket{a, b, c} {
		if _, found, _ := store.Ticket(ctx, ticket.Queue, ticket.ID, ticket.Subject); found {
			t.Fatalf("ticket %s still reads past retention", ticket.ID)
		}
	}
}

// 同一逻辑队列：Mode 或 GroupSize 不同的分区拒绝（ErrQueueInvalid）；张数、重复票、同一主体两张票、
// 票不在所说的分区，都在不改任何分区的前提下拒绝。全部票在一个分区时等同于 Commit。
func TestCommitAcrossRefusesSetsOutsideOneLogicalQueue(t *testing.T) {
	store, _ := newStore(t)
	ctx := context.Background()
	gold, silver := tiered("gold"), tiered("silver")
	a := enqueue(t, store, gold, player(1, 1500), "a")
	b := enqueue(t, store, gold, player(2, 1490), "b")
	c := enqueue(t, store, silver, player(3, 1300), "c")
	casual := enqueue(t, store, Queue{Mode: "casual", GroupSize: 3, Partition: "silver"}, player(7, 1), "")
	four := enqueue(t, store, Queue{Mode: "ranked", GroupSize: 4, Partition: "silver"}, player(8, 1), "")
	sameSubject := enqueue(t, store, silver, player(1, 1500), "a-again")

	for name, tc := range map[string]struct {
		refs TicketRefs
		want error
	}{
		"empty":                {nil, ErrTicketInvalid},
		"another mode":         {refsOf(a, b, casual), ErrQueueInvalid},
		"another group size":   {refsOf(a, b, four), ErrQueueInvalid},
		"too few tickets":      {refsOf(a, c), ErrTicketInvalid},
		"a ticket twice":       {refsOf(a, c, a), ErrTicketInvalid},
		"an empty ticket id":   {TicketRefs{{Queue: gold, TicketID: a.ID}, {Queue: silver, TicketID: c.ID}, {Queue: gold}}, ErrTicketInvalid},
		"one subject twice":    {refsOf(a, c, sameSubject), ErrTicketInvalid},
		"the wrong partition":  {TicketRefs{{Queue: gold, TicketID: a.ID}, {Queue: gold, TicketID: b.ID}, {Queue: gold, TicketID: c.ID}}, ErrTicketMissing},
		"an invalid partition": {TicketRefs{{Queue: Queue{Mode: "ranked", GroupSize: 1}, TicketID: a.ID}}, ErrQueueInvalid},
	} {
		if _, err := store.CommitAcross(ctx, tc.refs); !errors.Is(err, tc.want) {
			t.Errorf("%s: CommitAcross = %v, want %v", name, err, tc.want)
		}
	}
	for _, ticket := range []Ticket{a, b, c} {
		if read, _, _ := store.Ticket(ctx, ticket.Queue, ticket.ID, ticket.Subject); read.State != TicketWaiting {
			t.Fatalf("a refused set resolved ticket %s to %s", ticket.ID, read.State)
		}
	}

	// 全部票在一个分区：等同于 Commit，两条路径互认重放。
	d := enqueue(t, store, gold, player(4, 1480), "d")
	match, err := store.CommitAcross(ctx, refsOf(a, b, d))
	if err != nil || len(match.TicketPartitions) != 0 || match.Queue != gold {
		t.Fatalf("a one-partition CommitAcross = (%+v, %v), want a plain Commit", match, err)
	}
	if again, err := store.Commit(ctx, gold, []string{d.ID, a.ID, b.ID}); err != nil || again.ID != match.ID {
		t.Fatalf("Commit replay of a one-partition CommitAcross = (%s, %v), want %s", again.ID, err, match.ID)
	}
}

// 并发：跨分区成局与单分区的 Commit / Cancel 在同一批票上竞争。不变量：每张票至多进一局；
// 每场比赛在它涉及的每个分区都存在，它的每张票都在自己的分区里指向它（没有只生效了一半的比赛）。
func TestCommitAcrossRacesSinglePartitionOperations(t *testing.T) {
	store, _, state := newStoreWithState(t, func(cfg *Config) { cfg.TicketTTL = time.Hour })
	ctx := context.Background()
	partitions := []Queue{tiered("gold"), tiered("silver"), tiered("bronze")}
	var tickets []Ticket
	for i := 0; i < 18; i++ {
		queue := partitions[i%len(partitions)]
		tickets = append(tickets, enqueue(t, store, queue, player(int64(i+1), int64(i)), ""))
	}
	byPartition := func(queue Queue) []Ticket {
		var out []Ticket
		for _, ticket := range tickets {
			if ticket.Queue == queue {
				out = append(out, ticket)
			}
		}
		return out
	}

	var wait sync.WaitGroup
	run := func(job func() error) {
		wait.Add(1)
		go func() {
			defer wait.Done()
			if err := job(); err != nil && !errors.Is(err, ErrConflict) && !errors.Is(err, ErrTicketMissing) && !errors.Is(err, ErrTicketMatched) {
				t.Errorf("unexpected error: %v", err)
			}
		}()
	}
	for round := 0; round < 6; round++ {
		// 跨三个分区的局，彼此重叠。
		for offset := 0; offset < 3; offset++ {
			group := refsOf(tickets[(round+offset)%18], tickets[(round+offset+1)%18], tickets[(round+offset+2)%18])
			run(func() error { _, err := store.CommitAcross(ctx, group); return err })
		}
		// 单分区成局与取消。
		queue := partitions[round%len(partitions)]
		local := byPartition(queue)
		ids := []string{local[round%6].ID, local[(round+1)%6].ID, local[(round+2)%6].ID}
		run(func() error { _, err := store.Commit(ctx, queue, ids); return err })
		victim := local[(round+3)%6]
		run(func() error { _, err := store.Cancel(ctx, victim.Queue, victim.ID, victim.Subject); return err })
	}
	wait.Wait()

	matched := map[string]string{}
	for _, queue := range partitions {
		current, _, err := state.Get(ctx, queue.Key())
		if err != nil {
			t.Fatal(err)
		}
		for id, ticket := range current.Value.Tickets {
			if ticket.State == TicketMatched {
				matched[id] = ticket.MatchID
			}
		}
		for matchID, match := range current.Value.Matches {
			for i, ticketID := range match.TicketIDs {
				home := match.Queue
				if match.TicketPartitions != nil {
					home = tiered(match.TicketPartitions[i])
				}
				other, _, _ := state.Get(ctx, home.Key())
				ticket, ok := other.Value.Tickets[ticketID]
				if !ok || ticket.State != TicketMatched || ticket.MatchID != matchID {
					t.Fatalf("match %s (seen in %s) lists ticket %s in %s, which is %+v (present=%v)", matchID, queue.Partition, ticketID, home.Partition, ticket, ok)
				}
				if _, ok := other.Value.Matches[matchID]; !ok {
					t.Fatalf("match %s is missing from partition %s, where its ticket %s lives", matchID, home.Partition, ticketID)
				}
			}
		}
	}
	if len(matched) == 0 {
		t.Fatal("no match formed; the race proved nothing")
	}
}
