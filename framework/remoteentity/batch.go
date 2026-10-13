package remoteentity

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/infra/base/fctx"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
	"github.com/tjbdwanghaibo/roost-core/infra/storage/redis"
)

// remoteWriteEntry 是写批次中的一个实体：持有它的写门、所有权读锁（共享模式下还有分布式锁）与写租约，定稿后带
// 冻结的 Remote 提交。
type remoteWriteEntry struct {
	wrapper   *remoteEntityWrapper
	entity    entity.IThreadSafeRemoteEntity
	lease     entity.RemoteWriteLease
	distLock  bool
	finalized bool
	commit    entity.RemoteCommit
	// unloaded 表示持久拒绝后旧实例已被仅内存卸载，重试时跳过。
	unloaded bool
}

// remoteWriteBatch 实现 entity.RemoteWriteBatch：Prepare 取得全部写权限 → 持锁 FinalizeLocked 冻结提交 →
// 解锁后 Commit 按持久级别提交或等待结论 → Close 释放资源，结论未定时交给 finalizer。
type remoteWriteBatch struct {
	mgr     *Manager
	entries []*remoteWriteEntry
	ids     []int64

	mu            sync.Mutex
	outcome       entity.RemoteTransactionOutcome
	finalized     bool
	committed     bool
	aborted       bool
	indeterminate bool
	// rejected 表示 Durability 0 被权威明确拒绝，Commit 已同步回滚并隔离；Close 释放后卸载旧实例。
	rejected bool
	closed   bool
	// onOutcome 是 Nest 交来的提交后工作，拿到持久结论后恰好调用一次。
	onOutcome func(bool)
	reserved  bool
}

var _ entity.RemoteWriteBatch = (*remoteWriteBatch)(nil)

// PrepareRemoteWriteBatch 在 Nest 加锁之前按 ID 升序为每个目标取得写权限（写门、所有权、共享模式的分布式锁与
// fence）并加载权威实例。整批共用一个 OpTimeout 上界；任一实体失败即中止并释放已取得的资源。
func (m *Manager) PrepareRemoteWriteBatch(ctx context.Context, ids []int64) (_ entity.RemoteWriteBatch, err error) {
	fctx.AssertBlockingAllowed("remoteentity.PrepareRemoteWriteBatch")
	started := time.Now()
	batchClass := "single"
	if len(ids) > 1 {
		batchClass = "multi"
	}
	defer func() {
		result := "ok"
		if err != nil {
			result = "error"
		}
		labels := metrics.Labels{"result": result, "batch": batchClass}
		metrics.IncCounter("remote_entity.remote.prepare_total", labels, 1)
		metrics.ObserveDuration("remote_entity.remote.prepare_latency", labels, time.Since(started))
	}()
	if m == nil || m.cfg == nil {
		return nil, entity.ErrRemoteWriteCapabilityDisabled
	}
	if fatal := m.FatalError(); fatal != nil {
		return nil, errors.Join(entity.ErrRemoteFenced, fatal)
	}
	m.StartFinalizer()
	ordered, err := entity.ValidateRemoteWriteBatchIDs(ids)
	if err != nil {
		return nil, err
	}
	if len(ordered) == 0 {
		return &remoteWriteBatch{mgr: m}, nil
	}
	if m.cfg.MaxWriteBatch > 0 && len(ordered) > m.cfg.MaxWriteBatch {
		return nil, fmt.Errorf("%w: batch=%d max=%d", entity.ErrRemoteOverloaded, len(ordered), m.cfg.MaxWriteBatch)
	}
	if m.backend == nil {
		return nil, entity.ErrRemoteWriteCapabilityDisabled
	}
	if !m.reserveRemoteWriteSlot() {
		return nil, entity.ErrRemoteOverloaded
	}
	// 整批准入共用一个上界，不能给每个实体重开 OpTimeout。
	ctx, cancel := m.ownershipContext(ctx)
	defer cancel()
	batch := &remoteWriteBatch{mgr: m, ids: ordered, entries: make([]*remoteWriteEntry, 0, len(ordered)), reserved: true}
	for _, id := range ordered {
		meta := entity.ResolveEntityID(id)
		wrapper := m.getOrCreate(meta.FullID, meta.Category, meta.Kind)
		if wrapper == nil {
			_ = batch.Abort(ctx, entity.ErrRemoteRejected)
			_ = batch.Close(ctx)
			return nil, fmt.Errorf("%w: wrapper capacity reached for %d", entity.ErrRemoteOverloaded, id)
		}
		entry, beginErr := wrapper.beginWrite(ctx)
		if beginErr != nil {
			wrapper.release()
			_ = batch.Abort(ctx, beginErr)
			_ = batch.Close(ctx)
			return nil, beginErr
		}
		batch.entries = append(batch.entries, entry)
	}
	return batch, nil
}

