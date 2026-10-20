package policy

import (
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/entitysync"
	"github.com/tjbdwanghaibo/roost-core/infra/base/spatial"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
)

var ErrInterestQueueFull = errors.New("policy: interest fact queue is full")

// ErrObserverViewRate：同一玩家改视野太频繁（见 InterestConfig.ViewChangeInterval），这次请求被丢弃。
var ErrObserverViewRate = errors.New("policy: observer view changed too often")

// viewRejectedMetric 计数被拒绝的视野请求，reason 为 rate（超频）或 lod（层级未配置）。
const viewRejectedMetric = "entitysync_interest_view_rejected_total"

type queuedInterestFact struct {
	releasePublication func()
	commit             *entity.SyncMutation
	ready, discarded   bool
	coverage           []int64
	replaceCoverage    bool
	coverageOnly       bool
	id                 int64
	condition          func() (bool, bool)
	at                 spatial.Point
	// observer 为真时同时移动观察者；observerID 是 id 换算出的玩家 ID（关系事实同样以它为观察者）。
	observer   bool
	observerID int64
	// subjectGone：Hide 在事实应用前移走了被观察方，只剩观察者那一半要移动。
	subjectGone     bool
	relation        string
	subjects        []int64
	relationDelta   bool
	removedSubjects []int64
}

// ---- Leave / Hide 与排队事实 ----
//
// 排队事实在调用顺序上早于之后的 Leave / Hide，却在下一次政策阶段才应用。若 Leave / Hide 不改写 in.facts，
// 就绪的位置事实会在 MoveSubject 处得到 ErrInterestUnknown，被留下并阻塞该 id，错误一路返回到 Flush，Flush 在
// 取 pending 之前失败，之后每一次都一样——这个 Manager 上所有 subject 停发，Stop / Drain 一直报错。
//
// 所以 Leave / Hide 把自己的效果写进排队事实，使应用结果与“先应用事实、再 Leave / Hide”逐项相同，不多丢也不多留：
//   - 位置事实：MoveSubject 后 RemoveSubject ≡ RemoveSubject，被观察方那一半作废；MoveObserver 后 RemoveObserver
//     同理。两半都作废才丢弃整条——Hide 只移走被观察方，observer=true 的事实保留观察者的移动。
//   - 关系事实：Set(o, S) 后 Forget(e) ≡ Set(o, S\{e})，从成员里去掉实体 e；Leave 还 Clear(p)，
//     Set(p, S) 后 Clear(p) ≡ Clear(p)，玩家 p 作为观察者的关系事实整条丢弃。
// 只按 AOI 实际发生的删除改写（RemoveObserver / RemoveSubject 失败时那一半不动），所以不会丢掉仍然有效的事实。

// supersedeQueuedRelationsLocked 在关系源 Clear(observer)（observer 非零时）与 Forget(subject)（subject 非零时）之后调用。
// observer 是玩家 ID，subject 是实体 ID。调用方持有 in.mu。
func (in *Interest) supersedeQueuedRelationsLocked(observer, subject int64) {
	in.queueMu.Lock()
	defer in.queueMu.Unlock()
	in.rewriteFactsLocked(func(fact *queuedInterestFact) bool {
		if fact.relation == "" {
			return true
		}
		if observer != 0 && fact.observerID == observer {
			return false
		}
		if subject == 0 {
			return true
		}
		fact.subjects = slices.DeleteFunc(fact.subjects, func(id int64) bool { return id == subject })
		// 增量同样不能在退出后重放旧新增；旧移除已由 Forget 完成。
		fact.removedSubjects = slices.DeleteFunc(fact.removedSubjects, func(id int64) bool { return id == subject })
		return true
	})
}

