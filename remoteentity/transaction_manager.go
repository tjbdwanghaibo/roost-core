package remoteentity

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tjbdwanghaibo/roost-core/cache"
	"github.com/tjbdwanghaibo/roost-core/entity"
	"github.com/tjbdwanghaibo/roost-core/metrics"
)

type remoteVersionWaiter struct {
	version uint64
	done    chan struct{}
}

type remoteState struct {
	cache     *entity.RemoteSnapshotCache
	interests *remoteInterestRegistry
	// interestGeneration stamps this consumer's renewals and releases. Seeded
	// from the clock so a restart that reuses the SID starts above anything
	// the previous process could have issued, instead of at zero where an
	// old release in flight would outrank every new renewal.
	interestGeneration    atomic.Uint64
	localInterestMu       sync.Mutex
	localInterestLocks    [64]sync.Mutex
	localInterests        map[entity.RemoteSnapshotKey]int64
	localInterestOps      atomic.Uint64
	localInterestCapacity int

	txMu sync.Mutex
	txs  map[entity.RemoteTransactionID]*remoteTransactionTracker
	// 终态按首次完成顺序链接，和 txs 共用 txMu；等待者单独持有 tracker。
	closedHead  *remoteTransactionTracker
	closedTail  *remoteTransactionTracker
	closedCount int

	versionMu    sync.Mutex
	versions     map[int64]uint64
	versionOrder []int64
	maxVersions  int
	waiters      map[int64][]remoteVersionWaiter
	waiterCount  int
	maxWaiters   int
	txCapacity   int
	txTTL        time.Duration

	finalizeCtx    context.Context
	finalizeCancel context.CancelFunc
	finalizeQueue  chan deferredRemoteClose
	writeSlots     chan struct{}
	writeRejected  atomic.Uint64
	finalizeDone   chan struct{}
	finalizeWG     sync.WaitGroup
	retryWG        sync.WaitGroup
	retryMu        sync.Mutex
	stopping       bool
	finalizeOnce   sync.Once
}

type deferredRemoteClose struct {
	txID entity.RemoteTransactionID
	// durability 决定 Unknown 由谁收敛：1/2 有 WAL，投影会写出 Applied 或持久拒绝；
	// 0 没有 WAL，只能由 finalizer 自己写下持久拒绝（rejectUnresolvedMemoryTransaction）。
	durability uint8
	entries    []*remoteWriteEntry
	attempt    int
}

// remoteUnresolvedRejecter 以事务 _id 持久写入拒绝，撞键时返回已有事务的真实状态。
// MongoCommitter 与正式 Backend 实现它；其他存储没有这项能力时 Durability 0 的未知结果
// 保持隔离重试，不能在无持久结论时释放。
type remoteUnresolvedRejecter interface {
	RejectUnresolvedRemoteCommits(context.Context, []entity.RemoteCommit, string) (entity.RemoteCommitStatus, error)
}

const unresolvedMemoryRejectCause = "memory durability outcome unresolved; rejected by finalizer"

func newRemoteState(mgr *Manager, cfg *Config, snapshotL2 ...cache.Store[entity.RemoteSnapshotKey, entity.RemoteSnapshotEnvelope]) *remoteState {
	finalizeCtx, finalizeCancel := context.WithCancel(context.Background())
	capacity := cfg.AsyncFinalizeCapacity
	if capacity <= 0 {
		capacity = 4096
	}
	// 一个写批次只预留一次，转入后台收尾也不归还。
	// 上限不能超过收尾容量，否则释放路径可能反过来卡住慢 worker。
	writeLimit := capacity
	if cfg.MaxConcurrentWrites > 0 {
		writeLimit = min(writeLimit, cfg.MaxConcurrentWrites)
	}
	state := &remoteState{
		txs:         make(map[entity.RemoteTransactionID]*remoteTransactionTracker),
		txCapacity:  cfg.TransactionTrackLimit,
		txTTL:       cfg.TransactionTrackTTL,
		versions:    make(map[int64]uint64),
		maxVersions: cfg.SnapshotCacheEntries,
		waiters:     make(map[int64][]remoteVersionWaiter),
		maxWaiters:  cfg.SnapshotMaxWaiters,
		interests:   newRemoteInterestRegistry(cfg.SnapshotInterestKeys, cfg.SnapshotInterestSubs),
		// (interestGeneration is seeded right after construction, below.)
		localInterests:        make(map[entity.RemoteSnapshotKey]int64),
		localInterestCapacity: cfg.SnapshotInterestKeys,
		finalizeCtx:           finalizeCtx, finalizeCancel: finalizeCancel,
		finalizeQueue: make(chan deferredRemoteClose, capacity),
		writeSlots:    make(chan struct{}, writeLimit), finalizeDone: make(chan struct{}),
	}
	var l2 cache.Store[entity.RemoteSnapshotKey, entity.RemoteSnapshotEnvelope]
	if len(snapshotL2) > 0 {
		l2 = snapshotL2[0]
	}
	state.cache = entity.NewRemoteSnapshotCache(entity.RemoteSnapshotCacheConfig{
		Shards: cfg.SnapshotCacheShards, MaxEntries: cfg.SnapshotCacheEntries,
		MaxBytes: cfg.SnapshotCacheBytes, TTL: cfg.SnapshotCacheTTL,
		LoadTimeout: cfg.SnapshotLoadTimeout, MaxWaiters: cfg.SnapshotMaxWaiters,
	}, l2, func(ctx context.Context, key entity.RemoteSnapshotKey, consistency entity.RemoteReadConsistency, minVersion uint64) (entity.RemoteSnapshotEnvelope, bool, error) {
		if mgr == nil || mgr.backend == nil {
			return entity.RemoteSnapshotEnvelope{}, false, nil
		}
		return mgr.backend.LoadRemoteSnapshot(ctx, key, consistency, minVersion)
	})
	return state
}

