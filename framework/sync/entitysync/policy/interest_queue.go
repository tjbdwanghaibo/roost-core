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
	// 两种模式都不在业务线程（Nest worker）上做 AOI 工作：异步模式只通知应用线程；同步模式由 Manager 的政策阶段应用，
	// 已确认时提醒 Manager（on_change 下尽快 Flush，periodic 下无效果）。
	if in.applyMode == ApplyAsync {
		in.signalApplierLocked()
	} else if ready, _ := fact.condition(); ready {
		in.wake()
	}
	return nil
}

// ---- 排队事实的应用（DESIGN-TODO D19“两种应用模式”）----
//
// 两种模式共用同一个取批与应用步骤（applyBatchLocked）和同一个结算入口（applyLocked → releaseAppliedLocked），
// 只是谁、何时调用 applyBatchLocked 不同：
//
//   - ApplySync：settleQueued（Manager Flush 的政策阶段）先在 mu 内应用一批，再结算；没有应用线程。
//   - ApplyAsync：应用线程调用（下文），settleQueued 只结算。
//
// 所以“只有提交后的事实生效、同实体按序、Leave / Hide 改写、屏障到结算之后才释放、Drain、Close”两种模式逐项相同。
//
// 异步模式：排队事实不在 Flush 里应用：每个 Interest 有一个应用线程（第一条排队事实时启动），一步取一批已确认的事实
// （≤ BatchSize），在 mu 内做 AOI 写阶段与并行重评；AOI 事件留在 AOI 的事件缓冲里，应用完的事实记入 applied，
// 发布屏障仍然持有。Manager Flush 的政策阶段（settleQueued）只做结算：把 AOI 事件归并进 held、说给 Manager，
// 然后释放 applied 的屏障，同一 Flush 随后捕获内容——新位置与它带来的进出视野仍在同一帧。
// 一致点是批次边界：应用一步与结算都持 mu，Flush 撞上正在算的一批时最多等这一批。
//
// 唤醒：queueFact 与视野登记表变更非阻塞地通知应用线程；periodic 模式下提交确认没有通知（Manager.WakeSync 只在
// on_change 生效），所以队里有未确认事实时按 applierPollMin 起、最长 applierPollMax 的退避轮询提交门。

const (
	applierPollMin = time.Millisecond
	applierPollMax = 8 * time.Millisecond
	// 指标：排队 + 应用中 + 已应用未结算的事实数；应用线程每一步的耗时；一批从应用完成到被结算的等待。
	factBacklogMetric   = "entitysync_interest_fact_backlog"
	applyDurationMetric = "entitysync_interest_apply_duration"
	settleDelayMetric   = "entitysync_interest_settle_delay"
)

// applyGatherFor 是应用线程两步之间的最短间隔：periodic 模式取同步周期的 1/10（20Hz 时 5ms），结算只在 Flush 里
// 发生，周期内再细的拆分只会让同一观察者被多次重评；on_change 取 OnChangeBatchWindow（缺省 0，不合批）。
func applyGatherFor(m *entitysync.Manager) time.Duration {
	if m.Mode() == entitysync.ModeOnChange {
		return m.OnChangeBatchWindow()
	}
	return m.Interval() / 10
}

// signalApplierLocked 非阻塞地唤醒应用线程（第一次调用时启动它）。调用方持有 queueMu，且队列未关闭。
func (in *Interest) signalApplierLocked() {
	if !in.applierStarted {
		in.applierStarted = true
		in.applierRunning.Store(true)
		go in.runApplier()
	}
	select {
	case in.applierWake <- struct{}{}:
	default:
	}
}

// wakeViews 是视野登记表的唤醒：通知 Manager（on_change 下尽快进入政策阶段），已启动的应用线程也一并唤醒。
// 在 viewMu 内调用，不阻塞。
func (in *Interest) wakeViews() {
	// 只读原子标志：这里持有 viewMu，不能再取 queueMu（两把锁不同时持有）。通道不会关闭，停止后的发送无害。
	if in.applierRunning.Load() {
		select {
		case in.applierWake <- struct{}{}:
		default:
		}
	}
	in.wake()
}

func (in *Interest) runApplier() {
	defer close(in.applierDone)
	poll := time.NewTimer(time.Hour)
	poll.Stop()
	defer poll.Stop()
	backoff := time.Duration(0)
	var last time.Time
	for {
		select {
		case <-in.applierStop:
			return
		case <-in.applierWake:
		case <-poll.C:
		}
		// 合批：两步之间至少隔 applyGather，让这段时间里到达的事实并进同一批（同一观察者被几个事实波及时只重评一次）。
		if wait := time.Until(last.Add(in.applyGather)); wait > 0 {
			gather := time.NewTimer(wait)
			select {
			case <-in.applierStop:
				gather.Stop()
				return
			case <-gather.C:
			}
		}
		last = time.Now()
		progressed, waiting := false, false
		for {
			applied, more, unready := in.applyStep()
			progressed = progressed || applied
			waiting = unready
			if !more {
				break
			}
			select {
			case <-in.applierStop:
				return
			default:
			}
		}
		if !waiting {
			backoff = 0
			continue
		}
		// 队里还有未确认的事实：有进展时从最短间隔重新轮询，没有进展时加倍退避。
		if progressed || backoff == 0 {
			backoff = applierPollMin
		} else {
			backoff = min(2*backoff, applierPollMax)
		}
		poll.Reset(backoff)
	}
}

