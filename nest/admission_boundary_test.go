package nest

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/entity"
	fctx "github.com/tjbdwanghaibo/roost-core/fctx"
)

// 准入回调修改的是锁保护的生命周期状态，必须早于 release 和异步完成交接。
func TestPipelinedAdmissionRunsBeforeUnlock(t *testing.T) {
	for _, async := range []bool{false, true} {
		name := "inline"
		if async {
			name = "async"
		}
		t.Run(name, func(t *testing.T) {
			_, releaseContext := fctx.NewContext()
			defer releaseContext()
			id, ent := newAsyncPilotEntity(t, 9801, 10)
			es := []entity.IThreadSafeEntity{ent}
			scope, closeScope := entity.NewGuardScope("admission-boundary")
			defer closeScope()
			if !scope.Guard().RequireEntity(ent) {
				t.Fatal("lock entity")
			}
			msg := &Msg{Tid: id, RetChan: make(chan any, 1)}
			pop := pushCurrentNestDispatchMsg(msg)
			defer pop()
			committer := newPipelinedTestCommitter(false)
			var pump *completionPump
			if async {
				pump = newCompletionPump(1, 4)
				pump.start()
				defer func() { _ = pump.stop(context.Background()) }()
			}
			var admitted atomic.Int32
			var lockedAtAdmission atomic.Bool
			var lsnAtAdmission atomic.Uint64
			var admittedAtRelease int32
			release := func() {
				admittedAtRelease = admitted.Load()
				scope.Guard().ReleaseEntity(id)
				// 精确控制持久化时机，无需 sleep：只有释放锁后才 resolve ticket。
				committer.resolveAll(nil)
			}
			_, err := invokeWithTransaction(HandlerMeta{Rollback: RollbackUndo, Durability: DurabilityPipelined}, es, committer, name, release, pump, func() (any, error) {
				tx := CurrentRollbackTx()
				tx.AfterAdmission(func() {
					guard := entity.CurrentGuardScope()
					lockedAtAdmission.Store(guard != nil && guard.Guard().Guarded(id))
					lsnAtAdmission.Store(ent.LastCommitLSN())
					admitted.Add(1)
				})
				ent.dao.Value++
				return "ok", MarkPersist(ent.dao, 1)
			})
			if err != nil {
				t.Fatal(err)
			}
			if async {
				select {
				case result := <-msg.RetChan:
					if result != "ok" {
						t.Fatalf("completion = %v", result)
					}
				case <-time.After(2 * time.Second):
					t.Fatal("completion did not finish")
				}
			}
			if admittedAtRelease != 1 || admitted.Load() != 1 || !lockedAtAdmission.Load() || lsnAtAdmission.Load() != 1 {
				t.Fatalf("admission at release=%d total=%d locked=%v LSN=%d; want 1/1/true/1", admittedAtRelease, admitted.Load(), lockedAtAdmission.Load(), lsnAtAdmission.Load())
			}
		})
	}
}

func TestPipelinedAdmissionPanicStillCompletesDurableTransaction(t *testing.T) {
	_, releaseContext := fctx.NewContext()
	defer releaseContext()
	_, ent := newAsyncPilotEntity(t, 9802, 10)
	committer := newPipelinedTestCommitter(false)
	var order []string
	_, err := invokeWithTransaction(HandlerMeta{Rollback: RollbackUndo, Durability: DurabilityPipelined}, []entity.IThreadSafeEntity{ent}, committer, "admission-panic", func() {
		order = append(order, "release")
		committer.resolveAll(nil)
	}, nil, func() (any, error) {
		tx := CurrentRollbackTx()
		tx.AfterAdmission(func() { panic("admission failed") })
		tx.AfterAdmission(func() { order = append(order, "admission") })
		tx.AfterCommit(func() { order = append(order, "commit") })
		ent.dao.Value++
		return nil, MarkPersist(ent.dao, 1)
	})
	if !errors.Is(err, ErrAfterCommitFailed) {
		t.Fatalf("error=%v, want committed callback failure", err)
	}
	if len(order) != 3 || order[0] != "admission" || order[1] != "release" || order[2] != "commit" || ent.dao.Value != 11 {
		t.Fatalf("order=%v value=%d; admitted state must remain committed", order, ent.dao.Value)
	}
}

func TestPipelinedAdmissionRejectAndEmptyRecord(t *testing.T) {
	for _, reject := range []bool{false, true} {
		name := "empty-record"
		if reject {
			name = "rejected-record"
		}
		t.Run(name, func(t *testing.T) {
			_, releaseContext := fctx.NewContext()
			defer releaseContext()
			_, ent := newAsyncPilotEntity(t, 9803, 10)
			committer := newPipelinedTestCommitter(true)
			if reject {
				committer.enqueueErr = errors.New("queue full")
			}
			var admitted, released int
			_, err := invokeWithTransaction(HandlerMeta{Rollback: RollbackUndo, Durability: DurabilityPipelined}, []entity.IThreadSafeEntity{ent}, committer, name, func() {
				released++
				if admitted != 1 {
					t.Error("empty transaction released before admission callback")
				}
			}, nil, func() (any, error) {
				CurrentRollbackTx().AfterAdmission(func() { admitted++ })
				if reject {
					return nil, MarkPersist(ent.dao, 1)
				}
				return nil, nil
			})
			if reject {
				if !errors.Is(err, ErrCommitRejected) || admitted != 0 || released != 0 {
					t.Fatalf("rejected: err=%v admitted=%d earlyRelease=%d", err, admitted, released)
				}
			} else if err != nil || admitted != 1 || released != 1 {
				t.Fatalf("empty: err=%v admitted=%d release=%d", err, admitted, released)
			}
		})
	}
}
