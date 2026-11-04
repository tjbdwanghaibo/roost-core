// 路由登记（联盟服务方案 §2.7）：写准入 / 认领 / 前置检查 / 预加载时登记，转移出去与失去所有权时按值删除，续期只续
// 自己的值、“不存在”先向权威确认，Redis 失败不影响写准入；前置检查不认领不存在的实体；预加载只加载本 sid 的实体。
// 三个 Lua 脚本本身的按值比较在真实 Redis 上验（route_redis_integration_test.go）。

package remoteentity

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	fredis "github.com/tjbdwanghaibo/roost-core/infra/storage/redis"
)

// kind 170～173：本包其他用例没有用过。
const (
	routeKindAlliance entity.EntityKind = 170
	routeKindMember   entity.EntityKind = 171
	routeKindOther    entity.EntityKind = 172
)

func registerRouteKinds() {
	entity.MustRegisterEntityKindDefs(
		entity.EntityKindDef{Kind: routeKindAlliance, Category: 1, RemotePolicy: entity.RemotePolicyManaged},
		entity.EntityKindDef{Kind: routeKindMember, Category: 1, RemotePolicy: entity.RemotePolicyManaged},
		entity.EntityKindDef{Kind: routeKindOther, Category: 1, RemotePolicy: entity.RemotePolicyManaged},
	)
}

// fakeRouteStore 用 Go 实现三个脚本的语义（按值比较），给 Manager 的登记逻辑用。
type fakeRouteStore struct {
	mu        sync.Mutex
	values    map[int64]string
	err       error
	registers int
}

func newFakeRouteStore() *fakeRouteStore { return &fakeRouteStore{values: make(map[int64]string)} }

func (s *fakeRouteStore) Register(_ context.Context, id int64, owner int32, epoch uint64) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.registers++
	if s.err != nil {
		return false, s.err
	}
	if current, ok := s.values[id]; ok {
		if _, stored, err := ParseRouteValue(current); err == nil && stored > epoch {
			return false, nil
		}
	}
	s.values[id] = RouteValue(owner, epoch)
	return true, nil
}

func (s *fakeRouteStore) Renew(_ context.Context, entries []RouteEntry) ([]RouteRenewResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return nil, s.err
	}
	results := make([]RouteRenewResult, len(entries))
	for i, entry := range entries {
		current, ok := s.values[entry.ID]
		switch {
		case !ok:
			results[i] = RouteMissing
		case current != entry.Value:
			results[i] = RouteReplaced
		default:
			results[i] = RouteRenewed
		}
	}
	return results, nil
}

func (s *fakeRouteStore) Delete(_ context.Context, entries []RouteEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	for _, entry := range entries {
		if s.values[entry.ID] == entry.Value {
			delete(s.values, entry.ID)
		}
	}
	return nil
}

func (s *fakeRouteStore) Lookup(_ context.Context, id int64) (int32, uint64, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return 0, 0, false, s.err
	}
	value, ok := s.values[id]
	if !ok {
		return 0, 0, false, nil
	}
	owner, epoch, err := ParseRouteValue(value)
	return owner, epoch, err == nil, err
}

func (s *fakeRouteStore) value(id int64) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.values[id]
}

func (s *fakeRouteStore) set(id int64, value string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if value == "" {
		delete(s.values, id)
		return
	}
	s.values[id] = value
}

func (s *fakeRouteStore) setErr(err error) {
	s.mu.Lock()
	s.err = err
	s.mu.Unlock()
}

// listingMarkerStore 是能按 owner 枚举的所有权存储替身（MongoCommitter.ListOwnedRemoteEntities 的语义）。
type listingMarkerStore struct {
	*mockMarkerStore
	listed  []int64 // 非空时原样返回这一份（模拟枚举之后所有权又变了）
	listErr error
}