// beginWrite 为一个实体取得写权限：写门 → 所有权（必要时认领）→ 共享模式取分布式锁与 fence（或独占模式取持久写许可）
// → 加载与权威版本一致的实例 → 校验所有权状态 → 写入版本向量。任一步失败都释放此前取得的锁与写门。
//
// 资源按“写门 → 所有权读锁 → 分布式锁（仅共享模式）”的顺序取得，失败时经 abortBeginWrite 反序归还。
// 各步骤自己取得的资源在自身失败时自己退还，不把半取得的状态留给调用方。
func (w *remoteEntityWrapper) beginWrite(parent context.Context) (*remoteWriteEntry, error) {
	if w == nil || w.mgr == nil {
		return nil, entity.ErrRemoteRejected
	}
	// OpTimeout 是整个操作的预算，排队等写门也计入其中：截止时间必须在等待之前设定，否则同一实体上的 N 个写者会让
	// 最后一个等完其余全部，配置的预算形同虚设。ownership.go 的加锁路径遵守同一规则。
	ctx, cancel := w.mgr.ownershipContext(parent)
	defer cancel()
	if err := w.acquireWriteGate(ctx); err != nil {
		return nil, err
	}
	w.ownershipMu.RLock()
	marked, err := w.confirmWriteOwnership(ctx)
	if err != nil {
		w.abortBeginWrite(ctx, false)
		return nil, err
	}
	authority, durable := w.mgr.ownershipStore.(WriteAuthority)
	lockFence := uint64(0)
	if marked {
		if lockFence, err = w.lockSharedWrite(ctx, durable); err != nil {
			w.abortBeginWrite(ctx, false)
			return nil, err
		}
	}
	// 共享模式此后持有分布式锁，失败分支要先解锁。
	distLocked := marked

	marker, err := w.writeMarker()
	if err != nil {
		w.abortBeginWrite(ctx, distLocked)
		return nil, err
	}
	expectedVersion := w.rMu.Version()
	if !marked && durable {
		grant, err := authority.GrantWrite(ctx, w.id, generateToken(), w.mgr.localSid)
		if err != nil {
			w.abortBeginWrite(ctx, distLocked)
			return nil, err
		}
		marker = grant.Ownership
		w.applyOwnership(marker)
		lockFence, expectedVersion = grant.Fence, grant.Version
	}
	remoteEntity, err := w.loadWriteTarget(ctx, marked || durable, expectedVersion)
	if err != nil {
		w.abortBeginWrite(ctx, distLocked)
		return nil, err
	}
	if remote, ok := remoteEntity.(entity.IThreadSafeRemoteEntity); ok {
		state, admitted, err := admitWriteOwnershipState(remote, marked)
		if err != nil {
			w.abortBeginWrite(ctx, distLocked)
			return nil, fmt.Errorf("%w: entity=%d state=%s: %v", entity.ErrRemoteOwnerTransition, w.id, state, err)
		}
		if !admitted {
			w.abortBeginWrite(ctx, distLocked)
			return nil, w.blockedOwnershipError(remote, state)
		}
	}
	w.attachEntity(remoteEntity)
	stateVersion := remoteEntity.EntityVersion()
	if stateVersion < 0 {
		stateVersion = 0
	}
	if durable && stateVersion != expectedVersion {
		// 版本冲突按权威版本解锁，不用本地锁记录的版本。
		if distLocked {
			_ = w.unlockObserved(ctx, expectedVersion)
		}
		w.releaseWriteGate()
		return nil, fmt.Errorf("%w: entity %d loaded version %d, authority %d", entity.ErrRemoteVersionConflict, w.id, stateVersion, expectedVersion)
	}
	if current, ok := remoteEntity.(entity.IThreadSafeRemoteEntity); ok && !marked && !durable {
		lockFence = current.RemoteVersionVector().LockFence
	}
	lease := w.newWriteLease(marked, stateVersion, marker, lockFence)
	if remote, ok := remoteEntity.(entity.IThreadSafeRemoteEntity); ok {
		if err := w.applyWriteLease(remote, lease); err != nil {
			w.abortBeginWrite(ctx, distLocked)
			return nil, err
		}
	}
	return &remoteWriteEntry{wrapper: w, entity: remoteEntity, lease: lease, distLock: distLocked}, nil
}

// acquireWriteGate 在 ctx 的截止时间内排队取得本实体的写门。
func (w *remoteEntityWrapper) acquireWriteGate(ctx context.Context) error {
	gateStarted := time.Now()
	select {
	case w.writeGate <- struct{}{}:
		metrics.ObserveDuration("remote_entity.remote.write_gate_wait", nil, time.Since(gateStarted))
		return nil
	case <-ctx.Done():
		metrics.IncCounter("remote_entity_write_gate_timeout_total", nil, 1)
		return ctx.Err()
	}
}

