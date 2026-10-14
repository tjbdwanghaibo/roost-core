// 被拒绝的写不随下一笔写入落库：旧实例隔离、仅内存卸载后从权威重载，重载窗口返回可重试哨兵，有订阅者时重新同步。

package remoteentity

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/nest"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/entitysync"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/frame"
	"github.com/tjbdwanghaibo/roost-core/infra/base/fctx"
	fmongo "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo"
)

// fullDocRemoteEntity 与生成实体的持久化形态一致：BuildRemoteCommitLocked 不改动内存、
// 把整份当前内存状态编码为整文档（生成 DAO 的 MarshalPersist 忽略 mask，写入是 ReplaceOne），
// RollbackRemoteCommit 是 no-op。被拒绝事务的内存修改因此会留在实体里。
type fullDocRemoteEntity struct {
	*testRemoteEntity
	mu   sync.Mutex
	a, b string
	// scope 是提交里 mutation 的 DatabaseScope（缺省 0 = global）；RR-20260927-32 的回归设成 sid，让 FinalizeLocked 按 RR-09 拒绝。
	scope uint8
}

const fullDocCollection = "remote_fulldoc"

func newFullDocRemoteEntity(id int64, kind entity.EntityKind) *fullDocRemoteEntity {
	return &fullDocRemoteEntity{testRemoteEntity: newTestRemoteEntity(id, 1, kind)}
}

func (e *fullDocRemoteEntity) set(a, b string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if a != "" {
		e.a = a
	}
	if b != "" {
		e.b = b
	}
}

func (*fullDocRemoteEntity) HasRemoteCommitLocked(entity.RemoteTransactionOutcome) bool { return true }

func (e *fullDocRemoteEntity) BuildRemoteCommitLocked(lease entity.RemoteWriteLease, _ entity.RemoteTransactionOutcome) (entity.RemoteCommit, error) {
	e.mu.Lock()
	data := []byte(fmt.Sprintf("a=%s;b=%s", e.a, e.b))
	scope := e.scope
	e.mu.Unlock()
	return entity.RemoteCommit{
		Schema: 1, Codec: 1,
		Mutations: []entity.RemoteDataMutation{{Collection: fullDocCollection, ID: e.GUId(), Version: lease.BaseVersion + 1, Mask: 1, Data: data, DatabaseScope: scope}},
	}, nil
}

func (*fullDocRemoteEntity) AcknowledgeRemoteCommit(entity.RemoteCommit) error { return nil }
func (*fullDocRemoteEntity) RollbackRemoteCommit(entity.RemoteCommit)          {}

// localLookupLoader 模拟正式 loader：本地实例来自 EntityManager，替换实例即“重新加载”。
type localLookupLoader struct{ *remoteTestLoader }

func (l localLookupLoader) LookupLocalRemoteEntity(id int64, _ entity.EntityKind) entity.IThreadSafeRemoteEntity {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.entities[id]
}

type switchableStorage struct {
	*MongoCommitter
	unreachable atomic.Bool
	// reject：权威明确拒绝（版本冲突），Durability 0 的 Commit 同步回滚。
	reject atomic.Bool
	// rejectWriteFailures > 0 时 finalizer 写持久拒绝（RejectUnresolvedRemoteCommits）失败并减一，模拟拒绝写入的瞬时故障。
	rejectWriteFailures atomic.Int32
	statusCalls         chan struct{}
}

func (s *switchableStorage) RejectUnresolvedRemoteCommits(ctx context.Context, commits []entity.RemoteCommit, cause string) (entity.RemoteCommitStatus, error) {
	if n := s.rejectWriteFailures.Load(); n > 0 && s.rejectWriteFailures.CompareAndSwap(n, n-1) {
		return entity.RemoteCommitStatus{}, errors.New("test: rejection write failed")
	}
	return s.MongoCommitter.RejectUnresolvedRemoteCommits(ctx, commits, cause)
}

