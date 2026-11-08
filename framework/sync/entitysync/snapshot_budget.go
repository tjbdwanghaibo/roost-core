package entitysync

import (
	"container/list"
	"maps"
	"slices"
	"time"
)

func (m *Manager) refreshSnapshotWindow() {
	m.refreshSnapshotWindowAt(time.Now())
}

// refreshSnapshotWindowAt 在 on_change 模式下按 Interval 轮转冷创建预算窗口，并把等待快照的 subject 重新排进 pending。
// 调用者持有 flush 所有权；时间参数便于验证晚醒和长时间空闲，不依赖 sleep。
func (m *Manager) refreshSnapshotWindowAt(now time.Time) {
	if m.config.Mode != ModeOnChange {
		return
	}
	interval := m.config.Interval
	if m.budgetWindow.IsZero() || now.Sub(m.budgetWindow) >= interval {
		if m.budgetWindow.IsZero() {
			m.budgetWindow = now
		} else {
			// 保持窗口边界，跳过空闲窗口而不积攒额度，也不把晚醒延迟滚入下一窗口。
			m.budgetWindow = m.budgetWindow.Add(now.Sub(m.budgetWindow) / interval * interval)
		}
		for _, lane := range m.lanes {
			lane.windowAllowance = snapshotAllowance{}
			lane.windowByteBlocked = false
			clear(lane.windowSessions)
			if lane.windowSessions == nil {
				lane.windowSessions = make(map[SessionID]int)
			}
		}
		m.pendingMu.Lock()
		for id := range m.waitingSnapshots {
			m.pending[id] = struct{}{}
		}
		m.pendingOverlap = len(m.waitingSnapshots)
		// 等待年龄在实际没有欠快照时才清除，不能在窗口轮转时重置。
		m.pendingMu.Unlock()
	}
}

type snapshotWait struct {
	subject *subject
	since   time.Time
	order   *list.Element
}

// deferSnapshot 只延后预算拒绝的重查。dirty/remove/订阅变化仍可直接 markPending。
// 不保存 subscription 指针或意图，重查时只使用当前 subject 的最新订阅，旧会话不会复活。
func (m *Manager) deferSnapshot(subj *subject) {
	if m.config.Mode != ModeOnChange {
		m.markPending(subj.id)
		return
	}
	// 与 forget 的删除串行化，防止退休完成后重新留下永远无法排空的重查项。
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.subjects[subj.id] != subj {
		return
	}
	m.pendingMu.Lock()
	if old, ok := m.waitingSnapshots[subj.id]; !ok || old.subject != subj {
		m.removeSnapshotWaitLocked(subj.id)
		m.waitingSnapshots[subj.id] = snapshotWait{subject: subj, since: time.Now(), order: m.waitingOrder.PushBack(subj.id)}
		if _, pending := m.pending[subj.id]; pending {
			m.pendingOverlap++
		}
		if m.config.Trace != nil {
			m.config.Trace.Record(SyncTraceEvent{Stage: "subject_budget_wait", SubjectID: subj.id, Snapshot: true})
		}
	}
	m.pendingMu.Unlock()
}

// clearSnapshotWaitLocked 在 subject 已没有可服务的待快照订阅时移除它的等待项。
// 调用者持有 subject.mu；成功交付/退订后及时清理等待观测，避免 Drain 空转。
func (m *Manager) clearSnapshotWaitLocked(subj *subject) {
	m.pendingMu.Lock()
	waiting := m.waitingSnapshots[subj.id].subject == subj
	m.pendingMu.Unlock()
	if !waiting {
		return
	}
	for sid, sub := range subj.subscribers {
		if sub.kind == kindSnapshot {
			if sess := m.session(sid); sess != nil && !sess.held && sess.lifetime == sub.lifetime {
				return
			}
		}
	}
	m.pendingMu.Lock()
	if wait := m.waitingSnapshots[subj.id]; wait.subject == subj {
		m.removeSnapshotWaitLocked(subj.id)
	}
	m.pendingMu.Unlock()
}

// removeSnapshotWaitLocked 删除等待项：索引与按首次等待时间排序的链表一起删除。调用方持有 pendingMu。
func (m *Manager) removeSnapshotWaitLocked(id int64) {
	wait, ok := m.waitingSnapshots[id]
	if !ok {
		return
	}
	m.waitingOrder.Remove(wait.order)
	delete(m.waitingSnapshots, id)
	if _, pending := m.pending[id]; pending {
		m.pendingOverlap--
	}
}

