package nest

import (
	"context"
	"errors"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/entity"
	"github.com/tjbdwanghaibo/roost-core/fctx"
)

func castTransactionEntities(t *testing.T) (int64, *rollbackTestEntity, *rollbackTestEntity, *mockGetter) {
	t.Helper()
	id, primary := newAsyncPilotEntity(t, 9950, 10)
	otherID := mustBuildCastID(t, 9951, castAllianceCategory, castAllianceKind)
	other := &rollbackTestEntity{
		EntityBase: entity.NewEntityBase(otherID, castAllianceCategory, false, castAllianceKind),
		dao:        &rollbackTestDao{id: otherID, Value: 20},
	}
	getter := newMockGetter()
	getter.Add(primary)
	getter.Add(other)
	return id, primary, other, getter
}

// 动态实体属于 Nest 事务；是否安装客户端 Sync 不应改变回滚语义。
func TestCastTransactionRollbackWithoutSync(t *testing.T) {
	for _, rejectCommit := range []bool{false, true} {
		name := "handler_error"
		if rejectCommit {
			name = "commit_rejected"
		}
		t.Run(name, func(t *testing.T) {
			id, primary, other, getter := castTransactionEntities(t)
			rejection := errors.New("rejected")
			committer := &recordingCommitter{err: rejection}
			engine := NewEngine(NestOptionWithGetter(getter), NestOptionWithTransactionCommitter(committer))
			handler := NewHandlerName("cast_rollback_" + name)
			meta := HandlerMeta{Rollback: RollbackState}
			if rejectCommit {
				meta.Durability = DurabilityStrict
			}
			engine.MustRegisterHandlerWithMeta(handler, func(_ []entity.IThreadSafeEntity, _ []any, _ ...HandlerOption) (any, error) {
				cast, err := CastOne[*rollbackTestEntity](other.ID())
				if err != nil {
					return nil, err
				}
				primary.dao.Value = 11
				cast.dao.Value = 99
				cast.dao.Tracker.MarkSync(1)
				if err := MarkPersist(cast.dao, 1); err != nil {
					return nil, err
				}
				ReleaseCast(cast)
				if !entity.CurrentGuardScope().Guard().Guarded(cast.GUId()) {
					t.Error("dynamic entity released before transaction outcome")
				}
				if rejectCommit {
					return nil, nil
				}
				return nil, rejection
			}, meta)
			if err := engine.Start(); err != nil {
				t.Fatal(err)
			}
			defer engine.Shutdown(context.Background())
			if _, err := engine.Request(context.Background(), handler, id, nil); !errors.Is(err, rejection) {
				t.Fatalf("request error = %v", err)
			}
			if primary.dao.Value != 10 || other.dao.Value != 20 {
				t.Errorf("rollback values = %d/%d, want 10/20", primary.dao.Value, other.dao.Value)
			}
			if mask := other.dao.Tracker.TakeEntitySyncDirty(); mask != 0 {
				t.Errorf("rolled-back dynamic entity retained sync dirty: %d", mask)
			}
		})
	}
}

type castBoundaryCommitter struct {
	*pipelinedTestCommitter
	beforeWait func()
}

func (c *castBoundaryCommitter) Enqueue(ctx context.Context, record CommitRecord) (CommitTicket, error) {
	ticket, err := c.pipelinedTestCommitter.Enqueue(ctx, record)
	if err != nil {
		return nil, err
	}
	return castBoundaryTicket{CommitTicket: ticket, beforeWait: c.beforeWait}, nil
}

type castBoundaryTicket struct {
	CommitTicket
	beforeWait func()
}

func (t castBoundaryTicket) Done() <-chan struct{} {
	t.beforeWait()
	return t.CommitTicket.Done()
}

func TestCastPipelinedReleasesAndStampsWithoutSync(t *testing.T) {
	_, closeContext := fctx.NewContext()
	defer closeContext()
	_, primary, other, getter := castTransactionEntities(t)
	scope, closeScope := entity.NewGuardScope("cast-pipelined")
	defer closeScope()
	pop := pushCurrentNestDispatchMsg(&Msg{getter: getter})
	defer pop()
	if !scope.Guard().RequireEntity(primary) {
		t.Fatal("primary lock")
	}
	committer := &castBoundaryCommitter{pipelinedTestCommitter: newPipelinedTestCommitter(false)}
	committer.beforeWait = func() {
		// ticket.Done 在 inline 路径等待 WAL 前调用；无需 sleep，也不会让修前测试挂死。
		if scope.Guard().Guarded(other.GUId()) {
			t.Error("dynamic entity still locked during durable wait")
		}
		if got := other.LastCommitLSN(); got != 1 {
			t.Errorf("dynamic entity LSN = %d, want 1", got)
		}
		committer.resolveAll(nil)
	}
	_, err := invokeWithTransaction(HandlerMeta{Rollback: RollbackState, Durability: DurabilityPipelined},
		[]entity.IThreadSafeEntity{primary}, committer, "cast-pipelined",
		func() { scope.Guard().ReleaseEntity(primary.GUId()) }, nil, func() (any, error) {
			cast, err := CastOne[*rollbackTestEntity](other.ID())
			if err != nil {
				return nil, err
			}
			cast.dao.Value++
			return nil, MarkPersist(cast.dao, 1)
		})
	if err != nil {
		t.Fatal(err)
	}
}
