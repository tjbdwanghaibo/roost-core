package entity

import (
	"errors"
	"fmt"
	"sync/atomic"
)

var ErrSyncCommitPending = errors.New("entity: sync commit is not released and confirmed")

// SyncCommitObserver 把业务提交边界交给同步调度器；内容层不认识会话或传输。
// CaptureSync 在 Entity 锁内调用，只能冻结内容。WakeSync 不得阻塞业务线程。
type SyncCommitObserver interface {
	BindSyncProducer()
	CaptureSync(*SubjectSyncState) error
	WakeSync()
}

// SyncSubjectRetractor 是 SyncCommitObserver 的可选能力：撤回事务内新建、随后被回滚或拒绝的
// 实体（RR-20260926-35）。实现方注销该 subject：尚未持有对象的订阅直接移除，已持有对象的会话
// 在下一帧收到 ObjectRemove（remove-before-create）。只登记在别的调度器上的 subject 不受影响。
// 在持有该实体锁的业务 goroutine 上调用，不得阻塞或等待网络。
type SyncSubjectRetractor interface {
	RetractSyncSubject(*SubjectSyncState)
}

// SyncChangeCollector 由生成器或手写实体实现。掩码属于实体内容 schema，
// 不得直接合并不同 DAO 的局部字段位；无法映射时返回 SyncMaskFull。
type SyncChangeCollector interface{ TakeEntitySyncChanges() uint64 }

type syncMutationEntry struct {
	state     *SubjectSyncState
	base      *EntityBase
	collector SyncChangeCollector
	parent    *syncMutationEntry
	previous  *syncCommitGate
	gate      *syncCommitGate
	mask      uint64
	full      bool
	reason    uint32
}

// syncCommitGate 是一个实体在一笔提交里的门：同一 SyncMutation 的每个实体各有一道，共享 batch 的
// released / confirmed。rejected 按实体记录（RR-20260926-58）：混合事务只有 Remote 部分被持久拒绝时，
// 只有这些实体的门按拒绝处理，已持久提交的本地实体仍按 Confirm 放行。
type syncCommitGate struct {
	batch    *SyncMutation
	previous *syncCommitGate
	rejected atomic.Bool
}

// passed 表示这道门不再阻塞：已释放且已确认，或持久结论为拒绝（不再阻塞后续提交，Reject）。
func (g *syncCommitGate) passed() bool {
	return g.rejected.Load() || (g.batch.released.Load() && g.batch.confirmed.Load())
}

func (g *syncCommitGate) ready() bool {
	for ; g != nil; g = g.previous {
		if !g.passed() {
			return false
		}
	}
	return true
}

// SyncMutation 由持有全部目标 Entity 锁的执行器创建。Admit 必须在解锁前，
// Release 必须在全部锁释放后，Confirm 必须在原有提交协议成功后调用。
// 三个边界分开，避免 WAL 完成池抢先运行或远端确认尚未完成时泄漏状态。
type SyncMutation struct {
	guard            *EntityGuard
	previousMutation *SyncMutation
	observer         SyncCommitObserver
	entries          []*syncMutationEntry
	admitted         bool // 只由持锁的业务 goroutine 访问
	released         atomic.Bool
	confirmed        atomic.Bool
	discarded        atomic.Bool
	// rejected 表示已准入的提交整体被持久拒绝（Remote 结论，RR-20260926-37）：全部实体的门不再阻塞后续提交，
	// 本提交自己的兴趣事实按丢弃处理；从不 Confirm。只拒绝其中一部分实体见 RejectEntities（各门自己的 rejected）。
	rejected atomic.Bool
	// rejectedIDs 是 RejectEntities 拒绝的实体，发布于 confirmed 之前；只供没有 SubjectSyncState 的实体
	// 判断自己的事实（SyncConditionFor 的回退路径）。
	rejectedIDs atomic.Pointer[map[int64]struct{}]
}

func BeginSyncMutation(es []IThreadSafeEntity, observer SyncCommitObserver) *SyncMutation {
	if observer == nil {
		return nil
	}
	b := &SyncMutation{observer: observer}
	if scope := CurrentGuardScope(); scope != nil && scope.Guard() != nil {
		b.guard = scope.Guard()
		b.previousMutation = b.guard.syncMutation
		b.guard.syncMutation = b
	}
	b.Include(es)

	return b
}

