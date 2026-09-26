package entitysync

import (
	"github.com/tjbdwanghaibo/roost-core/entity"
	"github.com/tjbdwanghaibo/roost-core/metrics"
)

// ---- sessions ----

// OpenSession admits a receiver. It is idempotent: an already open session
// stays as it is.
//
// 传输实现 SessionLifecycle 时，先由 SessionOpened 建立传输资源，成功后才发布会话；
// 在此之前 Subscribe/Flush 看不到它，不会向未注册的传输 Push 或报告 SessionLost。
// 返回 nil 表示会话此刻已打开；返回错误表示本次调用没有创建会话。可重试的错误：
//   - ErrSessionClosing：同 ID 上一个 lifetime 的旧发送仍在退出（如 CloseSession 或
//     传输失败后立即重连）。旧发送结束后重试，或为新连接分配新 SessionID。
//   - ErrSessionOpening：同 ID 的另一次 OpenSession 正在等待传输确认。重试即可得到
//     与那次结果一致的状态。
//
// 本方法不等待旧发送退出，也不等待另一次打开完成，可在快池内调用；需要等待的重试
// 由调用方安排在慢阶段或下一次调度。
func (m *Manager) OpenSession(id SessionID) error {
	if m == nil {
		return ErrManagerClosed
	}
	if id == 0 {
		return ErrSessionInvalid
	}
	lifecycle, managed := m.config.Transport.(SessionLifecycle)
	m.mu.Lock()
	if m.closed || m.closing {
		m.mu.Unlock()
		return ErrManagerClosed
	}
	if _, exists := m.sessions[id]; exists {
		m.mu.Unlock()
		return nil
	}
	if _, busy := m.opening[id]; busy {
		m.mu.Unlock()
		return ErrSessionOpening
	}
	if len(m.sessions)+len(m.opening) >= m.config.MaxSessions {
		m.mu.Unlock()
		return ErrSessionLimit
	}
	if !managed {
		m.publishSessionLocked(id)
		m.mu.Unlock()
		return nil
	}
	m.opening[id] = struct{}{}
	m.mu.Unlock()

	err := lifecycle.SessionOpened(id)
	m.mu.Lock()
	delete(m.opening, id)
	if err != nil {
		m.mu.Unlock()
		return err
	}
	if m.closed || m.closing {
		// 传输确认期间 Manager 开始关闭：Close 不会再看到这个 ID，由这里归还传输资源。
		m.mu.Unlock()
		lifecycle.SessionClosed(id)
		return ErrManagerClosed
	}
	m.publishSessionLocked(id)
	m.mu.Unlock()
	return nil
}

// publishSessionLocked 让新 lifetime 对订阅与 Flush 可见；调用方持有 m.mu。
func (m *Manager) publishSessionLocked(id SessionID) {
	opened := newSession(id)
	opened.lifetime.traceID = m.nextLifetime.Add(1)
	m.sessions[id] = opened
}

// OpenHeldSession admits a receiver that is not ready to receive yet: it can
// be subscribed, but no frame is encoded for it until ReadySession. A client
// that installs its decoder only after the login answer wants this — the
// first snapshot must not race the decoder (ARCH-10, "session ready").
func (m *Manager) OpenHeldSession(id SessionID) error {
	if err := m.OpenSession(id); err != nil {
		return err
	}
	return m.HoldSession(id)
}