func (s *listingMarkerStore) ListOwnedRemoteEntities(_ context.Context, owner int32, kinds []entity.EntityKind, after int64, limit int) ([]int64, int64, error) {
	if s.listErr != nil {
		return nil, 0, s.listErr
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var ids []int64
	if s.listed != nil {
		ids = append(ids, s.listed...)
	} else {
		for id, lease := range s.leases {
			if lease.OwnerSid == owner && !lease.Shared && slices.Contains(kinds, entity.ResolveEntityID(id).Kind) {
				ids = append(ids, id)
			}
		}
	}
	slices.Sort(ids)
	var page []int64
	for _, id := range ids {
		if id > after && len(page) < limit {
			page = append(page, id)
		}
	}
	return page, 0, nil
}

func newRoutedManager(t *testing.T, sid int32, store *fakeRouteStore, markers entity.IRemoteEntityOwnershipStore, loader entity.IRemoteEntityBackend) *Manager {
	t.Helper()
	registerRouteKinds()
	cfg := DefaultConfig()
	cfg.RouteKinds = []entity.EntityKind{routeKindAlliance, routeKindMember}
	mgr := NewManager(newMockVersionedLockFactory(), cfg, sid)
	mgr.SetOwnershipStore(markers)
	mgr.SetBackend(loader)
	mgr.routes = newRouteRegistry(store, cfg)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = mgr.StopFinalizer(ctx)
	})
	return mgr
}

// writeOnce 走一次完整的写准入（Prepare → Abort → Close），不提交。
func writeOnce(t *testing.T, mgr *Manager, id int64) error {
	t.Helper()
	ctx := context.Background()
	batch, err := mgr.PrepareRemoteWriteBatch(ctx, []int64{id})
	if err != nil {
		return err
	}
	if err := batch.Abort(ctx, errors.New("test: no commit")); err != nil {
		t.Fatal(err)
	}
	return batch.Close(ctx)
}

func TestRouteIsRegisteredOnWriteAndDeletedWhenOwnershipMovesAway(t *testing.T) {
	store, markers := newFakeRouteStore(), newMockMarkerStore()
	oldLoader := newRemoteTestLoader()
	old := newRoutedManager(t, 1000, store, markers, oldLoader)
	live := newTestRemoteEntity(7001, 1, routeKindAlliance)
	oldLoader.add(live)
	id := live.GUId()

	// 首写认领 → 登记；同一个值不再碰 Redis。
	if err := writeOnce(t, old, id); err != nil {
		t.Fatal(err)
	}
	if got := store.value(id); got != "1000:1" || !old.RouteHeld(id) {
		t.Fatalf("after the first write route=%q held=%v; want 1000:1 registered", got, old.RouteHeld(id))
	}
	if err := writeOnce(t, old, id); err != nil || store.registers != 1 {
		t.Fatalf("second write err=%v registers=%d; an unchanged registration must not touch Redis", err, store.registers)
	}

	// 转移出去：按值删除，不再持有。
	if _, err := old.TransferRemoteOwnership(context.Background(), id, 2000); err != nil {
		t.Fatal(err)
	}
	if got := store.value(id); got != "" || old.RouteHeld(id) {
		t.Fatalf("after the transfer route=%q held=%v; want it deleted", got, old.RouteHeld(id))
	}

	// 新 owner 第一次写时登记更新的代际。
	newLoader := newRemoteTestLoader()
	newLoader.add(newTestRemoteEntity(7001, 1, routeKindAlliance))
	next := newRoutedManager(t, 2000, store, markers, newLoader)
	if err := writeOnce(t, next, id); err != nil {
		t.Fatal(err)
	}
	if got := store.value(id); got != "2000:2" {
		t.Fatalf("new owner route=%q; want 2000:2", got)
	}

	// 迟到的旧 owner：旧代际的登记盖不掉、旧值的删除删不掉新 owner 的登记。
	old.registerRoute(context.Background(), id, routeKindAlliance, entity.RemoteEntityMarkerLease{OwnerSid: 1000, MarkerEpoch: 1, RouteEpoch: 1})
	old.dropRoute(context.Background(), id, "1000:1")
	if got := store.value(id); got != "2000:2" || old.RouteHeld(id) {
		t.Fatalf("after the late old owner route=%q oldHeld=%v; want 2000:2 kept", got, old.RouteHeld(id))
	}

	// 别的服务（旧 owner）写准入被 fence 时，它不再持有任何登记。
	if err := writeOnce(t, old, id); !errors.Is(err, entity.ErrRemoteFenced) {
		t.Fatalf("old owner write err=%v; want fenced", err)
	}
	if got := store.value(id); got != "2000:2" {
		t.Fatalf("a fenced write changed the new owner's route to %q", got)
	}
}