// releaseWriteGate 归还所有权读锁与写门（与取得顺序相反）。
func (w *remoteEntityWrapper) releaseWriteGate() {
	w.ownershipMu.RUnlock()
	<-w.writeGate
}

// abortBeginWrite 是 beginWrite 失败分支的统一收尾：共享模式已持有分布式锁时先按锁记录的版本解锁，
// 再归还所有权读锁与写门。
func (w *remoteEntityWrapper) abortBeginWrite(ctx context.Context, distLocked bool) {
	if distLocked {
		_ = w.unlockObserved(ctx, w.rMu.Version())
	}
	w.releaseWriteGate()
}

// confirmWriteOwnership 刷新 marker，没有所有权记录时为本节点认领，返回实体是否处于共享模式。
// 独占模式下只有本节点是所有者才允许写。
func (w *remoteEntityWrapper) confirmWriteOwnership(ctx context.Context) (marked bool, err error) {
	if err := w.ensureMarker(ctx); err != nil {
		return false, fmt.Errorf("remote_entity: refresh marker %d: %w", w.id, err)
	}
	if _, found := w.cachedOwnership(); !found {
		lease, err := w.mgr.ownershipStore.ClaimOwnership(ctx, w.id, w.mgr.localSid)
		if err != nil {
			// CAS 失败后刷新，让错误报告真正赢得认领的所有者，而不是把“没有记录”当作本地拥有。
			_ = w.refreshMarked(ctx)
			current, _ := w.cachedOwnership()
			return false, errors.Join(ownershipFenceError(w.id, current), err)
		}
		if !validOwnershipLease(lease) || lease.OwnerSid != w.mgr.localSid {
			return false, fmt.Errorf("%w: invalid ownership claim for entity=%d", entity.ErrRemoteFenced, w.id)
		}
		w.applyOwnership(lease)
	}
	marked = w.isMarked()
	if !marked && !w.isLocalOwner() {
		return false, fmt.Errorf("%w: entity=%d owner=%d", entity.ErrRemoteFenced, w.id, w.leaseOwner())
	}
	return marked, nil
}

// lockSharedWrite 为共享模式取得分布式锁与它分配的 fence，并确认实体仍在共享模式（持久权威从写许可确认，
// 否则从 marker 重读）。成功时持有分布式锁；失败时已取得的分布式锁在这里解开，调用方只需归还写门。
func (w *remoteEntityWrapper) lockSharedWrite(ctx context.Context, durable bool) (lockFence uint64, err error) {
	if err := w.rMu.Lock(ctx); err != nil {
		return 0, fmt.Errorf("remote_entity: shared lock %d: %w", w.id, err)
	}
	// 这是公开契约而非鸭子类型：Manager 构造时拒绝锁不具备该能力的工厂，所以每个 wrapper 上这个断言都成立。
	// 下面的零值检查是最后一道防线，防备实现了方法却给出错误值的锁。
	if provider, ok := w.rMu.(redis.IFencedVersionedLock); ok {
		lockFence = provider.Fence()
	}
	if lockFence == 0 {
		_ = w.unlockObserved(ctx, w.rMu.Version())
		return 0, fmt.Errorf("%w: shared lock did not allocate fence", entity.ErrRemoteFenced)
	}
	if durable {
		// 许可已经原子包含 ownership；重复读取会增加往返并可能混入下一代 ownership。
		provider, ok := w.rMu.(interface{ writeGrant() (WriteGrant, bool) })
		var grant WriteGrant
		if ok {
			grant, ok = provider.writeGrant()
		}
		if !ok || !grant.Ownership.Shared || grant.Fence != lockFence {
			_ = w.unlockObserved(ctx, w.rMu.Version())
			return 0, entity.ErrRemoteFenced
		}
		w.applyOwnership(grant.Ownership)
	} else if err := w.refreshMarked(ctx); err != nil || !w.isMarked() {
		_ = w.unlockObserved(ctx, w.rMu.Version())
		return 0, errors.Join(entity.ErrRemoteOwnerTransition, err)
	}
	return lockFence, nil
}

// writeMarker 读取当前缓存的 marker；它必须是一份完整有效的所有权租约。
func (w *remoteEntityWrapper) writeMarker() (entity.RemoteEntityMarkerLease, error) {
	w.markerMu.RLock()
	marker := w.markerLease
	w.markerMu.RUnlock()
	if !validOwnershipLease(marker) {
		return marker, ownershipFenceError(w.id, marker)
	}
	return marker, nil
}

