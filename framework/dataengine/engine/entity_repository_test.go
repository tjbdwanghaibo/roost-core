// 实体仓储：完整聚合单飞加载、恢复屏障、墓碑与缺失、Remote 向量与信封、经本地执行器发布、loader panic 不卡住聚合、新建实体首笔提交写齐聚合。

package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	coredata "github.com/tjbdwanghaibo/roost-core/framework/dataengine"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	corenest "github.com/tjbdwanghaibo/roost-core/framework/nest"
	"github.com/tjbdwanghaibo/roost-core/infra/base/fctx"
	"github.com/tjbdwanghaibo/roost-core/infra/storage/mongo/mongotest"
	"go.mongodb.org/mongo-driver/v2/bson"
)

const dataEngineRepositoryKind entity.EntityKind = 239
const dataEngineRemoteRepositoryKind entity.EntityKind = 240

type dataEngineRepositoryDAO struct {
	id         int64
	collection string
	version    uint64
}

func (dao *dataEngineRepositoryDAO) Id() int64         { return dao.id }
func (dao *dataEngineRepositoryDAO) SetId(id int64)    { dao.id = id }
func (*dataEngineRepositoryDAO) DbName() string        { return "game" }
func (dao *dataEngineRepositoryDAO) CollName() string  { return dao.collection }
func (*dataEngineRepositoryDAO) Dirty() entity.IDirty  { return nil }
func (*dataEngineRepositoryDAO) CleanDirty()           {}
func (*dataEngineRepositoryDAO) SchemaVersion() uint32 { return 1 }

func (dao *dataEngineRepositoryDAO) RestorePersisted(raw []byte, _ uint32, version uint64) error {
	var doc struct {
		ID int64 `bson:"_id"`
	}
	if err := bson.Unmarshal(raw, &doc); err != nil {
		return err
	}
	if dao.id != doc.ID {
		return fmt.Errorf("repository test DAO was not pre-bound: got=%d payload=%d", dao.id, doc.ID)
	}
	dao.version = version
	return nil
}

type dataEngineRepositoryEntity struct {
	*entity.EntityBase
	daos map[string]*dataEngineRepositoryDAO
}

func (value *dataEngineRepositoryEntity) Base() *entity.EntityBase               { return value.EntityBase }
func (*dataEngineRepositoryEntity) OnInitFinish(*entity.EntityCreateParam) error { return nil }
func (*dataEngineRepositoryEntity) OnDestroy(entity.EntityDestroyReason)         {}
func (*dataEngineRepositoryEntity) AutoPersist() bool                            { return true }
func (*dataEngineRepositoryEntity) IsRemoved() bool                              { return false }
func (*dataEngineRepositoryEntity) SetRemoved()                                  {}
func (*dataEngineRepositoryEntity) Touch() bool                                  { return true }
func (*dataEngineRepositoryEntity) UnTouch()                                     {}
func (*dataEngineRepositoryEntity) ClearBase()                                   {}
func (*dataEngineRepositoryEntity) IsClear() bool                                { return false }

var registerDataEngineRepositoryEntity sync.Once
var registerDataEngineRemoteRepositoryEntity sync.Once

func ensureDataEngineRepositoryEntity() {
	registerDataEngineRepositoryEntity.Do(func() {
		entity.RegisterEntityBuilder(&entity.EntityBuilderParam{
			Category: 1, Kind: dataEngineRepositoryKind,
			DaoBuilders: []entity.DaoBuilderFunc{
				func() entity.DaoInterface { return &dataEngineRepositoryDAO{collection: "repository_profile"} },
				func() entity.DaoInterface { return &dataEngineRepositoryDAO{collection: "repository_inventory"} },
			},
			Builder: func(param *entity.EntityCreateParam) (entity.IThreadSafeEntity, error) {
				daos := make(map[string]*dataEngineRepositoryDAO, len(param.Dao))
				for name, dao := range param.Dao {
					daos[name] = dao.(*dataEngineRepositoryDAO)
				}
				return &dataEngineRepositoryEntity{
					EntityBase: entity.NewEntityBaseWithMutex(param.Id, param.Category, false, param.Mutex, param.Kind), daos: daos,
				}, nil
			},
		})
	})
}

type dataEngineRemoteRepositoryEntity struct {
	*entity.RemoteEntityBase
}

func (value *dataEngineRemoteRepositoryEntity) Base() *entity.EntityBase {
	return &value.RemoteEntityBase.EntityBase
}
func (*dataEngineRemoteRepositoryEntity) OnInitFinish(*entity.EntityCreateParam) error { return nil }
func (*dataEngineRemoteRepositoryEntity) OnDestroy(entity.EntityDestroyReason)         {}
func (*dataEngineRemoteRepositoryEntity) AutoPersist() bool                            { return true }
func (*dataEngineRemoteRepositoryEntity) IsRemoved() bool                              { return false }
func (*dataEngineRemoteRepositoryEntity) SetRemoved()                                  {}
func (*dataEngineRemoteRepositoryEntity) Touch() bool                                  { return true }
func (*dataEngineRemoteRepositoryEntity) UnTouch()                                     {}
func (*dataEngineRemoteRepositoryEntity) ClearBase()                                   {}
func (*dataEngineRemoteRepositoryEntity) IsClear() bool                                { return false }

