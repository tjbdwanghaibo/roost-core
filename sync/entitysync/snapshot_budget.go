package entitysync

import (
	"slices"
	"time"
)

func (m *Manager) refreshSnapshotWindow() {
	if m.config.Mode != ModeOnChange {
		return
	}
	now := time.Now()
	if m.budgetWindow.IsZero() || now.Sub(m.budgetWindow) >= m.config.Interval {
		m.budgetWindow = now
		m.windowAllowance = snapshotAllowance{}
		clear(m.windowSessions)
		if m.windowSessions == nil {
			m.windowSessions = make(map[SessionID]int)
		}
	}
}

// SnapshotBudget 是每次 Flush 的快照准入软预算。MaxBytes 按实体更新包及
// 对象/组件头计，不含外层帧头；单包超过软预算时允许它独占一次额度以保证进度。
// 帧/组件/传输的硬上限仍然有效。增量和 remove 不受此预算影响。
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
	if limit.MaxObjects == 0 && limit.PerSessionObjects == 0 {
		return nil
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
			remaining := limit.MaxObjects - len(selected)
			if m.config.Mode == ModeOnChange {
				remaining -= m.windowAllowance.objects
			}
			count = min(count, max(0, remaining))
		}
		for offset := range count {
			selected[items[(start+offset)%len(items)].sub] = true
		}
		if limit.MaxObjects > 0 && len(selected) == limit.MaxObjects {
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
		m.markPending(entry.subjectID)
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
