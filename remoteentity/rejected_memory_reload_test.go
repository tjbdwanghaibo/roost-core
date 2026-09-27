package remoteentity

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/entity"
	"github.com/tjbdwanghaibo/roost-core/fctx"
	fmongo "github.com/tjbdwanghaibo/roost-core/mongo"
	"github.com/tjbdwanghaibo/roost-core/nest"
	"github.com/tjbdwanghaibo/roost-core/sync/entitysync"
	"github.com/tjbdwanghaibo/roost-core/sync/frame"
)

// RR-20260926-39：Remote 事务被持久拒绝、实例隔离后，框架在释放 gate 之后对旧实例做“仅内存卸载”
// （EntityManager.Destroy(deleteFromDB=false)），下一次访问经正式 ManagerAccess 从权威重新加载。
// 沿用 RR-28 复核的 TestRejected*IsNotCarriedByNextWrite 场景，但实例换代走生产路径，
// 不再由测试调用 loader.add 替换实例。

// reloadableFullDocEntity 在 fullDoc 替身上记录 OnDestroy（EntityManager.Destroy 的正式生命周期回调）。
type reloadableFullDocEntity struct {
	*fullDocRemoteEntity
	destroyed       chan entity.EntityDestroyReason
	destroyedOnFast atomic.Bool
	// duringDestroy 在 OnDestroy 开头调用（实例已离开索引、EntityManager 仍把该 ID 记为 removing），供回归停在卸载过程中。
	duringDestroy func()
	// rollbacks / rollbacksOffFast 记录 RollbackRemoteCommit 的调用次数与其中不在快 worker 上的次数（OPEN-ITEMS B26）。
	rollbacks, rollbacksOffFast atomic.Int32
}

func (e *reloadableFullDocEntity) OnDestroy(reason entity.EntityDestroyReason) {
	if e.duringDestroy != nil {
		e.duringDestroy()
	}
	e.destroyedOnFast.Store(fctx.InFastWorker())
	select {
	case e.destroyed <- reason:
	default:
	}
}

// authorityLoader 是 AggregateLoader：按 Mongo 权威（数据文档 + ownership 版本）构造实例并登记进
// EntityManager，与生成仓储一样由 ManagerAccess 在内存未命中时调用。
type authorityLoader struct {
	store   *MongoCommitter
	manager *entity.EntityManager
	kind    entity.EntityKind
	rawID   int64
	loads   atomic.Int32
	// prepare 在实例登记进 EntityManager 之前调用（生成工厂在发布前启用同步，RR-20260926-59 回归用它）。
	prepare func(*reloadableFullDocEntity)
}

func (l *authorityLoader) LoadEntity(ctx context.Context, fullID int64, _ entity.EntityKind) (entity.IThreadSafeEntity, error) {
	l.loads.Add(1)
	e := &reloadableFullDocEntity{fullDocRemoteEntity: newFullDocRemoteEntity(l.rawID, l.kind), destroyed: make(chan entity.EntityDestroyReason, 1)}
	if doc, ok := newCollectionLookup(l.store, fullDocCollection, fullID); ok {
		for _, part := range strings.Split(doc, ";") {
			key, value, _ := strings.Cut(part, "=")
			switch key {
			case "a":
				e.a = value
			case "b":
				e.b = value
			}
		}
	}
	authority, err := l.store.readAuthority(ctx, fullID)
	switch {
	case err == nil:
		e.SetEntityVersion(authority.Version)
	case !errors.Is(err, fmongo.ErrNotFound):
		return nil, err
	}
	if l.prepare != nil {
		l.prepare(e)
	}
	if err := l.manager.TryAdd(e); err != nil {
		return nil, err
	}
	return e, nil
}

type rejectingStorage struct {
	*switchableStorage
	reject atomic.Bool
	// loseReply：事务真实写进 Mongo，但回复丢失（调用方看到超时，结果未知）。
	loseReply atomic.Bool
	// statusHold 非 nil 时，回源读取在返回前等它关闭（进入时先通知 statusHeld），供回归按事件控制 finalizer 拿到结论的时刻。
	statusHold atomic.Pointer[chan struct{}]
	statusHeld chan struct{}
}

