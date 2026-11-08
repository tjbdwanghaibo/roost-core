package entitysync

import (
	"cmp"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
)

// ---- 超额降级（维护者 2026-11-08，可编辑地形方案 D17：“超额降级：减少同步对象，不断开”） ----
//
// 一个会话持有的对象按类别计上限：Limits.MaxObjects，配置了 Namespaces 时每个列出的命名空间各自一类
// （namespaces.go）。以前超出时关闭整个会话（object_limit）；现在会话不关，减少同步给它的对象：
//
//  1. 准入（admitWithinLimits，捕获之后、发送预算 / 快照预算与组帧之前）：按会话、按类别数
//     “已持有 − 本轮 remove + 本轮新对象”。都不超出时什么都不做（常见路径，帧逐字节不变）。超出的类里，新对象按
//     （钉住在前、Profile 优先级小的在前、实体 ID 小的在前）排序，前“空位”个照常创建，其余本轮不创建——对象要么完整
//     持有、要么完全不在客户端，不截断任何对象。没创建的订阅保持“待创建”并标为超额暂缓（subscription.overLimit），
//     通知带 Deferred 回调的订阅来源（下一次政策阶段）。remove 先于 create 的既有规则照旧：同一 tick 里先腾出的
//     位置算作空位，满额时的合法替换不会被误拒。
//  2. 已持有的对象 Manager 不替政策挤掉：它不知道距离，挤掉还会在线上产生 remove / create 往返。新旧之间的取舍由
//     政策收到通知后退订完成（Interest 收紧该玩家视野的上限、从最远的开始退订），所以对有距离概念的政策，最终可见集
//     是它按距离选出的最近 N 个。唯一例外是钉住的订阅（SubscribePinned，玩家看自己）：放不下时挤掉同类里优先级最低
//     （Profile 优先级最大、同级取实体 ID 最大）的未钉住对象，同一帧先 remove 再 create；被挤掉的订阅同样转为超额暂缓
//     并通知（DeferredSubscription.Evicted）。同类里全是钉住的对象时钉住的订阅也只能暂缓（pinned_blocked，配置错误）。
//     仍在暂缓的订阅每隔 overLimitRenotifyInterval 再通知一次：政策让出的位置可能被别的东西占了，或它退订的是还没
//     捕获的订阅而不是暂缓的那些（快照预算下常见），只通知一次就会停在“想要的比放得下的多”上；重复通知让政策继续收紧，
//     直到没有暂缓。
//  3. 暂缓的订阅不参与捕获、不进快照预算索引、不让 Drain 等待。会话的这一类出现空位时（本会话的帧准入后持有数低于
//     上限）releaseOverLimit 按同一排序放回前“空位”个，下一次 Flush 重新准入；HoldSession 从零计，全部放回。
//  4. 可观测：entitysync_over_limit_deferred_total{namespace, reason}（reason：deferred 新对象暂缓 / evicted 为钉住让位 /
//     pinned_blocked 钉住的也放不下）、entitysync_over_limit_waiting{namespace}、ManagerStats.OverLimit*、
//     CheckHealth 降级、限频 Warn（每 overLimitWarnInterval 至多一条，带被省略的次数）。暂缓的订阅都是政策持有的订阅，
//     政策不收紧也不会无界增长。
//
// 只比较本轮捕获到的新对象：被快照预算、发送预算、持久化门槛挡下、本轮没有捕获的不占位，之后到来时按当时的空位准入。

// DeferredSubscription 是一条超额暂缓的订阅（SubscriptionSourceHooks.Deferred）：会话里 Namespace 这一类对象已满，
// Subject 没有同步给 Session（Evicted 为真时是已持有的对象为钉住的订阅让位、刚被移除）。订阅保持，会话有空位时恢复。
type DeferredSubscription struct {
	Session SessionID
	Subject int64
	// Namespace 是计上限的那一类：ManagerConfig.Namespaces 里列出的命名空间名；没有列出的（Limits.MaxObjects 那一类）为空串。
	Namespace string
	Evicted   bool
}