func (m *Manager) StartFinalizer() {
	if m == nil || m.remote == nil {
		return
	}
	workers := m.cfg.AsyncFinalizeWorkers
	if workers <= 0 {
		workers = 16
	}
	m.remote.finalizeOnce.Do(func() { go m.runRemoteFinalizers(m.remote, workers) })
}

func (m *Manager) reserveRemoteWriteSlot() bool {
	if m == nil || m.remote == nil {
		return false
	}
	select {
	case m.remote.writeSlots <- struct{}{}:
		return true
	default:
		m.remote.writeRejected.Add(1)
		metrics.IncCounter("remote_entity.write_admission_rejected_total", nil, 1)
		return false
	}
}

func (m *Manager) releaseRemoteWriteSlot() {
	if m == nil || m.remote == nil {
		return
	}
	select {
	case <-m.remote.writeSlots:
	default:
	}
}

// deferRemoteClose hands one deferred close to the finalizers, or refuses so
// the caller cleans up synchronously. Exactly one of the two, ever.
//
// The refusal has to be decided under the SAME barrier the stop uses, not by a
// select race. finalizeCtx.Done being ready does not stop the select from
// picking a queue that still has room, so after the stop had completed a
// handoff could still be accepted into a queue nobody drains any more — and
// batch.Close, told the handoff succeeded, dropped its entries, writeGate,
// ownership read lock and finalize slot on the floor, with finalizeOnce
// preventing any restart that might have collected them (RR-20260911-03).
//
// Registering on retryWG before releasing retryMu is what makes it safe:
// runRemoteFinalizers waits on retryWG before its final drain, so a sender
// admitted here is always drained, and StopFinalizer sets stopping under the
// same lock, so no sender is admitted after it.
func (m *Manager) deferRemoteClose(item deferredRemoteClose) error {
	state := m.remote
	state.retryMu.Lock()
	if state.stopping {
		state.retryMu.Unlock()
		if err := state.finalizeCtx.Err(); err != nil {
			return err
		}
		return context.Canceled
	}
	state.retryWG.Add(1)
	state.retryMu.Unlock()
	defer state.retryWG.Done()

	select {
	case state.finalizeQueue <- item:
		return nil
	case <-state.finalizeCtx.Done():
		return state.finalizeCtx.Err()
	}
}

func (m *Manager) runRemoteFinalizers(state *remoteState, workers int) {
	state.finalizeWG.Add(workers)
	for range workers {
		go m.runRemoteFinalizerWorker(state)
	}
	state.finalizeWG.Wait()
	state.retryWG.Wait()
	// Retry goroutines send without holding a lock, so one may race the
	// workers' final drain and land an item after they exited. All senders
	// are done here (retryWG), so one last drain guarantees every deferred
	// close releases its entries and slot before finalizeDone is published.
	for {
		select {
		case item := <-state.finalizeQueue:
			if quarantineErr := m.quarantineEntries(item.entries, state.finalizeCtx.Err()); quarantineErr != nil {
				metrics.IncCounter("remote_entity.quarantine_error_total", nil, 1)
			}
			m.releaseRemoteEntriesObserved(context.Background(), item.entries)
			m.releaseRemoteWriteSlot()
		default:
			close(state.finalizeDone)
			return
		}
	}
}

func (m *Manager) runRemoteFinalizerWorker(state *remoteState) {
	defer state.finalizeWG.Done()
	for {
		select {
		case item := <-state.finalizeQueue:
			m.processDeferredRemoteClose(state, item)
		case <-state.finalizeCtx.Done():
			for {
				select {
				case item := <-state.finalizeQueue:
					if quarantineErr := m.quarantineEntries(item.entries, state.finalizeCtx.Err()); quarantineErr != nil {
						metrics.IncCounter("remote_entity.quarantine_error_total", nil, 1)
					}
					m.releaseRemoteEntriesObserved(context.Background(), item.entries)
					m.releaseRemoteWriteSlot()
				default:
					return
				}
			}
		}
	}
}