// loadWriteTarget 返回本次写入的实例：本地已有且（需要对齐权威版本时）版本与 expectedVersion 一致就直接用，
// 否则从后端加载。实例不存在返回 ErrRemoteRejected；旧实例正在卸载返回可重试的 ErrRemoteEntityReloading。
func (w *remoteEntityWrapper) loadWriteTarget(ctx context.Context, matchAuthority bool, expectedVersion int64) (entity.IThreadSafeRemoteEntity, error) {
	remoteEntity := w.lookupLocalEntity()
	if remoteEntity == nil || (matchAuthority && (remoteEntity.EntityVersion() != expectedVersion || expectedVersion <= 0)) {
		var loadErr error
		remoteEntity, loadErr = w.loadEntity(ctx)
		if loadErr != nil {
			if errors.Is(loadErr, entity.ErrEntityRemoved) {
				// 旧实例正被卸载（Destroy 已把它移出索引、生命周期回调未结束），EntityManager 拒绝登记重载的实例：
				// 这是卸载重载窗口，可重试。
				return nil, fmt.Errorf("%w: entity %d: %w", entity.ErrRemoteEntityReloading, w.id, loadErr)
			}
			return nil, fmt.Errorf("remote_entity: load entity %d: %w", w.id, loadErr)
		}
	}
	if remoteEntity == nil {
		return nil, fmt.Errorf("%w: entity %d not found", entity.ErrRemoteRejected, w.id)
	}
	return remoteEntity, nil
}

// admitWriteOwnershipState 检查实例的所有权状态能否接受写入。Recovering 就地解冻（err 是解冻失败）；
// Draining / Fenced / Quarantined 不接受（admitted=false），调用方归还资源后由 blockedOwnershipError 生成错误。
func admitWriteOwnershipState(remote entity.IThreadSafeRemoteEntity, marked bool) (state entity.RemoteOwnershipState, admitted bool, err error) {
	state = remote.RemoteOwnershipState()
	switch state {
	case entity.RemoteOwnershipRecovering:
		// 结果未知的所有权转换把实例冻结了。走到这里说明冻结时作废的 marker 已在上面从权威重读，且仍指向本节点，
		// 这份副本重新归本节点所有。
		thaw := entity.RemoteOwnershipLocalOwned
		if marked {
			thaw = entity.RemoteOwnershipShared
		}
		if err := remote.TransitionRemoteOwnership(thaw); err != nil {
			return state, false, err
		}
	case entity.RemoteOwnershipDraining, entity.RemoteOwnershipFenced, entity.RemoteOwnershipQuarantined:
		return state, false, nil
	}
	return state, true, nil
}

// blockedOwnershipError 为不接受写入的所有权状态选错误：持久拒绝后等待重载的旧实例可重试，Fenced / Quarantined
// 已被隔离，其余（Draining）是所有权转换中。在写门归还之后调用。
func (w *remoteEntityWrapper) blockedOwnershipError(remote entity.IThreadSafeRemoteEntity, state entity.RemoteOwnershipState) error {
	cause := entity.ErrRemoteOwnerTransition
	switch {
	case state == entity.RemoteOwnershipQuarantined && w.rejectedReloadPending(remote):
		// 持久拒绝已收尾、gate 已释放，框架即将卸载这份旧实例并从权威重载：可重试。
		cause = entity.ErrRemoteEntityReloading
	case state == entity.RemoteOwnershipFenced || state == entity.RemoteOwnershipQuarantined:
		cause = entity.ErrRemoteFenced
	}
	return fmt.Errorf("%w: entity=%d state=%s", cause, w.id, state)
}

// newWriteLease 生成本次写入的租约：共享模式走分布式锁，独占模式走所有者路由。
func (w *remoteEntityWrapper) newWriteLease(marked bool, stateVersion int64, marker entity.RemoteEntityMarkerLease, lockFence uint64) entity.RemoteWriteLease {
	state := entity.RemoteOwnershipLocalOwned
	mode := entity.RemoteWriteOwnerRouted
	if marked {
		state = entity.RemoteOwnershipShared
		mode = entity.RemoteWriteSharedLock
	}
	return entity.RemoteWriteLease{
		EntityID: w.id, OwnerSID: w.mgr.localSid, Mode: mode, State: state,
		BaseVersion: uint64(stateVersion), MarkerEpoch: marker.MarkerEpoch, LockFence: lockFence,
		RouteEpoch: marker.RouteEpoch, AcquiredAt: time.Now().UnixNano(),
	}
}

