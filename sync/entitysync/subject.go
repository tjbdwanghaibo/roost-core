package entitysync

import (
	"cmp"
	"fmt"
	"slices"
	"sync"

	"github.com/tjbdwanghaibo/roost-core/entity"
)

// subscriptionKind is where one session stands with respect to one subject.
type subscriptionKind uint8

const (
	// kindSnapshot：等待全量快照。首次订阅尚无对象，换 profile 或撤回退订时
	// 则可能仍持有旧对象；是否欠 remove 必须查会话的已交付引用表。
	kindSnapshot subscriptionKind = iota + 1
	// kindLive: holds baseVersion; the next frame carries a delta from it.
	kindLive
	// kindLeaving: unsubscribed or the subject is retiring; the next frame
	// carries an ObjectRemove and the entry is dropped.
	kindLeaving
)

// subscription is one session's standing with one subject. It is the whole
// of what used to be a coordinator row plus a sink row: the profile the
// session wants, the kind of frame it is owed, and the version it holds.
type subscription struct {
	// revision 标识订阅意图；inFlight 防止首次快照在途时丢掉待删除记录。
	revision    uint64
	inFlight    bool
	lifetime    *sessionLifetime
	sources     map[*SubscriptionSource]entity.SyncProfile
	profile     entity.SyncProfile
	kind        subscriptionKind
	baseVersion uint64
}

// subject is an entity's sync state plus its subscribers. The subscribers
// live HERE, not in an index somewhere else: the subject is the truth about
// who receives it (ARCH-10).
type subject struct {
	mu          sync.Mutex
	id          int64
	state       *entity.SubjectSyncState
	subscribers map[SessionID]*subscription
	// retiring: Unregister was called; every subscriber is leaving and the
	// subject is forgotten once the last remove has gone out.
	retiring bool
}

func newSubject(state *entity.SubjectSyncState) *subject {
	return &subject{id: state.SubjectID(), state: state, subscribers: make(map[SessionID]*subscription)}
}

// profilesLocked 一次遍历收集两种内容需求，不在订阅数量上反复建立 profile map。
// 内容层统一规范化、去重和排序；held 会话仍保留原有捕获语义。
func (s *subject) profilesLocked(selected map[*subscription]bool) (delta, snapshot []entity.SyncProfile) {
	for _, sub := range s.subscribers {
		switch sub.kind {
		case kindLive:
			if !slices.Contains(delta, sub.profile) {
				delta = append(delta, sub.profile)
			}
		case kindSnapshot:
			if selected != nil && !selected[sub] {
				continue
			}
			if !slices.Contains(snapshot, sub.profile) {
				snapshot = append(snapshot, sub.profile)
			}
		}
	}
	return delta, snapshot
}

// sessionsLocked lists the subscribed sessions in ascending order.
func (s *subject) sessionsLocked() []SessionID {
	ids := make([]SessionID, 0, len(s.subscribers))
	for id := range s.subscribers {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

// bestProfile 的排序只决定已授权视图的选择，不推断字段之间的包含关系。
func (m *Manager) bestProfile(profiles map[*SubscriptionSource]entity.SyncProfile) entity.SyncProfile {
	var best entity.SyncProfile
	first := true
	for _, profile := range profiles {
		if first || m.CompareProfiles(profile, best) < 0 {
			best = profile
			first = false
		}
	}
	return best
}

// ValidateViewPriority 检查 packer 视图与 Manager 的实际选择顺序是否使用同一配置。
func (m *Manager) ValidateViewPriority(view entity.SyncView) error {
	priority, ok := m.config.ProfilePriorities[view.Profile.Normalize()]
	if !ok {
		priority = int(view.Profile.LOD)
	}
	if priority != view.Priority {
		return fmt.Errorf("entitysync: profile %+v priority is %d, view declares %d", view.Profile, priority, view.Priority)
	}
	return nil
}

func compareProfiles(a, b entity.SyncProfile) int {
	if order := cmp.Compare(a.LOD, b.LOD); order != 0 {
		return order
	}
	if order := cmp.Compare(a.Key, b.Key); order != 0 {
		return order
	}
	return cmp.Compare(a.SchemaVersion, b.SchemaVersion)
}

// CompareProfiles 将业务优先级与稳定平局规则集中在一处，Interest 与多政策来源共用。
func (m *Manager) CompareProfiles(a, b entity.SyncProfile) int {
	a, b = a.Normalize(), b.Normalize()
	priority := func(p entity.SyncProfile) int {
		if rank, ok := m.config.ProfilePriorities[p]; ok {
			return rank
		}
		return int(p.LOD)
	}
	if order := cmp.Compare(priority(a), priority(b)); order != 0 {
		return order
	}
	return compareProfiles(a, b)
}