// SnapshotBudget 是快照准入软预算：periodic 每次 Flush 重置，on_change 在
// 同一个 Interval 窗口共享额度。MaxBytes 按实体更新包及
// 对象/组件头计，不含外层帧头；单包超过软预算时允许它独占一次额度以保证进度。
// 只限制客户端尚未持有对象的创建。现有对象的全量替换、增量和 remove 不占额度；
// 帧/组件/传输的硬上限仍然有效，预算不能替代总带宽或队列背压。
// 新入场与 Hold 后的基线恢复轮流使用同一额度，一类为空时另一类可用完剩余额度。
// 设了 MaxBytes 时按上次捕获大小预判准入，被挡的冷创建不在每个等待窗口重复打包；
// 发送的始终是准入当轮的最新冻结内容。
//
// 配置了 ManagerConfig.Namespaces 时，Manager 级预算只管没有列出的命名空间；列出的命名空间各用
// NamespaceLimits.SnapshotBudget，各路互不占额度，轮转、字节软上限与窗口语义在每一路内与上面相同。
type SnapshotBudget struct {
	MaxObjects        int
	MaxBytes          int
	PerSessionObjects int
}

// snapshotLane 是一路独立的快照预算：额度、会话与实体轮转游标、待快照索引。第 0 路服务没有列出的命名空间
// （Manager 级 SnapshotBudget，嵌在 Manager 里），每个列出的命名空间一路（Manager.lanes 按 subject.class 索引）。
// 额度窗口的起点（budgetWindow）各路共用。除 snapshotRequests 自带锁外，字段只由持有 flushGate 的调用方读写。
type snapshotLane struct {
	budget            SnapshotBudget
	snapshotRequests  snapshotRequests
	windowAllowance   snapshotAllowance
	windowSessions    map[SessionID]int
	snapshotCursor    [2]SessionID  // 两类分别保留会话轮转位置
	windowByteBlocked bool          // 本窗口已遇到放不下的冷对象，留到下一窗口首位竞争
	snapshotNextClass snapshotClass // 小额度跨窗口仍轮流服务两类
}

// snapshotMarks 是一个会话在各路预算、两类来源下最近准入的请求序号（各路序号独立编号）。
// 第 0 路内联；其余路放在 more 指向的切片里，写时整份替换：编码副本、批次与已发布会话共享它，不能就地修改。
// 用指针而不是切片头，session 只多 8 字节。
type snapshotMarks struct {
	base [2]int64
	more *[][2]int64 // 第 1.. 路，下标为路号减一；未记录的路为零
}

func (s snapshotMarks) get(lane uint8, class snapshotClass) int64 {
	if lane == 0 {
		return s.base[class]
	}
	if s.more != nil && int(lane) <= len(*s.more) {
		return (*s.more)[lane-1][class]
	}
	return 0
}

func (s *snapshotMarks) set(lane uint8, class snapshotClass, sequence int64) {
	if lane == 0 {
		s.base[class] = sequence
		return
	}
	var old [][2]int64
	if s.more != nil {
		old = *s.more
	}
	more := make([][2]int64, max(len(old), int(lane)))
	copy(more, old)
	more[lane-1][class] = sequence
	s.more = &more
}

func (b SnapshotBudget) enabled() bool {
	return b.MaxObjects > 0 || b.MaxBytes > 0 || b.PerSessionObjects > 0
}

type snapshotAllowance struct{ objects, bytes int }

// snapshotObjectOverhead 是 MaxBytes 计入的对象/组件头，加在实体更新包长度之上。
const snapshotObjectOverhead = 18

// snapshotClass 表达业务来源，不能通过变更 Profile 提升恢复请求的优先级。
type snapshotClass uint8

const (
	snapshotArrival snapshotClass = iota
	snapshotRecovery
)

type snapshotCandidate struct {
	subjectID int64
	sessionID SessionID
	sub       *subscription
	class     snapshotClass
	sequence  int64 // 意图入队顺序（每路独立编号），不能让不断到来的较小/较大 Entity ID 插队
	lane      uint8 // 预算路，即 subject.class
}