// processDeferredRemoteClose 只在拿到持久结论后收尾：Applied 先发布再释放，Committed
// 确认后释放，Rejected 回滚、解除本事务造成的隔离后释放。其余情况保留 gate、fence、
// 写额度并隔离实体，按退避重试。
func (m *Manager) processDeferredRemoteClose(state *remoteState, item deferredRemoteClose) {
	status, err := m.RemoteCommitStatus(state.finalizeCtx, item.txID)
	if err == nil && status.State == entity.RemoteCommitUnknown && item.durability == 0 {
		status, err = m.rejectUnresolvedMemoryTransaction(state.finalizeCtx, item)
	}
	if err == nil && status.State == entity.RemoteCommitApplied {
		err = m.publishAppliedRemoteTransaction(state.finalizeCtx, status)
		if err == nil {
			m.releaseRemoteEntriesObserved(context.Background(), item.entries)
			m.releaseRemoteWriteSlot()
			return
		}
	}
	if err == nil && status.State == entity.RemoteCommitCommitted {
		err = m.reconcileRemoteEntries(state.finalizeCtx, item.entries, status.Receipts)
		if err == nil {
			m.releaseRemoteEntriesObserved(context.Background(), item.entries)
			m.releaseRemoteWriteSlot()
			return
		}
	}
	if status.State == entity.RemoteCommitRejected {
		m.rollbackRejectedRemoteEntries(state.finalizeCtx, item.entries)
		m.releaseRemoteEntriesObserved(context.Background(), item.entries)
		m.releaseRemoteWriteSlot()
		return
	}
	// One failed/pending transaction must not monopolize a finalizer worker.
	// Keep its write gate and fence, quarantine the live object, and retry with
	// bounded exponential backoff on a timer.
	if quarantineErr := m.quarantineEntries(item.entries, err); quarantineErr != nil {
		metrics.IncCounter("remote_entity.quarantine_error_total", nil, 1)
	}
	metrics.IncCounter("remote_entity.finalize_retry_total", nil, 1)
	item.attempt++
	delay := m.cfg.FinalizeRetryInterval
	if delay <= 0 {
		delay = 100 * time.Millisecond
	}
	for i := 1; i < item.attempt && delay < 5*time.Second; i++ {
		delay *= 2
	}
	if delay > 5*time.Second {
		delay = 5 * time.Second
	}
	state.retryWG.Add(1)
	go func() {
		defer state.retryWG.Done()
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-state.finalizeCtx.Done():
			m.releaseRemoteEntriesObserved(context.Background(), item.entries)
			m.releaseRemoteWriteSlot()
			return
		case <-timer.C:
		}
		state.retryMu.Lock()
		stopping := state.stopping
		state.retryMu.Unlock()
		if stopping {
			m.releaseRemoteEntriesObserved(context.Background(), item.entries)
			m.releaseRemoteWriteSlot()
			return
		}
		// Send outside the lock: holding retryMu across a bounded-queue send
		// stalls Stop behind a full queue. The Done branch and the final
		// drain in runRemoteFinalizers together guarantee the item is always
		// consumed or released, whichever side wins the race.
		select {
		case state.finalizeQueue <- item:
		case <-state.finalizeCtx.Done():
			m.releaseRemoteEntriesObserved(context.Background(), item.entries)
			m.releaseRemoteWriteSlot()
		}
	}()
}

// rejectUnresolvedMemoryTransaction 为回源读不到的 Durability 0 事务写下持久结论。
//
// 这类事务只有 ApplyRemoteCommits 那一次提交尝试，没有 WAL 会重放它；后端返回瞬时
// 错误时事务可能根本没有到达 Mongo，CommitStatus 永远是 Unknown（RR-20260926-28）。
// 调用时本次提交尝试已经返回（batch.Close 在 Commit 之后才转交 finalizer），但服务端
// 仍可能有在途的提交，所以不能直接当成未提交：用同一事务 _id 写入拒绝，由唯一索引
// 在“拒绝”和“迟到提交”之间裁决。插入成功即 Rejected；撞键则返回事务的真实状态
// （Applied/Committed 走发布与确认，不回滚）。任何错误都意味着仍无结论，交回调用方重试。
func (m *Manager) rejectUnresolvedMemoryTransaction(ctx context.Context, item deferredRemoteClose) (entity.RemoteCommitStatus, error) {
	unknown := entity.RemoteCommitStatus{TransactionID: item.txID, State: entity.RemoteCommitUnknown}
	rejecter, ok := m.backend.(remoteUnresolvedRejecter)
	if !ok {
		return unknown, fmt.Errorf("%w: backend cannot persist a rejection for unresolved transaction %s", entity.ErrRemotePersistenceIndeterminate, item.txID)
	}
	commits := finalizedRemoteCommits(item.entries)
	if len(commits) == 0 {
		return unknown, fmt.Errorf("%w: unresolved transaction %s has no finalized commits", entity.ErrRemotePersistenceIndeterminate, item.txID)
	}
	status, err := rejecter.RejectUnresolvedRemoteCommits(ctx, commits, unresolvedMemoryRejectCause)
	if err != nil {
		metrics.IncCounter("remote_entity.unresolved_reject_error_total", nil, 1)
		return unknown, fmt.Errorf("remote_entity: persist rejection for transaction %s: %w", item.txID, err)
	}
	if status.TransactionID.IsZero() {
		status.TransactionID = item.txID
	}
	if status.State == entity.RemoteCommitRejected || status.State == entity.RemoteCommitCommitted {
		m.completeRemoteTransaction(item.txID, status)
	}
	metrics.IncCounter("remote_entity.unresolved_resolved_total", metrics.Labels{"state": remoteCommitStateLabel(status.State)}, 1)
	return status, nil
}

func remoteCommitStateLabel(state entity.RemoteCommitState) string {
	switch state {
	case entity.RemoteCommitRejected:
		return "rejected"
	case entity.RemoteCommitApplied:
		return "applied"
	case entity.RemoteCommitCommitted:
		return "committed"
	default:
		return "other"
	}
}

