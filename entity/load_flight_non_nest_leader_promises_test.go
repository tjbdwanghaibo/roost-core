package entity

import (
	"context"
	"errors"
	"testing"
	"time"
)

// OPEN-ITEMS B20：RR-20260926-54 之后共享加载在独立 goroutine 上运行，领头方仍在等待且没有指定本地执行器时，
// loader 经 RunLocal 的发布就地在加载 goroutine 上执行（修前在领头方自己的 goroutine 上）。bf-54 记为“非 Nest 的
// 领头方持有别的实体锁时触发冷加载，没有专门覆盖”。
//
// 验证：Nest 之外的调用方在 WithGuardScope 里持有 X 的锁，Get 冷加载 Y；loader 的发布在自己的 Guard 作用域里取 Y 的锁
// 再 TryAdd（与 DataEngine 仓库经 RunLocal → EntityManager.Create 发布同形）。结论：不死锁，Y 已发布且锁已归还，
// X 的锁在整个过程中仍由领头方持有。
//
// 边界：本用例只覆盖“发布只锁被加载的实体”。loader 的发布若还要锁领头方正持有的实体，修后会在加载 goroutine 上
// 等那把锁，而领头方在等加载——这一形状另行报告，不在这里断言。
type publishUnderOwnGuardLoader struct {
	manager *EntityManager
	locked  chan bool // 发布时领头方持有的 X 是否可被加载 goroutine 取得（应为 false）
	x       IThreadSafeEntity
}

func (l *publishUnderOwnGuardLoader) LoadEntity(ctx context.Context, id int64, _ EntityKind) (IThreadSafeEntity, error) {
	value := newMgrTestEntity(id, testEntityCategoryPlayer)
	var addErr error
	if err := RunLocal(ctx, func() {
		l.locked <- lockableElsewhere(l.x)
		addErr = WithGuardScope("load_publish", func(scope *GuardScope) error {
			if !scope.Guard().RequireEntity(value) {
				return errors.New("publish could not lock the loaded entity")
			}
			return l.manager.TryAdd(value)
		})
	}); err != nil {
		return nil, err
	}
	return value, addErr
}

func TestNonNestLeaderHoldingAnotherLockColdLoadsWithoutDeadlock(t *testing.T) {
	manager := NewEntityManager()
	access := NewManagerAccess(manager)
	x := newMgrTestEntity(mustBuildTestEntityID(t, 5501, testEntityCategoryPlayer, EntityKindNone), testEntityCategoryPlayer)
	if err := manager.TryAdd(x); err != nil {
		t.Fatal(err)
	}
	loader := &publishUnderOwnGuardLoader{manager: manager, locked: make(chan bool, 1), x: x}
	if _, err := access.ConfigureLoader(loader); err != nil {
		t.Fatal(err)
	}
	y := mustBuildTestEntityID(t, 5502, testEntityCategoryPlayer, EntityKindNone)

	type leaderResult struct {
		value IThreadSafeEntity
		err   error
	}
	done := make(chan leaderResult, 1)
	go func() {
		var got leaderResult
		got.err = WithGuardScope("non_nest_leader", func(scope *GuardScope) error {
			if !scope.Guard().RequireEntity(x) {
				return errors.New("leader could not lock X")
			}
			var err error
			got.value, err = access.Get(context.Background(), y, EntityCategoryNone)
			return err
		})
		done <- got
	}()
	var got leaderResult
	select {
	case got = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the non-Nest leader holding X never got Y back: the cold load's publish deadlocked")
	}
	if got.err != nil || got.value == nil {
		t.Fatalf("leader Get(Y): value=%v err=%v", got.value, got.err)
	}
	if xFreeDuringPublish := <-loader.locked; xFreeDuringPublish {
		t.Fatal("X was lockable by the load goroutine while the leader held it")
	}
	if published := manager.Get(y); published != got.value {
		t.Fatalf("Y not published: manager has %v, leader got %v", published, got.value)
	}
	if !lockableElsewhere(got.value) || !lockableElsewhere(x) {
		t.Fatalf("locks leaked: Y free=%v X free=%v", lockableElsewhere(got.value), lockableElsewhere(x))
	}
}
