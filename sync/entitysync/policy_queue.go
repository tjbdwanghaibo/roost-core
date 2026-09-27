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
// 记录的生命周期：撤销时写入 retracted；同 ID 新 subject 登记时移入 resubmits 并唤醒同步；政策阶段取出交付
// （回调期间留在 delivering），回调返回后删除。交付之前同 ID 又被撤销时，尚未交付的项回到 retracted，等下一次
// 登记（RR-20260926-78）。
// 来源在此期间自己释放这对 pair（Unsubscribe 得到 ErrSubjectNotRegistered / ErrSubscriptionNotFound）时删除对应
// 记录；业务 Unregister 该 ID 时删除该 ID 的全部记录（业务的注销意图优先）；Manager.Close 清空。同一来源、同一会话、
// 同一 subject 只留一条，条数不超过政策持有、尚未释放的 pair，且随政策释放 pair 而减少。resubmitMu 是叶子锁：持有它时不取其他锁、不回调。

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
//
// 再次撤销（RR-20260926-78）：上一次撤销的记录在本 subject 登记时已交还（移入 resubmits），政策还没来得及重新
// 订阅，本 subject 就又被撤销（仍有非政策来源的订阅者使重载器调度它）。这些记录此时不在 subscribers 里；旧实现
// 只记当时存在的订阅，排队项留在 resubmits，交付时 subject 已退役或已忘掉，Group / Direct 的 Subscribe 被拒绝后
// 放弃，这对订阅再也不恢复。所以把同 ID 尚未交付的排队项移回撤销表，正在交付的（deliverResubmits 已取出、
// 回调尚未返回，政策的 Subscribe 可能落在本次退役之后而被拒绝）复制一份回来，等下一次登记再交还。同一来源、
// 同一会话只留一条（addRetractedLocked），条数仍不超过政策自己持有、尚未释放的 pair。
func (m *Manager) recordRetractedLocked(subj *subject) {
	var entries []retractedSubscription
	for sid, sub := range subj.subscribers {
		for source := range sub.sources {
			if source != nil && source.resubmit != nil {
				entries = append(entries, retractedSubscription{source: source, session: sid})
			}
		}
	}
	m.resubmitMu.Lock()
	defer m.resubmitMu.Unlock()
	moved := 0
	kept := m.resubmits[:0]
	for _, item := range m.resubmits {
		if item.subscription.Subject != subj.id {
			kept = append(kept, item)
			continue
		}
		entries = append(entries, retractedSubscription{source: item.source, session: item.subscription.Session})
		moved++
	}
	clear(m.resubmits[len(kept):])
	m.resubmits = kept
	for _, item := range m.delivering {
		if item.subscription.Subject == subj.id {
			entries = append(entries, retractedSubscription{source: item.source, session: item.subscription.Session})
		}
	}
	if len(entries) == 0 {
		return
	}
	added := m.addRetractedLocked(subj.id, entries)
	m.resubmitEntries.Add(int64(added - moved))
}

// addRetractedLocked 把 entries 并入 subjectID 的撤销记录，同一来源、同一会话只留一条，返回新增条数。调用方持有 resubmitMu。
func (m *Manager) addRetractedLocked(subjectID int64, entries []retractedSubscription) int {
	if m.retracted == nil {
		m.retracted = make(map[int64][]retractedSubscription)
	}
	current := m.retracted[subjectID]
	seen := make(map[retractedSubscription]struct{}, len(current)+len(entries))
	for _, entry := range current {
		seen[entry] = struct{}{}
	}
	added := 0
	for _, entry := range entries {
		if _, dup := seen[entry]; dup {
			continue
		}
		seen[entry] = struct{}{}
		current = append(current, entry)
		added++
	}
	if len(current) > 0 {
		m.retracted[subjectID] = current
	}
	return added
}

// releaseRetracted 在同 ID 新 subject 登记后调用：把撤销记录移入待交付队列，并唤醒同步让政策阶段尽快运行。
// 检查与移动在 subj.mu 内完成（RR-20260926-78）：登记与这里之间 subject 可能已被再次撤销（记录已并入撤销表，
// 留给下一次登记）或被业务注销（记录已清除），这时不交还——交付时 subject 已不在，政策的重新订阅只会被拒绝。
// 撤销与注销也在 subj.mu 内处理记录，所以进入 resubmits 的记录在被取出交付之前，subject 一直是活的。
func (m *Manager) releaseRetracted(subj *subject) {
	if m.resubmitEntries.Load() == 0 {
		return
	}
	subj.mu.Lock()
	if subj.retiring {
		subj.mu.Unlock()
		return
	}
	m.resubmitMu.Lock()
	entries := m.retracted[subj.id]
	delete(m.retracted, subj.id)
	for _, entry := range entries {
		m.resubmits = append(m.resubmits, queuedResubmit{source: entry.source, subscription: RetractedSubscription{Session: entry.session, Subject: subj.id}})
	}
	m.resubmitMu.Unlock()
	subj.mu.Unlock()
	if len(entries) > 0 {
		m.WakeSync()
	}
}

// deliverResubmits 在 Flush 的政策阶段（持有 flushGate、不持有其他 Manager 锁）按来源分批交付，批次顺序为
// 各来源首次出现的顺序。取出的项在回调返回前留在 delivering（仍计入 resubmitEntries）：期间同 ID 被再次撤销时
// recordRetractedLocked 把它们复制回撤销表（RR-20260926-78），来源释放或业务注销时照常删除。
func (m *Manager) deliverResubmits() {
	if m.resubmitEntries.Load() == 0 {
		return
	}
	m.resubmitMu.Lock()
	queued := m.resubmits
	m.resubmits = nil
	m.delivering = queued
	var order []*SubscriptionSource
	batches := make(map[*SubscriptionSource][]RetractedSubscription)
	for _, item := range queued {
		if _, seen := batches[item.source]; !seen {
			order = append(order, item.source)
		}
		batches[item.source] = append(batches[item.source], item.subscription)
	}
	m.resubmitMu.Unlock()
	for _, source := range order {
		source.resubmit(batches[source])
	}
	m.resubmitMu.Lock()
	m.resubmitEntries.Add(-int64(len(m.delivering)))
	m.delivering = nil
	m.resubmitMu.Unlock()
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
	released := func(item queuedResubmit) bool {
		return item.source == source && item.subscription == RetractedSubscription{Session: session, Subject: subjectID}
	}
	before := len(m.resubmits) + len(m.delivering)
	m.resubmits = slices.DeleteFunc(m.resubmits, released)
	m.delivering = slices.DeleteFunc(m.delivering, released)
	m.resubmitEntries.Add(int64(len(m.resubmits) + len(m.delivering) - before))
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
	sameSubject := func(item queuedResubmit) bool { return item.subscription.Subject == subjectID }
	before := len(m.resubmits) + len(m.delivering)
	m.resubmits = slices.DeleteFunc(m.resubmits, sameSubject)
	m.delivering = slices.DeleteFunc(m.delivering, sameSubject)
	m.resubmitEntries.Add(-int64(removed + before - len(m.resubmits) - len(m.delivering)))
}

func (m *Manager) clearRetracted() {
	m.resubmitMu.Lock()
	clear(m.retracted)
	m.resubmits = nil
	m.delivering = nil
	m.resubmitEntries.Store(0)
	m.resubmitMu.Unlock()
}
