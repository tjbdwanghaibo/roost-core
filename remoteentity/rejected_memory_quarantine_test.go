package remoteentity

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/entity"
)

// fullDocRemoteEntity 与生成实体的持久化形态一致：BuildRemoteCommitLocked 不改动内存、
// 把整份当前内存状态编码为整文档（生成 DAO 的 MarshalPersist 忽略 mask，写入是 ReplaceOne），
// RollbackRemoteCommit 是 no-op。被拒绝事务的内存修改因此会留在实体里。
type fullDocRemoteEntity struct {
	*testRemoteEntity
	mu   sync.Mutex
	a, b string
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
	e.mu.Unlock()
	return entity.RemoteCommit{
		Schema: 1, Codec: 1,
		Mutations: []entity.RemoteDataMutation{{Collection: fullDocCollection, ID: e.GUId(), Version: lease.BaseVersion + 1, Mask: 1, Data: data}},
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
	statusCalls chan struct{}
}

func (s *switchableStorage) CommitStatus(ctx context.Context, id entity.RemoteTransactionID) (entity.RemoteCommitStatus, error) {
	select {
	case s.statusCalls <- struct{}{}:
	default:
	}
	return s.MongoCommitter.CommitStatus(ctx, id)
}

func (s *switchableStorage) CommitRemoteBatch(ctx context.Context, commits []entity.RemoteCommit) ([]entity.RemoteCommitReceipt, error) {
	if s.unreachable.Load() {
		return nil, context.DeadlineExceeded
	}
	return s.MongoCommitter.CommitRemoteBatch(ctx, commits)
}

func (s *switchableStorage) CommitRemote(ctx context.Context, commit entity.RemoteCommit) (entity.RemoteCommitReceipt, error) {
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

// RR-20260926-28 复核：Durability 0 从未到达 → finalizer 持久拒绝后，旧实例不能解冻。
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

// 既有 Rejected 分支（Durability 1，投影器持久拒绝）：finalizer 第一次回源就读到 Rejected，
// 实体此前没有被隔离——修复前这里会带着被拒绝修改继续写。
func TestRejectedAsyncWriteIsNotCarriedByNextWrite(t *testing.T) {
	f, live := newRejectedMemoryFixture(t, 1902)
	tx := remoteTestTxID(0xA2)
	batch := f.prepareRejectedWrite(t, live, tx, 1)
	if _, err := batch.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.mgr.RejectRemoteTransaction(tx, "lease expired")
	if err := batch.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.assertRejectedWriteNotCarried(t, live)
}

// Durability 1：finalizer 先看到 Unknown（实体被隔离），之后投影器持久拒绝。
func TestRejectedAsyncWriteAfterQuarantineIsNotCarriedByNextWrite(t *testing.T) {
	f, live := newRejectedMemoryFixture(t, 1903)
	tx := remoteTestTxID(0xA3)
	batch := f.prepareRejectedWrite(t, live, tx, 1)
	if _, err := batch.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := batch.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	for range 2 { // 第二次回源开始时，第一轮已看到 Unknown 并完成隔离
		select {
		case <-f.storage.statusCalls:
		case <-ctx.Done():
			t.Fatal("finalizer did not poll the durable status")
		}
	}
	if got := live.RemoteOwnershipState(); got != entity.RemoteOwnershipQuarantined {
		t.Fatalf("unresolved entity state=%v, want quarantined", got)
	}
	f.mgr.RejectRemoteTransaction(tx, "lease expired")
	f.assertRejectedWriteNotCarried(t, live)
}

// Durability 2：提交等待拿到拒绝，转交 finalizer 收尾。
func TestRejectedStrictWriteIsNotCarriedByNextWrite(t *testing.T) {
	f, live := newRejectedMemoryFixture(t, 1904)
	tx := remoteTestTxID(0xA4)
	batch := f.prepareRejectedWrite(t, live, tx, 2)
	f.mgr.RejectRemoteTransaction(tx, "lease expired")
	if _, err := batch.Commit(context.Background()); !errors.Is(err, entity.ErrRemoteRejected) {
		t.Fatalf("strict commit err=%v, want rejected", err)
	}
	if err := batch.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.assertRejectedWriteNotCarried(t, live)
}
