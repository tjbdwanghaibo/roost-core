package session

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
)

// stuckRun produces a finished run holding one resource whose Releaser can
// never succeed — a replica deleted out of band, an id that was never valid.
// It is indistinguishable from a transient outage, so the sweep retries it
// forever.
func stuckRun(t *testing.T, h *harness) Run {
	t.Helper()
	ctx := context.Background()
	run := mustEnter(t, h, 1, "r1")
	if _, err := h.service.Attach(ctx, 1, run.ID, scene("scene-1")); err != nil {
		t.Fatal(err)
	}
	// Permanently failing.
	h.releaser.failFor["scene:scene-1"] = 1 << 30
	finished, err := h.service.Finish(ctx, 1, run.ID, StateSucceeded, "cleared")
	if err == nil {
		t.Fatal("Finish succeeded despite a releaser that cannot succeed")
	}
	if len(finished.Pending()) != 1 {
		t.Fatalf("the resource is not pending after a failed release: %+v", finished.Resources)
	}
	return finished
}

// The dead end: nothing in this package can mark a resource released except a
// Releaser that returns nil, so a resource that can never be released stays
// pending for the life of the record and the sweep retries it forever.
func TestAResourceTheReleaserCanNeverFreeCanBeForced(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	stuck := stuckRun(t, h)

	// Confirm the dead end: retrying gets nowhere, however many times.
	for i := 0; i < 5; i++ {
		if _, err := h.service.Finish(ctx, 1, stuck.ID, StateSucceeded, "cleared"); err == nil {
			t.Fatal("a retry released a resource the releaser rejects")
		}
	}
	if got := h.releaser.count("scene", "scene-1"); got != 0 {
		t.Fatalf("the releaser reported %d successful releases", got)
	}

	// Captured BEFORE the intervention. Reading it afterwards is what made an
	// earlier version of this test miss the mutation entirely: the extra call
	// was already inside the baseline.
	attemptsBefore := h.releaser.attempted("scene", "scene-1")
	if attemptsBefore == 0 {
		t.Fatal("setup: the releaser was never even tried, so this test cannot see a call")
	}

	forced, err := h.service.ForceRelease(ctx, stuck.ID, "scene", "scene-1",
		"replica deleted out of band, confirmed absent in the orchestrator")
	if err != nil {
		t.Fatal(err)
	}
	if after := h.releaser.attempted("scene", "scene-1"); after != attemptsBefore {
		t.Fatalf("ForceRelease called the releaser (%d -> %d attempts); the reason an operator "+
			"is here is that it cannot succeed, and calling it anyway then ignoring the error "+
			"would look like a release was attempted and reported as done",
			attemptsBefore, after)
	}
	if len(forced.Pending()) != 0 {
		t.Fatalf("the resource is still pending after a forced release: %+v", forced.Resources)
	}
	if forced.ForcedReleases != 1 || forced.AdminActionAtUnix == 0 {
		t.Fatalf("the intervention was not recorded: %+v", forced)
	}
	if !strings.Contains(forced.AdminNote, "confirmed absent") {
		t.Fatalf("the operator note was not recorded: %q", forced.AdminNote)
	}

	// The Releaser was NOT called — that is the point. Calling it and ignoring
	// the error would look like a release was attempted and reported as done.
	//
	// Counted as ATTEMPTS rather than successes: a permanently failing
	// releaser increments no success counter, so a mutation that called it and
	// swallowed the error was invisible to a test that counted only successes.
	// And the refused path must not call it either.
	refusedBefore := h.releaser.attempted("scene", "scene-1")
	if _, err := h.service.ForceRelease(ctx, stuck.ID, "scene", "scene-1", "again"); err == nil {
		t.Fatal("forcing an already-forced resource succeeded")
	}
	if after := h.releaser.attempted("scene", "scene-1"); after != refusedBefore {
		t.Fatalf("a refused ForceRelease still called the releaser (%d -> %d)", refusedBefore, after)
	}
	if got := h.releaser.count("scene", "scene-1"); got != 0 {
		t.Fatalf("the releaser reported %d successful releases", got)
	}
}

