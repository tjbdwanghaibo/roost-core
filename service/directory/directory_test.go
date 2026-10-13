package directory

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/infra/observe/servicemetrics"
	fredis "github.com/tjbdwanghaibo/roost-core/infra/storage/redis"
	"github.com/tjbdwanghaibo/roost-core/infra/storage/redis/driver"
	"github.com/tjbdwanghaibo/roost-core/infra/storage/versionstore"
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

func newDirectory(t *testing.T) (Directory, *clock) {
	t.Helper()
	c := &clock{now: time.Unix(1_700_000_000, 0)}
	dir, err := New(versionstore.NewMemoryStore[string, Entry](), Config{
		Normalize:  NormalizeLower,
		DefaultTTL: time.Minute,
		Now:        c.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	return dir, c
}

// Uniqueness is decided on the normalized key, or "Alice" and "alice" are
// two names.
func TestReserveIsCaseAndSpaceInsensitive(t *testing.T) {
	dir, _ := newDirectory(t)
	ctx := context.Background()

	if _, err := dir.Reserve(ctx, "Alice", "player:1", 0); err != nil {
		t.Fatal(err)
	}
	for _, variant := range []string{"alice", "ALICE", "  Alice  "} {
		if _, err := dir.Reserve(ctx, variant, "player:2", 0); !errors.Is(err, ErrKeyTaken) {
			t.Fatalf("reserving %q for another owner returned %v, want ErrKeyTaken", variant, err)
		}
	}
	entry, found, err := dir.Lookup(ctx, "ALICE")
	if err != nil || !found {
		t.Fatalf("lookup: found=%v err=%v", found, err)
	}
	// The display form is preserved even though uniqueness folded it.
	if entry.Raw != "Alice" || entry.Key != "alice" {
		t.Fatalf("entry = %+v, want raw Alice key alice", entry)
	}
}

// A retried Reserve by the same owner must not consume a second reservation:
// it returns the claim already held.
func TestReserveIsIdempotentForTheSameOwner(t *testing.T) {
	dir, _ := newDirectory(t)
	ctx := context.Background()

	first, err := dir.Reserve(ctx, "alice", "player:1", 0)
	if err != nil {
		t.Fatal(err)
	}
	second, err := dir.Reserve(ctx, "alice", "player:1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if second.Token != first.Token {
		t.Fatalf("a repeat Reserve minted a new token (%s then %s)", first.Token, second.Token)
	}
}

// An abandoned reservation must free the key. A reservation with no expiry is
// how a name gets burned: reserved to an owner that never committed, and
// unreleasable by any code path.
func TestLapsedReservationFreesTheKey(t *testing.T) {
	dir, c := newDirectory(t)
	ctx := context.Background()

	if _, err := dir.Reserve(ctx, "alice", "player:1", 10*time.Second); err != nil {
		t.Fatal(err)
	}
	if _, err := dir.Reserve(ctx, "alice", "player:2", 0); !errors.Is(err, ErrKeyTaken) {
		t.Fatalf("the key was not held: %v", err)
	}
	if _, found, _ := dir.Lookup(ctx, "alice"); !found {
		t.Fatal("lookup did not see the live reservation")
	}

	c.advance(11 * time.Second)

	if _, found, err := dir.Lookup(ctx, "alice"); err != nil || found {
		t.Fatalf("a lapsed reservation still reports present: found=%v err=%v", found, err)
	}
	claim, err := dir.Reserve(ctx, "alice", "player:2", 0)
	if err != nil {
		t.Fatalf("the key was not freed by expiry: %v", err)
	}
	if claim.Owner != "player:2" {
		t.Fatalf("claim owner = %q", claim.Owner)
	}
}

// Commit requires the token. Committing a reservation someone else now holds
// must be refused, not silently overwrite it.
func TestCommitRequiresTheHeldToken(t *testing.T) {
	dir, c := newDirectory(t)
	ctx := context.Background()

	stale, err := dir.Reserve(ctx, "alice", "player:1", 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	c.advance(11 * time.Second)
	if _, err := dir.Reserve(ctx, "alice", "player:2", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := dir.Commit(ctx, stale); !errors.Is(err, ErrClaimStale) {
		t.Fatalf("committing a lapsed claim over a new owner returned %v, want ErrClaimStale", err)
	}
	entry, _, _ := dir.Lookup(ctx, "alice")
	if entry.Owner != "player:2" || entry.State != StateReserved {
		t.Fatalf("the new owner's reservation was damaged: %+v", entry)
	}
	// A forged token must not work either.
	forged := Claim{Key: "alice", Owner: "player:9", Token: "deadbeef"}
	if _, err := dir.Commit(ctx, forged); !errors.Is(err, ErrClaimStale) {
		t.Fatalf("a forged token returned %v, want ErrClaimStale", err)
	}
}

// A committed entry never expires, and a retried Commit succeeds so a client
// retry after a lost response does not fail.
func TestCommitIsPermanentAndIdempotent(t *testing.T) {
	dir, c := newDirectory(t)
	ctx := context.Background()

	claim, err := dir.Reserve(ctx, "alice", "player:1", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	entry, err := dir.Commit(ctx, claim)
	if err != nil {
		t.Fatal(err)
	}
	if entry.State != StateCommitted || entry.ExpiresAtUnix != 0 {
		t.Fatalf("committed entry = %+v", entry)
	}
	c.advance(time.Hour)
	if _, found, _ := dir.Lookup(ctx, "alice"); !found {
		t.Fatal("a committed entry expired")
	}
	again, err := dir.Commit(ctx, claim)
	if err != nil {
		t.Fatalf("a retried Commit failed: %v", err)
	}
	if again.State != StateCommitted {
		t.Fatalf("retried Commit returned %+v", again)
	}
}

// Cancel must be idempotent, and must not delete a key it no longer holds —
// that is the release-races-a-re-reservation bug.
func TestCancelIsIdempotentAndNeverTouchesAnotherOwner(t *testing.T) {
	dir, c := newDirectory(t)
	ctx := context.Background()

	claim, err := dir.Reserve(ctx, "alice", "player:1", 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := dir.Cancel(ctx, claim); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := dir.Lookup(ctx, "alice"); found {
		t.Fatal("Cancel left the reservation in place")
	}
	if err := dir.Cancel(ctx, claim); err != nil {
		t.Fatalf("a retried Cancel failed: %v", err)
	}

	// The dangerous case: our claim lapsed, someone else took the key, and we
	// roll back late.
	lapsing, err := dir.Reserve(ctx, "bob", "player:1", 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	c.advance(11 * time.Second)
	if _, err := dir.Reserve(ctx, "bob", "player:2", 0); err != nil {
		t.Fatal(err)
	}
	if err := dir.Cancel(ctx, lapsing); err != nil {
		t.Fatalf("a late Cancel returned %v, want a no-op", err)
	}
	entry, found, _ := dir.Lookup(ctx, "bob")
	if !found || entry.Owner != "player:2" {
		t.Fatalf("a late Cancel removed the new owner's claim: found=%v entry=%+v", found, entry)
	}
	// Cancel must refuse a committed entry rather than quietly deleting it.
	committedClaim, err := dir.Reserve(ctx, "carol", "player:1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dir.Commit(ctx, committedClaim); err != nil {
		t.Fatal(err)
	}
	if err := dir.Cancel(ctx, committedClaim); !errors.Is(err, ErrClaimStale) {
		t.Fatalf("Cancel on a committed entry returned %v, want ErrClaimStale", err)
	}
}

// Release is owner-checked: it is the only way a committed key goes away, and
// it must not let one owner remove another's entry.
func TestReleaseRequiresOwnership(t *testing.T) {
	dir, _ := newDirectory(t)
	ctx := context.Background()

	claim, err := dir.Reserve(ctx, "alice", "player:1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dir.Commit(ctx, claim); err != nil {
		t.Fatal(err)
	}
	if err := dir.Release(ctx, "alice", "player:2"); !errors.Is(err, ErrOwnerMismatch) {
		t.Fatalf("Release by another owner returned %v, want ErrOwnerMismatch", err)
	}
	if _, found, _ := dir.Lookup(ctx, "alice"); !found {
		t.Fatal("a refused Release removed the entry")
	}
	if err := dir.Release(ctx, "ALICE", "player:1"); err != nil {
		t.Fatalf("Release by the owner failed: %v", err)
	}
	if _, found, _ := dir.Lookup(ctx, "alice"); found {
		t.Fatal("Release left the entry in place")
	}
	// Releasing an absent key is a no-op, so a retried rename does not fail.
	if err := dir.Release(ctx, "alice", "player:1"); err != nil {
		t.Fatalf("Release of an absent key returned %v", err)
	}
	// And the key is genuinely reusable afterwards.
	if _, err := dir.Reserve(ctx, "alice", "player:3", 0); err != nil {
		t.Fatalf("the released key was not reusable: %v", err)
	}
}

// The property the primitive exists for: concurrent contenders for one key
// must produce exactly one holder.
func TestConcurrentReserveHasExactlyOneWinner(t *testing.T) {
	dir, _ := newDirectory(t)
	ctx := context.Background()

	const contenders = 16
	var wait sync.WaitGroup
	var mu sync.Mutex
	winners := []Owner{}
	taken := 0
	for i := 0; i < contenders; i++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			owner := Owner(fmt.Sprintf("player:%d", index))
			claim, err := dir.Reserve(ctx, "alice", owner, 0)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				winners = append(winners, claim.Owner)
			case errors.Is(err, ErrKeyTaken):
				taken++
			default:
				t.Errorf("owner %s: unexpected error %v", owner, err)
			}
		}(i)
	}
	wait.Wait()

	if len(winners) != 1 {
		t.Fatalf("%d contenders won the key, want exactly 1: %v", len(winners), winners)
	}
	if taken != contenders-1 {
		t.Fatalf("%d contenders were refused, want %d", taken, contenders-1)
	}
	entry, found, err := dir.Lookup(ctx, "alice")
	if err != nil || !found {
		t.Fatalf("lookup after the race: found=%v err=%v", found, err)
	}
	if entry.Owner != winners[0] {
		t.Fatalf("stored owner %q is not the winner %q", entry.Owner, winners[0])
	}
}

// Reserve then Commit under concurrency: only the holder may commit, and the
// committed owner must be the one that won.
func TestConcurrentCommitOnlySucceedsForTheHolder(t *testing.T) {
	dir, _ := newDirectory(t)
	ctx := context.Background()

	holder, err := dir.Reserve(ctx, "alice", "player:1", time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	const attackers = 8
	var wait sync.WaitGroup
	var mu sync.Mutex
	succeeded := 0
	for i := 0; i < attackers; i++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			forged := Claim{Key: "alice", Owner: Owner(fmt.Sprintf("player:%d", index+2)), Token: fmt.Sprintf("token-%d", index)}
			if _, err := dir.Commit(ctx, forged); err == nil {
				mu.Lock()
				succeeded++
				mu.Unlock()
			}
		}(i)
	}
	wait.Wait()
	if succeeded != 0 {
		t.Fatalf("%d forged commits succeeded", succeeded)
	}
	entry, err := dir.Commit(ctx, holder)
	if err != nil {
		t.Fatalf("the holder could not commit: %v", err)
	}
	if entry.Owner != "player:1" {
		t.Fatalf("committed owner = %q, want player:1", entry.Owner)
	}
}

func TestNewRejectsAConfigThatCannotBeSafe(t *testing.T) {
	state := versionstore.NewMemoryStore[string, Entry]()
	if _, err := New(nil, Config{Normalize: NormalizeLower, DefaultTTL: time.Minute}); err == nil {
		t.Fatal("a nil state store was accepted")
	}
	if _, err := New(state, Config{DefaultTTL: time.Minute}); err == nil {
		t.Fatal("a missing normalizer was accepted: Alice and alice would be two keys")
	}
	// A reservation with no expiry is the burned-key failure mode.
	if _, err := New(state, Config{Normalize: NormalizeLower}); err == nil {
		t.Fatal("a non-positive default TTL was accepted")
	}
}

func TestEmptyKeyAndOwnerAreRejected(t *testing.T) {
	dir, _ := newDirectory(t)
	ctx := context.Background()
	if _, err := dir.Reserve(ctx, "   ", "player:1", 0); !errors.Is(err, ErrKeyEmpty) {
		t.Fatalf("a blank key returned %v, want ErrKeyEmpty", err)
	}
	if _, err := dir.Reserve(ctx, "alice", "", 0); !errors.Is(err, ErrOwnerEmpty) {
		t.Fatalf("an empty owner returned %v, want ErrOwnerEmpty", err)
	}
}

// Cancel and Release answer nil for a claim that is no longer the caller's.
// That is correct — deleting there is the race this primitive prevents — but
// it means a rollback can do nothing while reporting success, which is the
// defect pattern this repository was built to remove. It is only acceptable
// because it is counted, so the count is asserted rather than assumed.
func TestReportsWhatEachOutcomeWas(t *testing.T) {
	sink := servicemetrics.NewRecorder()
	c := &clock{now: time.Unix(1_700_000_000, 0)}
	dir, err := New(versionstore.NewMemoryStore[string, Entry](), Config{
		Normalize: NormalizeLower, DefaultTTL: time.Minute, Now: c.Now, Metrics: sink,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	claim, err := dir.Reserve(ctx, "Alice", "acct-1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := sink.Count("accepted:reserve"); got != 1 {
		t.Fatalf("a fresh reservation reported %d accepts; %s", got, sink.Events())
	}

	// The same owner asking again gets its existing claim back. That is a
	// replay, not a second reservation, and conflating the two hides a client
	// that is retrying every request.
	if _, err := dir.Reserve(ctx, "Alice", "acct-1", 0); err != nil {
		t.Fatal(err)
	}
	if got := sink.Count("replayed:reserve"); got != 1 {
		t.Fatalf("a retried reservation reported %d replays; %s", got, sink.Events())
	}
	if got := sink.Count("accepted:reserve"); got != 1 {
		t.Fatalf("a retried reservation was also counted as accepted (%d); %s", got, sink.Events())
	}

	if _, err := dir.Reserve(ctx, "alice", "acct-2", 0); !errors.Is(err, ErrKeyTaken) {
		t.Fatalf("a second owner reserved the same key: %v", err)
	}
	if got := sink.Count("refused:reserve:taken"); got != 1 {
		t.Fatalf("a refused reservation reported %d refusals; %s", got, sink.Events())
	}

	if _, err := dir.Commit(ctx, claim); err != nil {
		t.Fatal(err)
	}
	if got := sink.Count("accepted:commit"); got != 1 {
		t.Fatalf("a commit reported %d accepts; %s", got, sink.Events())
	}

	// The silent no-op: cancelling a claim that is not ours. It answers nil,
	// so the counter is the only way anyone learns it happened.
	stale := Claim{Key: "alice", Owner: "acct-9", Token: "not-the-token"}
	if err := dir.Cancel(ctx, stale); err != nil {
		t.Fatalf("cancelling a foreign claim failed instead of no-opping: %v", err)
	}
	if got := sink.Count("dropped:cancel.not_ours"); got != 1 {
		t.Fatalf("a cancel that did nothing reported %d drops; %s", got, sink.Events())
	}

	// Reserving a key we already hold permanently is also a replay. This is a
	// second branch from the still-reserved one above, and it reports through
	// its own call — so it needs its own assertion, or one of the two can stop
	// reporting without any test noticing.
	if _, err := dir.Reserve(ctx, "Alice", "acct-1", 0); err != nil {
		t.Fatal(err)
	}
	if got := sink.Count("replayed:reserve"); got != 2 {
		t.Fatalf("reserving an already-committed key reported %d replays total, want 2; %s", got, sink.Events())
	}
	if got := sink.Count("accepted:reserve"); got != 1 {
		t.Fatalf("reserving an already-committed key was counted as accepted (%d); %s", got, sink.Events())
	}

	if err := dir.Release(ctx, "Alice", "acct-1"); err != nil {
		t.Fatal(err)
	}
	if got := sink.Count("accepted:release"); got != 1 {
		t.Fatalf("a release reported %d accepts; %s", got, sink.Events())
	}
}

// A nil reporter must never change behaviour: every call site is
// unconditional so one cannot be forgotten, which only works if nil is safe.
func TestANilReporterChangesNothing(t *testing.T) {
	dir, _ := newDirectory(t)
	ctx := context.Background()
	claim, err := dir.Reserve(ctx, "Alice", "acct-1", 0)
	if err != nil {
		t.Fatalf("a directory with no reporter failed to reserve: %v", err)
	}
	if _, err := dir.Commit(ctx, claim); err != nil {
		t.Fatal(err)
	}
	if err := dir.Release(ctx, "Alice", "acct-1"); err != nil {
		t.Fatal(err)
	}
}

// contendedStore is a versionstore.Store whose Update loses its first
// compare-and-set: the mutate callback runs once against the current value,
// that attempt is discarded as if another writer got in between, and the
// real store then re-reads and re-applies it. That is the retry the
// versionstore contract permits ("Mutate may be called more than once"), and
// kit's MemoryStore — which the rest of this package's tests run on — never
// exercises it.
type contendedStore struct {
	versionstore.Store[string, Entry]
}

func (s contendedStore) DeleteIf(ctx context.Context, key string, expect versionstore.Versioned[Entry], match func(Entry) bool) error {
	return s.Store.(versionstore.ConditionalDeleter[string, Entry]).DeleteIf(ctx, key, expect, match)
}

func (c contendedStore) Update(ctx context.Context, key string, mutate versionstore.Mutate[Entry]) (versionstore.Versioned[Entry], bool, error) {
	current, found, err := c.Store.Get(ctx, key)
	if err != nil {
		return versionstore.Versioned[Entry]{}, false, err
	}
	if _, _, err := mutate(current.Value, found); err != nil {
		return versionstore.Versioned[Entry]{}, false, err
	}
	return c.Store.Update(ctx, key, mutate)
}

// An operation is accepted once however many times its compare-and-set has
// to retry. Reporting from inside the mutate callback counts attempts, not
// outcomes, so under contention the accept rate reads higher than the number
// of reservations that exist — the metric operators size the directory by.
func TestAContendedWriteIsAcceptedOnce(t *testing.T) {
	sink := servicemetrics.NewRecorder()
	c := &clock{now: time.Unix(1_700_000_000, 0)}
	dir, err := New(contendedStore{versionstore.NewMemoryStore[string, Entry]()}, Config{
		Normalize: NormalizeLower, DefaultTTL: time.Minute, Now: c.Now, Metrics: sink,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	claim, err := dir.Reserve(ctx, "Alice", "acct-1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := sink.Count("accepted:reserve"); got != 1 {
		t.Errorf("one reservation that retried its compare-and-set reported %d accepts; %s", got, sink.Events())
	}
	if _, err := dir.Commit(ctx, claim); err != nil {
		t.Fatal(err)
	}
	if got := sink.Count("accepted:commit"); got != 1 {
		t.Errorf("one commit that retried its compare-and-set reported %d accepts; %s", got, sink.Events())
	}
	// Replays and refusals do not write, so they never retry — but they must
	// still be counted exactly once when the decision is made outside the
	// callback.
	if _, err := dir.Reserve(ctx, "Alice", "acct-1", 0); err != nil {
		t.Fatal(err)
	}
	if got := sink.Count("replayed:reserve"); got != 1 {
		t.Errorf("replayed:reserve = %d, want 1; %s", got, sink.Events())
	}
	if _, err := dir.Reserve(ctx, "Alice", "acct-2", 0); err == nil {
		t.Fatal("second owner reserved a committed key")
	}
	if got := sink.Count("refused:reserve:taken"); got != 1 {
		t.Errorf("refused:reserve:taken = %d, want 1; %s", got, sink.Events())
	}
}

// Redis 状态缺前缀 / 带 ttl 不能构造；Commit 拒绝空键、空令牌、从未预留的键；Cancel 拒绝空键；Release 拒绝空所有者。
func TestDirectoryRefusesBlankIdentifiersAndUnknownClaims(t *testing.T) {
	ctx := context.Background()
	if _, err := NewRedisState(nil, "  ", 0); err == nil || !strings.Contains(err.Error(), "key prefix is required") {
		t.Fatalf("NewRedisState with a blank prefix = %v", err)
	}
	if _, err := NewRedisState(nil, "dir", time.Minute); err == nil || !strings.Contains(err.Error(), "ttl is not supported") {
		t.Fatalf("NewRedisState with a ttl = %v", err)
	}
	dir, _ := newDirectory(t)
	if _, err := dir.Commit(ctx, Claim{Token: "t"}); !errors.Is(err, ErrKeyEmpty) {
		t.Fatalf("Commit without a key = %v", err)
	}
	if _, err := dir.Commit(ctx, Claim{Key: "alice"}); !errors.Is(err, ErrClaimNotFound) {
		t.Fatalf("Commit without a token = %v", err)
	}
	if _, err := dir.Commit(ctx, Claim{Key: "alice", Token: "ghost", Owner: "o"}); !errors.Is(err, ErrClaimNotFound) {
		t.Fatalf("Commit of a never-reserved key = %v", err)
	}
	if err := dir.Cancel(ctx, Claim{Token: "t"}); !errors.Is(err, ErrKeyEmpty) {
		t.Fatalf("Cancel without a key = %v", err)
	}
	if err := dir.Release(ctx, "alice", ""); !errors.Is(err, ErrOwnerEmpty) {
		t.Fatalf("Release without an owner = %v", err)
	}
}

func TestReleaseCannotBypassReservationToken(t *testing.T) {
	dir, _ := newDirectory(t)
	ctx := context.Background()
	claim, err := dir.Reserve(ctx, "name", "owner", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := dir.Release(ctx, "name", "owner"); !errors.Is(err, ErrClaimStale) {
		t.Fatalf("reserved name released without token: %v", err)
	}
	if _, err := dir.Commit(ctx, claim); err != nil {
		t.Fatalf("reservation lost: %v", err)
	}
}

func TestCASConflictHasRetryableDirectoryCode(t *testing.T) {
	if code, _ := Error(versionstore.ErrConflict); code != 530107 {
		t.Fatalf("CAS conflict classified as %d, want 530107", code)
	}
}
func TestReleaseExpiredReservationIsNoOp(t *testing.T) {
	dir, clock := newDirectory(t)
	ctx := context.Background()
	if _, err := dir.Reserve(ctx, "name", "old", time.Minute); err != nil {
		t.Fatal(err)
	}
	clock.advance(2 * time.Minute)
	if err := dir.Release(ctx, "name", "new"); err != nil {
		t.Fatalf("expired reservation conflicts: %v", err)
	}
}

type pausedDelete struct {
	versionstore.Store[string, Entry]
	once             atomic.Bool
	entered, release chan struct{}
}

func (s *pausedDelete) Delete(ctx context.Context, k string, v versionstore.Versioned[Entry]) error {
	s.waitDelete()
	return s.Store.Delete(ctx, k, v)
}

func (s *pausedDelete) DeleteIf(ctx context.Context, k string, v versionstore.Versioned[Entry], match func(Entry) bool) error {
	s.waitDelete()
	conditional, ok := s.Store.(versionstore.ConditionalDeleter[string, Entry])
	if !ok {
		return fmt.Errorf("fixture requires ConditionalDeleter")
	}
	return conditional.DeleteIf(ctx, k, v, match)
}

func (s *pausedDelete) waitDelete() {
	if s.once.CompareAndSwap(false, true) {
		close(s.entered)
		<-s.release
	}
}
func TestDirectoryDeleteCannotRemoveRecreatedOwner(t *testing.T) {
	for _, backend := range []string{"memory", "redis"} {
		for _, operation := range []string{"cancel", "release"} {
			for _, nextOwner := range []Owner{"owner-b", "owner-a"} {
				t.Run(backend+"/"+operation+"/"+string(nextOwner), func(t *testing.T) {
					var state versionstore.Store[string, Entry] = versionstore.NewMemoryStore[string, Entry]()
					if backend == "redis" {
						addr := os.Getenv("ROOST_REVIEW_REDIS")
						if addr == "" {
							t.Skip("set ROOST_REVIEW_REDIS for isolated Redis")
						}
						client, e := driver.NewClient(fredis.DefaultConfig(addr))
						if e != nil {
							t.Fatal(e)
						}
						defer client.Close()
						state, e = NewRedisState(client, fmt.Sprintf("directory-review3:%d", time.Now().UnixNano()), 0)
						if e != nil {
							t.Fatal(e)
						}
					}
					gate := &pausedDelete{Store: state, entered: make(chan struct{}), release: make(chan struct{})}
					dir, e := New(gate, Config{Normalize: NormalizeLower, DefaultTTL: time.Minute})
					if e != nil {
						t.Fatal(e)
					}
					ctx := context.Background()
					a, e := dir.Reserve(ctx, "Hero", "owner-a", time.Minute)
					if e != nil {
						t.Fatal(e)
					}
					if operation == "release" {
						if _, e = dir.Commit(ctx, a); e != nil {
							t.Fatal(e)
						}
					}
					done := make(chan error, 1)
					go func() {
						if operation == "cancel" {
							done <- dir.Cancel(ctx, a)
						} else {
							done <- dir.Release(ctx, "Hero", "owner-a")
						}
					}()
					<-gate.entered
					if operation == "cancel" {
						e = dir.Cancel(ctx, a)
					} else {
						e = dir.Release(ctx, "Hero", "owner-a")
					}
					if e != nil {
						close(gate.release)
						<-done
						t.Fatal(e)
					}
					b, e := dir.Reserve(ctx, "Hero", nextOwner, time.Minute)
					if e != nil {
						close(gate.release)
						<-done
						t.Fatal(e)
					}
					if operation == "release" {
						if _, e = dir.Commit(ctx, b); e != nil {
							close(gate.release)
							<-done
							t.Fatal(e)
						}
					}
					close(gate.release)
					oldErr := <-done
					got, found, e := dir.Lookup(ctx, "Hero")
					if e != nil {
						t.Fatal(e)
					}
					if !found || got.Owner != nextOwner || got.Token != b.Token {
						t.Fatalf("old %s deleted new owner on %s: found=%v entry=%+v oldErr=%v", operation, backend, found, got, oldErr)
					}
				})
			}
		}
	}
}

func TestDirectoryRequiresConditionalDelete(t *testing.T) {
	state := struct {
		versionstore.Store[string, Entry]
	}{versionstore.NewMemoryStore[string, Entry]()}
	if _, err := New(state, Config{Normalize: NormalizeLower, DefaultTTL: time.Minute}); err == nil {
		t.Fatal("constructor accepted a store that loses identity deletion capability")
	}
}
