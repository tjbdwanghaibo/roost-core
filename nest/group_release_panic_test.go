package nest

import (
	"testing"

	"github.com/tjbdwanghaibo/roost-core/entity"
	"github.com/tjbdwanghaibo/roost-core/goroutine"
)

func TestGroupReleasePanicStillReleasesGroupLockAndScope(t *testing.T) {
	for _, partial := range []bool{false, true} {
		name := "normal-release"
		if partial {
			name = "partial-acquire"
		}
		t.Run(name, func(t *testing.T) {
			testGroupReleasePanic(t, partial)
		})
	}
}

func testGroupReleasePanic(t *testing.T, partial bool) {
	id := mustBuildCastID(t, 9810, entity.EntityCategory(1), nestLocalKind)
	ent := newMockEntity(id, entity.EntityCategory(1))
	manager := entity.NewEntityManager()
	if err := manager.TryAdd(ent); err != nil {
		t.Fatal(err)
	}
	defer manager.RegisterOnEntityRelease(func(entity.IThreadSafeEntity) { panic("release hook failed") })()
	scope, closeScope := entity.NewGuardScope("group-release-panic")
	defer closeScope()
	locks := newEntityLockGroupLockManager()
	entities := []entity.IThreadSafeEntity{ent}
	if partial {
		busyID := mustBuildCastID(t, 9811, entity.EntityCategory(1), nestLocalKind)
		busy := newMockEntity(busyID, entity.EntityCategory(1))
		locked, unlock, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
		go func() {
			busy.GetMutex().Lock()
			close(locked)
			<-unlock
			busy.GetMutex().Unlock()
			close(done)
		}()
		<-locked
		defer func() { close(unlock); <-done }()
		entities = append(entities, busy)
	}
	var recovered any
	func() {
		defer func() { recovered = recover() }()
		_, release, err := lockDispatchEntitiesWithGroup(locks, scope.Guard(), entities, 9800, nil)
		if err != nil {
			t.Fatalf("lock: %v", err)
		}
		release()
	}()
	if recovered == nil {
		t.Fatal("release panic should reach the dispatch recovery boundary")
	}
	if group := CurrentEntityLockGroup(); group != nil {
		// 修前清理测试线程的残留 scope，避免影响其它测试。
		entityLockGroupScopes.Delete(goroutine.GoID())
		t.Errorf("group scope leaked: %d", group.GroupID())
	}
	if scope.Guard().Guarded(id) {
		t.Error("entity remained guarded")
	}
	locks.mu.Lock()
	remaining := len(locks.locks)
	locks.mu.Unlock()
	if remaining != 0 {
		t.Errorf("group lock entries leaked: %d", remaining)
	}
}
