package policy

import (
	"context"
	"errors"
	"log/slog"
	"maps"
	"slices"
	"sync"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/entitysync"
)

// GroupViewConfig 是 NewGroupView 的输入。
type GroupViewConfig struct {
	// Manager 承载组视图，必填。GroupView 拥有其中“组实体”（房间等）的登记：同一 Manager 上的其他政策可以订阅它们，
	// 但不能自己 Register / Unregister 组实体。会话由别的政策打开与关闭（通常是同一 Manager 上的 SelfView）。
	Manager *entitysync.Manager
	// Profile 是成员看组实体的视图；组实体的 packer 必须认识它（未知视图在捕获时被拒绝，不会被换成别的视图）。
	Profile entity.SyncProfile
	// Lookup 返回内存里已发布的实体的同步状态，没有就返回 nil；在政策阶段调用，只读内存、不加载、不等待、
	// 不取实体锁（正式接线是 EntityManager.Get(id).Base().Sync()，与 SelfViewConfig.Lookup 相同）。必填。
	Lookup func(subjectID int64) *entity.SubjectSyncState
	// Session 把成员换成会话 ID；nil 表示会话 ID 就是成员 ID（与 SelfView 一样按玩家 ID 开会话）。
	Session func(member int64) entitysync.SessionID
	// MaxMembers 是每组成员上限，0 表示不限（Manager 的 MaxSubscribersPerSubject 照样生效）。
	MaxMembers int
}

// GroupView 是“成员看组实体”（房间、队伍、副本）的生命周期实现：每个组以一个实体为 subject，组里的成员都以
// Profile 订阅它，同一次 Flush 里大家收到同一份内容。它与 SelfView 是同一种写法、装在同一个 Manager 上：
// 会话归 SelfView（玩家上线开、全部连接断开关、新连接 Hold/Ready），GroupView 只管“哪些成员该看哪个组实体”。
//
// 与 Group 的区别是会话归属。Group 要求调用方自己开会话、Join 时会话必须已开、Manager 丢掉会话之后由调用方再 Join；
// 会话由别的政策打开与关闭时调用方不知道这些时刻，Group 的订阅会在重连之后静默缺失（会话关闭时订阅随之删除，
// Group 仍记着成员）。GroupView 把成员关系当作事实：
//
//	Join / Leave / Disband / EntityLoaded 只在 mu 下记事实并标记组待处理，不调用 Manager、不等待，可以在任何协程
//	（含 Nest 快池）调用；真正的登记、订阅与退订都在 Manager 的政策阶段（Flush 开头，flushGate 串行）按此刻的
//	净事实一次做完：
//
//	组实体在内存里 → Register（重复登记是空操作，卸载后重新加载的新状态接上并强制全量）
//	每名成员        → 会话已开就 SubscribeStamped，没开（离线、断线宽限里、会话还没打开）就留在 waiting，
//	                  每个政策阶段复查一次，会话一开就订阅、从全量开始
//	Leave          → 退订（成员的客户端收到 ObjectRemove）
//	Disband        → 退订全部成员并 Unregister 组实体（之后同 ID 可以重新成组，remove-before-create）
//
// 框架丢掉订阅（会话关闭 / 丢失、组实体被业务以外的路径注销）时经 Released 通知删除簿记，成员仍在组里时下一个
// 阶段重新订阅；卸载撤销后重新登记经 Resubmit 交还，按当前成员重新订阅。所以“断线 → 重连”“顶号 Hold/Ready”
// “组实体卸载后重载”都不需要业务再调用任何东西：重连的成员在会话打开的那个阶段收到组实体的全量。
//
// 成员关系是进程内事实，不持久：进程重启后由业务在玩家重新连接时再 Join（与写连接代的那个提交同一处）。
// 政策阶段的顺序就是登记顺序：GroupView 登记在 SelfView 之后时，SelfView 打开会话的同一个阶段里成员就能订阅；
// 反过来晚一个阶段。
type GroupView struct {
	manager    *entitysync.Manager
	source     *entitysync.SubscriptionSource
	profile    entity.SyncProfile
	lookup     func(int64) *entity.SubjectSyncState
	session    func(int64) entitysync.SessionID
	maxMembers int
	unhook     func()

	mu      sync.Mutex // 只保护下面的事实；锁内不调用 Manager
	facts   map[int64]*groupFacts
	retire  map[int64]struct{} // Disband 过、政策阶段还没退订与注销的组
	pending map[int64]struct{} // 自上个政策阶段以来事实有变化的组
	closed  bool

	// stageMu 串行化政策阶段（apply）、框架回调（released / resubmit）与 Close；groups / waiting 只在它下面读写。
	stageMu sync.Mutex
	groups  map[int64]*groupActual
	waiting map[int64]struct{} // 有成员还没订阅上（会话没开、登记被拒）或组实体不在内存的组，每个阶段复查
}

