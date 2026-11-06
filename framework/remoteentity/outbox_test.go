// Outbox 单发布者：恢复分页不饿死后续事务、独立事务并行发布、停机前排空显式恢复、发布失败不阻塞投影、并发确认幂等。

package remoteentity

import (
	"context"
	"encoding/binary"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	coredata "github.com/tjbdwanghaibo/roost-core/framework/dataengine"
	"github.com/tjbdwanghaibo/roost-core/framework/dataengine/engine"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/nest"
	"github.com/tjbdwanghaibo/roost-core/framework/nestwal"
	fmongo "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo"
	"github.com/tjbdwanghaibo/roost-core/internal/stopcontract"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// 恢复入口没有写者重投帮忙：一整页历史坏记录不能饿死后面的正常事务。
func TestOutboxFailedFirstPageDoesNotStarveLaterTransaction(t *testing.T) {
	fixture, _, batch := prepareReplayBatch(t, nil)
	defer batch.Abort(context.Background(), nil)
	defer batch.Close(context.Background())
	defer fixture.StopFinalizer(context.Background())
	seed := batch.Commits()[0]
	db := newRemoteMongoFake()
	storage := newBoundMongoCommitter(db, "outbox_pages", 1000, 0)
	backend, err := NewBackend(newRemoteTestLoader(), storage)
	if err != nil {
		t.Fatal(err)
	}
	manager := NewManager(newMockVersionedLockFactory(), DefaultConfig(), 1000)
	manager.SetBackend(backend)
	defer manager.StopFinalizer(context.Background())
	// 同一毫秒创建，必须用 _id 作为稳定的第二排序键。
	created := time.UnixMilli(1000)
	var last entity.RemoteTransactionID
	for i := 1; i <= 257; i++ {
		var id entity.RemoteTransactionID
		binary.BigEndian.PutUint64(id[8:], uint64(i))
		commit := seed.Clone()
		commit.TransactionID = id
		receipt := mongoRemoteReceipt{TransactionID: id[:], EntityID: commit.EntityID, StateVersion: commit.NextVersion, MarkerEpoch: commit.MarkerEpoch, RouteEpoch: commit.RouteEpoch, LockFence: commit.LockFence}
		doc := mongoRemoteTransaction{ID: id.String(), State: uint8(entity.RemoteCommitApplied), CreatedAt: created, Receipts: []mongoRemoteReceipt{receipt}}
		if i == 257 {
			doc.Commits = []entity.RemoteCommit{commit}
			last = id
		}
		if _, err := db.Collection("outbox_pages", remoteTxCollection).InsertOne(context.Background(), doc); err != nil {
			t.Fatal(err)
		}
	}
	if err := manager.RecoverOutbox(context.Background()); err == nil {
		t.Fatal("corrupt pending records were hidden")
	}
	status, err := storage.CommitStatus(context.Background(), last)
	if err != nil || status.State != entity.RemoteCommitCommitted {
		t.Fatalf("later transaction starved: state=%v err=%v", status.State, err)
	}
	pending, err := storage.PendingRemoteCommits(context.Background(), 1000)
	if err != nil || len(pending) != 256 {
		t.Fatalf("failed records must remain pending: count=%d err=%v", len(pending), err)
	}
}

type parallelOutboxBackend struct {
	*remoteTestLoader
	statuses []entity.RemoteCommitStatus
	started  chan entity.RemoteTransactionID
	release  chan struct{}
	first    entity.RemoteTransactionID
	follower entity.RemoteTransactionID
	finished atomic.Bool
	blockAll bool
	mu       sync.Mutex
	marked   map[entity.RemoteTransactionID]bool
}

func (b *parallelOutboxBackend) PendingRemoteCommits(context.Context, int) ([]entity.RemoteCommitStatus, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var result []entity.RemoteCommitStatus
	for _, status := range b.statuses {
		if !b.marked[status.TransactionID] {
			result = append(result, status.Clone())
		}
	}
	return result, nil
}

func (b *parallelOutboxBackend) MarkRemoteCommitPublished(ctx context.Context, id entity.RemoteTransactionID) error {
	b.started <- id
	if id == b.first || b.blockAll {
		select {
		case <-b.release:
			b.finished.Store(true)
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if id == b.follower && !b.finished.Load() {
		return errors.New("overlapping transaction overtook its predecessor")
	}
	b.mu.Lock()
	b.marked[id] = true
	b.mu.Unlock()
	return nil
}

func newParallelOutboxBackend(t *testing.T, entities [][]int64) *parallelOutboxBackend {
	t.Helper()
	fixture, _, batch := prepareReplayBatch(t, nil)
	t.Cleanup(func() {
		_ = batch.Abort(context.Background(), nil)
		_ = batch.Close(context.Background())
		_ = fixture.StopFinalizer(context.Background())
	})
	seed := batch.Commits()[0]
	backend := &parallelOutboxBackend{remoteTestLoader: newRemoteTestLoader(), started: make(chan entity.RemoteTransactionID, 8), release: make(chan struct{}), marked: make(map[entity.RemoteTransactionID]bool)}
	for i, ids := range entities {
		tx := remoteTestTxID(byte(201 + i))
		status := entity.RemoteCommitStatus{TransactionID: tx, State: entity.RemoteCommitApplied}
		for _, id := range ids {
			commit := seed.Clone()
			commit.TransactionID, commit.EntityID = tx, id
			commit.Snapshots, commit.Invalidations = nil, nil
			status.Commits = append(status.Commits, commit)
			status.Receipts = append(status.Receipts, entity.RemoteCommitReceipt{TransactionID: tx, EntityID: id, StateVersion: commit.NextVersion, MarkerEpoch: commit.MarkerEpoch, RouteEpoch: commit.RouteEpoch, LockFence: commit.LockFence})
		}
		backend.statuses = append(backend.statuses, status)
	}
	return backend
}

func TestOutboxIndependentTransactionsPublishWhileOverlappingTransactionWaits(t *testing.T) {
	backend := newParallelOutboxBackend(t, [][]int64{{10, 20}, {30, 20}, {40, 50}})
	backend.first, backend.follower = backend.statuses[0].TransactionID, backend.statuses[1].TransactionID
	independent := backend.statuses[2].TransactionID
	manager := NewManager(newMockVersionedLockFactory(), DefaultConfig(), 1000)
	manager.SetBackend(backend)
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	finished := make(chan struct{})
	var release sync.Once
	t.Cleanup(func() {
		release.Do(func() { close(backend.release) })
		cancel()
		<-finished
		_ = manager.StopFinalizer(context.Background())
	})
	go func() {
		defer close(finished)
		result <- manager.RecoverOutbox(ctx)
	}()
	seen := make(map[entity.RemoteTransactionID]bool)
	for !seen[backend.first] || !seen[independent] {
		select {
		case id := <-backend.started:
			if id == backend.follower {
				t.Fatal("shared second Entity did not preserve publication order")
			}
			seen[id] = true
		case err := <-result:
			t.Fatalf("outbox returned before publishing independent transaction: %v", err)
		case <-time.After(time.Second):
			t.Fatal("independent transaction was blocked behind another transaction's publication I/O")
		}
	}
	release.Do(func() { close(backend.release) })
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("outbox did not drain after releasing publication")
	}
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if len(backend.marked) != 3 {
		t.Fatalf("marked transactions = %d, want 3", len(backend.marked))
	}
}

// 全部发布都阻塞时，配置上限必须生效；取消后不能派发剩余事务或误写Committed。
func TestOutboxWorkerLimitAndCancellationRetainUnpublishedTransactions(t *testing.T) {
	backend := newParallelOutboxBackend(t, [][]int64{{10}, {20}, {30}, {40}})
	backend.blockAll = true
	cfg := DefaultConfig()
	cfg.OutboxPublishWorkers = 2
	manager := NewManager(newMockVersionedLockFactory(), cfg, 1000)
	manager.SetBackend(backend)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	finished := make(chan struct{})
	t.Cleanup(func() { cancel(); <-finished; _ = manager.StopFinalizer(context.Background()) })
	go func() { defer close(finished); done <- manager.RecoverOutbox(ctx) }()
	for range 2 {
		select {
		case <-backend.started:
		case <-time.After(time.Second):
			t.Fatal("workers did not start")
		}
	}
	select {
	case <-backend.started:
		t.Fatal("publication exceeded configured worker count")
	case <-time.After(25 * time.Millisecond):
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel result: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled publisher did not drain")
	}
	select {
	case <-backend.started:
		t.Fatal("queued transaction started after cancellation")
	default:
	}
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if len(backend.marked) != 0 {
		t.Fatal("canceled transactions were marked published")
	}
}

type heldOutboxScan struct {
	*remoteTestLoader
	entered chan struct{}
	release chan struct{}
}

func (s *heldOutboxScan) PendingRemoteCommits(context.Context, int) ([]entity.RemoteCommitStatus, error) {
	close(s.entered)
	<-s.release // 有意不配合取消，验证停止不能提前交还依赖。
	return nil, nil
}

func TestExplicitOutboxRecoveryIsDrainedBeforeStop(t *testing.T) {
	manager := NewManager(newMockVersionedLockFactory(), DefaultConfig(), 1000)
	storage := &heldOutboxScan{remoteTestLoader: newRemoteTestLoader(), entered: make(chan struct{}), release: make(chan struct{})}
	manager.SetBackend(storage)
	done := make(chan error, 1)
	stop, released := stopcontract.CallerReleases(manager.StopFinalizer)
	stopcontract.Check(t, stopcontract.Hooks{
		Block: func(t testing.TB) {
			go func() { done <- manager.RecoverOutbox(context.Background()) }()
			select {
			case <-storage.entered:
			case <-time.After(3 * time.Second):
				t.Fatal("recovery did not enter storage")
			}
		},
		Stop:     stop,
		Release:  func() { close(storage.release) },
		Released: released,
	})
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := manager.RecoverOutbox(context.Background()); !errors.Is(err, ErrAssemblyStopped) {
		t.Fatalf("recovery after stop: %v", err)
	}
}

// Remote 事务的发布失败不挡 owner 的 WAL 投影：Mongo 已是 Applied 的记录记为已投影，后面的记录（包括普通 DAO）照常投影；
// 发布由 owner 自己的补发循环（与启动时的 RecoverOutbox 同一路径）在恢复后补上——即使这个事务没有写者在等，也不需要等到下次重启。
// 历史：RR-20261006-69。
type failingPublicationStorage struct {
	*MongoCommitter
	target entity.RemoteTransactionID
	fail   atomic.Bool
	marks  atomic.Int32
}

var errInjectedPublication = errors.New("injected publication failure")

func (s *failingPublicationStorage) MarkRemoteCommitPublished(ctx context.Context, id entity.RemoteTransactionID) error {
	if id == s.target {
		s.marks.Add(1)
		if s.fail.Load() {
			return errInjectedPublication
		}
	}
	return s.MongoCommitter.MarkRemoteCommitPublished(ctx, id)
}

func TestPublicationFailureDoesNotBlockLaterProjection(t *testing.T) {
	const kind entity.EntityKind = 123
	entity.MustRegisterEntityKindDefs(entity.EntityKindDef{Kind: kind, Category: 1, RemotePolicy: entity.RemotePolicyManaged})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	mongo := newRemoteMongoFake()
	committer := newBoundMongoCommitter(mongo, "control", 1000, 0)
	tx1 := remoteTestTxID(168)
	storage := &failingPublicationStorage{MongoCommitter: committer, target: tx1}
	storage.fail.Store(true)
	loader := newRemoteTestLoader()
	live := newTestRemoteEntity(6811, 1, kind)
	loader.add(live)
	backend, err := NewBackend(loader, storage)
	if err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig()
	cfg.FinalizeRetryInterval = 10 * time.Millisecond
	mgr := NewManager(newMockVersionedLockFactory(), cfg, 1000)
	mgr.SetBackend(backend)
	mgr.SetOwnershipStore(committer)
	t.Cleanup(func() {
		stop, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = mgr.StopFinalizer(stop)
	})
	store, err := engine.NewMongoStore(mongo, engine.MongoStoreConfig{Databases: committer.databases.Load(), DefaultDatabase: "game", ServerID: 1000, TransactionReceiptTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if err = store.SetRemoteProjection(committer, mgr); err != nil {
		t.Fatal(err)
	}
	walOptions := nestwal.DefaultOptions(t.TempDir())
	walOptions.GroupCommitInterval = time.Millisecond
	wal, err := nestwal.Open(walOptions)
	if err != nil {
		t.Fatal(err)
	}
	projector, err := engine.NewProjector(wal, store, engine.ProjectorOptions{
		CloseWAL: true, IdlePoll: 10 * time.Millisecond, RetryMin: 5 * time.Millisecond, RetryMax: 50 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		closing, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = projector.Close(closing)
	})

	// tx1：Remote 记录进 WAL；写者不 Close（模拟重放：没有 finalizer 项在等它）。
	batch1, err := mgr.PrepareRemoteWriteBatch(ctx, []int64{live.GUId()})
	if err != nil {
		t.Fatal(err)
	}
	live.dirty.set(true)
	if err = batch1.FinalizeLocked(entity.NewRemoteTransactionOutcome(tx1, "remote-hol", "", true, uint8(nest.DurabilityAsync.Record()))); err != nil {
		t.Fatal(err)
	}
	if err = projector.Commit(ctx, remoteCommitRecord(nest.DurabilityAsync.Record(), batch1.Commits())); err != nil {
		t.Fatal(err)
	}
	if _, err = batch1.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	projector.TransactionReleased(coredata.TransactionID(tx1))

	// 普通 DAO 记录排在 tx1 之后。
	var plainID coredata.TransactionID
	plainID[0], plainID[15] = 0x68, 2
	payload, _ := bson.Marshal(bson.M{"_id": int64(6812), "value": int32(1)})
	plain := coredata.CommitRecord{ID: plainID, Durability: nest.DurabilityStrict.Record(), Mutations: []coredata.Mutation{{
		Key:  coredata.DocumentKey{Database: "game", Resource: "rr68_plain", ID: 6812},
		Kind: coredata.MutationPut, NextVersion: 1, Data: payload,
	}}}
	if err = projector.Commit(ctx, plain); err != nil {
		t.Fatal(err)
	}
	projector.TransactionReleased(plainID)

	deadline := time.Now().Add(2 * time.Second)
	for projector.Stats().WALUnacked != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if stats := projector.Stats(); stats.WALUnacked != 0 {
		t.Fatalf("a failing Remote publication blocked the projection of later records: stats=%+v publish attempts=%d", stats, storage.marks.Load())
	}
	if status, err := committer.CommitStatus(ctx, tx1); err != nil || status.State != entity.RemoteCommitApplied {
		t.Fatalf("tx1 durable status=%+v err=%v, want Applied (persisted, publication pending)", status, err)
	}

	// 总线恢复：补发循环发布 tx1，持久状态变为 Committed——没有写者 Close，也没有重启。
	storage.fail.Store(false)
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if status, err := committer.CommitStatus(ctx, tx1); err == nil && status.State == entity.RemoteCommitCommitted {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if status, err := committer.CommitStatus(ctx, tx1); err != nil || status.State != entity.RemoteCommitCommitted {
		t.Fatalf("tx1 was never published after the bus recovered: status=%+v err=%v", status, err)
	}
	if stats := projector.Stats(); stats.FatalProjectionConflicts != 0 {
		t.Fatalf("projector stats=%+v", stats)
	}
	if err = batch1.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if err = mgr.FlushRemoteTransaction(ctx, tx1); err != nil {
		t.Fatalf("tx1 after recovery: %v", err)
	}
}

// flakyPublicationStorage 让下一次 CommitRemote（投影器发布路径上的重复提交）返回瞬时错误，模拟“投影器结果未知”。
type flakyPublicationStorage struct {
	*MongoCommitter
	failNextCommit atomic.Bool
}

func (s *flakyPublicationStorage) CommitRemote(ctx context.Context, commit entity.RemoteCommit) (entity.RemoteCommitReceipt, error) {
	if s.failNextCommit.CompareAndSwap(true, false) {
		return entity.RemoteCommitReceipt{}, errors.New("injected transient publication failure")
	}
	return s.MongoCommitter.CommitRemote(ctx, commit)
}

// 投影器报告“结果未知”后 finalizer 回源并自行发布，投影器对同一事务的重试同时走 afterRemoteCommit → AcknowledgeRemoteCommit：
// 两条路径并发确认同一提交是设计允许的，所以 AcknowledgeRemoteCommit 必须幂等且并发安全。以 -race 运行：参与者实现不得有
// 数据竞争，两条路径都结束后写额度归还、实体确认到本提交的版本。
// 历史：RR-20260926-63。
func TestProjectorRetryAndFinalizerAcknowledgeConcurrently(t *testing.T) {
	const kind entity.EntityKind = 125
	entity.MustRegisterEntityKindDefs(entity.EntityKindDef{Kind: kind, Category: 1, RemotePolicy: entity.RemotePolicyManaged})
	for i := 0; i < 200; i++ {
		store := newBoundMongoCommitter(newRemoteMongoFake(), "control", 1000, 0)
		storage := &flakyPublicationStorage{MongoCommitter: store}
		loader := newRemoteTestLoader()
		live := newTestRemoteEntity(int64(3000+i), 1, kind)
		loader.add(live)
		backend, err := NewBackend(loader, storage)
		if err != nil {
			t.Fatal(err)
		}
		cfg := DefaultConfig()
		cfg.FinalizeRetryInterval = time.Millisecond
		mgr := NewManager(newMockVersionedLockFactory(), cfg, 1000)
		mgr.SetBackend(backend)
		mgr.SetOwnershipStore(store)
		mgr.SetSyncer(&countingSnapshotSyncer{})
		batch, err := mgr.PrepareRemoteWriteBatch(context.Background(), []int64{live.GUId()})
		if err != nil {
			t.Fatal(err)
		}
		live.dirty.set(true)
		tx := remoteTestTxID(byte(i%250 + 1))
		if err = batch.FinalizeLocked(entity.NewRemoteTransactionOutcome(tx, "async", "", true, 1)); err != nil {
			t.Fatal(err)
		}
		commits := batch.Commits()
		if _, err = batch.Commit(context.Background()); err != nil {
			t.Fatal(err)
		}
		if err = batch.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
		// 投影器第 1 次：Mongo 事务已提交，发布报告未知 → tracker Indeterminate，finalizer 随即回源并发布。
		if _, err = store.CommitRemoteBatch(context.Background(), commits); err != nil {
			t.Fatal(err)
		}
		storage.failNextCommit.Store(true)
		_, _ = mgr.ApplyRemoteCommits(context.Background(), tx, commits)
		// 投影器第 2 次与 finalizer 的回源发布并发确认同一提交。
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = mgr.ApplyRemoteCommits(context.Background(), tx, commits)
		}()
		deadline := time.Now().Add(3 * time.Second)
		for len(mgr.remote.writeSlots) != 0 && time.Now().Before(deadline) {
			time.Sleep(50 * time.Microsecond)
		}
		wg.Wait()
		if slots := len(mgr.remote.writeSlots); slots != 0 {
			t.Fatalf("iteration %d: write slots=%d after both acknowledgement paths finished", i, slots)
		}
		if got, want := live.RemoteVersionVector().StateVersion, commits[0].NextVersion; got != want {
			t.Fatalf("iteration %d: entity acknowledged at version %d, want %d", i, got, want)
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_ = mgr.StopFinalizer(ctx)
		cancel()
	}
}

// ---- outbox 发布归属：写者自己先发，别的进程过了宽限期才代发 ----

// gatedOutboxStorage 让测试决定一个进程的 outbox 协调者何时读页、标记前做什么，把“别的进程先扫到”放在确定的位置。
type gatedOutboxStorage struct {
	*MongoCommitter
	scanStarted chan struct{} // 第一次读页时关闭
	scanRelease chan struct{} // 关闭后读页继续
	startOnce   sync.Once
	beforeMark  func(entity.RemoteTransactionID)
	marks       atomic.Int32
}

func newGatedOutboxStorage(committer *MongoCommitter, open bool) *gatedOutboxStorage {
	storage := &gatedOutboxStorage{MongoCommitter: committer, scanStarted: make(chan struct{}), scanRelease: make(chan struct{})}
	if open {
		close(storage.scanRelease)
	}
	return storage
}

func (s *gatedOutboxStorage) PendingRemoteCommitPage(ctx context.Context, after entity.RemoteOutboxCursor, limit int) ([]entity.RemoteCommitStatus, entity.RemoteOutboxCursor, error) {
	s.startOnce.Do(func() { close(s.scanStarted) })
	select {
	case <-s.scanRelease:
	case <-ctx.Done():
		return nil, after, ctx.Err()
	}
	return s.MongoCommitter.PendingRemoteCommitPage(ctx, after, limit)
}

func (s *gatedOutboxStorage) MarkRemoteCommitPublished(ctx context.Context, id entity.RemoteTransactionID) error {
	if s.beforeMark != nil {
		s.beforeMark(id)
	}
	s.marks.Add(1)
	return s.MongoCommitter.MarkRemoteCommitPublished(ctx, id)
}

// outboxProcess 是共享同一个 Mongo（同一个 outbox）的一个进程：Manager、它的存储，写者另有实体与 WAL 投影器。
type outboxProcess struct {
	mgr       *Manager
	storage   *gatedOutboxStorage
	loader    *remoteTestLoader
	live      *testRemoteEntity
	projector *engine.Projector
}

const outboxOwnerKind entity.EntityKind = 136

func newOutboxPeer(t *testing.T, mongo fmongo.IMongo, sid int32, cfg *Config, scanOpen bool) *outboxProcess {
	t.Helper()
	entity.MustRegisterEntityKindDefs(entity.EntityKindDef{Kind: outboxOwnerKind, Category: 1, RemotePolicy: entity.RemotePolicyManaged})
	committer := newBoundMongoCommitter(mongo, "control", sid, 0)
	storage := newGatedOutboxStorage(committer, scanOpen)
	loader := newRemoteTestLoader()
	backend, err := NewBackend(loader, storage)
	if err != nil {
		t.Fatal(err)
	}
	mgr := NewManager(newMockVersionedLockFactory(), cfg, sid)
	mgr.SetBackend(backend)
	mgr.SetOwnershipStore(committer)
	t.Cleanup(func() {
		close(storage.scanReleaseOnce())
		stop, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = mgr.StopFinalizer(stop)
	})
	process := &outboxProcess{mgr: mgr, storage: storage, loader: loader}
	process.live = newTestRemoteEntity(7300+int64(sid%100), 1, outboxOwnerKind)
	loader.add(process.live)
	return process
}

// scanReleaseOnce 返回一个可以安全关闭的通道：已打开的门返回新通道，避免清理时重复关闭。
func (s *gatedOutboxStorage) scanReleaseOnce() chan struct{} {
	select {
	case <-s.scanRelease:
		return make(chan struct{})
	default:
		return s.scanRelease
	}
}

// newOutboxWriter 在 newOutboxPeer 之上接 DataEngine 的 WAL 投影器：strict 写经投影器在 Mongo 事务里持久为 Applied，
// 再由 ApplyRemoteCommits 把 tracker 置为 Applied 并唤醒本进程的协调者——与正式链路相同。
func newOutboxWriter(t *testing.T, mongo fmongo.IMongo, sid int32, cfg *Config, scanOpen bool) *outboxProcess {
	t.Helper()
	process := newOutboxPeer(t, mongo, sid, cfg, scanOpen)
	store, err := engine.NewMongoStore(mongo, engine.MongoStoreConfig{Databases: process.storage.databases.Load(), DefaultDatabase: "game", ServerID: sid, TransactionReceiptTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if err = store.SetRemoteProjection(process.storage, process.mgr); err != nil {
		t.Fatal(err)
	}
	walOptions := nestwal.DefaultOptions(t.TempDir())
	walOptions.GroupCommitInterval = time.Millisecond
	wal, err := nestwal.Open(walOptions)
	if err != nil {
		t.Fatal(err)
	}
	process.projector, err = engine.NewProjector(wal, store, engine.ProjectorOptions{
		CloseWAL: true, IdlePoll: 10 * time.Millisecond, RetryMin: 5 * time.Millisecond, RetryMax: 50 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		closing, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = process.projector.Close(closing)
	})
	return process
}

// writeStrict 走 strict 写到“实体锁已释放、等确认”之前：WAL 追加、释放事务让投影器投影。返回的批次由调用方 Commit / Close。
func (p *outboxProcess) writeStrict(t *testing.T, ctx context.Context, tx entity.RemoteTransactionID) entity.RemoteWriteBatch {
	t.Helper()
	return p.writeStrictOn(t, ctx, tx, p.live)
}

// writeStrictOn 同 writeStrict，写的是指定实体（由 addEntity 加入本进程）。
func (p *outboxProcess) writeStrictOn(t *testing.T, ctx context.Context, tx entity.RemoteTransactionID, live *testRemoteEntity) entity.RemoteWriteBatch {
	t.Helper()
	batch, err := p.mgr.PrepareRemoteWriteBatch(ctx, []int64{live.GUId()})
	if err != nil {
		t.Fatal(err)
	}
	live.dirty.set(true)
	if err = batch.FinalizeLocked(entity.NewRemoteTransactionOutcome(tx, "outbox-owner", "", true, uint8(nest.DurabilityStrict.Record()))); err != nil {
		t.Fatal(err)
	}
	if err = p.projector.Commit(ctx, remoteCommitRecord(nest.DurabilityStrict.Record(), batch.Commits())); err != nil {
		t.Fatal(err)
	}
	p.projector.TransactionReleased(coredata.TransactionID(tx))
	return batch
}

// 两个进程的 outbox 协调者共享 outbox。写者的投影器刚把 strict 写持久为 Applied、自己的协调者还没读页时，别的进程的协调者
// 先扫描：它不能代发（还在宽限期内），写者自己发布，strict 写在请求期限内得到确认、tracker 结束。
// 修复前别的进程代发并标记已发布，写者的协调者读不到这笔提交，tracker 停在 Applied，strict 写等到期限超时、转延迟收尾，
// 收尾持写门到 FinalizeProjectionTimeout（KNOWN-LIMITS 原“跨实例写偶发 3 s 超时”）。
func TestOutboxWriterPublishesItsOwnCommitBeforeOtherCoordinators(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	mongo := newRemoteMongoFake()
	writer := newOutboxWriter(t, mongo, 1000, DefaultConfig(), false)
	other := newOutboxPeer(t, mongo, 2000, DefaultConfig(), true)
	tx := remoteTestTxID(201)
	batch := writer.writeStrict(t, ctx, tx)
	defer batch.Close(context.Background())

	awaitSignal(t, ctx, writer.storage.scanStarted, "the writer's outbox scan")
	if state := writer.mgr.localRemoteCommitState(tx); state != entity.RemoteCommitApplied {
		t.Fatalf("writer tracker=%v before any publication, want Applied", state)
	}
	if err := other.mgr.RecoverOutbox(ctx); err != nil {
		t.Fatal(err)
	}
	close(writer.storage.scanRelease)

	commitCtx, cancelCommit := context.WithTimeout(ctx, 2*time.Second)
	defer cancelCommit()
	_, err := batch.Commit(commitCtx)
	state := writer.mgr.localRemoteCommitState(tx)
	if err != nil || state != entity.RemoteCommitCommitted {
		t.Fatalf("the writer's strict write was not confirmed within its deadline: err=%v writer tracker=%v (marks: writer=%d other=%d)",
			err, state, writer.storage.marks.Load(), other.storage.marks.Load())
	}
	if marks := other.storage.marks.Load(); marks != 0 {
		t.Fatalf("another process published the writer's commit inside the takeover grace: marks=%d", marks)
	}
}

// outboxClock 是发布归属判定用的可控时钟：设了时刻就用它，清空后回到真实时间。
type outboxClock struct{ at atomic.Pointer[time.Time] }

func (c *outboxClock) set(at time.Time) { c.at.Store(&at) }
func (c *outboxClock) real()            { c.at.Store(nil) }
func (c *outboxClock) now() time.Time {
	if at := c.at.Load(); at != nil {
		return *at
	}
	return time.Now()
}

// insertAppliedOutboxCommit 直接写一条已持久未发布（Applied）的事务记录，模拟写者已经崩溃、没来得及发布。
func insertAppliedOutboxCommit(t *testing.T, mongo fmongo.IMongo, tx entity.RemoteTransactionID, entityID int64, base uint64, writer int32, created time.Time) {
	t.Helper()
	commit := entity.RemoteCommit{TransactionID: tx, EntityID: entityID, Kind: outboxOwnerKind, BaseVersion: base, NextVersion: base + 1,
		MarkerEpoch: 1, LockFence: 1, RouteEpoch: 1, Schema: 1, Codec: 1}
	doc := mongoRemoteTransaction{ID: tx.String(), State: uint8(entity.RemoteCommitApplied), WriterSID: writer, CreatedAt: created.UTC(),
		Receipts: []mongoRemoteReceipt{encodeMongoReceipt(remoteCommitReceipt(commit))}}
	doc.Commits, doc.MutationMasks = storeRemoteCommits([]entity.RemoteCommit{commit})
	if _, err := mongo.Database("control").Collection(remoteTxCollection).InsertOne(context.Background(), doc); err != nil {
		t.Fatal(err)
	}
}

func outboxCommitState(t *testing.T, process *outboxProcess, tx entity.RemoteTransactionID) entity.RemoteCommitState {
	t.Helper()
	status, err := process.storage.MongoCommitter.CommitStatus(context.Background(), tx)
	if err != nil {
		t.Fatal(err)
	}
	return status.State
}

// 宽限期：别的进程写的提交在写入后宽限期内不代发，到期那一刻起代发，且只发布一次；扫描时为最早的留存提交安排了到期唤醒。
func TestOutboxForeignCommitIsTakenOverOnlyAfterTheGrace(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	mongo := newRemoteMongoFake()
	cfg := DefaultConfig()
	cfg.FinalizeProjectionTimeout = time.Minute // 宽限期取它：测试时钟远离真实时间，计时器不会在用例里触发
	other := newOutboxPeer(t, mongo, 2000, cfg, true)
	clock := &outboxClock{}
	other.mgr.remote.outboxNow = clock.now
	created := time.Unix(1_800_000_000, 0)
	tx := remoteTestTxID(202)
	insertAppliedOutboxCommit(t, mongo, tx, 7401, 1, 1000, created)

	clock.set(created.Add(time.Minute - time.Millisecond))
	if err := other.mgr.RecoverOutbox(ctx); err != nil {
		t.Fatal(err)
	}
	if state := outboxCommitState(t, other, tx); state != entity.RemoteCommitApplied || other.storage.marks.Load() != 0 {
		t.Fatalf("taken over inside the grace: state=%v marks=%d", state, other.storage.marks.Load())
	}
	other.mgr.remote.republishMu.Lock()
	due, armed := other.mgr.remote.takeoverDue, other.mgr.remote.takeoverTimer != nil
	other.mgr.remote.republishMu.Unlock()
	if !armed || !due.Equal(created.Add(time.Minute)) {
		t.Fatalf("no wake-up at the grace expiry: armed=%v due=%v want %v", armed, due, created.Add(time.Minute))
	}

	clock.set(created.Add(time.Minute))
	if err := other.mgr.RecoverOutbox(ctx); err != nil {
		t.Fatal(err)
	}
	if state := outboxCommitState(t, other, tx); state != entity.RemoteCommitCommitted {
		t.Fatalf("not taken over at the grace expiry: state=%v", state)
	}
	if err := other.mgr.RecoverOutbox(ctx); err != nil {
		t.Fatal(err)
	}
	if marks := other.storage.marks.Load(); marks != 1 {
		t.Fatalf("published %d times, want once", marks)
	}
}

// 写者崩溃：它留下的 Applied 提交没有任何本地写再去唤醒别的进程，别的进程在第一次扫描时为它安排到期唤醒，宽限期后
// 自行代发；提交不丢、只发布一次，之后同 sid 重启的写者也不会再发一次。
func TestOutboxCrashedWritersCommitIsPublishedAfterTheGraceWithoutAnotherWake(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	mongo := newRemoteMongoFake()
	cfg := DefaultConfig()
	cfg.FinalizeProjectionTimeout = 300 * time.Millisecond
	cfg.FinalizeRetryInterval = 10 * time.Millisecond
	// 写者：strict 写持久为 Applied 后协调者一直停在读页之前，相当于发布前崩溃。
	writer := newOutboxWriter(t, mongo, 1000, cfg, false)
	tx := remoteTestTxID(203)
	batch := writer.writeStrict(t, ctx, tx)
	defer batch.Abort(context.Background(), nil)
	awaitSignal(t, ctx, writer.storage.scanStarted, "the writer's outbox scan")
	status, err := writer.storage.MongoCommitter.CommitStatus(ctx, tx)
	if err != nil || status.State != entity.RemoteCommitApplied || status.WriterSID != 1000 {
		t.Fatalf("durable record=%+v err=%v, want Applied written by 1000", status, err)
	}

	other := newOutboxPeer(t, mongo, 2000, cfg, true)
	clock := &outboxClock{}
	other.mgr.remote.outboxNow = clock.now
	clock.set(status.CreatedAt)
	if err := other.mgr.RecoverOutbox(ctx); err != nil {
		t.Fatal(err)
	}
	if state := outboxCommitState(t, other, tx); state != entity.RemoteCommitApplied {
		t.Fatalf("taken over before the grace: state=%v", state)
	}
	clock.real()
	// 不再调用 RecoverOutbox、也没有本地写：只靠扫描时安排的到期唤醒。
	for outboxCommitState(t, other, tx) != entity.RemoteCommitCommitted {
		select {
		case <-ctx.Done():
			t.Fatalf("the crashed writer's commit was never published (marks=%d)", other.storage.marks.Load())
		case <-time.After(10 * time.Millisecond):
		}
	}
	restarted := newOutboxPeer(t, mongo, 1000, cfg, true)
	if err := restarted.mgr.RecoverOutbox(ctx); err != nil {
		t.Fatal(err)
	}
	if err := other.mgr.RecoverOutbox(ctx); err != nil {
		t.Fatal(err)
	}
	if marks := other.storage.marks.Load() + restarted.storage.marks.Load() + writer.storage.marks.Load(); marks != 1 {
		t.Fatalf("published %d times, want once", marks)
	}
}

// 同 sid 重启：新进程把旧进程（同一个 sid）留下的 Applied 提交当作自己的，启动恢复立即发布，不等宽限期；别的进程仍按
// 宽限期留着它。单实例锁保证此时旧进程已经不在。
func TestOutboxRestartedWriterWithTheSameSidPublishesLeftoverCommitsAtOnce(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	mongo := newRemoteMongoFake()
	cfg := DefaultConfig()
	created := time.Now()
	tx := remoteTestTxID(204)
	insertAppliedOutboxCommit(t, mongo, tx, 7402, 1, 1000, created)

	other := newOutboxPeer(t, mongo, 2000, cfg, true)
	clock := &outboxClock{}
	other.mgr.remote.outboxNow = clock.now
	clock.set(created)
	if err := other.mgr.RecoverOutbox(ctx); err != nil {
		t.Fatal(err)
	}
	restarted := newOutboxPeer(t, mongo, 1000, cfg, true)
	restarted.mgr.remote.outboxNow = clock.now
	if err := restarted.mgr.RecoverOutbox(ctx); err != nil {
		t.Fatal(err)
	}
	if state := outboxCommitState(t, restarted, tx); state != entity.RemoteCommitCommitted {
		t.Fatalf("the restarted writer left its predecessor's commit unpublished: state=%v", state)
	}
	if marks := other.storage.marks.Load(); marks != 0 {
		t.Fatalf("another process took it over inside the grace: marks=%d", marks)
	}
	if state := restarted.mgr.localRemoteCommitState(tx); state != entity.RemoteCommitCommitted {
		t.Fatalf("restarted writer tracker=%v, want Committed", state)
	}
}

// 同一实体上排在后面的提交本轮要发布时，前面别的进程留下的提交立即代发、先于它发布；无关实体上的留存提交不受影响。
func TestOutboxForeignPredecessorOfAPublishedCommitIsTakenOverFirst(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	mongo := newRemoteMongoFake()
	cfg := DefaultConfig()
	cfg.OutboxPublishWorkers = 4
	created := time.Now()
	crashed, own, unrelated := remoteTestTxID(205), remoteTestTxID(206), remoteTestTxID(207)
	insertAppliedOutboxCommit(t, mongo, crashed, 7403, 1, 1000, created)
	insertAppliedOutboxCommit(t, mongo, own, 7403, 2, 2000, created.Add(time.Millisecond))
	insertAppliedOutboxCommit(t, mongo, unrelated, 7404, 1, 1000, created.Add(2*time.Millisecond))

	other := newOutboxPeer(t, mongo, 2000, cfg, true)
	clock := &outboxClock{}
	other.mgr.remote.outboxNow = clock.now
	clock.set(created.Add(3 * time.Millisecond))
	var mu sync.Mutex
	var order []entity.RemoteTransactionID
	other.storage.beforeMark = func(id entity.RemoteTransactionID) {
		mu.Lock()
		order = append(order, id)
		mu.Unlock()
	}
	if err := other.mgr.RecoverOutbox(ctx); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(order) != 2 || order[0] != crashed || order[1] != own {
		t.Fatalf("publication order=%v, want [%v %v]", order, crashed, own)
	}
	if state := outboxCommitState(t, other, unrelated); state != entity.RemoteCommitApplied {
		t.Fatalf("unrelated foreign commit taken over inside the grace: state=%v", state)
	}
}

// 时钟偏差下的同实体后继：outbox 页按写入时刻（各写者自己的时钟）排序。崩溃的写者时钟快，它留下的提交排到了同一实体
// 上后来的提交后面，“后继立即代发”看不出它是前序：后来的提交照常先发，它等到宽限期（按它的写入时刻，即多等了偏差）
// 才代发。两笔各发布一次，不重复；晚到的旧版本发布被版本比较丢弃（与跨页时相同，见 REMOTE_ENTITY“outbox 发布归属”）。
func TestOutboxSuccessorRuleUnderWriterClockSkewFallsBackToTheGrace(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	mongo := newRemoteMongoFake()
	cfg := DefaultConfig()
	now := time.Now()
	skew := 10 * time.Second
	crashed, own := remoteTestTxID(210), remoteTestTxID(211)
	insertAppliedOutboxCommit(t, mongo, crashed, 7405, 1, 1000, now.Add(skew)) // 写者时钟快 10 s
	insertAppliedOutboxCommit(t, mongo, own, 7405, 2, 2000, now.Add(time.Millisecond))

	other := newOutboxPeer(t, mongo, 2000, cfg, true)
	clock := &outboxClock{}
	other.mgr.remote.outboxNow = clock.now
	clock.set(now.Add(2 * time.Millisecond))
	if err := other.mgr.RecoverOutbox(ctx); err != nil {
		t.Fatal(err)
	}
	if state := outboxCommitState(t, other, own); state != entity.RemoteCommitCommitted {
		t.Fatalf("own commit state=%v, want published", state)
	}
	if state := outboxCommitState(t, other, crashed); state != entity.RemoteCommitApplied {
		t.Fatalf("skewed predecessor state=%v, want it left for the grace (it sorts after its successor)", state)
	}
	other.mgr.remote.republishMu.Lock()
	due := other.mgr.remote.takeoverDue
	other.mgr.remote.republishMu.Unlock()
	if want := now.Add(skew + other.mgr.outboxTakeoverGrace()); due.Sub(want).Abs() > time.Millisecond { // 存储只保留到毫秒
		t.Fatalf("takeover due=%v, want the writer's created_at + grace %v", due, want)
	}
	clock.set(due)
	if err := other.mgr.RecoverOutbox(ctx); err != nil {
		t.Fatal(err)
	}
	if state := outboxCommitState(t, other, crashed); state != entity.RemoteCommitCommitted {
		t.Fatalf("skewed predecessor state=%v at its grace expiry", state)
	}
	if marks := other.storage.marks.Load(); marks != 2 {
		t.Fatalf("marks=%d, want each commit published once", marks)
	}
}

// 写者活着但慢：它的协调者已读到页、标记之前别的进程（宽限期已过）抢先代发。写者标记时发现权威里已是 Committed，
// 当作完成，tracker 结束，strict 写照常得到确认。
func TestOutboxWriterFindingItsCommitAlreadyPublishedCompletesItsTracker(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	mongo := newRemoteMongoFake()
	writer := newOutboxWriter(t, mongo, 1000, DefaultConfig(), true)
	other := newOutboxPeer(t, mongo, 2000, DefaultConfig(), true)
	clock := &outboxClock{}
	other.mgr.remote.outboxNow = clock.now
	clock.set(time.Now().Add(time.Hour))
	tx := remoteTestTxID(208)
	var once sync.Once
	writer.storage.beforeMark = func(id entity.RemoteTransactionID) {
		if id == tx {
			once.Do(func() {
				if err := other.mgr.RecoverOutbox(ctx); err != nil {
					t.Error(err)
				}
			})
		}
	}
	batch := writer.writeStrict(t, ctx, tx)
	defer batch.Close(context.Background())
	commitCtx, cancelCommit := context.WithTimeout(ctx, 2*time.Second)
	defer cancelCommit()
	_, err := batch.Commit(commitCtx)
	if state := writer.mgr.localRemoteCommitState(tx); err != nil || state != entity.RemoteCommitCommitted {
		t.Fatalf("writer err=%v tracker=%v, want its strict write confirmed", err, state)
	}
	if marks := other.storage.marks.Load(); marks != 1 {
		t.Fatalf("the other process did not take over first: marks=%d", marks)
	}
}

// 写者活着但慢，且别的进程在写者读页之前就代发了：写者的协调者读不到这笔提交，没人通知它的 tracker（不回源是有意的，
// 见 REMOTE_ENTITY“outbox 发布归属”）。strict 写超时转延迟收尾，收尾在投影期限到期时回源读到 Committed 释放——
// 宽限期不短于这个期限，所以真实部署里代发发生时写者的收尾期限也已基本到期；这里钉住收尾不会超出期限挂着。
func TestOutboxWriterOvertakenBeforeItsScanIsReleasedAtTheProjectionDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	mongo := newRemoteMongoFake()
	cfg := DefaultConfig()
	cfg.FinalizeProjectionTimeout = 300 * time.Millisecond
	cfg.FinalizeRetryInterval = 10 * time.Millisecond
	writer := newOutboxWriter(t, mongo, 1000, cfg, false)
	steps := make(chan string, 16)
	writer.mgr.remote.finalizeTrace = func(_ entity.RemoteTransactionID, step string) { steps <- step }
	writer.mgr.StartFinalizer()
	other := newOutboxPeer(t, mongo, 2000, cfg, true)
	clock := &outboxClock{}
	other.mgr.remote.outboxNow = clock.now
	clock.set(time.Now().Add(time.Hour))
	tx := remoteTestTxID(209)
	batch := writer.writeStrict(t, ctx, tx)
	awaitSignal(t, ctx, writer.storage.scanStarted, "the writer's outbox scan")
	if err := other.mgr.RecoverOutbox(ctx); err != nil {
		t.Fatal(err)
	}
	close(writer.storage.scanRelease)
	commitCtx, cancelCommit := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancelCommit()
	if _, err := batch.Commit(commitCtx); !errors.Is(err, entity.ErrRemotePersistenceIndeterminate) {
		t.Fatalf("commit err=%v, want the strict wait to end without a conclusion", err)
	}
	closed := time.Now()
	if err := batch.Close(ctx); err != nil {
		t.Fatal(err)
	}
	for step := ""; step != "released"; {
		select {
		case step = <-steps:
		case <-ctx.Done():
			t.Fatal("the deferred close was never released")
		}
	}
	if elapsed := time.Since(closed); elapsed > cfg.FinalizeProjectionTimeout+2*time.Second {
		t.Fatalf("released %v after Close, want about FinalizeProjectionTimeout (%v)", elapsed, cfg.FinalizeProjectionTimeout)
	}
	if state := writer.mgr.localRemoteCommitState(tx); state != entity.RemoteCommitCommitted {
		t.Fatalf("writer tracker=%v after release, want Committed", state)
	}
}

// 宽限期的零值与下限都是 FinalizeProjectionTimeout。
func TestOutboxTakeoverGraceFloorsAtTheProjectionTimeout(t *testing.T) {
	for _, tc := range []struct{ grace, projection, want time.Duration }{
		{0, 30 * time.Second, 30 * time.Second},
		{time.Second, 30 * time.Second, 30 * time.Second},
		{time.Minute, 30 * time.Second, time.Minute},
		{0, 0, 30 * time.Second},
	} {
		cfg := DefaultConfig()
		cfg.OutboxTakeoverGrace, cfg.FinalizeProjectionTimeout = tc.grace, tc.projection
		if got := NewManager(newMockVersionedLockFactory(), cfg, 1000).outboxTakeoverGrace(); got != tc.want {
			t.Fatalf("grace=%v projection=%v: got %v want %v", tc.grace, tc.projection, got, tc.want)
		}
	}
}

// ---- 慢发布不挡住别的事务：发布页只剩慢项且有新提交在等时，慢项转到后台 ----

// addEntity 给进程加一个实体（与 live 不同的 id），用来在同一进程里做互不相关的写。
func (p *outboxProcess) addEntity(id int64) *testRemoteEntity {
	live := newTestRemoteEntity(id, 1, outboxOwnerKind)
	p.loader.add(live)
	return live
}

// 同一进程两笔无关实体上的 strict 写：第一笔发布时 Mongo 标记卡住（模拟一次几秒的慢写），第二笔必须在自己的请求期限内
// 得到确认。修复前唯一发布协调者一次发一页、等整页结束才读下一页，慢的那一笔挡住之后全部提交的发布：单实例 128 并发时
// 全部写额度（max_concurrent_writes 128）都停在“等确认”，到 Nest 请求期限（3 s）同时超时（KNOWN-LIMITS“单实例全体同时 3 s 超时”）。
func TestOutboxSlowPublicationDoesNotHoldUpLaterCommits(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	mongo := newRemoteMongoFake()
	writer := newOutboxWriter(t, mongo, 1000, DefaultConfig(), true)
	other := writer.addEntity(7501)
	slow, fast := remoteTestTxID(221), remoteTestTxID(222)
	slowStarted, slowRelease := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(slowRelease) }) }
	t.Cleanup(release)
	writer.storage.beforeMark = func(id entity.RemoteTransactionID) {
		if id == slow {
			close(slowStarted)
			<-slowRelease
		}
	}
	slowBatch := writer.writeStrict(t, ctx, slow)
	defer slowBatch.Close(context.Background())
	awaitSignal(t, ctx, slowStarted, "the slow publication")

	fastBatch := writer.writeStrictOn(t, ctx, fast, other)
	defer fastBatch.Close(context.Background())
	commitCtx, cancelCommit := context.WithTimeout(ctx, 2*time.Second)
	defer cancelCommit()
	_, err := fastBatch.Commit(commitCtx)
	if state := writer.mgr.localRemoteCommitState(fast); err != nil || state != entity.RemoteCommitCommitted {
		t.Fatalf("a strict write on another entity was not confirmed while one publication was slow: err=%v tracker=%v (slow tracker=%v)",
			err, state, writer.mgr.localRemoteCommitState(slow))
	}
	if state := writer.mgr.localRemoteCommitState(slow); state != entity.RemoteCommitApplied {
		t.Fatalf("slow tracker=%v while its mark is still held, want Applied", state)
	}

	release()
	slowCtx, cancelSlow := context.WithTimeout(ctx, 2*time.Second)
	defer cancelSlow()
	if _, err := slowBatch.Commit(slowCtx); err != nil {
		t.Fatalf("the slow write after its mark returned: %v", err)
	}
	if marks := writer.storage.marks.Load(); marks != 2 {
		t.Fatalf("marks=%d, want each commit published once", marks)
	}
}

// outboxLoopIdle 等后台发布循环退出（一页只剩慢项时它不再等，慢项转到后台）。
func outboxLoopIdle(t *testing.T, manager *Manager, what string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		manager.remote.republishMu.Lock()
		running := manager.remote.republishRunning
		manager.remote.republishMu.Unlock()
		if !running {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the outbox loop is still waiting: %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// 慢发布转到后台后，同一实体上排在它后面的提交仍要等它发布完才发布（发布顺序不变），无关实体的提交不等；慢项结束后
// 不需要别的唤醒，后继由它结束时的那次扫描发布。
func TestOutboxDetachedPublicationKeepsSameEntityOrder(t *testing.T) {
	backend := newParallelOutboxBackend(t, [][]int64{{10}, {10, 30}, {20}})
	backend.first, backend.follower = backend.statuses[0].TransactionID, backend.statuses[1].TransactionID
	independent := backend.statuses[2].TransactionID
	later := backend.statuses[1:]
	backend.statuses = backend.statuses[:1]
	manager := NewManager(newMockVersionedLockFactory(), DefaultConfig(), 1000)
	manager.remote.stragglerWait = 10 * time.Millisecond
	manager.SetBackend(backend)
	var release sync.Once
	t.Cleanup(func() {
		release.Do(func() { close(backend.release) })
		_ = manager.StopFinalizer(context.Background())
	})
	next := func(what string) entity.RemoteTransactionID {
		t.Helper()
		select {
		case id := <-backend.started:
			return id
		case <-time.After(2 * time.Second):
			t.Fatalf("no publication started: %s", what)
			return entity.RemoteTransactionID{}
		}
	}

	manager.scheduleOutboxRepublish()
	if id := next("the slow one"); id != backend.first {
		t.Fatalf("started %v, want the slow transaction first", id)
	}
	outboxLoopIdle(t, manager, "only the slow publication is left")
	backend.mu.Lock()
	backend.statuses = append(backend.statuses, later...)
	backend.mu.Unlock()
	manager.scheduleOutboxRepublish()
	if id := next("the unrelated commit"); id != independent {
		t.Fatalf("started %v while the slow publication is held, want only the unrelated commit", id)
	}
	outboxLoopIdle(t, manager, "the unrelated commit is published")
	select {
	case id := <-backend.started:
		t.Fatalf("started %v before the slow publication of the same entity finished", id)
	default:
	}

	release.Do(func() { close(backend.release) })
	if id := next("the same-entity successor after the slow one finished"); id != backend.follower {
		t.Fatalf("started %v, want the same-entity successor", id)
	}
	outboxLoopIdle(t, manager, "everything is published")
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if len(backend.marked) != 3 {
		t.Fatalf("marked=%d, want 3", len(backend.marked))
	}
	if detached := manager.remote.outboxInflight; len(detached) != 0 {
		t.Fatalf("in-flight registry not cleared: %v", detached)
	}
}

// heldMarkBackend 的标记不理会取消：转到后台的发布卡在这里时，停机必须等它结束才算停完。
type heldMarkBackend struct {
	*parallelOutboxBackend
	failOnce atomic.Bool
}

func (b *heldMarkBackend) MarkRemoteCommitPublished(ctx context.Context, id entity.RemoteTransactionID) error {
	b.started <- id
	<-b.release
	if b.failOnce.CompareAndSwap(true, false) {
		return errors.New("injected slow mark failure")
	}
	b.mu.Lock()
	b.marked[id] = true
	b.mu.Unlock()
	return nil
}

func TestOutboxDetachedPublicationIsDrainedBeforeStop(t *testing.T) {
	backend := &heldMarkBackend{parallelOutboxBackend: newParallelOutboxBackend(t, [][]int64{{10}})}
	manager := NewManager(newMockVersionedLockFactory(), DefaultConfig(), 1000)
	manager.remote.stragglerWait = 10 * time.Millisecond
	manager.SetBackend(backend)
	stop, released := stopcontract.CallerReleases(manager.StopFinalizer)
	stopcontract.Check(t, stopcontract.Hooks{
		Block: func(t testing.TB) {
			manager.scheduleOutboxRepublish()
			select {
			case <-backend.started:
			case <-time.After(3 * time.Second):
				t.Fatal("the publication did not start")
			}
			outboxLoopIdle(t.(*testing.T), manager, "the slow publication was not moved to the background")
		},
		Stop:     stop,
		Release:  func() { close(backend.release) },
		Released: released,
	})
}

// 转到后台的发布失败：tracker 置为 Indeterminate（同页内失败），发布循环先退避再重试，之后发布成功、tracker 得到 Committed。
func TestOutboxDetachedPublicationFailureIsRetried(t *testing.T) {
	backend := &heldMarkBackend{parallelOutboxBackend: newParallelOutboxBackend(t, [][]int64{{10}})}
	backend.failOnce.Store(true)
	tx := backend.statuses[0].TransactionID
	cfg := DefaultConfig()
	cfg.FinalizeRetryInterval = 20 * time.Millisecond
	manager := NewManager(newMockVersionedLockFactory(), cfg, 1000)
	manager.remote.stragglerWait = 10 * time.Millisecond
	manager.SetBackend(backend)
	t.Cleanup(func() { _ = manager.StopFinalizer(context.Background()) })
	if err := manager.trackRemoteTransaction(tx); err != nil {
		t.Fatal(err)
	}
	manager.completeRemoteTransaction(tx, entity.RemoteCommitStatus{TransactionID: tx, State: entity.RemoteCommitApplied, Receipts: backend.statuses[0].Receipts})
	manager.scheduleOutboxRepublish()
	select {
	case <-backend.started:
	case <-time.After(3 * time.Second):
		t.Fatal("the publication did not start")
	}
	outboxLoopIdle(t, manager, "the slow publication was not moved to the background")
	failedAt := time.Now()
	close(backend.release)
	select {
	case <-backend.started:
	case <-time.After(3 * time.Second):
		t.Fatalf("the failed background publication was never retried (tracker=%v)", manager.localRemoteCommitState(tx))
	}
	if elapsed := time.Since(failedAt); elapsed < cfg.FinalizeRetryInterval {
		t.Fatalf("retried after %v, want a back-off of at least %v", elapsed, cfg.FinalizeRetryInterval)
	}
	outboxLoopIdle(t, manager, "the retry finished")
	if state := manager.localRemoteCommitState(tx); state != entity.RemoteCommitCommitted {
		t.Fatalf("tracker=%v after the retry, want Committed", state)
	}
}