// rollbackRejectedRemoteEntries 在持久拒绝后回滚本事务的定稿提交并解除本事务造成的隔离。
//
// finalizer 不是 Nest 快 worker，也拿不到已结束消息的快池续行；回滚经 entity.RunLocal
// 的本地执行路径（finalizeCtx 未注入执行器时就地执行），在每个实体自己的本地锁内进行，
// 与快池 handler 经 Guard 取得的是同一把锁，hook panic 也会解锁（rollbackRemoteEntries）。
//
// 隔离只退到 Recovering 并作废 marker 缓存：持有 gate 期间没有其他写者，实体此时的
// Quarantined 只能来自本事务的未知结果；拒绝后 Mongo 保持 BaseVersion，本地版本也未前进，
// 但仍要求下一写者经 beginWrite 重新向权威读取 ownership/许可与版本后才恢复写入。
func (m *Manager) rollbackRejectedRemoteEntries(ctx context.Context, entries []*remoteWriteEntry) {
	var thawErr error
	runErr := entity.RunLocal(ctx, func() {
		m.rollbackRemoteEntries(entries)
		for _, entry := range entries {
			if entry == nil || entry.entity == nil || entry.entity.GetMutex() == nil {
				continue
			}
			func() {
				mu := entry.entity.GetMutex()
				mu.Lock()
				defer mu.Unlock()
				if entry.entity.RemoteOwnershipState() != entity.RemoteOwnershipQuarantined {
					return
				}
				if err := entry.entity.TransitionRemoteOwnership(entity.RemoteOwnershipRecovering); err != nil {
					thawErr = errors.Join(thawErr, fmt.Errorf("remote_entity: thaw rejected entity %d: %w", entry.entity.ID(), err))
				}
			}()
			if entry.wrapper != nil {
				entry.wrapper.invalidateMarker()
			}
		}
	})
	if err := errors.Join(runErr, thawErr); err != nil {
		// 拒绝结论已持久，释放照常进行；解冻失败的实体保持隔离，只是不可写，不会越权。
		metrics.IncCounter("remote_entity.quarantine_error_total", nil, 1)
	}
}

func (m *Manager) reconcileRemoteEntries(ctx context.Context, entries []*remoteWriteEntry, receipts []entity.RemoteCommitReceipt) error {
	byEntity := make(map[int64]entity.RemoteCommitReceipt, len(receipts))
	for _, receipt := range receipts {
		byEntity[receipt.EntityID] = receipt
	}
	for _, entry := range entries {
		if entry == nil || !entry.finalized {
			continue
		}
		receipt, ok := byEntity[entry.commit.EntityID]
		if !ok {
			return fmt.Errorf("%w: committed transaction missing receipt for entity %d", entity.ErrRemotePersistenceIndeterminate, entry.commit.EntityID)
		}
		if err := m.afterRemoteCommit(ctx, entry.commit.Clone(), receipt); err != nil {
			return err
		}
	}
	return nil
}

func (m *Manager) StopFinalizer(ctx context.Context) error {
	if m == nil || m.remote == nil {
		return nil
	}
	m.StartFinalizer()
	m.remote.retryMu.Lock()
	m.remote.stopping = true
	m.remote.finalizeCancel()
	m.remote.retryMu.Unlock()
	select {
	case <-m.remote.finalizeDone:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *Manager) releaseRemoteEntries(parent context.Context, entries []*remoteWriteEntry) error {
	if parent == nil {
		parent = context.Background()
	}
	var joined error
	for i := len(entries) - 1; i >= 0; i-- {
		entry := entries[i]
		if entry == nil || entry.wrapper == nil {
			continue
		}
		if entry.distLock && entry.wrapper.rMu.IsAcquired() {
			version := int64(entry.lease.BaseVersion)
			if entry.finalized {
				version = int64(entry.commit.NextVersion)
			}
			ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), m.cfg.OpTimeout)
			err := entry.wrapper.rMu.UnlockWithRetry(ctx, version, m.cfg.VersionTTL, m.cfg.UnlockRetryCount, m.cfg.UnlockRetryInterval)
			cancel()
			if err != nil {
				joined = errors.Join(joined, entity.ErrRemoteReleaseIncomplete, err)
			}
		}
		entry.wrapper.ownershipMu.RUnlock()
		<-entry.wrapper.writeGate
		entry.wrapper.release()
	}
	return joined
}

func (m *Manager) releaseRemoteEntriesObserved(parent context.Context, entries []*remoteWriteEntry) {
	if err := m.releaseRemoteEntries(parent, entries); err != nil {
		m.recordReleaseFailure(err)
	}
}

