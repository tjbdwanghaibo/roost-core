package saga_test

import (
	"context"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/dataengine"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/nest"
	"github.com/tjbdwanghaibo/roost-core/framework/saga"
	"go.mongodb.org/mongo-driver/v2/bson"
)

const sagaNativeTestKind entity.EntityKind = 242

type sagaNativeDAO struct {
	receipts map[string][]byte
	id       int64
	tracker  dataengine.Tracker
}

func (dao *sagaNativeDAO) Id() int64                         { return dao.id }
func (dao *sagaNativeDAO) SetId(id int64)                    { dao.id = id }
func (*sagaNativeDAO) DbName() string                        { return "game" }
func (*sagaNativeDAO) CollName() string                      { return "saga_native" }
func (dao *sagaNativeDAO) Dirty() entity.IDirty              { return &dao.tracker }
func (dao *sagaNativeDAO) CleanDirty()                       { dao.tracker.SelfClean() }
func (dao *sagaNativeDAO) DirtyTracker() *dataengine.Tracker { return &dao.tracker }
func (dao *sagaNativeDAO) PrepareMutation(change nest.PersistChange) (dataengine.Mutation, error) {
	raw, _ := bson.Marshal(bson.M{"_id": dao.id, "value": 1})
	version := dao.tracker.Version()
	return dataengine.Mutation{
		Key:  dataengine.DocumentKey{Database: "game", Resource: dao.CollName(), ID: dao.id},
		Kind: dataengine.MutationPut, ExpectedVersion: version, NextVersion: version + 1,
		Mask: change.Mask, Schema: 1, Codec: "bson-v2", Data: raw,
	}, nil
}
func (dao *sagaNativeDAO) AcceptMutation(mutation dataengine.Mutation) error {
	return dao.tracker.AcceptVersion(mutation.ExpectedVersion, mutation.NextVersion)
}

type sagaNativeEntity struct {
	*entity.EntityBase
	dao *sagaNativeDAO
}

func (value *sagaNativeEntity) Base() *entity.EntityBase                 { return value.EntityBase }
func (*sagaNativeEntity) OnInitFinish(*entity.EntityCreateParam) error   { return nil }
func (*sagaNativeEntity) OnDestroy(entity.EntityDestroyReason)           {}
func (*sagaNativeEntity) AutoPersist() bool                              { return true }
func (*sagaNativeEntity) IsRemoved() bool                                { return false }
func (*sagaNativeEntity) SetRemoved()                                    {}
func (*sagaNativeEntity) Touch() bool                                    { return true }
func (*sagaNativeEntity) UnTouch()                                       {}
func (*sagaNativeEntity) ClearBase()                                     {}
func (*sagaNativeEntity) IsClear() bool                                  { return false }
func (value *sagaNativeEntity) RangeDao(visit func(entity.DaoInterface)) { visit(value.dao) }

// SagaStepReceipts 让这个实体成为原生步骤的回执宿主：回执写进 DAO 字段（掩码位 2），与业务字段（位 1）同一份文档、同一次提交。
func (value *sagaNativeEntity) SagaStepReceipts() saga.StepReceipts {
	return sagaNativeReceipts{value.dao}
}

type sagaNativeReceipts struct{ dao *sagaNativeDAO }

func (r sagaNativeReceipts) StepReceipt(operation string) ([]byte, bool) {
	raw, ok := r.dao.receipts[operation]
	return raw, ok
}

func (r sagaNativeReceipts) PutStepReceipt(operation string, encoded []byte) {
	if r.dao.receipts == nil {
		r.dao.receipts = map[string][]byte{}
	}
	r.dao.receipts[operation] = encoded
	if err := nest.MarkPersist(r.dao, 2); err != nil {
		panic(err)
	}
}

func (r sagaNativeReceipts) DeleteStepReceipt(operation string) { delete(r.dao.receipts, operation) }

func (r sagaNativeReceipts) RangeStepReceipts(f func(string, []byte) bool) {
	for operation, raw := range r.dao.receipts {
		if !f(operation, raw) {
			return
		}
	}
}

type sagaNativeGetter struct{ value entity.IThreadSafeEntity }