func TestRouteRenewalReRegistersMissingKeysOnlyWhileStillTheOwner(t *testing.T) {
	store, markers := newFakeRouteStore(), newMockMarkerStore()
	loader := newRemoteTestLoader()
	mgr := newRoutedManager(t, 1000, store, markers, loader)
	first, second := newTestRemoteEntity(7101, 1, routeKindAlliance), newTestRemoteEntity(7102, 1, routeKindMember)
	loader.add(first, second)
	for _, id := range []int64{first.GUId(), second.GUId()} {
		if err := writeOnce(t, mgr, id); err != nil {
			t.Fatal(err)
		}
	}

	// Redis 丢了键：续期得到“不存在”，向权威确认仍是本 sid 后重新登记。
	store.set(first.GUId(), "")
	mgr.renewRoutes(context.Background())
	if got := store.value(first.GUId()); got != "1000:1" {
		t.Fatalf("a lost key was re-registered as %q; want 1000:1", got)
	}

	// 所有权已在权威里改到别人（例如运维直接改）且键丢了：不重新登记，不再持有。
	markers.mu.Lock()
	markers.leases[first.GUId()] = entity.RemoteEntityMarkerLease{OwnerSid: 2000, MarkerEpoch: 2, RouteEpoch: 2}
	markers.mu.Unlock()
	store.set(first.GUId(), "")
	mgr.renewRoutes(context.Background())
	if got := store.value(first.GUId()); got != "" || mgr.RouteHeld(first.GUId()) {
		t.Fatalf("route=%q held=%v after ownership moved away; a lost key must not be re-registered", got, mgr.RouteHeld(first.GUId()))
	}

	// 值已被别人换掉：不再续这个键，也不动别人的值。
	store.set(second.GUId(), "2000:9")
	mgr.renewRoutes(context.Background())
	if got := store.value(second.GUId()); got != "2000:9" || mgr.RouteHeld(second.GUId()) {
		t.Fatalf("route=%q held=%v; a replaced registration must be left alone", got, mgr.RouteHeld(second.GUId()))
	}
}

// Redis 不可用：写准入照常（准入看所有权记录与 fence），登记留在 held 里，Redis 恢复后续期重建；路由查找回退读所有权记录。
func TestRedisOutageKeepsWriteAdmissionAndFallsBackToTheOwnershipRecord(t *testing.T) {
	store, markers := newFakeRouteStore(), newMockMarkerStore()
	loader := newRemoteTestLoader()
	mgr := newRoutedManager(t, 1000, store, markers, loader)
	live := newTestRemoteEntity(7201, 1, routeKindAlliance)
	loader.add(live)
	id := live.GUId()

	store.setErr(errors.New("redis: connection refused"))
	if err := writeOnce(t, mgr, id); err != nil {
		t.Fatalf("write admission failed while Redis was down: %v", err)
	}
	if !mgr.RouteHeld(id) {
		t.Fatal("a failed registration is not kept for the renewal to retry")
	}
	lookup, err := NewRouteLookup(&RouteStore{redis: failingRouteRedis{}, ttl: DefaultRouteTTL}, mgr)
	if err != nil {
		t.Fatal(err)
	}
	owner, found, err := lookup.LookupOwner(context.Background(), id)
	if err != nil || !found || owner != 1000 {
		t.Fatalf("lookup with Redis down = %d,%v,%v; want 1000 from the ownership record", owner, found, err)
	}
	if _, found, err := lookup.LookupOwner(context.Background(), newTestRemoteEntity(7299, 1, routeKindAlliance).GUId()); err != nil || found {
		t.Fatalf("lookup of a missing entity = found %v, err %v; want not found", found, err)
	}

	store.setErr(nil)
	mgr.renewRoutes(context.Background())
	if got := store.value(id); got != "1000:1" {
		t.Fatalf("after Redis came back the renewal registered %q; want 1000:1", got)
	}
}

type failingRouteRedis struct{ fredis.IRedis }

func (failingRouteRedis) Get(context.Context, string) ([]byte, error) {
	return nil, errors.New("redis: connection refused")
}

