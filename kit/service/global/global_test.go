package global

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/versionstore"

	"github.com/tjbdwanghaibo/roost-kit/service/servicemetrics"
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

func newService(t *testing.T, mutate ...func(*Config)) (*Service, *clock) {
	t.Helper()
	c := &clock{now: time.Unix(1_700_000_000, 0)}
	cfg := Config{
		Routes:   versionstore.NewMemoryStore[int32, RouteBinding](),
		Leases:   versionstore.NewMemoryStore[int32, GameLease](),
		LeaseTTL: 30 * time.Second,
		Now:      c.Now,
	}
	for _, m := range mutate {
		m(&cfg)
	}
	service, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return service, c
}

func bind(t *testing.T, s *Service, gameSID int32, globalSID int32) RouteBinding {
	t.Helper()
	binding, err := s.Bind(context.Background(), gameSID, "group-a", globalSID)
	if err != nil {
		t.Fatal(err)
	}
	return binding
}

// Binding is insert-only: a game server that is already bound must go through
// migration, which requires the current epoch.
func TestBindIsInsertOnly(t *testing.T) {
	service, _ := newService(t)
	ctx := context.Background()

	binding := bind(t, service, 100, 1)
	if binding.Epoch != 1 || binding.State != RouteActive {
		t.Fatalf("first binding = %+v", binding)
	}
	if _, err := service.Bind(ctx, 100, "group-b", 2); !errors.Is(err, ErrConflict) {
		t.Fatalf("rebinding through Bind returned %v, want ErrConflict", err)
	}
	// The original binding is untouched.
	resolved, err := service.Resolve(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.GlobalGroupID != "group-a" || resolved.GlobalSID != 1 || resolved.Epoch != 1 {
		t.Fatalf("the refused bind changed the binding: %+v", resolved)
	}
	for _, testCase := range []struct {
		label   string
		gameSID int32
		group   string
		global  int32
	}{
		{"zero game sid", 0, "group-a", 1},
		{"empty group", 101, "  ", 1},
		{"zero global sid", 102, "group-a", 0},
	} {
		if _, err := service.Bind(ctx, testCase.gameSID, testCase.group, testCase.global); !errors.Is(err, ErrRouteInvalid) {
			t.Fatalf("%s: %v", testCase.label, err)
		}
	}
	if _, err := service.Resolve(ctx, 999); !errors.Is(err, ErrRouteMissing) {
		t.Fatal("resolving an unbound game did not report not-found")
	}
}

// Migration is epoch-gated. The boundary document of the implementation this
// replaces required epoch CAS and forbade an in-process lock as the
// cross-instance guarantee; the implementation used the lock.
func TestMigrationRequiresTheCurrentEpoch(t *testing.T) {
	service, _ := newService(t)
	ctx := context.Background()
	binding := bind(t, service, 100, 1)

	// A stale epoch is refused.
	if _, err := service.BeginMigration(ctx, 100, 2, binding.Epoch+5); !errors.Is(err, ErrRouteStale) {
		t.Fatalf("a stale epoch was accepted: %v", err)
	}
	migrating, err := service.BeginMigration(ctx, 100, 2, binding.Epoch)
	if err != nil {
		t.Fatal(err)
	}
	if migrating.State != RouteMigrating || migrating.TargetGlobalSID != 2 {
		t.Fatalf("migrating binding = %+v", migrating)
	}
	if migrating.Epoch != binding.Epoch+1 {
		t.Fatalf("epoch = %d, want %d: every accepted change must move the epoch", migrating.Epoch, binding.Epoch+1)
	}
	// The epoch the first caller used is now stale, so a second migration
	// with it loses — this is the property that makes the lock unnecessary.
	if _, err := service.BeginMigration(ctx, 100, 3, binding.Epoch); !errors.Is(err, ErrRouteStale) {
		t.Fatalf("a second migration reused the old epoch: %v", err)
	}
	// Even with the right epoch, a binding already migrating is refused.
	if _, err := service.BeginMigration(ctx, 100, 3, migrating.Epoch); !errors.Is(err, ErrRouteMigrating) {
		t.Fatalf("a concurrent migration was accepted: %v", err)
	}
	completed, err := service.CompleteMigration(ctx, 100, migrating.Epoch)
	if err != nil {
		t.Fatal(err)
	}
	if completed.GlobalSID != 2 || completed.State != RouteActive || completed.TargetGlobalSID != 0 {
		t.Fatalf("completed binding = %+v", completed)
	}
	if completed.Epoch != migrating.Epoch+1 {
		t.Fatalf("completion did not move the epoch: %d", completed.Epoch)
	}
}

// A migration can be abandoned, returning the binding to its current
// instance without losing the epoch discipline.
func TestMigrationCanBeAborted(t *testing.T) {
	service, _ := newService(t)
	ctx := context.Background()
	binding := bind(t, service, 100, 1)
	migrating, err := service.BeginMigration(ctx, 100, 2, binding.Epoch)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.AbortMigration(ctx, 100, binding.Epoch); !errors.Is(err, ErrRouteStale) {
		t.Fatal("abort accepted a stale epoch")
	}
	aborted, err := service.AbortMigration(ctx, 100, migrating.Epoch)
	if err != nil {
		t.Fatal(err)
	}
	if aborted.State != RouteActive || aborted.GlobalSID != 1 || aborted.TargetGlobalSID != 0 {
		t.Fatalf("aborted binding = %+v", aborted)
	}
	// A retried completion on a binding that is no longer migrating is a
	// no-op rather than a failure, and cannot resurrect the target.
	settled, err := service.CompleteMigration(ctx, 100, aborted.Epoch)
	if err != nil {
		t.Fatal(err)
	}
	if settled.GlobalSID != 1 {
		t.Fatalf("a retried completion moved an aborted binding: %+v", settled)
	}
}

// Concurrent migrations on one binding must produce exactly one winner.
func TestConcurrentMigrationsHaveOneWinner(t *testing.T) {
	service, _ := newService(t)
	ctx := context.Background()
	binding := bind(t, service, 100, 1)

	const racers = 12
	var wait sync.WaitGroup
	var mu sync.Mutex
	won := 0
	stale := 0
	for i := 0; i < racers; i++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			_, err := service.BeginMigration(ctx, 100, int32(index+2), binding.Epoch)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				won++
			case errors.Is(err, ErrRouteStale), errors.Is(err, ErrRouteMigrating):
				stale++
			default:
				t.Errorf("unexpected error %v", err)
			}
		}(i)
	}
	wait.Wait()
	if won != 1 {
		t.Fatalf("%d migrations were accepted, want exactly 1", won)
	}
	if stale != racers-1 {
		t.Fatalf("%d were refused, want %d", stale, racers-1)
	}
	resolved, err := service.Resolve(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.State != RouteMigrating {
		t.Fatalf("binding = %+v", resolved)
	}
}