// applyStep 是应用线程的一步（异步模式）：取一批已确认的事实应用到 AOI，见 applyBatchLocked。
func (in *Interest) applyStep() (applied, more, waiting bool) {
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.closed {
		return false, false, false
	}
	// 视野登记表的变更先按顺序应用（批次写阶段之外），事实里的跟随移动才看得到新定义的视野。
	in.syncViewsLocked()
	return in.applyBatchLocked()
}

// applyBatchLocked 取一批（≤ BatchSize）已确认的事实应用到 AOI，应用完的事实记入 applied（发布屏障仍持有，
// 由下一次结算释放）。返回 applied（本步应用或丢弃了事实）、more（还有已确认的事实没取完）、waiting（队里仍有
// 事实，可能在等提交确认）。未确认事实留在原顺序，后来的同 ID 事实不能越过；其他 ID 可在本批继续。
// 调用方持有 mu，且已先应用登记表的变更（syncViewsLocked）。
func (in *Interest) applyBatchLocked() (applied, more, waiting bool) {
	in.queueMu.Lock()
	if len(in.facts) == 0 {
		in.queueMu.Unlock()
		return false, false, false
	}
	started := time.Now()
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
	completed := make([]queuedInterestFact, 0, len(batch))
	var failed error
	taken := 0
	appliedCommits := make(map[*entity.SyncMutation]bool)
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
		if taken >= in.batchSize && (fact.commit == nil || !appliedCommits[fact.commit]) {
			pending = append(pending, fact)
			blocked[fact.id] = true
			more = true
			continue
		}
		taken++
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
	// 未确认的前缀必须仍在新事实之前，且在途工作一直占预算；应用完的事实占预算直到被结算。
	arrived := len(in.facts)
	in.facts = append(pending, in.facts...)
	in.inflight -= len(pending)
	waiting = len(in.facts) > 0
	backlog := len(in.facts) + in.inflight
	in.queueMu.Unlock()
	more = more || arrived > 0
	in.applyErr = errors.Join(in.applyErr, failed)
	if len(completed) > 0 {
		if len(in.applied) == 0 {
			in.appliedAt = time.Now()
		}
		in.applied = append(in.applied, completed...)
		applied = true
		// 异步模式：on_change 下让 Manager 尽快结算；periodic 下由下一个周期结算。同步模式已在政策阶段里，随即结算。
		if in.applyMode == ApplyAsync {
			in.wake()
		}
	}
	metrics.SetGauge(factBacklogMetric, nil, int64(backlog))
	metrics.ObserveHistogram(applyDurationMetric, nil, time.Since(started))
	return applied, more, waiting
}

// settleQueued 是 Manager Flush 的政策阶段。同步模式先在这里应用一批已确认的事实（还有剩余时提醒 Manager，
// on_change 下尽快再来一轮，periodic 下等下一周期）；异步模式不应用（那是应用线程的事）。然后两种模式相同：应用登记表
// 剩余的变更，把 AOI 事件归并进 held 并说给 Manager，释放已应用事实的发布屏障（applyLocked），返回应用时攒下的错误
// （无法应用而丢弃的事实，只让一次 Flush 失败）。排过事实或记过业务视野变更之前（纯直接模式）由调用方的 Apply 驱动。
func (in *Interest) settleQueued() error {
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.closed {
		return nil
	}
	_, viewsActive := in.registry.state()
	in.syncViewsLocked()
	in.queueMu.Lock()
	active := in.queueActive
	in.queueMu.Unlock()
	if !active && !viewsActive {
		return nil
	}
	if in.applyMode == ApplySync {
		if _, more, _ := in.applyBatchLocked(); more {
			in.wake()
		}
	}
	in.applyLocked()
	err := in.applyErr
	in.applyErr = nil
	return err
}

// releaseAppliedLocked 在结算之后释放已应用事实的发布屏障，并把它们移出预算。任何结算入口（Flush 的政策阶段、
// 业务直接 Apply）都调它，订阅先到、内容被屏障挡住的半态不会留到下一次捕获。调用方持有 mu。
func (in *Interest) releaseAppliedLocked() {
	if len(in.applied) == 0 {
		return
	}
	metrics.ObserveHistogram(settleDelayMetric, nil, time.Since(in.appliedAt))
	for _, fact := range in.applied {
		fact.finish()
	}
	in.queueMu.Lock()
	in.inflight -= len(in.applied)
	backlog := len(in.facts) + in.inflight
	in.queueMu.Unlock()
	clear(in.applied)
	in.applied = in.applied[:0]
	metrics.SetGauge(factBacklogMetric, nil, int64(backlog))
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
