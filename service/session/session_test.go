package session

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

// recordingReleaser counts releases per resource. Per resource rather than in
// total, because "released twice" is the defect and a total hides it behind a
// second resource.
type recordingReleaser struct {
	mu       sync.Mutex
	releases map[string]int
	attempts map[string]int
	failFor  map[string]int
}

func newReleaser() *recordingReleaser {
	return &recordingReleaser{releases: map[string]int{}, failFor: map[string]int{}, attempts: map[string]int{}}
}

// attempted counts CALLS, not successes. It exists because a mutation that
// called the Releaser and ignored its error was invisible to a test that only
// counted successes: a permanently failing releaser increments nothing either
// way.
func (r *recordingReleaser) attempted(kind, id string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.attempts[kind+":"+id]
}

func (r *recordingReleaser) Release(_ context.Context, _ Run, resource Resource) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := resource.Kind + ":" + resource.ID
	r.attempts[key]++
	if remaining := r.failFor[key]; remaining > 0 {
		r.failFor[key] = remaining - 1
		return fmt.Errorf("scene manager is down")
	}
	r.releases[key]++
	return nil
}

func (r *recordingReleaser) count(kind, id string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.releases[kind+":"+id]
}

func (r *recordingReleaser) total() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	sum := 0
	for _, count := range r.releases {
		sum += count
	}
	return sum
}

type harness struct {
	service  *Service
	clock    *clock
	releaser *recordingReleaser
	runs     RunStore
	claims   ClaimStore
	metrics  *servicemetrics.Recorder
}

func ids(prefix string) func() (string, error) {
	var mu sync.Mutex
	next := 0
	return func() (string, error) {
		mu.Lock()
		defer mu.Unlock()
		next++
		return fmt.Sprintf("%s-%d", prefix, next), nil
	}
}

func newHarness(t *testing.T, mutate ...func(*Config)) *harness {
	t.Helper()
	c := &clock{now: time.Unix(1_700_000_000, 0)}
	h := &harness{
		clock:    c,
		releaser: newReleaser(),
		runs:     versionstore.NewMemoryStore[string, Run](),
		claims:   versionstore.NewMemoryStore[int64, Claim](),
		metrics:  servicemetrics.NewRecorder(),
	}
	cfg := Config{
		Runs:     h.runs,
		Claims:   h.claims,
		Requests: versionstore.NewMemoryStore[string, LedgerEntry](),
		Release:  h.releaser,
		TTL:      10 * time.Minute,
		NewRunID: ids("run"),
		Now:      c.Now,
		Metrics:  h.metrics,
	}
	for _, m := range mutate {
		m(&cfg)
	}
	service, err := New(cfg)
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	h.service = service
	return h
}

func enterReq(requestID string) EnterRequest {
	return EnterRequest{Kind: "dungeon-7", RequestID: requestID}
}

func mustEnter(t *testing.T, h *harness, ownerID int64, requestID string) Run {
	t.Helper()
	run, err := h.service.Enter(context.Background(), ownerID, enterReq(requestID))
	if err != nil {
		t.Fatalf("enter %q: %v", requestID, err)
	}
	return run
}

func scene(id string) Resource { return Resource{Kind: "scene", ID: id} }

// --- defect 1: idempotency failed open ---

// The confirmed defect: the dedupe key was built from
// (owner, kind, requestID) and returned "" when the request id was empty, and
// an empty key made the lookup MISS rather than fail. So a caller that omitted
// the id could enter repeatedly, allocating a new run and a new scene each
// time, with no capacity accounting anywhere.
//
// Here the key is required, so the vulnerable call is refused.
func TestAnEnterWithoutAnIdempotencyKeyIsRefused(t *testing.T) {
	h := newHarness(t)
	req := enterReq("")
	if _, err := h.service.Enter(context.Background(), 1, req); !errors.Is(err, ErrRequestInvalid) {
		t.Fatalf("a keyless enter produced %v, want ErrRequestInvalid; a missing key that "+
			"makes the dedupe lookup miss is idempotency failing open", err)
	}
}

