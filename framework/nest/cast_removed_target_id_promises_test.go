package nest

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
)

// RR-20260926-73 复核残留：Cast 等锁期间目标被 Destroy / 仅内存卸载时返回的 ErrEntityNotFound，错误文本要带目标 ID，便于定位。
// 旧行为（REPRO-2026-09-26-08 §1 探针 D）：错误文本用取得锁之后的 e.GUId()，实体清理后它已归零，打印成
// “nest: entity not found: id=0 was removed while waiting for its lock”；errors.Is 语义本身正确。
func TestCastTargetRemovedWhileWaitingNamesTheTarget(t *testing.T) {
	for i, tc := range []struct {
		name   string
		remove func(*entity.ManagerAccess, entity.IThreadSafeEntity) error
	}{
		{"destroy", func(access *entity.ManagerAccess, e entity.IThreadSafeEntity) error {
			return access.Destroy(context.Background(), e, entity.EntityDestroyReason(0), false)
		}},
		{"unload", func(access *entity.ManagerAccess, e entity.IThreadSafeEntity) error {
			return access.Unload(context.Background(), e)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manager := entity.NewEntityManager()
			pID, p := newAsyncPilotEntity(t, 48600+int64(i)*10, 1)
			if err := manager.TryAdd(p); err != nil {
				t.Fatal(err)
			}
			zID := mustBuildCastID(t, 48601+int64(i)*10, castPlayerCategory, castPlayerKind)
			zMu := newLockEntrySignalMutex(zID)
			z := &rollbackTestEntity{EntityBase: entity.NewEntityBaseWithMutex(zID, castPlayerCategory, false, zMu, castPlayerKind), dao: &rollbackTestDao{id: zID, Value: 1}}
			if err := manager.TryAdd(z); err != nil {
				t.Fatal(err)
			}
			access := entity.NewManagerAccess(manager)
			mgr := NewEngine(NestOptionWithGetter(access), NestOptionWithTransactionCommitter(&recordingCommitter{}), NestOptionWithWorkerPools(WorkerPoolConfig{Workers: 2, QueueCap: 16}, WorkerPoolConfig{}))
			holding, proceed := make(chan struct{}), make(chan struct{})
			holder, follower := NewHandlerName("rr73_id_holder_"+tc.name), NewHandlerName("rr73_id_follower_"+tc.name)
			var removeErr error
			mgr.MustRegisterHandlerWithMeta(holder, func(es []entity.IThreadSafeEntity, _ []any, _ ...HandlerOption) (any, error) {
				close(holding)
				<-proceed
				removeErr = tc.remove(access, es[0])
				return "removed", nil
			}, HandlerMeta{})
			var castErr error
			mgr.MustRegisterHandlerWithMeta(follower, func(es []entity.IThreadSafeEntity, _ []any, _ ...HandlerOption) (any, error) {
				if _, err := guardFixtureCastOne[*rollbackTestEntity](zID); err != nil {
					castErr = err
					return nil, err
				}
				return "ok", nil
			}, HandlerMeta{})
			if err := mgr.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = mgr.Shutdown(context.Background()) }()

			hOut, fOut := make(chan requestResult, 1), make(chan requestResult, 1)
			sendRequest(mgr, "holder", holder, zID, hOut)
			<-holding // holder 持有 z 的锁
			zMu.armed.Store(true)
			sendRequest(mgr, "follower", follower, pID, fOut)
			select {
			case <-zMu.entered: // follower 已通过 Touch，正在等 z 的锁
			case <-time.After(10 * time.Second):
				t.Fatal("follower never reached z's lock")
			}
			close(proceed)
			rh := <-hOut
			var rf requestResult
			select {
			case rf = <-fOut:
			case <-time.After(10 * time.Second):
				t.Fatal("follower did not reply within 10s")
			}
			if rh.err != nil || removeErr != nil {
				t.Fatalf("premise: holder reply=%v remove=%v", rh.err, removeErr)
			}
			if !errors.Is(castErr, ErrEntityNotFound) || errors.Is(castErr, ErrLockTimeout) {
				t.Fatalf("premise: Cast error=%v, want ErrEntityNotFound without ErrLockTimeout (reply=%v)", castErr, rf.err)
			}
			if want := fmt.Sprintf("id=%d ", zID); !strings.Contains(castErr.Error(), want) {
				t.Fatalf("Cast error %q does not name the target (want %q): the ID was read after the entity was cleared", castErr, strings.TrimSpace(want))
			}
		})
	}
}
