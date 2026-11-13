package engine

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	coredata "github.com/tjbdwanghaibo/roost-core/framework/dataengine"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/infra/storage/mongo/mongotest"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// 本地实体起服预加载（Runtime.PreloadLocalEntities）与换服搬档（CopyHandedOffEntity / DropEntityCopy）：
// DAO 是 dbscope=sid，两个 sid 共用一个 Mongo 替身，各自的库是 game_<sid>。

// preload 用的 kind 与同包其他用例互不相同（kind 注册表是进程级的）。
const (
	preloadFirstKind  entity.EntityKind = 220 // preload_order=1
	preloadSecondKind entity.EntityKind = 221 // preload_order=2
	preloadGlobalKind entity.EntityKind = 222 // DAO 是 dbscope=global：不能做本地预加载
	preloadRemoteKind entity.EntityKind = 223 // remote=managed：不能做本地预加载
)

// preloadDAO 是 dbscope=sid 的 DAO（global=true 时是 dbscope=global）。
type preloadDAO struct {
	id         int64
	collection string
	global     bool
}

func (dao *preloadDAO) Id() int64         { return dao.id }
func (dao *preloadDAO) SetId(id int64)    { dao.id = id }
func (*preloadDAO) DbName() string        { return testDatabase }
func (dao *preloadDAO) CollName() string  { return dao.collection }
func (*preloadDAO) Dirty() entity.IDirty  { return nil }
func (*preloadDAO) CleanDirty()           {}
func (*preloadDAO) SchemaVersion() uint32 { return 1 }
func (dao *preloadDAO) DbScope() coredata.DatabaseScope {
	if dao.global {
		return coredata.DatabaseGlobal
	}
	return coredata.DatabaseServer
}
func (dao *preloadDAO) RestorePersisted(raw []byte, _ uint32, _ uint64) error {
	var doc struct {
		ID int64 `bson:"_id"`
	}
	if err := bson.Unmarshal(raw, &doc); err != nil {
		return err
	}
	if doc.ID != dao.id {
		return fmt.Errorf("preload DAO bound to %d got %d", dao.id, doc.ID)
	}
	return nil
}

type preloadEntity struct{ *entity.EntityBase }

func (value *preloadEntity) Base() *entity.EntityBase { return value.EntityBase }

var registerPreloadKinds sync.Once

func ensurePreloadKinds() {
	registerPreloadKinds.Do(func() {
		register := func(kind entity.EntityKind, order int, collection string, global bool, policy entity.RemotePolicy) {
			lifetime := entity.EntityLifetimeDefault
			if policy == entity.RemotePolicyManaged {
				lifetime = entity.EntityLifetimeRemoteManaged
			}
			entity.RegisterEntityBuilder(&entity.EntityBuilderParam{
				Category: 1, Kind: kind, LoadPriority: order, RemotePolicy: policy, Lifetime: lifetime,
				DaoBuilders: []entity.DaoBuilderFunc{func() entity.DaoInterface { return &preloadDAO{collection: collection, global: global} }},
				Builder: func(param *entity.EntityCreateParam) (entity.IThreadSafeEntity, error) {
					return &preloadEntity{EntityBase: entity.NewEntityBaseWithMutex(param.Id, param.Category, false, param.Mutex, param.Kind)}, nil
				},
			})
		}
		register(preloadFirstKind, 1, "preload_first", false, entity.RemotePolicyNone)
		register(preloadSecondKind, 2, "preload_second", false, entity.RemotePolicyNone)
		register(preloadGlobalKind, 0, "preload_global", true, entity.RemotePolicyNone)
		register(preloadRemoteKind, 0, "preload_remote", false, entity.RemotePolicyManaged)
	})
}

// preloadServer 是共用 Mongo 替身的一个服：自己的 MongoStore（ServerID=sid）、EntityManager、共享加载与仓库。
type preloadServer struct {
	sid     int32
	store   *MongoStore
	manager *entity.EntityManager
	access  *entity.ManagerAccess
	runtime *Runtime
	loaded  *loadOrder
}

