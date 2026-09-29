package account

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/versionstore"

	"github.com/tjbdwanghaibo/roost-core/kit/service/directory"
	"github.com/tjbdwanghaibo/roost-core/kit/service/servicemetrics"
)

// namesFailingCommitOnce is a directory whose first Commit is lost — the
// round trip that never came back — while every other operation is real.
type namesFailingCommitOnce struct {
	directory.Directory
	failed bool
}

func (n *namesFailingCommitOnce) Commit(ctx context.Context, claim directory.Claim) (directory.Entry, error) {
	if !n.failed {
		n.failed = true
		return directory.Entry{}, fmt.Errorf("names: connection reset")
	}
	return n.Directory.Commit(ctx, claim)
}

// slotsFailingUpdateOnce loses the final slot write of a create.
type slotsFailingUpdateOnce struct {
	versionstore.Store[string, Slot]
	failed bool
}

func (s *slotsFailingUpdateOnce) Update(ctx context.Context, key string, mutate versionstore.Mutate[Slot]) (versionstore.Versioned[Slot], bool, error) {
	if !s.failed {
		s.failed = true
		return versionstore.Versioned[Slot]{}, false, fmt.Errorf("slots: connection reset")
	}
	return s.Store.Update(ctx, key, mutate)
}

// Unknown writes retain one durable plan. Retry uses the same ID instead of
// assuming a failed response means that a possibly committed role is absent.
func TestACreateWithAnUnknownWriteRetainsOneRecoverablePlan(t *testing.T) {
	const fixedID = int64(4242)
	for name, mutate := range map[string]func(*Config){
		"name commit lost": func(cfg *Config) { cfg.Names = &namesFailingCommitOnce{Directory: cfg.Names} },
		"slot write lost":  func(cfg *Config) { cfg.Slots = &slotsFailingUpdateOnce{Store: cfg.Slots} },
	} {
		t.Run(name, func(t *testing.T) {
			sink := servicemetrics.NewRecorder()
			service, _, cfg := newService(t, mutate, func(cfg *Config) {
				cfg.Allocator = AllocatorFunc(func(context.Context, int32) (int64, error) { return fixedID, nil })
				cfg.Metrics = sink
			})
			ctx := context.Background()
			owner := login(t, service, "u1")
			other := login(t, service, "u2")

			if _, err := service.CreateRole(ctx, owner.ID, 1, "Alice"); err == nil {
				t.Fatal("a create whose commit tail failed reported success")
			}
			slot, found, err := cfg.Slots.Get(ctx, slotKeyFor(owner.ID, 1))
			if err != nil || !found || slot.Value.Creation.ID == "" || slot.Value.Creation.PlayerID != fixedID {
				t.Fatalf("unknown write lost its recovery plan: %+v %v %v", slot, found, err)
			}
			if stored, exists, err := cfg.Roles.Get(ctx, fixedID); err != nil || (exists && stored.Value.CreationID != slot.Value.Creation.ID) {
				t.Fatalf("role is not linked to recovery intent: %+v %v", stored, err)
			}
			if got := sink.Count("accepted:create_role"); got != 0 {
				t.Fatalf("a failed create was counted as accepted (%d); %s", got, sink.Events())
			}
			if got := sink.Count("dropped:rollback.failed"); got != 0 {
				t.Fatalf("the rollback reported %d failures; %s", got, sink.Events())
			}

			// The retry resumes this plan, including a possibly stored role.
			role, err := service.CreateRole(ctx, owner.ID, 1, "Alice")
			if err != nil {
				t.Fatalf("the retry after a rolled-back create failed: %v", err)
			}
			if role.PlayerID != fixedID || role.Name != "Alice" {
				t.Fatalf("retry produced %+v", role)
			}
			// And the committed name is really committed: another account is
			// refused, not admitted after some lapse.
			if _, err := service.CreateRole(ctx, other.ID, 1, "alice"); !errors.Is(err, ErrNameTaken) {
				t.Fatalf("another account took a committed name: %v", err)
			}
		})
	}
}

func (s *slotsFailingUpdateOnce) DeleteIf(ctx context.Context, key string, expect versionstore.Versioned[Slot], match func(Slot) bool) error {
	return s.Store.(versionstore.ConditionalDeleter[string, Slot]).DeleteIf(ctx, key, expect, match)
}
