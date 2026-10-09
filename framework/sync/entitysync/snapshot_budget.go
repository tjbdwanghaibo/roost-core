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
		m.windowAllowance = snapshotAllowance{}
		m.windowByteBlocked = false
		clear(m.windowSessions)
		if m.windowSessions == nil {
			m.windowSessions = make(map[SessionID]int)
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

// 调用方持有 pendingMu；索引与按首次等待时间排序的链表一起删除。
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
type SnapshotBudget struct {
	MaxObjects        int
	MaxBytes          int
	PerSessionObjects int
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
	sequence  int64 // 意图入队顺序，不能让不断到来的较小/较大 Entity ID 插队
}

type snapshotPlan struct {
	selected    map[*subscription]bool
	order       []snapshotCandidate // 数量预选与字节准入共用顺序，不能再按会话重新争抢额度
	charges     []snapshotCharge    // 仅计划预留；真正尝试 Push 的会话才结算窗口与游标
	byteBlocked bool
}

type snapshotCharge struct {
	candidate snapshotCandidate
	bytes     int
}

// 冷创建按业务来源交替，来源内部沿用会话和实体轮转。空闲来源的额度可被另一类使用。
// 返回 nil 表示不设预算；非 nil 的空计划表示额度已耗尽。
//
// 设了 MaxBytes 时，计划按上次捕获的大小（未知时取最小可能大小）先做一遍与
// scheduleSnapshots 同序的字节预判：预判放不下的首个候选及其后缀本轮不捕获，
// 被挡的大对象不会在每个等待窗口重复打包。预判只决定“是否值得捕获”，
// 准入仍按本轮最新冻结内容的实际编码计算；窗口内首个对象照常独占软预算。
func (m *Manager) planSnapshotCaptures(ids []int64) *snapshotPlan {
	limit := m.config.SnapshotBudget
	if !limit.enabled() {
		return nil
	}
	plan := &snapshotPlan{selected: make(map[*subscription]bool)}
	if m.config.Mode == ModeOnChange && m.windowByteBlocked {
		return plan
	}
	if m.config.Mode == ModeOnChange && limit.MaxBytes > 0 && m.windowAllowance.objects > 0 && m.windowAllowance.bytes >= limit.MaxBytes {
		return plan
	}
	remaining := limit.MaxObjects
	if remaining > 0 && m.config.Mode == ModeOnChange {
		remaining = max(0, remaining-m.windowAllowance.objects)
		if remaining == 0 {
			return plan
		}
	}

	// 固定本轮的 subject 集合；仅遍历增量索引中的快照请求。
	pending := make(map[int64]struct{}, len(ids))
	for _, id := range ids {
		pending[id] = struct{}{}
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	m.snapshotRequests.mu.Lock()
	defer m.snapshotRequests.mu.Unlock()
	var iterators [2]snapshotIterator
	for class := snapshotArrival; class <= snapshotRecovery; class++ {
		order := m.snapshotRequests.sessionOrder(class)
		start, _ := slices.BinarySearch(order, m.snapshotCursor[class])
		for start < len(order) && order[start] <= m.snapshotCursor[class] {
			start++
		}
		iterators[class] = snapshotIterator{sessions: order, start: start}
	}
	exhausted := [2]bool{}
	selectedSessions := make(map[SessionID]int)
	next := m.snapshotNextClass
	admitted, plannedBytes := 0, 0 // 本窗口已结算加本计划预选的对象数与预估字节
	if m.config.Mode == ModeOnChange {
		admitted, plannedBytes = m.windowAllowance.objects, m.windowAllowance.bytes
	}

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
			selected += m.windowSessions[candidate.sessionID]
		}
		if limit.PerSessionObjects > 0 && selected >= limit.PerSessionObjects {
			continue
		}
		if limit.MaxBytes > 0 {
			bytes := m.snapshotRequests.estimateLocked(candidate.sub)
			if admitted > 0 && bytes > limit.MaxBytes-plannedBytes {
				// 与准入同一规则：首个放不下的候选挡住后缀，游标不越过它。
				plan.byteBlocked = true
				break
			}
			admitted++
			plannedBytes += bytes
		}
		plan.selected[candidate.sub] = true
		plan.order = append(plan.order, candidate)
		selectedSessions[candidate.sessionID]++
		next = 1 - class
		if limit.MaxObjects > 0 && len(plan.order) >= remaining {
			break
		}
	}
	return plan
}

// scheduleSnapshots 在组帧前按同一顺序预留额度，不修改实际窗口或游标。
// 这里只读取捕获的 settlement，不读可变订阅字段。
// 现有对象的 Full 更新、delta、remove 不消耗冷创建额度。
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
			if entry.kind != entryCreate {
				continue
			}
			if _, exists := batch.session.objects[entry.subjectID]; exists {
				continue
			}
			pending[batch.settlements[i].sub] = capturedSnapshot{batch, entry, &batch.settlements[i]}
		}
	}
	limit := m.config.SnapshotBudget
	if limit.MaxBytes > 0 {
		// 捕获过的冷创建都记下实际大小（含本轮会被挡下的），下一轮计划据此判断是否值得再捕获。
		sizes := make(map[*subscription]int, len(pending))
		for sub, item := range pending {
			sizes[sub] = snapshotObjectOverhead + subjectUpdateBytes(item.entry.update.update)
		}
		m.snapshotRequests.recordEstimates(sizes)
	}
	used := snapshotAllowance{}
	sessions := make(map[SessionID]int)
	if m.config.Mode == ModeOnChange {
		used = m.windowAllowance
		sessions = maps.Clone(m.windowSessions)
	}
	for _, candidate := range plan.order {
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
			plan.byteBlocked = true
			break
		}
		used.objects++
		used.bytes += bytes
		sessions[candidate.sessionID]++
		batch.snapshotAfter[candidate.class] = candidate.sequence
		plan.charges = append(plan.charges, snapshotCharge{candidate, bytes})
		delete(pending, candidate.sub)
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

// 一次 Push 尝试消费该会话本批冷创建的额度（包括 RetryLater），防止同窗口空转。
// 编码失败、会话失效、提前取消和未轮到的会话不消费额度。游标按原计划顺序只推进到
// 实际尝试的前缀：遇到仍然有效却未被尝试的会话就停下，其后即使有已尝试的会话
// （同一会话跨两个类别时可能出现）也不越过它；已失效的会话不再参与轮转，可以越过。
// 已发送的帧前缀仍由 Flush 单独结算。
func (m *Manager) commitSnapshotAttempts(work map[SessionID]*flushSession, plan *snapshotPlan) {
	if plan == nil {
		return
	}
	attempted := 0
	prefix := true
	for _, charge := range plan.charges {
		candidate := charge.candidate
		batch := work[candidate.sessionID]
		if !batch.snapshotAttempted {
			if m.session(candidate.sessionID) == batch.session {
				prefix = false
			}
			continue
		}
		attempted++
		if m.config.Mode == ModeOnChange {
			m.windowAllowance.objects++
			m.windowAllowance.bytes += charge.bytes
			m.windowSessions[candidate.sessionID]++
		}
		if prefix {
			m.snapshotCursor[candidate.class] = candidate.sessionID
			m.snapshotNextClass = 1 - candidate.class
		}
	}
	if m.config.Mode == ModeOnChange && plan.byteBlocked && attempted == len(plan.charges) {
		m.windowByteBlocked = true
	}
}
