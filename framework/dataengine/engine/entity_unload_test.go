package engine

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	coredata "github.com/tjbdwanghaibo/roost-core/framework/dataengine"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	corenest "github.com/tjbdwanghaibo/roost-core/framework/nest"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/entitysync"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/frame"
	"github.com/tjbdwanghaibo/roost-core/infra/base/fctx"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/admin"
	"github.com/tjbdwanghaibo/roost-core/infra/storage/mongo/mongotest"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// entity.unload（D16）：Runtime.UnloadEntity 的契约，经正式 Nest（业务池、锁内 WAL 准入、undo 回滚）、真实文件 WAL、
// 共享冷加载与 Mongo 替身。投影器是手动回放的（manualProjector）：记录准入之后，Flush 之前一直在途，用来控制
// “已提交、未投影”的窗口。带真实 Mongo 的同一流程见生成工程用例 TestGeneratedDataEngineAdminUnload。

// unloadTestKind / unloadRemoteTestKind 与同包其他用例的 kind 互不相同（已用 71～73、235、236、239～248）：
// kind 注册表是进程级的。
const (
	unloadTestKind       entity.EntityKind = 230
	unloadRemoteTestKind entity.EntityKind = 231
	// unloadResidentKind 与 unloadTestKind 同一个 DAO 与实体类型，lifetime=resident。
	unloadResidentKind   entity.EntityKind = 224
	unloadTestCollection                   = "unload_heroes"
)

type unloadHeroDAO struct {
	id      int64
	level   int32
	version uint64
}

func (dao *unloadHeroDAO) Id() int64         { return dao.id }
func (dao *unloadHeroDAO) SetId(id int64)    { dao.id = id }
func (*unloadHeroDAO) DbName() string        { return testDatabase }
func (*unloadHeroDAO) CollName() string      { return unloadTestCollection }
func (*unloadHeroDAO) Dirty() entity.IDirty  { return nil }
func (*unloadHeroDAO) CleanDirty()           {}
func (*unloadHeroDAO) SchemaVersion() uint32 { return 1 }
func (dao *unloadHeroDAO) RestorePersisted(raw []byte, _ uint32, version uint64) error {
	var doc struct {
		Level int32 `bson:"level"`
	}
	if err := bson.Unmarshal(raw, &doc); err != nil {
		return err
	}
	dao.level, dao.version = doc.Level, version
	return nil
}

// unloadHero 的 level / version 只在持实体锁时读写（handler 在业务池持锁执行，测试经 heroState 持锁读取）。
type unloadHero struct {
	*entity.EntityBase
	level   int32
	version uint64
	persist bool
}

func (hero *unloadHero) Base() *entity.EntityBase { return hero.EntityBase }
func (hero *unloadHero) AutoPersist() bool        { return hero.persist }

type unloadRemoteHero struct{ *entity.RemoteEntityBase }

func (hero *unloadRemoteHero) Base() *entity.EntityBase { return &hero.RemoteEntityBase.EntityBase }

var registerUnloadKinds sync.Once

func ensureUnloadKinds() {
	registerUnloadKinds.Do(func() {
		entity.RegisterEntityBuilder(&entity.EntityBuilderParam{
			Category: 1, Kind: unloadTestKind,
			DaoBuilders: []entity.DaoBuilderFunc{func() entity.DaoInterface { return &unloadHeroDAO{} }},
			Builder: func(param *entity.EntityCreateParam) (entity.IThreadSafeEntity, error) {
				hero := &unloadHero{EntityBase: entity.NewEntityBaseWithMutex(param.Id, param.Category, false, param.Mutex, param.Kind), persist: true}
				if dao, ok := param.Dao[unloadTestCollection].(*unloadHeroDAO); ok {
					hero.level, hero.version = dao.level, dao.version
				}
				return hero, nil
			},
		})
		entity.RegisterEntityBuilder(&entity.EntityBuilderParam{
			Category: 1, Kind: unloadResidentKind, Lifetime: entity.EntityLifetimeResident,
			DaoBuilders: []entity.DaoBuilderFunc{func() entity.DaoInterface { return &unloadHeroDAO{} }},
			Builder: func(param *entity.EntityCreateParam) (entity.IThreadSafeEntity, error) {
				hero := &unloadHero{EntityBase: entity.NewEntityBaseWithMutex(param.Id, param.Category, false, param.Mutex, param.Kind), persist: true}
				if dao, ok := param.Dao[unloadTestCollection].(*unloadHeroDAO); ok {
					hero.level, hero.version = dao.level, dao.version
				}
				return hero, nil
			},
		})
		entity.RegisterEntityBuilder(&entity.EntityBuilderParam{
			Category: 1, Kind: unloadRemoteTestKind, RemotePolicy: entity.RemotePolicyManaged, Lifetime: entity.EntityLifetimeRemoteManaged,
			Builder: func(param *entity.EntityCreateParam) (entity.IThreadSafeEntity, error) {
				return &unloadRemoteHero{RemoteEntityBase: entity.NewRemoteEntityBaseWithMutex(param.Id, param.Category, false, param.Mutex, param.Kind)}, nil
			},
		})
	})
}

