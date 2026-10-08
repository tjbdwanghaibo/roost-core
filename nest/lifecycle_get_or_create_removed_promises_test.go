package nest

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/entity"
)

// RR-20260926-57：同 ID 的上一个实例刚被撤销（或销毁），收尾尚未完成时 TryAdd 返回 ErrEntityRemoved。
// 生成 Lifecycle 的 GetOrCreate 对它有界重试（再 Get、再 Create），不把这个瞬时状态当成业务错误返回。
//
// 时序完全由同步点控制：A 在 handler 内新建 X 后失败，事务撤销发布；A 的 Guard 释放锁之后、撤销收尾（清除
// removing 标记）之前，A 注册的 post-release 回调停住。B 在 Nest 之外 GetOrCreate(X)：第一次 Create 必然得到
// ErrEntityRemoved；第二次 Create 构造实例时（第 3 次构造）放行 A 的收尾，并等 A 完成后再继续。
func TestLifecycleGetOrCreateRetriesWhileRevokeFinishes(t *testing.T) {
	manager := entity.NewEntityManager()
	ids := addPilots(t, manager, 9950, 1)
	access := entity.NewManagerAccess(manager)
	lifecycle := &generatedShapeLifecycle{access: access}
	const unique = 9955
	inWindow := make(chan struct{})
	bRetried := make(chan struct{})
	aDone := make(chan struct{})
	var builds atomic.Int64
	hook := func() {
		if builds.Add(1) == 3 {
			close(bRetried)
			select {
			case <-aDone:
			case <-time.After(2 * time.Second):
			}
		}
	}
	createdInScopeBuildHook.Store(&hook)
	t.Cleanup(func() { createdInScopeBuildHook.Store(nil) })

	mgr := NewEngine(NestOptionWithGetter(access), NestOptionWithTransactionCommitter(&recordingCommitter{}), NestOptionWithWorkerPools(WorkerPoolConfig{Workers: 1, QueueCap: 16}, WorkerPoolConfig{}))
	boom := errors.New("boom")
	name := NewHandlerName("rr57_revoke_then_fail")
	mgr.MustRegisterHandlerWithMeta(name, func([]entity.IThreadSafeEntity, []any, ...HandlerOption) (any, error) {
		if _, err := lifecycle.Create(nestBaseContext(), unique); err != nil {
			return nil, err
		}
		entity.CurrentGuardScope().Guard().AppendPostRelease(func() {
			close(inWindow)
			select {
			case <-bRetried:
			case <-time.After(2 * time.Second):
			}
		})
		return nil, boom
	}, HandlerMeta{Rollback: RollbackState, Durability: DurabilityStrict})
	if err := mgr.Start(); err != nil {
		t.Fatal(err)
	}
	defer mgr.Shutdown(context.Background())

	aErr := make(chan error, 1)
	go func() { _, err := mgr.Request(context.Background(), name, ids[0], nil); aErr <- err }()
	<-inWindow
	type result struct {
		value   *rollbackTestEntity
		created bool
		err     error
	}
	bOut := make(chan result, 1)
	go func() {
		value, created, err := lifecycle.GetOrCreate(context.Background(), unique)
		bOut <- result{value, created, err}
	}()
	if err := <-aErr; !errors.Is(err, boom) {
		t.Fatalf("A: want boom, got %v", err)
	}
	close(aDone)
	b := <-bOut
	if b.err != nil || !b.created || b.value == nil {
		t.Fatalf("GetOrCreate while the previous instance's revoke was finishing: created=%v err=%v (ErrEntityRemoved=%v); want a new entity", b.created, b.err, errors.Is(b.err, entity.ErrEntityRemoved))
	}
	if got := manager.Get(b.value.ID()); got != b.value {
		t.Fatalf("GetOrCreate returned an entity that is not the published one: %v", got)
	}
	if !tryLockElsewhere(b.value) {
		t.Fatal("the entity created outside Nest is still locked")
	}
}