// 超额降级的指标名。namespace 标签取类别名，没有列出的那一类是 "*"；reason 见上。
const (
	overLimitDeferredMetric = "entitysync_over_limit_deferred_total"
	overLimitWaitingMetric  = "entitysync_over_limit_waiting"
)

// overLimitRenotifyInterval 是仍在暂缓的订阅再次通知来源的间隔（只扫描有暂缓订阅的会话，每个间隔至多一遍）。
// 变量只为测试能缩短它。
var overLimitRenotifyInterval = time.Second

// overLimitWarnInterval 是超额降级 Warn 的间隔：间隔内只累计次数，下一条带上被省略的次数。
const overLimitWarnInterval = 10 * time.Second

// overLimitWarnGate 是一个 Manager 的限频闸门；published 是各类别最近一次发布的 waiting 指标值（flushGate 独占）。
type overLimitWarnGate struct {
	mu         sync.Mutex
	next       time.Time
	suppressed uint64
	published  []int64
	renotifyAt time.Time // 下一次 renotifyOverLimit 扫描的时刻（flushGate 独占）
}

func (g *overLimitWarnGate) take(now time.Time) (bool, uint64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if now.Before(g.next) {
		g.suppressed++
		return false, 0
	}
	suppressed := g.suppressed
	g.suppressed, g.next = 0, now.Add(overLimitWarnInterval)
	return true, suppressed
}

// namespaceOf 是类别的命名空间名；没有列出的那一类（类别 0）为空串。
func (m *Manager) namespaceOf(class uint8) string {
	if class == 0 || int(class) >= len(m.classes.names) {
		return ""
	}
	return m.classes.names[class]
}

func (m *Manager) namespaceLabel(class uint8) string {
	if name := m.namespaceOf(class); name != "" {
		return name
	}
	return "*"
}

func (m *Manager) overLimitWaitingTotal() int {
	total := int64(0)
	for i := range m.overLimitWaiting {
		total += m.overLimitWaiting[i].Load()
	}
	return int(total)
}

// overLimitWaitingByNamespace 是进程里所有 Manager 按命名空间标签加总的 waiting 数：一个进程常有自身视图与场景
// 两个 Manager，没列出的那一类标签都是 "*"，各写各的会互相覆盖。
var overLimitWaitingByNamespace struct {
	mu    sync.Mutex
	total map[string]int64
}

// publishOverLimitGauge 在 Flush 结束（与 Close）时把变化了的类别 waiting 数计入进程级加总并写进指标；没有变化时不写。
// 调用方持有 flushGate。
func (m *Manager) publishOverLimitGauge() {
	gate := &m.overLimitWarn
	if gate.published == nil {
		gate.published = make([]int64, len(m.overLimitWaiting))
	}
	for class := range m.overLimitWaiting {
		value := m.overLimitWaiting[class].Load()
		delta := value - gate.published[class]
		if delta == 0 {
			continue
		}
		gate.published[class] = value
		label := m.namespaceLabel(uint8(class))
		sum := &overLimitWaitingByNamespace
		sum.mu.Lock()
		if sum.total == nil {
			sum.total = make(map[string]int64)
		}
		sum.total[label] += delta
		metrics.SetGauge(overLimitWaitingMetric, metrics.Labels{"namespace": label}, sum.total[label])
		sum.mu.Unlock()
	}
}

// markOverLimitLocked 把订阅标为超额暂缓：移出快照预算索引与等待表、记进会话的暂缓索引、计数并通知来源。
// 调用方持有 subject.mu、持有 flushGate（只有 Flush 标记，releaseOverLimit 才能放心地按索引放回）。
func (m *Manager) markOverLimitLocked(subj *subject, sid SessionID, sub *subscription, evicted bool) {
	if sub.overLimit {
		return
	}
	sub.overLimit = true
	subj.profilesValid = false
	m.lanes[subj.class].snapshotRequests.update(subj, sid, sub)
	m.clearSnapshotWaitLocked(subj)
	m.mu.Lock()
	if sub.lifetime.overLimit == nil {
		sub.lifetime.overLimit = make(map[int64]*subject)
	}
	sub.lifetime.overLimit[subj.id] = subj
	m.mu.Unlock()
	m.overLimitWaiting[subj.class].Add(1)
	sub.overLimitNoticed = overLimitClock()
	m.queueDeferralsLocked(sub, DeferredSubscription{Session: sid, Subject: subj.id, Namespace: m.namespaceOf(subj.class), Evicted: evicted})
}

