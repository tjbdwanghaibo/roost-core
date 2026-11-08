package entitysync

import (
	"cmp"
	"slices"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
)

// SendBudget 限制一个连接在 Interval 内尝试发送的状态字节。MaxBytes 是软值：
// 每窗口首个完整 subject 可超额，remove 始终放行，帧头也可能令实际字节略超预算。
// MaxWait 后按最早等待者优先，不是交付期限保证。Priority 越小越先发，与授权 Profile 无关。
type SendBudget struct {
	MaxBytes int
	MaxWait  time.Duration
	Priority map[entity.SyncProfile]int
}

func (b SendBudget) normalized() SendBudget {
	if b.MaxWait <= 0 {
		b.MaxWait = 250 * time.Millisecond
	}
	priorities := make(map[entity.SyncProfile]int, len(b.Priority))
	for profile, priority := range b.Priority {
		profile = profile.Normalize()
		if previous, exists := priorities[profile]; !exists || priority < previous {
			priorities[profile] = priority
		}
	}
	b.Priority = priorities
	return b
}

// scheduleSends 不保留旧 delta。全局内容提交后，未交付的订阅改为欠全量，
// 下一窗口从当前状态重建基线。entries/settlements 成对移动，inFlight 由此处释放被挡项。
func (m *Manager) scheduleSends(work map[SessionID]*flushSession, now time.Time) {
	limit := m.config.SendBudget
	if limit.MaxBytes <= 0 {
		return
	}
	for sid, batch := range work {
		life := batch.session.lifetime
		// on_change 与等待快照的唤醒窗口对齐，不能因几微秒捕获差异错过整轮额度。
		window := now
		if m.config.Mode == ModeOnChange {
			window = m.budgetWindow
		}
		if life.sendWindow.IsZero() {
			life.sendWindow, life.sendBytes = window, 0
		} else if elapsed := window.Sub(life.sendWindow); elapsed >= m.config.Interval {
			life.sendWindow = life.sendWindow.Add(elapsed / m.config.Interval * m.config.Interval)
			life.sendBytes = 0
		}
		type candidate struct {
			entry    frameEntry
			settle   settlement
			since    time.Time
			priority int
		}
		items := make([]candidate, 0, len(batch.entries))
		for i, entry := range batch.entries {
			settle := batch.settlements[i]
			settle.subj.mu.Lock()
			if settle.sub.sendWaiting.IsZero() {
				settle.sub.sendWaiting = now
			}
			since := settle.sub.sendWaiting
			settle.subj.mu.Unlock()
			priority := 0
			if entry.update != nil {
				priority = limit.Priority[entry.update.update.Profile.Normalize()]
			}
			items = append(items, candidate{entry, settle, since, priority})
		}
		// 让位的 remove（settle.evict，over_limit.go）与普通 remove 一样先排、不限：它腾出的位置本批已经算给钉住的对象。
		slices.SortStableFunc(items, func(a, b candidate) int {
			if removeA, removeB := a.settle.remove || a.settle.evict, b.settle.remove || b.settle.evict; removeA != removeB {
				if removeA {
					return -1
				}
				return 1
			}
			oldA, oldB := now.Sub(a.since) >= limit.MaxWait, now.Sub(b.since) >= limit.MaxWait
			if oldA != oldB {
				if oldA {
					return -1
				}
				return 1
			}
			if oldA {
				if order := a.since.Compare(b.since); order != 0 {
					return order
				}
			}
			return cmp.Compare(a.priority, b.priority)
		})
		used, count, blocked := life.sendBytes, 0, false
		for _, item := range items {
			bytes := 0
			if item.entry.update != nil {
				bytes = snapshotObjectOverhead + subjectUpdateBytes(item.entry.update.update)
			}
			if !item.settle.remove && !item.settle.evict && (blocked || (used > 0 && bytes > limit.MaxBytes-used)) {
				blocked = true // 不让后面小包反复越过首个被挡对象。
				settle := item.settle
				settle.subj.mu.Lock()
				settle.sub.inFlight = false
				if sub := settle.subj.subscribers[sid]; sub == settle.sub && sub.revision == settle.revision && sub.kind != kindLeaving {
					m.changeSubscriptionKindLocked(settle.subj, sid, sub, kindSnapshot)
				}
				settle.subj.mu.Unlock()
				m.deferSnapshot(settle.subj)
				m.sendDeferred.Add(1)
				if m.config.Trace != nil {
					m.config.Trace.Record(SyncTraceEvent{Stage: "send_budget_wait", SubjectID: settle.subj.id, Session: sid, Lifetime: life.traceID, Duration: now.Sub(item.since)})
				}
				continue
			}
			batch.entries[count], batch.settlements[count] = item.entry, item.settle
			count++
			used += bytes
		}
		clear(batch.entries[count:])
		clear(batch.settlements[count:])
		batch.entries, batch.settlements = batch.entries[:count], batch.settlements[:count]
	}
}