func (s *switchableStorage) CommitStatus(ctx context.Context, id entity.RemoteTransactionID) (entity.RemoteCommitStatus, error) {
	select {
	case s.statusCalls <- struct{}{}:
	default:
	}
	return s.MongoCommitter.CommitStatus(ctx, id)
}

func (s *switchableStorage) CommitRemoteBatch(ctx context.Context, commits []entity.RemoteCommit) ([]entity.RemoteCommitReceipt, error) {
	if s.reject.Load() {
		return nil, entity.ErrRemoteVersionConflict
	}
	if s.unreachable.Load() {
		return nil, context.DeadlineExceeded
	}
	return s.MongoCommitter.CommitRemoteBatch(ctx, commits)
}

func (s *switchableStorage) CommitRemote(ctx context.Context, commit entity.RemoteCommit) (entity.RemoteCommitReceipt, error) {
	if s.reject.Load() {
		return entity.RemoteCommitReceipt{}, entity.ErrRemoteVersionConflict
	}
	if s.unreachable.Load() {
		return entity.RemoteCommitReceipt{}, context.DeadlineExceeded
	}
	return s.MongoCommitter.CommitRemote(ctx, commit)
}

type rejectedMemoryFixture struct {
	mgr     *Manager
	store   *MongoCommitter
	storage *switchableStorage
	loader  localLookupLoader
	kind    entity.EntityKind
	rawID   int64
	id      int64
}

func newRejectedMemoryFixture(t *testing.T, rawID int64) (rejectedMemoryFixture, *fullDocRemoteEntity) {
	t.Helper()
	const kind entity.EntityKind = 123
	entity.MustRegisterEntityKindDefs(entity.EntityKindDef{Kind: kind, Category: 1, RemotePolicy: entity.RemotePolicyManaged})
	store := NewMongoCommitter(newRemoteMongoFake(), "control", 1000, 0)
	storage := &switchableStorage{MongoCommitter: store, statusCalls: make(chan struct{}, 1)}
	loader := localLookupLoader{newRemoteTestLoader()}
	live := newFullDocRemoteEntity(rawID, kind)
	loader.add(live)
	backend, err := NewBackend(loader, storage)
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
	return rejectedMemoryFixture{mgr: mgr, store: store, storage: storage, loader: loader, kind: kind, rawID: rawID, id: live.GUId()}, live
}

// writeB 以 Durability 0 提交第二笔业务：只修改 b。
func (f rejectedMemoryFixture) writeB(t *testing.T, ctx context.Context, batch entity.RemoteWriteBatch, live *fullDocRemoteEntity, tx entity.RemoteTransactionID) {
	t.Helper()
	live.set("", "second")
	if err := batch.FinalizeLocked(entity.NewRemoteTransactionOutcome(tx, "second", "", true, 0)); err != nil {
		t.Fatal(err)
	}
	if _, err := batch.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := batch.Close(ctx); err != nil {
		t.Fatal(err)
	}
}

func (f rejectedMemoryFixture) storedDocument(t *testing.T) string {
	t.Helper()
	doc, ok := newCollectionLookup(f.store, fullDocCollection, f.id)
	if !ok {
		return ""
	}
	return doc
}

func newCollectionLookup(store *MongoCommitter, collection string, id int64) (string, bool) {
	var doc struct {
		Data []byte `bson:"data"`
	}
	if err := store.controlDB().Collection(collection).FindOne(context.Background(), map[string]any{"_id": id}, &doc); err != nil {
		return "", false
	}
	return string(doc.Data), true
}

