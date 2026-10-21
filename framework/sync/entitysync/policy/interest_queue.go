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

// queuedInterestFact 是一条门控事实：位置 / 额外覆盖，走提交门，提交确认后才应用。
type queuedInterestFact struct {
	releasePublication func()
	commit             *entity.SyncMutation
	ready, discarded   bool
	coverage           []int64
	replaceCoverage    bool
	coverageOnly       bool
	id                 int64
	// gen 是登记时目标的代际号：之后的 Leave / Hide / 再次放置换代，旧代际的事实应用时作废（见 runLogLocked）。
	gen       uint64
	condition func() (bool, bool)
	at        spatial.Point
}

func (fact queuedInterestFact) finish() {
	if fact.releasePublication != nil {
		fact.releasePublication()
	}
}

// logEntry 是登记侧日志的一条：门控事实（op 为 nil）或其他命令（op）。日志的顺序就是登记顺序，只有应用方按序执行。
type logEntry struct {
	fact queuedInterestFact
	op   *viewOp
}

// subjectGen 是“同一目标、同一代际”的键：未确认的事实只挡住同一代际后面的事实。
type subjectGen struct {
	id  int64
	gen uint64
}

// QueueMove 在 Entity 锁内记录位置事实；锁外确认提交后、捕获同步内容前应用：移动目标 e，跟随 e 的空间视野
// （玩家主视野、单位视野）一起移动。位置是 DAO 驱动的事实，必须与提交一致，所以走提交门；视野配置类操作不走。
// 失败需由 handler 返回。
func (in *Interest) QueueMove(e entity.IThreadSafeEntity, at spatial.Point) error {
	return in.queueFact(e, queuedInterestFact{at: at})
}

// queueFact 只取登记侧短锁：检查边界、目标是否登记与预算，记下目标当前的代际，持有发布屏障并追加到日志。
// 不做 AOI 工作，不等应用方。
func (in *Interest) queueFact(e entity.IThreadSafeEntity, fact queuedInterestFact) error {
	if e == nil || e.Base() == nil {
		return errors.New("policy: entity required")
	}
	fact.id = e.ID()
	fact.commit = entity.CurrentSyncMutation()
	fact.condition = entity.SyncConditionFor(e)
	r := &in.registry
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return errors.New("policy: interest is closed")
	}
	if !fact.coverageOnly && !in.aoi.Bounds().Contains(fact.at) {
		return spatial.ErrInvalidBounds
	}
	gen, subject := r.subjects[fact.id]
	if !subject {
		return ErrInterestUnknown
	}
	if r.facts >= in.maxQueuedFacts {
		return ErrInterestQueueFull
	}
	fact.gen = gen
	fact.releasePublication = e.Base().Sync().HoldSyncPublication()
	r.facts++
	r.active = true
	r.log = append(r.log, logEntry{fact: fact})
	// 同步模式由 Manager 的政策阶段应用，已确认时提醒 Manager（on_change 下尽快 Flush，periodic 下无效果）。
	if in.applyMode == ApplyAsync {
		in.signalApplierLocked()
	} else if ready, _ := fact.condition(); ready {
		in.wake()
	}
	return nil
}

// ---- 日志的执行（单写者，REFACTOR-2026-10-21-interest-single-writer）----
//
// 全部写入口只在登记侧追加日志；应用方（同步模式：Manager Flush 的政策阶段；异步模式：应用线程；以及显式 Apply 与
// 诊断读）持 mu 按日志顺序执行，两种模式共用 runLogLocked 与同一个结算入口（applyLocked → releaseAppliedLocked）。
//
// 执行规则：
//   - 门控事实：回滚 / 拒绝的完成并释放屏障；代际号不是目标当前代际的（期间被 Leave / Hide / 再次放置）作废；
//     未确认的留在原位，同一 (目标, 代际) 后面的事实不能越过，其他目标可以；就绪的进入 AOI 批次。
//   - 其他命令不等未确认的事实（它们不依赖提交）；在 AOI 批次之外串行执行。
//   - 一步取到 BatchSize 个就绪事实即在此切开：之后的命令与事实留到下一步（同一提交不切开），已就绪的事实不会被
//     它后面的命令越过——所以“已提交的移动、再 Leave → Enter”与“先应用移动、再 Leave → Enter”逐项相同。
//
// 异步模式的唤醒：登记时非阻塞通知应用线程；periodic 模式下提交确认没有通知（Manager.WakeSync 只在 on_change
// 生效），所以日志里有未确认事实时按 applierPollMin 起、最长 applierPollMax 的退避轮询提交门。