// A retried enter returns the run it already produced, and allocates nothing.
func TestEnterIsIdempotentPerRequestID(t *testing.T) {
	h := newHarness(t)
	first := mustEnter(t, h, 1, "req-1")
	for attempt := 0; attempt < 4; attempt++ {
		again := mustEnter(t, h, 1, "req-1")
		if again.ID != first.ID {
			t.Fatalf("attempt %d produced a second run %s (first %s)", attempt, again.ID, first.ID)
		}
	}
	if got := h.metrics.Count("replayed:enter"); got != 4 {
		t.Fatalf("four retried enters reported %d replays; %s", got, h.metrics.Events())
	}
}

// One live run per owner. A caller sending a FRESH request id each time is
// exactly what the replaced implementation allowed, and it is what allocated
// unbounded scenes.
func TestAFreshRequestIDDoesNotBuyASecondRun(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	first := mustEnter(t, h, 1, "req-1")

	for attempt := 0; attempt < 5; attempt++ {
		_, err := h.service.Enter(ctx, 1, enterReq(fmt.Sprintf("req-fresh-%d", attempt)))
		if !errors.Is(err, ErrAlreadyRunning) {
			t.Fatalf("attempt %d with a fresh request id produced %v, want ErrAlreadyRunning; "+
				"a new key per retry is how one player allocated unbounded runs", attempt, err)
		}
	}
	current, found, err := h.service.Current(ctx, 1)
	if err != nil || !found {
		t.Fatalf("the owner's live run is not reachable: found=%v err=%v", found, err)
	}
	if current.ID != first.ID {
		t.Fatalf("the owner's live run is %s, want %s", current.ID, first.ID)
	}
	if got := h.metrics.Count("refused:enter:already_running"); got != 5 {
		t.Fatalf("five refused enters reported %d refusals; %s", got, h.metrics.Events())
	}
}

// An owner racing itself gets one run. This is why the claim is insert-only
// versioned state and not a directory reservation: a directory is idempotent
// for the same owner, so every racer would share one claim and all of them
// would proceed.
func TestAnOwnerRacingItselfGetsOneRun(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	const racers = 12
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		got  = map[string]bool{}
		lost int
	)
	wg.Add(racers)
	for i := 0; i < racers; i++ {
		go func(i int) {
			defer wg.Done()
			run, err := h.service.Enter(ctx, 1, enterReq(fmt.Sprintf("req-%d", i)))
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				lost++
				return
			}
			got[run.ID] = true
		}(i)
	}
	wg.Wait()

	if len(got) != 1 {
		t.Fatalf("%d racing enters produced %d distinct runs, want 1", racers, len(got))
	}
	if lost != racers-1 {
		t.Fatalf("%d racers were refused, want %d", lost, racers-1)
	}
}

// One idempotency key must not be answerable for two owners.
func TestOneRequestIDCannotServeTwoOwners(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	mustEnter(t, h, 1, "req-1")
	if _, err := h.service.Enter(ctx, 2, enterReq("req-1")); !errors.Is(err, ErrRequestInvalid) {
		t.Fatalf("owner 2 reused owner 1's request id and got %v, want ErrRequestInvalid", err)
	}
	if got := h.metrics.Count("refused:enter:request_owner_mismatch"); got != 1 {
		t.Fatalf("the refusal reported %d times; %s", got, h.metrics.Events())
	}
}