// HoldSession stops frames to an open session and starts it over: every
// subscription it holds goes back to "needs a snapshot" and the next frame it
// gets (after ReadySession) opens a new epoch with a FrameFull. Use it when
// the receiver has lost its state — a reconnect, a client-side reset.
func (m *Manager) HoldSession(id SessionID) error {
	if m == nil {
		return ErrManagerClosed
	}
	m.mu.Lock()
	sess, ok := m.sessions[id]
	if !ok {
		m.mu.Unlock()
		return ErrSessionUnknown
	}
	fresh := newSession(id)
	fresh.lifetime = sess.lifetime
	fresh.epoch = sess.epoch + 1
	fresh.framesSent = sess.framesSent
	fresh.held = true
	m.sessions[id] = fresh
	if !sess.held {
		m.heldSessions++
	}
	if m.config.Trace != nil {
		m.config.Trace.Record(SyncTraceEvent{Stage: "session_reset", Session: id, Lifetime: fresh.lifetime.traceID, Epoch: fresh.epoch})
	}
	subjects := sessionSubjectsLocked(sess)
	m.mu.Unlock()
	for _, subj := range subjects {
		subj.mu.Lock()
		if sub, subscribed := subj.subscribers[id]; subscribed && sub.lifetime == sess.lifetime {
			switch sub.kind {
			case kindLeaving:
				// It was owed a remove; with its state gone there is nothing to remove.
				m.removeSubscriptionLocked(subj, id)
			default:
				sub.baseVersion = 0
				sub.snapshotClass = snapshotRecovery
				m.changeSubscriptionKindLocked(subj, id, sub, kindSnapshot)
				sub.revision++
				subj.profilesValid = false
				m.traceSnapshotRequest(subj, fresh)
			}
		}
		m.clearSnapshotWaitLocked(subj)
		subj.mu.Unlock()
	}
	return nil
}

// ReadySession lets frames flow to a held session. Every subject it is
// subscribed to is scheduled so the snapshots go out on the next tick.
func (m *Manager) ReadySession(id SessionID) error {
	if m == nil {
		return ErrManagerClosed
	}
	m.mu.Lock()
	sess, ok := m.sessions[id]
	if !ok {
		m.mu.Unlock()
		return ErrSessionUnknown
	}
	if !sess.held {
		m.mu.Unlock()
		return nil
	}
	next := sess.clone()
	next.held = false
	m.sessions[id] = next
	m.heldSessions--
	subjects := sessionSubjectsLocked(sess)
	m.mu.Unlock()
	for _, subj := range subjects {
		// 并发退订最多产生一次空检查；并发新增由 Subscribe 自行调度。
		m.markPending(subj.id)
	}
	m.WakeSync()
	return nil
}

// CloseSession forgets a receiver: its frame state goes, and every subject
// drops its subscription without owing it a remove — there is nobody to send
// one to. The policy that subscribed it is expected to have forgotten it
// already; this is the safety net for the entries it missed.
func (m *Manager) CloseSession(id SessionID) {
	m.dropSession(id, nil, nil, false)
}

// loseSession is CloseSession for a session the transport refused: counted,
// and reported to the policy through SessionLost.
func (m *Manager) loseSession(expected *session, cause error) {
	m.dropSession(expected.id, expected, cause, true)
}

func (m *Manager) dropSession(id SessionID, expected *session, cause error, lost bool) {
	if m == nil || id == 0 {
		return
	}
	m.mu.Lock()
	removed, existed := m.sessions[id]
	if !existed {
		m.mu.Unlock()
		return
	}
	if expected != nil && removed != expected {
		m.mu.Unlock()
		return
	}
	delete(m.sessions, id)
	if removed.held {
		m.heldSessions--
	}
	subjects := sessionSubjectsLocked(removed)
	m.mu.Unlock()
	if lifecycle, ok := m.config.Transport.(SessionLifecycle); ok {
		lifecycle.SessionClosed(id)
	}
	for _, subj := range subjects {
		subj.mu.Lock()
		if sub, subscribed := subj.subscribers[id]; subscribed && sub.lifetime == removed.lifetime {
			m.removeSubscriptionLocked(subj, id)
			if subj.retiring && len(subj.subscribers) == 0 {
				// Its last subscriber left before the remove could go out.
				defer m.forget(subj.id)
			}
		}
		subj.mu.Unlock()
	}
	if lost {
		m.sessionsLost.Add(1)
		metrics.IncCounter("entitysync_sessions_lost_total", nil, 1)
		if m.config.SessionLost != nil {
			m.config.SessionLost(id, cause)
		}
	}
}

// adoptSession 只采纳仍属于同一会话状态的交付，Hold/重开不能被旧 Push 覆盖。
func (m *Manager) adoptSession(expected, next *session) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.sessions[next.id] != expected {
		return false
	}
	m.sessions[next.id] = next
	return true
}