// assertRejectedWriteNotCarried 是本组回归的核心断言：被拒绝事务的内存修改（a=rejected）
// 不能随同实体的下一次写入进入 Mongo。持有被拒绝修改的旧实例必须保持隔离；只有换成
// 从权威重新加载的实例后才能写，Mongo 终态必须等于“只应用第二笔”。
func (f rejectedMemoryFixture) assertRejectedWriteNotCarried(t *testing.T, stale *fullDocRemoteEntity) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	f.storage.unreachable.Store(false)
	next, err := f.mgr.PrepareRemoteWriteBatch(ctx, []int64{f.id})
	if err == nil {
		f.writeB(t, ctx, next, stale, remoteTestTxID(0xB1))
		t.Fatalf("instance holding a rejected mutation stayed writable; Mongo now holds %q, want only the second write %q",
			f.storedDocument(t), "a=;b=second")
	}
	if !errors.Is(err, entity.ErrRemoteFenced) {
		t.Fatalf("next writer on the stale instance err=%v, want immediate ErrRemoteFenced (gate released, entity quarantined)", err)
	}
	if got := stale.RemoteOwnershipState(); got != entity.RemoteOwnershipQuarantined {
		t.Fatalf("stale instance state=%v, want quarantined until reload", got)
	}
	if doc := f.storedDocument(t); doc != "" {
		t.Fatalf("rejected transaction reached Mongo: %q", doc)
	}

	// 重新加载：EntityManager 换成按权威状态加载的新实例，之后可以正常写入。
	reloaded := newFullDocRemoteEntity(f.rawID, f.kind)
	f.loader.add(reloaded)
	next, err = f.mgr.PrepareRemoteWriteBatch(ctx, []int64{f.id})
	if err != nil {
		t.Fatalf("reloaded instance not writable: %v", err)
	}
	f.writeB(t, ctx, next, reloaded, remoteTestTxID(0xB2))
	if doc := f.storedDocument(t); doc != "a=;b=second" {
		t.Fatalf("Mongo document=%q, want only the second write %q", doc, "a=;b=second")
	}
	if err = f.mgr.StopFinalizer(ctx); err != nil {
		t.Fatal(err)
	}
	if len(f.mgr.remote.writeSlots) != 0 {
		t.Fatalf("write slots leaked: %d", len(f.mgr.remote.writeSlots))
	}
}

// prepareRejectedWrite 取得写权限并写入第一笔修改 a=rejected，按指定持久级别定稿。
func (f rejectedMemoryFixture) prepareRejectedWrite(t *testing.T, live *fullDocRemoteEntity, tx entity.RemoteTransactionID, durability uint8) entity.RemoteWriteBatch {
	t.Helper()
	batch, err := f.mgr.PrepareRemoteWriteBatch(context.Background(), []int64{f.id})
	if err != nil {
		t.Fatal(err)
	}
	live.set("rejected", "")
	if err = batch.FinalizeLocked(entity.NewRemoteTransactionOutcome(tx, "rejected", "", true, durability)); err != nil {
		t.Fatal(err)
	}
	return batch
}

// Durability 0 从未到达 → finalizer 持久拒绝后，旧实例保持隔离，不能解冻。
// 历史：RR-20260926-28。
func TestRejectedMemoryWriteIsNotCarriedByNextWrite(t *testing.T) {
	f, live := newRejectedMemoryFixture(t, 1901)
	f.storage.unreachable.Store(true)
	batch := f.prepareRejectedWrite(t, live, remoteTestTxID(0xA1), 0)
	if _, err := batch.Commit(context.Background()); !errors.Is(err, entity.ErrRemotePersistenceIndeterminate) {
		t.Fatalf("commit err=%v, want indeterminate", err)
	}
	if err := batch.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	// 下一写者在 gate 上排队，finalizer 拿到持久结论并释放后才得到结果。
	f.assertRejectedWriteNotCarried(t, live)
	if status, err := f.store.CommitStatus(context.Background(), remoteTestTxID(0xA1)); err != nil || status.State != entity.RemoteCommitRejected {
		t.Fatalf("durable status=%+v err=%v, want Rejected", status, err)
	}
}

