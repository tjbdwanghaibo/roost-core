package entity

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
)

const (
	testEntityCategory     EntityCategory = 3
	testEntityKind         EntityKind     = 3
	testRemoteBadKind      EntityKind     = 252
	testRuntimeRebuildKind EntityKind     = 253
)

// factoryTestDao implements DaoInterface for factory tests.
type factoryTestDao struct {
	id   int64
	coll string
}

func (d *factoryTestDao) Id() int64        { return d.id }
func (d *factoryTestDao) SetId(id int64)   { d.id = id }
func (d *factoryTestDao) DbName() string   { return "test" }
func (d *factoryTestDao) CollName() string { return d.coll }
func (d *factoryTestDao) Dirty() IDirty    { return nil }
func (d *factoryTestDao) CleanDirty()      {}

// factoryTestEntity implements IThreadSafeEntity.
type factoryTestEntity struct {
	*EntityBase
	ComponentManager
	DaoManager
	initCalled bool
}

func (e *factoryTestEntity) Base() *EntityBase { return e.EntityBase }

func init() {
	// Register test builder in a high type number to avoid conflicts
	RegisterEntityBuilder(&EntityBuilderParam{
		Category: testEntityCategory,
		Kind:     testEntityKind,
		Builder: func(param *EntityCreateParam) (IThreadSafeEntity, error) {
			e := &factoryTestEntity{}
			e.EntityBase = NewEntityBaseWithMutex(param.Id, param.Category, false, param.Mutex, param.Kind)
			e.ComponentManager = NewComponentManager()
			e.DaoManager = NewDaoManager()
			e.initCalled = true

			// Wire DAOs
			for coll, dao := range param.Dao {
				e.DaoManager.Set(coll, dao)
			}
			return e, nil
		},
		DaoBuilders: []DaoBuilderFunc{
			func() DaoInterface { return &factoryTestDao{coll: "test_coll"} },
		},
	})
	RegisterEntityBuilder(&EntityBuilderParam{
		Category:     testEntityCategory,
		Kind:         testRemoteBadKind,
		RemotePolicy: RemotePolicyManaged,
		NoPersist:    true,
		Builder: func(param *EntityCreateParam) (IThreadSafeEntity, error) {
			return &factoryTestEntity{
				EntityBase: NewEntityBaseWithMutex(param.Id, param.Category, true, param.Mutex, param.Kind),
			}, nil
		},
	})
	RegisterEntityBuilder(&EntityBuilderParam{
		Category:  testEntityCategory,
		Kind:      testRuntimeRebuildKind,
		NoPersist: true,
		Lifetime:  EntityLifetimeRuntimeRebuild,
		Builder: func(param *EntityCreateParam) (IThreadSafeEntity, error) {
			return &factoryTestEntity{
				EntityBase: NewEntityBaseWithMutex(param.Id, param.Category, true, param.Mutex, param.Kind),
			}, nil
		},
	})
}

func TestNewEntity_Create(t *testing.T) {
	var seq atomic.Uint64
	manager := NewEntityManager(WithEntityIDGenerator(func() (uint64, error) {
		return seq.Add(1), nil
	}))

	param := &EntityCreateParam{
		IsCreate: true,
		Category: testEntityCategory,
		Kind:     testEntityKind,
	}

	e, err := manager.Create(param)
	if err != nil {
		t.Fatalf("NewEntity: %v", err)
	}

	if e.ID() == 0 {
		t.Fatal("entity should have generated ID")
	}
	if e.GetEntityCategory() != testEntityCategory {
		t.Fatalf("expected type %d, got %d", testEntityCategory, e.GetEntityCategory())
	}

	fe := e.(*factoryTestEntity)
	if !fe.initCalled {
		t.Fatal("builder should have been called")
	}

	// DAO should be created and wired
	dao := fe.DaoManager.Get("test_coll")
	if dao == nil {
		t.Fatal("DAO should be wired")
	}
	if dao.Id() != e.StorageID() {
		t.Fatalf("DAO id should match entity storage id: %d != %d", dao.Id(), e.StorageID())
	}

	// Should be in manager
	if manager.Get(e.ID()) != e {
		t.Fatal("entity should be in manager")
	}
}

