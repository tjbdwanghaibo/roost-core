// 事务跟踪：容量淘汰按首次完成、只清过期完成项、非法 Apply 不占容量、并发淘汰不丢 Flush 等待、淘汰后等待者仍拿到终态。

package remoteentity

import (
	"context"
	"encoding/binary"
	"fmt"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
)

func TestFlushRemoteAllKeepsTrackersAcrossConcurrentEviction(t *testing.T) {
	cfg := DefaultConfig()
	cfg.TransactionTrackLimit = 2
	m := NewManager(newMockVersionedLockFactory(), cfg, 1000)
	a, b := remoteTestTxID(101), remoteTestTxID(102)
	for _, id := range []entity.RemoteTransactionID{a, b} {
		if err := m.trackRemoteTransaction(id); err != nil {
			t.Fatal(err)
		}
	}
	barrier := &selectEntryBarrier{Context: context.Background(), entered: make(chan struct{}), resume: make(chan struct{})}
	done := make(chan error, 1)
	go func() { done <- m.FlushRemoteAll(barrier) }()
	select {
	case <-barrier.entered:
	case <-time.After(time.Second):
		t.Fatal("flush did not wait")
	}
	for _, id := range []entity.RemoteTransactionID{a, b} {
		m.completeRemoteTransaction(id, entity.RemoteCommitStatus{TransactionID: id, State: entity.RemoteCommitCommitted})
	}
	for _, id := range []entity.RemoteTransactionID{remoteTestTxID(103), remoteTestTxID(104)} {
		if err := m.trackRemoteTransaction(id); err != nil {
			t.Fatal(err)
		}
	}
	close(barrier.resume)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("original transactions completed but FlushAll returned %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("flush recreated an evicted transaction")
	}
	if stats := m.Stats(); stats.ActiveTransactions != 2 {
		t.Fatalf("new transactions changed: %+v", stats)
	}
}

func TestInvalidRemoteApplyDoesNotConsumeTrackerCapacity(t *testing.T) {
	cfg := DefaultConfig()
	cfg.TransactionTrackLimit = 1
	m := NewManager(newMockVersionedLockFactory(), cfg, 1000)
	m.SetBackend(newRemoteTestLoader())
	for i := byte(1); i <= 3; i++ {
		id := remoteTestTxID(i)
		if _, err := m.ApplyRemoteCommits(context.Background(), id, []entity.RemoteCommit{{TransactionID: id}}); err == nil {
			t.Fatal("accepted invalid commit")
		}
		if stats := m.Stats(); stats.Transactions != 0 {
			t.Fatalf("invalid input leaked tracker: %+v", stats)
		}
	}
	if err := m.trackRemoteTransaction(remoteTestTxID(10)); err != nil {
		t.Fatalf("valid admission blocked: %v", err)
	}
}

func trackingID(n uint64) entity.RemoteTransactionID {
	var id entity.RemoteTransactionID
	binary.BigEndian.PutUint64(id[8:], n+1)
	return id
}

func BenchmarkRemoteTrackerAtCapacity(b *testing.B) {
	for _, capacity := range []int{64, 1024, 8192, 65536} {
		b.Run(fmt.Sprint(capacity), func(b *testing.B) {
			cfg := DefaultConfig()
			cfg.TransactionTrackLimit = capacity
			cfg.TransactionTrackTTL = time.Hour
			m := NewManager(newMockVersionedLockFactory(), cfg, 1000)
			for i := range capacity {
				id := trackingID(uint64(i))
				m.completeRemoteTransaction(id, entity.RemoteCommitStatus{TransactionID: id, State: entity.RemoteCommitCommitted})
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				id := trackingID(uint64(capacity + i))
				if err := m.trackRemoteTransaction(id); err != nil {
					b.Fatal(err)
				}
				m.completeRemoteTransaction(id, entity.RemoteCommitStatus{TransactionID: id, State: entity.RemoteCommitCommitted})
			}
		})
	}
}