// groupFacts 是一个组此刻的事实：成员集合与“实体刚从权威加载”的提示。
type groupFacts struct {
	members map[int64]struct{}
	loaded  bool
}

// groupActual 是政策阶段维护的、一个组在 Manager 里的实际状态。
type groupActual struct {
	registered bool // 本 GroupView 登记过组实体（Disband 时据此注销）
	members    map[int64]*groupMember
}

// groupMember 是一名成员在 Manager 里的订阅状态。
type groupMember struct {
	session    entitysync.SessionID
	subscribed bool                         // 本来源的订阅在位（被框架丢掉时 released 置回 false）
	stamp      entitysync.SubscriptionStamp // 最近一次成功订阅的戳，释放通知只删除戳更早的
	failure    error                        // 上一次拒绝的原因，只在原因变化时记日志
}

// NewGroupView 创建组视图并在 Manager 上登记政策阶段。调用方在 Manager 关闭后调用 Close。
func NewGroupView(config GroupViewConfig) (*GroupView, error) {
	if config.Manager == nil {
		return nil, entitysync.ErrManagerClosed
	}
	if config.Lookup == nil {
		return nil, errors.New("policy: group view needs Lookup")
	}
	if config.MaxMembers < 0 {
		return nil, errors.New("policy: group view member limit is negative")
	}
	session := config.Session
	if session == nil {
		session = func(member int64) entitysync.SessionID { return entitysync.SessionID(member) }
	}
	view := &GroupView{
		manager: config.Manager, profile: config.Profile.Normalize(), lookup: config.Lookup, session: session, maxMembers: config.MaxMembers,
		facts: make(map[int64]*groupFacts), retire: make(map[int64]struct{}), pending: make(map[int64]struct{}),
		groups: make(map[int64]*groupActual), waiting: make(map[int64]struct{}),
	}
	view.source = config.Manager.NewSubscriptionSourceWithHooks(entitysync.SubscriptionSourceHooks{Resubmit: view.resubmit, Released: view.released})
	view.unhook = config.Manager.RegisterPolicy(entitysync.PolicyHooks{Apply: view.apply, Pending: view.hasPending})
	return view, nil
}

// Join 记录 member 是组 subject 的成员：会话打开的那个政策阶段起，它收到组实体（先全量，之后增量）。组不存在时
// 随之建立。重复 Join 是无变化的成功。拒绝：subject / member 为 0、组已满（ErrGroupMemberLimit）、视图已关闭
// （ErrGroupClosed）。不阻塞；调用方负责只在决定成员关系的业务事务提交之后调用（wiring/selfsync.Groups 替调用方
// 把 Nest 事务内的调用推迟到提交之后）。
func (v *GroupView) Join(subject, member int64) error {
	if v == nil {
		return ErrGroupClosed
	}
	if subject == 0 || member == 0 {
		return entitysync.ErrSubjectInvalid
	}
	v.mu.Lock()
	if v.closed {
		v.mu.Unlock()
		return ErrGroupClosed
	}
	facts := v.facts[subject]
	if facts == nil {
		facts = &groupFacts{members: make(map[int64]struct{})}
		v.facts[subject] = facts
	}
	if _, present := facts.members[member]; present {
		v.mu.Unlock()
		return nil
	}
	if v.maxMembers > 0 && len(facts.members) >= v.maxMembers {
		v.mu.Unlock()
		return ErrGroupMemberLimit
	}
	facts.members[member] = struct{}{}
	v.pending[subject] = struct{}{}
	v.mu.Unlock()
	v.manager.WakeSync()
	return nil
}