// 非创建类请求的前置检查不认领不存在的实体：没有所有权记录就答“不存在”，不碰写准入、不加载。
func TestServeRoutedRequestNeverClaimsAMissingEntity(t *testing.T) {
	store, markers := newFakeRouteStore(), newMockMarkerStore()
	loader := newRemoteTestLoader()
	mgr := newRoutedManager(t, 1000, store, markers, loader)
	missing := newTestRemoteEntity(7301, 1, routeKindAlliance)
	loader.add(missing)
	owner, found, err := mgr.ServeRoutedRequest(context.Background(), missing.GUId())
	if err != nil || found || owner != 0 {
		t.Fatalf("missing entity = %d,%v,%v; want not found", owner, found, err)
	}
	if _, claimed, _ := markers.GetOwnership(context.Background(), missing.GUId()); claimed {
		t.Fatal("the precheck claimed an entity that has no ownership record")
	}
	if len(loader.loaded) != 0 || store.value(missing.GUId()) != "" {
		t.Fatalf("loaded=%v route=%q for a missing entity", loader.loaded, store.value(missing.GUId()))
	}

	// owner 是别人：返回它（调用方答改投），不加载。
	other := newTestRemoteEntity(7302, 1, routeKindAlliance)
	loader.add(other)
	markers.leases[other.GUId()] = entity.RemoteEntityMarkerLease{OwnerSid: 2000, MarkerEpoch: 4, RouteEpoch: 4}
	if owner, found, err := mgr.ServeRoutedRequest(context.Background(), other.GUId()); err != nil || !found || owner != 2000 || len(loader.loaded) != 0 {
		t.Fatalf("other owner = %d,%v,%v loaded=%v; want 2000 without loading", owner, found, err, loader.loaded)
	}

	// owner 是本 sid、还没加载：冷加载并登记；之后走快路径，不再读权威。
	mine := newTestRemoteEntity(7303, 1, routeKindAlliance)
	loader.add(mine)
	markers.leases[mine.GUId()] = entity.RemoteEntityMarkerLease{OwnerSid: 1000, MarkerEpoch: 3, RouteEpoch: 3}
	if owner, found, err := mgr.ServeRoutedRequest(context.Background(), mine.GUId()); err != nil || !found || owner != 1000 {
		t.Fatalf("own entity = %d,%v,%v", owner, found, err)
	}
	if !slices.Contains(loader.loaded, mine.GUId()) || store.value(mine.GUId()) != "1000:3" {
		t.Fatalf("loaded=%v route=%q; want a cold load and registration 1000:3", loader.loaded, store.value(mine.GUId()))
	}
	gets := markers.gets
	if owner, _, err := mgr.ServeRoutedRequest(context.Background(), mine.GUId()); err != nil || owner != 1000 || markers.gets != gets {
		t.Fatalf("held route: owner=%d err=%v authority reads %d → %d; want the fast path", owner, err, gets, markers.gets)
	}
}

