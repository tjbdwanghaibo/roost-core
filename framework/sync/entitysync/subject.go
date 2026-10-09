package entitysync

import (
	"cmp"
	"fmt"
	"slices"
	"sync"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
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
	revision uint64
	inFlight bool
	lifetime *sessionLifetime
	sources  map[*SubscriptionSource]entity.SyncProfile
	profile  entity.SyncProfile
	kind     subscriptionKind
	// snapshotClass 只决定冷对象创建的调度；有效 Profile 和线协议语义不变。
	snapshotClass snapshotClass
	baseVersion   uint64
}

// subject is an entity's sync state plus its subscribers. The subscribers
// live HERE, not in an index somewhere else: the subject is the truth about
// who receives it (ARCH-10).
type subject struct {
	mu sync.Mutex
	id int64
	// state 是实体的内容状态；实体卸载后重新加载时由 Rebind 在 mu 下替换，读者在 mu 下取用。
	state       *entity.SubjectSyncState
	subscribers map[SessionID]*subscription
	// retiring: Unregister was called; every subscriber is leaving and the
	// subject is forgotten once the last remove has gone out.
	retiring bool
	// successor 是 RegisterAfterRetirement 排在退役完成之后登记的状态，至多一个；
	// forgotten 表示 forget 已经取走 successor，之后不能再排队（调用方改走 Register）。
	successor *queuedRegistration
	forgotten bool
	// unloadRetracted：退役来自 RetractUnloadedSubject（实体被仅内存卸载后重载不了，RR-20260926-59），不是业务
	// Unregister。退役完成前该实体又被加载出来时，Rebind / Register 把新状态排在退役完成之后登记
	// （RegisterAfterRetirement 的同一机制），不返回 ErrSubjectRetiring。业务再 Unregister 时清除。
	unloadRetracted bool
	// 缓存发布后不可修改；CaptureSync 解开 subject 锁后仍会使用这两组需求。
	// 增删订阅以及有效 profile/kind 改变必须使 profilesValid 失效。
	profilesValid                   bool
	deltaProfiles, snapshotProfiles []entity.SyncProfile
}

// currentState 读取当前内容状态；Rebind 会在 subject.mu 下替换它。
func (s *subject) currentState() *entity.SubjectSyncState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

func newSubject(state *entity.SubjectSyncState) *subject {
	return &subject{id: state.SubjectID(), state: state, subscribers: make(map[SessionID]*subscription)}
}

// profilesLocked 一次遍历收集两种内容需求，不在订阅数量上反复建立 profile map。
// 内容层统一规范化、去重和排序；held 会话仍保留原有捕获语义。
func (s *subject) profilesLocked(selected map[*subscription]bool) (delta, snapshot []entity.SyncProfile) {
	if !s.profilesValid {
		s.deltaProfiles, s.snapshotProfiles = s.collectProfilesLocked(nil)
		s.profilesValid = true
	}
	if selected == nil {
		return s.deltaProfiles, s.snapshotProfiles
	}
	// 空选择不必遍历订阅；非空集合只过滤预算相关的快照需求。
	if len(selected) == 0 {
		return s.deltaProfiles, nil
	}
	_, snapshot = s.collectProfilesLocked(selected)
	return s.deltaProfiles, snapshot
}

func (s *subject) collectProfilesLocked(selected map[*subscription]bool) (delta, snapshot []entity.SyncProfile) {
	for _, sub := range s.subscribers {
		switch sub.kind {
		case kindLive:
			if selected == nil && !slices.Contains(delta, sub.profile) {
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
