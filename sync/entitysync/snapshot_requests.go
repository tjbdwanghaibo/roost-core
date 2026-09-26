package entitysync

import (
	"cmp"
	"slices"
	"sync"
)

// snapshotRequests 仅索引未结算的快照意图，不保存实体内容。subject 仍是订阅真相。
// 更新时持有 subject.mu；计划只读索引和不可变 session，绝不反向获取 subject 锁。
type snapshotRequests struct {
	mu       sync.Mutex
	entries  map[*subscription]snapshotCandidate
	groups   [2]map[SessionID]*snapshotRequestGroup
	sessions [2][]SessionID
	sequence int64
}
type snapshotRequestGroup struct {
	entries map[*subscription]snapshotCandidate
	ordered []snapshotCandidate
}

func (r *snapshotRequests) update(subj *subject, sid SessionID, sub *subscription) {
	r.mu.Lock()
	defer r.mu.Unlock()
	old, found := r.entries[sub]
	if found && sub.kind == kindSnapshot && old.class == sub.snapshotClass {
		return
	}
	if found {
		group := r.groups[old.class][old.sessionID]
		delete(group.entries, sub)
		group.ordered = nil
		if len(group.entries) == 0 {
			delete(r.groups[old.class], old.sessionID)
			r.sessions[old.class] = nil
		}
		delete(r.entries, sub)
	}
	if sub.kind != kindSnapshot {
		return
	}
	if r.entries == nil {
		r.entries = make(map[*subscription]snapshotCandidate)
	}
	class := sub.snapshotClass
	if r.groups[class] == nil {
		r.groups[class] = make(map[SessionID]*snapshotRequestGroup)
	}
	group := r.groups[class][sid]
	if group == nil {
		group = &snapshotRequestGroup{entries: make(map[*subscription]snapshotCandidate)}
		r.groups[class][sid] = group
		r.sessions[class] = nil
	}
	r.sequence++
	candidate := snapshotCandidate{subj.id, sid, sub, class, r.sequence}
	r.entries[sub], group.entries[sub] = candidate, candidate
	group.ordered = nil
}
func (r *snapshotRequests) sessionOrder(class snapshotClass) []SessionID {
	if r.sessions[class] == nil {
		for sid := range r.groups[class] {
			r.sessions[class] = append(r.sessions[class], sid)
		}
		slices.Sort(r.sessions[class])
	}
	return r.sessions[class]
}
func (g *snapshotRequestGroup) order() []snapshotCandidate {
	if g.ordered == nil {
		g.ordered = make([]snapshotCandidate, 0, len(g.entries))
		for _, candidate := range g.entries {
			g.ordered = append(g.ordered, candidate)
		}
		slices.SortFunc(g.ordered, func(a, b snapshotCandidate) int { return cmp.Compare(a.sequence, b.sequence) })
	}
	return g.ordered
}

// snapshotIterator 按已有游标访问本轮所需候选；不会复制和展平全部冷恢复队列。
// 整个计划持有 Manager.mu.RLock 与 requests.mu，迭代结束后不保留缓存引用。
type snapshotIterator struct {
	sessions               []SessionID
	start, visited         int
	items                  []snapshotCandidate
	itemStart, itemVisited int
	session                *session
}

func (it *snapshotIterator) next(m *Manager, class snapshotClass, pending map[int64]struct{}) (snapshotCandidate, bool) {
	for {
		for it.itemVisited < len(it.items) {
			item := it.items[(it.itemStart+it.itemVisited)%len(it.items)]
			it.itemVisited++
			if _, ok := pending[item.subjectID]; !ok {
				continue
			}
			if item.sub.lifetime != it.session.lifetime {
				continue
			} // lifetime 发布后不变
			if _, held := it.session.objects[item.subjectID]; held {
				continue
			}
			return item, true
		}
		if it.visited == len(it.sessions) {
			return snapshotCandidate{}, false
		}
		sid := it.sessions[(it.start+it.visited)%len(it.sessions)]
		it.visited++
		it.session = m.sessions[sid]
		if it.session == nil || it.session.held {
			continue
		}
		it.items = m.snapshotRequests.groups[class][sid].order()
		it.itemStart, _ = slices.BinarySearchFunc(it.items, it.session.snapshotAfter[class], func(a snapshotCandidate, sequence int64) int { return cmp.Compare(a.sequence, sequence) })
		for it.itemStart < len(it.items) && it.items[it.itemStart].sequence <= it.session.snapshotAfter[class] {
			it.itemStart++
		}
		it.itemVisited = 0
	}
}

// changeSubscriptionKindLocked 集中维护快照索引；调用方持有 subject.mu。
func (m *Manager) changeSubscriptionKindLocked(subj *subject, sid SessionID, sub *subscription, kind subscriptionKind) {
	if sub.kind != kind {
		subj.profilesValid = false
	}
	sub.kind = kind
	m.snapshotRequests.update(subj, sid, sub)
}