func ensureDataEngineRemoteRepositoryEntity() {
	registerDataEngineRemoteRepositoryEntity.Do(func() {
		entity.RegisterEntityBuilder(&entity.EntityBuilderParam{
			Category: 1, Kind: dataEngineRemoteRepositoryKind, RemotePolicy: entity.RemotePolicyManaged,
			Lifetime: entity.EntityLifetimeRemoteManaged,
			DaoBuilders: []entity.DaoBuilderFunc{func() entity.DaoInterface {
				return &dataEngineRepositoryDAO{collection: "repository_remote"}
			}},
			Builder: func(param *entity.EntityCreateParam) (entity.IThreadSafeEntity, error) {
				return &dataEngineRemoteRepositoryEntity{RemoteEntityBase: entity.NewRemoteEntityBaseWithMutex(param.Id, param.Category, false, param.Mutex, param.Kind)}, nil
			},
		})
	})
}

type repositoryStore struct {
	mu           sync.Mutex
	docs         map[string][]coredata.RawDocument
	transactions atomic.Int32
	attempts     atomic.Int32
	delay        time.Duration
	// transientRetries reproduces ISession.WithTransaction's documented
	// automatic retry: the driver re-invokes the callback on a transient
	// transaction error, so every callback body must be idempotent.
	transientRetries int
}

func (store *repositoryStore) ReadConsistent(ctx context.Context, read func(context.Context) error) error {
	store.transactions.Add(1)
	if store.delay > 0 {
		time.Sleep(store.delay)
	}
	var err error
	for attempt := 0; attempt <= store.transientRetries; attempt++ {
		store.attempts.Add(1)
		err = read(ctx)
	}
	return err
}

// Load 与 MongoStore 同一约定：默认不交出墓碑，spec.IncludeDeleted 时交出。
func (store *repositoryStore) Load(_ context.Context, spec coredata.LoadSpec) ([]coredata.RawDocument, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	var docs []coredata.RawDocument
	for _, doc := range store.docs[spec.Resource] {
		if doc.Deleted && !spec.IncludeDeleted {
			continue
		}
		docs = append(docs, doc)
	}
	return docs, nil
}
func (store *repositoryStore) StreamLoad(ctx context.Context, spec coredata.LoadSpec, consume func(coredata.RawDocument) error) error {
	docs, _ := store.Load(ctx, spec)
	for _, doc := range docs {
		if err := consume(doc); err != nil {
			return err
		}
	}
	return nil
}

type repositoryGate bool

func (gate repositoryGate) Ready() bool { return bool(gate) }

func repositoryRaw(t *testing.T, resource string, id int64, version uint64) coredata.RawDocument {
	t.Helper()
	raw, err := bson.Marshal(bson.M{"_id": id, "_schema": uint32(1), "name": resource})
	if err != nil {
		t.Fatal(err)
	}
	return coredata.RawDocument{Key: coredata.DocumentKey{Database: "game", Resource: resource, ID: id}, Version: version, Schema: 1, Data: raw}
}

func TestEntityRepositorySingleFlightsCompleteAggregate(t *testing.T) {
	ensureDataEngineRepositoryEntity()
	id, _ := entity.BuildEntityID(991, dataEngineRepositoryKind)
	store := &repositoryStore{delay: 20 * time.Millisecond, docs: map[string][]coredata.RawDocument{
		"repository_profile":   {repositoryRaw(t, "repository_profile", id, 7)},
		"repository_inventory": {repositoryRaw(t, "repository_inventory", id, 8)},
	}}
	manager := entity.NewEntityManager()
	repository, err := NewEntityRepository(manager, store, repositoryGate(true))
	if err != nil {
		t.Fatal(err)
	}
	const callers = 24
	values := make(chan entity.IThreadSafeEntity, callers)
	var wait sync.WaitGroup
	for range callers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			value, loadErr := repository.LoadEntity(context.Background(), id, dataEngineRepositoryKind)
			if loadErr != nil {
				t.Error(loadErr)
			}
			values <- value
		}()
	}
	wait.Wait()
	close(values)
	var first entity.IThreadSafeEntity
	for value := range values {
		if first == nil {
			first = value
		}
		if value != first {
			t.Fatal("singleflight returned different aggregates")
		}
	}
	if store.transactions.Load() != 1 || len(first.(*dataEngineRepositoryEntity).daos) != 2 {
		t.Fatalf("transactions=%d entity=%+v", store.transactions.Load(), first)
	}
}

func TestEntityRepositoryRecoveryBarrierAndIncompleteAggregate(t *testing.T) {
	ensureDataEngineRepositoryEntity()
	id, _ := entity.BuildEntityID(992, dataEngineRepositoryKind)
	store := &repositoryStore{docs: map[string][]coredata.RawDocument{"repository_profile": {repositoryRaw(t, "repository_profile", id, 1)}}}
	manager := entity.NewEntityManager()
	notReady, _ := NewEntityRepository(manager, store, repositoryGate(false))
	if _, err := notReady.LoadEntity(context.Background(), id, dataEngineRepositoryKind); !errors.Is(err, coredata.ErrRecoveryIncomplete) {
		t.Fatalf("barrier err=%v", err)
	}
	repository, _ := NewEntityRepository(manager, store, repositoryGate(true))
	if _, err := repository.LoadEntity(context.Background(), id, dataEngineRepositoryKind); !errors.Is(err, ErrEntityAggregateCorrupt) {
		t.Fatalf("missing DAO err=%v", err)
	}
	if manager.Get(id) != nil {
		t.Fatal("incomplete aggregate was published")
	}
}