func TestEntityManagerCreateUsesInstanceDependencies(t *testing.T) {
	var sequence atomic.Uint64
	manager := NewEntityManager(WithEntityIDGenerator(func() (uint64, error) {
		return sequence.Add(1), nil
	}))
	access := NewManagerAccess(manager)
	value, err := access.Create(&EntityCreateParam{IsCreate: true, Kind: testEntityKind})
	if err != nil {
		t.Fatal(err)
	}
	if value == nil || manager.Get(value.ID()) != value || value.UniqueID() != 1 {
		t.Fatalf("instance create did not publish generated entity: %+v", value)
	}
	id := value.ID()
	if err := access.Destroy(context.Background(), value, EntityDestroyReason(0), false); err != nil {
		t.Fatal(err)
	}
	if manager.Get(id) != nil {
		t.Fatal("instance destroy did not remove entity")
	}
}

func TestEntityManagerCreateInScopeLocksBeforePublication(t *testing.T) {
	manager := NewEntityManager()
	id := mustBuildTestEntityID(t, 99, testEntityCategory, testEntityKind)
	err := WithGuardScope("create_publish_order", func(scope *GuardScope) error {
		value, err := manager.CreateInScope(scope, &EntityCreateParam{
			Id:       id,
			UniqueID: 99,
			Category: testEntityCategory,
			Kind:     testEntityKind,
		})
		if err != nil {
			return err
		}
		if manager.Get(id) != value {
			t.Fatal("entity was not published")
		}
		locked := make(chan bool, 1)
		go func() {
			if value.GetMutex().TryLock() {
				value.GetMutex().Unlock()
				locked <- true
				return
			}
			locked <- false
		}()
		if <-locked {
			t.Fatal("published entity mutex was not held by the creation scope")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestInitEntitySyncInstallsContentState(t *testing.T) {
	e := &factoryTestEntity{
		EntityBase:       NewEntityBase(301, testEntityCategory, true, testEntityKind),
		ComponentManager: NewComponentManager(),
		DaoManager:       NewDaoManager(),
	}
	bp := &EntityBuilderParam{Sync: EntitySyncBuilderParam{
		Enabled:   true,
		Namespace: "factory.subject",
		PackerFactory: func(IThreadSafeEntity) SubjectSyncPacker {
			return SubjectSyncPackFunc{
				Snapshot: func(SyncProfile) (FrozenSyncPayload, error) {
					return CopyFrozenSyncPayload(7, []byte("snapshot")), nil
				},
			}
		},
	}}

	initEntitySync(e, nil, bp)
	state := e.Sync()
	if state == nil || state.SubjectID() != e.ID() || state.Namespace() != "factory.subject" || state.SubjectKind() != uint32(testEntityKind) {
		t.Fatalf("subject sync state mismatch: %+v", state)
	}
	prepared, err := state.PrepareViews(nil, []SyncProfile{{}})
	if err != nil {
		t.Fatalf("subject snapshot: %v", err)
	}
	defer func() { _ = prepared.Abort() }()
	if updates := prepared.Snapshots(); len(updates) != 1 || updates[0].Payload.Codec() != 7 {
		t.Fatalf("subject snapshot updates=%+v", updates)
	}
}

func TestBuildEntity_RemoteManagedRequiresRemoteInterface(t *testing.T) {
	_, err := BuildEntity(&EntityCreateParam{
		IsCreate: true,
		Category: testEntityCategory,
		Kind:     testRemoteBadKind,
		UniqueID: 1,
	})
	if err == nil {
		t.Fatal("expected remote managed policy validation error")
	}
}

func TestBuildEntity_LifetimePolicy(t *testing.T) {
	e, err := BuildEntity(&EntityCreateParam{
		IsCreate: true,
		Category: testEntityCategory,
		Kind:     testRuntimeRebuildKind,
		UniqueID: 2,
	})
	if err != nil {
		t.Fatalf("BuildEntity: %v", err)
	}
	if e.Base().Lifetime() != EntityLifetimeRuntimeRebuild {
		t.Fatalf("lifetime = %d, want %d", e.Base().Lifetime(), EntityLifetimeRuntimeRebuild)
	}
}

func TestNewEntity_Load(t *testing.T) {
	manager := NewEntityManager()

	// Simulate loading with pre-existing DAO. Persistent IDs are full entity IDs.
	wantID := mustBuildTestEntityID(t, 42, testEntityCategory, testEntityKind)
	existingDao := &factoryTestDao{id: wantID, coll: "test_coll"}
	param := &EntityCreateParam{
		IsCreate: false,
		Category: testEntityCategory,
		Kind:     testEntityKind,
		Id:       wantID,
		Dao:      map[string]DaoInterface{"test_coll": existingDao},
	}

	e, err := manager.Create(param)
	if err != nil {
		t.Fatalf("NewEntity: %v", err)
	}

	if e.ID() != wantID {
		t.Fatalf("expected id %d, got %d", wantID, e.ID())
	}
	if e.StorageID() != wantID {
		t.Fatalf("expected storage id %d, got %d", wantID, e.StorageID())
	}

	fe := e.(*factoryTestEntity)
	dao := fe.DaoManager.Get("test_coll")
	if dao != existingDao {
		t.Fatal("loaded DAO should be the one provided")
	}
}

func TestNewEntity_UnregisteredType(t *testing.T) {
	manager := NewEntityManager(WithEntityIDGenerator(func() (uint64, error) { return 1, nil }))

	param := &EntityCreateParam{
		IsCreate: true,
		Category: EntityCategory(255), // not registered
		Kind:     EntityKind(255),
	}

	_, err := manager.Create(param)
	if err == nil {
		t.Fatal("expected error for unregistered type")
	}
}

// newAggregateRecorder 替代 dataengine.Tracker 记下 BuildEntity 绑定的聚合（entity 包测试不能 import dataengine）。
type newAggregateRecorder struct{ aggregate *DaoAggregate }

func (r *newAggregateRecorder) Dirty() bool                           { return false }
func (r *newAggregateRecorder) SelfClean()                            {}
func (r *newAggregateRecorder) BindAggregate(aggregate *DaoAggregate) { r.aggregate = aggregate }

type newAggregateFactoryDao struct {
	factoryTestDao
	tracker newAggregateRecorder
}

func (d *newAggregateFactoryDao) Dirty() IDirty { return &d.tracker }

const (
	newAggregatePersistentKind EntityKind = 221
	newAggregateNoPersistKind  EntityKind = 222
)

func init() {
	for _, kind := range []EntityKind{newAggregatePersistentKind, newAggregateNoPersistKind} {
		RegisterEntityBuilder(&EntityBuilderParam{
			Category: testEntityCategory, Kind: kind, NoPersist: kind == newAggregateNoPersistKind,
			Builder: func(param *EntityCreateParam) (IThreadSafeEntity, error) {
				e := &factoryTestEntity{EntityBase: NewEntityBaseWithMutex(param.Id, param.Category, false, param.Mutex, param.Kind), DaoManager: NewDaoManager()}
				for coll, dao := range param.Dao {
					e.DaoManager.Set(coll, dao)
				}
				return e, nil
			},
			DaoBuilders: []DaoBuilderFunc{
				func() DaoInterface { return &newAggregateFactoryDao{factoryTestDao: factoryTestDao{coll: "rank"}} },
				func() DaoInterface { return &newAggregateFactoryDao{factoryTestDao: factoryTestDao{coll: "profile"}} },
			},
		})
	}
}

// 持久实体（新建与冷加载）把全部 DAO（按集合名排序）作为同一个 DaoAggregate 绑定到每个 DAO 的 tracker：持久事务据此
// 让新建实体的创建事务、冷加载补空 DAO 的下一笔提交写齐聚合；noPersist 实体不绑定。
func TestBuildEntityBindsTheDAOAggregateForPersistentEntities(t *testing.T) {
	daoOf := func(e IThreadSafeEntity, coll string) *newAggregateFactoryDao {
		return e.(*factoryTestEntity).DaoManager.Get(coll).(*newAggregateFactoryDao)
	}
	assertBound := func(name string, e IThreadSafeEntity) {
		t.Helper()
		profile, rank := daoOf(e, "profile"), daoOf(e, "rank")
		aggregate := profile.tracker.aggregate
		if rank.tracker.aggregate != aggregate {
			t.Fatalf("%s: profile and rank bound different aggregates", name)
		}
		if got := aggregate.DAOs(); len(got) != 2 || got[0] != DaoInterface(profile) || got[1] != DaoInterface(rank) {
			t.Fatalf("%s: bound %v, want [profile rank] of the same entity", name, got)
		}
	}
	created, err := BuildEntity(&EntityCreateParam{IsCreate: true, Kind: newAggregatePersistentKind, Id: mustBuildTestEntityID(t, 51, testEntityCategory, newAggregatePersistentKind)})
	if err != nil {
		t.Fatal(err)
	}
	assertBound("created", created)

	loadedID := mustBuildTestEntityID(t, 52, testEntityCategory, newAggregatePersistentKind)
	loadedDAOs := map[string]DaoInterface{
		"profile": &newAggregateFactoryDao{factoryTestDao: factoryTestDao{id: loadedID, coll: "profile"}},
		"rank":    &newAggregateFactoryDao{factoryTestDao: factoryTestDao{id: loadedID, coll: "rank"}},
	}
	loaded, err := BuildEntity(&EntityCreateParam{Kind: newAggregatePersistentKind, Id: loadedID, Dao: loadedDAOs})
	if err != nil {
		t.Fatal(err)
	}
	assertBound("loaded", loaded)

	transient, err := BuildEntity(&EntityCreateParam{IsCreate: true, Kind: newAggregateNoPersistKind, Id: mustBuildTestEntityID(t, 53, testEntityCategory, newAggregateNoPersistKind)})
	if err != nil {
		t.Fatal(err)
	}
	for _, coll := range []string{"profile", "rank"} {
		if got := daoOf(transient, coll).tracker.aggregate; got != nil {
			t.Fatalf("noPersist entity bound an aggregate on %s: %v", coll, got.DAOs())
		}
	}
}

// 事务外新建持久实体经 CreateAdmitter（DataEngine 注册）：Create 没有事务捕获者时把 CreateInScope 交给准入门；
// 准入门给的作用域没有捕获者时 CreateInScope 拒绝，不会悄悄只发布到内存。非持久 kind 与没有准入门时照旧立即发布。
func TestEntityManagerCreateOutsideATransactionGoesThroughTheCreateAdmitter(t *testing.T) {
	manager := NewEntityManager()
	var admitted []EntityKind
	unregister, err := manager.RegisterCreateAdmitter(func(_ context.Context, create func(*GuardScope) (IThreadSafeEntity, error)) (IThreadSafeEntity, error) {
		var created IThreadSafeEntity
		err := WithGuardScope("test_create_admitter", func(scope *GuardScope) error {
			restore := scope.Guard().SwapCreatedEntityCapturer(acceptingCapturer{})
			defer scope.Guard().SwapCreatedEntityCapturer(restore)
			value, err := create(scope)
			if value != nil {
				admitted = append(admitted, value.GetEntityKind())
			}
			created = value
			return err
		})
		return created, err
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.RegisterCreateAdmitter(func(context.Context, func(*GuardScope) (IThreadSafeEntity, error)) (IThreadSafeEntity, error) {
		return nil, nil
	}); !errors.Is(err, ErrCreateAdmitterExists) {
		t.Fatalf("second admitter err=%v", err)
	}

	persistentID := mustBuildTestEntityID(t, 61, testEntityCategory, newAggregatePersistentKind)
	if _, err := manager.Create(&EntityCreateParam{IsCreate: true, Kind: newAggregatePersistentKind, Id: persistentID}); err != nil {
		t.Fatal(err)
	}
	if len(admitted) != 1 || admitted[0] != newAggregatePersistentKind || manager.Get(persistentID) == nil {
		t.Fatalf("persistent create admitted=%v published=%v, want it to go through the admitter", admitted, manager.Get(persistentID) != nil)
	}
	transientID := mustBuildTestEntityID(t, 62, testEntityCategory, newAggregateNoPersistKind)
	if _, err := manager.Create(&EntityCreateParam{IsCreate: true, Kind: newAggregateNoPersistKind, Id: transientID}); err != nil {
		t.Fatal(err)
	}
	if len(admitted) != 1 || manager.Get(transientID) == nil {
		t.Fatalf("noPersist create admitted=%v, want it published directly", admitted)
	}

	bareID := mustBuildTestEntityID(t, 63, testEntityCategory, newAggregatePersistentKind)
	err = WithGuardScope("bare", func(scope *GuardScope) error {
		_, err := manager.CreateInScope(scope, &EntityCreateParam{IsCreate: true, Kind: newAggregatePersistentKind, Id: bareID})
		return err
	})
	if !errors.Is(err, ErrPersistentCreateOutsideTransaction) || manager.Get(bareID) != nil {
		t.Fatalf("CreateInScope of a persistent entity on a scope without a transaction: err=%v published=%v", err, manager.Get(bareID) != nil)
	}

	unregister()
	afterID := mustBuildTestEntityID(t, 64, testEntityCategory, newAggregatePersistentKind)
	if _, err := manager.Create(&EntityCreateParam{IsCreate: true, Kind: newAggregatePersistentKind, Id: afterID}); err != nil {
		t.Fatal(err)
	}
	if len(admitted) != 1 || manager.Get(afterID) == nil {
		t.Fatalf("create without an admitter admitted=%v, want it published directly (no DataEngine, nothing to store to)", admitted)
	}
}

// acceptingCapturer 模拟持久事务：接受新实体，不撤销。
type acceptingCapturer struct{}

func (acceptingCapturer) CaptureCreatedEntity(IThreadSafeEntity, func()) error { return nil }
func (acceptingCapturer) CreatedEntityLockBusy(id int64) error {
	return fmt.Errorf("entity %d is busy", id)
}
