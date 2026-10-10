package engine

import (
	"context"
	"errors"
	coredata "github.com/tjbdwanghaibo/roost-core/framework/dataengine"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	corenest "github.com/tjbdwanghaibo/roost-core/framework/nest"
	"github.com/tjbdwanghaibo/roost-core/framework/remoteentity"
	"github.com/tjbdwanghaibo/roost-core/infra/base/fctx"
	"go.mongodb.org/mongo-driver/v2/bson"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestFastWorkerRemoteDeleteIsDefiniteRejection(t *testing.T) {
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
	_, release := fctx.NewContext(fctx.WithFastWorker(), fctx.WithHandler("default_handler"))
	destroyErr := runtime.access.Destroy(context.Background(), remote, entity.DestroyReasonCommon, true)
	release()
	if !errors.Is(destroyErr, fctx.ErrBlockingInFastWorker) {
		t.Errorf("Destroy err=%v, want errors.Is(fctx.ErrBlockingInFastWorker)", destroyErr)
	}
	if len(fatal) != 0 {
		t.Errorf("fast-worker rejection before any side effect escalated to runtime fatal: %v", fatal)
	}
	if manager.Get(remote.ID()) != remote {
		t.Error("definitively rejected delete removed the entity from memory")
	}
}

// 其余 panic 仍无法证明远端/WAL 未被动过，保持不确定处理：fatal、摘除实体，并保留原错误链。
type panickingBatchManager struct {
	entity.IRemoteEntityManager
	cause error
}

func (m panickingBatchManager) PrepareRemoteWriteBatch(context.Context, []int64) (entity.RemoteWriteBatch, error) {
	panic(m.cause)
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

func (value *bareDeleteEntity) Base() *entity.EntityBase       { return value.EntityBase }
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

type panickingStore struct {
	repositoryStore
	panics int
	reads  int
}

func (store *panickingStore) ReadConsistent(ctx context.Context, read func(context.Context) error) error {
	store.reads++
	if store.reads <= store.panics {
		panic("store blew up")
	}
	return store.repositoryStore.ReadConsistent(ctx, read)
}

func TestARepositoryLoadPanicDoesNotWedgeTheAggregate(t *testing.T) {
	ensureDataEngineRepositoryEntity()
	id, err := entity.BuildEntityID(1471, dataEngineRepositoryKind)
	if err != nil {
		t.Fatal(err)
	}
	store := &panickingStore{
		repositoryStore: repositoryStore{docs: map[string][]coredata.RawDocument{
			"repository_profile":   {repositoryRaw(t, "repository_profile", id, 1)},
			"repository_inventory": {repositoryRaw(t, "repository_inventory", id, 1)},
		}},
		panics: 1,
	}
	repository, err := newEntityRepository(entity.NewEntityManager(), store, repositoryGate(true))
	if err != nil {
		t.Fatal(err)
	}

	func() {
		defer func() {
			if recovered := recover(); recovered == nil {
				t.Fatal("the store panic was swallowed; a failed load must not look like a successful one")
			}
		}()
		_, _ = repository.LoadEntity(context.Background(), id, dataEngineRepositoryKind)
	}()

	fullID, err := entity.NormalizeFullID(id, dataEngineRepositoryKind)
	if err != nil {
		t.Fatal(err)
	}
	repository.flightMu.Lock()
	_, stuck := repository.flights[fullID]
	repository.flightMu.Unlock()
	if stuck {
		t.Fatal("the flight survived the panic; every later load of this aggregate would wait forever")
	}

	// The retry reaches the store again and succeeds.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := repository.LoadEntity(ctx, id, dataEngineRepositoryKind)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the retry after a panic failed: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the retry hung on the flight the panic left behind")
	}
}

// A waiter attached to the panicking load is released with an error that says
// so, rather than by its own deadline.
func TestRepositoryWaitersOfAPanickingLoadAreReleased(t *testing.T) {
	ensureDataEngineRepositoryEntity()
	id, err := entity.BuildEntityID(1472, dataEngineRepositoryKind)
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	store := &blockingPanicStore{entered: entered, release: release}
	repository, err := newEntityRepository(entity.NewEntityManager(), store, repositoryGate(true))
	if err != nil {
		t.Fatal(err)
	}

	leaderDone := make(chan struct{})
	go func() {
		defer func() { _ = recover(); close(leaderDone) }()
		_, _ = repository.LoadEntity(context.Background(), id, dataEngineRepositoryKind)
	}()
	<-entered

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	waiter := make(chan error, 1)
	go func() {
		_, err := repository.LoadEntity(ctx, id, dataEngineRepositoryKind)
		waiter <- err
	}()
	time.Sleep(20 * time.Millisecond)
	close(release)
	<-leaderDone

	select {
	case err := <-waiter:
		if err == nil || !strings.Contains(err.Error(), "panic") {
			t.Fatalf("the waiter got %v, want an error naming the panic", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the waiter is still waiting on a flight nobody will close")
	}
}

type blockingPanicStore struct {
	repositoryStore
	entered chan struct{}
	release chan struct{}
	once    bool
}

func (store *blockingPanicStore) ReadConsistent(context.Context, func(context.Context) error) error {
	if !store.once {
		store.once = true
		close(store.entered)
		<-store.release
		panic("store blew up")
	}
	return nil
}

const kindDefManagedRepositoryKind entity.EntityKind = 247

var kindDefManagedRepositoryOnce sync.Once

// registerKindDefManagedRepositoryKind：先经 kind 定义声明 managed，再注册省略 RemotePolicy 与 Lifetime 的手写 builder。
func registerKindDefManagedRepositoryKind() {
	kindDefManagedRepositoryOnce.Do(func() {
		entity.MustRegisterEntityKindDefs(entity.EntityKindDef{Kind: kindDefManagedRepositoryKind, Category: 1, RemotePolicy: entity.RemotePolicyManaged})
		entity.RegisterEntityBuilder(&entity.EntityBuilderParam{
			Category: 1, Kind: kindDefManagedRepositoryKind, // RemotePolicy 与 Lifetime 都省略
			DaoBuilders: []entity.DaoBuilderFunc{func() entity.DaoInterface {
				return &dataEngineRepositoryDAO{collection: "rr71_remote"}
			}},
			Builder: func(param *entity.EntityCreateParam) (entity.IThreadSafeEntity, error) {
				return &dataEngineRemoteRepositoryEntity{RemoteEntityBase: entity.NewRemoteEntityBaseWithMutex(param.Id, param.Category, false, param.Mutex, param.Kind)}, nil
			},
		})
	})
}

// REPRO-2026-09-26-06 §7 探针改写：修前向量 {StateVersion:0 …}、ownership unknown，“无版本信封视为损坏”检查被跳过。
func TestRepositoryRestoresRemoteEnvelopeByKindPolicy(t *testing.T) {
	registerKindDefManagedRepositoryKind()
	if !entity.IsEntityKindRemoteManaged(kindDefManagedRepositoryKind) {
		t.Fatal("setup: kind is not managed")
	}
	if entity.GetEntityBuilderParam(kindDefManagedRepositoryKind).RemotePolicy.RemoteManaged() {
		t.Fatal("setup: the builder itself must omit RemotePolicy")
	}
	id, _ := entity.BuildEntityID(9971, kindDefManagedRepositoryKind)
	inner, _ := bson.Marshal(bson.M{"_id": id, "_schema": uint32(1), "name": "remote"})
	outer, _ := bson.Marshal(bson.M{"_id": id, "_ver": uint64(12), "_marker_epoch": uint64(3), "_lock_fence": uint64(8), "_route_epoch": uint64(5), "data": inner})
	enveloped := coredata.RawDocument{
		Key:     coredata.DocumentKey{Database: "game", Resource: "rr71_remote", ID: id},
		Version: 12, MarkerEpoch: 3, LockFence: 8, RouteEpoch: 5, Enveloped: true, Data: outer,
	}
	store := &repositoryStore{docs: map[string][]coredata.RawDocument{"rr71_remote": {enveloped}}}
	repository, err := newEntityRepository(entity.NewEntityManager(), store, repositoryGate(true))
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := repository.LoadEntity(context.Background(), id, kindDefManagedRepositoryKind)
	if err != nil {
		t.Fatal(err)
	}
	remote := loaded.(*dataEngineRemoteRepositoryEntity)
	want := entity.RemoteVersionVector{StateVersion: 12, MarkerEpoch: 3, LockFence: 8, RouteEpoch: 5}
	if vector := remote.RemoteVersionVector(); vector != want {
		t.Fatalf("kind is remote-managed in the registry, but the cold load dropped the stored Remote envelope: vector=%+v ownership=%v (want %+v, recovering)", vector, remote.RemoteOwnershipState(), want)
	}
	if got := remote.RemoteOwnershipState(); got != entity.RemoteOwnershipRecovering {
		t.Fatalf("ownership=%v, want recovering after a cold load", got)
	}
}

// 没有版本信封的记录对托管 kind 视为损坏，同样按 kind 策略判断（修前检查被跳过，按普通实体加载成功）。
func TestRepositoryRejectsManagedKindRecordWithoutEnvelope(t *testing.T) {
	registerKindDefManagedRepositoryKind()
	otherID, _ := entity.BuildEntityID(9972, kindDefManagedRepositoryKind)
	bare := &repositoryStore{docs: map[string][]coredata.RawDocument{"rr71_remote": {repositoryRaw(t, "rr71_remote", otherID, 4)}}}
	repository, err := newEntityRepository(entity.NewEntityManager(), bare, repositoryGate(true))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repository.LoadEntity(context.Background(), otherID, kindDefManagedRepositoryKind); !errors.Is(err, ErrEntityAggregateCorrupt) {
		t.Fatalf("a remote-managed kind loaded a record without a version envelope: err=%v, want ErrEntityAggregateCorrupt", err)
	}
}

const (
	dataEngineNoBuilderKind entity.EntityKind = 243
	dataEngineNoPersistKind entity.EntityKind = 244
	dataEngineNoLoaderKind  entity.EntityKind = 245
	dataEngineWrongIDKind   entity.EntityKind = 246
)

// noLoaderDAO shadows RestorePersisted with a different signature so it no
// longer satisfies entity.PersistedDaoLoader.
type noLoaderDAO struct{ dataEngineRepositoryDAO }

func (*noLoaderDAO) RestorePersisted() {}

// wrongIDDAO decodes to a different entity than the one requested.
type wrongIDDAO struct{ dataEngineRepositoryDAO }

func (dao *wrongIDDAO) RestorePersisted(raw []byte, schema uint32, version uint64) error {
	if err := dao.dataEngineRepositoryDAO.RestorePersisted(raw, schema, version); err != nil {
		return err
	}
	dao.id++
	return nil
}

var registerLoadGuardKinds sync.Once

func ensureLoadGuardKinds() {
	registerLoadGuardKinds.Do(func() {
		build := func(param *entity.EntityCreateParam) (entity.IThreadSafeEntity, error) {
			return &dataEngineRepositoryEntity{EntityBase: entity.NewEntityBaseWithMutex(param.Id, param.Category, false, param.Mutex, param.Kind)}, nil
		}
		entity.MustRegisterEntityKindCategory(dataEngineNoBuilderKind, 1)
		entity.RegisterEntityBuilder(&entity.EntityBuilderParam{Category: 1, Kind: dataEngineNoPersistKind, NoPersist: true, Builder: build})
		entity.RegisterEntityBuilder(&entity.EntityBuilderParam{
			Category: 1, Kind: dataEngineNoLoaderKind,
			DaoBuilders: []entity.DaoBuilderFunc{func() entity.DaoInterface {
				return &noLoaderDAO{dataEngineRepositoryDAO{collection: "repository_noloader"}}
			}},
			Builder: build,
		})
		entity.RegisterEntityBuilder(&entity.EntityBuilderParam{
			Category: 1, Kind: dataEngineWrongIDKind,
			DaoBuilders: []entity.DaoBuilderFunc{func() entity.DaoInterface {
				return &wrongIDDAO{dataEngineRepositoryDAO{collection: "repository_wrongid"}}
			}},
			Builder: build,
		})
	})
}

// U-0101 (C2): the load path's remaining refusals — a kind the registry does
// not know, a kind with nothing persistent, a DAO that cannot be hydrated, a
// DAO that decodes to another id, and a stored schema the repository has no
// migrator for — each fail by name and publish nothing.
func TestEntityRepositoryRefusesEachUnloadableAggregate(t *testing.T) {
	ensureDataEngineRepositoryEntity()
	ensureLoadGuardKinds()
	ctx := context.Background()

	if _, err := newEntityRepository(nil, &repositoryStore{}, repositoryGate(true)); err == nil {
		t.Fatal("repository without a manager accepted")
	}
	if _, err := newEntityRepository(entity.NewEntityManager(), nil, repositoryGate(true)); err == nil {
		t.Fatal("repository without a store accepted")
	}
	var nilRepository *EntityRepository
	if _, err := nilRepository.LoadEntity(ctx, 1, dataEngineRepositoryKind); !errors.Is(err, coredata.ErrStoreRequired) {
		t.Fatalf("nil repository LoadEntity = %v", err)
	}

	schema2 := func(resource string, id int64) coredata.RawDocument {
		doc := repositoryRaw(t, resource, id, 1)
		doc.Schema = 2
		return doc
	}
	cases := []struct {
		name string
		kind entity.EntityKind
		docs func(id int64) map[string][]coredata.RawDocument
		want error
		text string
	}{
		{"no builder for kind", dataEngineNoBuilderKind, func(int64) map[string][]coredata.RawDocument { return nil }, nil, "no builder for entity kind 243"},
		{"kind without persistent DAO", dataEngineNoPersistKind, func(int64) map[string][]coredata.RawDocument { return nil }, ErrEntityAggregateNotFound, "has no persistent DAO"},
		{"DAO cannot be hydrated", dataEngineNoLoaderKind, func(id int64) map[string][]coredata.RawDocument {
			return map[string][]coredata.RawDocument{"repository_noloader": {repositoryRaw(t, "repository_noloader", id, 1)}}
		}, ErrEntityAggregateCorrupt, "resource=repository_noloader does not implement PersistedDaoLoader"},
		{"DAO decodes to another id", dataEngineWrongIDKind, func(id int64) map[string][]coredata.RawDocument {
			return map[string][]coredata.RawDocument{"repository_wrongid": {repositoryRaw(t, "repository_wrongid", id, 1)}}
		}, ErrEntityAggregateCorrupt, "decoded id="},
		{"stored schema without a migrator", dataEngineRepositoryKind, func(id int64) map[string][]coredata.RawDocument {
			return map[string][]coredata.RawDocument{
				"repository_profile":   {schema2("repository_profile", id)},
				"repository_inventory": {repositoryRaw(t, "repository_inventory", id, 1)},
			}
		}, coredata.ErrSchemaMismatch, "resource=repository_profile"},
	}
	for index, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id, err := entity.BuildEntityID(int64(1301+index), tc.kind)
			if err != nil {
				t.Fatal(err)
			}
			manager := entity.NewEntityManager()
			repository, err := newEntityRepository(manager, &repositoryStore{docs: tc.docs(id)}, repositoryGate(true))
			if err != nil {
				t.Fatal(err)
			}
			_, err = repository.LoadEntity(ctx, id, tc.kind)
			if err == nil || (tc.want != nil && !errors.Is(err, tc.want)) || !strings.Contains(err.Error(), tc.text) {
				t.Fatalf("LoadEntity = %v; want %v containing %q", err, tc.want, tc.text)
			}
			if manager.Get(id) != nil {
				t.Fatal("an unloadable aggregate was published")
			}
		})
	}
}

