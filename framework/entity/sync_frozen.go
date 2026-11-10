package entity

import "errors"

var ErrSyncFrozenCapacity = errors.New("entity: frozen sync capacity exceeded")

type frozenSubjectSync struct {
	item    *PreparedSubjectSync
	release func()
}

// FreezeSyncViews 在调用者已经持有 Entity 锁时冻结内容，不占用交付 token。
// 同一实体只保留一个待处理内容槽：新冻结整份替换旧的，冻下的就是此刻一次普通捕获会得到的内容（脏位未清前一直累积，
// 增量从当前版本起、带全部未确认的变化），不拼接两份 delta。
// 已有冻结或交付在途时也不升级为全量（2026-11-10 起）：在途捕获提交后版本前进，这份增量的基线随之过时，
// 由 frozenSubjectSync.prepare 的基线校验拒绝复用、在 Entity 锁内重新捕获（只带确认之后的变化，列表操作不重放）；
// 在途捕获被作废时版本不变，这份增量原样可用。全量只在状态本身要求时出现（fullDirty、新订阅的快照）。
// reserve 只核算保留字节，不可阻塞或访问订阅锁。
func (s *SubjectSyncState) FreezeSyncViews(delta, snapshot []SyncProfile, reserve func(int) (func(), bool)) error {
	if s == nil {
		return ErrSubjectSyncClosed
	}
	s.prepareMu.Lock()
	defer s.prepareMu.Unlock()
	s.mu.Lock()
	if !s.enabled {
		s.mu.Unlock()
		return ErrSubjectSyncClosed
	}
	if s.frozen != nil {
		s.frozen.release()
		s.frozen = nil
	}
	clone := &SubjectSyncState{
		enabled: s.enabled, subjectID: s.subjectID, namespace: s.namespace, subjectKind: s.subjectKind,
		version: s.version, dirtyMask: s.dirtyMask, fullDirty: s.fullDirty, fullReason: s.fullReason,
		dirtyGeneration: s.dirtyGeneration, packer: s.packer,
	}
	clone.lastCommitLSN.Store(s.lastCommitLSN.Load())
	s.mu.Unlock()
	item, err := clone.prepareLocked(NormalizeSyncProfiles(delta), NormalizeSyncProfiles(snapshot))
	if err != nil {
		return err
	}
	size := 0
	for _, u := range item.updates {
		size += u.Payload.Len()
	}
	for _, u := range item.snapshots {
		size += u.Payload.Len()
	}
	release, ok := reserve(size)
	if !ok {
		return ErrSyncFrozenCapacity
	}
	s.mu.Lock()
	s.frozen = &frozenSubjectSync{item: item, release: release}
	s.mu.Unlock()
	return nil
}

func (f *frozenSubjectSync) prepare(s *SubjectSyncState, token, generation, base, version uint64, delta, snapshot []SyncProfile) *PreparedSubjectSync {
	p := f.item
	if p.generation != generation || p.commitLSN != s.lastCommitLSN.Load() {
		return nil
	}
	updates := make([]SubjectSyncUpdate, 0, len(delta))
	snapshots := make([]SubjectSyncUpdate, 0, len(snapshot))
	for _, profile := range delta {
		found := false
		for _, u := range p.updates {
			if u.Profile != profile {
				continue
			}
			if !u.Full && p.baseVersion != base {
				return nil
			}
			u.BaseVersion, u.Version = base, version
			updates = append(updates, u)
			found = true
			break
		}
		if !found {
			return nil
		}
	}
	for _, profile := range snapshot {
		found := false
		for _, u := range p.snapshots {
			if u.Profile != profile {
				continue
			}
			u.BaseVersion, u.Version = version, version
			snapshots = append(snapshots, u)
			found = true
			break
		}
		if !found {
			return nil
		}
	}
	return &PreparedSubjectSync{state: s, token: token, generation: generation, baseVersion: base,
		version: version, commitLSN: p.commitLSN, updates: updates, snapshots: snapshots, changeVersion: p.changeVersion, changePacker: p.changePacker}
}

// DiscardFrozenSync 释放调度器停止管理的保留内容；已有交付 token 不受影响。
func (s *SubjectSyncState) DiscardFrozenSync() {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.frozen != nil {
		s.frozen.release()
		s.frozen = nil
	}
	s.mu.Unlock()
}