func (m *Manager) session(id SessionID) *session {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.sessions[id]
}

// sessionHoldsSubject 以成功准入后采纳的引用表判断客户端是否持有对象。
// kindSnapshot 只表示下一次要发全量，不能证明旧视图从未交付。
func (m *Manager) sessionHoldsSubject(id SessionID, subjectID int64) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	sess := m.sessions[id]
	if sess == nil || sess.held {
		return false
	}
	_, held := sess.objects[subjectID]
	return held
}

// ---- subscriptions ----

// SubscriptionSource 表示一个独立的订阅来源。重复订阅替换本来源的 profile，
// 不增加计数；释放只影响本来源。来源令牌必须复用，不能每次调用都新建。
// 它只能合并业务已授权的视图，不承担权限校验。
type SubscriptionSource struct{ manager *Manager }

// NewSubscriptionSource 为一个独立所有者创建可复用的来源令牌。
func (m *Manager) NewSubscriptionSource() *SubscriptionSource {
	return &SubscriptionSource{manager: m}
}

func (s *SubscriptionSource) Subscribe(session SessionID, subjectID int64, profile entity.SyncProfile) error {
	if s == nil {
		return ErrManagerClosed
	}
	return s.manager.subscribe(s, session, subjectID, profile)
}

func (s *SubscriptionSource) Unsubscribe(session SessionID, subjectID int64) error {
	if s == nil {
		return ErrManagerClosed
	}
	return s.manager.unsubscribe(s, session, subjectID)
}

// Subscribe 使用默认来源订阅。重复调用幂等，改变 profile 时替换默认来源的视图。
// 与其他来源重叠时，只发送按 LOD、Key、SchemaVersion 升序选出的一个视图。
func (m *Manager) Subscribe(session SessionID, subjectID int64, profile entity.SyncProfile) error {
	return m.subscribe(nil, session, subjectID, profile)
}

func (m *Manager) subscribe(source *SubscriptionSource, session SessionID, subjectID int64, profile entity.SyncProfile) error {
	if m == nil {
		return ErrManagerClosed
	}
	if session == 0 {
		return ErrSessionInvalid
	}
	sess := m.session(session)
	if sess == nil {
		return ErrSessionUnknown
	}
	subj := m.subject(subjectID)
	if subj == nil {
		return ErrSubjectNotRegistered
	}
	profile = profile.Normalize()
	subj.mu.Lock()
	defer subj.mu.Unlock()
	// 验证生命周期与双向登记在同一临界区完成，Close 不会漏掉新订阅。
	m.mu.Lock()
	defer m.mu.Unlock()
	if current := m.sessions[session]; current == nil || current.lifetime != sess.lifetime {
		return ErrSessionUnknown
	}
	if subj.retiring {
		return ErrSubjectRetiring
	}
	if existing := subj.subscribers[session]; existing != nil && existing.lifetime == sess.lifetime {
		existing.sources[source] = profile
		active := m.bestProfile(existing.sources)
		if existing.kind != kindLeaving && existing.profile == active {
			return nil
		}
		if existing.kind == kindLeaving {
			existing.snapshotClass = snapshotArrival
		}
		existing.profile, existing.baseVersion = active, 0
		m.changeSubscriptionKindLocked(subj, session, existing, kindSnapshot)
		existing.revision++
		subj.profilesValid = false
		m.traceSnapshotRequest(subj, m.sessions[session])
		m.markPending(subjectID)
		m.WakeSync()
		return nil
	}
	if subj.subscribers[session] == nil && len(subj.subscribers) >= m.config.MaxSubscribersPerSubject {
		return ErrSubscriberLimit
	}
	if old := subj.subscribers[session]; old != nil {
		delete(old.lifetime.subjects, subjectID)
		m.changeSubscriptionKindLocked(subj, session, old, kindLeaving)
		m.subscriptionCount.Add(-1)
	}
	subj.subscribers[session] = &subscription{
		profile: profile, sources: map[*SubscriptionSource]entity.SyncProfile{source: profile},
		kind: kindSnapshot, revision: 1, lifetime: sess.lifetime,
	}
	subj.profilesValid = false
	m.snapshotRequests.update(subj, session, subj.subscribers[session])
	m.subscriptionCount.Add(1)
	sess.lifetime.subjects[subjectID] = subj
	m.traceSnapshotRequest(subj, m.sessions[session])
	m.markPending(subjectID)
	m.WakeSync()
	return nil
}