// Leave 记录 member 离开组：下一个政策阶段退订，客户端收到组实体的 ObjectRemove。不认识的成员 / 组被忽略。
func (v *GroupView) Leave(subject, member int64) {
	if v == nil {
		return
	}
	v.mu.Lock()
	facts := v.facts[subject]
	if v.closed || facts == nil {
		v.mu.Unlock()
		return
	}
	if _, present := facts.members[member]; !present {
		v.mu.Unlock()
		return
	}
	delete(facts.members, member)
	v.pending[subject] = struct{}{}
	v.mu.Unlock()
	v.manager.WakeSync()
}

// Disband 解散组：下一个政策阶段退订全部成员并注销组实体（成员收到 ObjectRemove）。之后同 ID 再 Join 是一个新组，
// 在旧登记退役完成之后重新登记。不认识的组被忽略。组实体随后可以销毁；先销毁也安全，注销照常进行。
func (v *GroupView) Disband(subject int64) {
	if v == nil {
		return
	}
	v.mu.Lock()
	if v.closed {
		v.mu.Unlock()
		return
	}
	delete(v.facts, subject)
	v.retire[subject] = struct{}{}
	v.pending[subject] = struct{}{}
	v.mu.Unlock()
	v.manager.WakeSync()
}

// EntityLoaded 记录一个实体刚从权威加载发布（DataEngine 的 OnEntityLoaded，在业务池上调用）。是某个组的实体时，
// 下一个政策阶段以新状态登记；其他实体忽略。不阻塞。
func (v *GroupView) EntityLoaded(subject int64) {
	if v == nil {
		return
	}
	v.mu.Lock()
	facts := v.facts[subject]
	if facts != nil && !v.closed {
		facts.loaded = true
		v.pending[subject] = struct{}{}
	}
	v.mu.Unlock()
	if facts != nil {
		v.manager.WakeSync()
	}
}