// snapshotPlan 是本轮全部有预算的路的计划：order 按路号连续排列，同一路内保持该路的轮转顺序。
type snapshotPlan struct {
	selected    map[*subscription]bool
	order       []snapshotCandidate // 数量预选与字节准入共用顺序，不能再按会话重新争抢额度
	charges     []snapshotCharge    // 仅计划预留；真正尝试 Push 的会话才结算窗口与游标
	byteBlocked []bool              // 按路；遇到放不下的冷对象的路，懒分配
}

// selectedFor 是 class 这一路本轮的入选集合；不设预算的路（或整轮没有预算）返回 nil，表示该路全部快照照常捕获。
func (plan *snapshotPlan) selectedFor(m *Manager, class uint8) map[*subscription]bool {
	if plan == nil || !m.lanes[class].budget.enabled() {
		return nil
	}
	return plan.selected
}

func (plan *snapshotPlan) blocked(lane uint8) bool {
	return int(lane) < len(plan.byteBlocked) && plan.byteBlocked[lane]
}

func (plan *snapshotPlan) block(lane uint8, lanes int) {
	if plan.byteBlocked == nil {
		plan.byteBlocked = make([]bool, lanes)
	}
	plan.byteBlocked[lane] = true
}

type snapshotCharge struct {
	candidate snapshotCandidate
	bytes     int
}

// planSnapshotCaptures 规划本轮冷创建：每一路预算各自规划（见 planLane），结果按路号连续排进同一个计划。
// 返回 nil 表示没有任何一路设预算；非 nil 的空计划表示有预算的路额度都已耗尽。
func (m *Manager) planSnapshotCaptures(ids []int64) *snapshotPlan {
	var plan *snapshotPlan
	var pending map[int64]struct{}
	// 固定本轮的 subject 集合并取读锁；只在确有一路要遍历候选时做，额度已耗尽的窗口不付这笔开销。
	prepare := func() map[int64]struct{} {
		if pending == nil {
			pending = make(map[int64]struct{}, len(ids))
			for _, id := range ids {
				pending[id] = struct{}{}
			}
			m.mu.RLock()
		}
		return pending
	}
	defer func() {
		if pending != nil {
			m.mu.RUnlock()
		}
	}()
	for index, lane := range m.lanes {
		if !lane.budget.enabled() {
			continue
		}
		if plan == nil {
			plan = &snapshotPlan{selected: make(map[*subscription]bool)}
		}
		m.planLane(plan, uint8(index), lane, prepare)
	}
	return plan
}

// planLane 规划一路的冷创建：按业务来源交替，来源内部沿用会话和实体轮转，空闲来源的额度可被另一类使用。
//
// 设了 MaxBytes 时，计划按上次捕获的大小（未知时取最小可能大小）先做一遍与
// scheduleSnapshots 同序的字节预判：预判放不下的首个候选及其后缀本轮不捕获，
// 被挡的大对象不会在每个等待窗口重复打包。预判只决定“是否值得捕获”，
// 准入仍按本轮最新冻结内容的实际编码计算；窗口内首个对象照常独占软预算。
func (m *Manager) planLane(plan *snapshotPlan, index uint8, lane *snapshotLane, prepare func() map[int64]struct{}) {
	limit := lane.budget
	if m.config.Mode == ModeOnChange && lane.windowByteBlocked {
		return
	}
	if m.config.Mode == ModeOnChange && limit.MaxBytes > 0 && lane.windowAllowance.objects > 0 && lane.windowAllowance.bytes >= limit.MaxBytes {
		return
	}
	remaining := limit.MaxObjects
	if remaining > 0 && m.config.Mode == ModeOnChange {
		remaining = max(0, remaining-lane.windowAllowance.objects)
		if remaining == 0 {
			return
		}
	}

	// 仅遍历增量索引中的快照请求。
	pending := prepare()
	lane.snapshotRequests.mu.Lock()
	defer lane.snapshotRequests.mu.Unlock()
	var iterators [2]snapshotIterator
	for class := snapshotArrival; class <= snapshotRecovery; class++ {
		order := lane.snapshotRequests.sessionOrder(class)
		start, _ := slices.BinarySearch(order, lane.snapshotCursor[class])
		for start < len(order) && order[start] <= lane.snapshotCursor[class] {
			start++
		}
		iterators[class] = snapshotIterator{lane: lane, laneIndex: index, sessions: order, start: start}
	}
	exhausted := [2]bool{}
	selectedSessions := make(map[SessionID]int)
	next := lane.snapshotNextClass
	admitted, plannedBytes := 0, 0 // 本窗口已结算加本计划预选的对象数与预估字节
	if m.config.Mode == ModeOnChange {
		admitted, plannedBytes = lane.windowAllowance.objects, lane.windowAllowance.bytes
	}
	planned := 0

	for !exhausted[0] || !exhausted[1] {
		class := next
		if exhausted[class] {
			class = 1 - class
		}
		candidate, ok := iterators[class].next(m, class, pending)
		if !ok {
			exhausted[class] = true
			continue
		}
		selected := selectedSessions[candidate.sessionID]
		if m.config.Mode == ModeOnChange {
			selected += lane.windowSessions[candidate.sessionID]
		}
		if limit.PerSessionObjects > 0 && selected >= limit.PerSessionObjects {
			continue
		}
		if limit.MaxBytes > 0 {
			bytes := lane.snapshotRequests.estimateLocked(candidate.sub)
			if admitted > 0 && bytes > limit.MaxBytes-plannedBytes {
				// 与准入同一规则：首个放不下的候选挡住后缀，游标不越过它。
				plan.block(index, len(m.lanes))
				break
			}
			admitted++
			plannedBytes += bytes
		}
		plan.selected[candidate.sub] = true
		plan.order = append(plan.order, candidate)
		planned++
		selectedSessions[candidate.sessionID]++
		next = 1 - class
		if limit.MaxObjects > 0 && planned >= remaining {
			break
		}
	}
}