func TestEntityRepositoryRejectsTombstone(t *testing.T) {
	ensureDataEngineRepositoryEntity()
	id, _ := entity.BuildEntityID(993, dataEngineRepositoryKind)
	deleted := repositoryRaw(t, "repository_profile", id, 2)
	deleted.Deleted = true
	store := &repositoryStore{docs: map[string][]coredata.RawDocument{
		"repository_profile": {deleted}, "repository_inventory": {repositoryRaw(t, "repository_inventory", id, 2)},
	}}
	repository, _ := NewEntityRepository(entity.NewEntityManager(), store, repositoryGate(true))
	if _, err := repository.LoadEntity(context.Background(), id, dataEngineRepositoryKind); !errors.Is(err, ErrEntityAggregateCorrupt) {
		t.Fatalf("tombstone err=%v", err)
	}
}

// 全部缺失与全部墓碑是两个哨兵（D10 方案 B）：从未创建可以创建（not found），删过档这个 ID 不可再用（deleted）。
// 删过档刻意不满足 not found：“不存在就创建”的代码（生成 GetOrCreate）不能把它当成可创建。
func TestEntityRepositorySeparatesAbsentFromDeleted(t *testing.T) {
	ensureDataEngineRepositoryEntity()
	id, _ := entity.BuildEntityID(995, dataEngineRepositoryKind)
	profile := repositoryRaw(t, "repository_profile", id, 2)
	inventory := repositoryRaw(t, "repository_inventory", id, 2)
	profile.Deleted, inventory.Deleted = true, true
	for _, test := range []struct {
		name string
		docs map[string][]coredata.RawDocument
		want func(error) bool
	}{
		{name: "all missing", docs: map[string][]coredata.RawDocument{}, want: func(err error) bool {
			return errors.Is(err, ErrEntityAggregateNotFound) && errors.Is(err, entity.ErrAuthorityEntityNotFound) && !errors.Is(err, entity.ErrAuthorityEntityDeleted)
		}},
		{name: "all tombstones", docs: map[string][]coredata.RawDocument{"repository_profile": {profile}, "repository_inventory": {inventory}}, want: func(err error) bool {
			return errors.Is(err, ErrEntityAggregateDeleted) && errors.Is(err, entity.ErrAuthorityEntityDeleted) &&
				!errors.Is(err, ErrEntityAggregateNotFound) && !errors.Is(err, entity.ErrAuthorityEntityNotFound)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			repository, _ := NewEntityRepository(entity.NewEntityManager(), &repositoryStore{docs: test.docs}, repositoryGate(true))
			if _, err := repository.LoadEntity(context.Background(), id, dataEngineRepositoryKind); !test.want(err) {
				t.Fatalf("LoadEntity err=%v", err)
			}
		})
	}
}

func TestEntityRepositoryPreservesRemoteVersionVector(t *testing.T) {
	ensureDataEngineRemoteRepositoryEntity()
	id, _ := entity.BuildEntityID(994, dataEngineRemoteRepositoryKind)
	inner, _ := bson.Marshal(bson.M{"_id": id, "_schema": uint32(1), "name": "remote"})
	outer, _ := bson.Marshal(bson.M{
		"_id": id, "_ver": uint64(12), "_marker_epoch": uint64(3),
		"_lock_fence": uint64(8), "_route_epoch": uint64(5), "data": inner,
	})
	store := &repositoryStore{docs: map[string][]coredata.RawDocument{
		"repository_remote": {{
			Key:     coredata.DocumentKey{Database: "game", Resource: "repository_remote", ID: id},
			Version: 12, MarkerEpoch: 3, LockFence: 8, RouteEpoch: 5, Enveloped: true, Data: outer,
		}},
	}}
	repository, _ := NewEntityRepository(entity.NewEntityManager(), store, repositoryGate(true))
	loaded, err := repository.LoadEntity(context.Background(), id, dataEngineRemoteRepositoryKind)
	if err != nil {
		t.Fatal(err)
	}
	vector := loaded.(*dataEngineRemoteRepositoryEntity).RemoteVersionVector()
	if vector != (entity.RemoteVersionVector{StateVersion: 12, MarkerEpoch: 3, LockFence: 8, RouteEpoch: 5}) {
		t.Fatalf("vector=%+v", vector)
	}
}

