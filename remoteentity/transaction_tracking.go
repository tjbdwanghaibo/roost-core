package remoteentity

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/tjbdwanghaibo/roost-core/entity"
	"github.com/tjbdwanghaibo/roost-core/fctx"
	"github.com/tjbdwanghaibo/roost-core/metrics"
)

type remoteTransactionTracker struct {
	done       chan struct{}
	status     entity.RemoteCommitStatus
	closed     bool
	closedAt   time.Time
	id         entity.RemoteTransactionID
	nextClosed *remoteTransactionTracker
}

func (m *Manager) trackRemoteTransaction(id entity.RemoteTransactionID) error {
	_, err := m.trackedRemoteTransaction(id)
	return err
}

// trackedRemoteTransaction admits the transaction and hands back its tracker.
//
// A caller that is going to WAIT must keep this pointer rather than looking the
// id up again: capacity admission evicts the oldest CLOSED record, which is
// exactly the record a waiter is being woken by, so between close(done) and the
// waiter reacquiring txMu the map entry can be gone. Re-reading the map there
// dereferenced nil, inside the txMu critical section, so the deferred Unlock
// never ran and every later tracker call blocked on it (RR-20260910-03).
// Eviction only drops the index; the object stays alive for whoever holds it.
func (m *Manager) trackedRemoteTransaction(id entity.RemoteTransactionID) (*remoteTransactionTracker, error) {
	if m == nil || m.remote == nil || id.IsZero() {
		return nil, entity.ErrRemoteRejected
	}
	m.remote.txMu.Lock()
	defer m.remote.txMu.Unlock()
	return m.newRemoteTransactionLocked(id)
}

// 调用方持有 txMu；仅完成的记录可以让出容量，pending 永不淘汰。
func (m *Manager) newRemoteTransactionLocked(id entity.RemoteTransactionID) (*remoteTransactionTracker, error) {
	if tracker := m.remote.txs[id]; tracker != nil {
		return tracker, nil
	}
	if m.remote.txCapacity > 0 && len(m.remote.txs) >= m.remote.txCapacity {
		m.pruneRemoteTransactionsLocked(time.Now())
		if len(m.remote.txs) >= m.remote.txCapacity {
			m.evictOldestClosedRemoteTransactionLocked()
		}
		if len(m.remote.txs) >= m.remote.txCapacity {
			return nil, entity.ErrRemoteOverloaded
		}
	}
	tracker := &remoteTransactionTracker{id: id, done: make(chan struct{}), status: entity.RemoteCommitStatus{TransactionID: id, State: entity.RemoteCommitAdmitted}}
	m.remote.txs[id] = tracker
	return tracker, nil
}

func (m *Manager) evictOldestClosedRemoteTransactionLocked() {
	oldest := m.remote.closedHead
	if oldest == nil {
		return
	}
	m.remote.closedHead = oldest.nextClosed
	if m.remote.closedHead == nil {
		m.remote.closedTail = nil
	}
	// 已被唤醒的等待者仍可能持有 oldest，不能让它保留整个终态队列。
	oldest.nextClosed = nil
	m.remote.closedCount--
	delete(m.remote.txs, oldest.id)
}

func (m *Manager) pruneRemoteTransactionsLocked(now time.Time) {
	if m.remote.txTTL <= 0 {
		return
	}
	for oldest := m.remote.closedHead; oldest != nil; oldest = m.remote.closedHead {
		if now.Sub(oldest.closedAt) < m.remote.txTTL {
			return
		}
		m.evictOldestClosedRemoteTransactionLocked()
	}
}

func (m *Manager) completeRemoteTransaction(id entity.RemoteTransactionID, status entity.RemoteCommitStatus) {
	if m == nil || m.remote == nil || id.IsZero() {
		return
	}
	m.remote.txMu.Lock()
	tracker, err := m.newRemoteTransactionLocked(id)
	if err != nil {
		m.remote.txMu.Unlock()
		metrics.IncCounter("remote_entity_transaction_tracker_drop_total", nil, 1)
		return
	}
	if remoteCommitFinal(tracker.status.State) {
		// 终态不可改写（RR-20260926-11 复核残留）：已提交事务的重放发布失败只影响这次
		// 调用的返回值，不能把 Committed 改回 Indeterminate，后到的等待方/FlushRemoteAll
		// 仍须得到已提交结论。相互矛盾的终态保留先到的持久结论并计数。
		if status.State != tracker.status.State {
			metrics.IncCounter("remote_entity_transaction_final_overwrite_ignored_total", nil, 1)
		}
		m.remote.txMu.Unlock()
		return
	}
	tracker.status = status.Clone()
	// Indeterminate 也会关闭 done 唤醒等待方，但不是终态：之后的持久结论仍可覆盖它。
	terminal := remoteCommitFinal(status.State) || status.State == entity.RemoteCommitIndeterminate
	if terminal && !tracker.closed {
		tracker.closed = true
		tracker.closedAt = time.Now()
		if m.remote.closedTail == nil {
			m.remote.closedHead = tracker
		} else {
			m.remote.closedTail.nextClosed = tracker
		}
		m.remote.closedTail = tracker
		m.remote.closedCount++
		close(tracker.done)
	}
	m.remote.txMu.Unlock()
}