// overLimitEpoch 是 overLimitClock 的起点；time.Since 用单调时钟，不受墙钟调整影响。
var overLimitEpoch = time.Now()

// overLimitClock 返回单调时钟上的纳秒数，用于 subscription.overLimitNoticed。
func overLimitClock() int64 { return int64(time.Since(overLimitEpoch)) }

// renotifyOverLimit 在 Flush 的政策阶段之后调用：有暂缓的订阅、且到了扫描时刻时，为暂缓超过 overLimitRenotifyInterval 的订阅
// 再通知一次来源（见文件开头第 2 条）。调用方持有 flushGate。
func (m *Manager) renotifyOverLimit() {
	if m.overLimitWaitingTotal() == 0 {
		return // 常态：不取时钟
	}
	gate := &m.overLimitWarn
	now := time.Now()
	if now.Before(gate.renotifyAt) {
		return
	}
	gate.renotifyAt = now.Add(overLimitRenotifyInterval)
	clock := overLimitClock()
	type waiting struct {
		sid  SessionID
		life *sessionLifetime
		subj *subject
	}
	var items []waiting
	m.mu.RLock()
	for sid, sess := range m.sessions {
		for _, subj := range sess.lifetime.overLimit {
			items = append(items, waiting{sid, sess.lifetime, subj})
		}
	}
	m.mu.RUnlock()
	slices.SortFunc(items, func(a, b waiting) int {
		if order := cmp.Compare(a.sid, b.sid); order != 0 {
			return order
		}
		return cmp.Compare(a.subj.id, b.subj.id)
	})
	for _, item := range items {
		item.subj.mu.Lock()
		if sub := item.subj.subscribers[item.sid]; sub != nil && sub.lifetime == item.life && sub.waitsForRoom() && time.Duration(clock-sub.overLimitNoticed) >= overLimitRenotifyInterval {
			sub.overLimitNoticed = clock
			m.queueDeferralsLocked(sub, DeferredSubscription{Session: item.sid, Subject: item.subj.id, Namespace: m.namespaceOf(item.subj.class)})
		}
		item.subj.mu.Unlock()
	}
}

// unmarkOverLimitLocked 清掉超额暂缓标记并放回快照预算索引；会话暂缓索引里的项由调用方删除或留给 releaseOverLimit
// 顺手清理（使用时复核）。调用方持有 subject.mu。
func (m *Manager) unmarkOverLimitLocked(subj *subject, sid SessionID, sub *subscription) {
	if !sub.overLimit {
		return
	}
	sub.overLimit = false
	m.overLimitWaiting[subj.class].Add(-1)
	subj.profilesValid = false
	m.lanes[subj.class].snapshotRequests.update(subj, sid, sub)
}

// overLimitRank 是超额时的排序键：钉住在前，Profile 优先级小的在前，实体 ID 小的在前。准入取前面的，让位取最后的。
type overLimitRank struct {
	pinned   bool
	priority int
	subject  int64
}

func compareOverLimitRank(a, b overLimitRank) int {
	if a.pinned != b.pinned {
		if a.pinned {
			return -1
		}
		return 1
	}
	if order := cmp.Compare(a.priority, b.priority); order != 0 {
		return order
	}
	return cmp.Compare(a.subject, b.subject)
}