// A transient transaction retry re-invokes the read callback. Loading must be
// idempotent across attempts: before the fix, `loaded` accumulated outside the
// callback, so the second attempt hit the duplicate-resource guard and a
// perfectly healthy aggregate was reported as corrupt — exactly during the
// failovers and snapshot hiccups that cause retries in the first place.
func TestEntityRepositoryLoadIsIdempotentAcrossTransactionRetries(t *testing.T) {
	ensureDataEngineRepositoryEntity()
	id, _ := entity.BuildEntityID(4711, dataEngineRepositoryKind)
	docs := map[string][]coredata.RawDocument{
		"repository_profile":   {repositoryRaw(t, "repository_profile", id, 7)},
		"repository_inventory": {repositoryRaw(t, "repository_inventory", id, 8)},
	}
	for _, retries := range []int{0, 1, 3} {
		store := &repositoryStore{docs: docs, transientRetries: retries}
		repository, err := NewEntityRepository(entity.NewEntityManager(), store, repositoryGate(true))
		if err != nil {
			t.Fatal(err)
		}
		value, err := repository.LoadEntity(context.Background(), id, dataEngineRepositoryKind)
		if err != nil {
			t.Fatalf("retries=%d: %v", retries, err)
		}
		loaded := value.(*dataEngineRepositoryEntity)
		if len(loaded.daos) != 2 {
			t.Fatalf("retries=%d: aggregate has %d DAOs, want 2", retries, len(loaded.daos))
		}
		if got := int(store.attempts.Load()); got != retries+1 {
			t.Fatalf("retries=%d: callback invocations=%d", retries, got)
		}
	}
}

// The remote version vector is also accumulated inside the callback, so a
// retry must not leave a stale envelope behind for a non-remote aggregate.
func TestEntityRepositoryRemoteVectorDoesNotLeakAcrossRetries(t *testing.T) {
	ensureDataEngineRepositoryEntity()
	id, _ := entity.BuildEntityID(4712, dataEngineRepositoryKind)
	profile := repositoryRaw(t, "repository_profile", id, 7)
	profile.Enveloped = true
	profile.Version = 42
	profile.MarkerEpoch, profile.LockFence, profile.RouteEpoch = 1, 2, 3
	store := &repositoryStore{transientRetries: 2, docs: map[string][]coredata.RawDocument{
		"repository_profile":   {profile},
		"repository_inventory": {repositoryRaw(t, "repository_inventory", id, 8)},
	}}
	repository, err := NewEntityRepository(entity.NewEntityManager(), store, repositoryGate(true))
	if err != nil {
		t.Fatal(err)
	}
	// The enveloped payload is not a valid persisted document for this kind,
	// so the load must fail cleanly on every attempt rather than accumulating.
	if _, err := repository.LoadEntity(context.Background(), id, dataEngineRepositoryKind); err != nil {
		if !errors.Is(err, ErrEntityAggregateCorrupt) {
			t.Fatalf("unexpected error shape: %v", err)
		}
		if strings.Contains(err.Error(), "duplicate DAO resource") {
			t.Fatalf("retry accumulated state across attempts: %v", err)
		}
	}
}

func TestEntityRepositoryPublishesThroughLocalExecutor(t *testing.T) {
	ensureDataEngineRepositoryEntity()
	id, _ := entity.BuildEntityID(9991, dataEngineRepositoryKind)
	store := &repositoryStore{docs: map[string][]coredata.RawDocument{
		"repository_profile":   {repositoryRaw(t, "repository_profile", id, 7)},
		"repository_inventory": {repositoryRaw(t, "repository_inventory", id, 8)},
	}}
	manager := entity.NewEntityManager()
	repository, err := NewEntityRepository(manager, store, repositoryGate(true))
	if err != nil {
		t.Fatal(err)
	}
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	ctx := entity.WithLocalExecutor(context.Background(), func(fn func()) error {
		close(entered)
		<-release
		fn()
		return nil
	})
	go func() { _, err := repository.LoadEntity(ctx, id, dataEngineRepositoryKind); done <- err }()
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("no local execution handoff")
	}
	if store.transactions.Load() != 1 || manager.Get(id) != nil {
		t.Fatal("I/O and entity publication were not separated")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if manager.Get(id) == nil {
		t.Fatal("entity was not published by local continuation")
	}
}

func TestRepositoryRejectsShortWorkerColdLoadBeforeJoiningFlight(t *testing.T) {
	ensureDataEngineRepositoryEntity()
	id, err := entity.BuildEntityID(9876, dataEngineRepositoryKind)
	if err != nil {
		t.Fatal(err)
	}
	store := &repositoryStore{}
	repo, err := NewEntityRepository(entity.NewEntityManager(), store, nil)
	if err != nil {
		t.Fatal(err)
	}
	repo.flights[id] = &entityLoadFlight{done: make(chan struct{})}
	_, release := fctx.NewContext(fctx.WithShortWorker(), fctx.WithHandler("repository_cold"))
	defer release()
	defer func() {
		err, ok := recover().(error)
		if !ok || !errors.Is(err, fctx.ErrBlockingInBusinessWorker) || !errors.Is(err, entity.ErrColdLoadInLogic) {
			t.Fatalf("panic=%v", err)
		}
		if store.transactions.Load() != 0 || len(repo.flights) != 1 {
			t.Fatal("load started before rejection")
		}
	}()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	_, _ = repo.LoadEntity(ctx, id, dataEngineRepositoryKind)
}