// Members 报告组 subject 当前的成员数（事实，不论是否已订阅上）。
func (v *GroupView) Members(subject int64) int {
	if v == nil {
		return 0
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if facts := v.facts[subject]; facts != nil {
		return len(facts.members)
	}
	return 0
}

// IsMember 报告 member 是否是组 subject 的成员（事实，不论是否已订阅上）。
func (v *GroupView) IsMember(subject, member int64) bool {
	if v == nil {
		return false
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if facts := v.facts[subject]; facts != nil {
		_, present := facts.members[member]
		return present
	}
	return false
}

// Groups 报告当前的组数（事实）。
func (v *GroupView) Groups() int {
	if v == nil {
		return 0
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	return len(v.facts)
}

// Close 撤下政策阶段并停止接收事实；幂等。在 Manager 关闭之后调用：订阅与登记随 Manager 一起释放。
func (v *GroupView) Close() {
	if v == nil {
		return
	}
	v.mu.Lock()
	already := v.closed
	v.closed = true
	clear(v.facts)
	clear(v.retire)
	clear(v.pending)
	v.mu.Unlock()
	if already {
		return
	}
	v.unhook()
	v.stageMu.Lock()
	clear(v.groups)
	clear(v.waiting)
	v.stageMu.Unlock()
}

func (v *GroupView) hasPending() bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	return len(v.pending) > 0
}

// released 是框架丢掉本来源订阅的通知（会话关闭 / 丢失、组实体被注销），在政策阶段的释放交付里调用：戳不晚于
// 通知的订阅已不在，置回未订阅；成员仍在组里时由下一个阶段（会话重开之后）重新订阅。
func (v *GroupView) released(pairs []entitysync.ReleasedSubscription) {
	var touched []int64
	v.stageMu.Lock()
	for _, pair := range pairs {
		group := v.groups[pair.Subject]
		if group == nil {
			continue
		}
		for _, member := range group.members {
			if member.session == pair.Session && member.subscribed && member.stamp < pair.Stamp {
				member.subscribed = false
				touched = append(touched, pair.Subject)
			}
		}
	}
	v.stageMu.Unlock()
	v.markPending(touched)
}

// resubmit 是卸载撤销后、组实体同 ID 重新登记时交还的订阅：置回未订阅，下一个阶段（同一次 Flush 的本政策阶段）
// 按当前成员重新订阅；缺席期间 Leave / Disband 过的不恢复。
func (v *GroupView) resubmit(retracted []entitysync.RetractedSubscription) {
	var touched []int64
	v.stageMu.Lock()
	for _, pair := range retracted {
		group := v.groups[pair.Subject]
		if group == nil {
			continue
		}
		for _, member := range group.members {
			if member.session == pair.Session && member.subscribed {
				member.subscribed = false
				touched = append(touched, pair.Subject)
			}
		}
	}
	v.stageMu.Unlock()
	v.markPending(touched)
}

func (v *GroupView) markPending(subjects []int64) {
	if len(subjects) == 0 {
		return
	}
	v.mu.Lock()
	if !v.closed {
		for _, subject := range subjects {
			v.pending[subject] = struct{}{}
		}
	}
	v.mu.Unlock()
}

// groupStep 是政策阶段对一个组读走的事实。members 为 nil 表示组已不存在（Disband 后没有再 Join）。
type groupStep struct {
	subject int64
	retire  bool
	loaded  bool
	members map[int64]struct{}
}

// apply 是政策阶段：取走待处理组（加上仍在 waiting 的）的事实，逐个对齐 Manager 里的状态。
func (v *GroupView) apply() error {
	v.stageMu.Lock()
	defer v.stageMu.Unlock()
	v.mu.Lock()
	if v.closed {
		v.mu.Unlock()
		return nil
	}
	candidates := make(map[int64]struct{}, len(v.pending)+len(v.waiting))
	maps.Copy(candidates, v.pending)
	maps.Copy(candidates, v.waiting)
	clear(v.pending)
	steps := make([]groupStep, 0, len(candidates))
	for _, subject := range slices.Sorted(maps.Keys(candidates)) {
		step := groupStep{subject: subject}
		_, step.retire = v.retire[subject]
		delete(v.retire, subject)
		if facts := v.facts[subject]; facts != nil {
			step.members, step.loaded = maps.Clone(facts.members), facts.loaded
			facts.loaded = false
		}
		steps = append(steps, step)
	}
	v.mu.Unlock()
	for _, step := range steps {
		v.align(step)
	}
	return nil
}

// align 把一个组在 Manager 里的状态对齐到它此刻的事实。只在 stageMu 下、政策阶段调用；Manager 调用都不等待，
// 也不会回调 GroupView（释放与重新提交在下个政策阶段交付）。
func (v *GroupView) align(step groupStep) {
	group := v.groups[step.subject]
	if step.retire && group != nil {
		v.dissolve(step.subject, group)
		group = nil
	}
	if step.members == nil {
		delete(v.waiting, step.subject)
		return
	}
	if group == nil {
		group = &groupActual{members: make(map[int64]*groupMember)}
		v.groups[step.subject] = group
	}
	for id, member := range group.members {
		if _, stays := step.members[id]; stays {
			continue
		}
		delete(group.members, id)
		if member.subscribed {
			v.unsubscribe(member.session, step.subject)
		}
	}
	if !v.ensureSubject(step, group) {
		v.waiting[step.subject] = struct{}{}
		return
	}
	complete := true
	for _, id := range slices.Sorted(maps.Keys(step.members)) {
		member := group.members[id]
		if member == nil {
			member = &groupMember{session: v.session(id)}
			group.members[id] = member
		}
		if member.subscribed {
			continue
		}
		stamp, err := v.source.SubscribeStamped(member.session, step.subject, v.profile)
		if err != nil {
			complete = false
			v.refused(step.subject, id, member, err)
			continue
		}
		member.subscribed, member.stamp, member.failure = true, stamp, nil
	}
	if complete {
		delete(v.waiting, step.subject)
	} else {
		v.waiting[step.subject] = struct{}{}
	}
}

// dissolve 退订组里每名成员并注销组实体（本 GroupView 登记过时）。
func (v *GroupView) dissolve(subject int64, group *groupActual) {
	delete(v.groups, subject)
	delete(v.waiting, subject)
	for _, member := range group.members {
		if member.subscribed {
			v.unsubscribe(member.session, subject)
		}
	}
	if group.registered {
		if err := v.manager.Unregister(subject); err != nil && !errors.Is(err, entitysync.ErrSubjectNotRegistered) {
			slog.Debug("group view: group subject not retired", "subject_id", subject, "err", err)
		}
	}
}

func (v *GroupView) unsubscribe(session entitysync.SessionID, subject int64) {
	err := v.source.Unsubscribe(session, subject)
	if err != nil && !errors.Is(err, entitysync.ErrSubscriptionNotFound) && !errors.Is(err, entitysync.ErrSubjectNotRegistered) && !errors.Is(err, entitysync.ErrSessionUnknown) {
		slog.Debug("group view: member not unsubscribed", "session", session, "subject_id", subject, "err", err)
	}
}

// ensureSubject 让组实体以内存里当前的状态登记；返回组实体此刻是否可订阅。实体不在内存（还没加载、事务内新建被
// 回滚）时，之前登记过就继续用 Manager 里的登记（卸载后由 Nest Mod 的重载 Rebind），从没登记过就留到下个阶段。
// 每个候选阶段都调用 Register：同一状态是空操作；被业务以外的路径注销 / 忘掉之后是新的登记；旧状态已关闭时接上新状态。
func (v *GroupView) ensureSubject(step groupStep, group *groupActual) bool {
	state := v.lookup(step.subject)
	if state == nil || !state.Enabled() {
		return group.registered
	}
	switch err := v.manager.Register(state); {
	case err == nil, errors.Is(err, entitysync.ErrSubjectRegistered):
		group.registered = true
		return true
	case errors.Is(err, entitysync.ErrSubjectRetiring), errors.Is(err, entitysync.ErrManagerClosed), errors.Is(err, entitysync.ErrManagerStopping):
		// 上一次 Disband（或回滚撤回）的 remove 还没交付完：退役完成之后再登记。Manager 正在停止 / 已关闭时也不再登记。
		return false
	default:
		slog.Warn("group view: group subject registration refused; retrying at the next sync tick", "subject_id", step.subject, "err", err)
		return false
	}
}

// refused 记录一次订阅拒绝；同一原因只记一次，避免 waiting 里的成员每个 tick 刷日志。会话没开（离线、断线宽限内、
// 会话还没打开）是正常状态，记 Debug。
func (v *GroupView) refused(subject, member int64, state *groupMember, err error) {
	if state.failure != nil && (errors.Is(err, state.failure) || err.Error() == state.failure.Error()) {
		return
	}
	state.failure = err
	level := slog.LevelWarn
	if errors.Is(err, entitysync.ErrSessionUnknown) || errors.Is(err, entitysync.ErrSubjectRetiring) || errors.Is(err, entitysync.ErrSubjectNotRegistered) ||
		errors.Is(err, entitysync.ErrManagerClosed) || errors.Is(err, entitysync.ErrManagerStopping) {
		level = slog.LevelDebug
	}
	slog.Log(context.Background(), level, "group view: member subscription refused; retrying at the next sync tick", "subject_id", subject, "member", member, "err", err)
}
