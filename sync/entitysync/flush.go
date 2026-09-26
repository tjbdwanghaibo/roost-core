package entitysync

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/tjbdwanghaibo/roost-core/entity"
	"github.com/tjbdwanghaibo/roost-core/metrics"
)

func (m *Manager) markPending(subjectID int64) {
	m.pendingMu.Lock()
	if _, exists := m.pending[subjectID]; !exists {
		if _, waiting := m.waitingSnapshots[subjectID]; waiting {
			m.pendingOverlap++
		}
		m.pending[subjectID] = struct{}{}
	}
	m.pendingMu.Unlock()
}

func (m *Manager) takePending() []int64 {
	m.pendingMu.Lock()
	ids := make([]int64, 0, len(m.pending))
	for id := range m.pending {
		ids = append(ids, id)
	}
	clear(m.pending)
	m.pendingOverlap = 0
	m.pendingMu.Unlock()
	slices.Sort(ids)
	return ids
}

// ---- the tick ----

// settlement is what becomes true for one (session, subject) once the
// session's frame has been admitted.
type settlement struct {
	sub           *subscription
	revision      uint64
	subj          *subject
	version       uint64
	remove        bool
	snapshotClass snapshotClass
}

// flushSession 集中保存同一次捕获的会话身份、帧内容与结算记录。
type flushSession struct {
	session       *session
	entries       []frameEntry
	settlements   []settlement
	snapshotAfter [2]int64
}