// Durability 0 结果未知，finalizer 第一次写持久拒绝失败：先回源得 Unknown 并隔离实体、按退避重试，
// 之后写下拒绝。隔离期间与拒绝之后，带着被拒绝修改的旧实例都不能写出。
func TestRejectedMemoryWriteAfterQuarantineIsNotCarriedByNextWrite(t *testing.T) {
	f, live := newRejectedMemoryFixture(t, 1903)
	f.storage.unreachable.Store(true)
	f.storage.rejectWriteFailures.Store(1)
	tx := remoteTestTxID(0xA3)
	batch := f.prepareRejectedWrite(t, live, tx, 0)
	if _, err := batch.Commit(context.Background()); !errors.Is(err, entity.ErrRemotePersistenceIndeterminate) {
		t.Fatalf("commit err=%v, want indeterminate", err)
	}
	if err := batch.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	for range 2 { // 第二次回源开始时，第一轮已看到 Unknown、写拒绝失败并保持隔离
		select {
		case <-f.storage.statusCalls:
		case <-ctx.Done():
			t.Fatal("finalizer did not poll the durable status")
		}
	}
	if got := live.RemoteOwnershipState(); got != entity.RemoteOwnershipQuarantined {
		t.Fatalf("unresolved entity state=%v, want quarantined", got)
	}
	f.assertRejectedWriteNotCarried(t, live)
	if status, err := f.store.CommitStatus(context.Background(), tx); err != nil || status.State != entity.RemoteCommitRejected {
		t.Fatalf("durable status=%+v err=%v, want Rejected after the retried rejection write", status, err)
	}
}

// Durability 0 被权威明确拒绝：Commit 同步回滚并隔离；loader 不能卸载时实例保持隔离到业务重新加载。
func TestDefinitelyRejectedMemoryWriteIsNotCarriedByNextWrite(t *testing.T) {
	f, live := newRejectedMemoryFixture(t, 1904)
	f.storage.reject.Store(true)
	batch := f.prepareRejectedWrite(t, live, remoteTestTxID(0xA4), 0)
	if _, err := batch.Commit(context.Background()); !errors.Is(err, entity.ErrRemoteVersionConflict) || errors.Is(err, entity.ErrRemotePersistenceIndeterminate) {
		t.Fatalf("commit err=%v, want the explicit version conflict without the unknown-outcome sentinel", err)
	}
	if err := batch.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.storage.reject.Store(false)
	f.assertRejectedWriteNotCarried(t, live)
}

// Remote 事务被持久拒绝、实例隔离后，框架在释放 gate 之后对旧实例做“仅内存卸载”（EntityManager.Destroy(deleteFromDB=false)），
// 下一次访问经正式 ManagerAccess 从权威重新加载；实例换代走生产路径，不由测试替换实例。
// 历史：RR-20260926-39。

// reloadableFullDocEntity 在 fullDoc 替身上记录 OnDestroy（EntityManager.Destroy 的正式生命周期回调）。
type reloadableFullDocEntity struct {
	*fullDocRemoteEntity
	destroyed       chan entity.EntityDestroyReason
	destroyedOnFast atomic.Bool
	// duringDestroy 在 OnDestroy 开头调用（实例已离开索引、EntityManager 仍把该 ID 记为 removing），供回归停在卸载过程中。
	duringDestroy func()
	// rollbacks / rollbacksOffFast 记录 RollbackRemoteCommit 的调用次数与其中不在业务 worker 上的次数（OPEN-ITEMS B26）。
	rollbacks, rollbacksOffFast atomic.Int32
}

