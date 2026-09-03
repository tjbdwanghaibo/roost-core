package dataengine

import (
	"context"
	"errors"
	"testing"

	coredata "github.com/tjbdwanghaibo/roost-core/dataengine"
	"github.com/tjbdwanghaibo/roost-core/entity"
	corenest "github.com/tjbdwanghaibo/roost-core/nest"
)

const deleteTestKind entity.EntityKind = 71

type deleteTestEntity struct {
	*entity.EntityBase
	prepared int
}

type panickingDeleteTestEntity struct{ *deleteTestEntity }

func (*panickingDeleteTestEntity) PrepareDelete(*corenest.RollbackTx) error {
	panic("generated delete panic")
}

func newDeleteTestEntity(id int64) *deleteTestEntity {
	return &deleteTestEntity{EntityBase: entity.NewEntityBase(id, entity.EntityCategory(1), false, deleteTestKind)}
}

func (value *deleteTestEntity) Base() *entity.EntityBase       { return value.EntityBase }
func (*deleteTestEntity) OnDestroy(entity.EntityDestroyReason) {}
func (value *deleteTestEntity) PrepareDelete(tx *corenest.RollbackTx) error {
	value.prepared++
	return tx.AddMutation(coredata.Mutation{
		Key:  coredata.DocumentKey{Database: "game", Resource: "delete_test", ID: value.ID()},
		Kind: coredata.MutationDelete, ExpectedVersion: 3, NextVersion: 4, Schema: 1, Codec: "bson-v2",
	})
}

type deleteRecordingCommitter struct {
	records []corenest.CommitRecord
}

func (committer *deleteRecordingCommitter) Commit(_ context.Context, record corenest.CommitRecord) error {
	committer.records = append(committer.records, coredata.CloneCommitRecord(record))
	return nil
}

func TestDataEngineDeleteDefersMemoryRemovalUntilTransactionAdmission(t *testing.T) {
	manager := entity.NewEntityManager()
	value := newDeleteTestEntity(701)
	manager.Add(value)
	runtime := &Runtime{Projector: &Projector{}, access: entity.NewManagerAccess(manager)}
	unregister, err := runtime.access.RegisterDeleteAdmitter(runtime.admitEntityDelete)
	if err != nil {
		t.Fatal(err)
	}
	defer unregister()
	committer := &deleteRecordingCommitter{}

	_, err = corenest.RunIsolatedTransaction(context.Background(), committer, "delete-test", func() (any, error) {
		if err := manager.Destroy(context.Background(), value, 1, true); err != nil {
			return nil, err
		}
		if manager.Get(value.ID()) != value || value.IsRemoved() {
			t.Fatal("entity was removed before transaction admission")
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(committer.records) != 1 || len(committer.records[0].Mutations) != 1 || committer.records[0].Mutations[0].Kind != coredata.MutationDelete {
		t.Fatalf("delete records=%+v", committer.records)
	}
	if manager.Get(701) != nil || !value.IsRemoved() || value.prepared != 1 {
		t.Fatalf("delete was not finalized: managed=%v removed=%t prepared=%d", manager.Get(701), value.IsRemoved(), value.prepared)
	}
}

func TestDataEngineDeleteRollbackLeavesEntityLive(t *testing.T) {
	manager := entity.NewEntityManager()
	value := newDeleteTestEntity(702)
	manager.Add(value)
	runtime := &Runtime{Projector: &Projector{}, access: entity.NewManagerAccess(manager)}
	unregister, err := runtime.access.RegisterDeleteAdmitter(runtime.admitEntityDelete)
	if err != nil {
		t.Fatal(err)
	}
	defer unregister()
	want := errors.New("business rollback")
	_, err = corenest.RunIsolatedTransaction(context.Background(), &deleteRecordingCommitter{}, "delete-test", func() (any, error) {
		if err := manager.Destroy(context.Background(), value, 1, true); err != nil {
			return nil, err
		}
		return nil, want
	})
	if !errors.Is(err, want) {
		t.Fatalf("err=%v", err)
	}
	if manager.Get(702) != value || value.IsRemoved() {
		t.Fatal("rolled-back delete removed the entity")
	}
}

func TestDataEngineDeleteAdmissionPanicFencesAndStopsServingEntity(t *testing.T) {
	manager := entity.NewEntityManager()
	value := &panickingDeleteTestEntity{deleteTestEntity: newDeleteTestEntity(703)}
	manager.Add(value)
	var fatal error
	runtime := &Runtime{
		Projector: &Projector{}, access: entity.NewManagerAccess(manager),
		onFatal: func(err error) { fatal = err },
	}
	unregister, err := runtime.access.RegisterDeleteAdmitter(runtime.admitEntityDelete)
	if err != nil {
		t.Fatal(err)
	}
	defer unregister()
	committer := &deleteRecordingCommitter{}
	_, err = corenest.RunIsolatedTransaction(context.Background(), committer, "delete-test", func() (any, error) {
		return nil, manager.Destroy(context.Background(), value, 1, true)
	})
	if err == nil || fatal == nil {
		t.Fatalf("err=%v fatal=%v, want fenced indeterminate outcome", err, fatal)
	}
	if manager.Get(703) != nil {
		t.Fatal("admission panic left possibly deleted state reachable")
	}
	if len(committer.records) != 0 {
		t.Fatalf("panicking admission committed %d records", len(committer.records))
	}
}
