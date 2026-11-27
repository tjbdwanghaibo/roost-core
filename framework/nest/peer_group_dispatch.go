package nest

import (
	"errors"
	"fmt"
	"slices"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/internal/entitypermit"
)

// 独立伴生域沿用自己的代表锁、写门及事务协议。只有本轮锁组快照全部为 0 时才能走它。
var errDispatchCompanionInstead = errors.New("nest: select companion dispatch")

// lockOrdinaryDispatch 取得实际参与事务的实体集合及唯一组的借用视图。业务参数不扩张，nil participants
// 表示无组，可以沿用原来的业务参数作为事务参与者。显式目标已由调用者 Touch。
func (mgr *NestMgr) lockOrdinaryDispatch(guard *entity.EntityGuard, explicit []entity.IThreadSafeEntity) (participants []entity.IThreadSafeEntity, view *EntityLockGroupScope, release func(), err error) {
	var storage [4]dispatchGroupSnapshot
	for attempt := 0; attempt < entityLockGroupDispatchRetryMax; attempt++ {
		snaps := captureDispatchGroupSnapshots(explicit, storage[:0])
		var ids []int64
		for _, s := range snaps {
			if s.state != entity.EntityGroupTransitionNone {
				return nil, nil, nil, ErrEntityGroupTransitionPending
			}
			if s.groupID != 0 {
				ids = append(ids, s.groupID)
			}
		}
		if len(ids) == 0 {
			if hasCompanionTarget(explicit) {
				return nil, nil, nil, errDispatchCompanionInstead
			}
			acquired, err := lockDispatchEntities(guard, explicit)
			if err != nil {
				return nil, nil, nil, err
			}
			if err := validateDispatchGroupSnapshots(snaps); err != nil {
				releaseDispatchEntities(guard, acquired)
				if errors.Is(err, ErrEntityLockGroupChanged) {
					continue
				}
				return nil, nil, nil, err
			}
			return nil, nil, func() { releaseDispatchEntities(guard, acquired) }, nil
		}
		slices.Sort(ids)
		participants, view, release, err = mgr.lockPeerGroups(guard, slices.Compact(ids), snaps)
		if !errors.Is(err, ErrEntityLockGroupChanged) {
			return
		}
	}
	return nil, nil, nil, ErrEntityLockGroupChanged
}