type unloadFixture struct {
	t         *testing.T
	ctx       context.Context
	id        int64
	docs      *mongotest.Collection
	projector *Projector
	manager   *entity.EntityManager
	access    *entity.ManagerAccess
	runtime   *Runtime
	scheduler *corenest.NestMgr
	// hold 非 nil 时 level_up 在修改之前通知 entered 并等 hold 关闭：让一笔事务在业务池持有实体。
	holdMu  sync.Mutex
	hold    chan struct{}
	entered chan struct{}
}

// newUnloadFixture：库里 level=1、_version=4；实体已经冷加载进内存（经正式共享加载与 EntityRepository）。
func newUnloadFixture(t *testing.T, uniqueID int64) *unloadFixture {
	t.Helper()
	return newUnloadFixtureOfKind(t, uniqueID, unloadTestKind)
}

// newUnloadFixtureOfKind 同 newUnloadFixture，实体的 kind 是 kind（unloadTestKind 或 unloadResidentKind）。
func newUnloadFixtureOfKind(t *testing.T, uniqueID int64, kind entity.EntityKind) *unloadFixture {
	t.Helper()
	ensureUnloadKinds()
	id, err := entity.BuildEntityID(uniqueID, kind)
	if err != nil {
		t.Fatal(err)
	}
	store, client, _ := newMongoStoreTest(t)
	f := &unloadFixture{t: t, ctx: context.Background(), id: id, docs: client.Collection(testDatabase, unloadTestCollection), manager: entity.NewEntityManager()}
	if err := f.docs.Seed(bson.M{"_id": id, "_version": uint64(4), "_schema": uint32(1), "level": int32(1)}); err != nil {
		t.Fatal(err)
	}
	f.projector, _ = manualProjector(t, store)
	f.access = entity.NewManagerAccess(f.manager)
	repository, err := NewEntityRepository(f.manager, store, projectionGate{f.projector})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.access.ConfigureLoader(repository); err != nil {
		t.Fatal(err)
	}
	f.runtime = &Runtime{Store: store, Projector: f.projector, Repository: repository, access: f.access}
	f.runtime.ready.Store(true)
	f.scheduler = corenest.NewEngine(corenest.NestOptionWithTransactionCommitter(f.projector), corenest.NestOptionWithGetter(f.access),
		corenest.NestOptionWithWorkerPools(corenest.WorkerPoolConfig{Workers: 4, QueueCap: 64}, corenest.WorkerPoolConfig{}, corenest.WorkerPoolConfig{}))
	f.scheduler.MustRegisterHandlerWithMeta(corenest.NewHandlerName("unload_level_up"), f.levelUp, corenest.HandlerMeta{Rollback: corenest.RollbackUndo, Durability: corenest.DurabilityStrict})
	if err := f.scheduler.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.scheduler.Shutdown(context.Background()) })
	if _, err := f.access.Get(f.ctx, id, entity.EntityCategoryNone); err != nil || f.manager.Get(id) == nil {
		t.Fatalf("initial load: %v", err)
	}
	return f
}

// levelUp 是一笔普通的 Nest 写事务：level+1、版本+1，登记 undo，写一条 patch。
func (f *unloadFixture) levelUp(es []entity.IThreadSafeEntity, _ []any, _ ...corenest.HandlerOption) (any, error) {
	hero := es[0].(*unloadHero)
	f.holdMu.Lock()
	hold, entered := f.hold, f.entered
	f.holdMu.Unlock()
	if hold != nil {
		close(entered)
		<-hold
	}
	tx := corenest.CurrentRollbackTx()
	level, version := hero.level, hero.version
	if err := tx.RecordUndo(hero, 1, func() error { hero.level, hero.version = level, version; return nil }); err != nil {
		return nil, err
	}
	hero.level, hero.version = level+1, version+1
	mutation := coredata.Mutation{
		Key:  coredata.DocumentKey{Database: testDatabase, Resource: unloadTestCollection, ID: hero.ID()},
		Kind: coredata.MutationPatch, ExpectedVersion: version, NextVersion: version + 1, Mask: 1, Schema: 1, Codec: "bson-v2",
	}
	mutation.Patch.SetBSON, _ = bson.Marshal(bson.D{{Key: "level", Value: level + 1}})
	return nil, tx.AddMutation(mutation)
}

