// 消息事务：回滚（state / undo）、strict 提交成功 / 拒绝 / 未知、提交回调、WAL 记录形状与 RollbackTx 的登记校验。

package nest

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/dataengine"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
)

func TestCommitRunsEveryCallbackAndReportsAPanickingOne(t *testing.T) {
	tx := NewRollbackTx(RollbackUndo)
	order := make([]string, 0, 3)
	tx.AfterCommit(func() { order = append(order, "first") })
	tx.AfterCommit(func() { panic("business hook exploded") })
	tx.AfterCommit(func() { order = append(order, "release") })

	err := tx.Commit()
	if err == nil {
		t.Fatal("Commit swallowed a panicking callback without reporting it")
	}
	if !errors.Is(err, ErrAfterCommitFailed) {
		t.Fatalf("Commit error = %v, want ErrAfterCommitFailed", err)
	}
	if len(order) != 2 || order[0] != "first" || order[1] != "release" {
		t.Fatalf("callbacks that ran = %v; the one after the panic must still run", order)
	}
}

// A second Commit on an already committed transaction is a no-op that does
// not re-run callbacks and reports nothing.
func TestCommitIsIdempotent(t *testing.T) {
	tx := NewRollbackTx(RollbackUndo)
	runs := 0
	tx.AfterCommit(func() { runs++ })
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if runs != 1 {
		t.Fatalf("callback ran %d times across two Commits, want once", runs)
	}
}

var errFinalizeRefused = errors.New("test: remote batch refused at FinalizeLocked")

// finalizeRefusingBatch 在 FinalizeLocked 明确拒绝（与 remoteentity 的 sid 作用域拒绝同一位置：WAL 准入之前）。
type finalizeRefusingBatch struct {
	*recordingRemoteBatch
}

func (b finalizeRefusingBatch) FinalizeLocked(o entity.RemoteTransactionOutcome) error {
	_ = b.recordingRemoteBatch.FinalizeLocked(o)
	return fmt.Errorf("remote_entity: validate commit: %w", errFinalizeRefused)
}

