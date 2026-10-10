package nest

import (
	"context"
	"errors"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
)

// RR-20260927-31：handler 内 Cast 取得实体后 tx.CaptureEntities 失败（回滚快照、提交参与者登记、durable 事务内的 Remote 托管实体等），
// Cast 只把错误还给业务；业务吞掉它继续返回成功时事务照常提交——一个未进入回滚 / 持久化参与的实体随事务落地（RR-11 的同类，
// 修前即有）。承诺：与 CreateInScope 捕获失败（RR-20260927-11）同一机制——捕获失败记在事务上，handler 结束时即使业务返回 nil
// 也整条回滚，调用方拿到可 errors.Is 的捕获错误（不是锁超时、不重排），committer 不被调用，Cast 已取得的锁在事务结束时归还。

// kind 199：nest 包测试已用 11/12/13、171、193～195、206/207、236、238、241、248～252；199 未用（进程级 builder 注册表按 kind 区分）。
const castCaptureFailingKind entity.EntityKind = 199

var errCastCaptureFailed = errors.New("test: cast entity rollback capture failed")

// castCaptureFailingEntity 的回滚捕获总是失败（RollbackState 下 CaptureEntities 调 RollbackParticipant.CaptureRollback）。
type castCaptureFailingEntity struct {
	*rollbackTestEntity
}

func (*castCaptureFailingEntity) CaptureRollback(*RollbackTx) error { return errCastCaptureFailed }

func init() {
	entity.RegisterEntityBuilder(&entity.EntityBuilderParam{
		Category: castOtherCategory,
		Kind:     castCaptureFailingKind,
		Builder: func(p *entity.EntityCreateParam) (entity.IThreadSafeEntity, error) {
			return &castCaptureFailingEntity{&rollbackTestEntity{
				EntityBase: entity.NewEntityBaseWithMutex(p.Id, p.Category, false, p.Mutex, p.Kind),
				dao:        &rollbackTestDao{id: p.Id},
			}}, nil
		},
	})
}

func TestCastCaptureFailureFailsTheTransactionEvenIfSwallowed(t *testing.T) {
	for _, returnIt := range []bool{false, true} {
		name := "swallowed"
		if returnIt {
			name = "returned"
		}
		t.Run(name, func(t *testing.T) {
			manager := entity.NewEntityManager()
			pilots := addPilots(t, manager, 49980, 1)
			access := entity.NewManagerAccess(manager)
			x := mustBuildCastID(t, 49985, castOtherCategory, castCaptureFailingKind)
			if _, err := access.Create(&entity.EntityCreateParam{IsCreate: true, Id: x, Category: castOtherCategory, Kind: castCaptureFailingKind}); err != nil {
				t.Fatal(err)
			}
			committer := &countingCommitter{}
			mgr := NewEngine(NestOptionWithGetter(access), NestOptionWithTransactionCommitter(committer), NestOptionWithWorkerPools(WorkerPoolConfig{Workers: 1, QueueCap: 16}, WorkerPoolConfig{}))
			handler := NewHandlerName("rr20260927_31_cast_capture_failure_" + name)
			var castErr error
			mgr.MustRegisterHandlerWithMeta(handler, func(es []entity.IThreadSafeEntity, _ []any, _ ...HandlerOption) (any, error) {
				pilot := es[0].(*rollbackTestEntity)
				pilot.dao.Value = 55
				if err := MarkPersist(pilot.dao, 1); err != nil {
					return nil, err
				}
				_, castErr = guardFixtureCastOne[*castCaptureFailingEntity](x)
				if returnIt {
					return nil, castErr
				}
				return "ok", nil // 业务吞掉 Cast 的捕获失败
			}, HandlerMeta{Rollback: RollbackState, Durability: DurabilityStrict})
			if err := mgr.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = mgr.Shutdown(context.Background()) }()

			ret, err := mgr.Request(context.Background(), handler, pilots[0], nil)
			if !errors.Is(castErr, errCastCaptureFailed) {
				t.Fatalf("fixture: Cast in the handler = %v, want the capture failure", castErr)
			}
			if err == nil {
				t.Fatalf("the transaction committed although the Cast target failed to be captured and the handler swallowed it: ret=%v commits=%d pilot=%d",
					ret, committer.calls.Load(), manager.Get(pilots[0]).(*rollbackTestEntity).dao.Value)
			}
			if !errors.Is(err, errCastCaptureFailed) {
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
			target := manager.Get(x)
			if target == nil {
				t.Fatal("the Cast target disappeared")
			}
			if !target.GetMutex().TryLock() {
				t.Fatal("the Cast target's lock was not released when the transaction ended")
			}
			target.GetMutex().Unlock()
		})
	}
}