func TestEntityRepositoryRejectsSchemaMismatchBeforePublishing(t *testing.T) {
	ensureDataEngineRepositoryEntity()
	for _, schema := range []uint32{0, 2} {
		for _, collection := range []string{"repository_profile", "repository_inventory"} {
			t.Run(fmt.Sprintf("%s/schema_%d", collection, schema), func(t *testing.T) {
				id, _ := entity.BuildEntityID(1401, dataEngineRepositoryKind)
				store := &repositoryStore{docs: map[string][]coredata.RawDocument{
					"repository_profile":   {repositoryRaw(t, "repository_profile", id, 4)},
					"repository_inventory": {repositoryRaw(t, "repository_inventory", id, 4)},
				}}
				store.docs[collection][0].Schema = schema
				manager := entity.NewEntityManager()
				repo, err := NewEntityRepository(manager, store, repositoryGate(true))
				if err != nil {
					t.Fatal(err)
				}
				loaded, err := repo.LoadEntity(context.Background(), id, dataEngineRepositoryKind)
				if !errors.Is(err, coredata.ErrSchemaMismatch) || loaded != nil || manager.Get(id) != nil {
					t.Fatalf("loaded=%v err=%v", loaded, err)
				}
				if store.transactions.Load() != 1 || store.docs[collection][0].Version != 4 || store.docs[collection][0].Schema != schema {
					t.Fatal("schema refusal retried or changed persisted document")
				}
			})
		}
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
	repository, err := NewEntityRepository(entity.NewEntityManager(), store, repositoryGate(true))
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
	repository, err := NewEntityRepository(entity.NewEntityManager(), store, repositoryGate(true))
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

// 按 kind 策略恢复 Remote 版本信封（向量与所有权状态来自信封）。
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
	repository, err := NewEntityRepository(entity.NewEntityManager(), store, repositoryGate(true))
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

// 没有版本信封的记录对托管 kind 视为损坏，同样按 kind 策略判断。
func TestRepositoryRejectsManagedKindRecordWithoutEnvelope(t *testing.T) {
	registerKindDefManagedRepositoryKind()
	otherID, _ := entity.BuildEntityID(9972, kindDefManagedRepositoryKind)
	bare := &repositoryStore{docs: map[string][]coredata.RawDocument{"rr71_remote": {repositoryRaw(t, "rr71_remote", otherID, 4)}}}
	repository, err := NewEntityRepository(entity.NewEntityManager(), bare, repositoryGate(true))
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

// The load path's remaining refusals — a kind the registry does not know, a kind with nothing persistent, a DAO that cannot be
// hydrated, a DAO that decodes to another id, and a stored schema the repository has no migrator for — each fail by name and publish nothing.
func TestEntityRepositoryRefusesEachUnloadableAggregate(t *testing.T) {
	ensureDataEngineRepositoryEntity()
	ensureLoadGuardKinds()
	ctx := context.Background()

	if _, err := NewEntityRepository(nil, &repositoryStore{}, repositoryGate(true)); err == nil {
		t.Fatal("repository without a manager accepted")
	}
	if _, err := NewEntityRepository(entity.NewEntityManager(), nil, repositoryGate(true)); err == nil {
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
			repository, err := NewEntityRepository(manager, &repositoryStore{docs: tc.docs(id)}, repositoryGate(true))
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
		repository, _ := NewEntityRepository(manager, store, repositoryGate(true))
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
		repository, _ := NewEntityRepository(entity.NewEntityManager(), store, repositoryGate(true))
		_, err := repository.LoadEntity(ctx, id, dataEngineRepositoryKind)
		expectCorrupt(t, err, "documents=1")
	})
	t.Run("builder declares one resource twice", func(t *testing.T) {
		id, _ := entity.BuildEntityID(1204, dataEngineDuplicateDAOKind)
		store := &repositoryStore{docs: map[string][]coredata.RawDocument{"repository_dup": {repositoryRaw(t, "repository_dup", id, 1)}}}
		repository, _ := NewEntityRepository(entity.NewEntityManager(), store, repositoryGate(true))
		_, err := repository.LoadEntity(ctx, id, dataEngineDuplicateDAOKind)
		expectCorrupt(t, err, `duplicate DAO resource "repository_dup"`)
	})
	t.Run("nil DAO builder", func(t *testing.T) {
		id, _ := entity.BuildEntityID(1205, dataEngineNilBuilderKind)
		repository, _ := NewEntityRepository(entity.NewEntityManager(), &repositoryStore{docs: map[string][]coredata.RawDocument{}}, repositoryGate(true))
		_, err := repository.LoadEntity(ctx, id, dataEngineNilBuilderKind)
		expectCorrupt(t, err, "DAO builder 0 is nil")
	})
	t.Run("remote-managed entity without a version envelope", func(t *testing.T) {
		id, _ := entity.BuildEntityID(1206, dataEngineRemoteRepositoryKind)
		store := &repositoryStore{docs: map[string][]coredata.RawDocument{"repository_remote": {repositoryRaw(t, "repository_remote", id, 3)}}}
		repository, _ := NewEntityRepository(entity.NewEntityManager(), store, repositoryGate(true))
		_, err := repository.LoadEntity(ctx, id, dataEngineRemoteRepositoryKind)
		expectCorrupt(t, err, "has no version envelope")
	})
}

// remoteTombstoneStore 在 repositoryStore 之上报告 Remote 元数据墓碑（MongoStore → Remote 原子存储的替身）。
type remoteTombstoneStore struct {
	*repositoryStore
	deleted map[int64]bool
}

func (store *remoteTombstoneStore) RemoteEntityDeleted(_ context.Context, id int64) (bool, error) {
	return store.deleted[id], nil
}

// Remote 托管实体删档时数据文档被物理删除，墓碑只在 Remote 元数据里：全部缺失时还要查它，才能区分删过档与从未创建（D10）。
// 本地实体不查（它的墓碑就在 DAO 文档里）。
func TestEntityRepositoryReadsRemoteTombstoneForAbsentRemoteAggregate(t *testing.T) {
	ensureDataEngineRemoteRepositoryEntity()
	ensureDataEngineRepositoryEntity()
	remote, _ := entity.BuildEntityID(996, dataEngineRemoteRepositoryKind)
	never, _ := entity.BuildEntityID(997, dataEngineRemoteRepositoryKind)
	local, _ := entity.BuildEntityID(998, dataEngineRepositoryKind)
	store := &remoteTombstoneStore{repositoryStore: &repositoryStore{docs: map[string][]coredata.RawDocument{}}, deleted: map[int64]bool{remote: true, local: true}}
	repository, _ := NewEntityRepository(entity.NewEntityManager(), store, repositoryGate(true))
	if _, err := repository.LoadEntity(context.Background(), remote, dataEngineRemoteRepositoryKind); !errors.Is(err, ErrEntityAggregateDeleted) {
		t.Fatalf("load remote tombstone err=%v, want deleted", err)
	}
	if _, err := repository.LoadEntity(context.Background(), never, dataEngineRemoteRepositoryKind); !errors.Is(err, ErrEntityAggregateNotFound) {
		t.Fatalf("load never-created remote id err=%v, want not found", err)
	}
	if _, err := repository.LoadEntity(context.Background(), local, dataEngineRepositoryKind); !errors.Is(err, ErrEntityAggregateNotFound) {
		t.Fatalf("local aggregate consulted the remote tombstone: %v", err)
	}
}

// ---- 新建实体的第一笔持久提交写齐聚合的每个 DAO ----
//
// 冷加载要求聚合的每个持久 DAO 都有文档（部分存在按损坏拒绝），而事务只登记改过的 DAO。多 DAO 实体新建后只改其中
// 一个就提交，另一个 DAO 永远没有文档，重启后这个实体再也加载不出来。下面经正式 Nest（业务池、锁内 WAL 准入）、
// 真实文件 WAL、投影与 Mongo 替身走完整链路，再用全新的 EntityManager + EntityRepository 冷加载（等价于重启后的
// 第一次加载）。三条新建入口各一例：handler 内 Create、handler 外 RunDetachedTransaction 里 Create、事务外 Create 后
// 由 handler 第一次写；最后确认首笔提交已接受的实体之后只写改过的 DAO。

// newAggregateKind 与同包其他用例的 kind 互不相同（见 entity_unload_test.go 的登记表）：kind 注册表是进程级的。
const (
	newAggregateKind     entity.EntityKind = 249
	newAggregateProfiles                   = "new_aggregate_profiles"
	newAggregateRanks                      = "new_aggregate_ranks"
)

// newAggregateDAO 与生成 DAO 同形：tracker 记已接受的持久版本（Dirty 返回它），版本 0 时准备整文档 Put，之后准备
// 单字段 patch；setter 登记 undo 与持久化变化。
type newAggregateDAO struct {
	id         int64
	collection string
	tracker    coredata.Tracker
	value      int64
}

func (dao *newAggregateDAO) Id() int64                       { return dao.id }
func (dao *newAggregateDAO) SetId(id int64)                  { dao.id = id }
func (*newAggregateDAO) DbName() string                      { return testDatabase }
func (dao *newAggregateDAO) CollName() string                { return dao.collection }
func (dao *newAggregateDAO) Dirty() entity.IDirty            { return &dao.tracker }
func (dao *newAggregateDAO) CleanDirty()                     { dao.tracker.SelfClean() }
func (*newAggregateDAO) SchemaVersion() uint32               { return 1 }
func (dao *newAggregateDAO) DirtyTracker() *coredata.Tracker { return &dao.tracker }

func (dao *newAggregateDAO) setValue(value int64) error {
	tx := corenest.CurrentRollbackTx()
	old := dao.value
	if err := tx.RecordUndo(dao, 1, func() error { dao.value = old; return nil }); err != nil {
		return err
	}
	dao.value = value
	return tx.MarkPersist(dao, 1)
}

func (dao *newAggregateDAO) PrepareMutation(change corenest.PersistChange) (coredata.Mutation, error) {
	version := dao.tracker.Version()
	mutation := coredata.Mutation{
		Key:  coredata.DocumentKey{Database: testDatabase, Resource: dao.collection, ID: dao.id},
		Kind: coredata.MutationPatch, ExpectedVersion: version, NextVersion: version + 1,
		Mask: change.Mask, Schema: 1, Codec: "bson-v2",
	}
	if version == 0 || change.Mask == coredata.AllFields {
		data, err := bson.Marshal(bson.M{"_id": dao.id, "value": dao.value})
		mutation.Kind, mutation.Data = coredata.MutationPut, data
		return mutation, err
	}
	patch, err := bson.Marshal(bson.D{{Key: "value", Value: dao.value}})
	mutation.Patch.SetBSON = patch
	return mutation, err
}

func (dao *newAggregateDAO) AcceptMutation(mutation coredata.Mutation) error {
	return dao.tracker.AcceptVersion(mutation.ExpectedVersion, mutation.NextVersion)
}

func (dao *newAggregateDAO) RestorePersisted(raw []byte, _ uint32, version uint64) error {
	var doc struct {
		ID    int64 `bson:"_id"`
		Value int64 `bson:"value"`
	}
	if err := bson.Unmarshal(raw, &doc); err != nil {
		return err
	}
	dao.id, dao.value = doc.ID, doc.Value
	dao.tracker.SetVersion(version)
	return nil
}

type newAggregateEntity struct {
	*entity.EntityBase
	entity.DaoManager
	profile *newAggregateDAO
	rank    *newAggregateDAO
}

func (value *newAggregateEntity) Base() *entity.EntityBase { return value.EntityBase }
func (*newAggregateEntity) AutoPersist() bool              { return true }

var registerNewAggregateKind sync.Once

func ensureNewAggregateKind() {
	registerNewAggregateKind.Do(func() {
		entity.RegisterEntityBuilder(&entity.EntityBuilderParam{
			Category: 1, Kind: newAggregateKind,
			DaoBuilders: []entity.DaoBuilderFunc{
				func() entity.DaoInterface { return &newAggregateDAO{collection: newAggregateProfiles} },
				func() entity.DaoInterface { return &newAggregateDAO{collection: newAggregateRanks} },
			},
			Builder: func(param *entity.EntityCreateParam) (entity.IThreadSafeEntity, error) {
				value := &newAggregateEntity{
					EntityBase: entity.NewEntityBaseWithMutex(param.Id, param.Category, false, param.Mutex, param.Kind),
					DaoManager: entity.NewDaoManager(),
				}
				value.profile, _ = param.Dao[newAggregateProfiles].(*newAggregateDAO)
				value.rank, _ = param.Dao[newAggregateRanks].(*newAggregateDAO)
				if value.profile == nil || value.rank == nil {
					return nil, errors.New("new aggregate test entity: missing DAO")
				}
				value.DaoManager.Set(newAggregateProfiles, value.profile)
				value.DaoManager.Set(newAggregateRanks, value.rank)
				return value, nil
			},
		})
	})
}

type newAggregateFixture struct {
	t         *testing.T
	ctx       context.Context
	store     *MongoStore
	client    *mongotest.Client
	projector *Projector
	access    *entity.ManagerAccess
	scheduler *corenest.NestMgr
	caller    int64
}

// newNewAggregateFixture：正式 Nest + 文件 WAL + Mongo 替身；caller 是库里已有的 unloadHero，handler 内新建时作为
// 消息目标。
func newNewAggregateFixture(t *testing.T) *newAggregateFixture {
	t.Helper()
	ensureUnloadKinds()
	ensureNewAggregateKind()
	store, client, _ := newMongoStoreTest(t)
	f := &newAggregateFixture{t: t, ctx: context.Background(), store: store, client: client}
	caller, err := entity.BuildEntityID(9301, unloadTestKind)
	if err != nil {
		t.Fatal(err)
	}
	f.caller = caller
	if err := client.Collection(testDatabase, unloadTestCollection).Seed(bson.M{"_id": caller, "_version": uint64(1), "_schema": uint32(1), "level": int32(1)}); err != nil {
		t.Fatal(err)
	}
	f.projector, _ = manualProjector(t, store)
	manager := entity.NewEntityManager()
	f.access = entity.NewManagerAccess(manager)
	repository, err := NewEntityRepository(manager, store, projectionGate{f.projector})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.access.ConfigureLoader(repository); err != nil {
		t.Fatal(err)
	}
	f.scheduler = corenest.NewEngine(corenest.NestOptionWithTransactionCommitter(f.projector), corenest.NestOptionWithGetter(f.access),
		corenest.NestOptionWithWorkerPools(corenest.WorkerPoolConfig{Workers: 4, QueueCap: 64}, corenest.WorkerPoolConfig{}, corenest.WorkerPoolConfig{}))
	meta := corenest.HandlerMeta{Rollback: corenest.RollbackUndo, Durability: corenest.DurabilityStrict}
	// new_aggregate_create：在 handler 内新建 params[0] 指定的实体，只改 profile。
	f.scheduler.MustRegisterHandlerWithMeta(corenest.NewHandlerName("new_aggregate_create"), func(_ []entity.IThreadSafeEntity, params []any, _ ...corenest.HandlerOption) (any, error) {
		created, err := f.access.Create(&entity.EntityCreateParam{IsCreate: true, Kind: newAggregateKind, Id: params[0].(int64)})
		if err != nil {
			return nil, err
		}
		return nil, created.(*newAggregateEntity).profile.setValue(7)
	}, meta)
	// new_aggregate_touch：目标实体的 profile 加一，rank 不动。
	f.scheduler.MustRegisterHandlerWithMeta(corenest.NewHandlerName("new_aggregate_touch"), func(es []entity.IThreadSafeEntity, _ []any, _ ...corenest.HandlerOption) (any, error) {
		profile := es[0].(*newAggregateEntity).profile
		return nil, profile.setValue(profile.value + 1)
	}, meta)
	if err := f.scheduler.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.scheduler.Shutdown(context.Background()) })
	return f
}