func BenchmarkRemoteTrackerStats(b *testing.B) {
	cfg := DefaultConfig()
	cfg.TransactionTrackLimit = 65536
	m := NewManager(newMockVersionedLockFactory(), cfg, 1000)
	for i := range cfg.TransactionTrackLimit {
		id := trackingID(uint64(i))
		m.completeRemoteTransaction(id, entity.RemoteCommitStatus{TransactionID: id, State: entity.RemoteCommitCommitted})
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if m.Stats().ActiveTransactions != 0 {
			b.Fatal("unexpected active")
		}
	}
}

func TestRemoteTrackerEvictionUsesFirstCompletionAndKeepsPending(t *testing.T) {
	cfg := DefaultConfig()
	cfg.TransactionTrackLimit = 3
	cfg.TransactionTrackTTL = time.Hour
	m := NewManager(newMockVersionedLockFactory(), cfg, 1000)
	pending, first, second := trackingID(0), trackingID(1), trackingID(2)
	if err := m.trackRemoteTransaction(pending); err != nil {
		t.Fatal(err)
	}
	for _, id := range []entity.RemoteTransactionID{first, second} {
		m.completeRemoteTransaction(id, entity.RemoteCommitStatus{TransactionID: id, State: entity.RemoteCommitIndeterminate})
	}
	retained := m.remote.txs[first]
	m.completeRemoteTransaction(first, entity.RemoteCommitStatus{TransactionID: first, State: entity.RemoteCommitCommitted})
	if err := m.trackRemoteTransaction(trackingID(3)); err != nil {
		t.Fatal(err)
	}
	if m.remote.txs[first] != nil || m.remote.txs[pending] == nil || m.remote.txs[second] == nil {
		t.Fatal("duplicate completion changed eviction order or evicted pending")
	}
	if retained.nextClosed != nil {
		t.Fatal("evicted waiter retains terminal queue")
	}
	if s := m.Stats(); s.Transactions != 3 || s.ActiveTransactions != 2 {
		t.Fatalf("stats=%+v", s)
	}
}

func TestRemoteTrackerTTLPrunesOnlyExpiredCompletions(t *testing.T) {
	cfg := DefaultConfig()
	cfg.TransactionTrackTTL = time.Hour
	m := NewManager(newMockVersionedLockFactory(), cfg, 1000)
	for i := range 3 {
		id := trackingID(uint64(i))
		m.completeRemoteTransaction(id, entity.RemoteCommitStatus{TransactionID: id, State: entity.RemoteCommitCommitted})
	}
	pending := trackingID(5)
	if err := m.trackRemoteTransaction(pending); err != nil {
		t.Fatal(err)
	}
	// 受控完成时刻，无需 sleep；边界恰好过期，后继终态尚未过期。
	now := time.Now()
	m.remote.txMu.Lock()
	m.remote.txs[trackingID(0)].closedAt = now.Add(-2 * time.Hour)
	m.remote.txs[trackingID(1)].closedAt = now.Add(-time.Hour)
	m.remote.txs[trackingID(2)].closedAt = now.Add(-time.Minute)
	m.pruneRemoteTransactionsLocked(now)
	m.remote.txMu.Unlock()
	if s := m.Stats(); s.Transactions != 2 || s.ActiveTransactions != 1 {
		t.Fatalf("stats=%+v", s)
	}
	if m.remote.txs[trackingID(2)] == nil || m.remote.txs[pending] == nil {
		t.Fatal("removed live entry")
	}
	m.remote.txMu.Lock()
	m.pruneRemoteTransactionsLocked(now.Add(2 * time.Hour))
	m.remote.txMu.Unlock()
	if s := m.Stats(); s.Transactions != 1 || s.ActiveTransactions != 1 {
		t.Fatalf("second prune=%+v", s)
	}
}

