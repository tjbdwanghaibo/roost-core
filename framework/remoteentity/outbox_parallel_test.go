package remoteentity

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
)

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