func TestPreCommitRejectionsCarryErrCommitRejected(t *testing.T) {
	memoryRemote := HandlerMeta{Rollback: RollbackNone, Durability: DurabilityMemory}
	stateStrict := HandlerMeta{Rollback: RollbackState, Durability: DurabilityStrict}
	for i, tc := range []struct {
		name string
		meta HandlerMeta
		// fence：handler 内 fence 引擎（别的原因），提交点按 RR-06 拒绝；否则 Remote 批次在 FinalizeLocked 拒绝。
		fence bool
		cause error
		// wantValue：声明实体（初值 10，handler 改成 77）在回复之后的值。
		wantValue int
	}{
		{name: "finalize_refused/state_strict", meta: stateStrict, cause: errFinalizeRefused, wantValue: 10},
		{name: "finalize_refused/memory_remote", meta: memoryRemote, cause: errFinalizeRefused, wantValue: 77},
		{name: "fenced/state_strict", meta: stateStrict, fence: true, cause: ErrNestFenced, wantValue: 10},
		// memory handler 带 Remote 批次、emit 了 effect（升级为 strict，记录要交给 committer）：RR-06 的拒绝不撤销内存修改。
		{name: "fenced/memory_remote_with_effect", meta: memoryRemote, fence: true, cause: ErrNestFenced, wantValue: 77},
	} {
		t.Run(tc.name, func(t *testing.T) {
			localID, e := newAsyncPilotEntity(t, 49600+int64(i)*10, 10)
			getter := newMockGetter()
			remoteID := mustBuildCastID(t, 49601+int64(i)*10, entity.EntityCategoryRemote, nestRemoteManagedKind)
			getter.Add(newMockEntity(remoteID, entity.EntityCategoryRemote))
			getter.Add(e)
			recording := &recordingRemoteBatch{}
			var batch entity.RemoteWriteBatch = recording
			if !tc.fence {
				batch = finalizeRefusingBatch{recording}
			}
			manager := stagedRemoteManager{prepare: func(context.Context) (entity.RemoteWriteBatch, error) { return batch, nil }}
			committer := &recordsCommitter{}
			var mgr *NestMgr
			mgr = NewEngine(NestOptionWithGetter(getter), NestOptionWithRemoteEntityManager(manager), NestOptionWithTransactionCommitter(committer), NestOptionWithWorkerPools(WorkerPoolConfig{Workers: 1, QueueCap: 16}, WorkerPoolConfig{}, WorkerPoolConfig{}))
			name := NewHandlerName(fmt.Sprintf("rr20260927_32_%d", i))
			mgr.MustRegisterHandlerWithMeta(name, func([]entity.IThreadSafeEntity, []any, ...HandlerOption) (any, error) {
				e.dao.Value = 77
				if err := MarkPersist(e.dao, 1); err != nil && tc.meta.Rollback != RollbackNone {
					return nil, err
				}
				if tc.fence {
					if tc.meta.Rollback == RollbackNone {
						if err := Emit(Effect{Topic: "rr32.effect", Payload: []byte("x")}); err != nil {
							return nil, err
						}
					}
					mgr.Fence(errors.New("rr32: fenced by another message"))
				}
				return "ok", nil
			}, tc.meta)
			if err := mgr.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = mgr.Shutdown(context.Background()) }()
			msg, ch := GenSyncMsg(MsgTypeMulti)
			msg.Name, msg.Tids, msg.Cost, msg.HasRemote = name.String(), []int64{remoteID, localID}, true, true
			if err := mgr.dispatcher.TrySendMsg(msg); err != nil {
				t.Fatal(err)
			}
			reply := stagedWait(t, ch)
			err, _ := reply.(error)
			if !errors.Is(err, tc.cause) {
				t.Fatalf("reply=%v, want errors.Is the rejection cause %v", reply, tc.cause)
			}
			if !errors.Is(err, ErrCommitRejected) {
				t.Fatalf("a rejection before any durable record must carry ErrCommitRejected (decision table row 12 since RR-20260928-03), reply=%v", err)
			}
			for _, committed := range []error{ErrCommitIndeterminate, ErrAfterCommitFailed, ErrNestedTransactionCommitted} {
				if errors.Is(err, committed) {
					t.Fatalf("reply %v must not claim a (possible) commit via %v", err, committed)
				}
			}
			if n := committer.count(); n != 0 {
				t.Fatalf("committer received %d record(s) for a rejected transaction", n)
			}
			// 锁内拒绝时先 Abort 一次，收尾（finishRemoteWriteBatch）再 Abort 一次；Abort 幂等，这里只要求没有 Commit、至少一次 Abort。
			if c, a := recording.commits.Load(), recording.aborts.Load(); c != 0 || a == 0 {
				t.Fatalf("Remote batch Commit=%d Abort=%d, want no Commit and an Abort", c, a)
			}
			if e.dao.Value != tc.wantValue {
				t.Fatalf("declared entity value=%d after the rejection, want %d (rollback=%s)", e.dao.Value, tc.wantValue, tc.meta.Rollback)
			}
		})
	}
}

func TestPrepareCommitRecordPreservesMutationIdentity(t *testing.T) {
	tx := NewRollbackTx(RollbackUndo)
	tx.durability = DurabilityAsync
	if err := tx.AddMutation(EntityMutation{Key: dataengine.DocumentKey{ID: 7, Database: "game", Resource: "hero"}, Kind: dataengine.MutationPut, ExpectedVersion: 4, NextVersion: 5, Data: []byte{1}}); err != nil {
		t.Fatal(err)
	}

	record, err := tx.prepareCommitRecord()
	if err != nil {
		t.Fatal(err)
	}
	if record.Durability != DurabilityAsync.Record() {
		t.Fatalf("durability = %d", record.Durability)
	}
	if len(record.Mutations) != 1 {
		t.Fatalf("mutations = %d", len(record.Mutations))
	}
	mutation := record.Mutations[0]
	if mutation.Kind != dataengine.MutationPut || mutation.Key.ID != 7 || mutation.ExpectedVersion != 4 || mutation.NextVersion != 5 {
		t.Fatalf("mutation identity changed: %+v", mutation)
	}
}