// supersedeQueuedMovesLocked 在 AOI 实际移走实体 subject 的被观察方，或玩家 observer 的观察者之后调用（另一个传 0）。
// 调用方持有 in.mu。
func (in *Interest) supersedeQueuedMovesLocked(subject, observer int64) {
	in.queueMu.Lock()
	defer in.queueMu.Unlock()
	if subject != 0 {
		delete(in.subjectRoles, subject)
	}
	if observer != 0 {
		delete(in.observerRoles, observer)
	}
	in.rewriteFactsLocked(func(fact *queuedInterestFact) bool {
		if fact.relation != "" {
			return true
		}
		if subject != 0 && fact.id == subject {
			fact.subjectGone = true
			fact.replaceCoverage = false
			fact.coverage = nil
		}
		if observer != 0 && fact.observerID == observer {
			fact.observer = false
		}
		return !fact.subjectGone || fact.observer
	})
}

func (fact queuedInterestFact) finish() {
	if fact.releasePublication != nil {
		fact.releasePublication()
	}
}

func (in *Interest) rewriteFactsLocked(keep func(*queuedInterestFact) bool) {
	kept := in.facts[:0]
	for _, fact := range in.facts {
		if keep(&fact) {
			kept = append(kept, fact)
		} else {
			fact.finish()
		}
	}
	clear(in.facts[len(kept):])
	in.facts = kept
}

// QueueMove 在 Entity 锁内记录位置事实；锁外确认提交后、捕获同步内容前应用。
// observer=true 同时移动观察者（玩家 ID 由 InterestConfig.ObserverOf 从 e 换算），false 只移动被观察实体。
// 失败需由 handler 返回。
func (in *Interest) QueueMove(e entity.IThreadSafeEntity, at spatial.Point, observer bool) error {
	return in.queueFact(e, queuedInterestFact{at: at, observer: observer})
}

// QueueRelation 复制关系成员，避免业务后续修改切片。source 必须已声明。
// 观察者是 ObserverOf(e.ID()) 换算出的玩家 ID，成员是实体 ID。
func (in *Interest) QueueRelation(e entity.IThreadSafeEntity, source string, subjects []int64) error {
	if source == "" {
		return errors.New("policy: relation source required")
	}
	return in.queueFact(e, queuedInterestFact{relation: source, subjects: slices.Clone(subjects)})
}

// QueueRelationDelta 在 Entity 锁内记录关系增量，提交确认后按同 ID 顺序应用。
// 输入立即复制；同一调用中 removed 优先，重复 ID 幂等，零 ID 忽略。
// 错误必须由 handler 返回。它不替代业务关系 DAO 的修改和回滚。
func (in *Interest) QueueRelationDelta(e entity.IThreadSafeEntity, source string, added, removed []int64) error {
	if source == "" {
		return errors.New("policy: relation source required")
	}
	return in.queueFact(e, queuedInterestFact{relation: source, relationDelta: true, subjects: slices.Clone(added), removedSubjects: slices.Clone(removed)})
}

func (in *Interest) queueFact(e entity.IThreadSafeEntity, fact queuedInterestFact) error {
	if e == nil || e.Base() == nil {
		return errors.New("policy: entity required")
	}
	fact.id = e.ID()
	if fact.relation != "" || fact.observer {
		playerID, ok := in.observerOf(fact.id)
		if !ok || playerID == 0 {
			return fmt.Errorf("%w: entity %d has no observer", ErrInterestUnknown, fact.id)
		}
		fact.observerID = playerID
	}
	fact.commit = entity.CurrentSyncMutation()
	fact.condition = entity.SyncConditionFor(e)
	in.queueMu.Lock()
	defer in.queueMu.Unlock()
	if in.queueClosed {
		return errors.New("policy: interest is closed")
	}
	if fact.relation != "" && in.relations[fact.relation] == nil {
		return errors.New("policy: unknown relation source")
	}
	if fact.relation == "" {
		if !fact.coverageOnly && !in.aoi.Bounds().Contains(fact.at) {
			return spatial.ErrInvalidBounds
		}
		_, subject := in.subjectRoles[fact.id]
		_, observer := in.observerRoles[fact.observerID]
		if !subject || fact.observer && !observer {
			return ErrInterestUnknown
		}
	}
	if len(in.facts)+in.inflight >= in.maxQueuedFacts {
		return ErrInterestQueueFull
	}
	fact.releasePublication = e.Base().Sync().HoldSyncPublication()
	in.queueActive = true
	in.facts = append(in.facts, fact)
	if ready, _ := fact.condition(); ready {
		in.wake()
	}
	return nil
}