// admitWithinLimits 是 Flush 的超额准入：捕获之后、发送预算 / 快照预算与组帧之前，让每个会话本批的新对象不超出
// 各类的持有上限（见本文件开头）。会话按 ID 顺序处理，结果与通知顺序确定。调用方持有 flushGate。
func (m *Manager) admitWithinLimits(work map[SessionID]*flushSession) {
	var over []SessionID
	for sid, batch := range work {
		if m.batchOverLimit(batch) {
			over = append(over, sid)
		}
	}
	if len(over) == 0 {
		return
	}
	slices.Sort(over)
	for _, sid := range over {
		m.degradeBatch(sid, work[sid])
	}
}

// batchOverLimit 报告本批是否有某一类会超出上限。没有新对象（常见）时一次遍历就返回，不分配。
func (m *Manager) batchOverLimit(batch *flushSession) bool {
	sess := batch.session
	fresh := false
	for i := range batch.entries {
		if entry := &batch.entries[i]; entry.kind == entryCreate {
			if _, held := sess.objects[entry.subjectID]; !held {
				fresh = true
				break
			}
		}
	}
	if !fresh {
		return false
	}
	var buf [8]int
	delta := buf[:0]
	if classes := len(m.overLimitWaiting); classes <= len(buf) {
		delta = buf[:classes]
	} else {
		delta = make([]int, classes)
	}
	for i := range batch.entries {
		entry := &batch.entries[i]
		_, held := sess.objects[entry.subjectID]
		switch {
		case entry.kind == entryCreate && !held:
			delta[entry.class]++
		case entry.kind == entryRemove && held:
			delta[entry.class]--
		}
	}
	for class, d := range delta {
		if d > 0 && sess.heldIn(uint8(class), m.wire)+d > m.wire.classLimit(uint8(class)) {
			return true
		}
	}
	return false
}