func (s *rejectingStorage) CommitStatus(ctx context.Context, id entity.RemoteTransactionID) (entity.RemoteCommitStatus, error) {
	if hold := s.statusHold.Load(); hold != nil {
		select {
		case s.statusHeld <- struct{}{}:
		default:
		}
		select {
		case <-*hold:
		case <-ctx.Done():
			return entity.RemoteCommitStatus{}, ctx.Err()
		}
	}
	return s.switchableStorage.CommitStatus(ctx, id)
}

func (s *rejectingStorage) CommitRemote(ctx context.Context, commit entity.RemoteCommit) (entity.RemoteCommitReceipt, error) {
	if s.reject.Load() {
		return entity.RemoteCommitReceipt{}, entity.ErrRemoteVersionConflict
	}
	if s.loseReply.Load() {
		if _, err := s.switchableStorage.CommitRemote(ctx, commit); err != nil {
			return entity.RemoteCommitReceipt{}, err
		}
		return entity.RemoteCommitReceipt{}, context.DeadlineExceeded
	}
	return s.switchableStorage.CommitRemote(ctx, commit)
}

type reloadFixture struct {
	rejectedMemoryFixture
	storage *rejectingStorage
	access  *entity.ManagerAccess
	loader  *authorityLoader
}

func newReloadFixture(t *testing.T, rawID int64) (reloadFixture, *reloadableFullDocEntity) {
	t.Helper()
	const kind entity.EntityKind = 125
	entity.MustRegisterEntityKindDefs(entity.EntityKindDef{Kind: kind, Category: 1, RemotePolicy: entity.RemotePolicyManaged})
	store := NewMongoCommitter(newRemoteMongoFake(), "control", 1000, 0)
	storage := &rejectingStorage{switchableStorage: &switchableStorage{MongoCommitter: store, statusCalls: make(chan struct{}, 1)}, statusHeld: make(chan struct{}, 1)}
	manager := entity.NewEntityManager()
	access := entity.NewManagerAccess(manager)
	loader := &authorityLoader{store: store, manager: manager, kind: kind, rawID: rawID}
	if _, err := access.ConfigureLoader(loader); err != nil {
		t.Fatal(err)
	}
	backend, err := NewBackend(access, storage)
	if err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig()
	cfg.FinalizeRetryInterval = 10 * time.Millisecond
	mgr := NewManager(newMockVersionedLockFactory(), cfg, 1000)
	mgr.SetBackend(backend)
	mgr.SetOwnershipStore(store)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = mgr.StopFinalizer(ctx)
	})
	first, err := access.Get(context.Background(), testRemoteFullIDWithKind(rawID, 1, kind), entity.EntityCategoryNone)
	if err != nil || first == nil {
		t.Fatalf("initial load: %v", err)
	}
	live := first.(*reloadableFullDocEntity)
	f := reloadFixture{
		rejectedMemoryFixture: rejectedMemoryFixture{mgr: mgr, store: store, storage: storage.switchableStorage, kind: kind, rawID: rawID, id: live.GUId()},
		storage:               storage, access: access, loader: loader,
	}
	return f, live
}