func (m *Manager) ApplyRemoteCommits(ctx context.Context, txID entity.RemoteTransactionID, commits []entity.RemoteCommit) (receipts []entity.RemoteCommitReceipt, err error) {
	started := time.Now()
	defer func() {
		result := "ok"
		if err != nil {
			result = "error"
		}
		labels := metrics.Labels{"result": result}
		metrics.IncCounter("remote_entity.remote.apply_total", labels, 1)
		metrics.ObserveDuration("remote_entity.remote.apply_latency", labels, time.Since(started))
	}()
	if m == nil || m.backend == nil {
		return nil, entity.ErrRemoteWriteCapabilityDisabled
	}
	if txID.IsZero() {
		return nil, entity.ErrRemoteRejected
	}
	if len(commits) == 0 {
		return nil, fmt.Errorf("%w: empty commit batch", entity.ErrRemoteRejected)
	}
	cloned := make([]entity.RemoteCommit, len(commits))
	for i, commit := range commits {
		if commit.TransactionID != txID {
			return nil, fmt.Errorf("%w: transaction mismatch", entity.ErrRemoteRejected)
		}
		if err := commit.Validate(); err != nil {
			return nil, err
		}
		if err := validateRemoteCommitEncodable(commit); err != nil {
			return nil, err
		}
		cloned[i] = commit.Clone()
	}
	// 校验失败不能留下无法完成的 pending tracker；已存在的事务也不应被坏输入覆盖。
	if err := m.trackRemoteTransaction(txID); err != nil {
		return nil, err
	}
	if len(cloned) > 1 {
		receipts, err = m.backend.CommitRemoteBatch(ctx, cloned)
	} else if len(cloned) == 1 {
		var receipt entity.RemoteCommitReceipt
		receipt, err = m.backend.CommitRemote(ctx, cloned[0])
		receipts = []entity.RemoteCommitReceipt{receipt}
	}
	if err != nil {
		state := entity.RemoteCommitIndeterminate
		if errors.Is(err, entity.ErrRemoteFenced) || errors.Is(err, entity.ErrRemoteVersionConflict) || errors.Is(err, entity.ErrRemoteRejected) {
			state = entity.RemoteCommitRejected
		}
		m.completeRemoteTransaction(txID, entity.RemoteCommitStatus{TransactionID: txID, State: state, Cause: err.Error()})
		if state == entity.RemoteCommitIndeterminate {
			err = errors.Join(entity.ErrRemotePersistenceIndeterminate, err)
		}
		return nil, err
	}
	if len(receipts) != len(cloned) {
		err = fmt.Errorf("%w: receipt count=%d commits=%d", entity.ErrRemotePersistenceIndeterminate, len(receipts), len(cloned))
		m.completeRemoteTransaction(txID, entity.RemoteCommitStatus{TransactionID: txID, State: entity.RemoteCommitIndeterminate, Cause: err.Error()})
		return nil, err
	}
	for i := range cloned {
		if err := m.afterRemoteCommit(ctx, cloned[i], receipts[i]); err != nil {
			m.completeRemoteTransaction(txID, entity.RemoteCommitStatus{TransactionID: txID, State: entity.RemoteCommitIndeterminate, Receipts: receipts, Cause: err.Error()})
			return nil, errors.Join(entity.ErrRemotePersistenceIndeterminate, err)
		}
	}
	if err := m.backend.MarkRemoteCommitPublished(ctx, txID); err != nil {
		m.completeRemoteTransaction(txID, entity.RemoteCommitStatus{TransactionID: txID, State: entity.RemoteCommitIndeterminate, Receipts: receipts, Commits: cloned, Cause: err.Error()})
		return nil, errors.Join(entity.ErrRemotePersistenceIndeterminate, err)
	}
	status := entity.RemoteCommitStatus{TransactionID: txID, State: entity.RemoteCommitCommitted, Receipts: append([]entity.RemoteCommitReceipt(nil), receipts...)}
	m.completeRemoteTransaction(txID, status)
	return receipts, nil
}

func (m *Manager) RecoverOutbox(ctx context.Context) error {
	outbox := m.backend
	for {
		pending, err := outbox.PendingRemoteCommits(ctx, 256)
		if err != nil {
			return fmt.Errorf("remote_entity: load commit outbox: %w", err)
		}
		if len(pending) == 0 {
			return nil
		}
		for _, status := range pending {
			if err := m.publishAppliedRemoteTransaction(ctx, status); err != nil {
				return fmt.Errorf("remote_entity: recover transaction %s: %w", status.TransactionID, err)
			}
		}
		if len(pending) < 256 {
			return nil
		}
	}
}

func (m *Manager) publishAppliedRemoteTransaction(ctx context.Context, status entity.RemoteCommitStatus) error {
	if status.State != entity.RemoteCommitApplied || status.TransactionID.IsZero() || len(status.Commits) == 0 || len(status.Commits) != len(status.Receipts) {
		return fmt.Errorf("remote_entity: corrupt commit outbox transaction %s", status.TransactionID)
	}
	if m.backend == nil {
		return entity.ErrRemoteWriteCapabilityDisabled
	}
	for i := range status.Commits {
		commit := status.Commits[i].Clone()
		if commit.TransactionID != status.TransactionID {
			return fmt.Errorf("remote_entity: outbox transaction mismatch %s", status.TransactionID)
		}
		if err := m.afterRemoteCommit(ctx, commit, status.Receipts[i]); err != nil {
			return err
		}
	}
	if err := m.backend.MarkRemoteCommitPublished(ctx, status.TransactionID); err != nil {
		return err
	}
	m.completeRemoteTransaction(status.TransactionID, entity.RemoteCommitStatus{
		TransactionID: status.TransactionID,
		State:         entity.RemoteCommitCommitted,
		Receipts:      append([]entity.RemoteCommitReceipt(nil), status.Receipts...),
	})
	return nil
}

func (m *Manager) ReadRemoteSnapshot(ctx context.Context, key entity.RemoteSnapshotKey, consistency entity.RemoteReadConsistency, minVersion uint64) (snapshot entity.RemoteSnapshotEnvelope, found bool, err error) {
	started := time.Now()
	defer func() {
		result := "hit"
		if err != nil {
			result = "error"
		} else if !found {
			result = "miss"
		}
		labels := metrics.Labels{"result": result, "consistency": remoteConsistencyLabel(consistency)}
		metrics.IncCounter("remote_entity.remote.read_total", labels, 1)
		metrics.ObserveDuration("remote_entity.remote.read_latency", labels, time.Since(started))
	}()
	if m == nil || m.remote == nil || m.remote.cache == nil || !key.Valid() {
		return entity.RemoteSnapshotEnvelope{}, false, entity.ErrRemoteRejected
	}
	if ctx == nil {
		ctx = context.Background()
	}
	// Reads automatically renew soft-state interest for this exact scope/policy.
	// A lost renewal only causes a later authoritative refill, never stale apply.
	_ = m.RenewRemoteSnapshotInterest(ctx, key)
	if consistency == entity.RemoteReadLinearizable {
		return m.remote.cache.LoadAuthoritative(ctx, key, consistency, minVersion)
	}
	snapshot, found, err = m.remote.cache.Get(ctx, key, consistency, minVersion)
	if err == nil && found {
		return snapshot, true, nil
	}
	if consistency == entity.RemoteReadCached && !errors.Is(err, entity.ErrRemoteSnapshotStale) {
		return snapshot, found, err
	}
	if consistency == entity.RemoteReadMonotonic && minVersion > 0 {
		waitCtx, cancel := context.WithTimeout(ctx, 2*time.Millisecond)
		waitErr := m.remote.cache.WaitForVersion(waitCtx, key, minVersion)
		cancel()
		if waitErr == nil {
			return m.remote.cache.Get(ctx, key, consistency, minVersion)
		}
	}
	return m.remote.cache.LoadAuthoritative(ctx, key, consistency, minVersion)
}

