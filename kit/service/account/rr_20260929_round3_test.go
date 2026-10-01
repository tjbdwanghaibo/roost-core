package account

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/kit/service/directory"
	fredis "github.com/tjbdwanghaibo/roost-core/redis"
	driver "github.com/tjbdwanghaibo/roost-core/redis/driver"
	"github.com/tjbdwanghaibo/roost-core/security"
	"github.com/tjbdwanghaibo/roost-core/versionstore"
)

type review3FailCreate[K comparable, V any] struct {
	versionstore.Store[K, V]
	failed bool
}

func (s *review3FailCreate[K, V]) Create(ctx context.Context, k K, v V) (versionstore.Versioned[V], bool, error) {
	if !s.failed {
		s.failed = true
		return versionstore.Versioned[V]{}, false, errors.New("control: before write")
	}
	return s.Store.Create(ctx, k, v)
}

type review3FailCommit struct {
	directory.Directory
	failed bool
}

func (s *review3FailCommit) Commit(ctx context.Context, c directory.Claim) (directory.Entry, error) {
	if !s.failed {
		s.failed = true
		return directory.Entry{}, errors.New("control: before commit")
	}
	return s.Directory.Commit(ctx, c)
}
func TestReview3BeforeWriteFailuresRemainRetryable(t *testing.T) {
	for _, point := range []string{"slot", "role", "name"} {
		t.Run(point, func(t *testing.T) {
			s, _, _ := review3AccountService(t, func(c *Config) {
				switch point {
				case "slot":
					c.Slots = &review3FailCreate[string, Slot]{Store: c.Slots}
				case "role":
					c.Roles = &review3FailCreate[int64, Role]{Store: c.Roles}
				case "name":
					c.Names = &review3FailCommit{Directory: c.Names}
				}
			})
			ctx := context.Background()
			a, e := s.Login(ctx, Identity{Channel: "test", OpenID: "control", Credential: "good"})
			if e != nil {
				t.Fatal(e)
			}
			if _, e = s.CreateRole(ctx, a.ID, 1, "Hero"); e == nil {
				t.Fatal("control must reject first operation")
			}
			if _, e = s.CreateRole(ctx, a.ID, 1, "Hero"); e != nil {
				t.Fatalf("before-write failure should permit clean retry: %v", e)
			}
		})
	}
}