// applyQueued 与直接生命周期操作由 mu 串行化；业务 Queue* 只获取 queueMu。
// 未确认事实留在原顺序，后来的同 ID 事实不能越过；其他 ID 可在本批继续。
func (in *Interest) applyQueued() error {
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.closed {
		return nil
	}
	in.queueMu.Lock()
	if !in.queueActive {
		in.queueMu.Unlock()
		return nil
	}
	// 抽取有界总队列；以 batchSize 限制新提交，已开始的同一提交整体应用。
	// 仍检查未确认前缀后面的其他ID，避免待确认事务饿死无关实体。
	count := len(in.facts)
	batch := in.facts
	// 提交条件只读取框架原子门，不调用业务。必须在抽取的同一短锁内固定：
	// 若锁外才读ready，producer可能刚追加同一handler的后半段并提交，
	// 导致本轮只应用前半段。这里ready意味着该提交的全部事实已经在队列内。
	for i := range batch {
		batch[i].ready, batch[i].discarded = batch[i].condition()
	}
	in.facts = nil
	in.inflight += count
	in.queueMu.Unlock()
	blocked := make(map[int64]bool)
	pending := make([]queuedInterestFact, 0, len(batch))
	var failed error
	completed := make([]queuedInterestFact, 0, len(batch))
	applied := 0
	appliedCommits := make(map[*entity.SyncMutation]bool)
	more := false
	in.aoi.beginBatch()
	for _, fact := range batch {
		if fact.discarded {
			completed = append(completed, fact)
			continue
		}
		if !fact.ready || blocked[fact.id] {
			blocked[fact.id] = true
			pending = append(pending, fact)
			continue
		}
		if applied >= in.batchSize && (fact.commit == nil || !appliedCommits[fact.commit]) {
			pending = append(pending, fact)
			blocked[fact.id] = true
			more = true
			continue
		}
		applied++
		if fact.commit != nil {
			appliedCommits[fact.commit] = true
		}
		completed = append(completed, fact)
		var err error
		if fact.relation != "" {
			if fact.relationDelta {
				in.relations[fact.relation].ApplyDelta(fact.observerID, fact.subjects, fact.removedSubjects)
			} else {
				in.relations[fact.relation].Set(fact.observerID, fact.subjects)
			}
		} else {
			if !fact.subjectGone {
				if fact.replaceCoverage {
					err = in.aoi.SetObservedBlocks(fact.id, fact.coverage)
				}
				if err == nil && !fact.coverageOnly {
					err = in.aoi.MoveSubject(fact.id, fact.at)
				}
			}
			if err == nil && fact.observer {
				err = in.aoi.MoveObserver(fact.observerID, fact.at)
			}
		}
		if err != nil {
			failed = errors.Join(failed, fmt.Errorf("policy: dropped queued fact for %d: %w", fact.id, err))
			blocked[fact.id] = true
		}
	}
	in.aoi.endBatch(in.workers)
	in.queueMu.Lock()
	// 未确认的前缀必须仍在新事实之前，且在途工作一直占预算。
	in.facts = append(pending, in.facts...)
	in.inflight -= len(pending)
	more = more || len(in.facts) > len(pending)
	in.queueMu.Unlock()
	in.applyLocked()
	for _, fact := range completed {
		fact.finish()
	}
	in.queueMu.Lock()
	in.inflight -= count - len(pending)
	in.queueMu.Unlock()
	if more {
		in.wake()
	}
	return failed
}

// queuedPending 告诉 Drain 政策还有没做完的事：排队事实，以及还没停滞的重试 pair。
func (in *Interest) queuedPending() bool {
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.closed {
		return false
	}
	in.queueMu.Lock()
	pending := len(in.facts) > 0 || in.inflight > 0
	active := in.queueActive
	in.queueMu.Unlock()
	if !active {
		return false
	}
	if pending {
		return true
	}
	for _, key := range in.retry {
		if in.refused[key].count < retryStallAfter {
			return true
		}
	}
	return false
}