func (f *unloadFixture) levelUpRequest() error {
	_, err := f.scheduler.Request(f.ctx, corenest.NewHandlerName("unload_level_up"), f.id, nil)
	return err
}

// holdNextTransaction 让下一笔 level_up 在业务池持有实体，直到返回的函数被调用。
func (f *unloadFixture) holdNextTransaction() (entered <-chan struct{}, release func()) {
	hold, signal := make(chan struct{}), make(chan struct{})
	f.holdMu.Lock()
	f.hold, f.entered = hold, signal
	f.holdMu.Unlock()
	var once sync.Once
	return signal, func() {
		once.Do(func() {
			f.holdMu.Lock()
			f.hold, f.entered = nil, nil
			f.holdMu.Unlock()
			close(hold)
		})
	}
}

// heroState 持实体锁读取常驻实例的 level / version；不常驻时 ok=false。
func (f *unloadFixture) heroState() (level int32, version uint64, ok bool) {
	value, _ := f.manager.Get(f.id).(*unloadHero)
	if value == nil {
		return 0, 0, false
	}
	mu := value.GetMutex()
	mu.Lock()
	defer mu.Unlock()
	return value.level, value.version, true
}

func (f *unloadFixture) storedState() (int32, uint64) {
	f.t.Helper()
	doc, ok := f.docs.Lookup(f.id)
	if !ok {
		f.t.Fatal("hero document missing")
	}
	level, _ := doc["level"].(int32)
	version, _ := doc["_version"].(int64)
	return level, uint64(version)
}

func (f *unloadFixture) unloadAsync(timeout time.Duration) <-chan unloadOutcomeForTest {
	result := make(chan unloadOutcomeForTest, 1)
	go func() {
		ctx, cancel := context.WithTimeout(f.ctx, timeout)
		defer cancel()
		value, err := f.runtime.UnloadEntity(ctx, f.id)
		result <- unloadOutcomeForTest{value, err}
	}()
	return result
}

type unloadOutcomeForTest struct {
	result EntityUnloadResult
	err    error
}

func (f *unloadFixture) waitBarrier(want uint64) {
	f.t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for f.projector.Stats().UnloadingEntities != want {
		if time.Now().After(deadline) {
			f.t.Fatalf("unloading entities = %d, want %d", f.projector.Stats().UnloadingEntities, want)
		}
		time.Sleep(time.Millisecond)
	}
}

func receiveUnload(t *testing.T, outcome <-chan unloadOutcomeForTest) unloadOutcomeForTest {
	t.Helper()
	select {
	case got := <-outcome:
		return got
	case <-time.After(5 * time.Second):
		t.Fatal("UnloadEntity did not return")
		return unloadOutcomeForTest{}
	}
}

// 已提交、未投影的修改先落库再卸载；卸载期间的写入以可重试的 ErrEntityUnloading 拒绝并回滚；卸载后正常冷加载，
// 读到的就是卸载前内存里的状态，之后照常写。
func TestEntityUnloadDrainsProjectionsRefusesWritesThenColdLoadsTheSameState(t *testing.T) {
	f := newUnloadFixture(t, 9101)
	if err := f.levelUpRequest(); err != nil {
		t.Fatal(err)
	}
	if level, version, _ := f.heroState(); level != 2 || version != 5 {
		t.Fatalf("memory after the first write: level=%d version=%d", level, version)
	}
	if level, _ := f.storedState(); level != 1 {
		t.Fatalf("premise: the write must still be unprojected, stored level=%d", level)
	}

	outcome := f.unloadAsync(5 * time.Second)
	f.waitBarrier(1)
	// 屏障之后的写入：在 WAL 准入处拒绝，Nest 在 Guard 内回滚，内存不变。
	err := f.levelUpRequest()
	if !errors.Is(err, coredata.ErrEntityUnloading) || !errors.Is(err, corenest.ErrCommitRejected) {
		t.Fatalf("write during unload = %v, want ErrEntityUnloading and ErrCommitRejected", err)
	}
	if level, version, ok := f.heroState(); !ok || level != 2 || version != 5 {
		t.Fatalf("the refused write was not rolled back: level=%d version=%d resident=%v", level, version, ok)
	}
	select {
	case got := <-outcome:
		t.Fatalf("UnloadEntity returned before the admitted write was projected: %+v", got)
	case <-time.After(50 * time.Millisecond):
	}
	if err := f.projector.Flush(f.ctx); err != nil {
		t.Fatal(err)
	}
	got := receiveUnload(t, outcome)
	if got.err != nil || !got.result.Resident || !got.result.Unloaded || got.result.Drained != 1 {
		t.Fatalf("UnloadEntity = %+v, %v; want resident, unloaded, drained=1", got.result, got.err)
	}
	if f.manager.Get(f.id) != nil {
		t.Fatal("the entity is still resident")
	}
	if level, version := f.storedState(); level != 2 || version != 5 {
		t.Fatalf("stored after unload: level=%d version=%d, want 2/5", level, version)
	}
	if stats := f.projector.Stats(); stats.UnloadingEntities != 0 || stats.UnloadAdmissionRejected != 1 {
		t.Fatalf("barrier stats after unload = %+v", stats)
	}

	// 下一次访问正常冷加载：内存与卸载前相同，写入照常。
	if err := f.levelUpRequest(); err != nil {
		t.Fatalf("write after unload: %v", err)
	}
	if level, version, ok := f.heroState(); !ok || level != 3 || version != 6 {
		t.Fatalf("after cold load and write: level=%d version=%d resident=%v", level, version, ok)
	}
	if err := f.projector.Flush(f.ctx); err != nil {
		t.Fatal(err)
	}
	if level, version := f.storedState(); level != 3 || version != 6 {
		t.Fatalf("stored after the next write: level=%d version=%d", level, version)
	}
}