// loadOrder 记下实体发布进内存的先后（OnEntityLoaded）。slow 里的 kind 发布后先停一下再记，用来让“同一批”与“下一批”
// 在时间上分得开：分批正确时慢的那一批全部记完下一批才开始；分批错了，下一批的实体会插到慢的中间。
type loadOrder struct {
	mu    sync.Mutex
	kinds []entity.EntityKind
	slow  map[entity.EntityKind]time.Duration
}

func (order *loadOrder) record(value entity.IThreadSafeEntity) {
	kind := value.GetEntityKind()
	if pause := order.slow[kind]; pause > 0 {
		time.Sleep(pause)
	}
	order.mu.Lock()
	order.kinds = append(order.kinds, kind)
	order.mu.Unlock()
}

func (order *loadOrder) snapshot() []entity.EntityKind {
	order.mu.Lock()
	defer order.mu.Unlock()
	return slices.Clone(order.kinds)
}

func newPreloadServer(t *testing.T, client *mongotest.Client, sid int32) *preloadServer {
	t.Helper()
	ensurePreloadKinds()
	store, err := NewMongoStore(client, MongoStoreConfig{Databases: identityDatabases(t), DefaultDatabase: testDatabase, ServerID: sid, TransactionReceiptTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	server := &preloadServer{sid: sid, store: store, manager: entity.NewEntityManager(), loaded: &loadOrder{slow: map[entity.EntityKind]time.Duration{}}}
	server.access = entity.NewManagerAccess(server.manager)
	repository, err := NewEntityRepository(server.manager, store, nil)
	if err != nil {
		t.Fatal(err)
	}
	repository.OnEntityLoaded(server.loaded.record)
	if _, err := server.access.ConfigureLoader(repository); err != nil {
		t.Fatal(err)
	}
	server.runtime = &Runtime{Store: store, Repository: repository, access: server.access}
	server.runtime.ready.Store(true)
	return server
}

// seed 在 sid 的库里放 count 个 kind 的实体文档，返回它们的完整 ID。unique 从 base 开始。
func seedPreload(t *testing.T, client *mongotest.Client, sid int32, kind entity.EntityKind, collection string, base int64, count int, extra bson.M) []int64 {
	t.Helper()
	coll := client.Collection(fmt.Sprintf("%s_%d", testDatabase, sid), collection)
	ids := make([]int64, 0, count)
	for i := 0; i < count; i++ {
		id, err := entity.BuildEntityID(base+int64(i), kind)
		if err != nil {
			t.Fatal(err)
		}
		doc := bson.M{"_id": id, "_version": uint64(3), "_schema": uint32(1), "_last_tx": fmt.Sprintf("tx-%d", id)}
		for key, value := range extra {
			doc[key] = value
		}
		if err := coll.Seed(doc); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	return ids
}

// preload_order：越小越先，前一批全部完成才开始下一批。第一批的实体每个都慢一点，分批错了第二批就会插进来。
func TestPreloadLocalEntitiesRunsBatchesInLoadPriorityOrder(t *testing.T) {
	client := mongotest.NewClient()
	server := newPreloadServer(t, client, 3)
	first := seedPreload(t, client, 3, preloadFirstKind, "preload_first", 100, 40, nil)
	second := seedPreload(t, client, 3, preloadSecondKind, "preload_second", 100, 40, nil)
	server.loaded.slow[preloadFirstKind] = 5 * time.Millisecond

	// 调用方给出的顺序故意与批次相反。
	report, err := server.runtime.PreloadLocalEntities(context.Background(), []entity.EntityKind{preloadSecondKind, preloadFirstKind}, LocalPreloadLimits{})
	if err != nil {
		t.Fatal(err)
	}
	if report.Listed != 80 || report.Loaded != 80 || report.Failed != 0 {
		t.Fatalf("report = %+v, want 80 listed and loaded", report)
	}
	if len(report.Kinds) != 2 || report.Kinds[0].Kind != preloadFirstKind || report.Kinds[1].Kind != preloadSecondKind {
		t.Fatalf("kinds ran in order %+v, want first (order 1) then second (order 2)", report.Kinds)
	}
	order := server.loaded.snapshot()
	lastFirst := -1
	firstSecond := len(order)
	for i, kind := range order {
		if kind == preloadFirstKind {
			lastFirst = i
		} else if kind == preloadSecondKind && i < firstSecond {
			firstSecond = i
		}
	}
	if lastFirst > firstSecond {
		t.Fatalf("an entity of the second batch was loaded at %d before the first batch finished (its last at %d): %v", firstSecond, lastFirst, order)
	}
	for _, id := range append(first, second...) {
		if server.manager.Get(id) == nil {
			t.Fatalf("entity %d was not preloaded", id)
		}
	}
}

// 一个坏文档（存储 schema 与运行时不一致）不挡启动：它计入 Failed、留给按需加载，其余照常加载；删过档的计入 Deleted。
func TestPreloadLocalEntitiesSkipsABadDocument(t *testing.T) {
	client := mongotest.NewClient()
	server := newPreloadServer(t, client, 3)
	good := seedPreload(t, client, 3, preloadFirstKind, "preload_first", 200, 5, nil)
	bad := seedPreload(t, client, 3, preloadFirstKind, "preload_first", 300, 1, bson.M{"_schema": uint32(9)})
	seedPreload(t, client, 3, preloadFirstKind, "preload_first", 400, 2, bson.M{"_deleted": true})

	report, err := server.runtime.PreloadLocalEntities(context.Background(), []entity.EntityKind{preloadFirstKind}, LocalPreloadLimits{})
	if err != nil {
		t.Fatalf("a bad document failed the whole preload: %v", err)
	}
	if report.Listed != 6 || report.Loaded != 5 || report.Failed != 1 {
		t.Fatalf("report = %+v, want 6 listed (tombstones are not listed), 5 loaded, 1 failed", report)
	}
	for _, id := range good {
		if server.manager.Get(id) == nil {
			t.Fatalf("good entity %d was not preloaded", id)
		}
	}
	if server.manager.Get(bad[0]) != nil {
		t.Fatal("the bad document was published")
	}
}

// 枚举失败让启动失败：错误原样带出，已经完成的批次留在报告里。
func TestPreloadLocalEntitiesFailsWhenEnumerationFails(t *testing.T) {
	ensurePreloadKinds()
	targets, err := resolveLocalPreloadTargets([]entity.EntityKind{preloadFirstKind, preloadSecondKind})
	if err != nil {
		t.Fatal(err)
	}
	broken := errors.New("mongo is unreachable")
	lister := listerFunc(func(_ context.Context, key coredata.DocumentKey, _ int64, _ int) ([]int64, int64, error) {
		if key.Resource == "preload_second" {
			return nil, 0, broken
		}
		return []int64{1}, 0, nil
	})
	loads := 0
	report, err := preloadLocalTargets(context.Background(), lister, func(context.Context, int64) error { loads++; return nil }, targets, LocalPreloadLimits{})
	if !errors.Is(err, broken) {
		t.Fatalf("preload with a failing enumeration = %v, want the enumeration error", err)
	}
	if loads != 1 || len(report.Kinds) != 2 || report.Kinds[0].Loaded != 1 {
		t.Fatalf("report = %+v (loads %d), want the first batch done and the failure reported", report, loads)
	}
}

// 预算（KNOWN-LIMITS“预加载没有数量上限与时间预算”）：Mongo 卡住时列 ID 的查询不返回，修前起服一直等到进程被停
// （进程的基础 context 没有期限）；现在整次预加载按 Timeout 取消在途的查询与加载、返回 ErrLocalPreloadBudget。
func TestPreloadLocalEntitiesGivesUpOnAStuckStoreAtItsTimeout(t *testing.T) {
	ensurePreloadKinds()
	targets, err := resolveLocalPreloadTargets([]entity.EntityKind{preloadFirstKind, preloadSecondKind})
	if err != nil {
		t.Fatal(err)
	}
	for _, stuckAt := range []string{"enumeration", "load"} {
		lister := listerFunc(func(ctx context.Context, _ coredata.DocumentKey, after int64, _ int) ([]int64, int64, error) {
			if stuckAt == "enumeration" {
				<-ctx.Done()
				return nil, 0, ctx.Err()
			}
			return []int64{1, 2, 3}, 0, nil
		})
		load := func(ctx context.Context, _ int64) error {
			<-ctx.Done() // 冷加载卡在 Mongo 上，只在期限到期时返回
			return ctx.Err()
		}
		done := make(chan error, 1)
		go func() {
			_, err := preloadLocalTargets(context.Background(), lister, load, targets, LocalPreloadLimits{Timeout: 100 * time.Millisecond})
			done <- err
		}()
		select {
		case err := <-done:
			if !errors.Is(err, ErrLocalPreloadBudget) || !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("stuck at %s: preload = %v, want ErrLocalPreloadBudget and DeadlineExceeded", stuckAt, err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("stuck at %s: preload did not give up within 5s of a 100ms timeout", stuckAt)
		}
	}
}

// 数量上限按全部 kind 合计：正好到上限的不报错；再多一个时那一个不加载、返回 ErrLocalPreloadBudget，已完成的部分留在报告里。
func TestPreloadLocalEntitiesStopsAtItsEntityLimit(t *testing.T) {
	ensurePreloadKinds()
	targets, err := resolveLocalPreloadTargets([]entity.EntityKind{preloadFirstKind, preloadSecondKind})
	if err != nil {
		t.Fatal(err)
	}
	lister := listerFunc(func(_ context.Context, key coredata.DocumentKey, _ int64, _ int) ([]int64, int64, error) {
		return []int64{1, 2, 3}, 0, nil // 每个 kind 3 个
	})
	var loads atomic.Int64
	load := func(context.Context, int64) error { loads.Add(1); return nil }
	report, err := preloadLocalTargets(context.Background(), lister, load, targets, LocalPreloadLimits{MaxEntities: 6})
	if err != nil || report.Loaded != 6 {
		t.Fatalf("exactly at the limit: report %+v, err %v; want 6 loaded and no error", report, err)
	}
	loads.Store(0)
	report, err = preloadLocalTargets(context.Background(), lister, load, targets, LocalPreloadLimits{MaxEntities: 5})
	if !errors.Is(err, ErrLocalPreloadBudget) || !strings.Contains(err.Error(), "dataengine.preload.max_entities") {
		t.Fatalf("one over the limit: err %v, want ErrLocalPreloadBudget naming dataengine.preload.max_entities", err)
	}
	if loads.Load() != 5 || report.Listed != 5 || report.Loaded != 5 {
		t.Fatalf("one over the limit: %d loads, report %+v; want the first 5 loaded and the 6th not", loads.Load(), report)
	}
}

type listerFunc func(context.Context, coredata.DocumentKey, int64, int) ([]int64, int64, error)

func (f listerFunc) ListEntityIDs(ctx context.Context, key coredata.DocumentKey, after int64, limit int) ([]int64, int64, error) {
	return f(ctx, key, after, limit)
}

// 分页：页满时 next 是最后一个 _id，按它继续，不漏不重。
func TestListEntityIDsPagesInIDOrder(t *testing.T) {
	client := mongotest.NewClient()
	server := newPreloadServer(t, client, 3)
	ids := seedPreload(t, client, 3, preloadFirstKind, "preload_first", 500, 5, nil)
	key := coredata.DocumentKey{Database: testDatabase, Scope: coredata.DatabaseServer, Resource: "preload_first"}
	var got []int64
	for after := int64(0); ; {
		page, next, err := server.store.ListEntityIDs(context.Background(), key, after, 2)
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, page...)
		if next == 0 {
			break
		}
		after = next
	}
	if !slices.Equal(got, ids) {
		t.Fatalf("paged ids = %v, want %v", got, ids)
	}
}

// 两个 sid 共用一个 Mongo：各自只预加载本服库里的实体；按 ID 加载别的服的实体得到“不存在”。
func TestPreloadLocalEntitiesLoadsOnlyThisServersDatabase(t *testing.T) {
	client := mongotest.NewClient()
	three, four := newPreloadServer(t, client, 3), newPreloadServer(t, client, 4)
	ofThree := seedPreload(t, client, 3, preloadFirstKind, "preload_first", 600, 3, nil)
	ofFour := seedPreload(t, client, 4, preloadFirstKind, "preload_first", 700, 2, nil)

	for _, server := range []*preloadServer{three, four} {
		if _, err := server.runtime.PreloadLocalEntities(context.Background(), []entity.EntityKind{preloadFirstKind}, LocalPreloadLimits{}); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range ofThree {
		if three.manager.Get(id) == nil || four.manager.Get(id) != nil {
			t.Fatalf("entity %d of sid 3: loaded on 3=%v on 4=%v", id, three.manager.Get(id) != nil, four.manager.Get(id) != nil)
		}
	}
	for _, id := range ofFour {
		if four.manager.Get(id) == nil || three.manager.Get(id) != nil {
			t.Fatalf("entity %d of sid 4: loaded on 4=%v on 3=%v", id, four.manager.Get(id) != nil, three.manager.Get(id) != nil)
		}
	}
	// 按 ID：sid 3 的进程加载 sid 4 的实体只会在 game_3 里找。
	if value, err := three.access.Get(context.Background(), ofFour[0], entity.EntityCategoryNone); value != nil || !errors.Is(err, entity.ErrAuthorityEntityNotFound) {
		t.Fatalf("sid 3 loading an entity of sid 4 by id = %v, %v; want not found", value, err)
	}
}

// 规则在运行期强制一次：DAO 是 dbscope=global 的、Remote 的、没有 builder 的 kind 都不能做本地预加载，启动失败。
func TestPreloadLocalEntitiesRefusesKindsOutsideTheRule(t *testing.T) {
	client := mongotest.NewClient()
	server := newPreloadServer(t, client, 3)
	for _, tc := range []struct {
		kind entity.EntityKind
		want string
	}{
		{preloadGlobalKind, "dbscope=global"},
		{preloadRemoteKind, "Remote kind"},
		{entity.EntityKind(219), "no registered entity builder"},
	} {
		_, err := server.runtime.PreloadLocalEntities(context.Background(), []entity.EntityKind{preloadFirstKind, tc.kind}, LocalPreloadLimits{})
		if !errors.Is(err, ErrLocalPreloadKind) || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("preload of kind %d = %v, want ErrLocalPreloadKind mentioning %q", tc.kind, err, tc.want)
		}
	}
	if len(server.loaded.snapshot()) != 0 {
		t.Fatal("a refused preload still loaded entities")
	}
}

// copyServer 是可以交出实体的源服：preloadServer 加上一个手动回放的投影器。
func newCopyServer(t *testing.T, client *mongotest.Client, sid int32) *preloadServer {
	t.Helper()
	server := newPreloadServer(t, client, sid)
	server.runtime.Projector, _ = manualProjector(t, server.store)
	return server
}

// 搬档：只在交出状态下做；复制幂等；目标里别的版本——overwrite=false 冲突且不写，overwrite=true 覆盖；
// 删副本按版本条件、幂等，之后被写过的保留。
func TestCopyHandedOffEntity(t *testing.T) {
	ctx := context.Background()
	client := mongotest.NewClient()
	source := newCopyServer(t, client, 3)
	id := seedPreload(t, client, 3, preloadFirstKind, "preload_first", 800, 1, bson.M{"level": int32(7)})[0]
	target := client.Collection(testDatabase+"_4", "preload_first")

	if _, err := source.runtime.CopyHandedOffEntity(ctx, id, 4, true); !errors.Is(err, ErrEntityNotHandedOff) {
		t.Fatalf("copy without a hand-off = %v, want ErrEntityNotHandedOff", err)
	}
	if _, err := source.runtime.DropEntityCopy(ctx, EntityCopy{EntityID: id, Documents: []EntityCopyDocument{{Database: testDatabase, Collection: "preload_first", Version: 3}}}, 3); !errors.Is(err, ErrEntityNotHandedOff) {
		t.Fatalf("drop without a hand-off = %v, want ErrEntityNotHandedOff", err)
	}
	if _, found := target.Lookup(id); found {
		t.Fatal("a refused copy wrote the target")
	}
	source.runtime.Projector.holdEntityHandOff(id)

	copied, err := source.runtime.CopyHandedOffEntity(ctx, id, 4, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(copied.Documents) != 1 || copied.Documents[0].Outcome != EntityCopyCopied || copied.Documents[0].Version != 3 {
		t.Fatalf("copy = %+v", copied)
	}
	if doc, found := target.Lookup(id); !found || doc["level"] != int32(7) || doc["_last_tx"] != fmt.Sprintf("tx-%d", id) {
		t.Fatalf("target after the copy = %v", doc)
	}
	again, err := source.runtime.CopyHandedOffEntity(ctx, id, 4, false)
	if err != nil || again.Documents[0].Outcome != EntityCopyAlreadyPresent {
		t.Fatalf("copy again = %+v, %v; want already_present", again, err)
	}

	// 目标库里是别的版本（以前留下的旧副本）。
	if _, err := target.ReplaceOne(ctx, bson.M{"_id": id}, bson.M{"_id": id, "_version": uint64(1), "_schema": uint32(1), "_last_tx": "old", "level": int32(1)}); err != nil {
		t.Fatal(err)
	}
	if _, err := source.runtime.CopyHandedOffEntity(ctx, id, 4, false); !errors.Is(err, ErrEntityCopyConflict) {
		t.Fatalf("copy over another version without overwrite = %v, want ErrEntityCopyConflict", err)
	}
	if doc, _ := target.Lookup(id); doc["level"] != int32(1) {
		t.Fatalf("a conflicting copy changed the target: %v", doc)
	}
	replaced, err := source.runtime.CopyHandedOffEntity(ctx, id, 4, true)
	if err != nil || replaced.Documents[0].Outcome != EntityCopyReplaced {
		t.Fatalf("copy with overwrite = %+v, %v; want replaced", replaced, err)
	}
	if doc, _ := target.Lookup(id); doc["level"] != int32(7) {
		t.Fatalf("overwrite did not write this server's version: %v", doc)
	}

	// 删源库：按版本条件，重做无害。
	if dropped, err := source.runtime.DropEntityCopy(ctx, copied, 3); err != nil || dropped != 1 {
		t.Fatalf("drop the source = %d, %v", dropped, err)
	}
	if dropped, err := source.runtime.DropEntityCopy(ctx, copied, 3); err != nil || dropped != 0 {
		t.Fatalf("drop the source again = %d, %v", dropped, err)
	}
	if _, found := client.Collection(testDatabase+"_3", "preload_first").Lookup(id); found {
		t.Fatal("the source document is still there")
	}
	// 目标服之后写过（版本 4）：中止的清理不删它。
	if _, err := target.UpdateOne(ctx, bson.M{"_id": id}, bson.M{"$set": bson.M{"_version": uint64(4)}}); err != nil {
		t.Fatal(err)
	}
	if dropped, err := source.runtime.DropEntityCopy(ctx, copied, 4); err != nil || dropped != 0 {
		t.Fatalf("drop a target copy that was written since = %d, %v; want it kept", dropped, err)
	}
	if _, found := target.Lookup(id); !found {
		t.Fatal("a target document written after the copy was dropped")
	}
	// 源库没了之后，这个实体在源服按 ID 加载不到，也不再被预加载。
	report, err := source.runtime.PreloadLocalEntities(ctx, []entity.EntityKind{preloadFirstKind}, LocalPreloadLimits{})
	if err != nil || report.Listed != 0 {
		t.Fatalf("preload on the source after the move = %+v, %v; want nothing listed", report, err)
	}
	// 新服预加载到他。
	target4 := newPreloadServer(t, client, 4)
	if report, err := target4.runtime.PreloadLocalEntities(ctx, []entity.EntityKind{preloadFirstKind}, LocalPreloadLimits{}); err != nil || report.Loaded != 1 || target4.manager.Get(id) == nil {
		t.Fatalf("preload on the target after the move = %+v, %v; want the moved entity loaded", report, err)
	}
}
