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
	store, err := engine.NewMongoStore(mongo, engine.MongoStoreConfig{DefaultDatabase: "game", ServerID: 1000, TransactionReceiptTTL: time.Hour})
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
