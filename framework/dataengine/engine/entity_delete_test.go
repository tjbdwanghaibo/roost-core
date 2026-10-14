// 删除准入：事务内删除延后到准入、回滚保留实体、准入 panic fence 并停止服务、远端 / 本地删除的前置条件。

package engine

import (
	"context"
	"errors"
	"strings"
	"testing"

	coredata "github.com/tjbdwanghaibo/roost-core/framework/dataengine"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	corenest "github.com/tjbdwanghaibo/roost-core/framework/nest"
	"github.com/tjbdwanghaibo/roost-core/framework/remoteentity"
	"github.com/tjbdwanghaibo/roost-core/infra/base/fctx"
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

func TestShortWorkerRemoteDeleteIsDefiniteRejection(t *testing.T) {
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
	_, release := fctx.NewContext(fctx.WithShortWorker(), fctx.WithHandler("default_handler"))
	destroyErr := runtime.access.Destroy(context.Background(), remote, entity.DestroyReasonCommon, true)
	release()
	if !errors.Is(destroyErr, fctx.ErrBlockingInBusinessWorker) {
		t.Errorf("Destroy err=%v, want errors.Is(fctx.ErrBlockingInBusinessWorker)", destroyErr)
	}
	if len(fatal) != 0 {
		t.Errorf("fast-worker rejection before any side effect escalated to runtime fatal: %v", fatal)
	}
	if manager.Get(remote.ID()) != remote {
		t.Error("definitively rejected delete removed the entity from memory")
	}
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

func TestDeleteAdmissionRejectsUnconfiguredRuntime(t *testing.T) {
	ctx := context.Background()
	value := newDeleteTestEntity(801)
	cases := []struct {
		name    string
		runtime *Runtime
		value   entity.IThreadSafeEntity
	}{
		{"nil runtime", nil, value},
		{"runtime without projector", &Runtime{access: entity.NewManagerAccess(entity.NewEntityManager())}, value},
		{"runtime without access", &Runtime{Projector: &Projector{}}, value},
		{"nil entity", newDeleteRuntime(entity.NewEntityManager(), nil), nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			admission, err := tc.runtime.admitEntityDelete(ctx, tc.value, 1)
			if err == nil || !strings.Contains(err.Error(), "runtime is not configured") || admission != entity.DeleteAdmissionImmediate {
				t.Fatalf("admission=%v err=%v", admission, err)
			}
		})
	}
}

func TestDeleteAdmissionInsideTransactionRequiresPreparerAndDeclaredRemoteTarget(t *testing.T) {
	ensureDeleteGuardKinds()
	ctx := context.Background()
	manager := entity.NewEntityManager()
	runtime := newDeleteRuntime(manager, nil)

	bare := &bareDeleteEntity{EntityBase: entity.NewEntityBase(802, entity.EntityCategory(1), false, bareDeleteTestKind)}
	committer := &deleteRecordingCommitter{}
	// 事务体可能在另一个 goroutine 上运行：断言放在事务外面。
	var admission entity.DeleteAdmission
	var admitErr error
	if _, err := corenest.RunIsolatedTransaction(ctx, committer, "delete-test", func() (any, error) {
		admission, admitErr = runtime.admitEntityDelete(ctx, bare, 1)
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	if admitErr == nil || !strings.Contains(admitErr.Error(), "no generated delete preparer") || admission != entity.DeleteAdmissionImmediate {
		t.Fatalf("bare entity inside tx: admission=%v err=%v", admission, admitErr)
	}

	remote := newRemoteDeleteEntity(803)
	if _, err := corenest.RunIsolatedTransaction(ctx, committer, "delete-test", func() (any, error) {
		admission, admitErr = runtime.admitEntityDelete(ctx, remote, 1)
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(admitErr, corenest.ErrDurableRemoteWriteUnsupported) || admission != entity.DeleteAdmissionImmediate {
		t.Fatalf("undeclared remote target inside tx: admission=%v err=%v", admission, admitErr)
	}
	if len(committer.records) != 0 {
		t.Fatalf("rejected admissions committed %d records", len(committer.records))
	}
}

func TestRemoteDeleteAdmissionRequiresRemoteWriteCapability(t *testing.T) {
	ensureDeleteGuardKinds()
	ctx := context.Background()
	remote := newRemoteDeleteEntity(804)

	noManager := newDeleteRuntime(entity.NewEntityManager(), nil)
	if admission, err := noManager.admitEntityDelete(ctx, remote, 1); !errors.Is(err, entity.ErrRemoteWriteCapabilityDisabled) || admission != entity.DeleteAdmissionImmediate {
		t.Fatalf("without remote manager: admission=%v err=%v", admission, err)
	}

	nilBatch := newDeleteRuntime(entity.NewEntityManager(), batchManager{})
	if admission, err := nilBatch.admitEntityDelete(ctx, remote, 1); !errors.Is(err, entity.ErrRemoteWriteCapabilityDisabled) || admission != entity.DeleteAdmissionImmediate {
		t.Fatalf("manager handing out no batch: admission=%v err=%v", admission, err)
	}

	wantErr := errors.New("ownership lost")
	failing := newDeleteRuntime(entity.NewEntityManager(), batchManager{err: wantErr})
	if admission, err := failing.admitEntityDelete(ctx, remote, 1); !errors.Is(err, wantErr) || admission != entity.DeleteAdmissionImmediate {
		t.Fatalf("manager refusing the batch: admission=%v err=%v", admission, err)
	}
}

// 事务外的本地删除走 admitLocalEntityDelete：没有生成的删除准备器同样必须在
// 开启隔离事务之前拒绝，而不是拿着 nil 准备器进事务。
func TestLocalDeleteAdmissionRequiresPreparer(t *testing.T) {
	ensureDeleteGuardKinds()
	runtime := newDeleteRuntime(entity.NewEntityManager(), nil)
	bare := &bareDeleteEntity{EntityBase: entity.NewEntityBase(805, entity.EntityCategory(1), false, bareDeleteTestKind)}
	admission, err := runtime.admitEntityDelete(context.Background(), bare, 1)
	if err == nil || !strings.Contains(err.Error(), "no generated delete preparer") || admission != entity.DeleteAdmissionImmediate {
		t.Fatalf("bare entity outside tx: admission=%v err=%v", admission, err)
	}
}