// Include 将持锁后动态取得的 Cast 实体、事务内 CreateInScope 新建的实体加入同一提交边界。
func (b *SyncMutation) Include(es []IThreadSafeEntity) {
	if b == nil || b.admitted {
		return
	}
	seen := make(map[*SubjectSyncState]bool, len(es)+len(b.entries))
	for _, entry := range b.entries {
		seen[entry.state] = true
	}
	for _, e := range es {
		if e == nil || e.Base() == nil {
			continue
		}
		s := e.Base().Sync()
		if s == nil || seen[s] {
			continue
		}
		seen[s] = true
		collector, _ := e.(SyncChangeCollector)
		s.mu.Lock()
		previous := s.commitGate
		// 已放行的头结点无需保留，避免前序 WAL 等待时后续内存提交积成历史链。
		for previous != nil && previous.passed() {
			previous = previous.previous
		}
		gate := &syncCommitGate{batch: b, previous: previous}
		entry := &syncMutationEntry{state: s, base: e.Base(), collector: collector, parent: s.mutation, previous: previous, gate: gate}
		s.mutation = entry
		s.commitGate = gate
		s.mu.Unlock()
		b.entries = append(b.entries, entry)
	}
}

func (b *SyncMutation) Admit() {
	if b == nil || b.admitted {
		return
	}
	b.admitted = true
	for _, entry := range b.entries {
		s := entry.state
		if entry.collector != nil {
			mask := entry.collector.TakeEntitySyncChanges()
			entry.mask |= mask
			entry.full = entry.full || mask == SyncMaskFull
		}
		s.mu.Lock()
		s.mutation = entry.parent
		if entry.parent != nil {
			entry.parent.mask |= entry.mask
			entry.parent.full = entry.parent.full || entry.full
			if entry.reason != 0 {
				entry.parent.reason = entry.reason
			}
			s.commitGate = entry.previous
			s.mu.Unlock()
			continue
		}
		changed := entry.mask != 0 || entry.full
		if changed {
			s.dirtyMask |= entry.mask
			s.fullDirty = s.fullDirty || entry.full
			if entry.full {
				s.fullReason = entry.reason
				if s.fullReason == 0 {
					s.fullReason = SyncFullReasonDirty
				}
			}
			s.dirtyGeneration++
		}
		notifier := s.dirtyNotifier
		pending := s.dirtyMask != 0 || s.fullDirty
		s.mu.Unlock()
		if pending {
			if notifier != nil {
				notifySubjectSyncDirty(notifier, s)
			}
			// 冻结失败不能回滚已准入的业务；保留 dirty 供同一流水线重试。
			if err := captureCommittedSync(b.observer, s); err != nil {
				s.setSubjectSyncError(err)
			}
		}
	}
}
func captureCommittedSync(observer SyncCommitObserver, s *SubjectSyncState) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("entity: sync capture panic: %v", p)
		}
	}()
	return observer.CaptureSync(s)
}
func (b *SyncMutation) Release() {
	if b != nil {
		if !b.released.Swap(true) && b.confirmed.Load() {
			b.observer.WakeSync()
		}
	}
}
func (b *SyncMutation) Confirm() {
	if b != nil {
		if !b.confirmed.Swap(true) && b.released.Load() {
			b.observer.WakeSync()
		}
	}
}

// Reject 用于已准入、但远端持久结论为拒绝、且本提交没有其他已持久提交部分的提交（RR-20260926-37）：
// 丢弃全部实体的门，不 Confirm，不再阻塞同一实体的后续提交，本提交的兴趣事实全部丢弃。被拒绝实例在锁内
// 冻结的内容由它的仅内存卸载关闭同步状态时丢弃，调用方须在卸载之后调用。本地部分已持久提交的混合事务
// 用 RejectEntities，只拒绝 Remote 实体。
func (b *SyncMutation) Reject() {
	if b == nil {
		return
	}
	for _, entry := range b.entries {
		entry.gate.rejected.Store(true)
	}
	if !b.rejected.Swap(true) && b.released.Load() {
		b.observer.WakeSync()
	}
}

// RejectEntities 用于本地部分已持久提交、Remote 部分被持久拒绝的提交（RR-20260926-58）：ids 是被拒绝的
// Remote 实体（Remote 批次的实体 ID）。只有它们的门按 Reject 处理——不再阻塞后续提交、本提交里关于它们的
// 兴趣事实丢弃；其余实体的提交已经持久，门按 Confirm 放行，事实与冻结内容照常交付。ids 覆盖全部实体时
// 等同 Reject。与 Reject 一样须在被拒绝实例卸载之后调用。
func (b *SyncMutation) RejectEntities(ids []int64) {
	if b == nil {
		return
	}
	rejected := make(map[int64]struct{}, len(ids))
	for _, id := range ids {
		rejected[id] = struct{}{}
	}
	remaining := false
	for _, entry := range b.entries {
		if _, ok := rejected[entry.base.ID()]; ok {
			entry.gate.rejected.Store(true)
		} else {
			remaining = true
		}
	}
	if !remaining {
		b.Reject()
		return
	}
	// 先发布被拒绝的实体，再 Confirm：任何看到 confirmed 的读者也看到了拒绝标记。
	b.rejectedIDs.Store(&rejected)
	b.Confirm()
}