const (
	dataEngineDuplicateDAOKind entity.EntityKind = 241
	dataEngineNilBuilderKind   entity.EntityKind = 242
)

var registerCorruptBuilderKinds sync.Once

func ensureCorruptBuilderKinds() {
	registerCorruptBuilderKinds.Do(func() {
		build := func(param *entity.EntityCreateParam) (entity.IThreadSafeEntity, error) {
			return &dataEngineRepositoryEntity{EntityBase: entity.NewEntityBaseWithMutex(param.Id, param.Category, false, param.Mutex, param.Kind)}, nil
		}
		entity.RegisterEntityBuilder(&entity.EntityBuilderParam{
			Category: 1, Kind: dataEngineDuplicateDAOKind,
			DaoBuilders: []entity.DaoBuilderFunc{
				func() entity.DaoInterface { return &dataEngineRepositoryDAO{collection: "repository_dup"} },
				func() entity.DaoInterface { return &dataEngineRepositoryDAO{collection: "repository_dup"} },
			},
			Builder: build,
		})
		entity.RegisterEntityBuilder(&entity.EntityBuilderParam{
			Category: 1, Kind: dataEngineNilBuilderKind,
			DaoBuilders: []entity.DaoBuilderFunc{nil},
			Builder:     build,
		})
	})
}