// 投影在 timeout 内没完成：明确的 ErrEntityUnloadTimeout，实体保持常驻、屏障撤销，之后的写入照常准入；投影恢复后
// 库与内存一致。
func TestEntityUnloadTimesOutWithoutTouchingTheEntity(t *testing.T) {
	f := newUnloadFixture(t, 9102)
	if err := f.levelUpRequest(); err != nil {
		t.Fatal(err)
	}
	got := receiveUnload(t, f.unloadAsync(100*time.Millisecond))
	if !errors.Is(got.err, ErrEntityUnloadTimeout) || errors.Is(got.err, context.DeadlineExceeded) || got.result.Unloaded || got.result.Drained != 1 {
		t.Fatalf("UnloadEntity with an unprojected write = %+v, %v; want ErrEntityUnloadTimeout (a known outcome, not a ctx error)", got.result, got.err)
	}
	if _, _, ok := f.heroState(); !ok {
		t.Fatal("a timed-out unload removed the entity")
	}
	if f.projector.Stats().UnloadingEntities != 0 {
		t.Fatal("the barrier outlived the failed unload")
	}
	if err := f.levelUpRequest(); err != nil {
		t.Fatalf("write after the failed unload: %v", err)
	}
	if err := f.projector.Flush(f.ctx); err != nil {
		t.Fatal(err)
	}
	if level, version := f.storedState(); level != 3 || version != 6 {
		t.Fatalf("stored = %d/%d, want 3/6", level, version)
	}
	if level, version, _ := f.heroState(); level != 3 || version != 6 {
		t.Fatalf("memory = %d/%d, want 3/6", level, version)
	}
}

// 在途事务：一笔事务在业务池持有实体超过 timeout，卸载以 ErrEntityUnloadBusy 失败、没有登记屏障，那笔事务照常提交；
// 持有时间在 timeout 之内时，卸载等它结束（它不被拒绝），再排空它的投影后卸载。
func TestEntityUnloadWaitsForOrRefusesATransactionHoldingTheEntity(t *testing.T) {
	t.Run("held_past_timeout", func(t *testing.T) {
		f := newUnloadFixture(t, 9103)
		entered, release := f.holdNextTransaction()
		defer release()
		written := make(chan error, 1)
		go func() { written <- f.levelUpRequest() }()
		<-entered
		got := receiveUnload(t, f.unloadAsync(100*time.Millisecond))
		if !errors.Is(got.err, ErrEntityUnloadBusy) || got.result.Unloaded {
			t.Fatalf("UnloadEntity while a transaction holds the entity = %+v, %v; want ErrEntityUnloadBusy", got.result, got.err)
		}
		release()
		if err := <-written; err != nil {
			t.Fatalf("the in-flight transaction was affected by the failed unload: %v", err)
		}
		if level, version, ok := f.heroState(); !ok || level != 2 || version != 5 {
			t.Fatalf("after the in-flight transaction: level=%d version=%d resident=%v", level, version, ok)
		}
		if stats := f.projector.Stats(); stats.UnloadingEntities != 0 || stats.UnloadAdmissionRejected != 0 {
			t.Fatalf("barrier stats = %+v, want none raised", stats)
		}
	})
	t.Run("finishes_within_timeout", func(t *testing.T) {
		f := newUnloadFixture(t, 9104)
		entered, release := f.holdNextTransaction()
		written := make(chan error, 1)
		go func() { written <- f.levelUpRequest() }()
		<-entered
		outcome := f.unloadAsync(5 * time.Second)
		time.Sleep(20 * time.Millisecond) // 卸载已在业务池等实体锁
		release()
		if err := <-written; err != nil {
			t.Fatalf("the transaction that held the entity when the unload began was refused: %v", err)
		}
		f.waitBarrier(1)
		if err := f.projector.Flush(f.ctx); err != nil {
			t.Fatal(err)
		}
		got := receiveUnload(t, outcome)
		if got.err != nil || !got.result.Unloaded || got.result.Drained != 1 {
			t.Fatalf("UnloadEntity = %+v, %v; want unloaded with the held transaction drained", got.result, got.err)
		}
		if level, version := f.storedState(); level != 2 || version != 5 {
			t.Fatalf("stored = %d/%d, want 2/5", level, version)
		}
	})
}