// ForcedReleases accumulates across resources rather than resetting: a run
// with three forced releases is a run whose resource accounting is not
// automatic any more, and the next reader needs that figure.
func TestForcedReleasesAccumulateAcrossResources(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	run := mustEnter(t, h, 1, "r1")
	// Different KINDS: Attach refuses a second resource of a kind the run
	// already holds, which is the one-of-each rule rather than a limit worth
	// working around here.
	kinds := [][2]string{{"scene", "scene-1"}, {"instance", "inst-1"}, {"shard", "shard-1"}}
	for _, pair := range kinds {
		if _, err := h.service.Attach(ctx, 1, run.ID, Resource{Kind: pair[0], ID: pair[1]}); err != nil {
			t.Fatal(err)
		}
		h.releaser.failFor[pair[0]+":"+pair[1]] = 1 << 30
	}
	if _, err := h.service.Finish(ctx, 1, run.ID, StateSucceeded, "cleared"); err == nil {
		t.Fatal("Finish succeeded despite three unreleasable resources")
	}
	for index, pair := range kinds {
		forced, err := h.service.ForceRelease(ctx, run.ID, pair[0], pair[1], "gone")
		if err != nil {
			t.Fatal(err)
		}
		if want := int32(index + 1); forced.ForcedReleases != want {
			t.Fatalf("after forcing %d resources ForcedReleases is %d, want %d",
				want, forced.ForcedReleases, want)
		}
	}
}

// A forced release is recorded AS forced, on the resource itself.
//
// "Handed back" and "declared gone by a human" are different facts. A reader
// that summed them would report resources as reclaimed that nothing reclaimed,
// which is the same class of mistake as counting a refunded order as
// delivered.
func TestAForcedReleaseIsDistinguishableFromARealOne(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	// A real release, for contrast.
	real := mustEnter(t, h, 1, "r1")
	if _, err := h.service.Attach(ctx, 1, real.ID, scene("scene-real")); err != nil {
		t.Fatal(err)
	}
	realFinished, err := h.service.Finish(ctx, 1, real.ID, StateSucceeded, "cleared")
	if err != nil {
		t.Fatal(err)
	}
	for _, resource := range realFinished.Resources {
		if resource.ForcedRelease {
			t.Fatalf("a real release is marked forced: %+v", resource)
		}
	}
	if realFinished.ForcedReleases != 0 {
		t.Fatalf("a run with no forced releases reports %d", realFinished.ForcedReleases)
	}

	// And a forced one.
	h2 := newHarness(t)
	stuck := stuckRun(t, h2)
	forced, err := h2.service.ForceRelease(ctx, stuck.ID, "scene", "scene-1", "gone")
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, resource := range forced.Resources {
		if resource.Kind == "scene" && resource.ID == "scene-1" {
			found = true
			if !resource.ForcedRelease {
				t.Fatal("a forced release is not marked forced, so it is indistinguishable " +
					"from a resource something actually handed back")
			}
			if resource.ReleasedAtUnix == 0 {
				t.Fatal("the forced release has no timestamp")
			}
		}
	}
	if !found {
		t.Fatal("the resource is missing from the run")
	}
}

