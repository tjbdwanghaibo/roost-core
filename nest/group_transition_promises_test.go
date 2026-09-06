package nest

import (
	"errors"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/entity"
)

// Lock-group transitions are requested by entity id; the guards in front of
// the request are what keep a zero group id, an unknown entity, or a second
// concurrent request from reaching the dispatcher. Each is pinned.
func TestGroupTransitionRequestsRefuseZeroGroupsUnknownEntitiesAndOverlaps(t *testing.T) {
	const kind entity.EntityKind = 171
	const category entity.EntityCategory = 1
	entity.MustRegisterEntityKindCategory(kind, category)
	getter := newMockGetter()
	mgr := NewEngine(NestOptionWithGetter(getter))
	if err := mgr.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mgr.Shutdown(nil) })

	id, err := entity.BuildEntityID(9001, kind)
	if err != nil {
		t.Fatal(err)
	}
	ent := newMockEntityWithKind(id, category, kind)
	getter.Add(ent)
	missing, err := entity.BuildEntityID(9002, kind)
	if err != nil {
		t.Fatal(err)
	}

	if err := mgr.RequestJoinEntityLockGroup(id, 0); !errors.Is(err, ErrInvalidEntityLockGroup) {
		t.Fatalf("join group 0 = %v", err)
	}
	if err := mgr.RequestMoveEntityLockGroup(id, 0); !errors.Is(err, ErrInvalidEntityLockGroup) {
		t.Fatalf("move to group 0 = %v", err)
	}
	if err := mgr.RequestJoinEntityLockGroup(missing, 5); !errors.Is(err, ErrEntityNotFound) {
		t.Fatalf("join for an entity the getter does not know = %v", err)
	}
	if ent.Base().GroupTransitionPending() {
		t.Fatal("refused requests must not leave a transition pending on the entity")
	}

	if err := mgr.RequestJoinEntityLockGroup(id, 5); err != nil {
		t.Fatalf("first join request: %v", err)
	}
	if err := mgr.RequestMoveEntityLockGroup(id, 6); !errors.Is(err, ErrEntityGroupTransitionPending) {
		t.Fatalf("second request while the first is pending = %v", err)
	}
	if err := mgr.RequestLeaveEntityLockGroup(id); !errors.Is(err, ErrEntityGroupTransitionPending) {
		t.Fatalf("leave while a join is pending = %v", err)
	}
}
