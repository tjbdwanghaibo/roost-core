package dataengine

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	corecheckpoint "github.com/tjbdwanghaibo/cube-core/checkpoint"
	coredata "github.com/tjbdwanghaibo/cube-core/dataengine"
	"github.com/tjbdwanghaibo/cube-core/entity"
	"go.mongodb.org/mongo-driver/v2/bson"
)

const dataEngineRepositoryKind entity.EntityKind = 239
const dataEngineRemoteRepositoryKind entity.EntityKind = 240

type dataEngineRepositoryDAO struct {
	id         int64
	collection string
	version    uint64
}

func (dao *dataEngineRepositoryDAO) Id() int64                                { return dao.id }
func (dao *dataEngineRepositoryDAO) SetId(id int64)                           { dao.id = id }
func (*dataEngineRepositoryDAO) DbName() string                               { return "game" }
func (dao *dataEngineRepositoryDAO) CollName() string                         { return dao.collection }
func (*dataEngineRepositoryDAO) Dirty() entity.IDirty                         { return nil }
func (*dataEngineRepositoryDAO) CleanDirty()                                  {}
func (*dataEngineRepositoryDAO) SchemaVersion() uint32                        { return 1 }
func (*dataEngineRepositoryDAO) Migrate(raw []byte, _ uint32) ([]byte, error) { return raw, nil }
func (dao *dataEngineRepositoryDAO) RestorePersisted(raw []byte, _ uint32, version uint64) error {
	var doc struct {
		ID int64 `bson:"_id"`
	}
	if err := bson.Unmarshal(raw, &doc); err != nil {
		return err
	}
	dao.id, dao.version = doc.ID, version
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
func (value *dataEngineRepositoryEntity) RemoveSnapshot() []corecheckpoint.SaveItem {
	return []corecheckpoint.SaveItem{{Db: "game", Collection: "repository_profile", ID: value.ID(), Version: 1, Deleted: true}}
}

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
func (value *dataEngineRemoteRepositoryEntity) RemoveSnapshot() []corecheckpoint.SaveItem {
	return []corecheckpoint.SaveItem{{Db: "game", Collection: "repository_remote", ID: value.ID(), Version: 1, Deleted: true}}
}

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
	delay        time.Duration
}

func (store *repositoryStore) ReadConsistent(ctx context.Context, read func(context.Context) error) error {
	store.transactions.Add(1)
	if store.delay > 0 {
		time.Sleep(store.delay)
	}
	return read(ctx)
}
func (store *repositoryStore) Load(_ context.Context, spec coredata.LoadSpec) ([]coredata.RawDocument, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	return append([]coredata.RawDocument(nil), store.docs[spec.Resource]...), nil
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

type repositoryMigrationCommitter struct{ store *repositoryStore }

func (committer repositoryMigrationCommitter) CommitSystem(_ context.Context, record coredata.CommitRecord) (coredata.ProjectionTicket, error) {
	mutation := record.Mutations[0]
	committer.store.mu.Lock()
	committer.store.docs[mutation.Key.Resource] = []coredata.RawDocument{{
		Key: mutation.Key, Version: mutation.NextVersion, Schema: mutation.Schema, Data: append([]byte(nil), mutation.Data...),
	}}
	committer.store.mu.Unlock()
	done := make(chan struct{})
	close(done)
	return projectedSystemTicket{done: done}, nil
}

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
	repository, err := newEntityRepository(manager, store, nil, repositoryGate(true))
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
	notReady, _ := newEntityRepository(manager, store, nil, repositoryGate(false))
	if _, err := notReady.LoadEntity(context.Background(), id, dataEngineRepositoryKind); !errors.Is(err, coredata.ErrRecoveryIncomplete) {
		t.Fatalf("barrier err=%v", err)
	}
	repository, _ := newEntityRepository(manager, store, nil, repositoryGate(true))
	if _, err := repository.LoadEntity(context.Background(), id, dataEngineRepositoryKind); !errors.Is(err, ErrEntityAggregateNotFound) {
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
	repository, _ := newEntityRepository(entity.NewEntityManager(), store, nil, repositoryGate(true))
	if _, err := repository.LoadEntity(context.Background(), id, dataEngineRepositoryKind); !errors.Is(err, ErrEntityAggregateNotFound) {
		t.Fatalf("tombstone err=%v", err)
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
	repository, _ := newEntityRepository(entity.NewEntityManager(), store, nil, repositoryGate(true))
	loaded, err := repository.LoadEntity(context.Background(), id, dataEngineRemoteRepositoryKind)
	if err != nil {
		t.Fatal(err)
	}
	vector := loaded.(*dataEngineRemoteRepositoryEntity).RemoteVersionVector()
	if vector != (entity.RemoteVersionVector{StateVersion: 12, MarkerEpoch: 3, LockFence: 8, RouteEpoch: 5}) {
		t.Fatalf("vector=%+v", vector)
	}
}

func TestEntityRepositoryMigratesThenReloadsBeforePublishing(t *testing.T) {
	ensureDataEngineRepositoryEntity()
	id, _ := entity.BuildEntityID(995, dataEngineRepositoryKind)
	oldProfile := repositoryRaw(t, "repository_profile", id, 4)
	oldProfile.Schema = 0
	oldProfile.Data, _ = bson.Marshal(bson.M{"_id": id, "_schema": uint32(0), "name": "old"})
	store := &repositoryStore{docs: map[string][]coredata.RawDocument{
		"repository_profile":   {oldProfile},
		"repository_inventory": {repositoryRaw(t, "repository_inventory", id, 4)},
	}}
	runner, _ := NewMigrationRunner(repositoryMigrationCommitter{store: store})
	repository, _ := newEntityRepository(entity.NewEntityManager(), store, runner, repositoryGate(true))
	loaded, err := repository.LoadEntity(context.Background(), id, dataEngineRepositoryKind)
	if err != nil {
		t.Fatal(err)
	}
	profile := loaded.(*dataEngineRepositoryEntity).daos["repository_profile"]
	if profile.version != 5 || store.transactions.Load() != 2 {
		t.Fatalf("profile version=%d snapshot transactions=%d", profile.version, store.transactions.Load())
	}
}