func (e *reloadableFullDocEntity) OnDestroy(reason entity.EntityDestroyReason) {
	if e.duringDestroy != nil {
		e.duringDestroy()
	}
	e.destroyedOnFast.Store(fctx.InShortWorker())
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
		if err := e.SetEntityVersion(authority.Version); err != nil {
			return nil, err
		}
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
	// 销毁回调（stale.destroyed）先于 EntityManager 清除 removing 标记执行（见 RR-20260926-81 的撤销 / 销毁收尾窗口），
	// 这个窗口内的写得到文档承诺的可重试哨兵 ErrRemoteEntityReloading（RR-20260926-62）。CI 高负载下会落进窗口，
	// 所以按契约有界重试该哨兵；其他任何错误、或窗口在期限内不结束，仍判失败（OPEN-ITEMS B46）。
	next, err := f.mgr.PrepareRemoteWriteBatch(ctx, []int64{f.id})
	for err != nil && errors.Is(err, entity.ErrRemoteEntityReloading) && ctx.Err() == nil {
		time.Sleep(time.Millisecond)
		next, err = f.mgr.PrepareRemoteWriteBatch(ctx, []int64{f.id})
	}
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

// 正式装配：Remote Manager 交给 Nest，NewEngine 把 NestMgr.RunLocal 绑定给它（nest.LocalExecutorBinder，与 DataEngine
// 驱逐同一接线）。finalizer 拿到持久拒绝后，仅内存卸载在 Nest 业务池执行（不在 finalizer goroutine 上取实体锁）。
// Sync 与 DataEngine 驱逐同一规则：卸载关闭旧同步状态（被拒绝的内容不交付、也不发 remove，订阅保持），
// 重载后的实例重新接上（kit 在 OnEntityLoaded 上调 Rebind；这里用等价的 Register 路径）时原订阅者收到全量。
func TestRejectedWriteUnloadsOnNestShortPoolAndRebindsSync(t *testing.T) {
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
	engine := nest.NewEngine(nest.NestOptionWithGetter(f.access), nest.NestOptionWithRemoteEntityManager(f.mgr), nest.NestOptionWithEntitySync(syncMgr), nest.NestOptionWithWorkerPools(nest.WorkerPoolConfig{Workers: 1, QueueCap: 16}, nest.WorkerPoolConfig{}, nest.WorkerPoolConfig{}))
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
		t.Fatal("rejected instance was unloaded outside the Nest business pool")
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

// 持久拒绝收尾（回滚 + 隔离）释放 gate 之后、仅内存卸载完成之前，下一写者碰到的是“正在重载”的可重试窗口：得到可 errors.Is 的
// entity.ErrRemoteEntityReloading，同时仍满足 errors.Is(err, ErrRemoteFenced)；窗口结束后重试得到从权威重载的新实例。
// 历史：RR-20260926-62。

func assertReloadingSentinel(t *testing.T, err error, where string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: next writer admitted while the rejected instance is still being reloaded", where)
	}
	if !errors.Is(err, entity.ErrRemoteEntityReloading) {
		t.Fatalf("%s: next writer err=%v, want the retryable entity.ErrRemoteEntityReloading (callers cannot tell this window from a real fence)", where, err)
	}
	if !errors.Is(err, entity.ErrRemoteFenced) {
		t.Fatalf("%s: err=%v no longer satisfies errors.Is(ErrRemoteFenced)", where, err)
	}
}

func probeNextWriter(t *testing.T, f reloadFixture) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	next, err := f.mgr.PrepareRemoteWriteBatch(ctx, []int64{f.id})
	if err == nil {
		_ = next.Abort(context.Background(), errors.New("probe"))
		_ = next.Close(context.Background())
	}
	return err
}

// finalizer 路径（Durability 0 从未到达 → 持久拒绝）：本地执行入口先放行回滚，再把卸载停住；gate 已释放、旧实例仍隔离在内存。
func TestRejectedWriteBeforeUnloadReturnsReloadingSentinel(t *testing.T) {
	f, live := newReloadFixture(t, 1981)
	var calls atomic.Int32
	unloadHeld, resumeUnload := make(chan struct{}), make(chan struct{})
	f.mgr.BindLocalExecutor(func(fn func()) error {
		// 第 1 次是拒绝收尾的回滚（settleRejectedRemoteEntries），第 2 次是 gate 释放之后的仅内存卸载。
		if calls.Add(1) == 2 {
			close(unloadHeld)
			<-resumeUnload
		}
		fn()
		return nil
	})
	f.storage.unreachable.Store(true)
	batch := f.prepareRejected(t, live, remoteTestTxID(0xE1), 0)
	if _, err := batch.Commit(context.Background()); !errors.Is(err, entity.ErrRemotePersistenceIndeterminate) {
		t.Fatalf("commit err=%v, want indeterminate", err)
	}
	if err := batch.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-unloadHeld:
	case <-time.After(5 * time.Second):
		t.Fatal("finalizer never reached the unload of the rejected instance")
	}
	assertReloadingSentinel(t, probeNextWriter(t, f), "gate released, unload pending")
	close(resumeUnload)
	f.assertReloadedFromAuthority(t, live)
}