func review3AccountService(t *testing.T, mutate ...func(*Config)) (*Service, *clock, Config) {
	t.Helper()
	if os.Getenv("ROOST_REVIEW3_BACKEND") != "redis" {
		return newService(t, mutate...)
	}
	addr := os.Getenv("ROOST_REVIEW_REDIS")
	if addr == "" {
		t.Fatal("Redis backend requires ROOST_REVIEW_REDIS")
	}
	client, err := driver.NewClient(fredis.DefaultConfig(addr))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	stores, err := NewRedisStores(client, fmt.Sprintf("account-review3:%d", time.Now().UnixNano()), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	base := func(c *Config) {
		c.Accounts = stores.Accounts
		c.Roles = stores.Roles
		c.Servers = stores.Servers
		c.Slots = stores.Slots
		c.Names = stores.Names
	}
	return newService(t, append([]func(*Config){base}, mutate...)...)
}

type review3LostCommit struct {
	directory.Directory
	lost bool
}

func (n *review3LostCommit) Commit(ctx context.Context, c directory.Claim) (directory.Entry, error) {
	v, e := n.Directory.Commit(ctx, c)
	if e == nil && !n.lost {
		n.lost = true
		return directory.Entry{}, errors.New("name commit reply lost")
	}
	return v, e
}
func TestReview3NameCommitLostReplyCannotBurnName(t *testing.T) {
	s, _, cfg := review3AccountService(t, func(c *Config) { c.Names = &review3LostCommit{Directory: c.Names} })
	ctx := context.Background()
	a, e := s.Login(ctx, Identity{Channel: "test", OpenID: "name-lost", Credential: "good"})
	if e != nil {
		t.Fatal(e)
	}
	_, firstErr := s.CreateRole(ctx, a.ID, 1, "Hero")
	if firstErr == nil {
		t.Fatal("fixture must lose the first commit reply")
	}
	name, found, e := cfg.Names.Lookup(ctx, "Hero")
	if e != nil {
		t.Fatal(e)
	}
	slot, slotFound, e := cfg.Slots.Get(ctx, slotKeyFor(a.ID, 1))
	if e != nil {
		t.Fatal(e)
	}
	recovered, retryErr := s.CreateRole(ctx, a.ID, 1, "Hero")
	if retryErr != nil || !found || name.State != directory.StateCommitted || !slotFound || recovered.PlayerID != slot.Value.Creation.PlayerID {
		t.Fatalf("committed name burned: name=%+v slot=%+v slotFound=%v retry=%v", name, slot, slotFound, retryErr)
	}
}

type review3LostRole struct {
	versionstore.Store[int64, Role]
	lost  bool
	first int64
}

func (s *review3LostRole) Create(ctx context.Context, k int64, v Role) (versionstore.Versioned[Role], bool, error) {
	got, new, e := s.Store.Create(ctx, k, v)
	if e == nil && new && !s.lost {
		s.lost = true
		s.first = k
		return versionstore.Versioned[Role]{}, false, errors.New("role create reply lost")
	}
	return got, new, e
}
func TestReview3RoleCreateLostReplyCannotLeaveDuplicateNames(t *testing.T) {
	var roles *review3LostRole
	s, _, _ := review3AccountService(t, func(c *Config) { roles = &review3LostRole{Store: c.Roles}; c.Roles = roles })
	ctx := context.Background()
	a, e := s.Login(ctx, Identity{Channel: "test", OpenID: "role-lost", Credential: "good"})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.CreateRole(ctx, a.ID, 1, "Hero"); e == nil {
		t.Fatal("expected lost reply")
	}
	second, e := s.CreateRole(ctx, a.ID, 1, "Hero")
	if e != nil {
		t.Fatal(e)
	}
	orphan, found, e := roles.Get(ctx, roles.first)
	if e != nil {
		t.Fatal(e)
	}
	if !found || orphan.Value.PlayerID != second.PlayerID || orphan.Value.CreationID != second.CreationID {
		t.Fatalf("two persisted roles share account/server/name: first=%+v second=%+v", orphan.Value, second)
	}
}

type review3LostSlotCreate struct {
	versionstore.Store[string, Slot]
	lost bool
}

func (s *review3LostSlotCreate) Create(ctx context.Context, k string, v Slot) (versionstore.Versioned[Slot], bool, error) {
	got, new, e := s.Store.Create(ctx, k, v)
	if e == nil && new && !s.lost {
		s.lost = true
		return versionstore.Versioned[Slot]{}, false, errors.New("slot create reply lost")
	}
	return got, new, e
}
func TestReview3InitialSlotUnknownCannotPermanentlyBlockCreate(t *testing.T) {
	s, clock, cfg := review3AccountService(t, func(c *Config) { c.Slots = &review3LostSlotCreate{Store: c.Slots} })
	ctx := context.Background()
	a, e := s.Login(ctx, Identity{Channel: "test", OpenID: "slot-first", Credential: "good"})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.CreateRole(ctx, a.ID, 1, "Hero"); e == nil {
		t.Fatal("expected lost reply")
	}
	clock.advance(24 * time.Hour)
	recovered, retryErr := s.CreateRole(ctx, a.ID, 1, "Hero")
	slot, found, e := cfg.Slots.Get(ctx, slotKeyFor(a.ID, 1))
	if e != nil {
		t.Fatal(e)
	}
	if retryErr != nil || !found || slot.Value.PlayerID == 0 || slot.Value.PlayerID != recovered.PlayerID {
		t.Fatalf("empty permanent slot blocks recovered client: slot=%+v retry=%v", slot.Value, retryErr)
	}
}
func TestReview3ProfileOutputCannotMutateRoleWithoutCAS(t *testing.T) {
	for _, method := range []string{"update", "validate", "create_retry"} {
		t.Run(method, func(t *testing.T) {
			s, _, cfg := review3AccountService(t)
			ctx := context.Background()
			a, e := s.Login(ctx, Identity{Channel: "test", OpenID: "profile", Credential: "good"})
			if e != nil {
				t.Fatal(e)
			}
			role, e := s.CreateRole(ctx, a.ID, 1, "Hero")
			if e != nil {
				t.Fatal(e)
			}
			out, e := s.UpdateProfile(ctx, a.ID, role.PlayerID, []byte("safe"))
			if e != nil {
				t.Fatal(e)
			}
			if method == "validate" {
				session, e := s.SelectRole(ctx, a.ID, role.PlayerID)
				if e != nil {
					t.Fatal(e)
				}
				out, e = s.ValidateSession(ctx, role.PlayerID, session.Token)
				if e != nil {
					t.Fatal(e)
				}
			}
			if method == "create_retry" {
				out, e = s.CreateRole(ctx, a.ID, 1, "Hero")
				if e != nil {
					t.Fatal(e)
				}
			}
			before, _, e := cfg.Roles.Get(ctx, role.PlayerID)
			if e != nil {
				t.Fatal(e)
			}
			out.Profile[0] = 'X'
			after, _, e := cfg.Roles.Get(ctx, role.PlayerID)
			if e != nil {
				t.Fatal(e)
			}
			if string(after.Value.Profile) != "safe" || after.Version != before.Version {
				t.Fatalf("output mutated stored role: value=%q version=%d/%d", after.Value.Profile, before.Version, after.Version)
			}
		})
	}
}

func TestPendingRoleRecoversAcrossServiceRestart(t *testing.T) {
	var roles *review3LostRole
	s, _, cfg := review3AccountService(t, func(c *Config) { roles = &review3LostRole{Store: c.Roles}; c.Roles = roles })
	ctx := context.Background()
	a := login(t, s, "restart")
	if _, err := s.CreateRole(ctx, a.ID, 1, "Hero"); err == nil {
		t.Fatal("expected unknown Create")
	}
	// A pending role is durable recovery state, not a playable published role.
	if _, err := s.SelectRole(ctx, a.ID, roles.first); !errors.Is(err, ErrConflict) {
		t.Fatalf("pending role selected: %v", err)
	}
	token, err := security.SignSessionToken(roles.first, cfg.SessionSecret, time.Hour, cfg.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ValidateSession(ctx, roles.first, token); !errors.Is(err, ErrConflict) {
		t.Fatalf("pending token accepted: %v", err)
	}
	if _, err = s.UpdateProfile(ctx, a.ID, roles.first, []byte("premature")); !errors.Is(err, ErrConflict) {
		t.Fatalf("pending profile updated: %v", err)
	}
	restarted, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := restarted.CreateRole(ctx, a.ID, 1, "Hero")
	if err != nil || recovered.PlayerID != roles.first {
		t.Fatalf("restart lost creation identity: %+v %v", recovered, err)
	}
	if _, err = restarted.SelectRole(ctx, a.ID, recovered.PlayerID); err != nil {
		t.Fatal(err)
	}
}

func TestConcurrentCreationRetriesShareOneDurablePlan(t *testing.T) {
	s, _, cfg := review3AccountService(t, func(c *Config) { c.Slots = &review3LostSlotCreate{Store: c.Slots} })
	ctx := context.Background()
	a := login(t, s, "concurrent-retry")
	if _, err := s.CreateRole(ctx, a.ID, 1, "Hero"); err == nil {
		t.Fatal("expected lost initial slot reply")
	}
	slot, found, err := cfg.Slots.Get(ctx, slotKeyFor(a.ID, 1))
	if err != nil || !found {
		t.Fatal(slot, found, err)
	}
	var group sync.WaitGroup
	type result struct {
		role Role
		err  error
	}
	results := make(chan result, 16)
	for range 16 {
		group.Go(func() { role, err := s.CreateRole(ctx, a.ID, 1, "Hero"); results <- result{role, err} })
	}
	group.Wait()
	close(results)
	for got := range results {
		if got.err != nil || got.role.PlayerID != slot.Value.Creation.PlayerID || got.role.CreationID != slot.Value.Creation.ID {
			t.Fatalf("retry forked plan: role=%+v err=%v", got.role, got.err)
		}
	}
}

func TestLegacyEmptySlotRequiresExplicitReconciliation(t *testing.T) {
	s, _, cfg := newService(t)
	ctx := context.Background()
	a := login(t, s, "legacy-slot")
	original, _, err := cfg.Slots.Create(ctx, slotKeyFor(a.ID, 1), Slot{AccountID: a.ID, ServerID: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.CreateRole(ctx, a.ID, 1, "Hero"); !errors.Is(err, ErrConflict) {
		t.Fatalf("legacy empty slot was guessed away: %v", err)
	}
	current, found, err := cfg.Slots.Get(ctx, slotKeyFor(a.ID, 1))
	if err != nil || !found || current.Version != original.Version {
		t.Fatal(current, found, err)
	}
}

func TestAccountRequiresIdentityCheckedSlotCleanup(t *testing.T) {
	_, _, cfg := newService(t)
	cfg.Slots = struct {
		versionstore.Store[string, Slot]
	}{cfg.Slots}
	if _, err := New(cfg); err == nil {
		t.Fatal("constructor accepted unsafe slot cleanup")
	}
}

// RR-20261001-06 changed this contract: a name COMMITTED to another owner
// releases the pending slot (the plan can never commit it), while the orphan
// role the lost reply created stays unplayable. A name merely RESERVED by
// someone else still retains the proof — see
// TestNameReservedElsewhereKeepsThePendingSlot.
func TestPendingRoleNameConflictReleasesSlotButNotTheOrphanRole(t *testing.T) {
	var roles *review3LostRole
	s, clock, cfg := newService(t, func(c *Config) { roles = &review3LostRole{Store: c.Roles}; c.Roles = roles })
	ctx := context.Background()
	a := login(t, s, "name-conflict")
	if _, err := s.CreateRole(ctx, a.ID, 1, "Hero"); err == nil {
		t.Fatal("expected lost role reply")
	}
	clock.advance(time.Minute)
	rival, err := cfg.Names.Reserve(ctx, "Hero", "rival", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = cfg.Names.Commit(ctx, rival); err != nil {
		t.Fatal(err)
	}
	if _, err = s.CreateRole(ctx, a.ID, 1, "Hero"); !errors.Is(err, ErrNameTaken) {
		t.Fatal(err)
	}
	slot, found, err := cfg.Slots.Get(ctx, slotKeyFor(a.ID, 1))
	if err != nil || found {
		t.Fatalf("slot retained after its name was committed elsewhere: %+v found=%v err=%v", slot, found, err)
	}
	if _, err = s.SelectRole(ctx, a.ID, roles.first); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflicted pending role became playable: %v", err)
	}
	orphan, found, err := cfg.Roles.Get(ctx, roles.first)
	if err != nil || !found || orphan.Value.Name != "Hero" {
		t.Fatalf("orphan role record was deleted or changed: %+v found=%v err=%v", orphan, found, err)
	}
}

func (s *review3LostSlotCreate) DeleteIf(ctx context.Context, key string, expect versionstore.Versioned[Slot], match func(Slot) bool) error {
	return s.Store.(versionstore.ConditionalDeleter[string, Slot]).DeleteIf(ctx, key, expect, match)
}

func (s *review3FailCreate[K, V]) DeleteIf(ctx context.Context, key K, expect versionstore.Versioned[V], match func(V) bool) error {
	return s.Store.(versionstore.ConditionalDeleter[K, V]).DeleteIf(ctx, key, expect, match)
}
