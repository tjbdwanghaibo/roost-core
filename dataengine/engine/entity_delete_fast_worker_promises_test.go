package engine

import (
	"context"
	"errors"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/entity"
	"github.com/tjbdwanghaibo/roost-core/fctx"
	"github.com/tjbdwanghaibo/roost-core/remoteentity"
)

// RR-20260926-27：快 worker 上删除 Remote 托管实体，快池断言发生在任何副作用之前，
// 应是确定拒绝。
//
// 默认 handler（RollbackNone/Memory，没有 RollbackTx）在快阶段调用 ManagerAccess.Destroy
// 或生成 lifecycle 的 Destroy(deletePersisted=true)，删除准入走 admitRemoteEntityDelete →
// PrepareRemoteWriteBatch，入口的 fctx 断言 panic。准入的 recover 把它当成“结果未知”：
// 标 DeleteAdmissionIndeterminate、runtime.fail（kit 里就是 Nest.Fence + RuntimeFailure），
// EntityManager 因不确定结果摘除实体，错误还用 %v 丢了 errors.Is 链。一次误用就 fence
// 全进程，而此时远端、WAL、内存都还没有被动过。
func TestFastWorkerRemoteDeleteIsDefiniteRejection(t *testing.T) {
	ensureDeleteGuardKinds()
	manager := entity.NewEntityManager()
	var fatal []error
	runtime := &Runtime{Projector: &Projector{}, access: entity.NewManagerAccess(manager), remoteManager: new(remoteentity.Manager), onFatal: func(err error) { fatal = append(fatal, err) }}
	unregister, err := runtime.access.RegisterDeleteAdmitter(runtime.admitEntityDelete)
	if err != nil {
		t.Fatal(err)
	}
	defer unregister()
	remote := newRemoteDeleteEntity(9901)
	if err := manager.TryAdd(remote); err != nil {
		t.Fatal(err)
	}
	_, release := fctx.NewContext(fctx.WithFastWorker(), fctx.WithHandler("default_handler"))
	destroyErr := runtime.access.Destroy(context.Background(), remote, entity.DestroyReasonCommon, true)
	release()
	if !errors.Is(destroyErr, fctx.ErrBlockingInFastWorker) {
		t.Errorf("Destroy err=%v, want errors.Is(fctx.ErrBlockingInFastWorker)", destroyErr)
	}
	if len(fatal) != 0 {
		t.Errorf("fast-worker rejection before any side effect escalated to runtime fatal: %v", fatal)
	}
	if manager.Get(remote.ID()) != remote {
		t.Error("definitively rejected delete removed the entity from memory")
	}
}

// 其余 panic 仍无法证明远端/WAL 未被动过，保持不确定处理：fatal、摘除实体，并保留原错误链。
type panickingBatchManager struct {
	entity.IRemoteEntityManager
	cause error
}

func (m panickingBatchManager) PrepareRemoteWriteBatch(context.Context, []int64) (entity.RemoteWriteBatch, error) {
	panic(m.cause)
}

func TestDeleteAdmissionOtherPanicStaysIndeterminate(t *testing.T) {
	ensureDeleteGuardKinds()
	manager := entity.NewEntityManager()
	cause := errors.New("remote store exploded mid-prepare")
	var fatal []error
	runtime := newDeleteRuntime(manager, panickingBatchManager{cause: cause})
	runtime.onFatal = func(err error) { fatal = append(fatal, err) }
	unregister, err := runtime.access.RegisterDeleteAdmitter(runtime.admitEntityDelete)
	if err != nil {
		t.Fatal(err)
	}
	defer unregister()
	remote := newRemoteDeleteEntity(9902)
	if err := manager.TryAdd(remote); err != nil {
		t.Fatal(err)
	}
	destroyErr := runtime.access.Destroy(context.Background(), remote, entity.DestroyReasonCommon, true)
	if !errors.Is(destroyErr, cause) {
		t.Errorf("Destroy err=%v lost the panic cause", destroyErr)
	}
	if len(fatal) != 1 || !errors.Is(fatal[0], cause) {
		t.Errorf("indeterminate delete panic must be fatal with its cause: %v", fatal)
	}
	if manager.Get(remote.ID()) != nil {
		t.Error("indeterminate delete kept serving the entity")
	}
}