func TestRollbackStateRestoresDaoAndDirty(t *testing.T) {
	t.Cleanup(ResetHandlersForTest) // 清掉本用例注册的包级 handler，-count>1 重跑不重复注册
	getter := newMockGetter()
	id := mustBuildCastID(t, 301, entity.EntityCategory(1), nestLocalKind)
	dao := &rollbackTestDao{id: id, Value: 10}
	e := &rollbackTestEntity{
		EntityBase: entity.NewEntityBase(id, entity.EntityCategory(1), false, nestLocalKind),
		dao:        dao,
	}
	getter.Add(e)

	InitNest(
		NestOptionWithGetter(getter),
		NestOptionWithWorkerPools(WorkerPoolConfig{Workers: 1, QueueCap: 64}, WorkerPoolConfig{}, WorkerPoolConfig{}),
		NestOptionWithTickDuration(100*time.Millisecond),
	)
	defer StopNest()

	committed := false
	MustRegisterHandlerWithMeta(NewHandlerName("test_rollback_state"), func(es []entity.IThreadSafeEntity, param []any, opts ...HandlerOption) (any, error) {
		ent := es[0].(*rollbackTestEntity)
		ent.dao.Value = 99
		if err := MarkPersist(ent.dao, 1); err != nil {
			return nil, err
		}
		ent.dao.Tracker.MarkSync(2)
		AfterCommit(func() { committed = true })
		return nil, errors.New("boom")
	}, HandlerMeta{Rollback: RollbackState, Durability: DurabilityMemory})

	_, err := Nest.Request(context.Background(), NewHandlerName("test_rollback_state"), id, nil)
	if err == nil {
		t.Fatal("expected handler error")
	}
	if dao.Value != 10 {
		t.Fatalf("dao value = %d, want rollback to 10", dao.Value)
	}
	if dao.Tracker.Dirty() {
		t.Fatal("dirty mask should be restored to clean")
	}
	if committed {
		t.Fatal("after commit callback should not run on rollback")
	}
}

func TestRollbackAfterCommitRunsOnSuccess(t *testing.T) {
	t.Cleanup(ResetHandlersForTest) // 清掉本用例注册的包级 handler，-count>1 重跑不重复注册
	getter := newMockGetter()
	id := mustBuildCastID(t, 303, entity.EntityCategory(1), nestLocalKind)
	dao := &rollbackTestDao{id: id, Value: 10}
	e := &rollbackTestEntity{
		EntityBase: entity.NewEntityBase(id, entity.EntityCategory(1), false, nestLocalKind),
		dao:        dao,
	}
	getter.Add(e)

	InitNest(
		NestOptionWithGetter(getter),
		NestOptionWithWorkerPools(WorkerPoolConfig{Workers: 1, QueueCap: 64}, WorkerPoolConfig{}, WorkerPoolConfig{}),
		NestOptionWithTickDuration(100*time.Millisecond),
	)
	defer StopNest()

	committed := make(chan struct{}, 1)
	MustRegisterHandlerWithMeta(NewHandlerName("test_rollback_commit"), func(es []entity.IThreadSafeEntity, param []any, opts ...HandlerOption) (any, error) {
		ent := es[0].(*rollbackTestEntity)
		ent.dao.Value = 20
		if !AfterCommit(func() { committed <- struct{}{} }) {
			return nil, errors.New("missing rollback tx")
		}
		return "ok", nil
	}, HandlerMeta{Rollback: RollbackState, Durability: DurabilityMemory})

	ret, err := Nest.Request(context.Background(), NewHandlerName("test_rollback_commit"), id, nil)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if ret != "ok" || dao.Value != 20 {
		t.Fatalf("ret=%v value=%d", ret, dao.Value)
	}
	select {
	case <-committed:
	case <-time.After(time.Second):
		t.Fatal("after commit callback was not called")
	}
}