// 一次尝试：去重的组锁按同迁移相同的顺序 Try → 枚举成员并持稳引用 → 全部实体按全局锁序 Try →
// 校验组代际和伴生代际。包括未分组显式目标在内，组锁内绝不等另一实体/组的锁。
func (mgr *NestMgr) lockPeerGroups(guard *entity.EntityGuard, groupIDs []int64, explicit []dispatchGroupSnapshot) ([]entity.IThreadSafeEntity, *EntityLockGroupScope, func(), error) {
	if msg := currentNestDispatchMsg(); msg != nil && msg.RemoteWriteBatch != nil {
		return nil, nil, nil, fmt.Errorf("%w: Remote write batch", ErrPeerGroupUnsupported)
	}
	store := groupStoreOf(mgr.getter)
	if store == nil {
		return nil, nil, nil, fmt.Errorf("%w: getter has no group index", ErrPeerGroupUnsupported)
	}
	unlockGroups, ok := mgr.tryLockEntityLockGroups(groupIDs)
	if !ok {
		return nil, nil, nil, ErrLockTimeout
	}
	var touched, acquired, all []entity.IThreadSafeEntity
	releaseRefs := func() {
		for _, e := range touched {
			defer e.UnTouch()
		}
	}
	release := func() {
		defer releaseRefs()
		defer unlockGroups()
		releaseDispatchEntities(guard, acquired)
	}
	handed := false
	defer func() {
		if !handed {
			release()
		}
	}()
	if err := validateDispatchGroupSnapshots(explicit); err != nil {
		return nil, nil, nil, err
	}
	// 同一 ID 的不同实例不能互相顶替。事务快照和 Guard 均以逻辑 ID 登记。
	members := make(map[int64]entity.IThreadSafeEntity, len(explicit))
	snaps := make([]dispatchGroupSnapshot, 0, len(explicit))
	for _, s := range explicit {
		id := s.ent.GUId()
		if old := members[id]; old != nil {
			if old != s.ent {
				return nil, nil, nil, ErrEntityLockGroupChanged
			}
			continue
		}
		members[id] = s.ent
		all = append(all, s.ent)
		snaps = append(snaps, s)
	}
	for _, groupID := range groupIDs {
		for _, e := range store.GetGroupEntities(groupID) {
			// 索引给的是未持有的候选；Touch 前绝不读其 ID、Base 或关系。
			if e == nil || !e.Touch() {
				return nil, nil, nil, ErrEntityLockGroupChanged
			}
			touched = append(touched, e)
			id := e.GUId()
			if id == 0 || e.Base() == nil || e.Base().GroupLockID() != groupID || store.GetGroupEntity(groupID, id) != e {
				return nil, nil, nil, ErrEntityLockGroupChanged
			}
			if old := members[id]; old != nil {
				if old != e {
					return nil, nil, nil, ErrEntityLockGroupChanged
				}
				continue
			}
			members[id] = e
			all = append(all, e)
			snaps = capturePeerSnapshot(snaps, e)
			if snaps[len(snaps)-1].groupID != groupID {
				return nil, nil, nil, ErrEntityLockGroupChanged
			}
		}
	}
	var domains []entity.CompanionDomainSnapshot
	for _, e := range all {
		if companionRemote(e) {
			return nil, nil, nil, fmt.Errorf("%w: Remote member %d", ErrPeerGroupUnsupported, e.GUId())
		}
		if entity.IsCompanionParticipant(e.GetEntityKind()) {
			s := entity.SnapshotCompanionDomain(e)
			if s.Root != e || len(s.Members) != 0 {
				return nil, nil, nil, fmt.Errorf("%w: bound companion domain at %d", ErrPeerGroupUnsupported, e.GUId())
			}
			domains = append(domains, s)
		}
	}
	SortEntity(all)
	// acquired 在每次成功后立即登记；即使后续实体的自定义锁 panic，前面的获取也由同一清理函数逆序撤销。
	for _, e := range all {
		if guard.GuardedEntity(e) {
			continue
		}
		if !guard.TryRequireEntity(entitypermit.System(), e) {
			return nil, nil, nil, ErrLockTimeout
		}
		acquired = append(acquired, e)
	}
	if err := validateDispatchGroupSnapshots(snaps); err != nil {
		return nil, nil, nil, err
	}
	for _, s := range domains {
		if !s.Valid() {
			return nil, nil, nil, ErrEntityLockGroupChanged
		}
		if err := checkPeerGroupKind(s.Root); err != nil {
			return nil, nil, nil, err
		}
	}
	var view *EntityLockGroupScope
	if len(groupIDs) == 1 {
		view = &EntityLockGroupScope{groupID: groupIDs[0], members: make(map[int64]dispatchGroupSnapshot)}
		for _, s := range explicit {
			if s.groupID == view.groupID {
				view.anchors = append(view.anchors, s)
			}
		}
		if len(domains) != 0 {
			view.domains = make(map[int64]entity.CompanionDomainSnapshot, len(domains))
			for _, s := range domains {
				view.domains[s.Root.GUId()] = s
			}
		}
		for _, s := range snaps {
			if s.groupID == view.groupID {
				id := s.ent.GUId()
				view.members[id] = s
				view.ids = append(view.ids, id)
			}
		}
		slices.Sort(view.ids)
	}
	handed = true
	return all, view, release, nil
}

func capturePeerSnapshot(snaps []dispatchGroupSnapshot, e entity.IThreadSafeEntity) []dispatchGroupSnapshot {
	b := e.Base()
	return append(snaps, dispatchGroupSnapshot{ent: e, groupID: b.GroupLockID(), epoch: b.GroupEpoch(), state: b.GroupTransitionState(), targetID: b.GroupTransitionTargetID()})
}
