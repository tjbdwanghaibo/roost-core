package account

import (
	"context"
	"errors"
	"fmt"
	goredis "github.com/redis/go-redis/v9"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/servicemetrics"
	fredis "github.com/tjbdwanghaibo/roost-core/infra/storage/redis"
	"github.com/tjbdwanghaibo/roost-core/infra/storage/redis/driver"
	"github.com/tjbdwanghaibo/roost-core/infra/storage/versionstore"
	"github.com/tjbdwanghaibo/roost-core/service/directory"
	"os"
	"testing"
	"time"
)

func revn06Redis() string {
	if os.Getenv("ROOST_REVIEW3_BACKEND") != "redis" {
		return ""
	}
	return os.Getenv("ROOST_REVIEW_REDIS")
}

// revn06AccountService is newService, optionally over the isolated Redis.
func revn06AccountService(t *testing.T, mutate ...func(*Config)) (*Service, *clock, Config) {
	t.Helper()
	addr := revn06Redis()
	if addr == "" {
		return newService(t, mutate...)
	}
	client, err := driver.NewClient(fredis.DefaultConfig(addr))
	if err != nil {
		t.Fatal(err)
	}
	prefix := fmt.Sprintf("revn06:account:%d", time.Now().UnixNano())
	t.Cleanup(func() {
		client.Close()
		c := goredis.NewClient(&goredis.Options{Addr: addr})
		defer c.Close()
		keys, err := c.Keys(context.Background(), prefix+":*").Result()
		if err == nil && len(keys) > 0 {
			err = c.Del(context.Background(), keys...).Err()
		}
		if err != nil {
			t.Errorf("cleanup %s: %v", prefix, err)
		}
	})
	stores, err := NewRedisStores(client, prefix, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	base := func(c *Config) {
		c.Accounts, c.Roles, c.Servers, c.Slots, c.Names = stores.Accounts, stores.Roles, stores.Servers, stores.Slots, stores.Names
		c.ClaimTTL = time.Second
	}
	return newService(t, append([]func(*Config){base}, mutate...)...)
}

// revn06Lapse lets the current reservation of name expire.
func revn06Lapse(t *testing.T, cfg Config, clock *clock, name string) {
	t.Helper()
	if revn06Redis() == "" {
		clock.advance(time.Minute)
		return
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		_, found, err := cfg.Names.Lookup(context.Background(), name)
		if err != nil {
			t.Fatal(err)
		}
		if !found {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("name %q reservation did not lapse within 10s", name)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// revn06DeadPlan leaves account "revn06-loser" with an admitted plan for
// "Hero" whose role Create reply was lost, lets the reservation lapse and has
// another account commit "Hero". The loser's plan can no longer complete.
func revn06DeadPlan(t *testing.T, mutate ...func(*Config)) (*Service, Config, *review3LostRole, Account, Role) {
	t.Helper()
	var roles *review3LostRole
	s, clock, cfg := revn06AccountService(t, append([]func(*Config){func(c *Config) {
		roles = &review3LostRole{Store: c.Roles}
		c.Roles = roles
	}}, mutate...)...)
	ctx := context.Background()
	a := login(t, s, "revn06-loser")
	if _, err := s.CreateRole(ctx, a.ID, 1, "Hero"); err == nil {
		t.Fatal("fixture must lose the role create reply")
	}
	revn06Lapse(t, cfg, clock, "Hero")
	b := login(t, s, "revn06-winner")
	winner, err := s.CreateRole(ctx, b.ID, 1, "Hero")
	if err != nil {
		t.Fatalf("winner could not take the lapsed name: %v", err)
	}
	return s, cfg, roles, a, winner
}

func TestADifferentNameReleasesAPlanWhoseNameWasCommittedElsewhere(t *testing.T) {
	sink := servicemetrics.NewRecorder()
	s, cfg, roles, a, winner := revn06DeadPlan(t, func(c *Config) { c.Metrics = sink })
	ctx := context.Background()

	// No same-name retry first: the player simply picks another name.
	knight, err := s.CreateRole(ctx, a.ID, 1, "Knight")
	if err != nil {
		t.Fatalf("account %s cannot create a role under another name after its pending name was committed elsewhere: %v", a.ID, err)
	}
	if knight.PlayerID == roles.first || knight.Name != "Knight" {
		t.Fatalf("fresh plan reused the abandoned plan: %+v (orphan %d)", knight, roles.first)
	}
	if slot := mustSlot(t, cfg, a.ID); slot.PlayerID != knight.PlayerID || slot.Creation.Name != "Knight" {
		t.Fatalf("slot after the fresh plan: %+v", slot)
	}
	if _, err := s.SelectRole(ctx, a.ID, knight.PlayerID); err != nil {
		t.Fatalf("new role not playable: %v", err)
	}
	if _, err := s.SelectRole(ctx, a.ID, roles.first); !errors.Is(err, ErrConflict) {
		t.Fatalf("orphan role %d became playable: %v", roles.first, err)
	}
	if entry, found, err := cfg.Names.Lookup(ctx, "Hero"); err != nil || !found || entry.State != directory.StateCommitted ||
		entry.Owner != directory.Owner(slotKeyFor(winner.AccountID, 1)+"/"+winner.CreationID) {
		t.Fatalf("winner's name entry changed: %+v found=%v err=%v", entry, found, err)
	}
	if got := sink.Count("dropped:create_role.plan_released"); got != 1 {
		t.Fatalf("plan release counted %d times; %s", got, sink.Events())
	}
	if got := sink.Count("dropped:rollback.failed"); got != 0 {
		t.Fatalf("a successful release counted %d failures; %s", got, sink.Events())
	}
}

// What a different name must NOT release: plans that can still complete.
func TestADifferentNameKeepsAPlanThatCanStillComplete(t *testing.T) {
	ctx := context.Background()
	t.Run("own live reservation", func(t *testing.T) {
		var roles *review3LostRole
		s, _, cfg := revn06AccountService(t, func(c *Config) { roles = &review3LostRole{Store: c.Roles}; c.Roles = roles })
		a := login(t, s, "revn06-live")
		if _, err := s.CreateRole(ctx, a.ID, 1, "Hero"); err == nil {
			t.Fatal("fixture must lose the role reply")
		}
		if _, err := s.CreateRole(ctx, a.ID, 1, "Knight"); !errors.Is(err, ErrRoleLimit) {
			t.Fatalf("got %v, want ErrRoleLimit", err)
		}
		if slot := mustSlot(t, cfg, a.ID); slot.Creation.PlayerID != roles.first || slot.Creation.Name != "Hero" {
			t.Fatalf("slot changed: %+v", slot)
		}
		if hero, err := s.CreateRole(ctx, a.ID, 1, "Hero"); err != nil || hero.PlayerID != roles.first {
			t.Fatalf("same-name retry: %+v %v", hero, err)
		}
	})
	t.Run("own committed name", func(t *testing.T) {
		s, clock, cfg := revn06AccountService(t, func(c *Config) { c.Names = &review3LostCommit{Directory: c.Names} })
		a := login(t, s, "revn06-committed")
		if _, err := s.CreateRole(ctx, a.ID, 1, "Hero"); err == nil {
			t.Fatal("fixture must lose the commit reply")
		}
		clock.advance(time.Minute) // a committed entry never lapses; only time moves
		if _, err := s.CreateRole(ctx, a.ID, 1, "Knight"); !errors.Is(err, ErrRoleLimit) {
			t.Fatalf("got %v, want ErrRoleLimit", err)
		}
		if slot := mustSlot(t, cfg, a.ID); slot.Creation.Name != "Hero" || slot.PlayerID != 0 {
			t.Fatalf("slot changed: %+v", slot)
		}
		if hero, err := s.CreateRole(ctx, a.ID, 1, "Hero"); err != nil || hero.Name != "Hero" {
			t.Fatalf("same-name retry no longer completes the plan: %+v %v", hero, err)
		}
	})
	t.Run("foreign reservation only", func(t *testing.T) {
		var roles *review3LostRole
		s, clock, cfg := revn06AccountService(t, func(c *Config) { roles = &review3LostRole{Store: c.Roles}; c.Roles = roles })
		a := login(t, s, "revn06-reserved")
		if _, err := s.CreateRole(ctx, a.ID, 1, "Hero"); err == nil {
			t.Fatal("fixture must lose the role reply")
		}
		revn06Lapse(t, cfg, clock, "Hero")
		if _, err := cfg.Names.Reserve(ctx, "Hero", "rival", time.Minute); err != nil {
			t.Fatal(err)
		}
		if _, err := s.CreateRole(ctx, a.ID, 1, "Knight"); !errors.Is(err, ErrRoleLimit) {
			t.Fatalf("got %v, want ErrRoleLimit", err)
		}
		if slot := mustSlot(t, cfg, a.ID); slot.Creation.PlayerID != roles.first || !slot.Creation.Admitted {
			t.Fatalf("plan released under a mere reservation: %+v", slot)
		}
	})
}

// slotsFailingDeleteOnce loses the first conditional delete — the release —
// while every other slot operation is real.
type slotsFailingDeleteOnce struct {
	versionstore.Store[string, Slot]
	failed bool
}

func (s *slotsFailingDeleteOnce) DeleteIf(ctx context.Context, key string, expect versionstore.Versioned[Slot], match func(Slot) bool) error {
	if !s.failed {
		s.failed = true
		return fmt.Errorf("slots: connection reset")
	}
	return s.Store.(versionstore.ConditionalDeleter[string, Slot]).DeleteIf(ctx, key, expect, match)
}

func TestAFailedReleaseOfADeadPlanIsCountedAndRetriedByTheNextName(t *testing.T) {
	sink := servicemetrics.NewRecorder()
	var slots *slotsFailingDeleteOnce
	s, cfg, roles, a, _ := revn06DeadPlan(t, func(c *Config) {
		c.Metrics = sink
		slots = &slotsFailingDeleteOnce{Store: c.Slots}
		c.Slots = slots
	})
	ctx := context.Background()
	if _, err := s.CreateRole(ctx, a.ID, 1, "Knight"); err == nil {
		t.Fatal("a create whose release failed reported success")
	}
	if !slots.failed {
		t.Fatal("the different-name create never tried to release the dead plan")
	}
	if slot := mustSlot(t, cfg, a.ID); slot.Creation.PlayerID != roles.first {
		t.Fatalf("slot after a failed release: %+v", slot)
	}
	if got := sink.Count("dropped:rollback.failed"); got != 1 {
		t.Fatalf("a failed release counted %d times, want 1; %s", got, sink.Events())
	}
	knight, err := s.CreateRole(ctx, a.ID, 1, "Knight")
	if err != nil {
		t.Fatalf("the next attempt did not retry the release: %v", err)
	}
	if slot := mustSlot(t, cfg, a.ID); slot.PlayerID != knight.PlayerID {
		t.Fatalf("slot after the retried release: %+v", slot)
	}
}

// NC-50：同名重试路径的补偿失败同样要计数（Config.Metrics 的“failed rollback”信号）。
func TestAFailedReleaseOnTheSameNamePathIsCounted(t *testing.T) {
	sink := servicemetrics.NewRecorder()
	var slots *slotsFailingDeleteOnce
	s, cfg, roles, a, _ := revn06DeadPlan(t, func(c *Config) {
		c.Metrics = sink
		slots = &slotsFailingDeleteOnce{Store: c.Slots}
		c.Slots = slots
	})
	ctx := context.Background()
	if _, err := s.CreateRole(ctx, a.ID, 1, "Hero"); !errors.Is(err, ErrNameTaken) {
		t.Fatalf("same-name retry after a foreign commit: got %v, want ErrNameTaken", err)
	}
	if !slots.failed {
		t.Fatal("the same-name retry never tried to release the dead plan")
	}
	if slot := mustSlot(t, cfg, a.ID); slot.Creation.PlayerID != roles.first {
		t.Fatalf("slot after a failed release: %+v", slot)
	}
	if got := sink.Count("dropped:rollback.failed"); got != 1 {
		t.Fatalf("a failed compensation counted %d times, want 1; %s", got, sink.Events())
	}
	if got := sink.Count("dropped:create_role.plan_released"); got != 0 {
		t.Fatalf("a failed release was counted as released; %s", sink.Events())
	}
}

func pendingClaimTTL() time.Duration {
	if os.Getenv("ROOST_REVIEW3_BACKEND") == "redis" {
		return time.Second
	}
	return DefaultClaimTTL
}

// lapseNameClaim makes the current reservation of name expire: on Memory by
// advancing the shared clock, on Redis by waiting for the directory to report
// the key absent (a condition, not a fixed sleep).
func lapseNameClaim(t *testing.T, cfg Config, clock *clock, name string) {
	t.Helper()
	if os.Getenv("ROOST_REVIEW3_BACKEND") != "redis" {
		clock.advance(time.Minute)
		return
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		_, found, err := cfg.Names.Lookup(context.Background(), name)
		if err != nil {
			t.Fatal(err)
		}
		if !found {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("name %q reservation did not lapse within 10s", name)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// losingPlan leaves account a with an admitted plan for "Hero" whose role
// Create reply was lost, then lets the reservation lapse. roles.first is the
// orphan role id the lost reply created.
func losingPlan(t *testing.T, mutate ...func(*Config)) (*Service, *clock, Config, *review3LostRole, Account) {
	t.Helper()
	var roles *review3LostRole
	s, clock, cfg := review3AccountService(t, append([]func(*Config){func(c *Config) {
		roles = &review3LostRole{Store: c.Roles}
		c.Roles = roles
		c.ClaimTTL = pendingClaimTTL()
	}}, mutate...)...)
	a := login(t, s, "rr06-loser")
	if _, err := s.CreateRole(context.Background(), a.ID, 1, "Hero"); err == nil {
		t.Fatal("fixture must lose the role create reply")
	}
	lapseNameClaim(t, cfg, clock, "Hero")
	return s, clock, cfg, roles, a
}

func TestNameCommittedElsewhereReleasesThePendingSlot(t *testing.T) {
	s, _, cfg, roles, a := losingPlan(t)
	ctx := context.Background()
	b := login(t, s, "rr06-winner")
	winner, err := s.CreateRole(ctx, b.ID, 1, "Hero")
	if err != nil {
		t.Fatalf("winner could not take the lapsed name: %v", err)
	}
	if _, err = s.CreateRole(ctx, a.ID, 1, "Hero"); !errors.Is(err, ErrNameTaken) {
		t.Fatalf("same-name retry after a foreign commit: got %v, want ErrNameTaken", err)
	}
	knight, err := s.CreateRole(ctx, a.ID, 1, "Knight")
	if err != nil {
		t.Fatalf("account %s cannot create any role on server 1 after its name was committed elsewhere: %v", a.ID, err)
	}
	if knight.PlayerID == roles.first {
		t.Fatalf("the abandoned plan's player id %d was reused for a different name", roles.first)
	}
	if slot := mustSlot(t, cfg, a.ID); slot.PlayerID != knight.PlayerID {
		t.Fatalf("slot after the fresh plan: %+v", slot)
	}
	// The orphan the lost reply left behind stays unplayable; the winner's role is untouched.
	if _, err = s.SelectRole(ctx, a.ID, roles.first); !errors.Is(err, ErrConflict) {
		t.Fatalf("orphan role %d became playable: %v", roles.first, err)
	}
	if _, err = s.SelectRole(ctx, b.ID, winner.PlayerID); err != nil {
		t.Fatalf("winner's role broke: %v", err)
	}
	if entry, found, err := cfg.Names.Lookup(ctx, "Hero"); err != nil || !found || entry.State != directory.StateCommitted || entry.Owner != directory.Owner(slotKeyFor(b.ID, 1)+"/"+winner.CreationID) {
		t.Fatalf("winner's name entry changed: %+v found=%v err=%v", entry, found, err)
	}
}

func mustSlot(t *testing.T, cfg Config, accountID string) Slot {
	t.Helper()
	slot, found, err := cfg.Slots.Get(context.Background(), slotKeyFor(accountID, 1))
	if err != nil || !found {
		t.Fatalf("slot of %s: found=%v err=%v", accountID, found, err)
	}
	return slot.Value
}

// A foreign RESERVATION is not final: it lapses, and the plan can still
// complete. The slot and its proof stay, exactly as RR-20260929-19 pinned.
func TestNameReservedElsewhereKeepsThePendingSlot(t *testing.T) {
	s, _, cfg, roles, a := losingPlan(t)
	ctx := context.Background()
	if _, err := cfg.Names.Reserve(ctx, "Hero", "rival", time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateRole(ctx, a.ID, 1, "Hero"); !errors.Is(err, ErrNameTaken) {
		t.Fatalf("same-name retry under a foreign reservation: got %v, want ErrNameTaken", err)
	}
	if slot := mustSlot(t, cfg, a.ID); !slot.Creation.Admitted || slot.Creation.PlayerID != roles.first {
		t.Fatalf("admitted plan was released under a mere reservation: %+v", slot)
	}
	if _, err := s.CreateRole(ctx, a.ID, 1, "Knight"); !errors.Is(err, ErrRoleLimit) {
		t.Fatalf("a different name under a pending plan: got %v, want ErrRoleLimit", err)
	}
}

func TestResolvePendingCreationFreesTheSlotAndRecordsTheNote(t *testing.T) {
	s, clock, cfg, roles, a := losingPlan(t)
	ctx := context.Background()
	if _, err := cfg.Names.Reserve(ctx, "Hero", "rival", time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ResolvePendingCreation(ctx, a.ID, 1, "   "); !errors.Is(err, ErrAdminNoteRequired) {
		t.Fatalf("empty note: got %v, want ErrAdminNoteRequired", err)
	}
	if _, found, err := cfg.Slots.Get(ctx, slotKeyFor(a.ID, 1)); err != nil || !found {
		t.Fatalf("a refused call touched the slot: found=%v err=%v", found, err)
	}
	clock.advance(time.Second)
	plan, err := s.ResolvePendingCreation(ctx, a.ID, 1, "ticket 42: player wants another name")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if plan.PlayerID != roles.first || plan.Name != "Hero" || !plan.Admitted {
		t.Fatalf("returned plan is not the abandoned one: %+v (orphan %d)", plan, roles.first)
	}
	if _, found, err := cfg.Slots.Get(ctx, slotKeyFor(a.ID, 1)); err != nil || found {
		t.Fatalf("slot survived the resolution: found=%v err=%v", found, err)
	}
	acct, found, err := cfg.Accounts.Get(ctx, a.ID)
	if err != nil || !found || acct.Value.AdminNote != "ticket 42: player wants another name" || acct.Value.AdminActionAtUnix != clock.Now().Unix() {
		t.Fatalf("note not recorded on the account: %+v found=%v err=%v", acct.Value, found, err)
	}
	// The account creates again, with a fresh plan and a fresh id.
	knight, err := s.CreateRole(ctx, a.ID, 1, "Knight")
	if err != nil {
		t.Fatalf("account still blocked after resolution: %v", err)
	}
	if knight.PlayerID == roles.first || knight.CreationID == plan.ID {
		t.Fatalf("fresh plan reused the abandoned identity: %+v vs %+v", knight, plan)
	}
	if _, err = s.SelectRole(ctx, a.ID, roles.first); !errors.Is(err, ErrConflict) {
		t.Fatalf("orphan role %d became playable: %v", roles.first, err)
	}
	if _, err = s.ResolvePendingCreation(ctx, a.ID, 1, "again"); !errors.Is(err, ErrNotResolvable) {
		t.Fatalf("resolving a published slot: got %v, want ErrNotResolvable", err)
	}
	// Login still works and does not erase the note.
	again := login(t, s, "rr06-loser")
	if again.AdminNote != "ticket 42: player wants another name" {
		t.Fatalf("login erased the operator note: %+v", again)
	}
}

// What the entry refuses, because it cannot prove the release safe.
func TestResolvePendingCreationRefusesWhatItCannotProve(t *testing.T) {
	ctx := context.Background()
	t.Run("no slot", func(t *testing.T) {
		s, _, _ := newService(t)
		a := login(t, s, "rr06-empty")
		if _, err := s.ResolvePendingCreation(ctx, a.ID, 1, "note"); !errors.Is(err, ErrNotResolvable) {
			t.Fatalf("got %v, want ErrNotResolvable", err)
		}
	})
	t.Run("legacy empty slot", func(t *testing.T) {
		s, _, cfg := newService(t)
		a := login(t, s, "rr06-legacy")
		if _, _, err := cfg.Slots.Create(ctx, slotKeyFor(a.ID, 1), Slot{AccountID: a.ID, ServerID: 1}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.ResolvePendingCreation(ctx, a.ID, 1, "note"); !errors.Is(err, ErrNotResolvable) {
			t.Fatalf("got %v, want ErrNotResolvable", err)
		}
		if _, found, err := cfg.Slots.Get(ctx, slotKeyFor(a.ID, 1)); err != nil || !found {
			t.Fatalf("legacy slot was guessed away: found=%v err=%v", found, err)
		}
	})
	t.Run("live own reservation", func(t *testing.T) {
		var roles *review3LostRole
		s, _, cfg := newService(t, func(c *Config) { roles = &review3LostRole{Store: c.Roles}; c.Roles = roles })
		a := login(t, s, "rr06-live")
		if _, err := s.CreateRole(ctx, a.ID, 1, "Hero"); err == nil {
			t.Fatal("fixture must lose the role reply")
		}
		// The reservation has not lapsed: an attempt may be in flight.
		if _, err := s.ResolvePendingCreation(ctx, a.ID, 1, "note"); !errors.Is(err, ErrNotResolvable) {
			t.Fatalf("got %v, want ErrNotResolvable", err)
		}
		if slot := mustSlot(t, cfg, a.ID); slot.Creation.PlayerID != roles.first {
			t.Fatalf("slot changed by a refusal: %+v", slot)
		}
		// And the plan does complete by the ordinary path.
		if hero, err := s.CreateRole(ctx, a.ID, 1, "Hero"); err != nil || hero.PlayerID != roles.first {
			t.Fatalf("same-name retry: %+v %v", hero, err)
		}
	})
	t.Run("own committed name", func(t *testing.T) {
		s, clock, cfg := newService(t, func(c *Config) { c.Names = &review3LostCommit{Directory: c.Names} })
		a := login(t, s, "rr06-committed")
		if _, err := s.CreateRole(ctx, a.ID, 1, "Hero"); err == nil {
			t.Fatal("fixture must lose the commit reply")
		}
		clock.advance(time.Minute)
		if _, err := s.ResolvePendingCreation(ctx, a.ID, 1, "note"); !errors.Is(err, ErrNotResolvable) {
			t.Fatalf("got %v, want ErrNotResolvable", err)
		}
		if _, err := s.CreateRole(ctx, a.ID, 1, "Hero"); err != nil {
			t.Fatalf("same-name retry no longer recovers the committed name: %v", err)
		}
		if _, err := s.ResolvePendingCreation(ctx, a.ID, 1, "note"); !errors.Is(err, ErrNotResolvable) {
			t.Fatalf("published slot: got %v, want ErrNotResolvable", err)
		}
		if name, found, err := cfg.Names.Lookup(ctx, "Hero"); err != nil || !found || name.State != directory.StateCommitted {
			t.Fatalf("name entry: %+v found=%v err=%v", name, found, err)
		}
	})
	t.Run("slot moved after the decision", func(t *testing.T) {
		var roles *review3LostRole
		s, clock, cfg := newService(t, func(c *Config) { roles = &review3LostRole{Store: c.Roles}; c.Roles = roles })
		a := login(t, s, "rr06-moved")
		if _, err := s.CreateRole(ctx, a.ID, 1, "Hero"); err == nil {
			t.Fatal("fixture must lose the role reply")
		}
		clock.advance(time.Minute)
		// A same-name retry lands between the operator's read and the
		// release: the plan re-admits (version bump) and publishes.
		cfg.Slots = &slotsRacingResolution{Store: cfg.Slots, race: func() {
			if hero, err := s.CreateRole(ctx, a.ID, 1, "Hero"); err != nil || hero.PlayerID != roles.first {
				t.Errorf("racing retry: %+v %v", hero, err)
			}
		}}
		racer, err := New(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := racer.ResolvePendingCreation(ctx, a.ID, 1, "note"); !errors.Is(err, ErrConflict) {
			t.Fatalf("release landed on a slot that moved: got %v, want ErrConflict", err)
		}
		if slot := mustSlot(t, cfg, a.ID); slot.PlayerID != roles.first {
			t.Fatalf("published role lost its slot: %+v", slot)
		}
		if _, err := s.SelectRole(ctx, a.ID, roles.first); err != nil {
			t.Fatalf("published role not playable: %v", err)
		}
	})
}

// slotsRacingResolution runs race once, after ResolvePendingCreation decided
// (slot and name read, note written) and immediately before its DeleteIf —
// the narrowest window a concurrent retry can land in.
type slotsRacingResolution struct {
	versionstore.Store[string, Slot]
	race func()
	ran  bool
}

func (s *slotsRacingResolution) DeleteIf(ctx context.Context, key string, expect versionstore.Versioned[Slot], match func(Slot) bool) error {
	if !s.ran {
		s.ran = true
		s.race()
	}
	return s.Store.(versionstore.ConditionalDeleter[string, Slot]).DeleteIf(ctx, key, expect, match)
}

type lostReserveReply struct {
	directory.Directory
	lost bool
}

func (n *lostReserveReply) Reserve(ctx context.Context, name string, owner directory.Owner, ttl time.Duration) (directory.Claim, error) {
	claim, err := n.Directory.Reserve(ctx, name, owner, ttl)
	if err == nil && !n.lost {
		n.lost = true
		return directory.Claim{}, errors.New("name reserve reply lost")
	}
	return claim, err
}

func TestADifferentNameReleasesAnUnadmittedPlanWhoseNameIsReservedElsewhere(t *testing.T) {
	sink := servicemetrics.NewRecorder()
	s, clock, cfg := revn06AccountService(t, func(c *Config) {
		c.Metrics = sink
		c.Names = &lostReserveReply{Directory: c.Names}
	})
	ctx := context.Background()
	a := login(t, s, "revn09f-rename")
	if _, err := s.CreateRole(ctx, a.ID, 1, "Hero"); err == nil {
		t.Fatal("fixture must lose the reserve reply")
	}
	if slot := mustSlot(t, cfg, a.ID); slot.Creation.Name != "Hero" || slot.Creation.Admitted {
		t.Fatalf("fixture must leave an unadmitted plan for Hero: %+v", slot)
	}
	revn06Lapse(t, cfg, clock, "Hero")
	if _, err := cfg.Names.Reserve(ctx, "Hero", "rival", time.Minute); err != nil {
		t.Fatal(err)
	}

	knight, err := s.CreateRole(ctx, a.ID, 1, "Knight")
	if err != nil {
		t.Fatalf("a different name must release the unadmitted plan, got %v (slot %+v)", err, mustSlot(t, cfg, a.ID))
	}
	if knight.Name != "Knight" {
		t.Fatalf("created %+v, want Knight", knight)
	}
	if slot := mustSlot(t, cfg, a.ID); slot.PlayerID != knight.PlayerID || slot.Creation.Name != "Knight" {
		t.Fatalf("slot after the fresh plan: %+v", slot)
	}
	if entry, found, err := cfg.Names.Lookup(ctx, "Hero"); err != nil || !found || entry.Owner != "rival" {
		t.Fatalf("the rival's reservation changed: %+v found=%v err=%v", entry, found, err)
	}
	if got := sink.Count("dropped:create_role.plan_released"); got != 1 {
		t.Fatalf("plan release counted %d times; %s", got, sink.Events())
	}
}

// 维护者第七轮决定（O37，2026-10-06）：同一行的 free 格也释放。计划还没 admitted、它的名字已经
// 无人持有（预约过期或从未预约成功）时，换名请求释放 slot 并用新名字建角，与 res-else 格一致。
//
// 旧行为（free 格 = limit）：答 ErrRoleLimit、slot 保留——玩家只能先用旧名字重试一次才能换名，
// 而这个计划 admitted 之前什么都没发生，释放不会留下角色。
func TestADifferentNameReleasesAnUnadmittedPlanWhoseNameIsFree(t *testing.T) {
	sink := servicemetrics.NewRecorder()
	s, clock, cfg := revn06AccountService(t, func(c *Config) {
		c.Metrics = sink
		c.Names = &lostReserveReply{Directory: c.Names}
	})
	ctx := context.Background()
	a := login(t, s, "o37-rename-free")
	if _, err := s.CreateRole(ctx, a.ID, 1, "Hero"); err == nil {
		t.Fatal("fixture must lose the reserve reply")
	}
	if slot := mustSlot(t, cfg, a.ID); slot.Creation.Name != "Hero" || slot.Creation.Admitted {
		t.Fatalf("fixture must leave an unadmitted plan for Hero: %+v", slot)
	}
	revn06Lapse(t, cfg, clock, "Hero")
	if _, found, err := cfg.Names.Lookup(ctx, "Hero"); err != nil || found {
		t.Fatalf("fixture must leave Hero free: found=%v err=%v", found, err)
	}

	knight, err := s.CreateRole(ctx, a.ID, 1, "Knight")
	if err != nil {
		t.Fatalf("a different name must release the unadmitted plan whose name is free, got %v (slot %+v)", err, mustSlot(t, cfg, a.ID))
	}
	if knight.Name != "Knight" {
		t.Fatalf("created %+v, want Knight", knight)
	}
	if slot := mustSlot(t, cfg, a.ID); slot.PlayerID != knight.PlayerID || slot.Creation.Name != "Knight" {
		t.Fatalf("slot after the fresh plan: %+v", slot)
	}
	if _, found, err := cfg.Names.Lookup(ctx, "Hero"); err != nil || found {
		t.Fatalf("Hero must stay free: found=%v err=%v", found, err)
	}
	if got := sink.Count("dropped:create_role.plan_released"); got != 1 {
		t.Fatalf("plan release counted %d times; %s", got, sink.Events())
	}
}
