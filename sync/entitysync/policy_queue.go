package entitysync

import (
	"errors"
	"maps"
	"slices"
)

type policyHook struct {
	apply   func() error
	pending func() bool
}

// RegisterPolicy 安装锁外、捕获前的兴趣事实处理器。处理器不得调用 Flush，
// 不得读取未冻结的业务字段；可更新订阅。返回取消函数供政策关闭时释放。
func (m *Manager) RegisterPolicy(apply func() error, pending ...func() bool) func() {
	if apply == nil {
		return func() {}
	}
	m.policyMu.Lock()
	if m.policies == nil {
		m.policies = make(map[uint64]policyHook)
	}
	m.nextPolicy++
	id := m.nextPolicy
	hook := policyHook{apply: apply}
	if len(pending) > 0 {
		hook.pending = pending[0]
	}
	m.policies[id] = hook
	m.policyMu.Unlock()
	return func() { m.policyMu.Lock(); delete(m.policies, id); m.policyMu.Unlock() }
}
func (m *Manager) policyHooks() []policyHook {
	m.policyMu.Lock()
	defer m.policyMu.Unlock()
	hooks := make([]policyHook, 0, len(m.policies))
	for _, id := range slices.Sorted(maps.Keys(m.policies)) {
		hooks = append(hooks, m.policies[id])
	}
	return hooks
}
func (m *Manager) applyPolicies() error {
	m.deliverResubmits()
	var err error
	for _, hook := range m.policyHooks() {
		err = errors.Join(err, hook.apply())
	}
	return err
}
func (m *Manager) policiesPending() bool {
	if m.resubmitsPending() {
		return true
	}
	for _, hook := range m.policyHooks() {
		if hook.pending != nil && hook.pending() {
			return true
		}
	}
	return false
}

// ---- 框架撤销后的政策重新提交（RR-20260926-70） ----
//
// RR-59 卸载后重载不了时，RetractUnloadedSubject 退回 remove 并在全部 remove 交付后忘掉 subject。订阅是政策
// 说的，框架替政策撤销了它，政策自己并不知道：Interest 仍记这对 pair 已订阅，实体之后重新登记时谁也不再说一遍。
// 所以撤销时记下每条订阅属于哪个来源、哪个会话；同 ID 重新登记（新 subject）时把这些记录交还给来源所属的政策，
// 在下一次 Flush 的政策阶段（捕获前、不持有 Manager 的锁，与 RegisterPolicy 的处理器同一位置）调用来源的
// resubmit。政策按自己的判定重新提交：仍持有的 pair 重新订阅（可见性、视图白名单、优先级照旧由政策与 Manager
// 校验），已释放的不恢复。与 SessionLost → Interest.Resubscribe 是同一种“Manager 忘了、政策没忘”的修补。
//
// 只记录经 NewSubscriptionSourceWithResubmit 创建的来源；Manager.Subscribe 的默认来源与普通 SubscriptionSource
// 没有政策可问，行为不变。每个来源独立：各自被记录、各自被交还、各自撤销（多来源订阅的既有语义）。
//
// 记录的生命周期：撤销时写入 retracted；同 ID 新 subject 登记时移入 resubmits 并唤醒同步；政策阶段交付后删除。
// 来源在此期间自己释放这对 pair（Unsubscribe 得到 ErrSubjectNotRegistered / ErrSubscriptionNotFound）时删除对应
// 记录；业务 Unregister 该 ID 时删除该 ID 的全部记录（业务的注销意图优先）；Manager.Close 清空。记录条数不超过
// 撤销时的订阅数，且随政策释放 pair 而减少。resubmitMu 是叶子锁：持有它时不取其他锁、不回调。

// RetractedSubscription 是一条被框架撤销、subject 已重新登记、交还给来源所属政策的订阅：RR-59 卸载后重载
// 不了，RetractUnloadedSubject 退回 remove 时，该来源曾让 Session 订阅 Subject。
type RetractedSubscription struct {
	Session SessionID
	Subject int64
}

type retractedSubscription struct {
	source  *SubscriptionSource
	session SessionID
}

type queuedResubmit struct {
	source       *SubscriptionSource
	subscription RetractedSubscription
}

// NewSubscriptionSourceWithResubmit 与 NewSubscriptionSource 相同，另外登记政策的重新提交入口（RR-20260926-70）：
// 本来源的订阅被框架撤销（RetractUnloadedSubject）、同 ID 之后重新登记时，Manager 在下一次 Flush 的政策阶段
// 以这些订阅调用 resubmit，每次调用只含本来源的订阅。resubmit 在 Flush 内执行，不持有 Manager 的锁，可以取
// 政策自己的锁并 Subscribe；不得调用 Flush，不得阻塞。它应按政策当前的判定决定是否重新订阅——政策已释放的
// pair 不要恢复。resubmit 为 nil 时等同 NewSubscriptionSource。
func (m *Manager) NewSubscriptionSourceWithResubmit(resubmit func([]RetractedSubscription)) *SubscriptionSource {
	return &SubscriptionSource{manager: m, resubmit: resubmit}
}