func remoteConsistencyLabel(consistency entity.RemoteReadConsistency) string {
	switch consistency {
	case entity.RemoteReadCached:
		return "cached"
	case entity.RemoteReadLinearizable:
		return "linearizable"
	default:
		return "monotonic"
	}
}

var _ entity.RemoteSnapshotInterestManager = (*Manager)(nil)

type remoteInterestPublisher interface {
	PublishRemoteInterest(context.Context, entity.RemoteSnapshotInterest, bool) error
}

func (m *Manager) RenewRemoteSnapshotInterest(ctx context.Context, key entity.RemoteSnapshotKey) error {
	if m == nil || m.remote == nil || !key.Valid() {
		return entity.ErrRemoteRejected
	}
	ttl := m.cfg.SnapshotInterestTTL
	now := time.Now().UnixNano()
	interest := entity.RemoteSnapshotInterest{ConsumerSID: m.localSid, Key: key, ExpiresAt: now + ttl.Nanoseconds(), Generation: m.nextInterestGeneration()}
	stripe := &m.remote.localInterestLocks[uint64(key.EntityID)%uint64(len(m.remote.localInterestLocks))]
	stripe.Lock()
	defer stripe.Unlock()
	m.remote.localInterestMu.Lock()
	current, loaded := m.remote.localInterests[key]
	if loaded && current-now > (ttl/2).Nanoseconds() {
		m.remote.localInterestMu.Unlock()
		return nil
	}
	if !loaded && m.remote.localInterestCapacity > 0 && len(m.remote.localInterests) >= m.remote.localInterestCapacity {
		m.pruneLocalInterestsLocked(now)
		if len(m.remote.localInterests) >= m.remote.localInterestCapacity {
			m.remote.localInterestMu.Unlock()
			return entity.ErrRemoteOverloaded
		}
	}
	m.remote.localInterests[key] = interest.ExpiresAt
	m.remote.localInterestMu.Unlock()
	if err := m.remote.interests.renew(interest); err != nil {
		m.rollbackLocalInterest(key, interest.ExpiresAt)
		return err
	}
	if publisher, ok := m.syncer.(remoteInterestPublisher); ok {
		if err := publisher.PublishRemoteInterest(ctx, interest, false); err != nil {
			m.rollbackLocalInterest(key, interest.ExpiresAt)
			return err
		}
	}
	if m.remote.localInterestOps.Add(1)&1023 == 0 {
		m.remote.localInterestMu.Lock()
		m.pruneLocalInterestsLocked(now)
		m.remote.localInterestMu.Unlock()
	}
	return nil
}

// interestGenerationClock is the process-wide high-water mark of every
// interest generation issued by any Manager in this process. A Manager seeds
// strictly above it, so two instances created within the same clock tick (a
// restart harness, coarse clocks on Windows — RR-20260913-02 复核) cannot
// both start at the same UnixNano and re-issue each other's generations. It
// does not survive the process; across restarts the clock is the guarantee.
var interestGenerationClock atomic.Uint64

// interestGenerationNow is the clock behind the seed; a variable so tests can
// freeze it and prove the ordering does not depend on nanosecond resolution.
var interestGenerationNow = func() uint64 { return uint64(time.Now().UnixNano()) }

// seedInterestGeneration returns a seed above both now and every generation
// already issued in this process, and records it as issued.
func seedInterestGeneration(now uint64) uint64 {
	for {
		last := interestGenerationClock.Load()
		next := now
		if next <= last {
			next = last + 1
		}
		if interestGenerationClock.CompareAndSwap(last, next) {
			return next
		}
	}
}

// noteInterestGenerationIssued raises the process-wide high-water mark.
func noteInterestGenerationIssued(generation uint64) {
	for {
		last := interestGenerationClock.Load()
		if generation <= last || interestGenerationClock.CompareAndSwap(last, generation) {
			return
		}
	}
}

// nextInterestGeneration issues the next generation for this consumer's
// interest messages. The first call seeds from the clock (see the field).
func (m *Manager) nextInterestGeneration() uint64 {
	for {
		current := m.remote.interestGeneration.Load()
		if current == 0 {
			seed := seedInterestGeneration(interestGenerationNow())
			if !m.remote.interestGeneration.CompareAndSwap(0, seed) {
				continue
			}
			current = seed
		}
		generation := m.remote.interestGeneration.Add(1)
		noteInterestGenerationIssued(generation)
		return generation
	}
}

func (m *Manager) ReleaseRemoteSnapshotInterest(ctx context.Context, key entity.RemoteSnapshotKey) error {
	if m == nil || m.remote == nil || !key.Valid() {
		return entity.ErrRemoteRejected
	}
	interest := entity.RemoteSnapshotInterest{ConsumerSID: m.localSid, Key: key, ExpiresAt: time.Now().UnixNano(), Generation: m.nextInterestGeneration()}
	stripe := &m.remote.localInterestLocks[uint64(key.EntityID)%uint64(len(m.remote.localInterestLocks))]
	stripe.Lock()
	defer stripe.Unlock()
	m.remote.localInterestMu.Lock()
	delete(m.remote.localInterests, key)
	m.remote.localInterestMu.Unlock()
	m.remote.interests.release(key, m.localSid, interest.Generation)
	if publisher, ok := m.syncer.(remoteInterestPublisher); ok {
		return publisher.PublishRemoteInterest(ctx, interest, true)
	}
	return nil
}