func (f *newAggregateFixture) id(uniqueID int64) int64 {
	f.t.Helper()
	id, err := entity.BuildEntityID(uniqueID, newAggregateKind)
	if err != nil {
		f.t.Fatal(err)
	}
	return id
}

func (f *newAggregateFixture) param(id int64) *entity.EntityCreateParam {
	return &entity.EntityCreateParam{IsCreate: true, Kind: newAggregateKind, Id: id}
}

// coldLoad 投影完在途记录，再用全新的 EntityManager + EntityRepository 读同一个库：等价于重启后的第一次冷加载。
func (f *newAggregateFixture) coldLoad(id int64) (*newAggregateEntity, error) {
	f.t.Helper()
	if err := f.projector.Flush(f.ctx); err != nil {
		f.t.Fatal(err)
	}
	repository, err := NewEntityRepository(entity.NewEntityManager(), f.store, nil)
	if err != nil {
		f.t.Fatal(err)
	}
	value, err := repository.LoadEntity(f.ctx, id, newAggregateKind)
	if err != nil {
		return nil, err
	}
	return value.(*newAggregateEntity), nil
}

// storedVersion 返回 Mongo 替身里该文档的 _version；没有文档时 ok=false。
func (f *newAggregateFixture) storedVersion(collection string, id int64) (uint64, bool) {
	doc, ok := f.client.Collection(testDatabase, collection).Lookup(id)
	if !ok {
		return 0, false
	}
	version, _ := doc["_version"].(int64)
	return uint64(version), true
}