// Flush 执行一次同步 tick：捕获 pending subject，按会话组帧并逐帧准入，最后提交内容版本。
// 同一 subject 的快照和增量在实体锁内捕获；持久化水位不足时整体暂缓该 subject。
//
// ErrRetryLater 会中止本轮内容提交并保留脏状态；已准入的会话时钟和引用继续有效，
// 相关订阅在下次用全量恢复基线。其他交付错误只关闭失败会话。
// flushGate 串行化整个过程，避免同一内容同时参与两次捕获。
func (m *Manager) Flush(ctx context.Context) (result error) {
	if m == nil {
		return ErrManagerClosed
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := m.acquireFlush(ctx); err != nil {
		return err
	}
	defer m.releaseFlush()
	if m.isClosed() {
		return ErrManagerClosed
	}
	started := time.Now()
	m.flushCalls.Add(1)
	defer func() {
		// 所有已执行的失败 Flush 在一个出口记账，policy 失败也必须保留原因。
		if result != nil {
			m.flushFailures.Add(1)
			m.setLastError(result)
		}
		elapsed := time.Since(started)
		m.flushNanos.Add(uint64(elapsed))
		m.lastFlushNanos.Store(uint64(elapsed))
		metrics.ObserveHistogram("entitysync_flush_duration", nil, elapsed)
	}()
	if err := m.applyPolicies(); err != nil {
		return fmt.Errorf("sync policy: %w", err)
	}
	m.refreshSnapshotWindow()
	ids := m.takePending()
	if len(ids) == 0 {
		m.emptyFlushes.Add(1)
		return nil
	}
	var watermark uint64
	gated := m.config.DurableWatermark != nil
	if gated {
		watermark = m.config.DurableWatermark()
	}

	prepared := make([]*entity.PreparedSubjectSync, 0, len(ids))
	// 常见单 profile 用一块连续存储；每会话仅持有捕获项指针。
	captures := make([]capturedUpdate, 0, len(ids))
	work := m.flushWork
	if work == nil {
		work = make(map[SessionID]*flushSession)
	}
	defer m.recycleFlush(work)
	defer releaseInFlight(work)
	var retry []int64
	var failures []error

	captureStarted := time.Now()
	plan := m.planSnapshotCaptures(ids)
	var selectedSnapshots map[*subscription]bool
	if plan != nil {
		selectedSnapshots = plan.selected
	}
	// 捕获：按 subject 获取同一版本的快照与增量，归入各会话待交付内容。
	for _, id := range ids {
		subj := m.subject(id)
		if subj == nil {
			continue
		}
		subj.mu.Lock()
		// Sessions that closed since are pruned here: their entries owe
		// nothing. Held sessions are skipped for this tick — ReadySession
		// schedules the subject again when they can receive.
		var held map[SessionID]bool
		hold := func(sid SessionID) {
			if held == nil {
				held = make(map[SessionID]bool)
			}
			held[sid] = true
		}
		for sid, sub := range subj.subscribers {
			sess := m.session(sid)
			switch {
			case sess == nil || sess.lifetime != sub.lifetime:
				m.removeSubscriptionLocked(subj, sid)
			case sess.held:
				hold(sid)
			default:
				if captured := work[sid]; captured != nil && captured.session != sess {
					hold(sid)
				} else {
					if work[sid] == nil {
						work[sid] = m.takeFlushSession(sess)
					}
				}
			}
		}
		if selectedSnapshots != nil {
			for sid, sub := range subj.subscribers {
				if sub.kind == kindSnapshot && !held[sid] && work[sid] != nil {
					// 已有引用的视图替换仍是一次对象更新，不能等待冷恢复预算。
					if _, exists := work[sid].session.objects[id]; exists {
						selectedSnapshots[sub] = true
					}
				}
				if sub.kind == kindSnapshot && !held[sid] && !selectedSnapshots[sub] {
					m.deferSnapshot(subj)
					m.snapshotsDeferred.Add(1)
				}
			}
		}
		deltaProfiles, snapshotProfiles := subj.profilesLocked(selectedSnapshots)
		dirty := subj.state.PendingDirty()
		wantsCapture := len(snapshotProfiles) > 0 || dirty
		if wantsCapture {
			// 无接收者也捕获并提交脏版本，但不为无人需要的默认视图打包。
			revisions := make(map[*subscription]uint64, len(subj.subscribers))
			for _, sub := range subj.subscribers {
				revisions[sub] = sub.revision
			}
			subj.mu.Unlock()
			item, err := subj.state.PrepareViews(deltaProfiles, snapshotProfiles)
			subj.mu.Lock()
			switch {
			case errors.Is(err, entity.ErrSubjectSyncNotDirty):
			case errors.Is(err, entity.ErrSyncCommitPending):
				if m.config.Trace != nil {
					m.config.Trace.Record(SyncTraceEvent{Stage: "commit_pending", SubjectID: id})
				}
				retry = append(retry, id)
			case err != nil:
				failures = append(failures, fmt.Errorf("sync prepare subject %d: %w", id, err))
				retry = append(retry, id)
			case gated && capturedAbove(item, watermark):
				if m.config.Trace != nil {
					m.config.Trace.Record(SyncTraceEvent{Stage: "durability_wait", SubjectID: id, Version: item.Version(), CommitLSN: item.CommitLSN()})
				}
				_ = item.AbortWithError(ErrDurabilityDeferred)
				m.deferred.Add(1)
				metrics.IncCounter("entitysync_durability_gate_deferred_total", nil, 1)
				retry = append(retry, id)
			default:
				dirty = item.Version() != item.BaseVersion()
				if m.config.Trace != nil {
					m.config.Trace.Record(SyncTraceEvent{Stage: "prepared", SubjectID: id, Version: item.Version(), BaseVersion: item.BaseVersion(), CommitLSN: item.CommitLSN()})
				}
				prepared = append(prepared, item)
				if dirty {
					m.dirtyCaptured.Add(1)
				}
				snapshotUpdates, deltaUpdates := item.Snapshots(), item.Updates()
				m.snapshotsCaptured.Add(uint64(len(snapshotUpdates)))
				recordFullReasons(snapshotUpdates)
				recordFullReasons(deltaUpdates)
				snapshots, deltas := captureUpdates(&captures, snapshotUpdates), captureUpdates(&captures, deltaUpdates)
				for sid, sub := range subj.subscribers {
					if held[sid] {
						continue
					}
					if work[sid] == nil || revisions[sub] != sub.revision {
						m.markPending(id)
						continue
					}
					switch sub.kind {
					case kindSnapshot:
						if selectedSnapshots != nil && !selectedSnapshots[sub] {
							continue
						}
						if update, ok := updateFor(snapshots, sub.profile); ok {
							work[sid].entries = append(work[sid].entries, frameEntry{subjectID: id, kind: entryCreate, update: update})
							work[sid].settlements = append(work[sid].settlements, captureSettlement(subj, sub, item.Version(), false))
						}
					case kindLive:
						if !dirty {
							continue
						}
						if update, ok := updateFor(deltas, sub.profile); ok {
							work[sid].entries = append(work[sid].entries, frameEntry{subjectID: id, kind: entryUpdate, update: update})
							work[sid].settlements = append(work[sid].settlements, captureSettlement(subj, sub, item.Version(), false))
						}
					}
				}
			}
		}
		for sid, sub := range subj.subscribers {
			if sub.kind == kindLeaving && !held[sid] && work[sid] != nil {
				work[sid].entries = append(work[sid].entries, frameEntry{subjectID: id, kind: entryRemove})
				work[sid].settlements = append(work[sid].settlements, captureSettlement(subj, sub, 0, true))
			}
		}
		subj.mu.Unlock()
	}

	m.captureNanos.Add(uint64(time.Since(captureStarted)))
	// 准入：会话有序处理，每帧成功后推进线协议状态，该会话全部帧成功后结算订阅。
	sessionIDs := make([]SessionID, 0, len(work))
	for sid, batch := range work {
		if len(batch.entries) == 0 {
			continue
		}
		sessionIDs = append(sessionIDs, sid)
	}
	slices.Sort(sessionIDs)
	m.scheduleSnapshots(work, plan)

	var admittedSessions []SessionID
	for _, sid := range sessionIDs {
		batch := work[sid]
		sess := batch.session
		if sess == nil || m.session(sid) != sess {
			continue
		}
		encodeStarted := time.Now()
		if m.config.Trace != nil {
			for _, entry := range batch.entries {
				if entry.update == nil {
					continue
				}
				u := entry.update.update
				m.config.Trace.Record(SyncTraceEvent{Stage: "selected", SubjectID: entry.subjectID, Session: sid, Lifetime: sess.lifetime.traceID, Epoch: sess.epoch, Version: u.Version, BaseVersion: u.BaseVersion, Full: u.Full, Reason: uint8(u.Reason), Snapshot: entry.kind == entryCreate})
			}
		}
		// 编码只改副本；每一帧准入后分别采纳其时钟与引用表。
		// 后一帧或另一会话失败时，已成功交付的前缀不能回滚。
		next := sess.clone()
		next.snapshotAfter = batch.snapshotAfter
		payloads, err := next.encode(batch.entries, m.wire)
		if m.config.Trace != nil {
			if err != nil {
				m.config.Trace.Record(SyncTraceEvent{Stage: "encode_failed", Session: sid, Lifetime: sess.lifetime.traceID, Epoch: sess.epoch, Duration: time.Since(encodeStarted)})
			}
			for _, encoded := range payloads {
				m.config.Trace.Record(SyncTraceEvent{Stage: "encoded", Session: sid, Lifetime: sess.lifetime.traceID, Epoch: encoded.next.epoch, Tick: encoded.next.tick, Duration: time.Since(encodeStarted)})
			}
		}
		m.encodeNanos.Add(uint64(time.Since(encodeStarted)))
		if err != nil {
			m.loseSession(sess, err)
			continue
		}
		var pushErr error
		stale := false
		for i, encoded := range payloads {
			if m.session(sid) != sess {
				stale = true
				break
			}
			admissionStarted := time.Now()
			pushErr = m.config.Transport.Push(ctx, sid, encoded.payload)
			if m.config.Trace != nil {
				stage := "admitted"
				if pushErr != nil {
					stage = "admission_failed"
				}
				m.config.Trace.Record(SyncTraceEvent{Stage: stage, Session: sid, Lifetime: sess.lifetime.traceID, Epoch: encoded.next.epoch, Tick: encoded.next.tick, Duration: time.Since(admissionStarted)})
			}
			m.admissionNanos.Add(uint64(time.Since(admissionStarted)))
			if pushErr != nil {
				break
			}
			adopted := m.adoptSession(sess, encoded.next)
			if i == 0 {
				admittedSessions = append(admittedSessions, sid)
			}
			m.framesAdmitted.Add(1)
			m.createsAdmitted.Add(encoded.creates)
			m.updatesAdmitted.Add(encoded.updates)
			m.removesAdmitted.Add(encoded.removes)
			metrics.IncCounter("entitysync_frames_admitted_total", nil, 1)
			if !adopted {
				stale = true
				break
			}
			sess = encoded.next
		}
		if ctx.Err() != nil && pushErr != nil {
			pushErr = ctx.Err()
		}
		if errors.Is(pushErr, ErrRetryLater) || (pushErr != nil && ctx.Err() != nil) {
			// 内容捕获作废并保留脏位，但已交付会话可能已应用未提交版本的增量。
			// 它们下次必须收全量，不能再把旧 baseVersion 的增量当成连续历史。
			abortAll(prepared, pushErr)
			m.requireSnapshotsAfterRetry(admittedSessions, work)
			for _, id := range ids {
				m.markPending(id)
			}
			return fmt.Errorf("sync admission session %d: %w", sid, pushErr)
		}
		if pushErr != nil {
			m.loseSession(sess, pushErr)
			continue
		}
		if !stale {
			m.settleSubscriptions(sid, batch.settlements)
		}
	}

	// 内容提交：只有未触发整体重试才提交捕获；清理退役 subject 并重排暂缓项。
	if len(prepared) > 0 {
		batch, err := entity.ReservePreparedSubjectSyncBatch(prepared)
		if err != nil {
			abortAll(prepared, err)
			failures = append(failures, fmt.Errorf("sync reserve: %w", err))
		} else if err := batch.Commit(); err != nil {
			failures = append(failures, fmt.Errorf("sync commit: %w", err))
		}
	}
	for _, id := range ids {
		subj := m.subject(id)
		if subj == nil {
			continue
		}
		subj.mu.Lock()
		done := subj.retiring && len(subj.subscribers) == 0
		m.clearSnapshotWaitLocked(subj)
		subj.mu.Unlock()
		if done {
			m.forget(id)
		}
	}
	for _, id := range retry {
		m.markPending(id)
	}
	return errors.Join(failures...)
}

// captureSettlement 在 subject 锁内记录本轮捕获对应的意图。
func captureSettlement(subj *subject, sub *subscription, version uint64, remove bool) settlement {
	sub.inFlight = true
	return settlement{subj: subj, sub: sub, revision: sub.revision, version: version, remove: remove, snapshotClass: sub.snapshotClass}
}

func releaseInFlight(work map[SessionID]*flushSession) {
	for _, batch := range work {
		for _, entry := range batch.settlements {
			entry.subj.mu.Lock()
			entry.sub.inFlight = false
			entry.subj.mu.Unlock()
		}
	}
}

// requireSnapshotsAfterRetry 为已收到本轮内容的会话重新建立全量基线。
// remove 仍按原意结算；已交付的会话时钟和引用表不在这里回滚。
func (m *Manager) requireSnapshotsAfterRetry(admittedSessions []SessionID, work map[SessionID]*flushSession) {
	for _, admitted := range admittedSessions {
		for _, settle := range work[admitted].settlements {
			if settle.remove {
				continue
			}
			settle.subj.mu.Lock()
			if sub := settle.subj.subscribers[admitted]; sub == settle.sub && sub.kind != kindLeaving {
				sub.baseVersion = 0
				m.changeSubscriptionKindLocked(settle.subj, admitted, sub, kindSnapshot)
				settle.subj.profilesValid = false
				m.traceSnapshotRequest(settle.subj, work[admitted].session)
			}
			settle.subj.mu.Unlock()
		}
	}
}

// settleSubscriptions 在当前会话的全部帧准入后更新订阅意图；
// 编码和传输期间已经删除的订阅不会在这里重新创建。
func (m *Manager) settleSubscriptions(sid SessionID, settlements []settlement) {
	for _, settle := range settlements {
		settle.subj.mu.Lock()
		if sub := settle.subj.subscribers[sid]; sub == settle.sub && sub.revision == settle.revision {
			if settle.remove {
				m.removeSubscriptionLocked(settle.subj, sid)
			} else {
				if sub.kind != kindLive {
					settle.subj.profilesValid = false
				}
				sub.baseVersion = settle.version
				m.changeSubscriptionKindLocked(settle.subj, sid, sub, kindLive)
				sub.snapshotClass = snapshotArrival
			}
		}
		settle.subj.mu.Unlock()
	}
}

// capturedAbove reports whether any part of the capture came from a commit
// the durable watermark has not reached.
func capturedAbove(item *entity.PreparedSubjectSync, watermark uint64) bool {
	return item.CommitLSN() > watermark
}

func abortAll(prepared []*entity.PreparedSubjectSync, cause error) {
	for _, item := range prepared {
		_ = item.AbortWithError(cause)
	}
}

// 固定标签集合：不以实体、会话或业务提供的 reason 数值制造无限监控序列。
func recordFullReasons(updates []entity.SubjectSyncUpdate) {
	for _, update := range updates {
		if !update.Full {
			continue
		}
		reason := "other"
		switch update.Reason {
		case entity.SyncFullReasonDirty:
			reason = "dirty"
		case entity.SyncFullReasonResync:
			reason = "resync"
		case entity.SyncFullReasonSchema:
			reason = "schema"
		}
		metrics.IncCounter("entitysync_full_captures_total", metrics.Labels{"reason": reason}, 1)
	}
}