const (
	applierPollMin = time.Millisecond
	applierPollMax = 8 * time.Millisecond
	// 指标：排队 + 应用中 + 已应用未结算的事实数；应用方每一步的耗时；一批从应用完成到被结算的等待。
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

// syncApplyBudgetFor 是同步模式一次政策阶段排空日志的耗时预算：同步周期的一半（至少 1ms）。超出时剩余的留到下一次
// 政策阶段（on_change 下立即再来一轮），Flush 不会因为积压而无界变长。
func syncApplyBudgetFor(m *entitysync.Manager) time.Duration {
	return max(m.Interval()/2, time.Millisecond)
}

// signalApplierLocked 非阻塞地唤醒应用线程（第一次调用时启动它）。调用方持有登记侧锁，且登记侧未关闭。
func (in *Interest) signalApplierLocked() {
	if !in.applierStarted {
		in.applierStarted = true
		go in.runApplier()
	}
	select {
	case in.applierWake <- struct{}{}:
	default:
	}
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
		// 日志里还有未确认的事实：有进展时从最短间隔重新轮询，没有进展时加倍退避。
		if progressed || backoff == 0 {
			backoff = applierPollMin
		} else {
			backoff = min(2*backoff, applierPollMax)
		}
		poll.Reset(backoff)
	}
}

// applyStep 是应用线程的一步（异步模式）：见 runLogLocked。
func (in *Interest) applyStep() (applied, more, waiting bool) {
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.closed {
		return false, false, false
	}
	return in.runLogLocked()
}

// drainLogLocked 一步接一步执行日志，直到没有可执行的条目（只剩未确认事实或日志空）。显式 Apply 与诊断读用它；
// 同步模式的政策阶段另有耗时预算（settleQueued）。调用方持有 mu。
func (in *Interest) drainLogLocked() {
	for {
		if _, more, _ := in.runLogLocked(); !more {
			return
		}
	}
}

// runLogLocked 执行一步：取出日志，按序执行命令、把就绪事实应用到 AOI（≤ BatchSize，见上文规则），应用完的事实
// 记入 applied（发布屏障仍持有，由下一次结算释放）。返回 progressed（执行了命令或完成了事实）、more（还有可执行的
// 条目没取完）、waiting（日志里仍有事实，可能在等提交确认）。调用方持有 mu。
func (in *Interest) runLogLocked() (progressed, more, waiting bool) {
	r := &in.registry
	r.mu.Lock()
	if len(r.log) == 0 {
		r.mu.Unlock()
		return false, false, false
	}
	started := time.Now()
	batch := r.log
	r.log = nil
	// 提交条件只读取框架原子门，不调用业务。必须在取日志的同一短锁内固定：若锁外才读 ready，producer 可能刚追加
	// 同一 handler 的后半段并提交，导致本步只应用前半段。这里 ready 意味着该提交的全部事实已经在日志里。
	for i := range batch {
		if batch[i].op == nil {
			batch[i].fact.ready, batch[i].fact.discarded = batch[i].fact.condition()
		}
	}
	r.mu.Unlock()

	blocked := make(map[subjectGen]bool)
	pending := make([]logEntry, 0, len(batch))
	completed := make([]queuedInterestFact, 0, len(batch))
	appliedCommits := make(map[*entity.SyncMutation]bool)
	var failed error
	taken, cut, batching := 0, false, false
	endBatch := func() {
		if batching {
			in.aoi.endBatch(in.workers)
			batching = false
		}
	}
	for _, entry := range batch {
		if entry.op != nil {
			if cut {
				pending = append(pending, entry)
				continue
			}
			endBatch()
			in.runOpLocked(entry.op)
			progressed = true
			continue
		}
		fact := entry.fact
		key := subjectGen{id: fact.id, gen: fact.gen}
		if fact.discarded {
			completed = append(completed, fact)
			continue
		}
		if !fact.ready || blocked[key] {
			blocked[key] = true
			pending = append(pending, entry)
			continue
		}
		sameCommit := fact.commit != nil && appliedCommits[fact.commit]
		if (cut || taken >= in.batchSize) && !sameCommit {
			cut, more = true, true
			blocked[key] = true
			pending = append(pending, entry)
			continue
		}
		taken++
		if fact.commit != nil {
			appliedCommits[fact.commit] = true
		}
		completed = append(completed, fact)
		if gen, placed := in.placed[fact.id]; !placed || gen != fact.gen {
			// 目标在事实之后被撤除或重新放置（那条命令越过了还没确认的它）：之后的放置决定位置，这条作废。
			continue
		}
		if !batching {
			in.aoi.beginBatch()
			batching = true
		}
		var err error
		if fact.replaceCoverage {
			err = in.aoi.SetObservedBlocks(fact.id, fact.coverage)
		}
		if err == nil && !fact.coverageOnly {
			err = in.aoi.MoveSubject(fact.id, fact.at)
			if err == nil {
				in.moveFollowersLocked(fact.id, fact.at)
			}
		}
		if err != nil {
			failed = errors.Join(failed, fmt.Errorf("policy: dropped queued fact for %d: %w", fact.id, err))
			blocked[key] = true
		}
	}
	endBatch()
	r.mu.Lock()
	// 留下的条目必须仍在新登记的条目之前（未确认的前缀与切开处之后的部分）。
	arrived := len(r.log)
	r.log = append(pending, r.log...)
	waiting = false
	for _, entry := range r.log {
		if entry.op == nil {
			waiting = true
			break
		}
	}
	backlog := r.facts
	r.mu.Unlock()
	more = more || arrived > 0
	in.applyErr = errors.Join(in.applyErr, failed)
	if len(completed) > 0 {
		if len(in.applied) == 0 {
			in.appliedAt = time.Now()
		}
		in.applied = append(in.applied, completed...)
		progressed = true
	}
	// 异步模式：on_change 下让 Manager 尽快结算；periodic 下由下一个周期结算。同步模式已在政策阶段里，随即结算。
	if progressed && in.applyMode == ApplyAsync {
		in.wake()
	}
	metrics.SetGauge(factBacklogMetric, nil, int64(backlog))
	metrics.ObserveHistogram(applyDurationMetric, nil, time.Since(started))
	return progressed, more, waiting
}