func (getter sagaNativeGetter) Get(context.Context, int64, entity.EntityCategory) (entity.IThreadSafeEntity, error) {
	return getter.value, nil
}
func (getter sagaNativeGetter) GetMany(_ context.Context, ids []int64, _ []entity.EntityCategory) ([]entity.IThreadSafeEntity, error) {
	values := make([]entity.IThreadSafeEntity, len(ids))
	for index := range values {
		values[index] = getter.value
	}
	return values, nil
}

type sagaNativeCommitter struct{ record dataengine.CommitRecord }

func (committer *sagaNativeCommitter) Commit(_ context.Context, record dataengine.CommitRecord) error {
	committer.record = dataengine.CloneCommitRecord(record)
	return nil
}

// 原生步骤的业务修改与这个操作的回执是同一笔 Nest 事务的同一条 CommitRecord（实体里的回执随实体文档一起持久），没有另外的
// DataEngine 回执或 completion effect；结论在提交之后交给这次运行（AfterCommit），消费者据此把结果发回协调器。
// 历史：TestNativeSagaStepCommitsMutationReceiptAndCompletionEffectAtomically（旧路径：业务修改 + DataEngine 回执 + completion effect）。
func TestNativeSagaStepCommitsMutationAndReceiptAtomically(t *testing.T) {
	entity.MustRegisterEntityKindDefs(entity.EntityKindDef{Kind: sagaNativeTestKind, Category: 1})
	id, err := entity.BuildEntityID(81, sagaNativeTestKind)
	if err != nil {
		t.Fatal(err)
	}
	dao := &sagaNativeDAO{id: id}
	value := &sagaNativeEntity{EntityBase: entity.NewEntityBase(id, 1, false, sagaNativeTestKind), dao: dao}
	committer := &sagaNativeCommitter{}
	nest.ResetHandlersForTest()
	now := time.Now().UTC()
	command := saga.Command{ID: "operation-1:1", IdempotencyKey: "operation-1", SagaID: "saga-1", SagaType: "rally", DefinitionVersion: 1, BusinessKey: "r-1", StepName: "reserve", Phase: saga.PhaseForward, Attempt: 1, Topic: "rally.reserve", Payload: []byte("state"), CreatedAt: now, DeadlineAt: now.Add(time.Minute)}
	var current saga.NativeStep
	handler := nest.NewHandlerName("test_native_saga_dataengine_atomic")
	nest.MustRegisterHandlerWithMeta(handler, func([]entity.IThreadSafeEntity, []any, ...nest.HandlerOption) (any, error) {
		if replayed, err := current.Begin(value); replayed || err != nil {
			return nil, err
		}
		if err := nest.MarkPersist(dao, 1); err != nil {
			return nil, err
		}
		return nil, current.Complete(true, "")
	}, nest.HandlerMeta{Rollback: nest.RollbackUndo, Durability: nest.DurabilityStrict})
	engine := nest.NewEngine(nest.NestOptionWithGetter(sagaNativeGetter{value: value}), nest.NestOptionWithTransactionCommitter(committer), nest.NestOptionWithWorkerPools(nest.WorkerPoolConfig{Workers: 1, QueueCap: 16}, nest.WorkerPoolConfig{}, nest.WorkerPoolConfig{}))
	if err := engine.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = engine.Shutdown(context.Background()); nest.ResetHandlersForTest() }()
	outcome, err := saga.RunNativeStep(context.Background(), command, time.Hour, func(ctx context.Context, step saga.NativeStep) error {
		current = step
		_, err := engine.Request(ctx, handler, id, nil)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	record := committer.record
	if len(record.Mutations) != 1 || len(record.Receipts) != 0 || len(record.Effects) != 0 {
		t.Fatalf("record=%+v, want one document mutation carrying both the business field and the receipt, no DataEngine receipt or effect", record)
	}
	if mask := record.Mutations[0].Mask; mask&1 == 0 || mask&2 == 0 {
		t.Fatalf("mutation mask %b, want the business bit and the receipt bit in the same mutation", mask)
	}
	if _, ok := dao.receipts[command.IdempotencyKey]; !ok {
		t.Fatal("the receipt was not written into the entity")
	}
	if outcome.Kind != saga.StepExecuted || !outcome.Completion.Success || outcome.Completion.CommandID != command.ID {
		t.Fatalf("outcome = %+v, want the executed success handed over after the commit", outcome)
	}
}