func (m *Manager) afterRemoteCommit(ctx context.Context, commit entity.RemoteCommit, receipt entity.RemoteCommitReceipt) error {
	if receipt.TransactionID != commit.TransactionID || receipt.EntityID != commit.EntityID || receipt.StateVersion != commit.NextVersion || receipt.MarkerEpoch != commit.MarkerEpoch || receipt.LockFence != commit.LockFence || receipt.RouteEpoch != commit.RouteEpoch {
		return fmt.Errorf("remote_entity: invalid commit receipt for %d", commit.EntityID)
	}
	if err := m.acknowledgeRemoteCommit(commit); err != nil {
		return err
	}
	for _, record := range commit.Snapshots {
		envelope := entity.RemoteSnapshotEnvelope{
			Key: record.Key, BaseVersion: record.BaseVersion, StateVersion: record.StateVersion,
			MarkerEpoch: record.MarkerEpoch, RouteEpoch: record.RouteEpoch,
			Schema: record.Schema, Codec: record.Codec, Checksum: record.Checksum, Full: record.Full,
			PublishedAt: time.Now().UnixNano(), Payload: entity.CopyFrozenRemoteSnapshotPayload(record.Data),
		}
		if err := m.remote.cache.Publish(ctx, envelope); err != nil {
			return err
		}
		if publisher, ok := m.snapshotPublisher(); ok {
			if err := publisher.PublishRemoteSnapshot(ctx, record.Clone()); err != nil {
				return err
			}
		}
	}
	for _, key := range commit.Invalidations {
		// Same rule as the replica side (U-0187): the local copy is deleted
		// at the commit's version so a concurrent older publish for the key
		// cannot repopulate it behind this commit.
		if err := m.remote.cache.DeleteAtVersion(ctx, key, commit.NextVersion); err != nil {
			return err
		}
		if publisher, ok := m.snapshotPublisher(); ok {
			if err := publisher.DeleteRemoteSnapshot(ctx, key, commit.NextVersion); err != nil {
				return err
			}
		}
	}
	m.notifyRemoteVersion(commit.EntityID, commit.NextVersion)
	return nil
}

func (m *Manager) snapshotPublisher() (entity.IRemoteSnapshotPublisher, bool) {
	if publisher, ok := m.syncer.(entity.IRemoteSnapshotPublisher); ok {
		return publisher, true
	}
	return nil, false
}

func (m *Manager) rollbackLocalInterest(key entity.RemoteSnapshotKey, expiresAt int64) {
	if m == nil || m.remote == nil {
		return
	}
	m.remote.localInterestMu.Lock()
	if m.remote.localInterests[key] == expiresAt {
		delete(m.remote.localInterests, key)
		// This process withdrawing its own lease locally: no message was
		// reordered, so the latest generation it issued is the right stamp.
		m.remote.interests.release(key, m.localSid, m.remote.interestGeneration.Load())
	}
	m.remote.localInterestMu.Unlock()
}

func (m *Manager) pruneLocalInterestsLocked(now int64) {
	for key, expiresAt := range m.remote.localInterests {
		if expiresAt <= now {
			delete(m.remote.localInterests, key)
			m.remote.interests.release(key, m.localSid, m.remote.interestGeneration.Load())
		}
	}
}