// batch.Close 路径（Durability 0 被权威明确拒绝）：消息收尾里的卸载被业务池拒绝投递，交给 finalizer 重试；重试前的窗口同样可重试。
func TestDefinitelyRejectedWriteBeforeUnloadReturnsReloadingSentinel(t *testing.T) {
	f, live := newReloadFixture(t, 1982)
	var paused atomic.Bool
	paused.Store(true)
	refuse := func(fn func()) error {
		if paused.Load() {
			return errors.New("test: business pool paused")
		}
		fn()
		return nil
	}
	f.mgr.BindLocalExecutor(refuse)
	f.storage.reject.Store(true)
	batch := f.prepareRejected(t, live, remoteTestTxID(0xE2), 0)
	if _, err := batch.Commit(context.Background()); !errors.Is(err, entity.ErrRemoteVersionConflict) {
		t.Fatalf("commit err=%v, want version conflict", err)
	}
	if err := batch.Close(entity.WithLocalExecutor(context.Background(), refuse)); err != nil {
		t.Fatal(err)
	}
	f.storage.reject.Store(false)
	assertReloadingSentinel(t, probeNextWriter(t, f), "definite rejection, unload pending")
	paused.Store(false)
	f.assertReloadedFromAuthority(t, live)
}

// 卸载过程中（Destroy 已把实例移出索引、生命周期回调尚未结束）发起的重载被 EntityManager 以“being removed”拒绝：
// 下一写者得到同一可重试哨兵（原错误仍可 errors.Is(entity.ErrEntityRemoved)），而不是通用的加载失败。
func TestReloadDuringUnloadReturnsReloadingSentinel(t *testing.T) {
	f, live := newReloadFixture(t, 1983)
	destroying, resume := make(chan struct{}), make(chan struct{})
	live.duringDestroy = func() {
		close(destroying)
		<-resume
	}
	f.mgr.BindLocalExecutor(func(fn func()) error { fn(); return nil })
	f.storage.unreachable.Store(true)
	batch := f.prepareRejected(t, live, remoteTestTxID(0xE3), 0)
	if _, err := batch.Commit(context.Background()); !errors.Is(err, entity.ErrRemotePersistenceIndeterminate) {
		t.Fatalf("commit err=%v, want indeterminate", err)
	}
	if err := batch.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-destroying:
	case <-time.After(5 * time.Second):
		t.Fatal("rejected instance was never unloaded")
	}
	f.storage.unreachable.Store(false)
	err := probeNextWriter(t, f)
	assertReloadingSentinel(t, err, "reload while the rejected instance is being destroyed")
	if !errors.Is(err, entity.ErrEntityRemoved) {
		t.Fatalf("err=%v lost the underlying entity.ErrEntityRemoved", err)
	}
	close(resume)
	f.assertReloadedFromAuthority(t, live)
}

// 持久拒绝后仅内存卸载时 subject 仍有订阅者：框架在业务池之外从权威重载实例并 Rebind，订阅者下一帧收到同一对象的
// 权威全量（ObjectUpdate + Full）；无订阅者时不主动重载。
// 历史：RR-20260926-59（原以 Durability 1 推测性确认后被投影器拒绝为场景；WAL 级拒绝已移除（2026-10-14），
// 改由 Durability 0 的 finalizer 持久拒绝驱动同一卸载 / 重载路径）。