// assertReloadedFromAuthority：被拒绝的旧实例被卸载（不删持久数据），下一次访问从权威重载得到新实例；
// 新实例内存等于 Mongo、可写，且第二笔写入之后 Mongo 只有第二笔（被拒绝的 a=rejected 没有被写出）。
func (f reloadFixture) assertReloadedFromAuthority(t *testing.T, stale *reloadableFullDocEntity) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	select {
	case reason := <-stale.destroyed:
		if reason != entity.DestroyReasonMemoryUnload {
			t.Fatalf("stale instance destroyed with reason %d, want DestroyReasonMemoryUnload", reason)
		}
	case <-ctx.Done():
		next, err := f.mgr.PrepareRemoteWriteBatch(context.Background(), []int64{f.id})
		if err == nil {
			_ = next.Abort(context.Background(), errors.New("probe"))
			_ = next.Close(context.Background())
		}
		t.Fatalf("rejected instance was never unloaded from memory (state=%v, next writer err=%v); only a restart or business reload makes the entity writable again",
			stale.RemoteOwnershipState(), err)
	}
	if !stale.IsRemoved() {
		t.Fatal("stale instance still managed after unload")
	}
	if got := stale.RemoteOwnershipState(); got != entity.RemoteOwnershipQuarantined {
		t.Fatalf("stale instance state=%v, want it to stay quarantined (never thawed)", got)
	}
	f.storage.unreachable.Store(false)
	f.storage.reject.Store(false)
	next, err := f.mgr.PrepareRemoteWriteBatch(ctx, []int64{f.id})
	if err != nil {
		t.Fatalf("entity not writable after the framework unloaded the rejected instance: %v", err)
	}
	value, err := f.access.Get(ctx, f.id, entity.EntityCategoryNone)
	if err != nil || value == nil {
		t.Fatalf("reload: %v", err)
	}
	fresh := value.(*reloadableFullDocEntity)
	if fresh == stale {
		t.Fatal("next access returned the rejected instance")
	}
	if fresh.a != "" || fresh.b != "" {
		t.Fatalf("reloaded memory a=%q b=%q, want the authority state (no document)", fresh.a, fresh.b)
	}
	if doc := f.storedDocument(t); doc != "" {
		t.Fatalf("rejected transaction reached Mongo: %q", doc)
	}
	f.writeB(t, ctx, next, fresh.fullDocRemoteEntity, remoteTestTxID(0xC9))
	if doc := f.storedDocument(t); doc != "a=;b=second" {
		t.Fatalf("Mongo document=%q, want only the second write %q", doc, "a=;b=second")
	}
	if got := "a=" + fresh.a + ";b=" + fresh.b; got != f.storedDocument(t) {
		t.Fatalf("memory %q != Mongo %q", got, f.storedDocument(t))
	}
	if err = f.mgr.StopFinalizer(ctx); err != nil {
		t.Fatal(err)
	}
	if slots := len(f.mgr.remote.writeSlots); slots != 0 {
		t.Fatalf("write slots leaked: %d", slots)
	}
}

func (f reloadFixture) prepareRejected(t *testing.T, live *reloadableFullDocEntity, tx entity.RemoteTransactionID, durability uint8) entity.RemoteWriteBatch {
	t.Helper()
	return f.prepareRejectedWrite(t, live.fullDocRemoteEntity, tx, durability)
}

// Durability 0 从未到达 Mongo：finalizer 持久拒绝、回滚、隔离、释放 gate，然后卸载。
func TestRejectedMemoryWriteReloadsFromAuthority(t *testing.T) {
	f, live := newReloadFixture(t, 1951)
	f.storage.unreachable.Store(true)
	batch := f.prepareRejected(t, live, remoteTestTxID(0xC1), 0)
	if _, err := batch.Commit(context.Background()); !errors.Is(err, entity.ErrRemotePersistenceIndeterminate) {
		t.Fatalf("commit err=%v, want indeterminate", err)
	}
	if err := batch.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.assertReloadedFromAuthority(t, live)
}

// Durability 0 被权威明确拒绝（版本冲突）：Commit 同步回滚并隔离，Close 释放后卸载。
func TestDefinitelyRejectedMemoryWriteReloadsFromAuthority(t *testing.T) {
	f, live := newReloadFixture(t, 1952)
	f.storage.reject.Store(true)
	batch := f.prepareRejected(t, live, remoteTestTxID(0xC2), 0)
	if _, err := batch.Commit(context.Background()); !errors.Is(err, entity.ErrRemoteVersionConflict) {
		t.Fatalf("commit err=%v, want version conflict", err)
	}
	if err := batch.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.assertReloadedFromAuthority(t, live)
}

// Durability 1：投影器持久拒绝（finalizer 首轮即拿到 Rejected）。
func TestRejectedAsyncWriteReloadsFromAuthority(t *testing.T) {
	f, live := newReloadFixture(t, 1953)
	tx := remoteTestTxID(0xC3)
	batch := f.prepareRejected(t, live, tx, 1)
	if _, err := batch.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.mgr.RejectRemoteTransaction(tx, "lease expired")
	if err := batch.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.assertReloadedFromAuthority(t, live)
}

// Durability 2：提交等待拿到拒绝，转交 finalizer 收尾。
func TestRejectedStrictWriteReloadsFromAuthority(t *testing.T) {
	f, live := newReloadFixture(t, 1954)
	tx := remoteTestTxID(0xC4)
	batch := f.prepareRejected(t, live, tx, 2)
	f.mgr.RejectRemoteTransaction(tx, "lease expired")
	if _, err := batch.Commit(context.Background()); !errors.Is(err, entity.ErrRemoteRejected) {
		t.Fatalf("strict commit err=%v, want rejected", err)
	}
	if err := batch.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.assertReloadedFromAuthority(t, live)
}