func TestRollbackUndoRestoresStateAndDirty(t *testing.T) {
	t.Cleanup(ResetHandlersForTest) // 清掉本用例注册的包级 handler，-count>1 重跑不重复注册
	getter := newMockGetter()
	id := mustBuildCastID(t, 304, entity.EntityCategory(1), nestLocalKind)
	dao := &rollbackTestDao{id: id, Value: 10}
	getter.Add(&rollbackTestEntity{
		EntityBase: entity.NewEntityBase(id, entity.EntityCategory(1), false, nestLocalKind),
		dao:        dao,
	})

	InitNest(
		NestOptionWithGetter(getter),
		NestOptionWithWorkerPools(WorkerPoolConfig{Workers: 1, QueueCap: 64}, WorkerPoolConfig{}, WorkerPoolConfig{}),
		NestOptionWithTickDuration(100*time.Millisecond),
	)
	defer StopNest()

	MustRegisterHandlerWithMeta(NewHandlerName("test_rollback_undo"), func(es []entity.IThreadSafeEntity, _ []any, _ ...HandlerOption) (any, error) {
		ent := es[0].(*rollbackTestEntity)
		old := ent.dao.Value
		if !RecordUndo(ent.dao, 1, func() error {
			ent.dao.Value = old
			return nil
		}) {
			return nil, errors.New("missing undo transaction")
		}
		ent.dao.Value = 99
		if err := MarkPersist(ent.dao, 1); err != nil {
			return nil, err
		}
		return nil, errors.New("boom")
	}, HandlerMeta{Rollback: RollbackUndo, Durability: DurabilityMemory})

	_, err := Nest.Request(context.Background(), NewHandlerName("test_rollback_undo"), id, nil)
	if err == nil {
		t.Fatal("expected handler error")
	}
	if dao.Value != 10 || dao.Tracker.Dirty() {
		t.Fatalf("value=%d dirty=%v, want value=10 dirty=false", dao.Value, dao.Tracker.Dirty())
	}
}

func TestStrictCommitFailureRollsBack(t *testing.T) {
	t.Cleanup(ResetHandlersForTest) // 清掉本用例注册的包级 handler，-count>1 重跑不重复注册
	getter := newMockGetter()
	id := mustBuildCastID(t, 305, entity.EntityCategory(1), nestLocalKind)
	dao := &rollbackTestDao{id: id, Value: 10}
	getter.Add(&rollbackTestEntity{
		EntityBase: entity.NewEntityBase(id, entity.EntityCategory(1), false, nestLocalKind),
		dao:        dao,
	})
	committer := &recordingCommitter{err: errors.New("wal fsync failed")}

	InitNest(
		NestOptionWithGetter(getter),
		NestOptionWithTransactionCommitter(committer),
		NestOptionWithWorkerPools(WorkerPoolConfig{Workers: 1, QueueCap: 64}, WorkerPoolConfig{}, WorkerPoolConfig{}),
		NestOptionWithTickDuration(100*time.Millisecond),
	)
	defer StopNest()

	MustRegisterHandlerWithMeta(NewHandlerName("test_strict_commit_failure"), func(es []entity.IThreadSafeEntity, _ []any, _ ...HandlerOption) (any, error) {
		ent := es[0].(*rollbackTestEntity)
		old := ent.dao.Value
		if !RecordUndo(ent.dao, 1, func() error { ent.dao.Value = old; return nil }) {
			return nil, errors.New("missing undo transaction")
		}
		ent.dao.Value = 20
		if err := MarkPersist(ent.dao, 1); err != nil {
			return nil, err
		}
		return "not-committed", nil
	}, HandlerMeta{Rollback: RollbackUndo, Durability: DurabilityStrict})

	ret, err := Nest.Request(context.Background(), NewHandlerName("test_strict_commit_failure"), id, nil)
	if !errors.Is(err, ErrCommitRejected) {
		t.Fatalf("err=%v, want %v", err, ErrCommitRejected)
	}
	if ret != nil {
		t.Fatalf("ret=%v, want nil", ret)
	}
	if dao.Value != 10 || dao.Tracker.Dirty() {
		t.Fatalf("value=%d dirty=%v, want rollback", dao.Value, dao.Tracker.Dirty())
	}
	if len(committer.record.Mutations) != 1 || committer.record.Mutations[0].Key.ID != id {
		t.Fatalf("commit record=%+v", committer.record)
	}
}