// applyWriteLease 把租约写进实例的版本向量，并把所有权状态切到租约的状态。
func (w *remoteEntityWrapper) applyWriteLease(remote entity.IThreadSafeRemoteEntity, lease entity.RemoteWriteLease) error {
	if err := remote.SetRemoteVersionVector(entity.RemoteVersionVector{
		StateVersion: lease.BaseVersion, MarkerEpoch: lease.MarkerEpoch,
		LockFence: lease.LockFence, RouteEpoch: lease.RouteEpoch,
	}); err != nil {
		return fmt.Errorf("remote_entity: update version vector %d: %w", w.id, err)
	}
	if err := remote.TransitionRemoteOwnership(lease.State); err != nil {
		return fmt.Errorf("remote_entity: update ownership state %d: %w", w.id, err)
	}
	return nil
}

// EntityIDs 返回批次目标（升序、去重）的副本。
func (b *remoteWriteBatch) EntityIDs() []int64 {
	if b == nil {
		return nil
	}
	return append([]int64(nil), b.ids...)
}

// FinalizeLocked 在业务锁内为每个有持久变化（或删除意图）的实体生成冻结的 Remote 提交，填入租约的版本与代际，
// 校验后登记事务。任何一个失败都回滚已生成的提交并返回错误；只能定稿一次。
func (b *remoteWriteBatch) FinalizeLocked(outcome entity.RemoteTransactionOutcome) error {
	if b == nil || b.mgr == nil || !outcome.Succeeded || outcome.TransactionID.IsZero() {
		return entity.ErrRemoteCommitNotFinalized
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.finalized || b.committed || b.aborted || b.closed {
		return entity.ErrRemoteCommitNotFinalized
	}
	for _, entry := range b.entries {
		if entry.entity == nil {
			continue
		}
		participant, ok := entry.entity.(entity.IRemoteCommitParticipant)
		if !ok {
			b.rollbackFinalizedLocked()
			return fmt.Errorf("%w: entity %d has no generated remote commit participant", entity.ErrRemoteWriteCapabilityDisabled, entry.lease.EntityID)
		}
		deleteRequested := outcome.DeleteIntents != nil && outcome.DeleteIntents.RemoteDeleteRequested(entry.lease.EntityID)
		if !deleteRequested && !participant.HasRemoteCommitLocked(outcome) {
			continue
		}
		commit, err := participant.BuildRemoteCommitLocked(entry.lease, outcome)
		if err != nil {
			b.rollbackFinalizedLocked()
			return fmt.Errorf("remote_entity: build commit %d: %w", entry.lease.EntityID, err)
		}
		commit.TransactionID = outcome.TransactionID
		commit.EntityID = entry.lease.EntityID
		commit.Kind = entry.entity.GetEntityKind()
		if commit.Delete != deleteRequested {
			participant.RollbackRemoteCommit(commit)
			b.rollbackFinalizedLocked()
			return fmt.Errorf("remote_entity: delete intent mismatch for entity %d", entry.lease.EntityID)
		}
		commit.BaseVersion = entry.lease.BaseVersion
		commit.NextVersion = entry.lease.BaseVersion + 1
		commit.MarkerEpoch = entry.lease.MarkerEpoch
		commit.LockFence = entry.lease.LockFence
		commit.RouteEpoch = entry.lease.RouteEpoch
		for i := range commit.Snapshots {
			commit.Snapshots[i].BaseVersion = commit.BaseVersion
			commit.Snapshots[i].StateVersion = commit.NextVersion
			commit.Snapshots[i].MarkerEpoch = commit.MarkerEpoch
			commit.Snapshots[i].RouteEpoch = commit.RouteEpoch
		}
		if err := commit.Validate(); err != nil {
			participant.RollbackRemoteCommit(commit)
			b.rollbackFinalizedLocked()
			return fmt.Errorf("remote_entity: validate commit %d: %w", entry.lease.EntityID, err)
		}
		if err := refuseServerScopedRemoteData(commit); err != nil {
			participant.RollbackRemoteCommit(commit)
			b.rollbackFinalizedLocked()
			return fmt.Errorf("remote_entity: validate commit %d: %w", entry.lease.EntityID, err)
		}
		entry.commit = commit.Clone()
		entry.finalized = true
	}
	if err := b.mgr.trackRemoteTransaction(outcome.TransactionID); err != nil {
		b.rollbackFinalizedLocked()
		return err
	}
	b.outcome = outcome
	b.finalized = true
	return nil
}

// refuseServerScopedRemoteData 在 WAL 准入之前拒绝按服选库（dbscope=sid）的托管数据。
// 装配期校验只看注册的 DAO 工厂；手写实体注册的工厂与实际持有的 DAO 不一致时它看不到，而提交按
// mutation 自带的 DatabaseScope 选库（mongo_payload.go → MongoCommitter.dataDB），又回到“提交按提交方 sid、加载按
// 本服 sid”的不一致。这里是 Remote 提交进入 WAL 前的唯一定稿点，所以只在这里拒绝：RemoteCommit.Validate 同时被
// WAL 编解码、投影与 ApplyRemoteCommits（投影重放）调用，在那里拒绝会把已准入的记录变成投影毒丸、让重启重放失败；
// DatabaseScope 字段、WAL 编码与 dataDB 的 sid 分支因此原样保留。
func refuseServerScopedRemoteData(commit entity.RemoteCommit) error {
	for i := range commit.Mutations {
		if entity.DatabaseScope(commit.Mutations[i].DatabaseScope) == entity.DatabaseServer {
			return fmt.Errorf("%w: kind=%d mutation collection=%q uses dbscope=sid; declare the DAO with dbscope=global (//roost:dao ... dbscope=global) so every process commits and loads the same database",
				entity.ErrRemoteManagedServerScopedDAO, commit.Kind, commit.Mutations[i].Collection)
		}
	}
	for i := range commit.Deletes {
		if entity.DatabaseScope(commit.Deletes[i].DatabaseScope) == entity.DatabaseServer {
			return fmt.Errorf("%w: kind=%d delete collection=%q uses dbscope=sid; declare the DAO with dbscope=global (//roost:dao ... dbscope=global) so every process commits and loads the same database",
				entity.ErrRemoteManagedServerScopedDAO, commit.Kind, commit.Deletes[i].Collection)
		}
	}
	return nil
}

// rollbackFinalizedLocked 撤销已定稿实体的 Remote 提交（调用方持 b.mu 且仍在业务锁内）。
func (b *remoteWriteBatch) rollbackFinalizedLocked() {
	for _, entry := range b.entries {
		if !entry.finalized || entry.entity == nil {
			continue
		}
		if participant, ok := entry.entity.(entity.IRemoteCommitParticipant); ok {
			participant.RollbackRemoteCommit(entry.commit.Clone())
		}
		entry.commit = entity.RemoteCommit{}
		entry.finalized = false
	}
}

// Commits 按批次实体顺序返回已定稿提交的副本。
func (b *remoteWriteBatch) Commits() []entity.RemoteCommit {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.commitsLocked()
}

// Commit 在实体锁释放后按持久级别收尾：Durability 0 直接写权威并等待发布确认；1 返回推测回执，由 finalizer 等投影；
// 2/3 等投影器的结论。失败时等待结论的批次标为结果未知（交 finalizer），Durability 0 的明确拒绝在快池回滚并隔离。
func (b *remoteWriteBatch) Commit(ctx context.Context) ([]entity.RemoteCommitReceipt, error) {
	fctx.AssertBlockingAllowed("remoteentity.remoteWriteBatch.Commit")
	if b == nil || b.mgr == nil {
		return nil, entity.ErrRemoteCommitNotFinalized
	}
	b.mu.Lock()
	if !b.finalized || b.aborted || b.closed {
		b.mu.Unlock()
		return nil, entity.ErrRemoteCommitNotFinalized
	}
	if b.committed {
		commits := b.commitsLocked()
		durability := b.outcome.Durability
		b.mu.Unlock()
		status, err := b.mgr.RemoteCommitStatus(ctx, b.outcome.TransactionID)
		if err != nil {
			if durability >= 2 {
				err = waitedCommitError(err)
			}
			return nil, err
		}
		if len(status.Receipts) > 0 {
			return status.Receipts, nil
		}
		return speculativeReceipts(commits), nil
	}
	commits := b.commitsLocked()
	outcome := b.outcome
	b.mu.Unlock()

	var receipts []entity.RemoteCommitReceipt
	var err error
	if len(commits) == 0 {
		b.mgr.completeRemoteTransaction(outcome.TransactionID, entity.RemoteCommitStatus{TransactionID: outcome.TransactionID, State: entity.RemoteCommitCommitted})
	} else if outcome.Durability == 0 {
		receipts, err = b.mgr.ApplyRemoteCommits(ctx, outcome.TransactionID, commits)
		if err == nil {
			// memory 不经过 WAL，但成功回复仍必须等唯一发布者完成确认。
			var status entity.RemoteCommitStatus
			status, err = b.mgr.waitRemoteTransaction(ctx, outcome.TransactionID)
			receipts = status.Receipts
			err = waitedCommitError(err)
		}
	} else if outcome.Durability == 1 {
		receipts = speculativeReceipts(commits)
	} else {
		var status entity.RemoteCommitStatus
		status, err = b.mgr.waitRemoteTransaction(ctx, outcome.TransactionID)
		receipts = status.Receipts
		err = waitedCommitError(err)
	}
	if err != nil {
		rejected := false
		// 等待投影器结论的批次（strict = 2，以及带 Remote 批次、随 strict 路径提交的 pipelined = 3）：本地记录已进 WAL，
		// 即使等到的是明确拒绝，也由 finalizer 按持久结论收尾（Rejected：回滚、隔离、释放、仅内存卸载后交付 false）。
		// 必须用 >= 2 覆盖 pipelined：否则它被拒时既不交给 finalizer、也不走 Durability 0 的同步回滚，实例隔离后永不
		// 卸载、提交后工作没有结论、Sync 门永久冻结。
		if outcome.Durability >= 2 || errors.Is(err, entity.ErrRemotePersistenceIndeterminate) {
			b.mu.Lock()
			b.indeterminate = true
			b.mu.Unlock()
		} else if outcome.Durability == 0 {
			err = errors.Join(err, entity.RunLocal(ctx, func() { b.mgr.rollbackRemoteEntries(b.entries) }))
			rejected = true
			b.mu.Lock()
			b.rejected = true
			b.mu.Unlock()
		}
		quarantineErr := b.mgr.quarantineEntries(b.entries, err)
		if rejected && quarantineErr == nil {
			// 明确拒绝：Close 释放后卸载这些实例，其间下一写者得到可重试的重载哨兵。
			b.mgr.markRejectedReloadPending(b.entries)
		}
		return nil, errors.Join(err, quarantineErr)
	}
	b.mu.Lock()
	b.committed = true
	b.mu.Unlock()
	return receipts, nil
}

// waitedCommitError 给等待投影器结论的 Commit（strict，以及随 strict 路径提交的 pipelined）的错误归类。
// 本地记录已经进 WAL，Remote 部分由投影器按 WAL 写权威；这里的错误只说明本批次没有等到结论，不说明 Remote 没写入。
// 只有 entity.ErrRemoteRejected 是明确拒绝：tracker 终态 Rejected（投影器写权威被 fenced / 版本冲突 / 持久拒绝，
// 过期租约跳过），或 finalizer 回源得到 Rejected，原样返回。其余——tracker 被容量淘汰后重新登记报
// ErrRemoteOverloaded、状态查询失败、未定终态——一律加上 entity.ErrRemotePersistenceIndeterminate；
// 已带它的（Indeterminate 终态、等待截止）不重复加。这些情形 Commit 已按结果未知交给 finalizer，错误若不带哨兵，
// nest 会按“不带哨兵 = 明确拒绝”给回复加 ErrRemotePartRejected，调用方照做会把已提交的 Remote 部分再写一次。
func waitedCommitError(err error) error {
	if err == nil || errors.Is(err, entity.ErrRemoteRejected) || errors.Is(err, entity.ErrRemotePersistenceIndeterminate) {
		return err
	}
	return errors.Join(entity.ErrRemotePersistenceIndeterminate, err)
}

// Abort 中止批次：未定稿时只做标记；已定稿时在快池撤销本地的 Remote 提交并把事务登记为 Rejected。
// 已提交、结果未知或已关闭的批次不受影响。
func (b *remoteWriteBatch) Abort(ctx context.Context, cause error) error {
	if b == nil {
		return nil
	}
	// 未定稿的批次没有本地状态要回滚（定稿失败已在 FinalizeLocked 内回滚），也没有登记事务：
	// 就地标记 aborted，不投递快池续行。标记与定稿共用 b.mu，
	// 之后的 FinalizeLocked 会拒绝；写门与写许可仍由 Close 统一释放。
	b.mu.Lock()
	if !b.finalized {
		if !b.committed && !b.indeterminate && !b.closed {
			b.aborted = true
		}
		b.mu.Unlock()
		return nil
	}
	b.mu.Unlock()
	// 已定稿：本地回滚归快阶段所有。已有 Guard 时复用锁，其余路径显式取得本地锁。
	// 进入快阶段后重新检查状态，期间可能已被提交或关闭。
	return entity.RunLocal(ctx, func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		if b.committed || b.indeterminate || b.closed || b.aborted {
			return
		}
		if b.finalized {
			for _, entry := range b.entries {
				if entry == nil || !entry.finalized || entry.entity == nil {
					continue
				}
				if entity.GetEntityGuard().Guarded(entry.entity.GUId()) {
					if p, ok := entry.entity.(entity.IRemoteCommitParticipant); ok {
						p.RollbackRemoteCommit(entry.commit.Clone())
					}
				} else {
					b.mgr.rollbackRemoteEntries([]*remoteWriteEntry{entry})
				}
				entry.finalized = false
				entry.commit = entity.RemoteCommit{}
			}
		}
		b.aborted = true
		if id := b.outcome.TransactionID; !id.IsZero() && b.mgr != nil {
			b.mgr.completeRemoteTransaction(id, entity.RemoteCommitStatus{TransactionID: id, State: entity.RemoteCommitRejected, Cause: errorString(cause)})
		}
	})
}