// 正式装配：Remote Manager 交给 Nest，NewEngine 把 NestMgr.RunLocal 绑定给它（nest.LocalExecutorBinder，与 DataEngine
// 驱逐同一接线）。finalizer 拿到持久拒绝后，仅内存卸载在 Nest 快池执行（不在 finalizer goroutine 上取实体锁）。
// Sync 与 RR-30 驱逐同一规则：卸载关闭旧同步状态（被拒绝的内容不交付、也不发 remove，订阅保持），
// 重载后的实例重新接上（kit 在 OnEntityLoaded 上调 Rebind；这里用等价的 Register 路径）时原订阅者收到全量。
func TestRejectedWriteUnloadsOnNestFastPoolAndRebindsSync(t *testing.T) {
	f, live := newReloadFixture(t, 1955)
	frames := make(chan []byte, 8)
	syncMgr, err := entitysync.NewManager(entitysync.ManagerConfig{Mode: entitysync.ModePeriodic, Interval: time.Hour, Transport: entitysync.TransportFunc(func(_ context.Context, _ entitysync.SessionID, p []byte) error {
		frames <- append([]byte(nil), p...)
		return nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syncMgr.Close(context.Background()) })
	enableSync := func(e *reloadableFullDocEntity, label string) {
		packed := func(entity.SyncProfile) (entity.FrozenSyncPayload, error) {
			return entity.CopyFrozenSyncPayload(1, []byte(label)), nil
		}
		e.EnableSync(entity.EntitySyncCreateParam{Enabled: true, EntityID: e.ID(), Namespace: "test", Packer: entity.SubjectSyncPackFunc{Snapshot: packed, Delta: func(p entity.SyncProfile, _ uint64) (entity.FrozenSyncPayload, error) { return packed(p) }}})
	}
	enableSync(live, "stale")
	if err = syncMgr.Register(live.Sync()); err != nil {
		t.Fatal(err)
	}
	if err = syncMgr.OpenSession(1); err != nil {
		t.Fatal(err)
	}
	if err = syncMgr.Subscribe(1, live.ID(), entity.SyncProfile{}); err != nil {
		t.Fatal(err)
	}
	if err = syncMgr.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	created, err := entitysync.DecodeFrame(<-frames, frame.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	engine := nest.NewEngine(nest.NestOptionWithGetter(f.access), nest.NestOptionWithRemoteEntityManager(f.mgr), nest.NestOptionWithEntitySync(syncMgr), nest.NestOptionWithWorkerNumAndMsgCap(1, 1, 16))
	if err = engine.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Shutdown(context.Background()) })

	f.storage.unreachable.Store(true)
	batch := f.prepareRejected(t, live, remoteTestTxID(0xC5), 0)
	live.MarkSyncDirty(1) // 被拒绝的修改也标了同步脏：卸载后不得交付
	if _, err = batch.Commit(context.Background()); !errors.Is(err, entity.ErrRemotePersistenceIndeterminate) {
		t.Fatalf("commit err=%v, want indeterminate", err)
	}
	if err = batch.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.assertReloadedFromAuthority(t, live)
	if !live.destroyedOnFast.Load() {
		t.Fatal("rejected instance was unloaded outside the Nest fast pool")
	}
	if err = syncMgr.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case raw := <-frames:
		decoded, _ := entitysync.DecodeFrame(raw, frame.DefaultLimits())
		t.Fatalf("unloaded instance still produced a frame: %+v", decoded.Objects)
	default:
	}
	value, err := f.access.Get(context.Background(), f.id, entity.EntityCategoryNone)
	if err != nil || value == nil {
		t.Fatalf("reload: %v", err)
	}
	fresh := value.(*reloadableFullDocEntity)
	enableSync(fresh, "authority")
	if err = syncMgr.Register(fresh.Sync()); err != nil {
		t.Fatalf("reloaded instance was not rebound to the subject: %v", err)
	}
	if err = syncMgr.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case raw := <-frames:
		decoded, err := entitysync.DecodeFrame(raw, frame.DefaultLimits())
		if err != nil {
			t.Fatal(err)
		}
		if len(decoded.Objects) != 1 || decoded.Objects[0].Operation != frame.ObjectUpdate || decoded.Objects[0].Ref != created.Objects[0].Ref {
			t.Fatalf("subscriber frame after reload=%+v, want one full ObjectUpdate of the same object", decoded.Objects)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("subscriber did not receive the reloaded state")
	}
}
