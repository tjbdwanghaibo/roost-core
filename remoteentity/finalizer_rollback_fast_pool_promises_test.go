package remoteentity

import (
	"context"
	"errors"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/entity"
	"github.com/tjbdwanghaibo/roost-core/fctx"
	"github.com/tjbdwanghaibo/roost-core/nest"
)

// OPEN-ITEMS B26：RR-20260926-28 的修复记录写“finalizer 回滚不是投递到 Nest 快池执行（RunLocal 就地执行）”，那是 RR-30/39 之前的状态。
// 现在 kit 装配（kit/nest 把 remote_entity 能力里的 *remoteentity.Manager 交给 nest.NestOptionWithRemoteEntityManager）下，
// NewEngine 经 nest.LocalExecutorBinder 把 NestMgr.RunLocal 绑定给 Manager；finalizer 拿到持久拒绝后，
// settleRejectedRemoteEntries 用 entity.RunLocal(m.localContext(ctx), …) 把回滚（实体本地锁内的 RollbackRemoteCommit）
// 放进快池执行。这里用与 kit 相同的 Nest 选项断言回滚 hook 在快 worker 上运行。

func (e *reloadableFullDocEntity) RollbackRemoteCommit(entity.RemoteCommit) {
	e.rollbacks.Add(1)
	if !fctx.InFastWorker() {
		e.rollbacksOffFast.Add(1)
	}
}

func TestFinalizerRejectionRollbackRunsOnNestFastWorker(t *testing.T) {
	for i, tc := range []struct {
		name       string
		durability uint8
	}{
		{"memory_unreachable", 0},
		{"strict_rejected", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, live := newReloadFixture(t, 1961+int64(i))
			engine := nest.NewEngine(nest.NestOptionWithGetter(f.access), nest.NestOptionWithRemoteEntityManager(entity.IRemoteEntityManager(f.mgr)), nest.NestOptionWithWorkerPools(nest.WorkerPoolConfig{Workers: 1, QueueCap: 16}, nest.WorkerPoolConfig{}))
			if err := engine.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = engine.Shutdown(context.Background()) })
			if f.mgr.localExecutor.Load() == nil {
				t.Fatal("premise: NewEngine did not bind NestMgr.RunLocal to the Remote Manager")
			}
			tx := remoteTestTxID(0xD0 + byte(i))
			f.storage.unreachable.Store(tc.durability == 0)
			batch := f.prepareRejected(t, live, tx, tc.durability)
			switch tc.durability {
			case 0:
				if _, err := batch.Commit(context.Background()); !errors.Is(err, entity.ErrRemotePersistenceIndeterminate) {
					t.Fatalf("commit err=%v, want indeterminate (finalizer resolves it)", err)
				}
			case 2:
				f.mgr.RejectRemoteTransaction(tx, "lease expired")
				if _, err := batch.Commit(context.Background()); !errors.Is(err, entity.ErrRemoteRejected) {
					t.Fatalf("strict commit err=%v, want rejected (finalizer settles it)", err)
				}
			}
			if err := batch.Close(context.Background()); err != nil {
				t.Fatal(err)
			}
			f.assertReloadedFromAuthority(t, live) // finalizer 已收尾：回滚、隔离、释放 gate、卸载
			if n := live.rollbacks.Load(); n != 1 {
				t.Fatalf("RollbackRemoteCommit ran %d time(s), want once", n)
			}
			if off := live.rollbacksOffFast.Load(); off != 0 {
				t.Fatalf("finalizer rejection rollback ran off the Nest fast pool (%d call(s)); with Nest bound it must go through NestMgr.RunLocal", off)
			}
		})
	}
}
