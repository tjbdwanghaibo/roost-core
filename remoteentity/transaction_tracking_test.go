package remoteentity

import (
	"context"
	"encoding/binary"
	"fmt"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/entity"
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