// The confirmed defect: a heartbeat from a previous incarnation must not
// overwrite the live lease. In the implementation this replaces the heartbeat
// read, computed a next version and wrote unconditionally, so a late
// heartbeat replaced the live holder's start time and load.
func TestAStaleIncarnationCannotRenewOrOverwriteALiveLease(t *testing.T) {
	service, c := newService(t)
	ctx := context.Background()
	bind(t, service, 100, 1)

	first, err := service.AcquireLease(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	// The first process dies; its lease lapses and a new process takes over.
	c.advance(31 * time.Second)
	second, err := service.AcquireLease(ctx, 100)
	if err != nil {
		t.Fatalf("the lapsed lease was not re-acquirable: %v", err)
	}
	if second.Incarnation == first.Incarnation {
		t.Fatal("re-acquiring reused the previous incarnation token")
	}
	if second.StartedAtUnix == first.StartedAtUnix {
		t.Fatal("the new incarnation inherited the old start time")
	}

	// Now the zombie heartbeat arrives.
	_, err = service.RenewLease(ctx, 100, first.Incarnation, map[string]string{"cpu": "99"})
	if !errors.Is(err, ErrLeaseNotHolder) {
		t.Fatalf("a stale incarnation renewed the lease: %v", err)
	}
	// And it changed nothing.
	live, found, err := service.Lease(ctx, 100)
	if err != nil || !found {
		t.Fatalf("lease read: found=%v err=%v", found, err)
	}
	if live.Incarnation != second.Incarnation {
		t.Fatalf("the live holder was replaced: %q", live.Incarnation)
	}
	if live.StartedAtUnix != second.StartedAtUnix {
		t.Fatalf("the zombie heartbeat overwrote the start time: %d", live.StartedAtUnix)
	}
	if live.Load != nil {
		t.Fatalf("the zombie heartbeat wrote its load: %+v", live.Load)
	}
	// The error must not leak the current token, or a caller that does not
	// hold the lease learns what would let it renew.
	if err != nil && (errContains(err.Error(), second.Incarnation) || errContains(err.Error(), first.Incarnation)) {
		t.Fatalf("the refusal leaked an incarnation token: %v", err)
	}
}

func errContains(haystack, needle string) bool {
	if needle == "" {
		return false
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

// A live lease cannot be displaced by another process claiming to be the same
// game server: that is a deployment fault, and letting the second one win
// silently is how a lease's start time and load become fiction.
func TestALiveLeaseCannotBeStolen(t *testing.T) {
	service, c := newService(t)
	ctx := context.Background()
	bind(t, service, 100, 1)
	held, err := service.AcquireLease(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.AcquireLease(ctx, 100); !errors.Is(err, ErrConflict) {
		t.Fatalf("a live lease was displaced: %v", err)
	}
	// Renewing keeps it alive past the original deadline.
	c.advance(20 * time.Second)
	renewed, err := service.RenewLease(ctx, 100, held.Incarnation, map[string]string{"players": "120"})
	if err != nil {
		t.Fatal(err)
	}
	if renewed.ExpiresAtUnix <= held.ExpiresAtUnix {
		t.Fatalf("renewal did not extend the deadline: %d -> %d", held.ExpiresAtUnix, renewed.ExpiresAtUnix)
	}
	if renewed.Load["players"] != "120" {
		t.Fatalf("the load snapshot was not recorded: %+v", renewed.Load)
	}
	if renewed.StartedAtUnix != held.StartedAtUnix {
		t.Fatal("renewal changed the start time")
	}
}

// An expired lease is not renewable: the holder must re-acquire, which mints a
// new incarnation and a new start time. Extending it instead would hide that
// the game server was gone.
func TestAnExpiredLeaseMustBeReacquiredNotRenewed(t *testing.T) {
	service, c := newService(t)
	ctx := context.Background()
	bind(t, service, 100, 1)
	held, err := service.AcquireLease(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	c.advance(31 * time.Second)
	if _, err := service.RenewLease(ctx, 100, held.Incarnation, nil); !errors.Is(err, ErrLeaseExpired) {
		t.Fatalf("an expired lease was renewed: %v", err)
	}
	// A reader sees it as gone even though nothing swept.
	lease, found, err := service.Lease(ctx, 100)
	if err != nil || !found {
		t.Fatalf("read: found=%v err=%v", found, err)
	}
	if lease.State == LeaseActive {
		t.Fatal("an elapsed lease reads as active")
	}
	fresh, err := service.AcquireLease(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Incarnation == held.Incarnation {
		t.Fatal("re-acquiring reused the incarnation")
	}
}

// Release is idempotent and must never drop a lease the caller does not hold —
// the release-races-a-reacquire bug.
func TestReleaseIsIdempotentAndOwnerScoped(t *testing.T) {
	service, _ := newService(t)
	ctx := context.Background()
	bind(t, service, 100, 1)
	first, err := service.AcquireLease(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.ReleaseLease(ctx, 100, first.Incarnation); err != nil {
		t.Fatal(err)
	}
	if err := service.ReleaseLease(ctx, 100, first.Incarnation); err != nil {
		t.Fatalf("a retried release failed: %v", err)
	}
	// A released lease is immediately re-acquirable.
	second, err := service.AcquireLease(ctx, 100)
	if err != nil {
		t.Fatalf("a released lease was not re-acquirable: %v", err)
	}
	// The late release from the previous holder must not drop the new one.
	if err := service.ReleaseLease(ctx, 100, first.Incarnation); err != nil {
		t.Fatal(err)
	}
	live, found, err := service.Lease(ctx, 100)
	if err != nil || !found {
		t.Fatalf("read: found=%v err=%v", found, err)
	}
	if live.State != LeaseActive || live.Incarnation != second.Incarnation {
		t.Fatalf("a late release dropped the new holder's lease: %+v", live)
	}
	// Releasing an unknown game is a no-op.
	if err := service.ReleaseLease(ctx, 999, "whatever"); err != nil {
		t.Fatalf("releasing an unknown lease returned %v", err)
	}
}

// Concurrent acquisitions must produce exactly one holder.
func TestConcurrentAcquireHasOneHolder(t *testing.T) {
	service, _ := newService(t)
	ctx := context.Background()
	bind(t, service, 100, 1)

	const racers = 12
	var wait sync.WaitGroup
	var mu sync.Mutex
	holders := map[string]bool{}
	refused := 0
	for i := 0; i < racers; i++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			lease, err := service.AcquireLease(ctx, 100)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				holders[lease.Incarnation] = true
			case errors.Is(err, ErrConflict):
				refused++
			default:
				t.Errorf("unexpected error %v", err)
			}
		}()
	}
	wait.Wait()
	if len(holders) != 1 {
		t.Fatalf("%d incarnations hold the lease, want 1", len(holders))
	}
	if refused != racers-1 {
		t.Fatalf("%d were refused, want %d", refused, racers-1)
	}
}

// Concurrent renewals by the holder must not lose a heartbeat, and must not
// corrupt the record.
func TestConcurrentRenewalsByTheHolderAllSucceed(t *testing.T) {
	service, _ := newService(t)
	ctx := context.Background()
	bind(t, service, 100, 1)
	held, err := service.AcquireLease(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}

	const writers = 8
	var wait sync.WaitGroup
	errs := make([]error, writers)
	for writer := 0; writer < writers; writer++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			for i := 0; i < 5; i++ {
				if _, err := service.RenewLease(ctx, 100, held.Incarnation,
					map[string]string{"writer": fmt.Sprint(index)}); err != nil {
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
	live, found, err := service.Lease(ctx, 100)
	if err != nil || !found {
		t.Fatalf("read: found=%v err=%v", found, err)
	}
	if live.Incarnation != held.Incarnation || live.State != LeaseActive {
		t.Fatalf("the record was corrupted by concurrent renewals: %+v", live)
	}
	if live.StartedAtUnix != held.StartedAtUnix {
		t.Fatal("concurrent renewals changed the start time")
	}
}

// A lease records the binding it was taken under, so it cannot be read as
// belonging to a group it does not.
func TestLeaseCarriesTheBindingItWasTakenUnder(t *testing.T) {
	service, _ := newService(t)
	ctx := context.Background()
	binding := bind(t, service, 100, 7)
	lease, err := service.AcquireLease(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	if lease.GlobalGroupID != binding.GlobalGroupID || lease.GlobalSID != binding.GlobalSID {
		t.Fatalf("lease = %+v, binding = %+v", lease, binding)
	}
	if lease.RouteEpoch != binding.Epoch {
		t.Fatalf("lease epoch = %d, binding epoch = %d", lease.RouteEpoch, binding.Epoch)
	}
	// A lease cannot be taken for an unbound game server: it would have no
	// group to be live in.
	if _, err := service.AcquireLease(ctx, 999); !errors.Is(err, ErrRouteMissing) {
		t.Fatalf("a lease was taken for an unbound game: %v", err)
	}
}

func TestLiveGamesIsBoundedAndGroupScoped(t *testing.T) {
	service, c := newService(t)
	ctx := context.Background()
	for gameSID := int32(100); gameSID < 105; gameSID++ {
		bind(t, service, gameSID, 1)
		if _, err := service.AcquireLease(ctx, gameSID); err != nil {
			t.Fatal(err)
		}
	}
	live, err := service.LiveGames(ctx, "group-a", []int32{100, 101, 102, 103, 104}, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 3 {
		t.Fatalf("got %d live games, want the requested 3", len(live))
	}
	if _, err := service.LiveGames(ctx, "group-b", []int32{100}, 10); err != nil {
		t.Fatal(err)
	}
	if other, _ := service.LiveGames(ctx, "group-b", []int32{100}, 10); len(other) != 0 {
		t.Fatalf("a lease from another group was returned: %+v", other)
	}
	for _, limit := range []int{0, -1, MaxPageSize + 1} {
		if _, err := service.LiveGames(ctx, "group-a", []int32{100}, limit); !errors.Is(err, ErrRangeInvalid) {
			t.Fatalf("limit %d returned %v, want ErrRangeInvalid", limit, err)
		}
	}
	// Expired leases are not live.
	c.advance(31 * time.Second)
	if live, _ := service.LiveGames(ctx, "group-a", []int32{100, 101}, 10); len(live) != 0 {
		t.Fatalf("expired leases are reported as live: %+v", live)
	}
}

func TestRenewRejectsAnOversizedLoadSnapshot(t *testing.T) {
	service, _ := newService(t)
	ctx := context.Background()
	bind(t, service, 100, 1)
	held, err := service.AcquireLease(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	load := map[string]string{}
	for i := 0; i <= MaxLoadEntries; i++ {
		load[fmt.Sprintf("k%d", i)] = "v"
	}
	if _, err := service.RenewLease(ctx, 100, held.Incarnation, load); !errors.Is(err, ErrLeaseInvalid) {
		t.Fatalf("an oversized load snapshot was accepted: %v", err)
	}
	if _, err := service.RenewLease(ctx, 100, "", nil); !errors.Is(err, ErrLeaseInvalid) {
		t.Fatal("an empty incarnation was accepted")
	}
}

func TestNewRejectsAnIncompleteConfig(t *testing.T) {
	routes := versionstore.NewMemoryStore[int32, RouteBinding]()
	leases := versionstore.NewMemoryStore[int32, GameLease]()
	if _, err := New(Config{Leases: leases}); err == nil {
		t.Fatal("a missing route store was accepted")
	}
	if _, err := New(Config{Routes: routes}); err == nil {
		t.Fatal("a missing lease store was accepted")
	}
	if _, err := New(Config{Routes: routes, Leases: leases, LeaseTTL: -time.Second}); err == nil {
		t.Fatal("a negative lease ttl was accepted")
	}
}

// A lease that stopped answering and a lease its holder gave up are different
// events, and an operator needs to tell them apart: one is a dead game
// server, the other is a clean shutdown. Collapsing them into one state — as
// an earlier version of Lease did — loses the only signal that distinguishes
// them, and it also left the elapsed deadline unreadable.
func TestALapsedLeaseIsDistinctFromAReleasedOne(t *testing.T) {
	service, c := newService(t)
	ctx := context.Background()
	bind(t, service, 100, 1)

	held, err := service.AcquireLease(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	c.advance(31 * time.Second)
	lapsed, found, err := service.Lease(ctx, 100)
	if err != nil || !found {
		t.Fatalf("read: found=%v err=%v", found, err)
	}
	if lapsed.State != LeaseLapsed {
		t.Fatalf("an unanswered lease reads as %q, want lapsed", lapsed.State)
	}
	// The deadline it missed must still be readable, or a caller cannot see
	// when the game server stopped answering.
	if lapsed.ExpiresAtUnix != held.ExpiresAtUnix {
		t.Fatalf("the elapsed deadline was erased: %d, want %d", lapsed.ExpiresAtUnix, held.ExpiresAtUnix)
	}
	if lapsed.LastHeartbeatAtUnix != held.LastHeartbeatAtUnix {
		t.Fatal("the last heartbeat time was erased")
	}

	// A clean release reads as released, with no deadline outstanding.
	bind(t, service, 200, 1)
	given, err := service.AcquireLease(ctx, 200)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.ReleaseLease(ctx, 200, given.Incarnation); err != nil {
		t.Fatal(err)
	}
	released, found, err := service.Lease(ctx, 200)
	if err != nil || !found {
		t.Fatalf("read: found=%v err=%v", found, err)
	}
	if released.State != LeaseReleased {
		t.Fatalf("a released lease reads as %q", released.State)
	}
	if released.ExpiresAtUnix != 0 {
		t.Fatalf("a released lease still carries a deadline: %d", released.ExpiresAtUnix)
	}
	// Neither is live, and both are re-acquirable.
	for _, gameSID := range []int32{100, 200} {
		if _, err := service.AcquireLease(ctx, gameSID); err != nil {
			t.Fatalf("game %d was not re-acquirable: %v", gameSID, err)
		}
	}
}

// The incarnation token is minted only when the lease is actually takeable,
// and only once however many compare-and-set retries the write needs.
//
// Minting before the check spends a token on every refused acquire; minting
// inside the callback spends one per retry, because Update may call the
// callback more than once. Neither matters for crypto/rand, and both matter
// for a rate-limited or remote minter — which is exactly the kind of
// dependency a caller is entitled to inject.
func TestAcquireMintsAnIncarnationOnlyWhenItCanTakeTheLease(t *testing.T) {
	var minted int
	service, c := newService(t, func(cfg *Config) {
		cfg.NewIncarnation = func() (string, error) {
			minted++
			return fmt.Sprintf("token-%d", minted), nil
		}
	})
	ctx := context.Background()
	bind(t, service, 100, 1)

	if _, err := service.AcquireLease(ctx, 100); err != nil {
		t.Fatal(err)
	}
	if minted != 1 {
		t.Fatalf("a successful acquire minted %d tokens, want 1", minted)
	}
	// Every refused acquire must mint nothing.
	for attempt := 0; attempt < 5; attempt++ {
		if _, err := service.AcquireLease(ctx, 100); !errors.Is(err, ErrConflict) {
			t.Fatalf("attempt %d: %v", attempt, err)
		}
	}
	if minted != 1 {
		t.Fatalf("five refused acquires minted %d tokens; a refused acquire must mint none", minted-1)
	}
	// And an acquire for an unbound game must not mint either: it fails
	// before the lease is ever considered.
	if _, err := service.AcquireLease(ctx, 999); !errors.Is(err, ErrRouteMissing) {
		t.Fatal("an unbound game was accepted")
	}
	if minted != 1 {
		t.Fatalf("an unroutable acquire minted a token")
	}
	// A takeable lease mints exactly one more.
	c.advance(31 * time.Second)
	if _, err := service.AcquireLease(ctx, 100); err != nil {
		t.Fatal(err)
	}
	if minted != 2 {
		t.Fatalf("re-acquiring a lapsed lease minted %d tokens total, want 2", minted)
	}
	// A minter that fails must fail the acquire rather than storing an empty
	// incarnation, which nothing could ever present.
	failing, _ := newService(t, func(cfg *Config) {
		cfg.NewIncarnation = func() (string, error) { return "", errors.New("minter is down") }
	})
	bind(t, failing, 200, 1)
	if _, err := failing.AcquireLease(ctx, 200); err == nil {
		t.Fatal("a failing minter produced a lease")
	}
	if _, found, _ := failing.Lease(ctx, 200); found {
		t.Fatal("a failed acquire stored a lease with no incarnation")
	}
}

// The two refusals this package exists to make — a stale route epoch and a
// heartbeat from a process that is no longer the holder — were both
// unobservable in the implementation it replaces. Making them is only half the
// job; an operator has to be able to see that they are happening, so the
// reports are asserted rather than assumed.
func TestRefusalsAndAcceptancesAreReported(t *testing.T) {
	sink := servicemetrics.NewRecorder()
	service, _ := newService(t, func(cfg *Config) { cfg.Metrics = sink })
	ctx := context.Background()

	binding := bind(t, service, 7, 100)
	if got := sink.Count("accepted:bind"); got != 1 {
		t.Fatalf("bind reported %d accepts; %s", got, sink.Events())
	}

	// A rebind presenting an epoch that is no longer current: the CAS the
	// boundary document required and an in-process lock cannot provide.
	if _, err := service.BeginMigration(ctx, 7, 200, binding.Epoch+9); !errors.Is(err, ErrRouteStale) {
		t.Fatalf("a stale epoch was accepted: %v", err)
	}
	if got := sink.Count("refused:begin_migration:stale_epoch"); got != 1 {
		t.Fatalf("a stale epoch reported %d refusals; %s", got, sink.Events())
	}

	if _, err := service.BeginMigration(ctx, 7, 200, binding.Epoch); err != nil {
		t.Fatal(err)
	}
	if got := sink.Count("accepted:begin_migration"); got != 1 {
		t.Fatalf("a migration reported %d accepts; %s", got, sink.Events())
	}

	lease, err := service.AcquireLease(ctx, 7)
	if err != nil {
		t.Fatal(err)
	}
	if got := sink.Count("accepted:acquire_lease"); got != 1 {
		t.Fatalf("an acquire reported %d accepts; %s", got, sink.Events())
	}

	// A second process claiming to be the same game server.
	if _, err := service.AcquireLease(ctx, 7); !errors.Is(err, ErrConflict) {
		t.Fatalf("a held lease was displaced: %v", err)
	}
	if got := sink.Count("refused:acquire_lease:held"); got != 1 {
		t.Fatalf("a refused acquire reported %d refusals; %s", got, sink.Events())
	}

	// The confirmed defect: a heartbeat from a previous incarnation.
	if _, err := service.RenewLease(ctx, 7, "a-token-from-a-dead-process", nil); !errors.Is(err, ErrLeaseNotHolder) {
		t.Fatalf("a foreign incarnation renewed the lease: %v", err)
	}
	if got := sink.Count("refused:renew_lease:not_holder"); got != 1 {
		t.Fatalf("a fenced-off renewal reported %d refusals; %s", got, sink.Events())
	}

	if _, err := service.RenewLease(ctx, 7, lease.Incarnation, nil); err != nil {
		t.Fatal(err)
	}
	if got := sink.Count("accepted:renew_lease"); got != 1 {
		t.Fatalf("a renewal reported %d accepts; %s", got, sink.Events())
	}

	// A release from a process that is no longer the holder answers nil, to
	// avoid dropping the new holder's lease. That is a success answer for an
	// operation that did nothing, so it is counted.
	if err := service.ReleaseLease(ctx, 7, "a-token-from-a-dead-process"); err != nil {
		t.Fatalf("a foreign release failed instead of no-opping: %v", err)
	}
	if got := sink.Count("dropped:release_lease.not_ours"); got != 1 {
		t.Fatalf("a release that did nothing reported %d drops; %s", got, sink.Events())
	}

	if err := service.ReleaseLease(ctx, 7, lease.Incarnation); err != nil {
		t.Fatal(err)
	}
	if got := sink.Count("accepted:release_lease"); got != 1 {
		t.Fatalf("a release reported %d accepts; %s", got, sink.Events())
	}
	// And a retried release is a replay, not a second release.
	if err := service.ReleaseLease(ctx, 7, lease.Incarnation); err != nil {
		t.Fatal(err)
	}
	if got := sink.Count("replayed:release_lease"); got != 1 {
		t.Fatalf("a retried release reported %d replays; %s", got, sink.Events())
	}
}

// A nil reporter must never change behaviour.
func TestANilReporterChangesNothing(t *testing.T) {
	service, _ := newService(t)
	ctx := context.Background()
	bind(t, service, 7, 100)
	lease, err := service.AcquireLease(ctx, 7)
	if err != nil {
		t.Fatalf("a service with no reporter failed to acquire: %v", err)
	}
	if _, err := service.RenewLease(ctx, 7, lease.Incarnation, nil); err != nil {
		t.Fatal(err)
	}
	if err := service.ReleaseLease(ctx, 7, lease.Incarnation); err != nil {
		t.Fatal(err)
	}
}
