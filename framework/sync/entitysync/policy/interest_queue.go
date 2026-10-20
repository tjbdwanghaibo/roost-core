package policy

import (
	"errors"
	"fmt"
	"log/slog"
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
	// subjectGone：Hide / Leave 在事实应用前移走了目标，只剩跟随它的空间视野那一半要移动。
	subjectGone bool
}

// ---- Leave / Hide 与排队事实 ----
//
// 排队事实在调用顺序上早于之后的 Leave / Hide，却在下一次政策阶段才应用。若 Leave / Hide 不改写 in.facts，
// 就绪的位置事实会在 MoveSubject 处得到 ErrInterestUnknown，被留下并阻塞该 id，错误一路返回到 Flush，Flush 在
// 取 pending 之前失败，之后每一次都一样——这个 Manager 上所有 subject 停发，Stop / Drain 一直报错。
//
// 所以 Leave / Hide 把自己的效果写进排队事实，使应用结果与“先应用事实、再 Leave / Hide”逐项相同，不多丢也不多留：
// MoveSubject 后 RemoveSubject ≡ RemoveSubject，目标那一半作废；跟随它的空间视野在“先应用”时会被带到新位置，
// 所以那一半保留（应用时按当时的跟随表移动，Leave 撤掉的主视野自然不在表里）。只改额外覆盖的事实整条丢弃。
// 只按 AOI 实际发生的删除改写（RemoveSubject 失败时不动），所以不会丢掉仍然有效的事实。

// supersedeQueuedMovesLocked 在 AOI 实际移走实体 subject 之后调用。调用方持有 in.mu。
func (in *Interest) supersedeQueuedMovesLocked(subject int64) {
	in.queueMu.Lock()
	defer in.queueMu.Unlock()
	delete(in.subjectRoles, subject)
	in.rewriteFactsLocked(func(fact *queuedInterestFact) bool {
		if fact.id == subject {
			fact.subjectGone = true
			fact.replaceCoverage = false
			fact.coverage = nil
		}
		return !fact.subjectGone || !fact.coverageOnly
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

// QueueMove 在 Entity 锁内记录位置事实；锁外确认提交后、捕获同步内容前应用：移动目标 e，跟随 e 的空间视野
// （玩家主视野、单位视野）一起移动。位置是 DAO 驱动的事实，必须与提交一致，所以走提交门；视野配置类操作不走。
// 失败需由 handler 返回。
func (in *Interest) QueueMove(e entity.IThreadSafeEntity, at spatial.Point) error {
	return in.queueFact(e, queuedInterestFact{at: at})
}

func (in *Interest) queueFact(e entity.IThreadSafeEntity, fact queuedInterestFact) error {
	if e == nil || e.Base() == nil {
		return errors.New("policy: entity required")
	}
	fact.id = e.ID()
	fact.commit = entity.CurrentSyncMutation()
	fact.condition = entity.SyncConditionFor(e)
	in.queueMu.Lock()
	defer in.queueMu.Unlock()
	if in.queueClosed {
		return errors.New("policy: interest is closed")
	}
	if !fact.coverageOnly && !in.aoi.Bounds().Contains(fact.at) {
		return spatial.ErrInvalidBounds
	}
	if _, subject := in.subjectRoles[fact.id]; !subject {
		return ErrInterestUnknown
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
	// 视野登记表的变更先按顺序应用（批次写阶段之外），事实里的跟随移动才看得到新定义的视野。
	// 排过事实或记过业务视野变更之后，政策阶段负责结算与重试；在此之前（纯直接模式）由调用方的 Apply 驱动。
	_, viewsActive := in.registry.state()
	in.syncViewsLocked()
	in.queueMu.Lock()
	if !in.queueActive {
		in.queueMu.Unlock()
		if viewsActive {
			in.applyLocked()
		}
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
		if !fact.subjectGone {
			if fact.replaceCoverage {
				err = in.aoi.SetObservedBlocks(fact.id, fact.coverage)
			}
			if err == nil && !fact.coverageOnly {
				err = in.aoi.MoveSubject(fact.id, fact.at)
			}
		}
		if err == nil && !fact.coverageOnly {
			in.moveFollowersLocked(fact.id, fact.at)
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

// queuedPending 告诉 Drain 政策还有没做完的事：未应用的视野变更、排队事实，以及还没停滞的重试 pair。
func (in *Interest) queuedPending() bool {
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.closed {
		return false
	}
	viewsPending, viewsActive := in.registry.state()
	if viewsPending {
		return true
	}
	in.queueMu.Lock()
	pending := len(in.facts) > 0 || in.inflight > 0
	active := in.queueActive || viewsActive
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