// A run that is over does not burn its owner's ability to enter. In the
// replaced implementation nothing indexed a run by its owner, so a leaked run
// was unreachable and its scene was never destroyed; here the claim names it,
// which is what lets a later enter resolve it.
func TestALapsedRunDoesNotBurnTheOwner(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	first := mustEnter(t, h, 1, "req-1")
	if _, err := h.service.Attach(ctx, 1, first.ID, scene("scene-1")); err != nil {
		t.Fatal(err)
	}

	// The client crashed: no finish, no leave. Wait out the deadline.
	h.clock.advance(time.Hour)

	second, err := h.service.Enter(ctx, 1, enterReq("req-2"))
	if err != nil {
		t.Fatalf("an owner whose run lapsed could not enter again: %v", err)
	}
	if second.ID == first.ID {
		t.Fatal("the second enter returned the lapsed run")
	}
	// And the lapsed run's resources were released as part of resolving it.
	if got := h.releaser.count("scene", "scene-1"); got != 1 {
		t.Fatalf("the lapsed run's scene was released %d times, want 1; a run that can never "+
			"be left is a scene that is never destroyed", got)
	}
	lapsed, _, err := h.service.Get(ctx, 1, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if lapsed.State != StateExpired {
		t.Fatalf("the lapsed run is %s, want expired", lapsed.State)
	}
}

// --- defect 2: compensation was asymmetric ---

// The confirmed defect: the first allocation step's failure path released what
// it had taken; the second step's failure path just returned, leaking both.
// Here the run records its resources, so release walks a list rather than
// being a hand-written unwind per failure branch.
func TestEveryAttachedResourceIsReleasedExactlyOnce(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	run := mustEnter(t, h, 1, "req-1")
	for _, resource := range []Resource{
		{Kind: "scene", ID: "scene-1"},
		{Kind: "replica", ID: "replica-1"},
		{Kind: "shard", ID: "shard-1"},
	} {
		if _, err := h.service.Attach(ctx, 1, run.ID, resource); err != nil {
			t.Fatalf("attach %s: %v", resource.ID, err)
		}
	}

	if _, err := h.service.Finish(ctx, 1, run.ID, StateSucceeded, "cleared"); err != nil {
		t.Fatal(err)
	}
	for _, resource := range []Resource{
		{Kind: "scene", ID: "scene-1"},
		{Kind: "replica", ID: "replica-1"},
		{Kind: "shard", ID: "shard-1"},
	} {
		if got := h.releaser.count(resource.Kind, resource.ID); got != 1 {
			t.Fatalf("%s %s was released %d times, want 1", resource.Kind, resource.ID, got)
		}
	}

	// A retried finish releases nothing again.
	for attempt := 0; attempt < 3; attempt++ {
		if _, err := h.service.Finish(ctx, 1, run.ID, StateSucceeded, "cleared"); err != nil {
			t.Fatalf("retried finish %d: %v", attempt, err)
		}
	}
	if got := h.releaser.total(); got != 3 {
		t.Fatalf("three retried finishes brought the release total to %d, want 3", got)
	}
}

// A release that fails leaves the resource PENDING, so a retry picks it up.
// Dropping it is how the replaced implementation leaked a replica permanently.
func TestAFailedReleaseLeavesTheResourcePendingForRetry(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	run := mustEnter(t, h, 1, "req-1")
	if _, err := h.service.Attach(ctx, 1, run.ID, scene("scene-1")); err != nil {
		t.Fatal(err)
	}
	h.releaser.failFor["scene:scene-1"] = 1

	finished, err := h.service.Finish(ctx, 1, run.ID, StateSucceeded, "cleared")
	if err == nil {
		t.Fatal("a failed release reported success")
	}
	// The run is terminal — the outcome is not lost — and the resource is
	// still pending, so nothing is leaked silently.
	if !finished.State.Terminal() {
		t.Fatalf("the run is %s after a failed release, want terminal", finished.State)
	}
	if len(finished.Pending()) != 1 {
		t.Fatalf("the run has %d pending resources after a failed release, want 1", len(finished.Pending()))
	}
	if got := h.metrics.Count("dropped:resource.release_failed"); got != 1 {
		t.Fatalf("a failed release reported %d drops; %s", got, h.metrics.Events())
	}

	// The retry frees it, exactly once.
	retried, err := h.service.Finish(ctx, 1, run.ID, StateSucceeded, "cleared")
	if err != nil {
		t.Fatalf("the retried finish failed: %v", err)
	}
	if len(retried.Pending()) != 0 {
		t.Fatalf("the retry left %d pending resources", len(retried.Pending()))
	}
	if got := h.releaser.count("scene", "scene-1"); got != 1 {
		t.Fatalf("the scene was released %d times across the failure and the retry, want 1", got)
	}
}

// A retried attach of the same resource is a no-op, so a caller retry after a
// lost response does not allocate a second one.
func TestARetriedAttachOfTheSameResourceIsANoOp(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	run := mustEnter(t, h, 1, "req-1")
	for attempt := 0; attempt < 4; attempt++ {
		updated, err := h.service.Attach(ctx, 1, run.ID, scene("scene-1"))
		if err != nil {
			t.Fatalf("attach %d: %v", attempt, err)
		}
		if len(updated.Resources) != 1 {
			t.Fatalf("attach %d left the run holding %d resources, want 1", attempt, len(updated.Resources))
		}
	}
}

// Attaching a DIFFERENT resource of a kind the run already holds is refused,
// because it would be an allocation nothing is tracking.
func TestAttachingASecondResourceOfOneKindIsRefused(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	run := mustEnter(t, h, 1, "req-1")
	if _, err := h.service.Attach(ctx, 1, run.ID, scene("scene-1")); err != nil {
		t.Fatal(err)
	}
	if _, err := h.service.Attach(ctx, 1, run.ID, scene("scene-2")); !errors.Is(err, ErrAlreadyAttached) {
		t.Fatalf("a second scene produced %v, want ErrAlreadyAttached", err)
	}
	if got := h.metrics.Count("refused:attach:already_attached"); got != 1 {
		t.Fatalf("the refusal reported %d times; %s", got, h.metrics.Events())
	}
}

// Attaching to a run that has run out of time is refused: that is how a
// resource gets allocated with nothing left to release it.
func TestAttachingToAnExpiredRunIsRefused(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	run := mustEnter(t, h, 1, "req-1")
	h.clock.advance(time.Hour)
	if _, err := h.service.Attach(ctx, 1, run.ID, scene("scene-1")); !errors.Is(err, ErrRunExpired) {
		t.Fatalf("attaching to an expired run produced %v, want ErrRunExpired", err)
	}
	if got := h.metrics.Count("refused:attach:expired"); got != 1 {
		t.Fatalf("the refusal reported %d times; %s", got, h.metrics.Events())
	}
}

// The resource list is bounded, and the bound is enforced.
func TestTheResourceListIsBounded(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	run := mustEnter(t, h, 1, "req-1")
	for i := 0; i < MaxResourceEntries; i++ {
		if _, err := h.service.Attach(ctx, 1, run.ID, Resource{
			Kind: fmt.Sprintf("kind-%d", i), ID: "x",
		}); err != nil {
			t.Fatalf("attach %d: %v", i, err)
		}
	}
	if _, err := h.service.Attach(ctx, 1, run.ID, Resource{Kind: "one-too-many", ID: "x"}); !errors.Is(err, ErrRunInvalid) {
		t.Fatalf("an over-limit attach produced %v, want ErrRunInvalid", err)
	}
}

// --- defect 3: the deadline was never compared to a clock ---

// The confirmed defect: the deadline was computed, stored, sent to clients,
// and nothing in the entire repository ever read it. Here a reader is correct
// before anything sweeps.
func TestAnExpiredRunReadsAsExpiredBeforeAnythingSweeps(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	run := mustEnter(t, h, 1, "req-1")
	h.clock.advance(time.Hour)

	read, found, err := h.service.Get(ctx, 1, run.ID)
	if err != nil || !found {
		t.Fatalf("the run is not readable: found=%v err=%v", found, err)
	}
	if read.State != StateExpired {
		t.Fatalf("a run past its deadline reads as %s, want expired; a deadline nothing "+
			"compares to a clock is a decoration", read.State)
	}
}

// A run must have a deadline. One without holds its resources forever.
func TestARunMustHaveADeadline(t *testing.T) {
	if _, err := New(Config{
		Runs: versionstore.NewMemoryStore[string, Run](), Claims: versionstore.NewMemoryStore[int64, Claim](),
		Requests: versionstore.NewMemoryStore[string, LedgerEntry](), Release: newReleaser(),
		TTL: -time.Second,
	}); err == nil {
		t.Fatal("New accepted a negative ttl")
	}
	run := Run{ID: "r", OwnerID: 1, Kind: "k", RequestID: "req", State: StateOpen}
	if err := run.Validate(); !errors.Is(err, ErrRunInvalid) {
		t.Fatalf("a run with no deadline validated: %v", err)
	}
}

// The sweep makes the deadline durable, releases resources and frees claims.
func TestSweepResolvesExpiredRunsAndFreesTheirClaims(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	owners := []int64{1, 2, 3}
	for _, ownerID := range owners {
		run := mustEnter(t, h, ownerID, fmt.Sprintf("req-%d", ownerID))
		if _, err := h.service.Attach(ctx, ownerID, run.ID, scene(fmt.Sprintf("scene-%d", ownerID))); err != nil {
			t.Fatal(err)
		}
	}
	h.clock.advance(time.Hour)

	swept, err := h.service.Sweep(ctx, owners, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(swept) != len(owners) {
		t.Fatalf("the sweep resolved %d runs, want %d", len(swept), len(owners))
	}
	for _, ownerID := range owners {
		if got := h.releaser.count("scene", fmt.Sprintf("scene-%d", ownerID)); got != 1 {
			t.Fatalf("owner %d's scene was released %d times, want 1", ownerID, got)
		}
		if _, found, err := h.service.Current(ctx, ownerID); err != nil {
			t.Fatal(err)
		} else if found {
			t.Fatalf("owner %d still holds a claim after the sweep", ownerID)
		}
		// And the owner can enter again.
		if _, err := h.service.Enter(ctx, ownerID, enterReq(fmt.Sprintf("after-%d", ownerID))); err != nil {
			t.Fatalf("owner %d could not enter after the sweep: %v", ownerID, err)
		}
	}
	if got := h.metrics.Count("dropped:run.expired"); got != len(owners) {
		t.Fatalf("the sweep reported %d expired runs, want %d; %s", got, len(owners), h.metrics.Events())
	}
}

// The sweep's limit is honoured and cannot be bypassed with a zero.
func TestSweepRespectsItsLimitAndRefusesAZeroOne(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	owners := make([]int64, 0, 10)
	for i := int64(1); i <= 10; i++ {
		owners = append(owners, i)
		mustEnter(t, h, i, fmt.Sprintf("req-%d", i))
	}
	h.clock.advance(time.Hour)

	if _, err := h.service.Sweep(ctx, owners, 0); !errors.Is(err, ErrRangeInvalid) {
		t.Fatalf("a zero limit produced %v, want ErrRangeInvalid", err)
	}
	swept, err := h.service.Sweep(ctx, owners, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(swept) != 3 {
		t.Fatalf("a sweep limited to 3 resolved %d runs", len(swept))
	}
	// A cut-short sweep must make progress on a deterministic prefix, or a
	// run at the back may never be reached.
	rest, err := h.service.Sweep(ctx, owners, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rest) != 7 {
		t.Fatalf("the second sweep resolved %d runs, want the remaining 7", len(rest))
	}
}

// A sweep must not rewrite a finished run's outcome. "The player cleared it"
// must not become "it ran out of time" because a sweep arrived later.
//
// Reaching that path takes a terminal run whose claim is STILL HELD, and the
// way that happens is the recovery case: a Finish whose resource release
// failed returns before freeing the claim, leaving a terminal run, a pending
// resource and a live claim. The first version of this test finished a run
// cleanly, which released the claim — so the sweep skipped the owner entirely
// and the test asserted nothing.
func TestSweepDoesNotRewriteAFinishedRunsOutcome(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	run := mustEnter(t, h, 1, "req-1")
	if _, err := h.service.Attach(ctx, 1, run.ID, scene("scene-1")); err != nil {
		t.Fatal(err)
	}
	h.releaser.failFor["scene:scene-1"] = 1

	if _, err := h.service.Finish(ctx, 1, run.ID, StateSucceeded, "cleared"); err == nil {
		t.Fatal("the failing release reported success")
	}
	// The state this test needs: terminal run, pending resource, claim held.
	if _, found, err := h.claims.Get(ctx, 1); err != nil {
		t.Fatal(err)
	} else if !found {
		t.Fatal("the claim was released despite the failed release, so the sweep will skip this owner")
	}

	h.clock.advance(time.Hour)
	swept, err := h.service.Sweep(ctx, []int64{1}, 10)
	if err != nil {
		t.Fatalf("the sweep failed: %v", err)
	}
	if len(swept) != 1 {
		t.Fatalf("the sweep resolved %d runs, want 1", len(swept))
	}

	after, found, err := h.service.Get(ctx, 1, run.ID)
	if err != nil || !found {
		t.Fatalf("the run vanished: found=%v err=%v", found, err)
	}
	if after.State != StateSucceeded {
		t.Fatalf("a finished run became %s after a sweep, want succeeded; a sweep that "+
			"rewrites outcomes turns every cleared run into a timeout", after.State)
	}
	if after.Outcome != "cleared" {
		t.Fatalf("the outcome became %q after a sweep, want \"cleared\"", after.Outcome)
	}
	// And the sweep did the work it exists for: the resource is freed and the
	// owner can enter again.
	if len(after.Pending()) != 0 {
		t.Fatalf("the sweep left %d pending resources", len(after.Pending()))
	}
	if got := h.releaser.count("scene", "scene-1"); got != 1 {
		t.Fatalf("the scene was released %d times across the failure and the sweep, want 1", got)
	}
	if _, err := h.service.Enter(ctx, 1, enterReq("after-sweep")); err != nil {
		t.Fatalf("the owner could not enter after the sweep: %v", err)
	}
}

// The same recovery without a sweep: a later Enter resolves the leftover
// state, so an owner is never stuck waiting for a background job.
func TestALaterEnterRecoversFromAFailedRelease(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	run := mustEnter(t, h, 1, "req-1")
	if _, err := h.service.Attach(ctx, 1, run.ID, scene("scene-1")); err != nil {
		t.Fatal(err)
	}
	h.releaser.failFor["scene:scene-1"] = 1
	if _, err := h.service.Finish(ctx, 1, run.ID, StateSucceeded, "cleared"); err == nil {
		t.Fatal("the failing release reported success")
	}

	next, err := h.service.Enter(ctx, 1, enterReq("req-2"))
	if err != nil {
		t.Fatalf("the owner could not enter after a failed release: %v", err)
	}
	if next.ID == run.ID {
		t.Fatal("the second enter returned the finished run")
	}
	if got := h.releaser.count("scene", "scene-1"); got != 1 {
		t.Fatalf("the leftover scene was released %d times, want 1", got)
	}
	// The finished run's outcome survived the recovery.
	old, _, err := h.service.Get(ctx, 1, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if old.State != StateSucceeded || old.Outcome != "cleared" {
		t.Fatalf("the recovered run is %s/%q, want succeeded/\"cleared\"", old.State, old.Outcome)
	}
}

// Expired is a distinct terminal state from abandoned. "The player gave up"
// and "the run ran out of time" call for different rewards and different
// support answers, and a service that collapses them cannot say which
// happened.
func TestExpiredIsDistinctFromAbandoned(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	left := mustEnter(t, h, 1, "req-1")
	if _, err := h.service.Leave(ctx, 1, left.ID, "player quit"); err != nil {
		t.Fatal(err)
	}
	lapsed := mustEnter(t, h, 2, "req-2")
	h.clock.advance(time.Hour)
	if _, err := h.service.Sweep(ctx, []int64{2}, 10); err != nil {
		t.Fatal(err)
	}

	leftRun, _, err := h.service.Get(ctx, 1, left.ID)
	if err != nil {
		t.Fatal(err)
	}
	lapsedRun, _, err := h.service.Get(ctx, 2, lapsed.ID)
	if err != nil {
		t.Fatal(err)
	}
	if leftRun.State != StateAbandoned {
		t.Fatalf("a run the owner left is %s, want abandoned", leftRun.State)
	}
	if lapsedRun.State != StateExpired {
		t.Fatalf("a run that ran out of time is %s, want expired", lapsedRun.State)
	}
	if leftRun.State == lapsedRun.State {
		t.Fatal("leaving and timing out produced the same state, so an operator cannot tell them apart")
	}
}

// Finishing a run whose deadline already passed records EXPIRED, not the
// outcome the caller asked for: the run was over before the call arrived, and
// recording the caller's outcome would credit a result it did not earn.
func TestFinishingAnExpiredRunDoesNotCreditTheOutcome(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	run := mustEnter(t, h, 1, "req-1")
	h.clock.advance(time.Hour)

	finished, err := h.service.Finish(ctx, 1, run.ID, StateSucceeded, "cleared")
	if !errors.Is(err, ErrRunExpired) {
		t.Fatalf("finishing an expired run produced %v, want ErrRunExpired", err)
	}
	if finished.State != StateExpired {
		t.Fatalf("the run is %s, want expired", finished.State)
	}
	if finished.Outcome == "cleared" {
		t.Fatal("an expired run was credited with the outcome the caller claimed")
	}
}

// --- authority is checked on every operation ---

// The confirmed defect: ownership was checked on one of four entry points,
// and the run ids were a per-process counter, so another owner's run was
// trivially nameable.
func TestEveryOperationChecksOwnership(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	run := mustEnter(t, h, 1, "req-1")

	if _, _, err := h.service.Get(ctx, 2, run.ID); !errors.Is(err, ErrNotOwner) {
		t.Fatalf("Get by a foreign owner produced %v, want ErrNotOwner", err)
	}
	if _, err := h.service.Attach(ctx, 2, run.ID, scene("scene-1")); !errors.Is(err, ErrNotOwner) {
		t.Fatalf("Attach by a foreign owner produced %v, want ErrNotOwner", err)
	}
	if _, err := h.service.Finish(ctx, 2, run.ID, StateSucceeded, "stolen"); !errors.Is(err, ErrNotOwner) {
		t.Fatalf("Finish by a foreign owner produced %v, want ErrNotOwner", err)
	}
	if _, err := h.service.Leave(ctx, 2, run.ID, "stolen"); !errors.Is(err, ErrNotOwner) {
		t.Fatalf("Leave by a foreign owner produced %v, want ErrNotOwner", err)
	}
	// And nothing the foreign owner did touched the run.
	after, found, err := h.service.Get(ctx, 1, run.ID)
	if err != nil || !found {
		t.Fatal("the run vanished")
	}
	if after.State != StateOpen {
		t.Fatalf("the run is %s after four foreign operations, want open", after.State)
	}
	if len(after.Resources) != 0 {
		t.Fatalf("a foreign attach added %d resources", len(after.Resources))
	}
	for _, event := range []string{
		"refused:get:not_owner", "refused:attach:not_owner", "refused:finish:not_owner",
	} {
		if h.metrics.Count(event) == 0 {
			t.Fatalf("%s was not reported; %s", event, h.metrics.Events())
		}
	}
}

// A caller may not choose expired as an outcome: expiry is something the
// clock decides, and letting a caller claim it would let a client convert a
// cleared run into a timeout.
func TestACallerCannotChooseTheExpiredOutcome(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	run := mustEnter(t, h, 1, "req-1")
	for _, state := range []State{StateExpired, StateOpen, State("whatever")} {
		if _, err := h.service.Finish(ctx, 1, run.ID, state, "x"); !errors.Is(err, ErrRunInvalid) {
			t.Fatalf("a caller chose outcome %q and got %v, want ErrRunInvalid", state, err)
		}
	}
}

// --- configuration must not lie ---

func TestNewRefusesAnIncompleteConfiguration(t *testing.T) {
	base := func() Config {
		return Config{
			Runs:     versionstore.NewMemoryStore[string, Run](),
			Claims:   versionstore.NewMemoryStore[int64, Claim](),
			Requests: versionstore.NewMemoryStore[string, LedgerEntry](),
			Release:  newReleaser(),
		}
	}
	cases := []struct {
		name   string
		mutate func(*Config)
	}{
		{"no run store", func(c *Config) { c.Runs = nil }},
		{"no claim store", func(c *Config) { c.Claims = nil }},
		{"no request ledger", func(c *Config) { c.Requests = nil }},
		{"no releaser", func(c *Config) { c.Release = nil }},
		{"negative ttl", func(c *Config) { c.TTL = -time.Second }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := base()
			tc.mutate(&cfg)
			if _, err := New(cfg); err == nil {
				t.Fatalf("New accepted a configuration with %s", tc.name)
			}
		})
	}
}

// A nil reporter must never change behaviour.
func TestANilReporterChangesNothing(t *testing.T) {
	h := newHarness(t, func(cfg *Config) { cfg.Metrics = nil })
	ctx := context.Background()
	run, err := h.service.Enter(ctx, 1, enterReq("req-1"))
	if err != nil {
		t.Fatalf("a service with no reporter failed to enter: %v", err)
	}
	if _, err := h.service.Attach(ctx, 1, run.ID, scene("scene-1")); err != nil {
		t.Fatal(err)
	}
	if _, err := h.service.Finish(ctx, 1, run.ID, StateSucceeded, "cleared"); err != nil {
		t.Fatal(err)
	}
}

// Terminal enumerates the terminal states rather than testing "not open",
// because "not open" makes the ZERO value terminal — and a zero Run returned
// alongside an error then reports itself as a finished run. That is not
// hypothetical: an earlier version of this package returned a zero Run on a
// release failure, and a test asserted "the run is terminal" successfully
// against it.
func TestTheZeroStateIsNotTerminal(t *testing.T) {
	if State("").Terminal() {
		t.Fatal("the zero State reports itself as terminal, so a zero Run looks like a finished run")
	}
	if State("something-new").Terminal() {
		t.Fatal("an unrecognized State reports itself as terminal")
	}
	if StateOpen.Terminal() {
		t.Fatal("StateOpen reports itself as terminal")
	}
	for _, state := range []State{StateSucceeded, StateFailed, StateAbandoned, StateExpired} {
		if !state.Terminal() {
			t.Fatalf("%s does not report itself as terminal", state)
		}
	}
}

// A corrupt claim — one naming no run — is refused rather than treated as an
// orphan. The alternative reading clears a live claim, because releaseClaim
// skips its ownership check when given no run id.
func TestAClaimWithNoRunIDIsRefusedNotTreatedAsOrphaned(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	live := mustEnter(t, h, 1, "req-1")

	// Corrupt the claim the way a bad migration or a misread would.
	if _, _, err := h.claims.Update(ctx, 1, func(current Claim, _ bool) (Claim, bool, error) {
		current.RunID = ""
		return current, true, nil
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := h.service.Enter(ctx, 1, enterReq("req-2")); !errors.Is(err, ErrConflict) {
		t.Fatalf("entering against a claim with no run id produced %v, want ErrConflict", err)
	}
	// The live run is untouched.
	run, found, err := h.service.Get(ctx, 1, live.ID)
	if err != nil || !found {
		t.Fatalf("the live run vanished: found=%v err=%v", found, err)
	}
	if run.State != StateOpen {
		t.Fatalf("the live run became %s", run.State)
	}
}