// entityRejected 报告 id 是否在 RejectEntities 中被拒绝，或整笔提交被 Reject。
func (b *SyncMutation) entityRejected(id int64) bool {
	if b.rejected.Load() {
		return true
	}
	if ids := b.rejectedIDs.Load(); ids != nil {
		_, ok := (*ids)[id]
		return ok
	}
	return false
}

// Finish 清理未准入的作用域。结果不确定时保留屏障，等待进程 fencing/recovery；
// 已准入的 pipelined 事务由完成池 Confirm，不能在当前函数返回时提前放行。
func (b *SyncMutation) Finish(indeterminate bool) {
	if b != nil && b.guard != nil && b.guard.syncMutation == b {
		b.guard.syncMutation = b.previousMutation
	}
	if b == nil || b.admitted {
		return
	}
	if !indeterminate {
		b.discarded.Store(true)
	}
	for _, entry := range b.entries {
		s := entry.state
		s.mu.Lock()
		if s.mutation == entry {
			s.mutation = entry.parent
			if !indeterminate {
				s.commitGate = entry.previous
			}
		}
		s.mu.Unlock()
	}
}

// SyncChangeMapper 为多 DAO 实体显式映射局部字段位到实体内容 schema。
type SyncChangeMapper interface {
	MapEntitySyncChanges(collection string, mask uint64) uint64
}

func MapDAOSyncChanges(e any, collection string, mask uint64, singleDAO bool) uint64 {
	if mask == 0 {
		return 0
	}
	if mapper, ok := e.(SyncChangeMapper); ok {
		return mapper.MapEntitySyncChanges(collection, mask)
	}
	if singleDAO {
		return mask
	}
	return SyncMaskFull
}

// SyncCommitCondition 固定当前操作的提交条件，供锁外的兴趣事实队列使用。
// 第二个返回值表示已回滚，应丢弃事实；事务外调用立即可用。
func (s *SubjectSyncState) SyncCommitCondition() func() (ready, discarded bool) {
	if s == nil {
		return func() (bool, bool) { return true, false }
	}
	s.mu.Lock()
	gate := s.commitGate
	s.mu.Unlock()
	return func() (bool, bool) {
		for current := gate; current != nil; current = current.previous {
			// 只有本提交自己被拒绝的门才丢弃事实；前序被拒绝的门只是不再阻塞（ready 跳过）。
			if current.batch.discarded.Load() || (current == gate && current.rejected.Load()) {
				return false, true
			}
		}
		return gate.ready(), false
	}
}

func (s *SubjectSyncState) SyncCommitReady() bool {
	if s == nil {
		return true
	}
	s.mu.Lock()
	ready := s.commitGate.ready()
	s.mu.Unlock()
	return ready
}

// CurrentSyncMutation 只在当前持锁的业务作用域使用，不传给异步工作线程。
func CurrentSyncMutation() *SyncMutation {
	if scope := CurrentGuardScope(); scope != nil && scope.Guard() != nil {
		return scope.Guard().syncMutation
	}
	return nil
}
func SyncConditionFor(e IThreadSafeEntity) func() (bool, bool) {
	if e != nil && e.Base() != nil && e.Base().Sync() != nil {
		return e.Base().Sync().SyncCommitCondition()
	}
	if batch := CurrentSyncMutation(); batch != nil {
		// 没有 SubjectSyncState 的实体只能看整笔提交；RejectEntities 按实体 ID 判断它自己是否被拒绝。
		var id int64
		known := e != nil && e.Base() != nil
		if known {
			id = e.Base().ID()
		}
		return func() (bool, bool) {
			rejected := batch.rejected.Load()
			if known {
				rejected = batch.entityRejected(id)
			}
			return !rejected && batch.released.Load() && batch.confirmed.Load(), rejected || batch.discarded.Load()
		}
	}
	return func() (bool, bool) { return true, false }
}

// SetLastCommitLSN 包括通过 Cast 动态加入的实体，必须仍持有所有实体锁。
func (b *SyncMutation) SetLastCommitLSN(lsn uint64) {
	if b != nil {
		for _, entry := range b.entries {
			entry.base.SetLastCommitLSN(lsn)
		}
	}
}
