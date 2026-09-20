package account

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/versionstore"

	"github.com/tjbdwanghaibo/roost-kit/service/directory"
	"github.com/tjbdwanghaibo/roost-kit/service/servicemetrics"
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

// CreateRole's commit point is its last write. When either of the two writes
// after the role record fails, the caller gets an error and NOTHING of the
// create survives: the same account can retry and succeed with the same name,
// which is only possible if the role, the name and the slot were all undone.
// Before this held, a lost name commit kept a role whose name lapsed and
// could be taken by someone else, and a lost slot write kept a role the
// retrying client was refused for with ErrRoleLimit.
func TestACreateThatFailsAfterTheRoleRecordLeavesNothingBehind(t *testing.T) {
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
			if _, found, _ := cfg.Roles.Get(ctx, fixedID); found {
				t.Fatal("the role record survived a failed create; the caller was never handed it")
			}
			if got := sink.Count("accepted:create_role"); got != 0 {
				t.Fatalf("a failed create was counted as accepted (%d); %s", got, sink.Events())
			}
			if got := sink.Count("dropped:rollback.failed"); got != 0 {
				t.Fatalf("the rollback reported %d failures; %s", got, sink.Events())
			}

			// The retry is a clean first attempt: name free, slot free.
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
