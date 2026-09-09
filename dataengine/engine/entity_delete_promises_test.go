package engine

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/entity"
	corenest "github.com/tjbdwanghaibo/roost-core/nest"
)

// U-0109 · C2（空洞测试）· `dataengine/engine` 删除准入的其余拒绝。
//
// 删除准入决定一个实体能不能从内存里消失：运行时没配好、实体没有生成的
// 删除准备器、远端实体没在 Nest 消息里声明、远端写能力不存在——每一条都必
// 须以 Immediate + 错误拒绝，而不是把实体删了却没有持久化的删除。

const (
	remoteDeleteTestKind entity.EntityKind = 72
	bareDeleteTestKind   entity.EntityKind = 73
)

var registerDeleteGuardKinds sync.Once

func ensureDeleteGuardKinds() {
	registerDeleteGuardKinds.Do(func() {
		build := func(param *entity.EntityCreateParam) (entity.IThreadSafeEntity, error) {
			return newDeleteTestEntity(param.Id), nil
		}
		entity.RegisterEntityBuilder(&entity.EntityBuilderParam{
			Category: 1, Kind: remoteDeleteTestKind, Builder: build,
			RemotePolicy: entity.RemotePolicyManaged, Lifetime: entity.EntityLifetimeRemoteManaged,
		})
		entity.RegisterEntityBuilder(&entity.EntityBuilderParam{Category: 1, Kind: bareDeleteTestKind, Builder: build})
	})
}

// bareDeleteEntity persists but has no generated PrepareDelete.
type bareDeleteEntity struct{ *entity.EntityBase }

func (value *bareDeleteEntity) Base() *entity.EntityBase        { return value.EntityBase }
func (*bareDeleteEntity) OnDestroy(entity.EntityDestroyReason) {}

func newRemoteDeleteEntity(id int64) *deleteTestEntity {
	return &deleteTestEntity{EntityBase: entity.NewEntityBase(id, entity.EntityCategory(1), false, remoteDeleteTestKind)}
}

// batchManager answers PrepareRemoteWriteBatch with a scripted result; every
// other manager method panics if reached.
type batchManager struct {
	entity.IRemoteEntityManager
	batch entity.RemoteWriteBatch
	err   error
}

func (m batchManager) PrepareRemoteWriteBatch(context.Context, []int64) (entity.RemoteWriteBatch, error) {
	return m.batch, m.err
}

func newDeleteRuntime(manager *entity.EntityManager, remote entity.IRemoteEntityManager) *Runtime {
	return &Runtime{Projector: &Projector{}, access: entity.NewManagerAccess(manager), remoteManager: remote, onFatal: func(error) {}}
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
