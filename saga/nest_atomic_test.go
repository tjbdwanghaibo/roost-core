package saga_test

import (
	"context"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/dataengine"
	"github.com/tjbdwanghaibo/roost-core/entity"
	"github.com/tjbdwanghaibo/roost-core/nest"
	"github.com/tjbdwanghaibo/roost-core/saga"
	"go.mongodb.org/mongo-driver/v2/bson"
)

const sagaNativeTestKind entity.EntityKind = 242

type sagaNativeDAO struct {
	id      int64
	tracker dataengine.Tracker
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

func TestNativeSagaStepCommitsMutationReceiptAndCompletionEffectAtomically(t *testing.T) {
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
	command := saga.Command{ID: "command-1", IdempotencyKey: "operation-1", SagaID: "saga-1", SagaType: "rally", DefinitionVersion: 1, BusinessKey: "r-1", StepName: "reserve", Phase: saga.PhaseForward, Attempt: 1, Topic: "rally.reserve", CreatedAt: now, DeadlineAt: now.Add(time.Minute)}
	handler := nest.NewHandlerName("test_native_saga_dataengine_atomic")
	nest.MustRegisterHandlerWithMeta(handler, func([]entity.IThreadSafeEntity, []any, ...nest.HandlerOption) (any, error) {
		if err := saga.BindCommand(command, now.Add(time.Hour)); err != nil {
			return nil, err
		}
		if err := nest.MarkPersist(dao, 1); err != nil {
			return nil, err
		}
		completion := saga.Completion{CommandID: command.ID, IdempotencyKey: command.IdempotencyKey, SagaID: command.SagaID, Success: true, Data: []byte("reserved"), CompletedAt: now}
		return nil, saga.EmitCompletion(completion)
	}, nest.HandlerMeta{Rollback: nest.RollbackUndo, Durability: nest.DurabilityStrict})
	engine := nest.NewEngine(nest.NestOptionWithGetter(sagaNativeGetter{value: value}), nest.NestOptionWithTransactionCommitter(committer), nest.NestOptionWithWorkerNumAndMsgCap(1, 1, 16))
	if err := engine.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = engine.Shutdown(context.Background()); nest.ResetHandlersForTest() }()
	if _, err := engine.Request(context.Background(), handler, id, nil); err != nil {
		t.Fatal(err)
	}
	record := committer.record
	if len(record.Mutations) != 1 || len(record.Receipts) != 1 || len(record.Effects) != 1 {
		t.Fatalf("record=%+v", record)
	}
	if record.Receipts[0].Namespace != saga.StepReceiptNamespace || record.Receipts[0].ID != command.ID {
		t.Fatalf("receipt=%+v", record.Receipts[0])
	}
	if record.Effects[0].ID != "saga-completion:"+command.ID {
		t.Fatalf("effect=%+v", record.Effects[0])
	}
	if completion, err := saga.DecodeCompletionEffect(record.Receipts[0].Payload); err != nil || completion.CommandID != command.ID {
		t.Fatalf("receipt completion=%+v err=%v", completion, err)
	}
}
