package rank

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/tjbdwanghaibo/roost-kit/service/servicemetrics"
	"time"
)

func newStore(t *testing.T) (*RedisStore, *fakeRedis) {
	t.Helper()
	fake := newFakeRedis()
	now := time.Unix(1_700_000_000, 0)
	store, err := NewRedisStore(fake, RedisConfig{Prefix: "test:rank", Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	return store, fake
}

func arena() Board { return Board{ID: "arena", Scope: ScopeServer, ScopeID: 1, Season: 3} }

func submit(t *testing.T, s *RedisStore, owner, value int64, mode UpdateMode, requestID string) Entry {
	t.Helper()
	entry, err := s.Submit(context.Background(), arena(), Score{OwnerID: owner, Value: value}, mode, requestID)
	if err != nil {
		t.Fatalf("submit owner=%d value=%d mode=%s: %v", owner, value, mode, err)
	}
	return entry
}

// A page's ranks and scores come from one read, so they cannot disagree — the
// defect where an entry appeared with a new score at its old rank.
func TestPageRanksMatchTheirScores(t *testing.T) {
	store, _ := newStore(t)
	ctx := context.Background()

	for owner, value := range map[int64]int64{1: 50, 2: 300, 3: 200, 4: 100} {
		submit(t, store, owner, value, UpdateSet, "")
	}
	page, err := store.Page(ctx, arena(), 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 4 {
		t.Fatalf("total = %d, want 4", page.Total)
	}
	wantOwners := []int64{2, 3, 4, 1}
	if len(page.Entries) != len(wantOwners) {
		t.Fatalf("got %d entries, want %d", len(page.Entries), len(wantOwners))
	}
	previous := int64(1 << 62)
	for index, entry := range page.Entries {
		if entry.Score.OwnerID != wantOwners[index] {
			t.Fatalf("position %d: owner %d, want %d", index, entry.Score.OwnerID, wantOwners[index])
		}
		if entry.Rank != int64(index+1) {
			t.Fatalf("position %d: rank %d", index, entry.Rank)
		}
		if entry.Score.Value > previous {
			t.Fatalf("position %d: value %d is higher than the entry above (%d)", index, entry.Score.Value, previous)
		}
		previous = entry.Score.Value
	}
}

// The page bound cannot be bypassed by leaving the limit at zero. The
// implementation this replaces turned a zero limit into a full-board read
// reachable from one client packet.
func TestPageLimitCannotBeBypassed(t *testing.T) {
	store, _ := newStore(t)
	ctx := context.Background()
	for owner := int64(1); owner <= 5; owner++ {
		submit(t, store, owner, owner*10, UpdateSet, "")
	}
	for _, limit := range []int{0, -1, MaxPageSize + 1, 1 << 20} {
		if _, err := store.Page(ctx, arena(), 0, limit); !errors.Is(err, ErrRangeInvalid) {
			t.Fatalf("limit %d returned %v, want ErrRangeInvalid", limit, err)
		}
	}
	if _, err := store.Page(ctx, arena(), -1, 10); !errors.Is(err, ErrRangeInvalid) {
		t.Fatal("a negative offset was accepted")
	}
	page, err := store.Page(ctx, arena(), 0, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Entries) != 2 {
		t.Fatalf("a limit of 2 returned %d entries", len(page.Entries))
	}
	if page.Total != 5 {
		t.Fatalf("total = %d, want the full board size 5", page.Total)
	}
}

// Accumulating submits must be replay-safe. The implementation this replaces
// added the same delta up to twenty times under redelivery, leaving the board
// permanently wrong with no repair path.
func TestAddModeIsIdempotentPerRequest(t *testing.T) {
	store, _ := newStore(t)
	ctx := context.Background()

	for attempt := 0; attempt < 5; attempt++ {
		entry, err := store.Submit(ctx, arena(), Score{OwnerID: 1, Value: 10}, UpdateAdd, "battle-42")
		if err != nil {
			t.Fatalf("attempt %d: %v", attempt, err)
		}
		if entry.Score.Value != 10 {
			t.Fatalf("attempt %d: value = %d, want 10 — the delta was applied more than once", attempt, entry.Score.Value)
		}
	}
	// A different request id is a different event and must accumulate.
	entry, err := store.Submit(ctx, arena(), Score{OwnerID: 1, Value: 10}, UpdateAdd, "battle-43")
	if err != nil {
		t.Fatal(err)
	}
	if entry.Score.Value != 20 {
		t.Fatalf("a new request id gave %d, want 20", entry.Score.Value)
	}
	// And an accumulating submit without a request id is refused rather than
	// silently unsafe.
	if _, err := store.Submit(ctx, arena(), Score{OwnerID: 1, Value: 5}, UpdateAdd, ""); !errors.Is(err, ErrRequestInvalid) {
		t.Fatalf("add without a request id returned %v, want ErrRequestInvalid", err)
	}
}

// The idempotency ring is bounded, so it cannot grow without limit — but a
// request inside the window must still be recognised.
func TestIdempotencyRingIsBoundedAndRecognisesRecentRequests(t *testing.T) {
	store, _ := newStore(t)
	ctx := context.Background()

	for i := 0; i < maxAppliedRequests; i++ {
		if _, err := store.Submit(ctx, arena(), Score{OwnerID: 1, Value: 1}, UpdateAdd, fmt.Sprintf("req-%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	entry, _, err := store.Rank(ctx, arena(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if entry.Score.Value != int64(maxAppliedRequests) {
		t.Fatalf("value = %d, want %d", entry.Score.Value, maxAppliedRequests)
	}
	// Every id still in the window is a no-op.
	for i := 0; i < maxAppliedRequests; i++ {
		if _, err := store.Submit(ctx, arena(), Score{OwnerID: 1, Value: 1}, UpdateAdd, fmt.Sprintf("req-%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	entry, _, _ = store.Rank(ctx, arena(), 1)
	if entry.Score.Value != int64(maxAppliedRequests) {
		t.Fatalf("replaying the whole window changed the value to %d", entry.Score.Value)
	}
	// The ring itself stays bounded.
	_, ring, err := store.readOwner(ctx, store.ownerKey(arena()), 1)
	if err != nil {
		t.Fatal(err)
	}
	if tokens := strings.Split(ring, ","); len(tokens) > maxAppliedRequests {
		t.Fatalf("the request ring holds %d ids, bound is %d", len(tokens), maxAppliedRequests)
	}
}

// An unset tiebreak becomes the submit time, so equal scores order by who got
// there first — not by owner id, which used to make a low id outrank a high
// one permanently.
func TestEqualScoresOrderByWhoArrivedFirst(t *testing.T) {
	fake := newFakeRedis()
	current := time.Unix(1_700_000_000, 0)
	store, err := NewRedisStore(fake, RedisConfig{Prefix: "test:rank", Now: func() time.Time { return current }})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// Owner 9 reaches 100 first, owner 2 reaches it later.
	if _, err := store.Submit(ctx, arena(), Score{OwnerID: 9, Value: 100}, UpdateSet, ""); err != nil {
		t.Fatal(err)
	}
	current = current.Add(time.Minute)
	if _, err := store.Submit(ctx, arena(), Score{OwnerID: 2, Value: 100}, UpdateSet, ""); err != nil {
		t.Fatal(err)
	}
	page, err := store.Page(ctx, arena(), 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if page.Entries[0].Score.OwnerID != 9 {
		t.Fatalf("first place is owner %d; the earlier arrival (9) must rank higher even though its id is larger",
			page.Entries[0].Score.OwnerID)
	}
}

func TestUpdateMaxKeepsTheBestAndNeverDemotes(t *testing.T) {
	store, _ := newStore(t)
	ctx := context.Background()

	submit(t, store, 1, 100, UpdateMax, "")
	entry := submit(t, store, 1, 50, UpdateMax, "")
	if entry.Score.Value != 100 {
		t.Fatalf("max mode accepted a lower value: %d", entry.Score.Value)
	}
	entry = submit(t, store, 1, 150, UpdateMax, "")
	if entry.Score.Value != 150 {
		t.Fatalf("max mode rejected a higher value: %d", entry.Score.Value)
	}
	// Resubmitting the same value must not move the owner down the board by
	// replacing an earlier tiebreak with a later one.
	before, _, _ := store.Rank(ctx, arena(), 1)
	entry = submit(t, store, 1, 150, UpdateMax, "")
	if entry.Score.Tie != before.Score.Tie {
		t.Fatalf("resubmitting an equal value changed the tiebreak from %d to %d", before.Score.Tie, entry.Score.Tie)
	}
}

func TestRemoveIsIdempotentAndClearsTheOrdering(t *testing.T) {
	store, _ := newStore(t)
	ctx := context.Background()

	submit(t, store, 1, 100, UpdateSet, "")
	submit(t, store, 2, 50, UpdateSet, "")
	if err := store.Remove(ctx, arena(), 1); err != nil {
		t.Fatal(err)
	}
	if err := store.Remove(ctx, arena(), 1); err != nil {
		t.Fatalf("a repeated Remove returned %v", err)
	}
	if _, found, _ := store.Rank(ctx, arena(), 1); found {
		t.Fatal("Rank still reports a removed owner")
	}
	page, err := store.Page(ctx, arena(), 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	// The critical part: no ordering entry survives without its owner record.
	// A leftover member is what made pages short and truncated archives.
	if page.Total != 1 || len(page.Entries) != 1 || page.Entries[0].Score.OwnerID != 2 {
		t.Fatalf("board after removal: total=%d entries=%+v", page.Total, page.Entries)
	}
	if size, _ := store.Size(ctx, arena()); size != 1 {
		t.Fatalf("size = %d, want 1", size)
	}
}

// Updating a score must replace its ordering entry, never add a second one.
func TestResubmitReplacesTheOrderingEntry(t *testing.T) {
	store, _ := newStore(t)
	ctx := context.Background()

	for _, value := range []int64{10, 20, 30, 40} {
		submit(t, store, 1, value, UpdateSet, "")
		size, err := store.Size(ctx, arena())
		if err != nil {
			t.Fatal(err)
		}
		if size != 1 {
			t.Fatalf("after submitting %d the board holds %d entries, want 1: an old ordering entry was left behind", value, size)
		}
	}
	entry, found, err := store.Rank(ctx, arena(), 1)
	if err != nil || !found {
		t.Fatalf("rank: found=%v err=%v", found, err)
	}
	if entry.Score.Value != 40 || entry.Rank != 1 {
		t.Fatalf("entry = %+v", entry)
	}
}

func TestAroundCentresOnTheOwnerAndIsBounded(t *testing.T) {
	store, _ := newStore(t)
	ctx := context.Background()
	for owner := int64(1); owner <= 20; owner++ {
		submit(t, store, owner, owner*10, UpdateSet, "")
	}
	page, err := store.Around(ctx, arena(), 10, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Entries) != 5 {
		t.Fatalf("radius 2 returned %d entries, want 5", len(page.Entries))
	}
	// Centred means the owner sits in the middle with radius entries either
	// side, ranks contiguous — not merely somewhere in the window. Owner 10
	// holds rank 11 (owners 20..11 rank above it), so the window is 9..13.
	if page.Entries[2].Score.OwnerID != 10 {
		t.Fatalf("the requested owner is not at the centre of its window: %+v", page.Entries)
	}
	for index, entry := range page.Entries {
		if want := int64(9 + index); entry.Rank != want {
			t.Fatalf("window position %d has rank %d, want %d", index, entry.Rank, want)
		}
	}
	for _, radius := range []int{0, -1, MaxPageSize} {
		if _, err := store.Around(ctx, arena(), 10, radius); !errors.Is(err, ErrRangeInvalid) {
			t.Fatalf("radius %d returned %v, want ErrRangeInvalid", radius, err)
		}
	}
	if _, err := store.Around(ctx, arena(), 999, 2); !errors.Is(err, ErrNotFound) {
		t.Fatal("Around on an absent owner did not report not-found")
	}
}

// Every business failure carries a code. The implementation this replaces
// returned an internal error for "board id is empty".
func TestInvalidInputIsRejectedWithABusinessError(t *testing.T) {
	store, _ := newStore(t)
	ctx := context.Background()
	for _, testCase := range []struct {
		label string
		board Board
		want  error
	}{
		{"empty id", Board{Scope: ScopeServer, ScopeID: 1}, ErrBoardInvalid},
		{"unknown scope", Board{ID: "a", Scope: "planet", ScopeID: 1}, ErrBoardInvalid},
		{"global with scope id", Board{ID: "a", Scope: ScopeGlobal, ScopeID: 1}, ErrBoardInvalid},
		{"scoped without id", Board{ID: "a", Scope: ScopeServer}, ErrBoardInvalid},
		{"negative season", Board{ID: "a", Scope: ScopeGlobal, Season: -1}, ErrBoardInvalid},
	} {
		if _, err := store.Submit(ctx, testCase.board, Score{OwnerID: 1, Value: 1}, UpdateSet, ""); !errors.Is(err, testCase.want) {
			t.Fatalf("%s: %v", testCase.label, err)
		}
	}
	if _, err := store.Submit(ctx, arena(), Score{OwnerID: 0, Value: 1}, UpdateSet, ""); !errors.Is(err, ErrOwnerInvalid) {
		t.Fatal("a zero owner id was accepted")
	}
	if _, err := store.Submit(ctx, arena(), Score{OwnerID: 1, Value: 1}, "sideways", ""); !errors.Is(err, ErrScoreInvalid) {
		t.Fatal("an unknown update mode was accepted")
	}
	oversized := Score{OwnerID: 1, Value: 1, Brief: make([]byte, MaxBriefBytes+1)}
	if _, err := store.Submit(ctx, arena(), oversized, UpdateSet, ""); !errors.Is(err, ErrScoreInvalid) {
		t.Fatal("an oversized brief was accepted")
	}
}

// Concurrent submits for one owner must not lose an accumulation, and must
// leave exactly one ordering entry.
func TestConcurrentAddsLoseNothing(t *testing.T) {
	store, _ := newStore(t)
	ctx := context.Background()

	const writers, per = 8, 10
	var wait sync.WaitGroup
	errs := make([]error, writers)
	for writer := 0; writer < writers; writer++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			for i := 0; i < per; i++ {
				_, err := store.Submit(ctx, arena(), Score{OwnerID: 1, Value: 1}, UpdateAdd,
					fmt.Sprintf("w%d-%d", index, i))
				if err != nil {
					errs[index] = err
					return
				}
			}
		}(writer)
	}
	wait.Wait()
	for writer, err := range errs {
		if err != nil {
			t.Fatalf("writer %d: %v", writer, err)
		}
	}
	entry, found, err := store.Rank(ctx, arena(), 1)
	if err != nil || !found {
		t.Fatalf("rank: found=%v err=%v", found, err)
	}
	if entry.Score.Value != writers*per {
		t.Fatalf("value = %d, want %d: accumulations were lost", entry.Score.Value, writers*per)
	}
	if size, _ := store.Size(ctx, arena()); size != 1 {
		t.Fatalf("board holds %d entries for one owner", size)
	}
}

func TestResetEmptiesTheBoard(t *testing.T) {
	store, _ := newStore(t)
	ctx := context.Background()
	submit(t, store, 1, 10, UpdateSet, "")
	submit(t, store, 2, 20, UpdateSet, "")
	if err := store.Reset(ctx, arena()); err != nil {
		t.Fatal(err)
	}
	if size, _ := store.Size(ctx, arena()); size != 0 {
		t.Fatalf("size after reset = %d", size)
	}
	if _, found, _ := store.Rank(ctx, arena(), 1); found {
		t.Fatal("an owner survived the reset")
	}
	// The board must be usable again, and the request ring must be gone with
	// it — otherwise a replayed request from before the reset would be
	// swallowed on the new season.
	if _, err := store.Submit(ctx, arena(), Score{OwnerID: 1, Value: 5}, UpdateAdd, "req-0"); err != nil {
		t.Fatal(err)
	}
	entry, _, _ := store.Rank(ctx, arena(), 1)
	if entry.Score.Value != 5 {
		t.Fatalf("value after reset = %d, want 5", entry.Score.Value)
	}
}

// Boards are isolated: the same owner on two boards, or two seasons of one
// board, must not interfere.
func TestBoardsAndSeasonsAreIsolated(t *testing.T) {
	store, _ := newStore(t)
	ctx := context.Background()
	seasonThree := arena()
	seasonFour := arena()
	seasonFour.Season = 4
	other := Board{ID: "battle_win", Scope: ScopeGlobal}

	if _, err := store.Submit(ctx, seasonThree, Score{OwnerID: 1, Value: 10}, UpdateSet, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Submit(ctx, seasonFour, Score{OwnerID: 1, Value: 99}, UpdateSet, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Submit(ctx, other, Score{OwnerID: 1, Value: 7}, UpdateSet, ""); err != nil {
		t.Fatal(err)
	}
	for _, testCase := range []struct {
		board Board
		want  int64
	}{{seasonThree, 10}, {seasonFour, 99}, {other, 7}} {
		entry, found, err := store.Rank(ctx, testCase.board, 1)
		if err != nil || !found {
			t.Fatalf("%s: found=%v err=%v", testCase.board, found, err)
		}
		if entry.Score.Value != testCase.want {
			t.Fatalf("%s: value = %d, want %d", testCase.board, entry.Score.Value, testCase.want)
		}
	}
}

func TestNewRedisStoreRequiresAPrefix(t *testing.T) {
	if _, err := NewRedisStore(nil, RedisConfig{Prefix: "x"}); err == nil {
		t.Fatal("a nil client was accepted")
	}
	if _, err := NewRedisStore(newFakeRedis(), RedisConfig{}); err == nil {
		t.Fatal("an empty prefix was accepted: two services could share a keyspace")
	}
	if _, err := NewRedisStore(newFakeRedis(), RedisConfig{Prefix: "   "}); err == nil {
		t.Fatal("a blank prefix was accepted")
	}
}

// A stored member the reader cannot parse must be reported, not skipped.
// Skipping it while still consuming a rank slot is what made pages short in
// the implementation this replaces — and a short page is what silently
// truncated season archives, whose loss was unrecoverable because the archive
// was the only record left after a reset.
func TestPageReportsAMalformedMemberInsteadOfSkippingIt(t *testing.T) {
	store, fake := newStore(t)
	ctx := context.Background()

	submit(t, store, 1, 300, UpdateSet, "")
	submit(t, store, 2, 100, UpdateSet, "")

	// Plant a member that cannot be decoded, as a partial write or an older
	// encoding would leave behind. It sorts between the two valid entries.
	zkey := store.boardKey(arena())
	fake.mu.Lock()
	fake.zsets[zkey]["8000000000000200:corrupt"] = true
	fake.mu.Unlock()

	_, err := store.Page(ctx, arena(), 0, 10)
	if err == nil {
		t.Fatal("Page silently skipped a member it could not decode")
	}
	if !strings.Contains(err.Error(), "position") {
		t.Fatalf("the error does not say where the bad member is: %v", err)
	}
	// Size still counts it, which is the point: a reader must not be able to
	// paper over the inconsistency by returning a shorter page.
	size, err := store.Size(ctx, arena())
	if err != nil {
		t.Fatal(err)
	}
	if size != 3 {
		t.Fatalf("size = %d, want 3", size)
	}
}

// The reporting seam has to be actually reached, or it is the kind of thing
// this repository exists to avoid: a mechanism that looks present and emits
// nothing. Each of these paths was silent in the implementation being
// replaced, which is why its submit-reported-as-failure and its truncating
// archive both survived a release.
//
// The recorder is the shared one rather than a local reimplementation: six
// near-identical hand-written doubles is six places for one to quietly stop
// asserting.
func TestSubmitAndPageReportWhatTheyDid(t *testing.T) {
	sink := servicemetrics.NewRecorder()
	fake := newFakeRedis()
	now := time.Unix(1_700_000_000, 0)
	store, err := NewRedisStore(fake, RedisConfig{
		Prefix: "test:rank", Now: func() time.Time { return now }, Metrics: sink,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	if _, err := store.Submit(ctx, arena(), Score{OwnerID: 1, Value: 10}, UpdateAdd, "req-1"); err != nil {
		t.Fatal(err)
	}
	if got := sink.Count("accepted:submit"); got != 1 {
		t.Fatalf("an accepted submit reported %d, want 1; %s", got, sink.Events())
	}

	// A replay must be distinguishable from a first submit: a rising replay
	// rate is how an operator learns the transport is redelivering.
	if _, err := store.Submit(ctx, arena(), Score{OwnerID: 1, Value: 10}, UpdateAdd, "req-1"); err != nil {
		t.Fatal(err)
	}
	if got := sink.Count("replayed:submit"); got != 1 {
		t.Fatalf("a replayed submit reported %d replays, want 1; %s", got, sink.Events())
	}
	if got := sink.Count("accepted:submit"); got != 1 {
		t.Fatalf("a replayed submit was also counted as accepted (%d); %s", got, sink.Events())
	}

	// Board size is the gauge.
	if _, err := store.Page(ctx, arena(), 0, 10); err != nil {
		t.Fatal(err)
	}
	if got := sink.Count("depth:board.arena"); got != 1 {
		t.Fatalf("board depth reported %d, want 1; %s", got, sink.Events())
	}

	// Contention is reported rather than left as an opaque internal error.
	fake.swapAlwaysLoses = true
	if _, err := store.Submit(ctx, arena(), Score{OwnerID: 2, Value: 1}, UpdateSet, ""); err == nil {
		t.Fatal("the submit succeeded despite never winning the swap")
	}
	if got := sink.Count("conflict:submit"); got != 1 {
		t.Fatalf("compare-and-swap exhaustion reported %d conflicts, want 1; %s", got, sink.Events())
	}
}

// A nil reporter must never fail an operation: every call site is
// unconditional so a report cannot be forgotten, which only works if nil is
// safe.
func TestANilReporterDoesNotAffectBehaviour(t *testing.T) {
	store, _ := newStore(t)
	ctx := context.Background()
	if _, err := store.Submit(ctx, arena(), Score{OwnerID: 1, Value: 5}, UpdateSet, ""); err != nil {
		t.Fatalf("a store with no reporter failed to submit: %v", err)
	}
	if _, err := store.Page(ctx, arena(), 0, 5); err != nil {
		t.Fatal(err)
	}
}

// Contention is a business outcome with its own code, not a server fault.
// Every other service in this module pairs CodeConflict with a coded
// sentinel; rank declared the code, then reported exhaustion through a plain
// errors.New, so the RPC envelope told the caller "server error" for the one
// failure mode the doc on ErrConflictSentinel promises is distinguishable.
func TestSubmitContentionReportsCodeConflict(t *testing.T) {
	store, fake := newStore(t)
	fake.swapAlwaysLoses = true
	_, err := store.Submit(context.Background(), arena(), Score{OwnerID: 2, Value: 1}, UpdateSet, "")
	if err == nil {
		t.Fatal("the submit succeeded despite never winning the swap")
	}
	if code, reason := Error(err); code != CodeConflict {
		t.Fatalf("exhausted compare-and-swap reports code %d (%q), want CodeConflict %d", code, reason, CodeConflict)
	}
	if !errors.Is(err, ErrConflictSentinel) {
		t.Fatal("the exported sentinel no longer matches the conflict error")
	}
}