// Indeterminate 标记本地提交结果未知：Close 时交给 finalizer 取得持久结论，不按 Abort 处理。
func (b *remoteWriteBatch) Indeterminate(_ context.Context, _ error) error {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed || b.aborted || !b.finalized {
		return entity.ErrRemoteCommitNotFinalized
	}
	b.indeterminate = true
	return nil
}

// DeferUntilDurableOutcome 接手本地已提交事务的提交后工作（entity.RemoteOutcomeDeferrer）。只在 Commit 之后、
// Close 之前、且结论仍待定（indeterminate：交给 finalizer）或已明确拒绝（rejected：Close 卸载后通知）时接手。
func (b *remoteWriteBatch) DeferUntilDurableOutcome(fn func(bool)) bool {
	if b == nil || fn == nil {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed || b.onOutcome != nil || (!b.indeterminate && !b.rejected) {
		return false
	}
	b.onOutcome = fn
	return true
}

var _ entity.RemoteOutcomeDeferrer = (*remoteWriteBatch)(nil)

// Close 结束批次：结论未定（或 Durability 1 待投影）时把条目交给 finalizer；否则释放写门、锁与写额度，明确拒绝的
// 批次随后卸载持有被拒绝修改的实例并交付结论。重复调用返回 nil。
func (b *remoteWriteBatch) Close(ctx context.Context) error {
	fctx.AssertBlockingAllowed("remoteentity.remoteWriteBatch.Close")
	if b == nil {
		return nil
	}
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil
	}
	b.closed = true
	entries := append([]*remoteWriteEntry(nil), b.entries...)
	deferred := b.indeterminate || (b.committed && b.outcome.Durability == 1 && len(b.commitsLocked()) > 0)
	txID := b.outcome.TransactionID
	durability := b.outcome.Durability
	rejected := b.rejected
	onOutcome := b.onOutcome
	b.onOutcome = nil
	b.entries = nil
	b.mu.Unlock()
	if deferred {
		if err := b.mgr.deferRemoteClose(deferredRemoteClose{txID: txID, durability: durability, entries: entries, onOutcome: onOutcome}); err == nil {
			return nil
		}
		// finalizer 已停止：拿不到持久结论，提交后工作不执行（Sync 门保持冻结）。
	}
	err := b.mgr.releaseRemoteEntries(ctx, entries)
	if b.reserved {
		b.mgr.releaseRemoteWriteSlot()
		b.reserved = false
	}
	if rejected {
		// 明确拒绝已回滚并隔离；gate 释放后卸载持有被拒绝修改的旧实例（ctx 带消息的快池续行）。
		// 卸载失败交给 finalizer 重试（这一项不再持有任何资源）；finalizer 已停止时实例保持隔离。
		unloadErr := b.mgr.unloadRejectedEntities(ctx, entries)
		switch {
		case unloadErr == nil:
			b.mgr.deliverRemoteOutcome(ctx, txID, onOutcome, false)
		case errors.Is(unloadErr, entity.ErrRemoteUnloadUnsupported):
			// 旧实例仍在内存、仍持有被拒绝的冻结内容：不丢弃 Sync 门（见 finishRejectedRemoteClose）。
		default:
			item := deferredRemoteClose{txID: txID, durability: durability, entries: entries, settled: true, onOutcome: onOutcome}
			if deferErr := b.mgr.deferRemoteClose(item); deferErr != nil {
				err = errors.Join(err, unloadErr)
			}
		}
	}
	return err
}