// 没有常驻：什么也不做。非持久实体：拒绝（卸载就是丢弃）。同一实体同时只能有一次卸载。在业务 worker 上调用：fail-fast。
func TestEntityUnloadRefusals(t *testing.T) {
	f := newUnloadFixture(t, 9105)
	other, err := entity.BuildEntityID(9199, unloadTestKind)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := f.runtime.UnloadEntity(f.ctx, other); err != nil || got.Resident || got.Unloaded {
		t.Fatalf("unload of an entity that is not loaded = %+v, %v", got, err)
	}

	f.manager.Get(f.id).(*unloadHero).persist = false
	if _, err := f.runtime.UnloadEntity(f.ctx, f.id); !errors.Is(err, ErrEntityUnloadNotPersistent) || f.manager.Get(f.id) == nil {
		t.Fatalf("unload of a non-persistent entity = %v (resident=%v), want ErrEntityUnloadNotPersistent", err, f.manager.Get(f.id) != nil)
	}
	f.manager.Get(f.id).(*unloadHero).persist = true

	release, err := f.projector.beginEntityUnload(f.id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.runtime.UnloadEntity(f.ctx, f.id); !errors.Is(err, ErrEntityUnloadBusy) || f.manager.Get(f.id) == nil {
		t.Fatalf("a second concurrent unload = %v, want ErrEntityUnloadBusy", err)
	}
	release()

	var onWorker error
	if err := f.scheduler.RunLocal(f.ctx, func() { _, onWorker = f.runtime.UnloadEntity(f.ctx, f.id) }); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(onWorker, fctx.ErrBlockingInBusinessWorker) || f.manager.Get(f.id) == nil {
		t.Fatalf("UnloadEntity on a business worker = %v, want ErrBlockingInBusinessWorker", onWorker)
	}
	if got, err := f.runtime.UnloadEntity(f.ctx, f.id); err != nil || !got.Unloaded {
		t.Fatalf("unload after the refusals = %+v, %v", got, err)
	}
}

// Remote 托管实体：所有权稳定（local_owned）时与本地实体同一流程卸载；处于隔离 / 恢复 / 迁移中时由 Remote 运行时
// 负责收尾，按忙拒绝、不动实体。屏障同样挡住写到它的 Remote 记录（在 WAL 之前）。
func TestEntityUnloadRemoteManagedEntity(t *testing.T) {
	f := newUnloadFixture(t, 9106)
	remoteID, err := entity.BuildEntityID(9106, unloadRemoteTestKind)
	if err != nil {
		t.Fatal(err)
	}
	add := func() *unloadRemoteHero {
		hero := &unloadRemoteHero{RemoteEntityBase: entity.NewRemoteEntityBase(remoteID, 1, false, unloadRemoteTestKind)}
		f.manager.Add(hero)
		return hero
	}
	quarantined := add()
	for _, state := range []entity.RemoteOwnershipState{entity.RemoteOwnershipRecovering, entity.RemoteOwnershipQuarantined} {
		if err := quarantined.TransitionRemoteOwnership(state); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.runtime.UnloadEntity(f.ctx, remoteID); !errors.Is(err, ErrEntityUnloadBusy) || f.manager.Get(remoteID) == nil {
		t.Fatalf("unload of a quarantined remote entity = %v, want ErrEntityUnloadBusy with the entity kept", err)
	}
	if err := quarantined.TransitionRemoteOwnership(entity.RemoteOwnershipRecovering); err != nil {
		t.Fatal(err)
	}
	if err := quarantined.TransitionRemoteOwnership(entity.RemoteOwnershipLocalOwned); err != nil {
		t.Fatal(err)
	}

	release, err := f.projector.beginEntityUnload(remoteID)
	if err != nil {
		t.Fatal(err)
	}
	record := testMutationRecord(coredata.MutationPatch)
	record.Mutations[0].Key = coredata.DocumentKey{Database: testDatabase, Resource: "remote_heroes", ID: remoteID}
	record.Mutations[0].Remote = &entity.RemoteCommit{}
	if err := f.projector.Commit(f.ctx, record); !errors.Is(err, coredata.ErrEntityUnloading) {
		t.Fatalf("remote write during unload = %v, want ErrEntityUnloading", err)
	}
	if stats := f.projector.Stats(); stats.Committed != 0 || stats.WALUnacked != 0 {
		t.Fatalf("the refused remote record reached the WAL: %+v", stats)
	}
	release()

	if got, err := f.runtime.UnloadEntity(f.ctx, remoteID); err != nil || !got.Unloaded || f.manager.Get(remoteID) != nil {
		t.Fatalf("unload of a local-owned remote entity = %+v, %v", got, err)
	}
}

// 运维卸载（entity.unload）后仍有订阅者：框架在业务池之外经正式 EntityRepository（等待该实体投影、发布回到业务池、
// OnEntityLoaded）从 Mongo 重载并 Rebind，订阅者收到权威全量；Mongo 里没有该实体时退回 remove。
// 原用例由“原生步骤被 lease fence 跳过后驱逐”触发卸载（RR-20260926-59 / RR-30）；lease fence 删除后（saga 设计第 5 批）
// 由运维卸载触发同一条 ManagerAccess.Unload → unload_resync 链路。订阅者先拿到一个与库不同的值（库在卸载前被另一写者改过），
// 卸载后必须收到库里的值，而不是停在旧内容上。

// unloadResyncKind 与同包其他用例的 kind 互不相同（RR-20260926-82）：kind 注册表是进程级的。
const unloadResyncKind entity.EntityKind = 248
const unloadResyncCollection = "resync_heroes"

type unloadResyncDAO struct {
	id    int64
	level int32
}

func (dao *unloadResyncDAO) Id() int64         { return dao.id }
func (dao *unloadResyncDAO) SetId(id int64)    { dao.id = id }
func (*unloadResyncDAO) DbName() string        { return testDatabase }
func (*unloadResyncDAO) CollName() string      { return unloadResyncCollection }
func (*unloadResyncDAO) Dirty() entity.IDirty  { return nil }
func (*unloadResyncDAO) CleanDirty()           {}
func (*unloadResyncDAO) SchemaVersion() uint32 { return 1 }
func (dao *unloadResyncDAO) RestorePersisted(raw []byte, _ uint32, _ uint64) error {
	var doc struct {
		Level int32 `bson:"level"`
	}
	if err := bson.Unmarshal(raw, &doc); err != nil {
		return err
	}
	dao.level = doc.Level
	return nil
}

type unloadResyncEntity struct {
	*entity.EntityBase
	mu    sync.Mutex
	level int32
}

func (value *unloadResyncEntity) Base() *entity.EntityBase { return value.EntityBase }
func (*unloadResyncEntity) AutoPersist() bool              { return true }

var registerUnloadResyncEntity sync.Once

func ensureUnloadResyncEntity() {
	registerUnloadResyncEntity.Do(func() {
		entity.RegisterEntityBuilder(&entity.EntityBuilderParam{
			Category: 1, Kind: unloadResyncKind,
			DaoBuilders: []entity.DaoBuilderFunc{func() entity.DaoInterface { return &unloadResyncDAO{} }},
			Sync: entity.EntitySyncBuilderParam{Enabled: true, Namespace: "hero", PackerFactory: func(e entity.IThreadSafeEntity) entity.SubjectSyncPacker {
				value := e.(*unloadResyncEntity)
				pack := func(entity.SyncProfile) (entity.FrozenSyncPayload, error) {
					value.mu.Lock()
					defer value.mu.Unlock()
					return entity.CopyFrozenSyncPayload(1, []byte{byte(value.level)}), nil
				}
				return entity.SubjectSyncPackFunc{Snapshot: pack, Delta: func(p entity.SyncProfile, _ uint64) (entity.FrozenSyncPayload, error) { return pack(p) }}
			}},
			Builder: func(param *entity.EntityCreateParam) (entity.IThreadSafeEntity, error) {
				value := &unloadResyncEntity{EntityBase: entity.NewEntityBaseWithMutex(param.Id, param.Category, false, param.Mutex, param.Kind)}
				if dao, ok := param.Dao[unloadResyncCollection].(*unloadResyncDAO); ok {
					value.level = dao.level
				}
				return value, nil
			},
		})
	})
}

// projectionGate 是 Runtime 作 RecoveryGate 时的最小等价物：已就绪，冷加载等该实体的在途投影。
type projectionGate struct{ projector *Projector }

func (projectionGate) Ready() bool { return true }
func (gate projectionGate) WaitEntityProjection(ctx context.Context, id int64) error {
	return gate.projector.WaitEntityProjection(ctx, id)
}

type unloadResyncFixture struct {
	t        *testing.T
	id       int64
	store    *MongoStore
	docs     *mongotest.Collection
	project  *Projector
	manager  *entity.EntityManager
	access   *entity.ManagerAccess
	sync     *entitysync.Manager
	frames   chan []byte
	decoder  *entitysync.Decoder // 订阅者的接收端：增量从基线继承身份，要按顺序解
	ref      frame.ObjectRef
	resident *unloadResyncEntity
	runtime  *Runtime
}

func newUnloadResyncFixture(t *testing.T, uniqueID int64) *unloadResyncFixture {
	t.Helper()
	ensureUnloadResyncEntity()
	id, err := entity.BuildEntityID(uniqueID, unloadResyncKind)
	if err != nil {
		t.Fatal(err)
	}
	store, client, _ := newMongoStoreTest(t)
	docs := client.Collection(testDatabase, unloadResyncCollection)
	if err = docs.Seed(bson.M{"_id": id, "_version": uint64(4), "_schema": uint32(1), "level": int32(1)}); err != nil {
		t.Fatal(err)
	}
	projector, _ := manualProjector(t, store)
	f := &unloadResyncFixture{t: t, id: id, store: store, docs: docs, project: projector, manager: entity.NewEntityManager(), frames: make(chan []byte, 16)}
	f.access = entity.NewManagerAccess(f.manager)
	repository, err := NewEntityRepository(f.manager, store, projectionGate{projector})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.access.ConfigureLoader(repository); err != nil {
		t.Fatal(err)
	}
	f.runtime = &Runtime{Store: store, Projector: projector, Repository: repository, access: f.access}
	f.runtime.ready.Store(true)
	f.sync, err = entitysync.NewManager(entitysync.ManagerConfig{Mode: entitysync.ModePeriodic, Interval: time.Hour, Transport: entitysync.TransportFunc(func(_ context.Context, _ entitysync.SessionID, p []byte) error {
		f.frames <- append([]byte(nil), p...)
		return nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.sync.Close(context.Background()) })
	// 与 kit Nest Mod 相同的接线：重载发布时 Rebind（OnEntityLoaded）；NewEngine 把 NestMgr.RunLocal 绑定给作为
	// Getter 的 ManagerAccess（BindLocalExecutor），卸载后重载与业务冷加载同一条共享加载，经它回业务池发布。
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
	f.resident = loaded.(*unloadResyncEntity)
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

func (f *unloadResyncFixture) next(what string) entitysync.ReceivedFrame {
	f.t.Helper()
	if f.decoder == nil {
		f.decoder = entitysync.NewDecoder(frame.DefaultLimits())
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		select {
		case raw := <-f.frames:
			decoded, err := f.decoder.Decode(raw)
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

func (f *unloadResyncFixture) level(object entitysync.ReceivedObject) int32 {
	f.t.Helper()
	payload := object.Components[0].Update.Payload.BytesCopy()
	if len(payload) != 1 {
		f.t.Fatalf("payload=%v", payload)
	}
	return int32(payload[0])
}

// unloadWithSubscriber 让库先被另一写者改到 level 3（本进程内存与订阅者仍是 1），再运维卸载该实体。
func (f *unloadResyncFixture) unloadWithSubscriber(authorityLevel int32) {
	f.t.Helper()
	if authorityLevel != 0 {
		if _, err := f.docs.UpdateOne(context.Background(), bson.M{"_id": f.id}, bson.M{"$set": bson.M{"level": authorityLevel, "_version": uint64(5)}}); err != nil {
			f.t.Fatal(err)
		}
	}
	result, err := f.runtime.UnloadEntity(context.Background(), f.id)
	if err != nil || !result.Resident || !result.Unloaded {
		f.t.Fatalf("UnloadEntity=%+v err=%v", result, err)
	}
	if f.manager.Get(f.id) == entity.IThreadSafeEntity(f.resident) {
		f.t.Fatal("premise: the unloaded instance is still resident")
	}
}

func TestEntityUnloadResyncsSubscribersFromMongo(t *testing.T) {
	f := newUnloadResyncFixture(t, 2431)
	f.unloadWithSubscriber(3)
	correction := f.next("the entity was unloaded, but the subscriber still holds level 1 (no authoritative full, no remove)")
	if len(correction.Objects) != 1 || correction.Objects[0].Operation != frame.ObjectUpdate || correction.Objects[0].Ref != f.ref {
		t.Fatalf("correction frame=%+v, want one ObjectUpdate of the held object %+v", correction.Objects, f.ref)
	}
	if got := f.level(correction.Objects[0]); got != 3 {
		t.Fatalf("subscriber got level %d after the unload, want the Mongo level 3", got)
	}
	reloaded, ok := f.manager.Get(f.id).(*unloadResyncEntity)
	if !ok || reloaded == f.resident {
		t.Fatal("the Mongo instance is not resident after the resync")
	}
	if doc, _ := f.docs.Lookup(f.id); doc["_version"] != int64(5) {
		t.Fatalf("resync persisted something: %v", doc)
	}
}

func TestEntityUnloadWithoutAuthorityRetractsSubscribers(t *testing.T) {
	f := newUnloadResyncFixture(t, 2432)
	if _, err := f.docs.DeleteOne(context.Background(), bson.M{"_id": f.id}); err != nil { // 权威中已不存在（例如另一写者删除了它）
		t.Fatal(err)
	}
	f.unloadWithSubscriber(0)
	correction := f.next("the entity was unloaded and Mongo has no such entity, but the subscriber got no remove")
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

// lifetime=resident 的实体不被运维卸载：entity.unload 拒绝（entity.ErrEntityResident），实体与屏障都不受影响、照常可写。
// force=true 是强制刷新：卸载后立刻从库重新载入，实体不离开内存（新实例、状态就是库里的）。
func TestResidentEntityUnloadIsRefusedAndForceRefreshesIt(t *testing.T) {
	f := newUnloadFixtureOfKind(t, 9131, unloadResidentKind)
	if err := f.levelUpRequest(); err != nil {
		t.Fatal(err)
	}
	if err := f.projector.Flush(f.ctx); err != nil {
		t.Fatal(err)
	}
	before := f.manager.Get(f.id)
	got := receiveUnload(t, f.unloadAsync(time.Second))
	if !errors.Is(got.err, entity.ErrEntityResident) || f.manager.Get(f.id) != before {
		t.Fatalf("UnloadEntity on a lifetime=resident entity = %+v, %v; still the same instance: %v; want entity.ErrEntityResident and the entity untouched",
			got.result, got.err, f.manager.Get(f.id) == before)
	}
	if stats := f.projector.Stats(); stats.UnloadingEntities != 0 {
		t.Fatalf("a refused unload left its barrier: %+v", stats)
	}
	if err := f.levelUpRequest(); err != nil {
		t.Fatalf("write after the refused unload: %v", err)
	}
	if err := f.projector.Flush(f.ctx); err != nil {
		t.Fatal(err)
	}

	reg := admin.NewRegistry()
	if err := RegisterAdminCommands(reg, func() *Runtime { return f.runtime }); err != nil {
		t.Fatal(err)
	}
	payload := func(force bool) []byte {
		raw, _ := json.Marshal(map[string]any{"entity_id": f.id, "force": force})
		return raw
	}
	if _, err := reg.Execute(f.ctx, admin.Command{Name: AdminCommandEntityUnload, Payload: payload(false)}); !errors.Is(err, entity.ErrEntityResident) {
		t.Fatalf("entity.unload without force on a resident entity = %v, want entity.ErrEntityResident", err)
	}
	result, err := reg.Execute(f.ctx, admin.Command{Name: AdminCommandEntityUnload, Payload: payload(true)})
	if err != nil {
		t.Fatalf("entity.unload force=true = %v", err)
	}
	after := f.manager.Get(f.id)
	if after == nil || after == before || result.Data["reloaded"] != true {
		t.Fatalf("entity.unload force=true: loaded=%v new instance=%v data=%v; want the entity reloaded from the store", after != nil, after != before, result.Data)
	}
	if level, version, ok := f.heroState(); !ok || level != 3 || version != 6 {
		t.Fatalf("refreshed instance: level=%d version=%d loaded=%v, want the stored 3/6", level, version, ok)
	}
	if err := f.levelUpRequest(); err != nil {
		t.Fatalf("write after the refresh: %v", err)
	}
	// 重新载入的实例同样是常驻的：再卸载仍被拒绝。
	if got := receiveUnload(t, f.unloadAsync(time.Second)); !errors.Is(got.err, entity.ErrEntityResident) || f.manager.Get(f.id) != after {
		t.Fatalf("UnloadEntity on the refreshed instance = %v; want entity.ErrEntityResident and the instance kept", got.err)
	}
}
