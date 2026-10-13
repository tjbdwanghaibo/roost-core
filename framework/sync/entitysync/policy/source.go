// Package policy holds the ways subscriptions get organized on top of an
// entitysync.Manager: by distance (Interest), by membership in a group
// (Group), or one binding at a time (Direct). None of them touch the wire or
// the frames — a policy only decides WHO receives WHOM and says so to the
// manager. The manager, in turn, knows nothing about any of them.
package policy

import (
	"cmp"
	"slices"
	"sync"
)

// Source is one reason an observer should receive a subject's state. Space is
// one reason; being in the same team, being on a friends list, being yourself
// are others. They all answer the same question, so they all produce the same
// events and Interest's aggregation does not care which is which.
//
// InterestEvent 定义在本包。空间来源可产生距离分档变化；关系来源始终使用
// band 0，只产生进入和离开事件。Interest 负责合并来源，Manager 负责交付。
type Source interface {
	// Name identifies the source in the aggregator's bookkeeping. Two sources
	// must not share a name.
	Name() string
	// Flush drains the changes since the last call.
	Flush() []InterestEvent
}

// RelationSource 将队伍、好友等业务关系转为订阅进入/离开事件。
// members 按观察者保存成员，observers 按实体反查观察者；两者在同一锁下更新。
// 关系没有距离分档，进入事件始终使用 band 0。
type RelationSource struct {
	name string

	mu      sync.Mutex
	members map[int64]map[int64]struct{} // observer -> subjects
	// 正反索引在同一 mu 下更新；退出只访问持有该 subject 的观察者。
	observers map[int64]map[int64]struct{} // subject -> observers
	pending   []InterestEvent
	// 仅供小集合构表，受 mu 保护；一旦成为 members 的正式集合便移交所有权。
	scratch map[int64]struct{}
}

const relationScratchLimit = 1024

func NewRelationSource(name string) *RelationSource {
	return &RelationSource{
		name:      name,
		members:   make(map[int64]map[int64]struct{}),
		observers: make(map[int64]map[int64]struct{}),
	}
}

func (s *RelationSource) Name() string { return s.name }

// Set replaces an observer's set. The diff is computed here rather than by the
// caller: a caller that had to compute it would need to remember the previous
// set, and then there would be two copies of the same fact.
func (s *RelationSource) Set(observer int64, subjects []int64) {
	if observer == 0 {
		return
	}
	var next map[int64]struct{}
	useScratch := len(subjects) <= relationScratchLimit
	if useScratch {
		s.mu.Lock()
		next = relationMembers(s.scratch, subjects)
	} else {
		// 大集合沿用锁外构表，避免扩大其他观察者、Forget 和 Flush 的等待。
		next = relationMembers(nil, subjects)
		s.mu.Lock()
	}
	defer s.mu.Unlock()
	previous := s.members[observer]
	changed := false
	for subject := range previous {
		if _, kept := next[subject]; !kept {
			changed = true
			s.recordLeaveLocked(observer, subject)
		}
	}
	for subject := range next {
		if _, had := previous[subject]; !had {
			changed = true
			s.recordEnterLocked(observer, subject)
		}
	}
	// 没有变化时保留原成员表；有变化时移交 next，不能再清空或复用它。
	if len(next) == 0 {
		delete(s.members, observer)
	} else if changed {
		s.members[observer] = next
		if useScratch {
			s.scratch = nil
		}
		return // next 已被正式成员持有，不能清空或再次作为工作区写入。
	}
	if useScratch {
		clear(next)
		s.scratch = next
	}
}

// relationMembers 复制并去重；零 ID 忽略，自关系和负数 ID 保留。
func relationMembers(next map[int64]struct{}, subjects []int64) map[int64]struct{} {
	if next == nil {
		next = make(map[int64]struct{}, len(subjects))
	}
	for _, subject := range subjects {
		if subject != 0 {
			next[subject] = struct{}{}
		}
	}
	return next
}

// Clear drops an observer and leaves everything it held.
func (s *RelationSource) Clear(observer int64) { s.Set(observer, nil) }

// ApplyDelta 原子应用 (旧成员 ∪ added) − removed，只遍历变化成员。
// 重复 ID 幂等，同次移除优先；不产生先进入再离开的中间事件。
// 此方法直接改政策状态；事务内业务应使用 Interest.QueueRelationDelta。
func (s *RelationSource) ApplyDelta(observer int64, added, removed []int64) {
	if observer == 0 || len(added)+len(removed) == 0 {
		return
	}
	excluded := relationMembers(nil, removed)
	s.mu.Lock()
	defer s.mu.Unlock()
	members := s.members[observer]
	for subject := range excluded {
		if _, exists := members[subject]; !exists {
			continue
		}
		delete(members, subject)
		s.recordLeaveLocked(observer, subject)
	}
	for _, subject := range added {
		if subject == 0 {
			continue
		}
		if _, remove := excluded[subject]; remove {
			continue
		}
		if _, exists := members[subject]; exists {
			continue
		}
		if members == nil {
			members = make(map[int64]struct{})
			s.members[observer] = members
		}
		members[subject] = struct{}{}
		s.recordEnterLocked(observer, subject)
	}
	if len(members) == 0 {
		delete(s.members, observer)
	}
}

// recordEnterLocked 登记反向关系并产生进入事件。
// 调用方持有 mu，负责正向成员表，并已确认关系确实发生变化。
func (s *RelationSource) recordEnterLocked(observer, subject int64) {
	observers := s.observers[subject]
	if observers == nil {
		observers = make(map[int64]struct{})
		s.observers[subject] = observers
	}
	observers[observer] = struct{}{}
	s.pending = append(s.pending, InterestEvent{Observer: observer, Subject: subject, Kind: InterestEnter})
}

// recordLeaveLocked 删除反向关系并产生离开事件；调用契约同 recordEnterLocked。
func (s *RelationSource) recordLeaveLocked(observer, subject int64) {
	observers := s.observers[subject]
	delete(observers, observer)
	if len(observers) == 0 {
		delete(s.observers, subject)
	}
	s.pending = append(s.pending, InterestEvent{Observer: observer, Subject: subject, Kind: InterestLeave, Band: -1})
}

// Forget removes a subject from every observer's set. It is what a departing
// player needs: the relation is gone in both directions, and the observers
// that held it must be told.
func (s *RelationSource) Forget(subject int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for observer := range s.observers[subject] {
		subjects := s.members[observer]
		delete(subjects, subject)
		s.pending = append(s.pending, InterestEvent{Observer: observer, Subject: subject, Kind: InterestLeave, Band: -1})
		if len(subjects) == 0 {
			delete(s.members, observer)
		}
	}
	delete(s.observers, subject)
}

// Flush drains the events, ordered like the spatial source's so the aggregator
// sees one deterministic sequence whatever the sources' internal iteration
// order was.
func (s *RelationSource) Flush() []InterestEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.pending) == 0 {
		return nil
	}
	events := s.pending
	s.pending = nil
	// 同一观察者与实体的事件保持产生顺序，避免把 Enter/Leave 颠倒。
	slices.SortStableFunc(events, func(a, b InterestEvent) int {
		if order := cmp.Compare(a.Observer, b.Observer); order != 0 {
			return order
		}
		return cmp.Compare(a.Subject, b.Subject)
	})
	return events
}

var _ Source = (*RelationSource)(nil)