func TestStrictCommitSuccessKeepsState(t *testing.T) {
	t.Cleanup(ResetHandlersForTest) // 清掉本用例注册的包级 handler，-count>1 重跑不重复注册
	getter := newMockGetter()
	id := mustBuildCastID(t, 306, entity.EntityCategory(1), nestLocalKind)
	dao := &rollbackTestDao{id: id, Value: 10}
	getter.Add(&rollbackTestEntity{
		EntityBase: entity.NewEntityBase(id, entity.EntityCategory(1), false, nestLocalKind),
		dao:        dao,
	})
	committer := &recordingCommitter{}

	InitNest(
		NestOptionWithGetter(getter),
		NestOptionWithTransactionCommitter(committer),
		NestOptionWithWorkerPools(WorkerPoolConfig{Workers: 1, QueueCap: 64}, WorkerPoolConfig{}, WorkerPoolConfig{}),
		NestOptionWithTickDuration(100*time.Millisecond),
	)
	defer StopNest()

	MustRegisterHandlerWithMeta(NewHandlerName("test_strict_commit_success"), func(es []entity.IThreadSafeEntity, _ []any, _ ...HandlerOption) (any, error) {
		ent := es[0].(*rollbackTestEntity)
		old := ent.dao.Value
		if !RecordUndo(ent.dao, 1, func() error { ent.dao.Value = old; return nil }) {
			return nil, errors.New("missing undo transaction")
		}
		ent.dao.Value = 20
		if err := MarkPersist(ent.dao, 1); err != nil {
			return nil, err
		}
		return "committed", nil
	}, HandlerMeta{Rollback: RollbackUndo, Durability: DurabilityStrict})

	ret, err := Nest.Request(context.Background(), NewHandlerName("test_strict_commit_success"), id, nil)
	if err != nil || ret != "committed" {
		t.Fatalf("ret=%v err=%v", ret, err)
	}
	if dao.Value != 20 || len(committer.record.Mutations) != 1 {
		t.Fatalf("value=%d record=%+v", dao.Value, committer.record)
	}
	if committer.released != committer.record.ID {
		t.Fatalf("released=%s record=%s", committer.released.String(), committer.record.ID.String())
	}
}

func TestIndeterminateCommitDoesNotRollback(t *testing.T) {
	t.Cleanup(ResetHandlersForTest) // 清掉本用例注册的包级 handler，-count>1 重跑不重复注册
	getter := newMockGetter()
	id := mustBuildCastID(t, 307, entity.EntityCategory(1), nestLocalKind)
	dao := &rollbackTestDao{id: id, Value: 10}
	getter.Add(&rollbackTestEntity{
		EntityBase: entity.NewEntityBase(id, entity.EntityCategory(1), false, nestLocalKind),
		dao:        dao,
	})
	committer := &recordingCommitter{err: errors.Join(ErrCommitIndeterminate, errors.New("fsync outcome unknown"))}

	InitNest(
		NestOptionWithGetter(getter),
		NestOptionWithTransactionCommitter(committer),
		NestOptionWithWorkerPools(WorkerPoolConfig{Workers: 1, QueueCap: 64}, WorkerPoolConfig{}, WorkerPoolConfig{}),
		NestOptionWithTickDuration(100*time.Millisecond),
	)
	defer StopNest()

	MustRegisterHandlerWithMeta(NewHandlerName("test_indeterminate_commit"), func(es []entity.IThreadSafeEntity, _ []any, _ ...HandlerOption) (any, error) {
		ent := es[0].(*rollbackTestEntity)
		old := ent.dao.Value
		if !RecordUndo(ent.dao, 1, func() error { ent.dao.Value = old; return nil }) {
			return nil, errors.New("missing undo transaction")
		}
		ent.dao.Value = 20
		if err := MarkPersist(ent.dao, 1); err != nil {
			return nil, err
		}
		return "unknown", nil
	}, HandlerMeta{Rollback: RollbackUndo, Durability: DurabilityStrict})

	ret, err := Nest.Request(context.Background(), NewHandlerName("test_indeterminate_commit"), id, nil)
	if !errors.Is(err, ErrCommitIndeterminate) || ret != nil {
		t.Fatalf("ret=%v err=%v", ret, err)
	}
	if dao.Value != 20 || dao.Tracker.Version() != 0 || dao.Tracker.Dirty() {
		t.Fatalf("value=%d version=%d sync_dirty=%v, indeterminate state must remain for WAL recovery without DAO persistence dirty", dao.Value, dao.Tracker.Version(), dao.Tracker.Dirty())
	}
	if !committer.released.IsZero() {
		t.Fatalf("indeterminate transaction was released to delivery: %s", committer.released.String())
	}
}