// recordRetractedLocked 在 RetractUnloadedSubject 退役之前（retireLocked 清空来源之前）记下政策来源的订阅。
// 调用方持有 subj.mu：记录必须在 forget（可能紧接着登记排队的新状态）能发生之前就位。
func (m *Manager) recordRetractedLocked(subj *subject) {
	var entries []retractedSubscription
	for sid, sub := range subj.subscribers {
		for source := range sub.sources {
			if source != nil && source.resubmit != nil {
				entries = append(entries, retractedSubscription{source: source, session: sid})
			}
		}
	}
	if len(entries) == 0 {
		return
	}
	m.resubmitMu.Lock()
	if m.retracted == nil {
		m.retracted = make(map[int64][]retractedSubscription)
	}
	m.retracted[subj.id] = append(m.retracted[subj.id], entries...)
	m.resubmitEntries.Add(int64(len(entries)))
	m.resubmitMu.Unlock()
}

// releaseRetracted 在同 ID 新 subject 登记后调用：把撤销记录移入待交付队列，并唤醒同步让政策阶段尽快运行。
func (m *Manager) releaseRetracted(subjectID int64) {
	if m.resubmitEntries.Load() == 0 {
		return
	}
	m.resubmitMu.Lock()
	entries := m.retracted[subjectID]
	delete(m.retracted, subjectID)
	for _, entry := range entries {
		m.resubmits = append(m.resubmits, queuedResubmit{source: entry.source, subscription: RetractedSubscription{Session: entry.session, Subject: subjectID}})
	}
	m.resubmitMu.Unlock()
	if len(entries) > 0 {
		m.WakeSync()
	}
}

// deliverResubmits 在 Flush 的政策阶段（持有 flushGate、不持有其他 Manager 锁）按来源分批交付，批次顺序为
// 各来源首次出现的顺序。
func (m *Manager) deliverResubmits() {
	if m.resubmitEntries.Load() == 0 {
		return
	}
	m.resubmitMu.Lock()
	queued := m.resubmits
	m.resubmits = nil
	m.resubmitEntries.Add(-int64(len(queued)))
	m.resubmitMu.Unlock()
	var order []*SubscriptionSource
	batches := make(map[*SubscriptionSource][]RetractedSubscription)
	for _, item := range queued {
		if _, seen := batches[item.source]; !seen {
			order = append(order, item.source)
		}
		batches[item.source] = append(batches[item.source], item.subscription)
	}
	for _, source := range order {
		source.resubmit(batches[source])
	}
}

func (m *Manager) resubmitsPending() bool {
	if m.resubmitEntries.Load() == 0 {
		return false
	}
	m.resubmitMu.Lock()
	defer m.resubmitMu.Unlock()
	return len(m.resubmits) > 0
}

// dropRetracted：来源自己释放了 (session, subject)，撤销记录与待交付项里属于它的那条不再交还。
func (m *Manager) dropRetracted(source *SubscriptionSource, session SessionID, subjectID int64) {
	if source == nil || source.resubmit == nil || m.resubmitEntries.Load() == 0 {
		return
	}
	m.resubmitMu.Lock()
	defer m.resubmitMu.Unlock()
	if entries, ok := m.retracted[subjectID]; ok {
		kept := slices.DeleteFunc(entries, func(entry retractedSubscription) bool {
			return entry.source == source && entry.session == session
		})
		m.resubmitEntries.Add(int64(len(kept) - len(entries)))
		if len(kept) == 0 {
			delete(m.retracted, subjectID)
		} else {
			m.retracted[subjectID] = kept
		}
	}
	before := len(m.resubmits)
	m.resubmits = slices.DeleteFunc(m.resubmits, func(item queuedResubmit) bool {
		return item.source == source && item.subscription == RetractedSubscription{Session: session, Subject: subjectID}
	})
	m.resubmitEntries.Add(int64(len(m.resubmits) - before))
}

// dropRetractedSubject：业务 Unregister 该 ID，之前的撤销记录与待交付项都不再交还。
func (m *Manager) dropRetractedSubject(subjectID int64) {
	if m.resubmitEntries.Load() == 0 {
		return
	}
	m.resubmitMu.Lock()
	defer m.resubmitMu.Unlock()
	removed := len(m.retracted[subjectID])
	delete(m.retracted, subjectID)
	before := len(m.resubmits)
	m.resubmits = slices.DeleteFunc(m.resubmits, func(item queuedResubmit) bool { return item.subscription.Subject == subjectID })
	m.resubmitEntries.Add(-int64(removed + before - len(m.resubmits)))
}

func (m *Manager) clearRetracted() {
	m.resubmitMu.Lock()
	clear(m.retracted)
	m.resubmits = nil
	m.resubmitEntries.Store(0)
	m.resubmitMu.Unlock()
}
