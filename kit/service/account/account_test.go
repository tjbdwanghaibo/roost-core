package account

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/security"
	"github.com/tjbdwanghaibo/roost-core/versionstore"

	"github.com/tjbdwanghaibo/roost-kit/service/directory"
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

// durableAllocator mints ids from a shared counter, standing in for a real
// durable allocator. Note what it is not: a per-process counter starting at
// one, which is what the implementation this replaces defaulted to.
type durableAllocator struct{ next atomic.Int64 }

func (a *durableAllocator) Allocate(context.Context, int32) (int64, error) {
	return a.next.Add(1) + 1_000_000, nil
}

func acceptingVerifier() IdentityVerifier {
	return VerifierFunc(func(_ context.Context, identity Identity) (Verified, error) {
		if identity.Credential != "good" {
			return Verified{}, fmt.Errorf("bad credential")
		}
		return Verified{Channel: identity.Channel, OpenID: identity.OpenID}, nil
	})
}

func simpleNameRules() NameValidator {
	return NameValidatorFunc(func(raw string) error {
		trimmed := strings.TrimSpace(raw)
		if len(trimmed) < 2 || len(trimmed) > 16 {
			return fmt.Errorf("name must be 2-16 characters")
		}
		return nil
	})
}

func newService(t *testing.T, mutate ...func(*Config)) (*Service, *clock, Config) {
	t.Helper()
	c := &clock{now: time.Unix(1_700_000_000, 0)}
	names, err := directory.New(versionstore.NewMemoryStore[string, directory.Entry](), directory.Config{
		Normalize: directory.NormalizeLower, DefaultTTL: time.Minute, Now: c.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		Accounts:      versionstore.NewMemoryStore[string, Account](),
		Roles:         versionstore.NewMemoryStore[int64, Role](),
		Servers:       versionstore.NewMemoryStore[int32, GameServer](),
		Names:         names,
		Slots:         versionstore.NewMemoryStore[string, Slot](),
		Verifier:      acceptingVerifier(),
		Allocator:     &durableAllocator{},
		NameRules:     simpleNameRules(),
		SessionSecret: "test-secret-please-rotate",
		Now:           c.Now,
	}
	for _, m := range mutate {
		m(&cfg)
	}
	service, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.UpsertServer(context.Background(), GameServer{ID: 1, Name: "s1", Status: ServerOpen}); err != nil {
		t.Fatal(err)
	}
	return service, c, cfg
}

// The configuration must refuse to start without the pieces whose absence was
// a vulnerability rather than an inconvenience.
func TestNewRefusesAConfigThatCannotAuthenticate(t *testing.T) {
	base := func() Config {
		c := &clock{now: time.Unix(1, 0)}
		names, _ := directory.New(versionstore.NewMemoryStore[string, directory.Entry](), directory.Config{
			Normalize: directory.NormalizeLower, DefaultTTL: time.Minute, Now: c.Now})
		return Config{
			Accounts: versionstore.NewMemoryStore[string, Account](),
			Roles:    versionstore.NewMemoryStore[int64, Role](),
			Servers:  versionstore.NewMemoryStore[int32, GameServer](),
			Names:    names, Slots: versionstore.NewMemoryStore[string, Slot](),
			Verifier: acceptingVerifier(), Allocator: &durableAllocator{},
			NameRules: simpleNameRules(), SessionSecret: "secret",
		}
	}
	for _, testCase := range []struct {
		label  string
		mutate func(*Config)
		want   string
	}{
		{"no verifier", func(c *Config) { c.Verifier = nil }, "Verifier"},
		{"no allocator", func(c *Config) { c.Allocator = nil }, "Allocator"},
		{"no name rules", func(c *Config) { c.NameRules = nil }, "NameRules"},
		{"no session secret", func(c *Config) { c.SessionSecret = "  " }, "SessionSecret"},
		{"no name directory", func(c *Config) { c.Names = nil }, "Names"},
		{"no slot directory", func(c *Config) { c.Slots = nil }, "Slots"},
		{"no role store", func(c *Config) { c.Roles = nil }, "Roles"},
	} {
		cfg := base()
		testCase.mutate(&cfg)
		_, err := New(cfg)
		if err == nil {
			t.Fatalf("%s: the configuration was accepted", testCase.label)
		}
		if !strings.Contains(err.Error(), testCase.want) {
			t.Fatalf("%s: error %q does not name the missing piece %q", testCase.label, err, testCase.want)
		}
	}
	// An option that is not implemented must be refused, not silently
	// downgraded — accepting it and enforcing something else is how a
	// configuration option becomes a lie.
	cfg := base()
	cfg.RolesPerServer = 3
	if _, err := New(cfg); err == nil {
		t.Fatal("RolesPerServer above one was accepted")
	}
}

// Login authenticates. The implementation this replaces accepted a channel and
// an open id and returned the account, which meant anyone reaching the
// endpoint could obtain a session for any player.
func TestLoginRequiresAVerifiedIdentity(t *testing.T) {
	service, _, _ := newService(t)
	ctx := context.Background()

	if _, err := service.Login(ctx, Identity{Channel: "store", OpenID: "u1", Credential: "forged"}); !errors.Is(err, ErrIdentityDenied) {
		t.Fatalf("a bad credential was accepted: %v", err)
	}
	account, err := service.Login(ctx, Identity{Channel: "store", OpenID: "u1", Credential: "good"})
	if err != nil {
		t.Fatal(err)
	}
	if account.ID != "store:u1" {
		t.Fatalf("account id = %q", account.ID)
	}
	// Repeat login is idempotent and does not create a second account.
	again, err := service.Login(ctx, Identity{Channel: "store", OpenID: "u1", Credential: "good"})
	if err != nil {
		t.Fatal(err)
	}
	if again.ID != account.ID || again.CreatedAtUnix != account.CreatedAtUnix {
		t.Fatalf("a repeat login created a different account: %+v vs %+v", again, account)
	}
	// Missing fields are a client error, not an internal one.
	for _, identity := range []Identity{
		{OpenID: "u1", Credential: "good"},
		{Channel: "store", Credential: "good"},
		{Channel: "  ", OpenID: "u1", Credential: "good"},
	} {
		if _, err := service.Login(ctx, identity); !errors.Is(err, ErrIdentityInvalid) {
			t.Fatalf("identity %+v returned %v, want ErrIdentityInvalid", identity, err)
		}
	}
}

// The account is derived from what the channel confirms, not from what the
// caller submitted, so a caller cannot claim another player's identifier by
// presenting its own credential.
func TestAccountIsDerivedFromTheVerifiedIdentity(t *testing.T) {
	service, _, _ := newService(t, func(cfg *Config) {
		cfg.Verifier = VerifierFunc(func(_ context.Context, identity Identity) (Verified, error) {
			// The channel says this credential belongs to someone else.
			return Verified{Channel: identity.Channel, OpenID: "the-real-owner"}, nil
		})
	})
	account, err := service.Login(context.Background(), Identity{Channel: "store", OpenID: "impersonated", Credential: "good"})
	if err != nil {
		t.Fatal(err)
	}
	if account.ID != "store:the-real-owner" {
		t.Fatalf("account id = %q; the submitted open id must not decide the account", account.ID)
	}
}

// A channel outage must not read as a denied credential: they need different
// responses, and telling a player their credential is bad when the channel is
// down is both wrong and unactionable.
func TestChannelOutageIsDistinctFromDenial(t *testing.T) {
	service, _, _ := newService(t, func(cfg *Config) {
		cfg.Verifier = VerifierFunc(func(context.Context, Identity) (Verified, error) {
			return Verified{}, ErrVerifierUnavailable
		})
	})
	_, loginErr := service.Login(context.Background(), Identity{Channel: "store", OpenID: "u1", Credential: "good"})
	if !errors.Is(loginErr, ErrVerifierUnavailable) {
		t.Fatalf("a channel outage returned %v, want ErrVerifierUnavailable", loginErr)
	}
	if errors.Is(loginErr, ErrIdentityDenied) {
		t.Fatal("a channel outage was reported as a denied identity")
	}
	// A verifier that returns an empty identity is a broken verifier, not a
	// successful login.
	broken, _, _ := newService(t, func(cfg *Config) {
		cfg.Verifier = VerifierFunc(func(context.Context, Identity) (Verified, error) {
			return Verified{}, nil
		})
	})
	if _, err := broken.Login(context.Background(), Identity{Channel: "store", OpenID: "u1", Credential: "good"}); !errors.Is(err, ErrIdentityInvalid) {
		t.Fatalf("an empty verified identity was accepted: %v", err)
	}
}

func login(t *testing.T, s *Service, openID string) Account {
	t.Helper()
	account, err := s.Login(context.Background(), Identity{Channel: "store", OpenID: openID, Credential: "good"})
	if err != nil {
		t.Fatal(err)
	}
	return account
}

// One role per account per server, enforced as an exclusive claim rather than
// a counted check two callers can both pass.
func TestOneRolePerAccountPerServer(t *testing.T) {
	service, _, _ := newService(t)
	ctx := context.Background()
	account := login(t, service, "u1")

	if _, err := service.CreateRole(ctx, account.ID, 1, "Alice"); err != nil {
		t.Fatal(err)
	}
	if _, err := service.CreateRole(ctx, account.ID, 1, "Bob"); !errors.Is(err, ErrRoleLimit) {
		t.Fatalf("a second role on the same server returned %v, want ErrRoleLimit", err)
	}
	// A different server is a different slot.
	if _, err := service.UpsertServer(ctx, GameServer{ID: 2, Status: ServerOpen}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.CreateRole(ctx, account.ID, 2, "Carol"); err != nil {
		t.Fatalf("a role on another server was refused: %v", err)
	}
}

// Names are globally unique on their normalized form.
func TestRoleNamesAreUniqueCaseInsensitively(t *testing.T) {
	service, _, _ := newService(t)
	ctx := context.Background()
	first := login(t, service, "u1")
	second := login(t, service, "u2")

	if _, err := service.CreateRole(ctx, first.ID, 1, "Alice"); err != nil {
		t.Fatal(err)
	}
	for _, variant := range []string{"alice", "ALICE", "  Alice "} {
		if _, err := service.CreateRole(ctx, second.ID, 1, variant); !errors.Is(err, ErrNameTaken) {
			t.Fatalf("name %q was accepted, want ErrNameTaken (got %v)", variant, err)
		}
	}
	// And losing the name must not consume the loser's server slot.
	if _, err := service.CreateRole(ctx, second.ID, 1, "Bob"); err != nil {
		t.Fatalf("the slot was consumed by a failed create: %v", err)
	}
}

// Role creation is insert-only. An allocator that returns a used id fails
// rather than overwriting the player who holds it — the defect that destroyed
// an existing player's record after a restart.
func TestCreateRoleNeverOverwritesAnExistingPlayer(t *testing.T) {
	fixed := int64(1_000_042)
	service, _, cfg := newService(t, func(cfg *Config) {
		cfg.Allocator = AllocatorFunc(func(context.Context, int32) (int64, error) { return fixed, nil })
	})
	ctx := context.Background()
	first := login(t, service, "u1")
	second := login(t, service, "u2")

	original, err := service.CreateRole(ctx, first.ID, 1, "Alice")
	if err != nil {
		t.Fatal(err)
	}
	if original.PlayerID != fixed {
		t.Fatalf("player id = %d", original.PlayerID)
	}
	_, err = service.CreateRole(ctx, second.ID, 1, "Bob")
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("a colliding player id returned %v, want ErrConflict", err)
	}
	// The original player is untouched.
	stored, found, err := cfg.Roles.Get(ctx, fixed)
	if err != nil || !found {
		t.Fatalf("role read: found=%v err=%v", found, err)
	}
	if stored.Value.AccountID != first.ID || stored.Value.Name != "Alice" {
		t.Fatalf("the original role was overwritten: %+v", stored.Value)
	}
	// An allocator returning zero is a broken allocator, not a valid id.
	zeroed, _, _ := newService(t, func(cfg *Config) {
		cfg.Allocator = AllocatorFunc(func(context.Context, int32) (int64, error) { return 0, nil })
	})
	third := login(t, zeroed, "u3")
	if _, err := zeroed.CreateRole(ctx, third.ID, 1, "Dave"); err == nil {
		t.Fatal("a zero player id was accepted")
	}
}

// A crash between reserving the name and writing the role must not burn the
// name. Simulated by an allocator that fails after the claims are taken.
func TestAFailedCreateReleasesItsClaims(t *testing.T) {
	failing := true
	allocator := &durableAllocator{}
	service, _, _ := newService(t, func(cfg *Config) {
		cfg.Allocator = AllocatorFunc(func(ctx context.Context, serverID int32) (int64, error) {
			if failing {
				return 0, fmt.Errorf("allocator is down")
			}
			return allocator.Allocate(ctx, serverID)
		})
	})
	ctx := context.Background()
	account := login(t, service, "u1")

	if _, err := service.CreateRole(ctx, account.ID, 1, "Alice"); err == nil {
		t.Fatal("the create succeeded despite a failing allocator")
	}
	failing = false
	// Both the name and the slot must be free again.
	role, err := service.CreateRole(ctx, account.ID, 1, "Alice")
	if err != nil {
		t.Fatalf("the failed create burned the name or the slot: %v", err)
	}
	if role.Name != "Alice" {
		t.Fatalf("role = %+v", role)
	}
}

// Even if the rollback itself fails, the claim expires — the property the
// two-phase claim exists for, and the one the two-collection write it replaces
// did not have.
func TestAnUnreleasedClaimLapsesInsteadOfBurningTheName(t *testing.T) {
	service, c, cfg := newService(t, func(cfg *Config) {
		cfg.ClaimTTL = 10 * time.Second
		cfg.Allocator = AllocatorFunc(func(context.Context, int32) (int64, error) {
			return 0, fmt.Errorf("allocator is down")
		})
	})
	ctx := context.Background()
	account := login(t, service, "u1")
	if _, err := service.CreateRole(ctx, account.ID, 1, "Alice"); err == nil {
		t.Fatal("the create succeeded")
	}
	// Re-reserve the name outside the service to simulate a rollback that
	// never happened, then let it lapse.
	if _, err := cfg.Names.Reserve(ctx, "Alice", "someone-who-died", 10*time.Second); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := cfg.Names.Lookup(ctx, "Alice"); !ok {
		t.Fatal("the reservation is not visible")
	}
	c.advance(11 * time.Second)
	if _, ok, _ := cfg.Names.Lookup(ctx, "Alice"); ok {
		t.Fatal("an abandoned reservation did not lapse; the name is burned")
	}
}

// Selecting a role signs the token before persisting the login stamp. The
// implementation this replaces persisted first, so a signing failure left the
// write durable and every retry bumped the version again.
func TestSelectRoleIssuesAVerifiableTokenForOwnedRolesOnly(t *testing.T) {
	service, c, cfg := newService(t)
	ctx := context.Background()
	owner := login(t, service, "u1")
	other := login(t, service, "u2")
	role, err := service.CreateRole(ctx, owner.ID, 1, "Alice")
	if err != nil {
		t.Fatal(err)
	}

	session, err := service.SelectRole(ctx, owner.ID, role.PlayerID)
	if err != nil {
		t.Fatal(err)
	}
	// The token verifies independently, against core's verifier.
	if _, err := security.VerifySessionToken(session.Token, cfg.SessionSecret, role.PlayerID, c.Now()); err != nil {
		t.Fatalf("the issued token does not verify: %v", err)
	}
	// It must not verify for another player.
	if _, err := security.VerifySessionToken(session.Token, cfg.SessionSecret, role.PlayerID+1, c.Now()); err == nil {
		t.Fatal("the token verifies for a different player")
	}
	// Another account cannot select this role.
	if _, err := service.SelectRole(ctx, other.ID, role.PlayerID); !errors.Is(err, ErrNotPermitted) {
		t.Fatalf("another account selected the role: %v", err)
	}
	if _, err := service.SelectRole(ctx, owner.ID, 999); !errors.Is(err, ErrRoleMissing) {
		t.Fatal("selecting a missing role did not report not-found")
	}
	// The login stamp landed.
	stored, _, _ := cfg.Roles.Get(ctx, role.PlayerID)
	if stored.Value.LastLoginAtUnix != c.Now().Unix() {
		t.Fatalf("the login stamp was not persisted: %+v", stored.Value)
	}
}

// A signing failure must not leave the login stamp written.
//
// core refuses to sign player id zero, so the one way to make signing fail for
// a role that exists and is owned is to seed such a role directly in the store.
// The previous version of this test selected player id zero WITHOUT seeding it:
// that failed at the role lookup, before signing was ever attempted, and then
// checked an unrelated role — an assertion that could not fail under either
// ordering. Reversing sign and persist in SelectRole left it green.
func TestSelectRoleDoesNotPersistWhenSigningFails(t *testing.T) {
	service, _, cfg := newService(t)
	ctx := context.Background()
	owner := login(t, service, "u1")
	seeded, created, err := cfg.Roles.Create(ctx, 0, Role{PlayerID: 0, AccountID: owner.ID, ServerID: 1, Name: "Zero"})
	if err != nil || !created {
		t.Fatalf("seed role: created=%v err=%v", created, err)
	}
	if _, err := service.SelectRole(ctx, owner.ID, 0); err == nil {
		t.Fatal("selecting a role core cannot sign succeeded")
	}
	after, found, err := cfg.Roles.Get(ctx, 0)
	if err != nil || !found {
		t.Fatalf("role read: found=%v err=%v", found, err)
	}
	if after.Version != seeded.Version || after.Value.LastLoginAtUnix != 0 {
		t.Fatalf("a failed signing persisted the login stamp: version %d -> %d, last_login=%d",
			seeded.Version, after.Version, after.Value.LastLoginAtUnix)
	}
}

func TestValidateSessionRejectsForgedAndExpiredTokens(t *testing.T) {
	service, c, cfg := newService(t, func(cfg *Config) { cfg.SessionTTL = time.Minute })
	ctx := context.Background()
	owner := login(t, service, "u1")
	role, err := service.CreateRole(ctx, owner.ID, 1, "Alice")
	if err != nil {
		t.Fatal(err)
	}
	session, err := service.SelectRole(ctx, owner.ID, role.PlayerID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.ValidateSession(ctx, role.PlayerID, session.Token); err != nil {
		t.Fatalf("a fresh token was rejected: %v", err)
	}
	for _, token := range []string{"", "garbage", session.Token + "x"} {
		if _, err := service.ValidateSession(ctx, role.PlayerID, token); !errors.Is(err, ErrSessionInvalid) {
			t.Fatalf("token %q returned %v, want ErrSessionInvalid", token, err)
		}
	}
	// A token signed with a different secret must not verify.
	foreign, err := security.SignSessionToken(role.PlayerID, "another-secret", time.Minute, c.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.ValidateSession(ctx, role.PlayerID, foreign); !errors.Is(err, ErrSessionInvalid) {
		t.Fatal("a token signed with another secret verified")
	}
	c.advance(61 * time.Second)
	if _, err := service.ValidateSession(ctx, role.PlayerID, session.Token); !errors.Is(err, ErrSessionInvalid) {
		t.Fatal("an expired token verified")
	}
	_ = cfg
}

// UpdateProfile can change the profile and nothing else. The implementation
// this replaces had one merge-upsert that could reparent a role to another
// account and rename it while orphaning the old name reservation.
func TestUpdateProfileCannotChangeIdentityOrName(t *testing.T) {
	service, _, cfg := newService(t)
	ctx := context.Background()
	owner := login(t, service, "u1")
	other := login(t, service, "u2")
	role, err := service.CreateRole(ctx, owner.ID, 1, "Alice")
	if err != nil {
		t.Fatal(err)
	}

	updated, err := service.UpdateProfile(ctx, owner.ID, role.PlayerID, []byte(`{"level":7}`))
	if err != nil {
		t.Fatal(err)
	}
	if string(updated.Profile) != `{"level":7}` {
		t.Fatalf("profile = %q", updated.Profile)
	}
	if updated.AccountID != owner.ID || updated.Name != "Alice" || updated.ServerID != 1 {
		t.Fatalf("identity fields changed: %+v", updated)
	}
	// Another account cannot write the profile.
	if _, err := service.UpdateProfile(ctx, other.ID, role.PlayerID, []byte(`{"level":99}`)); !errors.Is(err, ErrNotPermitted) {
		t.Fatalf("another account wrote the profile: %v", err)
	}
	stored, _, _ := cfg.Roles.Get(ctx, role.PlayerID)
	if string(stored.Value.Profile) != `{"level":7}` {
		t.Fatalf("the refused write landed: %q", stored.Value.Profile)
	}
	// The profile is bounded, and the refusal is a size error the caller can
	// act on — not ErrConflict, which a client reads as "retry". Accepting any
	// non-nil error here is what let the wrong code ship.
	if _, err := service.UpdateProfile(ctx, owner.ID, role.PlayerID, make([]byte, MaxProfileBytes+1)); !errors.Is(err, ErrRangeInvalid) {
		t.Fatalf("an oversized profile returned %v, want ErrRangeInvalid", err)
	}
	// And the stored profile is a copy, not the caller's slice.
	payload := []byte(`{"level":8}`)
	if _, err := service.UpdateProfile(ctx, owner.ID, role.PlayerID, payload); err != nil {
		t.Fatal(err)
	}
	payload[2] = 'X'
	stored, _, _ = cfg.Roles.Get(ctx, role.PlayerID)
	if string(stored.Value.Profile) != `{"level":8}` {
		t.Fatalf("the store aliases the caller's slice: %q", stored.Value.Profile)
	}
}

func TestCreateRoleRequiresAnOpenServerAndAKnownAccount(t *testing.T) {
	service, _, _ := newService(t)
	ctx := context.Background()
	account := login(t, service, "u1")

	if _, err := service.CreateRole(ctx, "store:nobody", 1, "Alice"); !errors.Is(err, ErrAccountMissing) {
		t.Fatalf("an unknown account was accepted: %v", err)
	}
	if _, err := service.CreateRole(ctx, account.ID, 99, "Alice"); !errors.Is(err, ErrServerInvalid) {
		t.Fatalf("an unknown server was accepted: %v", err)
	}
	for _, status := range []ServerStatus{ServerMaintenance, ServerFull, ServerClosed} {
		if _, err := service.UpsertServer(ctx, GameServer{ID: 5, Status: status}); err != nil {
			t.Fatal(err)
		}
		if _, err := service.CreateRole(ctx, account.ID, 5, "Alice"); !errors.Is(err, ErrServerClosed) {
			t.Fatalf("status %s accepted a new role: %v", status, err)
		}
	}
	if _, err := service.CreateRole(ctx, account.ID, 1, "x"); !errors.Is(err, ErrNameInvalid) {
		t.Fatalf("the name rules were not applied: %v", err)
	}
}

// A banned account cannot log in or create roles.
func TestBannedAccountsAreRefused(t *testing.T) {
	service, _, cfg := newService(t)
	ctx := context.Background()
	account := login(t, service, "u1")

	if _, _, err := cfg.Accounts.Update(ctx, account.ID, func(current Account, _ bool) (Account, bool, error) {
		current.Banned = true
		return current, true, nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Login(ctx, Identity{Channel: "store", OpenID: "u1", Credential: "good"}); !errors.Is(err, ErrIdentityDenied) {
		t.Fatalf("a banned account logged in: %v", err)
	}
	if _, err := service.CreateRole(ctx, account.ID, 1, "Alice"); !errors.Is(err, ErrIdentityDenied) {
		t.Fatalf("a banned account created a role: %v", err)
	}
}

// Concurrent creates for one account on one server must produce one role.
func TestConcurrentCreatesProduceOneRolePerSlot(t *testing.T) {
	service, _, _ := newService(t)
	ctx := context.Background()
	account := login(t, service, "u1")

	const racers = 12
	var wait sync.WaitGroup
	var mu sync.Mutex
	created := []int64{}
	refused := 0
	for i := 0; i < racers; i++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			role, err := service.CreateRole(ctx, account.ID, 1, fmt.Sprintf("Name%02d", index))
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				created = append(created, role.PlayerID)
			case errors.Is(err, ErrRoleLimit), errors.Is(err, ErrNameTaken), errors.Is(err, ErrConflict):
				refused++
			default:
				t.Errorf("unexpected error %v", err)
			}
		}(i)
	}
	wait.Wait()
	if len(created) != 1 {
		t.Fatalf("%d roles were created for one slot, want 1: %v", len(created), created)
	}
	if refused != racers-1 {
		t.Fatalf("%d creates were refused, want %d", refused, racers-1)
	}
}

// Concurrent creates racing for one name must produce one winner.
func TestConcurrentCreatesRacingOneNameHaveOneWinner(t *testing.T) {
	service, _, _ := newService(t)
	ctx := context.Background()

	const racers = 8
	accounts := make([]string, 0, racers)
	for i := 0; i < racers; i++ {
		accounts = append(accounts, login(t, service, fmt.Sprintf("u%d", i)).ID)
	}
	var wait sync.WaitGroup
	var mu sync.Mutex
	won := 0
	for _, accountID := range accounts {
		wait.Add(1)
		go func(id string) {
			defer wait.Done()
			_, err := service.CreateRole(ctx, id, 1, "Contested")
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				won++
			}
		}(accountID)
	}
	wait.Wait()
	if won != 1 {
		t.Fatalf("%d accounts got the same name, want 1", won)
	}
}

func TestMarkLogoutRequiresOwnership(t *testing.T) {
	service, c, cfg := newService(t)
	ctx := context.Background()
	owner := login(t, service, "u1")
	other := login(t, service, "u2")
	role, err := service.CreateRole(ctx, owner.ID, 1, "Alice")
	if err != nil {
		t.Fatal(err)
	}
	if err := service.MarkLogout(ctx, other.ID, role.PlayerID); !errors.Is(err, ErrNotPermitted) {
		t.Fatalf("another account marked logout: %v", err)
	}
	if err := service.MarkLogout(ctx, owner.ID, role.PlayerID); err != nil {
		t.Fatal(err)
	}
	stored, _, _ := cfg.Roles.Get(ctx, role.PlayerID)
	if stored.Value.LastLogoutAtUnix != c.Now().Unix() {
		t.Fatalf("the logout stamp was not written: %+v", stored.Value)
	}
	if err := service.MarkLogout(ctx, owner.ID, 999); !errors.Is(err, ErrRoleMissing) {
		t.Fatal("marking logout on a missing role did not report not-found")
	}
}

// A refused authority check is the signal this package added: the
// implementation it replaces took the account id from the request, so there
// was nothing to refuse and nothing to count. A counter nothing reaches is
// worth no more than the comment describing it, so the reports are asserted.
func TestAuthorityRefusalsAndAcceptancesAreReported(t *testing.T) {
	sink := servicemetrics.NewRecorder()
	service, _, _ := newService(t, func(cfg *Config) { cfg.Metrics = sink })
	ctx := context.Background()

	account, err := service.Login(ctx, Identity{Channel: "guest", OpenID: "open-1", Credential: "good"})
	if err != nil {
		t.Fatal(err)
	}
	if got := sink.Count("accepted:login"); got != 1 {
		t.Fatalf("a login reported %d accepts; %s", got, sink.Events())
	}

	role, err := service.CreateRole(ctx, account.ID, 1, "Alice")
	if err != nil {
		t.Fatal(err)
	}
	if got := sink.Count("accepted:create_role"); got != 1 {
		t.Fatalf("a create reported %d accepts; %s", got, sink.Events())
	}

	// The one-role-per-server rule, refused with a reason rather than a bare
	// error an aggregator cannot group.
	if _, err := service.CreateRole(ctx, account.ID, 1, "Alicia"); !errors.Is(err, ErrRoleLimit) {
		t.Fatalf("a second role on one server was created: %v", err)
	}
	if got := sink.Count("refused:create_role:role_limit"); got != 1 {
		t.Fatalf("a refused create reported %d refusals; %s", got, sink.Events())
	}

	// A name another account holds.
	other, err := service.Login(ctx, Identity{Channel: "guest", OpenID: "open-2", Credential: "good"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.CreateRole(ctx, other.ID, 1, "alice"); !errors.Is(err, ErrNameTaken) {
		t.Fatalf("a taken name was accepted: %v", err)
	}
	if got := sink.Count("refused:create_role:name_taken"); got != 1 {
		t.Fatalf("a taken name reported %d refusals; %s", got, sink.Events())
	}

	// Asking for someone else's role. This is the probe an operator needs to
	// see, and the implementation this replaces could not distinguish it from
	// a normal request.
	if _, err := service.SelectRole(ctx, other.ID, role.PlayerID); !errors.Is(err, ErrNotPermitted) {
		t.Fatalf("another account selected a role it does not own: %v", err)
	}
	if got := sink.Count("refused:select_role:not_owner"); got != 1 {
		t.Fatalf("a foreign select reported %d refusals; %s", got, sink.Events())
	}
	if _, err := service.UpdateProfile(ctx, other.ID, role.PlayerID, []byte("x")); !errors.Is(err, ErrNotPermitted) {
		t.Fatalf("another account wrote a profile it does not own: %v", err)
	}
	if got := sink.Count("refused:update_profile:not_owner"); got != 1 {
		t.Fatalf("a foreign profile write reported %d refusals; %s", got, sink.Events())
	}

	// A forged session token.
	if _, err := service.ValidateSession(ctx, role.PlayerID, "not-a-token"); !errors.Is(err, ErrSessionInvalid) {
		t.Fatalf("a forged token validated: %v", err)
	}
	if got := sink.Count("refused:validate_session:bad_token"); got != 1 {
		t.Fatalf("a forged token reported %d refusals; %s", got, sink.Events())
	}
}

// A nil reporter must never change behaviour.
func TestANilReporterChangesNothing(t *testing.T) {
	service, _, _ := newService(t)
	ctx := context.Background()
	account, err := service.Login(ctx, Identity{Channel: "guest", OpenID: "open-1", Credential: "good"})
	if err != nil {
		t.Fatalf("a service with no reporter failed to log in: %v", err)
	}
	if _, err := service.CreateRole(ctx, account.ID, 1, "Alice"); err != nil {
		t.Fatal(err)
	}
}