type resyncSubscriber struct {
	sync   *entitysync.Manager
	frames chan []byte
	ref    frame.ObjectRef
	// decided 收到卸载时“是否为订阅者重载”的判定（Unload 登记重载前查询的结果），让无订阅者用例不必猜时序。
	decided chan bool
}

// observedSync 转发给 entitysync.Manager，并把 SubjectAwaitsReload 的结果交给测试。
type observedSync struct {
	*entitysync.Manager
	decided chan bool
}

func (o observedSync) SubjectAwaitsReload(id int64) bool {
	awaits := o.Manager.SubjectAwaitsReload(id)
	select {
	case o.decided <- awaits:
	default:
	}
	return awaits
}

func enableFullDocSync(e *reloadableFullDocEntity) {
	packed := func(entity.SyncProfile) (entity.FrozenSyncPayload, error) {
		e.mu.Lock()
		defer e.mu.Unlock()
		return entity.CopyFrozenSyncPayload(1, []byte("a="+e.a)), nil
	}
	e.EnableSync(entity.EntitySyncCreateParam{Enabled: true, EntityID: e.ID(), Namespace: "test", Packer: entity.SubjectSyncPackFunc{
		Snapshot: packed, Delta: func(p entity.SyncProfile, _ uint64) (entity.FrozenSyncPayload, error) { return packed(p) },
	}})
}