// scheduleSnapshots 在组帧前按同一顺序预留额度，不修改实际窗口或游标。
// 这里只读取捕获的 settlement，不读可变订阅字段。
// 现有对象的 Full 更新、delta、remove 不消耗冷创建额度；不设预算的路的冷创建也不经过这里。
func (m *Manager) scheduleSnapshots(work map[SessionID]*flushSession, plan *snapshotPlan) {
	for _, batch := range work {
		batch.snapshotAfter = batch.session.snapshotAfter
	}
	if plan == nil {
		return
	}
	type capturedSnapshot struct {
		batch  *flushSession
		entry  *frameEntry
		settle *settlement
	}
	pending := make(map[*subscription]capturedSnapshot, len(plan.order))
	for _, batch := range work {
		for i := range batch.entries {
			entry := &batch.entries[i]
			if entry.kind != entryCreate || !m.lanes[entry.class].budget.enabled() {
				continue
			}
			if _, exists := batch.session.objects[entry.subjectID]; exists {
				continue
			}
			pending[batch.settlements[i].sub] = capturedSnapshot{batch, entry, &batch.settlements[i]}
		}
	}
	for index, lane := range m.lanes {
		limit := lane.budget
		if limit.MaxBytes <= 0 {
			continue
		}
		// 捕获过的冷创建都记下实际大小（含本轮会被挡下的），下一轮计划据此判断是否值得再捕获。
		sizes := make(map[*subscription]int, len(pending))
		for sub, item := range pending {
			if item.entry.class == uint8(index) {
				sizes[sub] = snapshotObjectOverhead + subjectUpdateBytes(item.entry.update.update)
			}
		}
		lane.snapshotRequests.recordEstimates(sizes)
	}
	// 各路按计划顺序各自预留：order 按路号连续排列，一路内的字节阻挡只挡这一路的后缀。
	for index, lane := range m.lanes {
		limit := lane.budget
		if !limit.enabled() {
			continue
		}
		used := snapshotAllowance{}
		sessions := make(map[SessionID]int)
		if m.config.Mode == ModeOnChange {
			used = lane.windowAllowance
			sessions = maps.Clone(lane.windowSessions)
		}
		for _, candidate := range plan.order {
			if candidate.lane != uint8(index) {
				continue
			}
			item, ok := pending[candidate.sub]
			if !ok {
				continue
			}
			batch, entry := item.batch, item.entry
			if m.session(candidate.sessionID) != batch.session || item.settle.snapshotClass != candidate.class {
				continue
			}
			data, err := entry.update.encode(m.wire.limits.MaxComponentBytes)
			if err != nil {
				delete(pending, candidate.sub)
				continue
			} // 保留在帧编码中，走统一失败路径
			bytes := snapshotObjectOverhead + len(data)
			if (limit.MaxObjects > 0 && used.objects >= limit.MaxObjects) ||
				(limit.PerSessionObjects > 0 && sessions[candidate.sessionID] >= limit.PerSessionObjects) {
				continue
			}
			if limit.MaxBytes > 0 && used.objects > 0 && bytes > limit.MaxBytes-used.bytes {
				// 不允许后续小包越过首个被挡对象推进游标；下一窗口它可独占软预算。
				// 本窗口也停止重捕获冷请求，避免即时通知反复编码同一个大包。
				plan.block(uint8(index), len(m.lanes))
				break
			}
			used.objects++
			used.bytes += bytes
			sessions[candidate.sessionID]++
			batch.snapshotAfter.set(candidate.lane, candidate.class, candidate.sequence)
			plan.charges = append(plan.charges, snapshotCharge{candidate, bytes})
			delete(pending, candidate.sub)
		}
	}
	for _, item := range pending {
		item.settle.subj.mu.Lock()
		item.settle.sub.inFlight = false
		item.settle.subj.mu.Unlock()
		m.deferSnapshot(item.settle.subj)
		m.snapshotsDeferred.Add(1)
		item.entry.kind = 0
	}
	for _, batch := range work {
		count := 0
		for i, entry := range batch.entries {
			if entry.kind == 0 {
				continue
			}
			batch.entries[count], batch.settlements[count] = entry, batch.settlements[i]
			count++
		}
		clear(batch.entries[count:])
		clear(batch.settlements[count:])
		batch.entries, batch.settlements = batch.entries[:count], batch.settlements[:count]
	}
}

