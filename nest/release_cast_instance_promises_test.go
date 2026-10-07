package nest

import (
	"context"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/entity"
)

// RR-20260927-26（OPEN-ITEMS B07）：ReleaseCast 按实例释放。另一个 goroutine 正按 Cast 的顺序 Touch 旧 X 并等它的锁时，
// 旧 X 的清理（ID 清零）推迟到最后一次 UnTouch，Destroy 之后旧实例 GUId 仍非零；handler 内 Destroy X → 新建同 ID 的 X →
// ReleaseCast(旧 X)。修前按 ID 释放，放掉的是新 X 的锁（handler 中途别的 goroutine 就能锁住新 X）；修后只释放旧 X 自己的锁
// （等待者随即拿到它、看到已销毁退出），新 X 的锁保持到 handler 结束。
func TestReleaseCastOfDestroyedInstanceKeepsRecreatedLock(t *testing.T) {
	manager := entity.NewEntityManager()
	pilots := addPilots(t, manager, 37200, 1)
	access := entity.NewManagerAccess(manager)
	x := mustBuildCastID(t, 37205, castOtherCategory, higherGroupCreatedKind)
	param := func() *entity.EntityCreateParam {
		return &entity.EntityCreateParam{IsCreate: true, Id: x, Category: castOtherCategory, Kind: higherGroupCreatedKind}
	}
	if _, err := access.Create(param()); err != nil {
		t.Fatal(err)
	}
	mgr := NewEngine(NestOptionWithGetter(access), NestOptionWithTransactionCommitter(&recordingCommitter{}), NestOptionWithWorkerNumAndMsgCap(1, 16))
	name := NewHandlerName("rr26_release_cast_destroyed_instance")
	type observed struct {
		oldGUID                   int64
		freshLockedAfterCreate    bool
		waiterReleasedMidHandler  bool
		waiterLockedDestroyed     bool
		freshLockedAfterRelease   bool
		freshReleasedByOwnRelease bool
	}
	var got observed
	var fresh entity.IThreadSafeEntity
	mgr.MustRegisterHandlerWithMeta(name, func([]entity.IThreadSafeEntity, []any, ...HandlerOption) (any, error) {
		old, err := CastOne[*rollbackTestEntity](x)
		if err != nil {
			return nil, err
		}
		waiterTouched := make(chan struct{})
		waiterDone := make(chan bool, 1)
		go func() { // Nest 之外的另一个访问者，按 Cast 的顺序：Touch → 等锁
			if !old.Touch() {
				close(waiterTouched)
				waiterDone <- false
				return
			}
			close(waiterTouched)
			var locked bool
			_ = entity.WithGuardScope("rr26_waiter", func(scope *entity.GuardScope) error {
				locked = scope.Guard().RequireEntity(old)
				return nil
			})
			old.UnTouch()
			waiterDone <- locked
		}()
		<-waiterTouched
		if err := access.Destroy(context.Background(), old, entity.EntityDestroyReason(0), false); err != nil {
			return nil, err
		}
		if fresh, err = access.Create(param()); err != nil {
			return nil, err
		}
		got.oldGUID = old.GUId()
		got.freshLockedAfterCreate = !tryLockElsewhere(fresh)
		ReleaseCast(old)
		got.freshLockedAfterRelease = !tryLockElsewhere(fresh)
		select { // 旧 X 自己的锁已释放：等待者拿到它、看到已销毁（RequireEntity=false）退出
		case got.waiterLockedDestroyed = <-waiterDone:
			got.waiterReleasedMidHandler = true
		case <-time.After(2 * time.Second):
		}
		ReleaseCast(fresh) // 当前实例照常按实例释放
		got.freshReleasedByOwnRelease = tryLockElsewhere(fresh)
		return "ok", nil
	}, HandlerMeta{})
	if err := mgr.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = mgr.Shutdown(context.Background()) }()
	if _, err := mgr.Request(context.Background(), name, pilots[0], nil); err != nil {
		t.Fatal(err)
	}
	t.Logf("%+v", got)
	if got.oldGUID == 0 || !got.freshLockedAfterCreate {
		t.Fatalf("fixture: the waiter's Touch must defer the old instance's cleanup (oldGUID=%d) and the handler must hold the re-created X (%v)", got.oldGUID, got.freshLockedAfterCreate)
	}
	if !got.freshLockedAfterRelease {
		t.Fatal("ReleaseCast(old X) released the re-created X's lock mid-handler")
	}
	if !got.waiterReleasedMidHandler || got.waiterLockedDestroyed {
		t.Fatalf("ReleaseCast(old X) must release the old instance's own lock: waiter released=%v lockedDestroyed=%v", got.waiterReleasedMidHandler, got.waiterLockedDestroyed)
	}
	if !got.freshReleasedByOwnRelease {
		t.Fatal("ReleaseCast(fresh X) did not release the current instance")
	}
	if !tryLockElsewhere(fresh) {
		t.Fatal("the re-created X is still locked after the handler")
	}
}