func TestPreloadLoadsOnlyEntitiesThisSidOwns(t *testing.T) {
	store := newFakeRouteStore()
	markers := &listingMarkerStore{mockMarkerStore: newMockMarkerStore()}
	loader := newRemoteTestLoader()
	mgr := newRoutedManager(t, 1000, store, markers, loader)
	own1, own2 := newTestRemoteEntity(7401, 1, routeKindAlliance), newTestRemoteEntity(7402, 1, routeKindAlliance)
	otherKind := newTestRemoteEntity(7403, 1, routeKindMember)
	otherOwner := newTestRemoteEntity(7404, 1, routeKindAlliance)
	shared := newTestRemoteEntity(7405, 1, routeKindAlliance)
	loader.add(own1, own2, otherKind, otherOwner, shared)
	markers.leases[own1.GUId()] = entity.RemoteEntityMarkerLease{OwnerSid: 1000, MarkerEpoch: 1, RouteEpoch: 1}
	markers.leases[own2.GUId()] = entity.RemoteEntityMarkerLease{OwnerSid: 1000, MarkerEpoch: 5, RouteEpoch: 5}
	markers.leases[otherKind.GUId()] = entity.RemoteEntityMarkerLease{OwnerSid: 1000, MarkerEpoch: 1, RouteEpoch: 1}
	markers.leases[otherOwner.GUId()] = entity.RemoteEntityMarkerLease{OwnerSid: 2000, MarkerEpoch: 1, RouteEpoch: 1}
	markers.leases[shared.GUId()] = entity.RemoteEntityMarkerLease{OwnerSid: 1000, MarkerEpoch: 2, RouteEpoch: 1, Shared: true}
	markers.marks[shared.GUId()] = true

	report, err := mgr.PreloadOwnedEntities(context.Background(), []entity.EntityKind{routeKindAlliance})
	if err != nil {
		t.Fatal(err)
	}
	loaded := append([]int64(nil), loader.loaded...)
	slices.Sort(loaded)
	if want := []int64{own1.GUId(), own2.GUId()}; !slices.Equal(loaded, want) || report.Loaded != 2 {
		t.Fatalf("preload loaded %v (report %+v); want exactly the two alliances this sid owns %v", loaded, report, want)
	}
	if store.value(own1.GUId()) != "1000:1" || store.value(own2.GUId()) != "1000:5" {
		t.Fatalf("preloaded entities registered %q / %q", store.value(own1.GUId()), store.value(own2.GUId()))
	}

	// 枚举之后所有权又变了（列表里有，权威说是别人）：跳过，不加载。单个加载失败只计数。
	loader.loaded = nil
	moved := newTestRemoteEntity(7406, 1, routeKindAlliance)
	broken := newTestRemoteEntity(7407, 1, routeKindAlliance)
	loader.add(moved)
	markers.leases[moved.GUId()] = entity.RemoteEntityMarkerLease{OwnerSid: 2000, MarkerEpoch: 2, RouteEpoch: 2}
	markers.leases[broken.GUId()] = entity.RemoteEntityMarkerLease{OwnerSid: 1000, MarkerEpoch: 1, RouteEpoch: 1}
	markers.listed = []int64{moved.GUId(), broken.GUId()}
	report, err = mgr.PreloadOwnedEntities(context.Background(), []entity.EntityKind{routeKindAlliance})
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(loader.loaded, moved.GUId()) || report.Skipped != 1 || report.Failed != 1 {
		t.Fatalf("loaded=%v report=%+v; want the moved entity skipped and the missing one counted as failed", loader.loaded, report)
	}

	// 枚举失败：启动失败。
	markers.listErr = errors.New("mongo: not primary")
	if _, err := mgr.PreloadOwnedEntities(context.Background(), []entity.EntityKind{routeKindAlliance}); err == nil {
		t.Fatal("a failed enumeration must fail the preload")
	}
}

// repositoryLoader 像 DataEngine 的聚合仓库那样答“权威里没有”：替身里没有的实体答满足 entity.ErrAuthorityEntityNotFound
// 的错误（engine.ErrEntityAggregateNotFound），deleted 里的答满足 entity.ErrAuthorityEntityDeleted 的错误。
type repositoryLoader struct {
	*remoteTestLoader
	deleted map[int64]bool
}

func (l *repositoryLoader) LoadRemoteEntity(ctx context.Context, id int64, kind entity.EntityKind) (entity.IThreadSafeRemoteEntity, error) {
	loaded, err := l.remoteTestLoader.LoadRemoteEntity(ctx, id, kind)
	switch {
	case err != nil || loaded != nil:
		return loaded, err
	case l.deleted[id]:
		return nil, fmt.Errorf("test repository: aggregate %d was deleted: %w", id, entity.ErrAuthorityEntityDeleted)
	default:
		return nil, fmt.Errorf("test repository: aggregate %d not found: %w", id, entity.ErrAuthorityEntityNotFound)
	}
}

