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

// SyncChangeCollector 由生成器或手写实体实现。掩码属于实体内容 schema，
// 不得直接合并不同 DAO 的局部字段位；无法映射时返回 SyncMaskFull。
type SyncChangeCollector interface{ TakeEntitySyncChanges() uint64 }

type syncMutationEntry struct {
	state     *SubjectSyncState
	base      *EntityBase
	collector SyncChangeCollector
	parent    *syncMutationEntry
	previous  *syncCommitGate
	mask      uint64
	full      bool
	reason    uint32
}

type syncCommitGate struct {
	batch    *SyncMutation
	previous *syncCommitGate
}

func (g *syncCommitGate) ready() bool {
	for ; g != nil; g = g.previous {
		if !g.batch.released.Load() || !g.batch.confirmed.Load() {
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

// Include 将持锁后动态取得的 Cast 实体加入同一提交边界。
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
		for previous != nil && previous.batch.released.Load() && previous.batch.confirmed.Load() {
			previous = previous.previous
		}
		entry := &syncMutationEntry{state: s, base: e.Base(), collector: collector, parent: s.mutation, previous: previous}
		s.mutation = entry
		s.commitGate = &syncCommitGate{batch: b, previous: previous}
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
			if current.batch.discarded.Load() {
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
		return func() (bool, bool) { return batch.released.Load() && batch.confirmed.Load(), batch.discarded.Load() }
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
