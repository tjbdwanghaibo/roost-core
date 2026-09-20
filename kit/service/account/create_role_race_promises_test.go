package account

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/versionstore"
)

// slotsVanishingOnce makes the slot row disappear exactly when CreateRole
// comes back to record the player on it — the row was deleted between the
// reservation and the final write.
type slotsVanishingOnce struct {
	versionstore.Store[string, Slot]
	vanished bool
}

func (s *slotsVanishingOnce) Update(ctx context.Context, key string, mutate versionstore.Mutate[Slot]) (versionstore.Versioned[Slot], bool, error) {
	if !s.vanished {
		s.vanished = true
		_, _, err := mutate(Slot{}, false)
		return versionstore.Versioned[Slot]{}, false, err
	}
	return s.Store.Update(ctx, key, mutate)
}

// U-0098 (C2): the three commit-tail refusals of CreateRole — a slot that
// vanished before the final write, an allocated player id already in use, and
// an allocator handing out zero — each surface as an error, and each leaves
// nothing behind: the same account retries and succeeds with the same name.
func TestCreateRoleRefusesEachCommitTailAnomalyAndLeavesNothingBehind(t *testing.T) {
	const fixedID = int64(4242)
	cases := []struct {
		name    string
		mutate  func(*Config)
		seed    func(*testing.T, Config)
		want    error
		message string
	}{
		{"slot vanished before the final write", func(cfg *Config) { cfg.Slots = &slotsVanishingOnce{Store: cfg.Slots} }, nil,
			ErrConflict, "vanished during create"},
		{"player id already in use", nil, func(t *testing.T, cfg Config) {
			if _, created, err := cfg.Roles.Create(context.Background(), fixedID, Role{PlayerID: fixedID, AccountID: "someone-else", ServerID: 1, Name: "Bob"}); err != nil || !created {
				t.Fatalf("seed role: created=%v err=%v", created, err)
			}
		}, ErrConflict, "player id 4242 is already in use"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			allocations := 0
			service, _, cfg := newService(t, func(cfg *Config) {
				if tc.mutate != nil {
					tc.mutate(cfg)
				}
				cfg.Allocator = AllocatorFunc(func(context.Context, int32) (int64, error) {
					allocations++
					return fixedID + int64(allocations) - 1, nil
				})
			})
			if tc.seed != nil {
				tc.seed(t, cfg)
			}
			owner := login(t, service, "u1")
			_, err := service.CreateRole(ctx, owner.ID, 1, "Alice")
			if !errors.Is(err, tc.want) || !strings.Contains(err.Error(), tc.message) {
				t.Fatalf("CreateRole = %v; want %v containing %q", err, tc.want, tc.message)
			}
			if tc.seed == nil {
				if _, found, _ := cfg.Roles.Get(ctx, fixedID); found {
					t.Fatal("the role record survived a failed create")
				}
			}
			// The retry is a clean first attempt: slot free, name free.
			role, err := service.CreateRole(ctx, owner.ID, 1, "Alice")
			if err != nil {
				t.Fatalf("retry after a rolled-back create failed: %v", err)
			}
			if role.Name != "Alice" || role.AccountID != owner.ID {
				t.Fatalf("retry produced %+v", role)
			}
		})
	}
}

// An allocator that returns zero is refused before any role is written; zero
// is the "no player" value everywhere else in the system.
func TestCreateRoleRefusesAZeroPlayerIDAndRollsBack(t *testing.T) {
	ctx := context.Background()
	calls := 0
	service, _, cfg := newService(t, func(cfg *Config) {
		cfg.Allocator = AllocatorFunc(func(context.Context, int32) (int64, error) {
			calls++
			if calls == 1 {
				return 0, nil
			}
			return 7000 + int64(calls), nil
		})
	})
	owner := login(t, service, "u1")
	if _, err := service.CreateRole(ctx, owner.ID, 1, "Alice"); err == nil || !strings.Contains(err.Error(), "allocator returned a zero player id") {
		t.Fatalf("CreateRole with a zero player id = %v", err)
	}
	if _, found, _ := cfg.Roles.Get(ctx, 0); found {
		t.Fatal("a role was recorded under player id 0")
	}
	if _, err := service.CreateRole(ctx, owner.ID, 1, "Alice"); err != nil {
		t.Fatalf("retry after the zero id failed: %v", err)
	}
}