// 空认领（联盟服务阶段 5）：首写认领发生在写准入，认领它的事务被拒绝回滚后认领不撤销，权威里没有聚合。同 sid 重启后
// 预加载把它计为 Empty（不算失败、不登记），前置检查答“由本实例服务”（交给业务：读请求由 Nest 答不存在，创建类请求
// 照常首写），之后同 ID 的首写成功并登记路由。删过档的（D10）与之区分：预加载计为 Deleted，前置检查答删过档。
func TestEmptyClaimIsAnOwnedEntityThatDoesNotExist(t *testing.T) {
	registerRouteKinds()
	ctx := context.Background()
	store := newFakeRouteStore()
	markers := &listingMarkerStore{mockMarkerStore: newMockMarkerStore()}

	// 重启前：业务在本地新建实体（GetOrCreate），写准入首写认领并登记，事务被拒绝回滚（Abort），没有任何提交。
	before := newRemoteTestLoader()
	created := newTestRemoteEntity(7501, 1, routeKindAlliance)
	before.add(created)
	first := newRoutedManager(t, 1000, store, markers, before)
	id := created.GUId()
	if err := writeOnce(t, first, id); err != nil {
		t.Fatal(err)
	}
	if _, claimed, _ := markers.GetOwnership(ctx, id); !claimed {
		t.Fatal("the rolled-back first write did not leave its claim; the case under test does not arise")
	}
	if err := first.stopRouteRenewal(ctx); err != nil {
		t.Fatal(err)
	}

	// 同 sid 重启：内存里没有它，权威里也没有聚合。另有一个删过档的实体，所有权记录同样是本 sid。
	tombstone := newTestRemoteEntity(7502, 1, routeKindAlliance)
	markers.leases[tombstone.GUId()] = entity.RemoteEntityMarkerLease{OwnerSid: 1000, MarkerEpoch: 1, RouteEpoch: 1}
	after := &repositoryLoader{remoteTestLoader: newRemoteTestLoader(), deleted: map[int64]bool{tombstone.GUId(): true}}
	restarted := newRoutedManager(t, 1000, store, markers, after)
	report, err := restarted.PreloadOwnedEntities(ctx, []entity.EntityKind{routeKindAlliance})
	if err != nil {
		t.Fatal(err)
	}
	if report.Listed != 2 || report.Empty != 1 || report.Deleted != 1 || report.Failed != 0 || report.Loaded != 0 {
		t.Fatalf("preload report %+v; want the empty claim counted as Empty and the tombstone as Deleted, neither as Failed", report)
	}
	if restarted.RouteHeld(id) || store.value(id) != "" || restarted.RouteHeld(tombstone.GUId()) {
		t.Fatalf("preload registered an entity that does not exist: route=%q", store.value(id))
	}

	// 前置检查：空认领由本实例服务（不是“可重试”，也不登记）；删过档答删过档。
	owner, found, err := restarted.ServeRoutedRequest(ctx, id)
	if err != nil || !found || owner != 1000 {
		t.Fatalf("route check on the empty claim = %d,%v,%v; want served here (1000,true,nil)", owner, found, err)
	}
	if restarted.RouteHeld(id) || store.value(id) != "" {
		t.Fatalf("the route check registered the empty claim: route=%q", store.value(id))
	}
	if _, _, err := restarted.ServeRoutedRequest(ctx, tombstone.GUId()); !errors.Is(err, entity.ErrAuthorityEntityDeleted) {
		t.Fatalf("route check on the deleted entity err=%v; want ErrAuthorityEntityDeleted", err)
	}

	// 同 ID 再建：业务新建实例后，写准入照常首写（所有权本来就是本 sid），之后才登记路由。
	after.add(newTestRemoteEntity(7501, 1, routeKindAlliance))
	if err := writeOnce(t, restarted, id); err != nil {
		t.Fatalf("first write on the empty claim after restart: %v", err)
	}
	if got := store.value(id); got != "1000:1" || !restarted.RouteHeld(id) {
		t.Fatalf("after the re-creating write route=%q held=%v; want 1000:1", got, restarted.RouteHeld(id))
	}
}