func expectCorrupt(t *testing.T, err error, text string) {
	t.Helper()
	if !errors.Is(err, ErrEntityAggregateCorrupt) || !strings.Contains(err.Error(), text) {
		t.Fatalf("LoadEntity = %v, want ErrEntityAggregateCorrupt containing %q", err, text)
	}
}

// Loading an aggregate is where a bad store answer would become a live
// entity. Each shape of bad answer is refused with the resource named, and
// nothing is published to the manager: two documents for one id, a document
// keyed to another entity, a builder that declares the same resource twice,
// a nil DAO builder, and a remote-managed entity persisted without its
// version envelope.
func TestEntityRepositoryRefusesEachCorruptAggregateShape(t *testing.T) {
	ensureDataEngineRepositoryEntity()
	ensureDataEngineRemoteRepositoryEntity()
	ensureCorruptBuilderKinds()
	ctx := context.Background()

	t.Run("two documents for one id", func(t *testing.T) {
		id, _ := entity.BuildEntityID(1201, dataEngineRepositoryKind)
		store := &repositoryStore{docs: map[string][]coredata.RawDocument{
			"repository_profile":   {repositoryRaw(t, "repository_profile", id, 1), repositoryRaw(t, "repository_profile", id, 2)},
			"repository_inventory": {repositoryRaw(t, "repository_inventory", id, 1)},
		}}
		manager := entity.NewEntityManager()
		repository, _ := newEntityRepository(manager, store, repositoryGate(true))
		_, err := repository.LoadEntity(ctx, id, dataEngineRepositoryKind)
		expectCorrupt(t, err, "resource=repository_profile")
		expectCorrupt(t, err, "documents=2")
		if manager.Get(id) != nil {
			t.Fatal("corrupt aggregate was published")
		}
	})
	t.Run("document keyed to another entity", func(t *testing.T) {
		id, _ := entity.BuildEntityID(1202, dataEngineRepositoryKind)
		other, _ := entity.BuildEntityID(1203, dataEngineRepositoryKind)
		store := &repositoryStore{docs: map[string][]coredata.RawDocument{
			"repository_profile":   {repositoryRaw(t, "repository_profile", other, 1)},
			"repository_inventory": {repositoryRaw(t, "repository_inventory", id, 1)},
		}}
		repository, _ := newEntityRepository(entity.NewEntityManager(), store, repositoryGate(true))
		_, err := repository.LoadEntity(ctx, id, dataEngineRepositoryKind)
		expectCorrupt(t, err, "documents=1")
	})
	t.Run("builder declares one resource twice", func(t *testing.T) {
		id, _ := entity.BuildEntityID(1204, dataEngineDuplicateDAOKind)
		store := &repositoryStore{docs: map[string][]coredata.RawDocument{"repository_dup": {repositoryRaw(t, "repository_dup", id, 1)}}}
		repository, _ := newEntityRepository(entity.NewEntityManager(), store, repositoryGate(true))
		_, err := repository.LoadEntity(ctx, id, dataEngineDuplicateDAOKind)
		expectCorrupt(t, err, `duplicate DAO resource "repository_dup"`)
	})
	t.Run("nil DAO builder", func(t *testing.T) {
		id, _ := entity.BuildEntityID(1205, dataEngineNilBuilderKind)
		repository, _ := newEntityRepository(entity.NewEntityManager(), &repositoryStore{docs: map[string][]coredata.RawDocument{}}, repositoryGate(true))
		_, err := repository.LoadEntity(ctx, id, dataEngineNilBuilderKind)
		expectCorrupt(t, err, "DAO builder 0 is nil")
	})
	t.Run("remote-managed entity without a version envelope", func(t *testing.T) {
		id, _ := entity.BuildEntityID(1206, dataEngineRemoteRepositoryKind)
		store := &repositoryStore{docs: map[string][]coredata.RawDocument{"repository_remote": {repositoryRaw(t, "repository_remote", id, 3)}}}
		repository, _ := newEntityRepository(entity.NewEntityManager(), store, repositoryGate(true))
		_, err := repository.LoadEntity(ctx, id, dataEngineRemoteRepositoryKind)
		expectCorrupt(t, err, "has no version envelope")
	})
}
