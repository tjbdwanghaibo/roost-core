package account

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/service/directory"
	"github.com/tjbdwanghaibo/roost-core/infra/storage/versionstore"
)

func TestCommittedLegacyNameOwnerIsNeverCompensated(t *testing.T) {
	s, _, cfg := newService(t)
	ctx := context.Background()
	a, err := s.Login(ctx, Identity{Channel: "test", OpenID: "legacy-name", Credential: "good"})
	if err != nil {
		t.Fatal(err)
	}
	// Simulate an old bare account owner matching the new account/server owner.
	claim, err := cfg.Names.Reserve(ctx, "Held", directory.Owner(slotKeyFor(a.ID, 1)), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = cfg.Names.Commit(ctx, claim); err != nil {
		t.Fatal(err)
	}
	if _, err = s.CreateRole(ctx, a.ID, 1, "Held"); !errors.Is(err, ErrNameTaken) {
		t.Fatalf("existing name reused: %v", err)
	}
	if _, found, err := cfg.Names.Lookup(ctx, "Held"); !found || err != nil {
		t.Fatal(found, err)
	}
	if _, found, err := cfg.Slots.Get(ctx, slotKeyFor(a.ID, 1)); found || err != nil {
		t.Fatal(found, err)
	}
}

type unknownSlotOutcome struct {
	versionstore.Store[string, Slot]
	failed     bool
	readFailed bool
}

func (s *unknownSlotOutcome) Update(ctx context.Context, key string, m versionstore.Mutate[Slot]) (versionstore.Versioned[Slot], bool, error) {
	v, saved, err := s.Store.Update(ctx, key, m)
	if err == nil && saved && v.Value.PlayerID != 0 {
		s.failed = true
		return versionstore.Versioned[Slot]{}, false, errors.New("lost write reply")
	}
	return v, saved, err
}
func (s *unknownSlotOutcome) Get(ctx context.Context, key string) (versionstore.Versioned[Slot], bool, error) {
	if s.failed && !s.readFailed {
		s.readFailed = true
		return versionstore.Versioned[Slot]{}, false, errors.New("reconciliation unavailable")
	}
	return s.Store.Get(ctx, key)
}
func TestUnknownSlotOutcomePreservesRoleAndName(t *testing.T) {
	store := &unknownSlotOutcome{Store: versionstore.NewMemoryStore[string, Slot]()}
	s, _, cfg := newService(t, func(c *Config) { c.Slots = store })
	ctx := context.Background()
	a, err := s.Login(ctx, Identity{Channel: "test", OpenID: "unknown", Credential: "good"})
	if err != nil {
		t.Fatal(err)
	}
	role, err := s.CreateRole(ctx, a.ID, 1, "Retained")
	if err == nil || role.PlayerID == 0 {
		t.Fatalf("unknown result omitted recovery identity: %+v %v", role, err)
	}
	slot, found, err := store.Get(ctx, slotKeyFor(a.ID, 1))
	if err != nil || !found || slot.Value.PlayerID != role.PlayerID {
		t.Fatal(slot, found, err)
	}
	if _, found, err := cfg.Roles.Get(ctx, role.PlayerID); !found || err != nil {
		t.Fatal(found, err)
	}
	if _, found, err := cfg.Names.Lookup(ctx, "Retained"); !found || err != nil {
		t.Fatal(found, err)
	}
}

func TestLegacyVerifiedIdentityRetainsAccountAndRefusesCollision(t *testing.T) {
	s, _, cfg := newService(t, func(c *Config) {
		c.Verifier = VerifierFunc(func(_ context.Context, i Identity) (Verified, error) {
			if i.Credential == "first" {
				return Verified{Channel: "vendor:a", OpenID: "b"}, nil
			}
			return Verified{Channel: "vendor", OpenID: "a:b"}, nil
		})
	})
	ctx := context.Background()
	legacy := Account{ID: "vendor:a:b", Channel: "vendor:a", OpenID: "b"}
	if _, _, err := cfg.Accounts.Create(ctx, legacy.ID, legacy); err != nil {
		t.Fatal(err)
	}
	first, err := s.Login(ctx, Identity{Channel: "test", OpenID: "first", Credential: "first"})
	if err != nil || first.ID != legacy.ID {
		t.Fatal(first, err)
	}
	if _, err := s.Login(ctx, Identity{Channel: "test", OpenID: "second", Credential: "second"}); !errors.Is(err, ErrIdentityInvalid) {
		t.Fatalf("legacy collision merged: %v", err)
	}
}

func (s *unknownSlotOutcome) DeleteIf(ctx context.Context, key string, expect versionstore.Versioned[Slot], match func(Slot) bool) error {
	return s.Store.(versionstore.ConditionalDeleter[string, Slot]).DeleteIf(ctx, key, expect, match)
}
