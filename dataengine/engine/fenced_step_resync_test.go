package engine

import (
	"context"
	"sync"
	"testing"
	"time"

	coredata "github.com/tjbdwanghaibo/roost-core/dataengine"
	"github.com/tjbdwanghaibo/roost-core/entity"
	"github.com/tjbdwanghaibo/roost-core/mongo/mongotest"
	corenest "github.com/tjbdwanghaibo/roost-core/nest"
	"github.com/tjbdwanghaibo/roost-core/sync/entitysync"
	"github.com/tjbdwanghaibo/roost-core/sync/frame"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// RR-20260926-59（RR-30 场景）：原生步骤在 Nest 内存生效、订阅者已收到含其效果的内容，投影时租约失效被跳过，
// DataEngine 在快池驱逐该实体。修复前订阅者停在从未成为权威的内容上，直到有人重新访问；修复后框架在快池之外
// 经正式 EntityRepository（等待该实体投影/驱逐完成、发布回到快池、OnEntityLoaded）从 Mongo 重载并 Rebind，
// 订阅者收到权威全量；Mongo 里没有该实体时退回 remove。

// fencedResyncKind 与同包其他用例的 kind 互不相同（RR-20260926-82）：kind 注册表是进程级的，原值 243 与
// entity_repository_load_promises_test.go 的 dataEngineNoBuilderKind 相同。本用例给 243 注册了 builder，同一进程里
// 第二轮（-count>1）“kind 没有 builder”的用例就不再成立。
const fencedResyncKind entity.EntityKind = 248
const fencedResyncCollection = "resync_heroes"

type fencedResyncDAO struct {
	id    int64
	level int32
}

func (dao *fencedResyncDAO) Id() int64         { return dao.id }
func (dao *fencedResyncDAO) SetId(id int64)    { dao.id = id }
func (*fencedResyncDAO) DbName() string        { return testDatabase }
func (*fencedResyncDAO) CollName() string      { return fencedResyncCollection }
func (*fencedResyncDAO) Dirty() entity.IDirty  { return nil }
func (*fencedResyncDAO) CleanDirty()           {}
func (*fencedResyncDAO) SchemaVersion() uint32 { return 1 }
func (dao *fencedResyncDAO) RestorePersisted(raw []byte, _ uint32, _ uint64) error {
	var doc struct {
		Level int32 `bson:"level"`
	}
	if err := bson.Unmarshal(raw, &doc); err != nil {
		return err
	}
	dao.level = doc.Level
	return nil
}

type fencedResyncEntity struct {
	*entity.EntityBase
	mu    sync.Mutex
	level int32
}

func (value *fencedResyncEntity) Base() *entity.EntityBase { return value.EntityBase }

func (value *fencedResyncEntity) setLevel(level int32) {
	value.mu.Lock()
	value.level = level
	value.mu.Unlock()
}

var registerFencedResyncEntity sync.Once

func ensureFencedResyncEntity() {
	registerFencedResyncEntity.Do(func() {
		entity.RegisterEntityBuilder(&entity.EntityBuilderParam{
			Category: 1, Kind: fencedResyncKind,
			DaoBuilders: []entity.DaoBuilderFunc{func() entity.DaoInterface { return &fencedResyncDAO{} }},
			Sync: entity.EntitySyncBuilderParam{Enabled: true, Namespace: "hero", PackerFactory: func(e entity.IThreadSafeEntity) entity.SubjectSyncPacker {
				value := e.(*fencedResyncEntity)
				pack := func(entity.SyncProfile) (entity.FrozenSyncPayload, error) {
					value.mu.Lock()
					defer value.mu.Unlock()
					return entity.CopyFrozenSyncPayload(1, []byte{byte(value.level)}), nil
				}
				return entity.SubjectSyncPackFunc{Snapshot: pack, Delta: func(p entity.SyncProfile, _ uint64) (entity.FrozenSyncPayload, error) { return pack(p) }}
			}},
			Builder: func(param *entity.EntityCreateParam) (entity.IThreadSafeEntity, error) {
				value := &fencedResyncEntity{EntityBase: entity.NewEntityBaseWithMutex(param.Id, param.Category, false, param.Mutex, param.Kind)}
				if dao, ok := param.Dao[fencedResyncCollection].(*fencedResyncDAO); ok {
					value.level = dao.level
				}
				return value, nil
			},
		})
	})
}

// projectionGate 是 Runtime 作 RecoveryGate 时的最小等价物：已就绪，冷加载等该实体的在途投影与驱逐。
type projectionGate struct{ projector *Projector }

func (projectionGate) Ready() bool { return true }
func (gate projectionGate) WaitEntityProjection(ctx context.Context, id int64) error {
	return gate.projector.WaitEntityProjection(ctx, id)
}

type fencedResyncFixture struct {
	t        *testing.T
	id       int64
	store    *MongoStore
	docs     *mongotest.Collection
	project  *Projector
	manager  *entity.EntityManager
	access   *entity.ManagerAccess
	sync     *entitysync.Manager
	frames   chan []byte
	ref      frame.ObjectRef
	resident *fencedResyncEntity
}

func newFencedResyncFixture(t *testing.T, uniqueID int64) *fencedResyncFixture {
	t.Helper()
	ensureFencedResyncEntity()
	id, err := entity.BuildEntityID(uniqueID, fencedResyncKind)
	if err != nil {
		t.Fatal(err)
	}
	store, client, _ := newMongoStoreTest(t)
	docs := client.Collection(testDatabase, fencedResyncCollection)
	if err = docs.Seed(bson.M{"_id": id, "_version": uint64(4), "_schema": uint32(1), "level": int32(1)}); err != nil {
		t.Fatal(err)
	}
	seedClaim(t, client, fencedClaim, "worker-1", 7, fencedDigest, time.Now().UTC().Add(-time.Second)) // 租约已过期：原生步骤会被跳过
	projector, _ := manualProjector(t, store)
	f := &fencedResyncFixture{t: t, id: id, store: store, docs: docs, project: projector, manager: entity.NewEntityManager(), frames: make(chan []byte, 16)}
	f.access = entity.NewManagerAccess(f.manager)
	repository, err := NewEntityRepository(f.manager, store, projectionGate{projector})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.access.ConfigureLoader(repository); err != nil {
		t.Fatal(err)
	}
	runtime := &Runtime{Projector: projector, access: f.access}
	projector.evictEntities = runtime.evictStaleEntities
	f.sync, err = entitysync.NewManager(entitysync.ManagerConfig{Mode: entitysync.ModePeriodic, Interval: time.Hour, Transport: entitysync.TransportFunc(func(_ context.Context, _ entitysync.SessionID, p []byte) error {
		f.frames <- append([]byte(nil), p...)
		return nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.sync.Close(context.Background()) })
	// 与 kit Nest Mod 相同的接线：重载发布时 Rebind（OnEntityLoaded）；NewEngine 把 NestMgr.RunLocal 绑定给作为
	// Getter 的 ManagerAccess（BindLocalExecutor），卸载后重载与业务冷加载同一条共享加载，经它回快池发布。
	unhook := repository.OnEntityLoaded(func(loaded entity.IThreadSafeEntity) { _ = f.sync.Rebind(loaded.Base().Sync()) })
	t.Cleanup(unhook)
	scheduler := corenest.NewEngine(corenest.NestOptionWithTransactionCommitter(projector), corenest.NestOptionWithGetter(f.access))
	if err = scheduler.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = scheduler.Shutdown(context.Background()) })
	stop, err := f.access.ConfigureUnloadResync(f.sync, entity.UnloadResyncConfig{Attempts: 3, RetryMin: time.Millisecond, RetryMax: 5 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stop(context.Background()) })

	loaded, err := f.access.Get(entity.WithLocalExecutor(context.Background(), func(fn func()) error { return scheduler.RunLocal(context.Background(), fn) }), id, entity.EntityCategoryNone)
	if err != nil || loaded == nil {
		t.Fatalf("initial load: %v", err)
	}
	f.resident = loaded.(*fencedResyncEntity)
	if err = f.sync.Register(f.resident.Sync()); err != nil {
		t.Fatal(err)
	}
	if err = f.sync.OpenSession(1); err != nil {
		t.Fatal(err)
	}
	if err = f.sync.Subscribe(1, id, entity.SyncProfile{}); err != nil {
		t.Fatal(err)
	}
	created := f.next("the initial create")
	if len(created.Objects) != 1 || created.Objects[0].Operation != frame.ObjectCreate || f.level(created.Objects[0]) != 1 {
		t.Fatalf("initial frame=%+v", created.Objects)
	}
	f.ref = created.Objects[0].Ref
	return f
}

func (f *fencedResyncFixture) next(what string) frame.Frame {
	f.t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		select {
		case raw := <-f.frames:
			decoded, err := entitysync.DecodeFrame(raw, frame.DefaultLimits())
			if err != nil {
				f.t.Fatal(err)
			}
			return decoded
		default:
		}
		if time.Now().After(deadline) {
			f.t.Fatalf("no frame within 3s: %s", what)
		}
		if err := f.sync.Flush(context.Background()); err != nil {
			f.t.Fatal(err)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func (f *fencedResyncFixture) level(object frame.ObjectDelta) int32 {
	f.t.Helper()
	update, err := entitysync.DecodeSubjectUpdate(object.Components[0].Data, 0)
	if err != nil {
		f.t.Fatal(err)
	}
	payload := update.Payload.BytesCopy()
	if len(payload) != 1 {
		f.t.Fatalf("payload=%v", payload)
	}
	return int32(payload[0])
}

// applySkippedStep 模拟 Nest：原生步骤在内存把 level 1 → 5 并准入 WAL，订阅者先收到 level=5。
func (f *fencedResyncFixture) applySkippedStep() {
	f.t.Helper()
	f.resident.setLevel(5)
	f.resident.MarkSyncDirty(1)
	speculative := f.next("the frame carrying the fenced step's in-memory effect")
	if len(speculative.Objects) != 1 || f.level(speculative.Objects[0]) != 5 {
		f.t.Fatalf("premise: subscriber frame=%+v, want level 5", speculative.Objects)
	}
	record := fencedDebit(f.t, 9, 7)
	record.Mutations[0].Key = coredata.DocumentKey{Database: testDatabase, Resource: fencedResyncCollection, ID: f.id}
	if err := commitReleased(f.project, record); err != nil {
		f.t.Fatal(err)
	}
	if err := f.project.Flush(context.Background()); err != nil {
		f.t.Fatal(err)
	}
	if err := f.project.WaitEntityProjection(context.Background(), f.id); err != nil {
		f.t.Fatal(err)
	}
	if f.manager.Get(f.id) == entity.IThreadSafeEntity(f.resident) {
		f.t.Fatal("premise: the entity carrying the skipped step is still resident")
	}
}

func TestSkippedFencedStepResyncsSubscribersFromMongo(t *testing.T) {
	f := newFencedResyncFixture(t, 2431)
	f.applySkippedStep()
	correction := f.next("the fenced step was skipped and the entity evicted, but the subscriber still holds level 5 (no authoritative full, no remove)")
	if len(correction.Objects) != 1 || correction.Objects[0].Operation != frame.ObjectUpdate || correction.Objects[0].Ref != f.ref {
		t.Fatalf("correction frame=%+v, want one ObjectUpdate of the held object %+v", correction.Objects, f.ref)
	}
	if got := f.level(correction.Objects[0]); got != 1 {
		t.Fatalf("subscriber got level %d after the skip, want the Mongo level 1", got)
	}
	reloaded, ok := f.manager.Get(f.id).(*fencedResyncEntity)
	if !ok || reloaded == f.resident {
		t.Fatal("the Mongo instance is not resident after the resync")
	}
	if doc, _ := f.docs.Lookup(f.id); doc["_version"] != int64(4) {
		t.Fatalf("resync persisted something: %v", doc)
	}
}

func TestSkippedFencedStepWithoutAuthorityRetractsSubscribers(t *testing.T) {
	f := newFencedResyncFixture(t, 2432)
	if _, err := f.docs.DeleteOne(context.Background(), bson.M{"_id": f.id}); err != nil { // 权威中不存在（例如被跳过的正是它的首次落库）
		t.Fatal(err)
	}
	f.applySkippedStep()
	correction := f.next("the fenced step was skipped and Mongo has no such entity, but the subscriber got no remove")
	if len(correction.Objects) != 1 || correction.Objects[0].Operation != frame.ObjectRemove {
		t.Fatalf("correction frame=%+v, want ObjectRemove", correction.Objects)
	}
	if f.manager.Get(f.id) != nil {
		t.Fatal("an entity missing from Mongo became resident")
	}
	if stats := f.access.UnloadResyncStats(); stats.Retracted != 1 || stats.Failures != 0 {
		t.Fatalf("missing authority was retried instead of retracted at once: %+v", stats)
	}
}