func (m *Manager) FlushRemoteEntity(ctx context.Context, id int64, minVersion uint64) error {
	if minVersion == 0 {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	m.remote.versionMu.Lock()
	if m.remote.versions[id] >= minVersion {
		m.remote.versionMu.Unlock()
		return nil
	}
	waiter := remoteVersionWaiter{version: minVersion, done: make(chan struct{})}
	if m.remote.maxWaiters > 0 && m.remote.waiterCount >= m.remote.maxWaiters {
		m.remote.versionMu.Unlock()
		return entity.ErrRemoteOverloaded
	}
	m.remote.waiters[id] = append(m.remote.waiters[id], waiter)
	m.remote.waiterCount++
	m.remote.versionMu.Unlock()
	select {
	case <-waiter.done:
		return nil
	case <-ctx.Done():
		m.removeRemoteVersionWaiter(id, waiter.done)
		return ctx.Err()
	}
}

func (m *Manager) notifyRemoteVersion(id int64, version uint64) {
	m.remote.versionMu.Lock()
	if _, exists := m.remote.versions[id]; !exists {
		m.remote.versionOrder = append(m.remote.versionOrder, id)
	}
	if version > m.remote.versions[id] {
		m.remote.versions[id] = version
	}
	waiters := m.remote.waiters[id]
	remaining := waiters[:0]
	for _, waiter := range waiters {
		if version >= waiter.version {
			close(waiter.done)
			m.remote.waiterCount--
		} else {
			remaining = append(remaining, waiter)
		}
	}
	if len(remaining) == 0 {
		delete(m.remote.waiters, id)
	} else {
		m.remote.waiters[id] = remaining
	}
	for m.remote.maxVersions > 0 && len(m.remote.versions) > m.remote.maxVersions && len(m.remote.versionOrder) > 0 {
		oldest := m.remote.versionOrder[0]
		m.remote.versionOrder = m.remote.versionOrder[1:]
		if len(m.remote.waiters[oldest]) == 0 {
			delete(m.remote.versions, oldest)
		} else {
			m.remote.versionOrder = append(m.remote.versionOrder, oldest)
			break
		}
	}
	m.remote.versionMu.Unlock()
}

func (m *Manager) removeRemoteVersionWaiter(id int64, done chan struct{}) {
	m.remote.versionMu.Lock()
	waiters := m.remote.waiters[id]
	for i := range waiters {
		if waiters[i].done == done {
			waiters = append(waiters[:i], waiters[i+1:]...)
			m.remote.waiterCount--
			break
		}
	}
	if len(waiters) == 0 {
		delete(m.remote.waiters, id)
	} else {
		m.remote.waiters[id] = waiters
	}
	m.remote.versionMu.Unlock()
}

func (m *Manager) quarantineEntries(entries []*remoteWriteEntry, cause error) error {
	var joined error
	for _, entry := range entries {
		if entry == nil || entry.entity == nil {
			continue
		}
		if remote, ok := entry.entity.(entity.IThreadSafeRemoteEntity); ok {
			transitions := []entity.RemoteOwnershipState{entity.RemoteOwnershipFenced, entity.RemoteOwnershipRecovering, entity.RemoteOwnershipQuarantined}
			switch remote.RemoteOwnershipState() {
			case entity.RemoteOwnershipUnknown:
				transitions = transitions[1:]
			case entity.RemoteOwnershipFenced:
				transitions = transitions[1:]
			case entity.RemoteOwnershipRecovering:
				transitions = transitions[2:]
			case entity.RemoteOwnershipQuarantined:
				continue
			}
			for _, target := range transitions {
				if err := remote.TransitionRemoteOwnership(target); err != nil {
					joined = errors.Join(joined, fmt.Errorf("remote_entity: quarantine entity %d at %s: %w", entry.entity.ID(), target, err))
					break
				}
			}
		}
	}
	if joined != nil {
		return errors.Join(cause, joined)
	}
	return nil
}

func (m *Manager) rollbackRemoteEntries(entries []*remoteWriteEntry) {
	for _, entry := range entries {
		if entry == nil || entry.entity == nil || !entry.finalized || entry.entity.GetMutex() == nil {
			continue
		}
		func() {
			mu := entry.entity.GetMutex()
			mu.Lock()
			// 自定义回滚 hook panic 也必须解锁，不能把后续快 worker 永久卡住。
			defer mu.Unlock()
			if participant, ok := entry.entity.(entity.IRemoteCommitParticipant); ok {
				participant.RollbackRemoteCommit(entry.commit.Clone())
			}
		}()
	}
}

var _ entity.RemoteWriteBatchManager = (*Manager)(nil)
var _ entity.RemoteCommitApplier = (*Manager)(nil)
var _ entity.RemoteSnapshotReader = (*Manager)(nil)

// SupportsConcurrentRemoteCommits 声明独立 Entity 的投影/发布可并行。
// 调用方负责保持同一 Entity 的版本顺序，Manager 继续逐笔确认完整发布。
func (*Manager) SupportsConcurrentRemoteCommits() bool { return true }

// 持久回执重放仍需发布历史快照，但不得用旧事务覆盖已获得更高许可的本地实体。
// 只在已验证回执的路径使用此判断；新写入的 fence 校验保持严格。
func remoteReceiptObsolete(live entity.IThreadSafeEntity, commit entity.RemoteCommit) bool {
	remote, ok := live.(entity.IThreadSafeRemoteEntity)
	if !ok {
		return false
	}
	v := remote.RemoteVersionVector()
	return v.StateVersion >= commit.NextVersion && v.MarkerEpoch >= commit.MarkerEpoch && v.RouteEpoch >= commit.RouteEpoch && v.LockFence >= commit.LockFence &&
		(v.StateVersion > commit.NextVersion || v.LockFence > commit.LockFence || v.MarkerEpoch > commit.MarkerEpoch || v.RouteEpoch > commit.RouteEpoch)
}
func (m *Manager) acknowledgeRemoteCommit(commit entity.RemoteCommit) error {
	wrapper, ok := m.get(commit.EntityID)
	if !ok || wrapper == nil {
		return nil
	}
	live := wrapper.attachedEntity()
	if live == nil || remoteReceiptObsolete(live, commit) {
		return nil
	}
	if remote, ok := live.(entity.IThreadSafeRemoteEntity); ok {
		vector := entity.RemoteVersionVector{StateVersion: commit.NextVersion, MarkerEpoch: commit.MarkerEpoch, LockFence: commit.LockFence, RouteEpoch: commit.RouteEpoch}
		if err := remote.SetRemoteVersionVector(vector); err != nil {
			// 并发取得新许可可能发生在上面的读取之后；再次核对完整向量。
			if remoteReceiptObsolete(live, commit) {
				return nil
			}
			return err
		}
		if remote.RemoteOwnershipState() == entity.RemoteOwnershipQuarantined {
			if err := remote.TransitionRemoteOwnership(entity.RemoteOwnershipRecovering); err != nil {
				return err
			}
			target := entity.RemoteOwnershipLocalOwned
			if wrapper.isMarked() {
				target = entity.RemoteOwnershipShared
			}
			if err := remote.TransitionRemoteOwnership(target); err != nil {
				return err
			}
		}
	} else {
		live.SetEntityVersion(int64(commit.NextVersion))
	}
	if participant, ok := live.(entity.IRemoteCommitParticipant); ok {
		return participant.AcknowledgeRemoteCommit(commit.Clone())
	}
	return nil
}