// Manager.ListOwnedRemoteEntities 是运维按 owner 枚举的正式入口：只列本 sid 独占的、kind 在列表里的实体，一页受
// OwnedListPageLimit 约束；没有枚举能力的所有权存储明确报错。
func TestListOwnedRemoteEntitiesListsThisSidsExclusiveEntities(t *testing.T) {
	registerRouteKinds()
	ctx := context.Background()
	markers := &listingMarkerStore{mockMarkerStore: newMockMarkerStore()}
	mgr := newRoutedManager(t, 1000, newFakeRouteStore(), markers, newRemoteTestLoader())
	var want []int64
	for raw := int64(7701); raw < 7701+OwnedListPageLimit+5; raw++ {
		id := newTestRemoteEntity(raw, 1, routeKindAlliance).GUId()
		markers.leases[id] = entity.RemoteEntityMarkerLease{OwnerSid: 1000, MarkerEpoch: 1, RouteEpoch: 1}
		want = append(want, id)
	}
	slices.Sort(want)
	otherOwner := newTestRemoteEntity(9001, 1, routeKindAlliance).GUId()
	otherKind := newTestRemoteEntity(9002, 1, routeKindMember).GUId()
	markers.leases[otherOwner] = entity.RemoteEntityMarkerLease{OwnerSid: 2000, MarkerEpoch: 1, RouteEpoch: 1}
	markers.leases[otherKind] = entity.RemoteEntityMarkerLease{OwnerSid: 1000, MarkerEpoch: 1, RouteEpoch: 1}

	ids, _, err := mgr.ListOwnedRemoteEntities(ctx, []entity.EntityKind{routeKindAlliance}, 0, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(ids, want[:OwnedListPageLimit]) {
		t.Fatalf("listed %d ids (first %v); want the first %d alliances this sid owns", len(ids), ids[:min(3, len(ids))], OwnedListPageLimit)
	}
	if ids, _, err := mgr.ListOwnedRemoteEntities(ctx, []entity.EntityKind{routeKindAlliance}, want[OwnedListPageLimit-1], 10); err != nil || !slices.Equal(ids, want[OwnedListPageLimit:]) {
		t.Fatalf("second page = %v, %v; want the remaining %d", ids, err, len(want)-OwnedListPageLimit)
	}
	if ids, _, err := mgr.ListOwnedRemoteEntities(ctx, nil, 0, 10); err != nil || len(ids) != 0 {
		t.Fatalf("no kinds = %v, %v; want an empty page", ids, err)
	}

	plain := newRoutedManager(t, 1000, newFakeRouteStore(), newMockMarkerStore(), newRemoteTestLoader())
	if _, _, err := plain.ListOwnedRemoteEntities(ctx, []entity.EntityKind{routeKindAlliance}, 0, 10); err == nil {
		t.Fatal("an ownership store that cannot enumerate must fail the listing")
	}
}

func TestStopDeletesHeldRoutesAndStopsRenewal(t *testing.T) {
	store, markers := newFakeRouteStore(), newMockMarkerStore()
	loader := newRemoteTestLoader()
	mgr := newRoutedManager(t, 1000, store, markers, loader)
	live := newTestRemoteEntity(7501, 1, routeKindAlliance)
	loader.add(live)
	if err := writeOnce(t, mgr, live.GUId()); err != nil {
		t.Fatal(err)
	}
	mgr.routes.interval = time.Millisecond
	mgr.startRouteRenewal()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := mgr.stopRouteRenewal(ctx); err != nil {
		t.Fatal(err)
	}
	if got := store.value(live.GUId()); got != "" || mgr.RouteHeld(live.GUId()) {
		t.Fatalf("after stop route=%q held=%v; want deleted", got, mgr.RouteHeld(live.GUId()))
	}
	mgr.startRouteRenewal() // 停止之后不再启动
	if mgr.routes.done != nil {
		select {
		case <-mgr.routes.done:
		default:
			t.Fatal("the renewal loop restarted after stop")
		}
	}
}

func TestRouteValueRoundTrip(t *testing.T) {
	if RouteKey("", 42) != "remote_entity:route:42" || RouteKey("roost:demo", 42) != "roost:demo:remote_entity:route:42" {
		t.Fatalf("keys %q / %q", RouteKey("", 42), RouteKey("roost:demo", 42))
	}
	owner, epoch, err := ParseRouteValue(RouteValue(3000, 1<<60))
	if err != nil || owner != 3000 || epoch != 1<<60 {
		t.Fatalf("round trip = %d,%d,%v", owner, epoch, err)
	}
	for _, bad := range []string{"", "3000", "x:1", "3000:0", "0:1", "3000:-1"} {
		if _, _, err := ParseRouteValue(bad); err == nil {
			t.Errorf("ParseRouteValue(%q) accepted", bad)
		}
	}
}
