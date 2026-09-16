package match

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/versionstore"

	"github.com/tjbdwanghaibo/roost-core/servicemetrics"
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
	if conflicts != committers-1 {
		t.Fatalf("%d committers were refused, want %d", conflicts, committers-1)
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
	if got := sink.Count("depth:queue." + ranked().Key()); got != 2 {
		t.Fatalf("queue depth reported %d, want 2; %s", got, sink.Events())
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