func TestInvalidRemoteApplyPreservesExistingCompletion(t *testing.T) {
	m := NewManager(newMockVersionedLockFactory(), DefaultConfig(), 1000)
	m.SetBackend(newRemoteTestLoader())
	id := remoteTestTxID(11)
	m.completeRemoteTransaction(id, entity.RemoteCommitStatus{TransactionID: id, State: entity.RemoteCommitCommitted})
	for _, commits := range [][]entity.RemoteCommit{nil, {{TransactionID: id}}, {{TransactionID: remoteTestTxID(12)}}} {
		if _, err := m.ApplyRemoteCommits(context.Background(), id, commits); err == nil {
			t.Fatal("invalid apply succeeded")
		}
		status, err := m.RemoteCommitStatus(context.Background(), id)
		if err != nil || status.State != entity.RemoteCommitCommitted {
			t.Fatalf("status=%+v err=%v", status, err)
		}
	}
}

// selectEntryBarrier pins a select at the moment it is entered.
//
// A select evaluates every channel operand before it blocks, so Done() runs
// once, at entry — after waitRemoteTransaction has already taken the tracker
// and its done channel, and before it can observe anything that happens next.
// Returning nil leaves ctx.Done() as a never-ready case, so the select
// resolves on the transaction's own done channel.
type selectEntryBarrier struct {
	context.Context
	entered chan struct{}
	resume  chan struct{}
}

func (c *selectEntryBarrier) Done() <-chan struct{} {
	close(c.entered)
	<-c.resume
	return nil
}

// 等待者拿到这笔事务的终态，即使记录已被容量淘汰：等待者从进入 select 之前就持有 tracker 指针，淘汰只把它移出索引；
// 唤醒后不能重新查 map（条目可能已不在，且在 txMu 临界区里 panic 会锁死后续所有事务跟踪）。
// 历史：RR-20260910-03。
func TestTransactionWaitReturnsTheTerminalStatusAfterEviction(t *testing.T) {
	cfg := DefaultConfig()
	cfg.TransactionTrackLimit = 1
	manager := NewManager(newMockVersionedLockFactory(), cfg, 1000)
	first, second := remoteTestTxID(201), remoteTestTxID(202)

	barrier := &selectEntryBarrier{
		Context: context.Background(),
		entered: make(chan struct{}),
		resume:  make(chan struct{}),
	}
	type outcome struct {
		status   entity.RemoteCommitStatus
		err      error
		panicked any
	}
	done := make(chan outcome, 1)
	go func() {
		var result outcome
		defer func() {
			result.panicked = recover()
			done <- result
		}()
		result.status, result.err = manager.waitRemoteTransaction(barrier, first)
	}()

	select {
	case <-barrier.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("the waiter never reached its select")
	}

	// The transaction completes, then another admission evicts the record it
	// just closed. Both go through the real paths.
	manager.completeRemoteTransaction(first, entity.RemoteCommitStatus{
		TransactionID: first, State: entity.RemoteCommitCommitted,
	})
	if err := manager.trackRemoteTransaction(second); err != nil {
		t.Fatal(err)
	}
	manager.remote.txMu.Lock()
	_, retained := manager.remote.txs[first]
	manager.remote.txMu.Unlock()
	if retained {
		t.Fatal("control: the completed tracker was not evicted, so this test proves nothing")
	}

	close(barrier.resume)
	select {
	case got := <-done:
		if got.panicked != nil {
			t.Fatalf("the waiter panicked instead of answering: %v", got.panicked)
		}
		if got.err != nil {
			t.Fatalf("wait after eviction = %v, want the committed status", got.err)
		}
		if got.status.State != entity.RemoteCommitCommitted {
			t.Fatalf("wait after eviction returned state %v, want committed", got.status.State)
		}
		if got.status.TransactionID != first {
			t.Fatalf("wait returned the status of %v, want %v", got.status.TransactionID, first)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the waiter never returned")
	}

	// The manager is still usable: a panic inside the txMu critical section
	// would have left the lock held forever.
	locked := make(chan error, 1)
	go func() { locked <- manager.trackRemoteTransaction(remoteTestTxID(203)) }()
	select {
	case <-locked:
	case <-time.After(2 * time.Second):
		t.Fatal("txMu is still held; a panic in the critical section skipped its Unlock")
	}
}
