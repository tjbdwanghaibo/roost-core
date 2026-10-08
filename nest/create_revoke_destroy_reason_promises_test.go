package nest

import (
	"context"
	"errors"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/entity"
)

// RR-20260927-12（OPEN-ITEMS C27）：事务回滚 / 拒绝时撤销 handler 内新建的实体（RR-20260926-35 revokeCreated），销毁回调一直用
// DestroyReasonCommon——与业务 Destroy 的缺省原因相同，而 RR-30 / 39 的仅内存卸载已有专门的 DestroyReasonMemoryUnload。
// 业务的 OnDestroy 因此分不清“这次新建被撤销、从未成为权威”与普通删除 / 卸载。承诺：撤销新建调用
// DestroyAll / OnDestroy(entity.DestroyReasonCreateRevoked)，与业务 Destroy（传入的原因）和 Unload（DestroyReasonMemoryUnload）互不相同。
func TestRevokedCreationIsDestroyedWithCreateRevokedReason(t *testing.T) {
	manager := entity.NewEntityManager()
	pilots := addPilots(t, manager, 38300, 1)
	access := entity.NewManagerAccess(manager)
	x := mustBuildCastID(t, 38305, entity.EntityCategory(1), createdInScopeKind)
	mgr := NewEngine(NestOptionWithGetter(access), NestOptionWithTransactionCommitter(&recordingCommitter{}), NestOptionWithWorkerPools(WorkerPoolConfig{Workers: 1, QueueCap: 16}, WorkerPoolConfig{}))
	name := NewHandlerName("rr20260927_12_revoke_reason")
	boom := errors.New("business failed after creating X")
	var reasons []entity.EntityDestroyReason
	mgr.MustRegisterHandlerWithMeta(name, func([]entity.IThreadSafeEntity, []any, ...HandlerOption) (any, error) {
		created, err := access.Create(createParam(x))
		if err != nil {
			return nil, err
		}
		created.Base().SetHooks(nil, func(reason entity.EntityDestroyReason) { reasons = append(reasons, reason) })
		return nil, boom
	}, HandlerMeta{Rollback: RollbackState, Durability: DurabilityStrict})
	if err := mgr.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = mgr.Shutdown(context.Background()) }()
	if _, err := mgr.Request(context.Background(), name, pilots[0], nil); !errors.Is(err, boom) {
		t.Fatalf("err=%v, want the business error", err)
	}
	if manager.Get(x) != nil {
		t.Fatal("fixture: the created entity must have been revoked")
	}
	if len(reasons) != 1 || reasons[0] == entity.DestroyReasonCommon || reasons[0] == entity.DestroyReasonMemoryUnload {
		t.Fatalf("revoked creation destroyed with reasons %v; want exactly one call with a reason distinct from DestroyReasonCommon (%d) and DestroyReasonMemoryUnload (%d)",
			reasons, entity.DestroyReasonCommon, entity.DestroyReasonMemoryUnload)
	}
	if reasons[0] != entity.DestroyReasonCreateRevoked {
		t.Fatalf("revoked creation destroyed with reason %d, want DestroyReasonCreateRevoked (%d)", reasons[0], entity.DestroyReasonCreateRevoked)
	}
}
