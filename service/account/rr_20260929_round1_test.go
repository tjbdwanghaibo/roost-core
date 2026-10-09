package account

import (
	"context"
	"fmt"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/infra/storage/versionstore"
)

type reviewFailSlot struct {
	versionstore.Store[string, Slot]
	fail bool
}

func (s *reviewFailSlot) Update(ctx context.Context, k string, m versionstore.Mutate[Slot]) (versionstore.Versioned[Slot], bool, error) {
	if s.fail {
		return versionstore.Versioned[Slot]{}, false, fmt.Errorf("injected slot write failure")
	}
	return s.Store.Update(ctx, k, m)
}
func TestReviewCrossServerRoleRollbackKeepsExistingName(t *testing.T) {
	slots := &reviewFailSlot{Store: versionstore.NewMemoryStore[string, Slot]()}
	s, _, cfg := newService(t, func(c *Config) { c.Slots = slots })
	ctx := context.Background()
	a, err := s.Login(ctx, Identity{Channel: "test", OpenID: "same", Credential: "good"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.UpsertServer(ctx, GameServer{ID: 2, Status: ServerOpen}); err != nil {
		t.Fatal(err)
	}
	r, err := s.CreateRole(ctx, a.ID, 1, "Hero")
	if err != nil {
		t.Fatal(err)
	}
	slots.fail = true
	if _, err = s.CreateRole(ctx, a.ID, 2, "Hero"); err == nil {
		t.Fatal("expected failure")
	}
	if _, found, err := cfg.Roles.Get(ctx, r.PlayerID); err != nil || !found {
		t.Fatal("original role missing", err)
	}
	if _, found, err := cfg.Names.Lookup(ctx, "Hero"); err != nil || !found {
		t.Fatalf("second creation rollback removed first role name: found=%v err=%v", found, err)
	}
}

func (s *reviewFailSlot) DeleteIf(ctx context.Context, key string, expect versionstore.Versioned[Slot], match func(Slot) bool) error {
	return s.Store.(versionstore.ConditionalDeleter[string, Slot]).DeleteIf(ctx, key, expect, match)
}
