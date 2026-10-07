package nest

import (
	"context"
	"errors"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/entity"
)

// RR-20260927-11（OPEN-ITEMS C26）：handler 内 CreateInScope 新建实体时事务捕获失败（回滚快照、提交参与者登记等），
// CreateInScope 撤销发布并把错误交给业务；业务吞掉这个错误继续返回成功时，事务照常提交——此前已登记的提交参与者照样
// 准备记录，本事务的其他修改带着一个“新建失败”的结果持久化（bf-35 未验证项）。承诺：与 handler 内新建锁冲突
// （RR-20260926-48 的 createLockBusy）同一种处理——捕获失败记在事务上，handler 结束时即使业务返回 nil 也整条回滚，
// 调用方拿到可 errors.Is 的捕获错误，committer 不被调用。

const captureFailingKind entity.EntityKind = 236

var errCreatedCaptureFailed = errors.New("test: created entity rollback capture failed")

// captureFailingEntity 的回滚捕获总是失败（RollbackState 下 CaptureEntities 调 RollbackParticipant.CaptureRollback）。
type captureFailingEntity struct {
	*rollbackTestEntity
}

func (*captureFailingEntity) CaptureRollback(*RollbackTx) error { return errCreatedCaptureFailed }

func init() {
	entity.RegisterEntityBuilder(&entity.EntityBuilderParam{
		Category: entity.EntityCategory(1),
		Kind:     captureFailingKind,
		Builder: func(p *entity.EntityCreateParam) (entity.IThreadSafeEntity, error) {
			return &captureFailingEntity{&rollbackTestEntity{
				EntityBase: entity.NewEntityBaseWithMutex(p.Id, p.Category, false, p.Mutex, p.Kind),
				dao:        &rollbackTestDao{id: p.Id},
			}}, nil
		},
	})
}

func TestCreateCaptureFailureFailsTheTransactionEvenIfSwallowed(t *testing.T) {
	manager := entity.NewEntityManager()
	pilots := addPilots(t, manager, 38200, 1)
	access := entity.NewManagerAccess(manager)
	x := mustBuildCastID(t, 38205, entity.EntityCategory(1), captureFailingKind)
	committer := &countingCommitter{}
	mgr := NewEngine(NestOptionWithGetter(access), NestOptionWithTransactionCommitter(committer), NestOptionWithWorkerNumAndMsgCap(1, 16))
	name := NewHandlerName("rr20260927_11_swallowed_capture_failure")
	var createErr error
	mgr.MustRegisterHandlerWithMeta(name, func(es []entity.IThreadSafeEntity, _ []any, _ ...HandlerOption) (any, error) {
		pilot := es[0].(*rollbackTestEntity)
		pilot.dao.Value = 99
		if err := MarkPersist(pilot.dao, 1); err != nil {
			return nil, err
		}
		_, createErr = access.Create(&entity.EntityCreateParam{IsCreate: true, Id: x, Category: entity.EntityCategory(1), Kind: captureFailingKind})
		return "ok", nil // 业务吞掉新建失败
	}, HandlerMeta{Rollback: RollbackState, Durability: DurabilityStrict})
	if err := mgr.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = mgr.Shutdown(context.Background()) }()

	ret, err := mgr.Request(context.Background(), name, pilots[0], nil)
	if !errors.Is(createErr, errCreatedCaptureFailed) {
		t.Fatalf("fixture: Create in the handler = %v, want the capture failure", createErr)
	}
	if err == nil {
		t.Fatalf("the transaction committed although creating X failed to be captured and the handler swallowed it: ret=%v commits=%d pilot=%d",
			ret, committer.calls.Load(), manager.Get(pilots[0]).(*rollbackTestEntity).dao.Value)
	}
	if !errors.Is(err, errCreatedCaptureFailed) {
		t.Fatalf("reply err=%v, want errors.Is the capture failure", err)
	}
	if errors.Is(err, ErrLockTimeout) {
		t.Fatalf("a capture failure must not be requeued as a lock timeout: %v", err)
	}
	if n := committer.calls.Load(); n != 0 {
		t.Fatalf("committer called %d times for a transaction that must fail", n)
	}
	if v := manager.Get(pilots[0]).(*rollbackTestEntity).dao.Value; v != 1 {
		t.Fatalf("declared entity value=%d after the forced rollback, want 1", v)
	}
	if got := manager.Get(x); got != nil {
		t.Fatalf("X is published after its capture failed: %v", got)
	}
}
