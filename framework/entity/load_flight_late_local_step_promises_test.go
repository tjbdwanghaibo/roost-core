package entity

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// RR-20260927-29：RR-27 之后，持有实体锁、在 Nest 之外的领头方经 leaderSteps 接收加载里的本地步骤，离开时关闭 leaderAway。
// 但只有“领头方按自己的 ctx 离开”这一分支关闭它：加载正常完成、领头方从 flight.done 返回后，loader 另起的 goroutine 若
// 仍用加载 ctx 调 RunLocal（契约外、编译期不拦），runOnLeader 向已无人接收的 leaderSteps 发送、leaderAway 永不关闭，
// 调用方永久阻塞（goroutine 泄漏）。承诺：领头方以任何方式离开之后，迟到的本地步骤改走绑定的执行器，未绑定时就地执行，
// 不阻塞。用例用 channel 控制先后（领头方 Get 返回之后才放开迟到步骤），不依赖 sleep。
type lateLocalStepLoader struct {
	manager *EntityManager
	release chan struct{} // 领头方 Get 返回后由用例关闭
	late    chan error
	ran     atomic.Bool
}

func (l *lateLocalStepLoader) LoadEntity(ctx context.Context, id int64, _ EntityKind) (IThreadSafeEntity, error) {
	value := newMgrTestEntity(id, testEntityCategoryPlayer)
	if err := l.manager.TryAdd(value); err != nil {
		return nil, err
	}
	go func() {
		<-l.release
		l.late <- RunLocal(ctx, func() { l.ran.Store(true) })
	}()
	return value, nil
}

func TestLateRunLocalAfterNonNestLeaderLoadCompletedDoesNotBlock(t *testing.T) {
	for _, bound := range []bool{false, true} {
		name := "unbound"
		if bound {
			name = "bound_executor"
		}
		t.Run(name, func(t *testing.T) {
			manager := NewEntityManager()
			access := NewManagerAccess(manager)
			var boundRuns atomic.Int32
			if bound {
				access.BindLocalExecutor(func(fn func()) error {
					boundRuns.Add(1)
					fn()
					return nil
				})
			}
			x := newMgrTestEntity(mustBuildTestEntityID(t, 5591, testEntityCategoryPlayer, EntityKindNone), testEntityCategoryPlayer)
			if err := manager.TryAdd(x); err != nil {
				t.Fatal(err)
			}
			loader := &lateLocalStepLoader{manager: manager, release: make(chan struct{}), late: make(chan error, 1)}
			if _, err := access.ConfigureLoader(loader); err != nil {
				t.Fatal(err)
			}
			y := mustBuildTestEntityID(t, 5592, testEntityCategoryPlayer, EntityKindNone)
			// 领头方持有 X 的锁，所以 flight 带 leaderSteps / leaderAway（RR-27 的交回领头方路径）。
			err := WithGuardScope("late_local_step_leader", func(scope *GuardScope) error {
				if !scope.Guard().RequireEntity(x) {
					return errors.New("lock x")
				}
				_, err := access.Get(context.Background(), y, EntityCategoryNone)
				return err
			})
			if err != nil {
				t.Fatalf("leader Get: %v", err)
			}
			close(loader.release)
			select {
			case lateErr := <-loader.late:
				if lateErr != nil {
					t.Fatalf("late RunLocal returned %v", lateErr)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("late RunLocal (after the leader left with the completed load) still blocked after 3s: leaderAway was never closed")
			}
			if !loader.ran.Load() {
				t.Fatal("late RunLocal returned without running the step")
			}
			want := int32(0)
			if bound {
				want = 1
			}
			if got := boundRuns.Load(); got != want {
				t.Fatalf("bound executor runs = %d, want %d", got, want)
			}
		})
	}
}