// degradeBatch 处理一个超额的会话本批：每个超出的类里按 overLimitRank 留下前“空位”个新对象，钉住的放不下时先让
// 已持有的低优先级对象让位，其余暂缓。被暂缓的项移出本批（entries / settlements 成对）。
func (m *Manager) degradeBatch(sid SessionID, batch *flushSession) {
	sess := batch.session
	classes := len(m.overLimitWaiting)
	creates := make([][]int, classes) // 每类新对象在本批里的下标
	removes := make([]int, classes)   // 每类本批要 remove 的已持有对象数
	removing := make(map[int64]bool)  // 本批 remove 的已持有对象，不能再被选来让位
	for i := range batch.entries {
		entry := &batch.entries[i]
		_, held := sess.objects[entry.subjectID]
		switch {
		case entry.kind == entryCreate && !held:
			creates[entry.class] = append(creates[entry.class], i)
		case entry.kind == entryRemove && held:
			removes[entry.class]++
			removing[entry.subjectID] = true
		}
	}
	dropped := false
	for class := range creates {
		c := uint8(class)
		limit, held := m.wire.classLimit(c), sess.heldIn(c, m.wire)
		room := max(0, limit-(held-removes[class]))
		if len(creates[class]) <= room {
			continue
		}
		ranked := m.rankCreates(batch, creates[class])
		pinned := 0
		for _, item := range ranked {
			if item.rank.pinned {
				pinned++
			}
		}
		evicted := 0
		if pinned > room {
			evicted = m.evictForPinned(sid, batch, c, pinned-room, removing)
			room += evicted
		}
		deferred, blocked := 0, 0
		for _, item := range ranked[min(room, len(ranked)):] {
			if item.rank.pinned {
				blocked++
			} else {
				deferred++
			}
			m.deferCreate(sid, batch, item.index)
			dropped = true
		}
		m.noteOverLimit(sid, c, held, limit, deferred, evicted, blocked)
	}
	if dropped {
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

type rankedCreate struct {
	index int
	rank  overLimitRank
}

// rankCreates 按 overLimitRank 排列一类的新对象。钉住要在 subject.mu 下读订阅；优先级取本轮捕获的 Profile。
func (m *Manager) rankCreates(batch *flushSession, indexes []int) []rankedCreate {
	ranked := make([]rankedCreate, 0, len(indexes))
	for _, index := range indexes {
		entry, settle := &batch.entries[index], &batch.settlements[index]
		settle.subj.mu.Lock()
		pinned := settle.sub.pinned()
		settle.subj.mu.Unlock()
		ranked = append(ranked, rankedCreate{index: index, rank: overLimitRank{
			pinned: pinned, priority: m.profilePriority(entry.update.update.Profile.Normalize()), subject: entry.subjectID,
		}})
	}
	slices.SortFunc(ranked, func(a, b rankedCreate) int { return compareOverLimitRank(a.rank, b.rank) })
	return ranked
}

// deferCreate 把本批的一个新对象移出（kind 置 0，由 degradeBatch 压实），订阅仍是本轮捕获的那一个时标为超额暂缓。
func (m *Manager) deferCreate(sid SessionID, batch *flushSession, index int) {
	batch.entries[index].kind = 0
	settle := batch.settlements[index]
	subj, sub := settle.subj, settle.sub
	subj.mu.Lock()
	sub.inFlight = false
	if subj.subscribers[sid] == sub && sub.revision == settle.revision && sub.kind == kindSnapshot && sub.lifetime == batch.session.lifetime {
		m.markOverLimitLocked(subj, sid, sub, false)
	}
	subj.mu.Unlock()
}

// evictForPinned 为放不下的钉住订阅在 class 里挑至多 need 个已持有、未钉住、本批不 remove 的对象让位：按
// overLimitRank 取最后的（优先级最低）。被选中的对象本批改发 remove（原有的 update / 全量项就地改写），结算时订阅
// 转为超额暂缓（settlement.evict）。返回让出的位置数。
func (m *Manager) evictForPinned(sid SessionID, batch *flushSession, class uint8, need int, removing map[int64]bool) int {
	sess := batch.session
	m.mu.RLock()
	var held []*subject
	for id := range sess.objects {
		if subj := sess.lifetime.subjects[id]; subj != nil && subj.class == class && !removing[id] {
			held = append(held, subj)
		}
	}
	m.mu.RUnlock()
	type victim struct {
		subj *subject
		sub  *subscription
		rank overLimitRank
	}
	victims := make([]victim, 0, len(held))
	for _, subj := range held {
		subj.mu.Lock()
		if sub := subj.subscribers[sid]; sub != nil && sub.lifetime == sess.lifetime && !sub.pinned() && sub.kind != kindLeaving {
			victims = append(victims, victim{subj, sub, overLimitRank{priority: m.profilePriority(sub.profile), subject: subj.id}})
		}
		subj.mu.Unlock()
	}
	slices.SortFunc(victims, func(a, b victim) int { return compareOverLimitRank(b.rank, a.rank) })
	evicted := 0
	for _, v := range victims[:min(need, len(victims))] {
		index := slices.IndexFunc(batch.entries, func(entry frameEntry) bool { return entry.subjectID == v.subj.id && entry.kind != 0 })
		v.subj.mu.Lock()
		if v.subj.subscribers[sid] != v.sub || v.sub.pinned() || v.sub.kind == kindLeaving {
			v.subj.mu.Unlock()
			continue
		}
		if index >= 0 {
			// 本批已有它的 update / 全量：同一个在途结算改成让位，内容不发。
			batch.entries[index].kind, batch.entries[index].update = entryRemove, nil
			batch.settlements[index].evict = true
		} else {
			settle := captureSettlement(v.subj, v.sub, 0, false)
			settle.evict = true
			batch.entries = append(batch.entries, frameEntry{subjectID: v.subj.id, kind: entryRemove, class: class})
			batch.settlements = append(batch.settlements, settle)
		}
		v.subj.mu.Unlock()
		removing[v.subj.id] = true
		evicted++
	}
	return evicted
}

// noteOverLimit 记一次超额降级：计数、指标与限频 Warn。
func (m *Manager) noteOverLimit(sid SessionID, class uint8, held, limit, deferred, evicted, blocked int) {
	label := m.namespaceLabel(class)
	for _, item := range []struct {
		reason string
		count  int
	}{{"deferred", deferred}, {"evicted", evicted}, {"pinned_blocked", blocked}} {
		if item.count > 0 {
			metrics.IncCounter(overLimitDeferredMetric, metrics.Labels{"namespace": label, "reason": item.reason}, int64(item.count))
		}
	}
	m.overLimitDeferred.Add(uint64(deferred + blocked))
	m.overLimitEvicted.Add(uint64(evicted))
	if allowed, suppressed := m.overLimitWarn.take(time.Now()); allowed {
		slog.Warn("entitysync: a session is over its object limit; the extra objects are not synced to it until there is room (the session stays open). "+
			"Keep the policy limits (AOIConfig.MaxVisible etc.) within the session limit",
			"manager", m.String(), "session", sid, "namespace", label, "limit", limit, "held", held,
			"deferred", deferred, "evicted", evicted, "pinned_blocked", blocked, "suppressed_since_last_warn", suppressed)
	}
}

// releaseOverLimit 在一个会话的帧全部准入、结算之后调用（sess 是最后采纳的会话状态）：这一次打开里有超额暂缓的
// 订阅、且其所在类别此刻有空位时，按 overLimitRank 放回前“空位”个（清标记、排进 pending），下一次 Flush 重新准入；
// 顺手删掉索引里已失效的项。调用方持有 flushGate。
func (m *Manager) releaseOverLimit(sid SessionID, sess *session) {
	life := sess.lifetime
	m.mu.RLock()
	if len(life.overLimit) == 0 {
		m.mu.RUnlock()
		return
	}
	room := make([]int, len(m.overLimitWaiting))
	open := false
	for class := range room {
		room[class] = m.wire.classLimit(uint8(class)) - sess.heldIn(uint8(class), m.wire)
		open = open || room[class] > 0
	}
	var waiting []*subject
	if open {
		for _, subj := range life.overLimit {
			if room[subj.class] > 0 {
				waiting = append(waiting, subj)
			}
		}
	}
	m.mu.RUnlock()
	if len(waiting) == 0 {
		return
	}
	type candidate struct {
		subj *subject
		sub  *subscription
		rank overLimitRank
	}
	var candidates []candidate
	var settled []*subject // 从会话暂缓索引删除的项：已失效或已放回
	for _, subj := range waiting {
		subj.mu.Lock()
		if sub := subj.subscribers[sid]; sub != nil && sub.lifetime == life && sub.waitsForRoom() {
			candidates = append(candidates, candidate{subj, sub, overLimitRank{pinned: sub.pinned(), priority: m.profilePriority(sub.profile), subject: subj.id}})
		} else {
			settled = append(settled, subj)
		}
		subj.mu.Unlock()
	}
	slices.SortFunc(candidates, func(a, b candidate) int {
		if order := cmp.Compare(a.subj.class, b.subj.class); order != 0 {
			return order
		}
		return compareOverLimitRank(a.rank, b.rank)
	})
	released := 0
	for _, c := range candidates {
		if room[c.subj.class] <= 0 {
			continue
		}
		c.subj.mu.Lock()
		if c.subj.subscribers[sid] == c.sub && c.sub.waitsForRoom() {
			m.unmarkOverLimitLocked(c.subj, sid, c.sub)
			m.markPending(c.subj.id)
			room[c.subj.class]--
			settled = append(settled, c.subj)
			released++
		}
		c.subj.mu.Unlock()
	}
	if len(settled) > 0 {
		m.mu.Lock()
		// 上面在 subject.mu 下判定的结果放开锁之后仍然成立：暂缓标记只在 Flush（本调用方）里设置。
		for _, subj := range settled {
			if life.overLimit[subj.id] == subj {
				delete(life.overLimit, subj.id)
			}
		}
		m.mu.Unlock()
	}
	if released > 0 {
		m.WakeSync()
	}
}