// admissionOrder 让带冷创建计费的会话按计划中首次出现的顺序先尝试 Push，其余会话仍按 ID 顺序。
// RetryLater 或取消中断准入时，实际尝试过的会话因此就是计划前缀，计费与游标一致。
func (plan *snapshotPlan) admissionOrder(sessionIDs []SessionID) []SessionID {
	if plan == nil || len(plan.charges) == 0 {
		return sessionIDs
	}
	planned := make(map[SessionID]bool, len(plan.charges))
	ordered := make([]SessionID, 0, len(sessionIDs))
	for _, charge := range plan.charges {
		if sid := charge.candidate.sessionID; !planned[sid] {
			planned[sid] = true
			ordered = append(ordered, sid)
		}
	}
	for _, sid := range sessionIDs {
		if !planned[sid] {
			ordered = append(ordered, sid)
		}
	}
	return ordered
}

// commitSnapshotAttempts 结算本轮冷创建额度：一次 Push 尝试消费该会话本批冷创建的额度（包括 RetryLater），防止同窗口空转。
// 编码失败、会话失效、提前取消和未轮到的会话不消费额度。每一路的游标按该路计划顺序只推进到
// 实际尝试的前缀：遇到仍然有效却未被尝试的会话就停下，其后即使有已尝试的会话
// （同一会话跨两个类别或两路时可能出现）也不越过它；已失效的会话不再参与轮转，可以越过。
// 已发送的帧前缀仍由 Flush 单独结算。
func (m *Manager) commitSnapshotAttempts(work map[SessionID]*flushSession, plan *snapshotPlan) {
	if plan == nil {
		return
	}
	for index, lane := range m.lanes {
		if !lane.budget.enabled() {
			continue
		}
		attempted, charged := 0, 0
		prefix := true
		for _, charge := range plan.charges {
			candidate := charge.candidate
			if candidate.lane != uint8(index) {
				continue
			}
			charged++
			batch := work[candidate.sessionID]
			if !batch.snapshotAttempted {
				if m.session(candidate.sessionID) == batch.session {
					prefix = false
				}
				continue
			}
			attempted++
			if m.config.Mode == ModeOnChange {
				lane.windowAllowance.objects++
				lane.windowAllowance.bytes += charge.bytes
				lane.windowSessions[candidate.sessionID]++
			}
			if prefix {
				lane.snapshotCursor[candidate.class] = candidate.sessionID
				lane.snapshotNextClass = 1 - candidate.class
			}
		}
		if m.config.Mode == ModeOnChange && plan.blocked(uint8(index)) && attempted == charged {
			lane.windowByteBlocked = true
		}
	}
}