func (f *newAggregateFixture) assertReloaded(id int64, profile int64) {
	f.t.Helper()
	loaded, err := f.coldLoad(id)
	if err != nil {
		f.t.Fatalf("cold load of the new entity after its first commit: %v", err)
	}
	if loaded.profile.value != profile || loaded.rank.value != 0 {
		f.t.Fatalf("reloaded profile=%d rank=%d, want profile=%d and the untouched rank at its initial 0", loaded.profile.value, loaded.rank.value, profile)
	}
	for _, dao := range []*newAggregateDAO{loaded.profile, loaded.rank} {
		if version := dao.tracker.Version(); version != 1 {
			f.t.Fatalf("reloaded %s version=%d, want 1 (one full document written by the first commit)", dao.collection, version)
		}
	}
}

func TestNewEntityFirstCommitWritesEveryDAOSoTheAggregateColdLoads(t *testing.T) {
	t.Run("created inside a handler", func(t *testing.T) {
		f := newNewAggregateFixture(t)
		id := f.id(9311)
		if _, err := f.scheduler.Request(f.ctx, corenest.NewHandlerName("new_aggregate_create"), f.caller, corenest.Params{id}); err != nil {
			t.Fatal(err)
		}
		f.assertReloaded(id, 7)
	})

	t.Run("created in a detached transaction outside a handler", func(t *testing.T) {
		f := newNewAggregateFixture(t)
		id := f.id(9312)
		if _, err := corenest.RunDetachedTransaction(f.ctx, f.projector, "new_aggregate_detached", func() (any, error) {
			created, err := f.access.Create(f.param(id))
			if err != nil {
				return nil, err
			}
			return nil, created.(*newAggregateEntity).profile.setValue(7)
		}); err != nil {
			t.Fatal(err)
		}
		f.assertReloaded(id, 7)
	})

	t.Run("created outside any transaction and first written by a handler", func(t *testing.T) {
		f := newNewAggregateFixture(t)
		id := f.id(9313)
		if _, err := f.access.Create(f.param(id)); err != nil {
			t.Fatal(err)
		}
		if _, err := f.scheduler.Request(f.ctx, corenest.NewHandlerName("new_aggregate_touch"), id, nil); err != nil {
			t.Fatal(err)
		}
		f.assertReloaded(id, 1)
	})

	// 首笔提交被接受后实体不再是“新建”：之后的普通提交只写改过的 DAO，不为其他 DAO 重写整文档。
	t.Run("after the first commit only the changed DAO is written", func(t *testing.T) {
		f := newNewAggregateFixture(t)
		id := f.id(9314)
		if _, err := f.scheduler.Request(f.ctx, corenest.NewHandlerName("new_aggregate_create"), f.caller, corenest.Params{id}); err != nil {
			t.Fatal(err)
		}
		if _, err := f.scheduler.Request(f.ctx, corenest.NewHandlerName("new_aggregate_touch"), id, nil); err != nil {
			t.Fatal(err)
		}
		if err := f.projector.Flush(f.ctx); err != nil {
			t.Fatal(err)
		}
		profile, profileOK := f.storedVersion(newAggregateProfiles, id)
		rank, rankOK := f.storedVersion(newAggregateRanks, id)
		if !profileOK || profile != 2 || !rankOK || rank != 1 {
			t.Fatalf("stored versions profile=%d(%t) rank=%d(%t), want profile=2 and rank still 1", profile, profileOK, rank, rankOK)
		}
	})
}