// ---- 重试停滞 ----
//
// 被拒的订阅留在 retry 里，每次 Apply 重说，直到被接受或 pair 被释放（拒绝不是对 pair 的裁决）。
// 不能把整个 retry 计入 queuedPending：一个永远不会被接受的 pair（观察者会话已关闭且不再重开、subject 已注销
// 而政策没 Hide……）会让 Drain 永不结束，只能等 ctx 超时，停机走不到 Close；排队模式也不返回 Refusal，
// 这种停滞没有任何其他痕迹。
//
// 同一 pair 连续被拒 retryStallAfter 次即判定为停滞：计数 entitysync_interest_retry_stalled_total、Warn 一次，
// 不再计入 queuedPending。它仍留在 retry 里照旧重说——会话重开、容量释放后照常订阅，计数随成功清零。
// 上界按 Flush 轮次计，默认周期 50ms 下约 500ms：足够覆盖“本轮 remove 释放容量、下一轮接受”这类暂时拒绝。
const retryStallAfter = 10

// refusal 是一个 pair 的连续拒绝记录：次数、第一次拒绝的时间与最后一次的错误（Drain 诊断用）。
type refusal struct {
	count int
	since time.Time
	err   error
}

// noteRefusedLocked 记一次拒绝，到达上界时告警。调用方持有 in.mu。
func (in *Interest) noteRefusedLocked(key pair, err error) {
	if in.refused == nil {
		in.refused = make(map[pair]refusal)
	}
	record := in.refused[key]
	if record.count == 0 {
		record.since = time.Now()
	}
	record.count++
	record.err = err
	in.refused[key] = record
	if record.count != retryStallAfter {
		return
	}
	metrics.IncCounter("entitysync_interest_retry_stalled_total", nil, 1)
	slog.Warn("entitysync/policy: subscription refused repeatedly; still retried but no longer holds Drain",
		"observer", key.observer, "subject", key.subject, "session", sessionOf(key.observer),
		"attempts", retryStallAfter, "err", err)
}

// drainStalls 是 Drain 超时诊断（entitysync.PolicyHooks.Stalls）：列出仍让 queuedPending 为真的条目——
// 未确认提交的排队事实（按入队顺序，同一 ID 只列一次），以及拒绝次数未到 retryStallAfter 的重试 pair。
// 排队事实不记录入队时间（入队在 Entity 锁内，不为诊断多读一次时钟），Waited 为 0。
func (in *Interest) drainStalls(limit int) ([]entitysync.DrainStall, int) {
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.closed {
		return nil, 0
	}
	var stalls []entitysync.DrainStall
	total := 0
	add := func(stall entitysync.DrainStall) {
		total++
		if len(stalls) < limit {
			stalls = append(stalls, stall)
		}
	}
	in.queueMu.Lock()
	seen := make(map[int64]bool)
	for _, fact := range in.facts {
		if seen[fact.id] {
			continue
		}
		seen[fact.id] = true
		reason := "queued_fact"
		if ready, discarded := fact.condition(); !ready && !discarded {
			reason = "commit_pending"
		}
		add(entitysync.DrainStall{Source: "interest", Subject: fact.id, Reason: reason})
	}
	if in.inflight > 0 && len(in.facts) == 0 {
		add(entitysync.DrainStall{Source: "interest", Reason: "facts_in_flight", Detail: fmt.Sprintf("%d fact(s) being applied", in.inflight)})
	}
	in.queueMu.Unlock()
	now := time.Now()
	for _, key := range in.retry {
		record := in.refused[key]
		if record.count >= retryStallAfter {
			continue
		}
		stall := entitysync.DrainStall{Source: "interest", Subject: key.subject, Session: sessionOf(key.observer), Reason: "subscribe_refused",
			Detail: fmt.Sprintf("observer %d, refused %d time(s)", key.observer, record.count), Err: record.err}
		if !record.since.IsZero() {
			stall.Waited = now.Sub(record.since)
		}
		add(stall)
	}
	return stalls, total
}