// A resource that was already released is refused rather than re-marked.
//
// Re-marking would overwrite the timestamp of a real release with a forced
// one, which loses both facts: when it was actually handed back, and that it
// ever was.
func TestForcingAnAlreadyReleasedResourceIsRefused(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	run := mustEnter(t, h, 1, "r1")
	if _, err := h.service.Attach(ctx, 1, run.ID, scene("scene-1")); err != nil {
		t.Fatal(err)
	}
	finished, err := h.service.Finish(ctx, 1, run.ID, StateSucceeded, "cleared")
	if err != nil {
		t.Fatal(err)
	}
	realReleaseAt := finished.Resources[0].ReleasedAtUnix
	if realReleaseAt == 0 {
		t.Fatal("setup: the resource was not released")
	}
	h.clock.advance(3600)

	_, err = h.service.ForceRelease(ctx, run.ID, "scene", "scene-1", "trying anyway")
	if !errors.Is(err, ErrNotResolvable) {
		t.Fatalf("forcing an already-released resource returned %v", err)
	}
	after, _, err := h.service.Get(ctx, 1, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Resources[0].ReleasedAtUnix != realReleaseAt {
		t.Fatalf("the real release timestamp was overwritten (%d -> %d)",
			realReleaseAt, after.Resources[0].ReleasedAtUnix)
	}
	if after.Resources[0].ForcedRelease {
		t.Fatal("a real release was re-marked as forced")
	}
	if after.ForcedReleases != 0 {
		t.Fatalf("a refused intervention incremented ForcedReleases to %d", after.ForcedReleases)
	}
}

// A resource the run does not hold, and a run that does not exist, are refused
// with different errors, because the fixes differ.
func TestForcingSomethingThatIsNotThereIsRefused(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	stuck := stuckRun(t, h)

	if _, err := h.service.ForceRelease(ctx, stuck.ID, "scene", "scene-nope", "gone"); !errors.Is(err, ErrNotResolvable) {
		t.Fatalf("forcing a resource the run does not hold returned %v", err)
	}
	if _, err := h.service.ForceRelease(ctx, "run-does-not-exist", "scene", "scene-1", "gone"); !errors.Is(err, ErrRunMissing) {
		t.Fatalf("forcing on a missing run returned %v, want ErrRunMissing", err)
	}
	// A blank kind or id is a caller mistake, not a missing resource.
	for _, bad := range [][2]string{{"", "scene-1"}, {"scene", ""}, {" ", " "}} {
		if _, err := h.service.ForceRelease(ctx, stuck.ID, bad[0], bad[1], "gone"); !errors.Is(err, ErrRequestInvalid) {
			t.Fatalf("kind=%q id=%q returned %v, want ErrRequestInvalid", bad[0], bad[1], err)
		}
	}
}

// Forcing a release asserts something about the outside world that this
// service cannot check, so the claim has to be recorded.
func TestForceReleaseRequiresANote(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	stuck := stuckRun(t, h)
	for _, blank := range []string{"", "   ", "\t\n"} {
		if _, err := h.service.ForceRelease(ctx, stuck.ID, "scene", "scene-1", blank); !errors.Is(err, ErrAdminNoteRequired) {
			t.Fatalf("a blank note %q returned %v", blank, err)
		}
	}
	if _, err := h.service.ForceRelease(ctx, stuck.ID, "scene", "scene-1", strings.Repeat("x", MaxAdminNoteBytes+1)); !errors.Is(err, ErrAdminNoteRequired) {
		t.Fatal("an oversized note was accepted")
	}
	after, _, err := h.service.Get(ctx, 1, stuck.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Pending()) != 1 || after.ForcedReleases != 0 {
		t.Fatalf("a refused intervention changed the run: %+v", after)
	}
}

// Two operators, or one clicking twice, must not both force: the second would
// overwrite the first's timestamp and double-count ForcedReleases.
func TestConcurrentForcedReleasesCountOnce(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	stuck := stuckRun(t, h)

	const racers = 8
	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		succeeded int
	)
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := h.service.ForceRelease(ctx, stuck.ID, "scene", "scene-1", "concurrent"); err == nil {
				mu.Lock()
				succeeded++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if succeeded != 1 {
		t.Fatalf("%d of %d concurrent forced releases succeeded, want exactly 1", succeeded, racers)
	}
	after, _, err := h.service.Get(ctx, 1, stuck.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.ForcedReleases != 1 {
		t.Fatalf("ForcedReleases is %d after %d concurrent attempts, want 1",
			after.ForcedReleases, racers)
	}
}

// A stuck resource BLOCKS THE OWNER, and ForceRelease unblocks them.
//
// This is the consequence that matters most, and it is not visible from
// Run.Live — which is why it was got backwards while planning this step. The
// path is:
//
//	Enter -> the owner's claim is held -> resolveClaim -> the run is not live
//	      -> resolve() releases its resources -> the release fails
//	      -> resolveClaim returns the error -> the claim is never freed
//
// The claim is freed only after cleanup succeeds — correct, so the claim never
// outlives the cleanup — and the price is that a release which can NEVER
// succeed is a player who can never enter again.
func TestAStuckResourceBlocksTheOwnerUntilForced(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	stuck := stuckRun(t, h)

	// Blocked, and not by chance: repeatedly.
	for i := 0; i < 3; i++ {
		if _, err := h.service.Enter(ctx, 1, enterReq("r2")); err == nil {
			t.Fatal("the owner entered a new run while a resource cannot be released; the " +
				"claim was freed before the cleanup succeeded")
		}
	}

	if _, err := h.service.ForceRelease(ctx, stuck.ID, "scene", "scene-1",
		"replica gone, confirmed in the orchestrator"); err != nil {
		t.Fatal(err)
	}

	// And now the player can play again. This is what the operator surface is
	// FOR — the leaked replica is the smaller half.
	again, err := h.service.Enter(ctx, 1, enterReq("r2"))
	if err != nil {
		t.Fatalf("the owner is still blocked after the resource was forced: %v", err)
	}
	if again.ID == stuck.ID {
		t.Fatal("Enter returned the stuck run rather than a new one")
	}
}

// Admin is deliberately not on the bus: the capability published to other
// processes (the generated wrapper) must not satisfy the operator surface.
func TestTheOperatorSurfaceIsNotOnTheSessionInterface(t *testing.T) {
	var asSession any = Capability(&Service{})
	if _, ok := asSession.(Admin); ok {
		t.Fatal("the capability published to other processes satisfies Admin; a caller over the " +
			"bus could declare a resource gone, which is a claim about the outside world that " +
			"this service cannot verify")
	}
}