func TestRecordUndoTokenSeparatesMapKeys(t *testing.T) {
	tx := NewRollbackTx(RollbackUndo)
	values := map[string]int{"a": 1, "b": 2}
	if err := tx.RecordUndoToken(&values, 1, "a", func() error { values["a"] = 1; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := tx.RecordUndoToken(&values, 1, "b", func() error { values["b"] = 2; return nil }); err != nil {
		t.Fatal(err)
	}
	// A repeated write to the same field/key must keep the first before-image.
	if err := tx.RecordUndoToken(&values, 1, "a", func() error { values["a"] = 99; return nil }); err != nil {
		t.Fatal(err)
	}
	values["a"], values["b"] = 10, 20
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if values["a"] != 1 || values["b"] != 2 {
		t.Fatalf("values=%v", values)
	}
}

func TestEmitUpgradesTransactionToStrictDurability(t *testing.T) {
	tx := NewRollbackTx(RollbackUndo)
	if err := tx.Emit(Effect{Topic: "player.changed", Payload: []byte("event")}); err != nil {
		t.Fatal(err)
	}
	if tx.durability != DurabilityStrict {
		t.Fatalf("durability=%s, want strict", tx.durability.String())
	}
	if len(tx.effects) != 1 || tx.effects[0].ID == "" {
		t.Fatalf("effects=%+v", tx.effects)
	}
}

type promiseParticipant struct{ calls int }

func (p *promiseParticipant) PrepareCommit(*RollbackTx) error { p.calls++; return nil }

// A committed or rolled-back transaction accepts nothing more; undo owners,
// tokens and participants must be comparable because they key a map; a
// mutation must have a complete document identity.
func TestRollbackTxRefusesLateAndUncomparableRegistrations(t *testing.T) {
	tx := NewRollbackTx(RollbackUndo)
	owner := &rollbackTestDao{}
	if err := tx.RecordUndo(owner, 1, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := tx.RecordUndo([]int{1}, 1, func() error { return nil }); err == nil || !strings.Contains(err.Error(), "undo owner is not comparable") {
		t.Fatalf("slice owner = %v", err)
	}
	if err := tx.RecordUndoToken(owner, 1, []int{1}, func() error { return nil }); err == nil || !strings.Contains(err.Error(), "undo token is not comparable") {
		t.Fatalf("slice token = %v", err)
	}
	if err := tx.RecordUndo(nil, 1, func() error { return nil }); err == nil || !strings.Contains(err.Error(), "invalid undo operation") {
		t.Fatalf("nil owner = %v", err)
	}
	if err := tx.AddMutation(EntityMutation{Key: dataengine.DocumentKey{Database: "g", Resource: "r", ID: 0}, Kind: dataengine.MutationPut, NextVersion: 1, Data: []byte("x")}); !errors.Is(err, dataengine.ErrInvalidDocumentKey) {
		t.Fatalf("missing mutation identity = %v", err)
	}
	participant := &promiseParticipant{}
	if err := tx.RegisterCommitParticipant(participant); err != nil {
		t.Fatal(err)
	}
	if err := tx.RegisterCommitParticipant(participant); err != nil || len(tx.participants) != 1 {
		t.Fatalf("re-registering the same participant: err=%v participants=%d, want dedupe", err, len(tx.participants))
	}
	tx.Commit()
	if err := tx.RecordUndo(owner, 2, func() error { return nil }); !errors.Is(err, ErrTransactionClosed) {
		t.Fatalf("RecordUndo after Commit = %v", err)
	}
	if err := tx.RegisterCommitParticipant(&promiseParticipant{}); !errors.Is(err, ErrTransactionClosed) {
		t.Fatalf("RegisterCommitParticipant after Commit = %v", err)
	}
	if err := tx.AddMutation(EntityMutation{}); !errors.Is(err, ErrTransactionClosed) {
		t.Fatalf("AddMutation after Commit = %v", err)
	}
}