// remoteCommitFinal 报告 tracker 的终态：Committed（已持久并发布）与 Rejected（持久拒绝或
// 确定未提交）。Admitted/Applied/Indeterminate/Unknown 都只是过程或未知结论，可以被后续的
// 持久结论覆盖；终态一旦写入就不再改变。
func remoteCommitFinal(state entity.RemoteCommitState) bool {
	return state == entity.RemoteCommitCommitted || state == entity.RemoteCommitRejected
}

func (m *Manager) waitRemoteTransaction(ctx context.Context, id entity.RemoteTransactionID) (entity.RemoteCommitStatus, error) {
	fctx.AssertBlockingAllowed("remoteentity.waitRemoteTransaction")
	if ctx == nil {
		ctx = context.Background()
	}
	tracker, err := m.trackedRemoteTransaction(id)
	if err != nil {
		return entity.RemoteCommitStatus{TransactionID: id, State: entity.RemoteCommitUnknown, Cause: err.Error()}, err
	}
	return m.waitTrackedRemoteTransaction(ctx, tracker)
}

// 单笔等待和 FlushAll 共用指针所有权；缓存淘汰不改变已接受等待的结果。
func (m *Manager) waitTrackedRemoteTransaction(ctx context.Context, tracker *remoteTransactionTracker) (entity.RemoteCommitStatus, error) {
	fctx.AssertBlockingAllowed("remoteentity.waitTrackedRemoteTransaction")
	if ctx == nil {
		ctx = context.Background()
	}
	id := tracker.id
	m.remote.txMu.Lock()
	done := tracker.done
	status := tracker.status.Clone()
	closed := tracker.closed
	m.remote.txMu.Unlock()
	if closed {
		return status, remoteStatusError(status)
	}
	select {
	case <-done:
		// Read through the tracker this call is holding, not through the map:
		// the record may have been evicted while this goroutine was parked.
		// Still under txMu, because completeRemoteTransaction writes status.
		m.remote.txMu.Lock()
		status = tracker.status.Clone()
		m.remote.txMu.Unlock()
		return status, remoteStatusError(status)
	case <-ctx.Done():
		return entity.RemoteCommitStatus{TransactionID: id, State: entity.RemoteCommitUnknown, Cause: ctx.Err().Error()}, errors.Join(entity.ErrRemoteCommitTimeout, ctx.Err())
	}
}

func remoteStatusError(status entity.RemoteCommitStatus) error {
	switch status.State {
	case entity.RemoteCommitPublished, entity.RemoteCommitCommitted:
		return nil
	case entity.RemoteCommitRejected:
		return fmt.Errorf("%w: %s", entity.ErrRemoteRejected, status.Cause)
	case entity.RemoteCommitIndeterminate:
		return fmt.Errorf("%w: %s", entity.ErrRemotePersistenceIndeterminate, status.Cause)
	default:
		return entity.ErrRemoteCommitNotFinalized
	}
}

func (m *Manager) RemoteCommitStatus(ctx context.Context, id entity.RemoteTransactionID) (entity.RemoteCommitStatus, error) {
	if m == nil || m.remote == nil || id.IsZero() {
		return entity.RemoteCommitStatus{}, entity.ErrRemoteRejected
	}
	m.remote.txMu.Lock()
	tracker := m.remote.txs[id]
	if tracker != nil {
		status := tracker.status.Clone()
		if status.State == entity.RemoteCommitCommitted || status.State == entity.RemoteCommitPublished || status.State == entity.RemoteCommitRejected {
			m.remote.txMu.Unlock()
			return status, nil
		}
	}
	m.remote.txMu.Unlock()
	if m.backend != nil {
		fctx.AssertBlockingAllowed("remoteentity.RemoteCommitStatus backend")
		status, err := m.backend.CommitStatus(ctx, id)
		if err == nil && (status.State == entity.RemoteCommitCommitted || status.State == entity.RemoteCommitRejected) {
			m.completeRemoteTransaction(id, status)
		}
		return status.Clone(), err
	}
	return entity.RemoteCommitStatus{TransactionID: id, State: entity.RemoteCommitUnknown}, nil
}

func (m *Manager) FlushRemoteTransaction(ctx context.Context, id entity.RemoteTransactionID) error {
	_, err := m.waitRemoteTransaction(ctx, id)
	return err
}

func (m *Manager) FlushRemoteAll(ctx context.Context) error {
	fctx.AssertBlockingAllowed("remoteentity.FlushRemoteAll")
	m.remote.txMu.Lock()
	trackers := make([]*remoteTransactionTracker, 0, len(m.remote.txs)-m.remote.closedCount)
	for _, tracker := range m.remote.txs {
		if !tracker.closed {
			trackers = append(trackers, tracker)
		}
	}
	m.remote.txMu.Unlock()
	for _, tracker := range trackers {
		if _, err := m.waitTrackedRemoteTransaction(ctx, tracker); err != nil {
			return err
		}
	}
	return nil
}

// RejectRemoteTransaction 仅由投影器在持久拒绝完成后通知；finalizer 仍可回源恢复此结论。
func (m *Manager) RejectRemoteTransaction(id entity.RemoteTransactionID, cause string) {
	m.completeRemoteTransaction(id, entity.RemoteCommitStatus{TransactionID: id, State: entity.RemoteCommitRejected, Cause: cause})
}