// newResyncFixture：正式 ManagerAccess 作 Remote loader，loader 按权威构造实例并在发布前启用同步（与生成工厂相同）；
// entitysync.Manager 经 ManagerAccess.ConfigureUnloadResync 接上卸载后重载（kit 在 Nest Mod 启动时做同样的接线）。
func newResyncFixture(t *testing.T, rawID int64, subscribe bool) (reloadFixture, *reloadableFullDocEntity, *resyncSubscriber) {
	t.Helper()
	f, live := newReloadFixture(t, rawID)
	f.loader.prepare = enableFullDocSync
	enableFullDocSync(live)
	sub := &resyncSubscriber{frames: make(chan []byte, 16), decided: make(chan bool, 16)}
	var err error
	sub.sync, err = entitysync.NewManager(entitysync.ManagerConfig{Mode: entitysync.ModePeriodic, Interval: time.Hour, Transport: entitysync.TransportFunc(func(_ context.Context, _ entitysync.SessionID, p []byte) error {
		sub.frames <- append([]byte(nil), p...)
		return nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sub.sync.Close(context.Background()) })
	stop, err := f.access.ConfigureUnloadResync(observedSync{Manager: sub.sync, decided: sub.decided}, entity.UnloadResyncConfig{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stop(context.Background()) })
	if err = sub.sync.Register(live.Sync()); err != nil {
		t.Fatal(err)
	}
	if !subscribe {
		return f, live, sub
	}
	if err = sub.sync.OpenSession(1); err != nil {
		t.Fatal(err)
	}
	if err = sub.sync.Subscribe(1, live.ID(), entity.SyncProfile{}); err != nil {
		t.Fatal(err)
	}
	created := sub.next(t, time.Second, "the initial create")
	if len(created.Objects) != 1 || created.Objects[0].Operation != frame.ObjectCreate {
		t.Fatalf("initial frame=%+v", created.Objects)
	}
	sub.ref = created.Objects[0].Ref
	return f, live, sub
}

// next 驱动 Flush 直到收到一帧（重载在后台 worker 上完成，这里只等事件发生，不靠固定睡眠判定结果）。
func (sub *resyncSubscriber) next(t *testing.T, timeout time.Duration, what string) frame.Frame {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		select {
		case raw := <-sub.frames:
			decoded, err := entitysync.DecodeFrame(raw, frame.DefaultLimits())
			if err != nil {
				t.Fatal(err)
			}
			return decoded
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("no frame within %v: %s", timeout, what)
		}
		if err := sub.sync.Flush(context.Background()); err != nil {
			t.Fatal(err)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func fullPayload(t *testing.T, object frame.ObjectDelta) (string, bool) {
	t.Helper()
	if len(object.Components) != 1 {
		t.Fatalf("object carries %d components", len(object.Components))
	}
	update, err := entitysync.DecodeSubjectUpdate(object.Components[0].Data, 0)
	if err != nil {
		t.Fatal(err)
	}
	return string(update.Payload.BytesCopy()), update.Full
}

func TestRejectedWriteResyncsSubscribersFromAuthority(t *testing.T) {
	f, live, sub := newResyncFixture(t, 1961, true)
	tx := remoteTestTxID(0xD1)
	f.storage.unreachable.Store(true)
	batch := f.prepareRejected(t, live, tx, 0) // 内存 a=rejected
	live.MarkSyncDirty(1)
	if _, err := batch.Commit(context.Background()); !errors.Is(err, entity.ErrRemotePersistenceIndeterminate) {
		t.Fatalf("commit err=%v, want indeterminate (the finalizer rejects it)", err)
	}
	// 本 fixture 没有 Nest 的 Sync 门：订阅者先收到被拒绝的内容，用来证明卸载后的全量确实把它纠正回权威状态。
	held := sub.next(t, time.Second, "the frame carrying the rejected content")
	if payload, _ := fullPayload(t, held.Objects[0]); payload != "a=rejected" {
		t.Fatalf("premise: subscriber frame=%q, want a=rejected", payload)
	}
	if err := batch.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-live.destroyed:
	case <-time.After(3 * time.Second):
		t.Fatal("rejected instance was not unloaded")
	}
	if doc := f.storedDocument(t); doc != "" {
		t.Fatalf("authority doc=%q, want none", doc)
	}
	correction := sub.next(t, 3*time.Second, "authority rejected the transaction and the instance was unloaded, but the subscriber still holds a=rejected (no authoritative full, no remove)")
	if len(correction.Objects) != 1 || correction.Objects[0].Operation != frame.ObjectUpdate || correction.Objects[0].Ref != sub.ref {
		t.Fatalf("correction frame=%+v, want one ObjectUpdate of the held object %+v", correction.Objects, sub.ref)
	}
	if payload, full := fullPayload(t, correction.Objects[0]); payload != "a=" || !full {
		t.Fatalf("subscriber got %q (full=%v) after the rejection, want the authority full state %q", payload, full, "a=")
	}
	fresh := f.access.LookupLocalRemoteEntity(f.id, entity.EntityKindNone)
	if fresh == nil || fresh == entity.IThreadSafeRemoteEntity(live) {
		t.Fatal("authority instance is not resident after the resync")
	}
	if stats := f.access.UnloadResyncStats(); stats.Reloaded != 1 || stats.Retracted != 0 {
		t.Fatalf("resync stats=%+v", stats)
	}
}

func TestRejectedWriteWithoutSubscribersIsNotReloaded(t *testing.T) {
	f, live, sub := newResyncFixture(t, 1962, false)
	tx := remoteTestTxID(0xD2)
	f.storage.unreachable.Store(true)
	batch := f.prepareRejected(t, live, tx, 0)
	if _, err := batch.Commit(context.Background()); !errors.Is(err, entity.ErrRemotePersistenceIndeterminate) {
		t.Fatalf("commit err=%v, want indeterminate (the finalizer rejects it)", err)
	}
	if err := batch.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-live.destroyed:
	case <-time.After(3 * time.Second):
		t.Fatal("rejected instance was not unloaded")
	}
	select {
	case awaits := <-sub.decided:
		if awaits {
			t.Fatal("unload of an entity without subscribers decided to reload it")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("unload never consulted the sync side")
	}
	loads := f.loader.loads.Load()
	if stats := f.access.UnloadResyncStats(); stats.Scheduled != 0 {
		t.Fatalf("unload without subscribers scheduled a reload: %+v", stats)
	}
	if f.access.LookupLocalRemoteEntity(f.id, entity.EntityKindNone) != nil || f.loader.loads.Load() != loads {
		t.Fatal("entity without subscribers was reloaded proactively")
	}
}