// settleQueued 是 Manager Flush 的政策阶段。同步模式先在这里执行日志（一步接一步，直到没有可执行的条目或耗时预算
// 用完；剩余的留给下一次，on_change 下尽快再来一轮）；异步模式不执行（那是应用线程的事）。然后两种模式相同：把 AOI
// 事件与关系变更归并进 held 并说给 Manager，释放已应用事实的发布屏障（applyLocked），拒绝交给 OnRefusal，返回
// 应用时攒下的错误（无法应用而丢弃的事实，只让一次 Flush 失败）。从未登记过任何条目的 Interest 什么也不做。
func (in *Interest) settleQueued() error {
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.closed {
		return nil
	}
	in.registry.mu.Lock()
	active := in.registry.active
	in.registry.mu.Unlock()
	if !active {
		return nil
	}
	if in.applyMode == ApplySync {
		deadline := time.Now().Add(in.syncBudget)
		for {
			_, more, _ := in.runLogLocked()
			if !more {
				break
			}
			if !time.Now().Before(deadline) {
				in.wake()
				break
			}
		}
	}
	refusals := in.applyLocked()
	if in.onRefusal != nil {
		for _, refusal := range refusals {
			in.onRefusal(refusal)
		}
	}
	err := in.applyErr
	in.applyErr = nil
	return err
}

// releaseAppliedLocked 在结算之后释放已应用事实的发布屏障，并把它们移出预算。任何结算入口（Flush 的政策阶段、
// 显式 Apply）都调它，订阅先到、内容被屏障挡住的半态不会留到下一次捕获。调用方持有 mu。
func (in *Interest) releaseAppliedLocked() {
	if len(in.applied) == 0 {
		return
	}
	metrics.ObserveHistogram(settleDelayMetric, nil, time.Since(in.appliedAt))
	for _, fact := range in.applied {
		fact.finish()
	}
	in.registry.mu.Lock()
	in.registry.facts -= len(in.applied)
	backlog := in.registry.facts
	in.registry.mu.Unlock()
	clear(in.applied)
	in.applied = in.applied[:0]
	metrics.SetGauge(factBacklogMetric, nil, int64(backlog))
}

// queuedPending 告诉 Drain 政策还有没做完的事：日志里的条目、应用中与已应用未结算的事实，以及还没停滞的重试 pair。
func (in *Interest) queuedPending() bool {
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.closed {
		return false
	}
	r := &in.registry
	r.mu.Lock()
	pending := len(r.log) > 0 || r.facts > 0
	active := r.active
	r.mu.Unlock()
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
// 被拒的订阅留在 retry 里，每次结算重说，直到被接受或 pair 被释放（拒绝不是对 pair 的裁决）。
// 不能把整个 retry 计入 queuedPending：一个永远不会被接受的 pair（观察者会话已关闭且不再重开、subject 已注销
// 而政策没 Hide……）会让 Drain 永不结束，只能等 ctx 超时，停机走不到 Close。
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
// 日志里的事实（按登记顺序，同一 ID 只列一次）与命令，以及拒绝次数未到 retryStallAfter 的重试 pair。
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
	r := &in.registry
	r.mu.Lock()
	seen := make(map[int64]bool)
	queued, commands := 0, 0
	for _, entry := range r.log {
		if entry.op != nil {
			commands++
			continue
		}
		queued++
		fact := entry.fact
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
	if commands > 0 {
		add(entitysync.DrainStall{Source: "interest", Reason: "queued_commands", Detail: fmt.Sprintf("%d command(s) not yet applied", commands)})
	}
	if inflight := r.facts - queued; inflight > 0 && queued == 0 {
		add(entitysync.DrainStall{Source: "interest", Reason: "facts_in_flight", Detail: fmt.Sprintf("%d fact(s) being applied", inflight)})
	}
	r.mu.Unlock()
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