// Unsubscribe 只释放默认来源。其他来源仍持有时不会发送 remove。
func (m *Manager) Unsubscribe(session SessionID, subjectID int64) error {
	return m.unsubscribe(nil, session, subjectID)
}

func (m *Manager) unsubscribe(source *SubscriptionSource, session SessionID, subjectID int64) error {
	if m == nil {
		return ErrManagerClosed
	}
	subj := m.subject(subjectID)
	if subj == nil {
		return ErrSubjectNotRegistered
	}
	subj.mu.Lock()
	defer subj.mu.Unlock()
	existing := subj.subscribers[session]
	if existing == nil {
		return ErrSubscriptionNotFound
	}
	if _, held := existing.sources[source]; !held {
		return ErrSubscriptionNotFound
	}
	delete(existing.sources, source)
	if len(existing.sources) > 0 {
		active := m.bestProfile(existing.sources)
		if active != existing.profile {
			existing.profile, existing.baseVersion = active, 0
			m.changeSubscriptionKindLocked(subj, session, existing, kindSnapshot)
			existing.revision++
			subj.profilesValid = false
			if sess := m.session(session); sess != nil {
				m.traceSnapshotRequest(subj, sess)
			}
			m.markPending(subjectID)
			m.WakeSync()
		}
		return nil
	}
	if m.config.Trace != nil {
		if sess := m.session(session); sess != nil {
			m.config.Trace.Record(SyncTraceEvent{Stage: "baseline_cancelled", SubjectID: subjectID, Session: session, Lifetime: sess.lifetime.traceID, Epoch: sess.epoch})
		}
	}
	if !existing.inFlight && !m.sessionHoldsSubject(session, subjectID) {
		m.removeSubscriptionLocked(subj, session)
		if subj.retiring && len(subj.subscribers) == 0 {
			defer m.forget(subjectID)
		}
		return nil
	}
	m.changeSubscriptionKindLocked(subj, session, existing, kindLeaving)
	existing.revision++
	subj.profilesValid = false
	m.markPending(subjectID)
	m.WakeSync()
	return nil
}

// Subscribers reports the sessions subscribed to a subject, in ascending
// order, whatever their standing.
func (m *Manager) Subscribers(subjectID int64) []SessionID {
	subj := m.subject(subjectID)
	if subj == nil {
		return nil
	}
	subj.mu.Lock()
	defer subj.mu.Unlock()
	return subj.sessionsLocked()
}

// sessionSubjectsLocked 在 Manager.mu 内复制；调用方必须解锁后再访问 subject。
func sessionSubjectsLocked(sess *session) []*subject {
	subjects := make([]*subject, 0, len(sess.lifetime.subjects))
	for _, subj := range sess.lifetime.subjects {
		subjects = append(subjects, subj)
	}
	return subjects
}

// removeSubscriptionLocked 统一清理双向索引，调用方持有 subject.mu。
// 使用订阅自己的 lifetime，旧帧或旧 Close 不能删除重开会话的订阅。
func (m *Manager) removeSubscriptionLocked(subj *subject, sid SessionID) {
	sub := subj.subscribers[sid]
	if sub == nil {
		return
	}
	m.mu.Lock()
	delete(sub.lifetime.subjects, subj.id)
	delete(subj.subscribers, sid)
	m.changeSubscriptionKindLocked(subj, sid, sub, kindLeaving)
	m.subscriptionCount.Add(-1)
	m.mu.Unlock()
	subj.profilesValid = false
	m.clearSnapshotWaitLocked(subj)
}
