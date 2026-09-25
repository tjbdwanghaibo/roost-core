package entitysync

import (
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
		clear(m.windowSessions)
		if m.windowSessions == nil {
			m.windowSessions = make(map[SessionID]int)
		}
		m.pendingMu.Lock()
		for id := range m.waitingSnapshots {
			m.pending[id] = struct{}{}
		}
		// 等待年龄在实际没有欠快照时才清除，不能在窗口轮转时重置。
		m.pendingMu.Unlock()
	}
}

type snapshotWait struct {
	subject *subject
	since   time.Time
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
		m.waitingSnapshots[subj.id] = snapshotWait{subject: subj, since: time.Now()}
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
		delete(m.waitingSnapshots, subj.id)
	}
	m.pendingMu.Unlock()
}

// SnapshotBudget 是快照准入软预算：periodic 每次 Flush 重置，on_change 在
// 同一个 Interval 窗口共享额度。MaxBytes 按实体更新包及
// 对象/组件头计，不含外层帧头；单包超过软预算时允许它独占一次额度以保证进度。
// 只限制客户端尚未持有对象的创建。现有对象的全量替换、增量和 remove 不占额度；
// 帧/组件/传输的硬上限仍然有效，预算不能替代总带宽或队列背压。
type SnapshotBudget struct {
	MaxObjects        int
	MaxBytes          int
	PerSessionObjects int
}

func (b SnapshotBudget) enabled() bool {
	return b.MaxObjects > 0 || b.MaxBytes > 0 || b.PerSessionObjects > 0
}

type snapshotAllowance struct{ objects, bytes int }

type snapshotCandidate struct {
	subjectID int64
	sub       *subscription
}

// 数量预算可在 packer 执行前决定。先按同一轮转顺序选出本轮有额度的订阅，
// 避免恢复期间为后面几轮的快照反复打包；字节额度需知道实际包长，仍在准入前检查。
// 选择按订阅指针匹配，期间重开/新建的订阅留到下一轮，不复活旧订阅。
func (m *Manager) planSnapshotCaptures(ids []int64) map[*subscription]bool {
	limit := m.config.SnapshotBudget
	if m.config.Mode == ModeOnChange && limit.MaxBytes > 0 && m.windowAllowance.objects > 0 && m.windowAllowance.bytes >= limit.MaxBytes {
		return map[*subscription]bool{}
	}
	if limit.MaxObjects == 0 && limit.PerSessionObjects == 0 {
		return nil
	}
	remainingObjects := limit.MaxObjects
	if remainingObjects > 0 && m.config.Mode == ModeOnChange {
		remainingObjects = max(0, remainingObjects-m.windowAllowance.objects)
		if remainingObjects == 0 {
			// nil 表示不限制；空集合表示本窗口没有快照额度。
			return map[*subscription]bool{}
		}
	}
	candidates := make(map[SessionID][]snapshotCandidate)
	sessions := make(map[SessionID]*session)
	for _, id := range ids {
		subj := m.subject(id)
		if subj == nil {
			continue
		}
		subj.mu.Lock()
		for sid, sub := range subj.subscribers {
			if sub.kind != kindSnapshot {
				continue
			}
			sess, found := sessions[sid]
			if !found {
				sess = m.session(sid)
				sessions[sid] = sess
			}
			if sess == nil || sess.held || sess.lifetime != sub.lifetime {
				continue
			}
			if _, exists := sess.objects[id]; exists {
				// 现有对象全量替换由 Flush 选择，不挤占新对象的恢复额度。
				continue
			}
			candidates[sid] = append(candidates[sid], snapshotCandidate{subjectID: id, sub: sub})
		}
		subj.mu.Unlock()
	}
	order := make([]SessionID, 0, len(candidates))
	for sid := range candidates {
		order = append(order, sid)
	}
	slices.Sort(order)
	m.rotateSnapshotSessions(order)
	selected := make(map[*subscription]bool)
	for _, sid := range order {
		items := candidates[sid]
		start := 0
		for start < len(items) && items[start].subjectID <= sessions[sid].snapshotAfter {
			start++
		}
		count := len(items)
		if limit.PerSessionObjects > 0 {
			remaining := limit.PerSessionObjects
			if m.config.Mode == ModeOnChange {
				remaining -= m.windowSessions[sid]
			}
			count = min(count, max(0, remaining))
		}
		if limit.MaxObjects > 0 {
			remaining := remainingObjects - len(selected)
			count = min(count, max(0, remaining))
		}
		for offset := range count {
			selected[items[(start+offset)%len(items)].sub] = true
		}
		if limit.MaxObjects > 0 && len(selected) == remainingObjects {
			break
		}
	}
	return selected
}

func (m *Manager) rotateSnapshotSessions(sessions []SessionID) {
	if m.config.SnapshotBudget.enabled() {
		start := 0
		for start < len(sessions) && sessions[start] <= m.snapshotCursor {
			start++
		}
		if start < len(sessions) {
			slices.Reverse(sessions[:start])
			slices.Reverse(sessions[start:])
			slices.Reverse(sessions)
		}
	}
}

func (m *Manager) scheduleSnapshots(sid SessionID, batch *flushSession, used *snapshotAllowance) {
	batch.snapshotAfter = batch.session.snapshotAfter
	limit := m.config.SnapshotBudget
	if !limit.enabled() {
		return
	}
	// entries 与 settlements 在捕获阶段一一对应；编码排序在此之后进行。
	// 从上次实体之后开始挑选，避免不断出现的小 ID 快照饿死老请求。
	start := 0
	for start < len(batch.entries) && batch.entries[start].subjectID <= batch.snapshotAfter {
		start++
	}
	selected := 0
	if m.config.Mode == ModeOnChange {
		selected = m.windowSessions[sid]
	}
	for offset := range len(batch.entries) {
		i := (start + offset) % len(batch.entries)
		entry := &batch.entries[i]
		if entry.kind != entryCreate {
			continue
		}
		if _, exists := batch.session.objects[entry.subjectID]; exists {
			// entryCreate 表示需要 Full 内容；线上也可能是 ObjectUpdate。
			continue
		}
		data, err := entry.update.encode(m.wire.limits.MaxComponentBytes)
		if err != nil {
			continue
		} // 交给编码的统一失败路径处理
		bytes := 18 + len(data)
		allowed := (limit.MaxObjects == 0 || used.objects < limit.MaxObjects) &&
			(limit.PerSessionObjects == 0 || selected < limit.PerSessionObjects) &&
			(limit.MaxBytes == 0 || used.objects == 0 || bytes <= limit.MaxBytes-used.bytes)
		if allowed {
			selected++
			if m.config.Mode == ModeOnChange {
				m.windowSessions[sid] = selected
			}
			used.objects++
			used.bytes += bytes
			batch.snapshotAfter = entry.subjectID
			m.snapshotCursor = sid
			continue
		}
		settle := &batch.settlements[i]
		settle.subj.mu.Lock()
		settle.sub.inFlight = false
		settle.subj.mu.Unlock()
		m.deferSnapshot(settle.subj)
		m.snapshotsDeferred.Add(1)
		entry.kind = 0 // 仅清除本 tick 捕获；订阅仍然等待快照
	}
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