// commitsLocked 返回已定稿提交的副本，调用方持 b.mu。
func (b *remoteWriteBatch) commitsLocked() []entity.RemoteCommit {
	return finalizedRemoteCommits(b.entries)
}

// finalizedRemoteCommits 按批次实体顺序复制已定稿的提交；事务 digest 依赖这个顺序，
// finalizer 为同一事务写持久拒绝时必须得到与提交时相同的内容。
func finalizedRemoteCommits(entries []*remoteWriteEntry) []entity.RemoteCommit {
	commits := make([]entity.RemoteCommit, 0, len(entries))
	for _, entry := range entries {
		if entry != nil && entry.finalized {
			commits = append(commits, entry.commit.Clone())
		}
	}
	return commits
}

// speculativeReceipts 按提交内容构造推测回执（Durability 1 不等投影结论时使用）。
func speculativeReceipts(commits []entity.RemoteCommit) []entity.RemoteCommitReceipt {
	receipts := make([]entity.RemoteCommitReceipt, len(commits))
	for i, commit := range commits {
		receipts[i] = entity.RemoteCommitReceipt{
			TransactionID: commit.TransactionID, EntityID: commit.EntityID,
			StateVersion: commit.NextVersion, MarkerEpoch: commit.MarkerEpoch,
			LockFence: commit.LockFence, RouteEpoch: commit.RouteEpoch,
		}
	}
	return receipts
}

func errorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
