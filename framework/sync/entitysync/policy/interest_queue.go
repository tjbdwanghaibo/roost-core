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
//   - 提交整笔取用：一笔提交在这一步的快照里只要有一条事实还在等（未确认，或被同一 (目标, 代际) 前面留下的事实
//     挡住），它的事实一条也不取用，整笔留在原位（waitingFacts）。
//   - 切点（纯前缀，方案 §11.2，维护者 2026-10-22 决定 A）：一步只在前缀边界停——切点之前的条目全部执行过（等着的
//     事实留在原位，同上），之后的全部留下。BatchSize 与同步模式的预算单位只决定在哪个边界停：到了上限之后，切点
//     落在第一个“已取用的提交全部结束”的位置。于是三条顺序规则同时成立：就绪事实不被它后面的命令越过（所以“已提交
//     的移动、再 Leave → Enter”与“先应用移动、再 Leave → Enter”逐项相同），命令不被它后面的事实越过，提交不切开。
//     代价：一步可以比上限多出交错进来的条目（已取用提交的后续事实之前、别的线程登记的命令与别的提交的事实，以及
//     为了让这些提交也结束而再往后取的条目），上界是这一步取到的日志快照。
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
	// 政策阶段（两种模式）：整次耗时、其中结算的耗时；同步模式：每次政策阶段结束时日志里剩下的条目数（事实 + 命令），
	// 因预算停下、留下可执行条目的次数。
	policyDurationMetric = "entitysync.interest.policy_duration"
	settleDurationMetric = "entitysync.interest.settle_duration"
	logBacklogMetric     = "entitysync.interest.log_backlog"
	budgetStopMetric     = "entitysync.interest.budget_stop.total"
)

// applyGatherFor 是应用线程两步之间的最短间隔：periodic 模式取同步周期的 1/10（20Hz 时 5ms），结算只在 Flush 里
// 发生，周期内再细的拆分只会让同一观察者被多次重评；on_change 取 OnChangeBatchWindow（缺省 0，不合批）。
func applyGatherFor(m *entitysync.Manager) time.Duration {
	if m.Mode() == entitysync.ModeOnChange {
		return m.OnChangeBatchWindow()
	}
	return m.Interval() / 10
}

// syncApplyBudgetFor 是同步模式一次政策阶段（执行日志 + 结算）的耗时预算：同步周期的一半（至少 1ms）。预计超出时
// 剩余的留到下一次政策阶段（on_change 下立即再来一轮），Flush 不会因为积压而无界变长。规则见 planSyncStep。
func syncApplyBudgetFor(m *entitysync.Manager) time.Duration {
	return max(m.Interval()/2, time.Millisecond)
}

// ---- 同步模式政策阶段的预算（REFACTOR-2026-10-21-interest-single-writer §12，维护者 2026-10-22 决定 A）----
//
// 一次政策阶段 = 若干步执行日志 + 一次结算，两者一起不超过预算。成本按“单位”估计：一个单位是一步里执行的一条命令
// 或取用的一条事实（被丢弃的事实不算）。
//   - 执行：每单位耗时 costs.unit = 最近一步的耗时 / 它的单位数；
//   - 结算：每单位耗时 costs.settle = 最近一次结算的耗时 / 它结算的单位数。结算的工作（AOI 事件归并、pair 说给
//     Manager、释放屏障）由这些单位产生，所以按单位摊而不是按一次结算的绝对值预留：绝对值预留在负载变化时会
//     在“多做一轮—留很大预留—只做很少”之间振荡（§12 的推演）。
//   - 结算预留 = max(预算 / 10, costs.settle × 已执行未结算的单位数)：下限覆盖结算的固定部分与还没测到时。
//
// 每一步之前：下一步最多 n = (预算 − 已用 − 结算预留) / (costs.unit + costs.settle) 个单位（到了 n 之后在第一个
// 前缀边界停，与 BatchSize 同一条切点规则，见 runLogLocked）。n ≥ 1 就执行这么多；n < 1 时停止，留给下一次——但
// 每次政策阶段的第一步总要执行（n 至少取 1）：不能饿死，代价是一个单位本身超预算时这一步会超出，超出量不超过一个
// 单位加上为了停在前缀边界而多执行的交错条目，再加结算。还没有任何估计时（启动后的第一步）不限单位数，只受
// BatchSize 限制。

// applyCosts 是同步模式政策阶段的成本估计（每单位执行 / 结算的耗时；0 表示还没测到）。只在应用侧（持 mu）读写。
type applyCosts struct {
	unit, settle time.Duration
}

// syncReserveFloorDivisor：结算预留的下限是预算的 1/10。
const syncReserveFloorDivisor = 10

// planSyncStep 决定政策阶段的下一步：run 为假时停止；limit 是这一步最多执行的单位数，0 表示不限（只受 BatchSize
// 限制）。elapsed 是本次政策阶段已用的时间，unsettled 是已执行未结算的单位数，first 表示本次政策阶段还没执行过。
func planSyncStep(budget, elapsed time.Duration, unsettled int, first bool, costs applyCosts) (limit int, run bool) {
	reserve := max(budget/syncReserveFloorDivisor, costs.settle*time.Duration(unsettled))
	remaining := budget - elapsed - reserve
	per := costs.unit + costs.settle
	if per <= 0 {
		return 0, first || remaining > 0
	}
	if remaining >= per {
		return int(remaining / per), true
	}
	if first {
		return 1, true
	}
	return 0, false
}

// stepTook / settleTook 用一步或一次结算的耗时更新每单位成本；没有执行单位（只完成了被丢弃的事实、只重试了
// 被拒的订阅）的不更新。
func (c *applyCosts) stepTook(took time.Duration, units int) {
	if units > 0 {
		c.unit = took / time.Duration(units)
	}
}

func (c *applyCosts) settleTook(took time.Duration, units int) {
	if units > 0 {
		c.settle = took / time.Duration(units)
	}
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
	return in.runLogLocked(0)
}

// drainLogLocked 一步接一步执行日志，直到没有可执行的条目（只剩未确认事实或日志空）。显式 Apply 与诊断读用它；
// 同步模式的政策阶段另有耗时预算（settleQueued）。调用方持有 mu。
func (in *Interest) drainLogLocked() {
	for {
		if _, more, _ := in.runLogLocked(0); !more {
			return
		}
	}
}

// runLogLocked 执行一步：取出日志，按序执行命令、把就绪事实应用到 AOI，应用完的事实记入 applied（发布屏障仍持有，
// 由下一次结算释放）。到了上限——取用的事实到达 BatchSize，或 unitLimit > 0 时执行的单位（命令与取用的事实）到达
// unitLimit——之后，在第一个前缀边界切开（见上文规则；0 不限单位，应用线程与显式 Apply 用）。执行的单位数累加到
// unsettledUnits。返回 progressed（执行了命令或完成了事实）、more（还有可执行的条目没取完）、waiting（日志里仍有
// 事实，可能在等提交确认）。调用方持有 mu。
func (in *Interest) runLogLocked(unitLimit int) (progressed, more, waiting bool) {
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
	// 同一 handler 的后半段并提交，导致本步只应用前半段。这里 ready 意味着该提交的全部事实已经在日志里——提交在
	// 全部事实登记之后才 Release，读到 ready 时它的登记都在本快照里，所以一笔可取用提交的结束位置就在快照之内。
	for i := range batch {
		if batch[i].op == nil {
			batch[i].fact.ready, batch[i].fact.discarded = batch[i].fact.condition()
		}
	}
	r.mu.Unlock()

	wait := waitingFacts(batch)
	// lastOf：每笔可取用提交在快照里最后一条事实的位置。取用了它的一条事实，切点就不能早于这里。
	lastOf := make(map[*entity.SyncMutation]int)
	for i := range batch {
		if fact := &batch[i].fact; batch[i].op == nil && fact.commit != nil && !fact.discarded && !wait[i] {
			lastOf[fact.commit] = i
		}
	}
	failedKeys := make(map[subjectGen]bool)
	pending := make([]logEntry, 0, len(batch))
	completed := make([]queuedInterestFact, 0, len(batch))
	var failed error
	// reach：已取用提交里最靠后的那条事实的位置；位置大于它的地方才是前缀边界。
	taken, units, reach, batching := 0, 0, -1, false
	endBatch := func() {
		if batching {
			in.aoi.endBatch(in.workers)
			batching = false
		}
	}
	for i, entry := range batch {
		if i > reach && (taken >= in.batchSize || (unitLimit > 0 && units >= unitLimit)) {
			// 纯前缀切开：之前的条目全部执行过，从这里起全部留下。
			pending = append(pending, batch[i:]...)
			for j := i; j < len(batch) && !more; j++ {
				more = batch[j].op != nil || batch[j].fact.discarded || !wait[j]
			}
			break
		}
		if entry.op != nil {
			endBatch()
			in.runOpLocked(entry.op)
			units++
			progressed = true
			continue
		}
		fact := entry.fact
		key := subjectGen{id: fact.id, gen: fact.gen}
		if fact.discarded {
			completed = append(completed, fact)
			continue
		}
		if wait[i] || failedKeys[key] {
			pending = append(pending, entry)
			continue
		}
		taken++
		units++
		if fact.commit != nil {
			reach = max(reach, lastOf[fact.commit])
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
			// 同一 (目标, 代际) 后面的事实这一步不再应用，留到下一步（错误路径：它所属的提交因此分两步应用）。
			failed = errors.Join(failed, fmt.Errorf("policy: dropped queued fact for %d: %w", fact.id, err))
			failedKeys[key] = true
		}
	}
	endBatch()
	r.mu.Lock()
	// 留下的条目必须仍在新登记的条目之前（等着的事实与切点之后的部分）。
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
	if progressed {
		in.unsettled = true
	}
	in.unsettledUnits += units
	// 异步模式：on_change 下让 Manager 尽快结算；periodic 下由下一个周期结算。同步模式已在政策阶段里，随即结算。
	if progressed && in.applyMode == ApplyAsync {
		in.wake()
	}
	metrics.SetGauge(factBacklogMetric, nil, int64(backlog))
	metrics.ObserveHistogram(applyDurationMetric, nil, time.Since(started))
	return progressed, more, waiting
}

// waitingFacts 标出这一步留在原位等的事实（与 batch 一一对应）：
//   - 未确认的；
//   - 同一 (目标, 代际) 前面有留下的事实的（不越过它）；
//   - 所属提交在快照里有一条这样的事实的：部分就绪的提交整笔不取用，不切成两半。
//
// 第三条会让同一提交更早的事实也留下，进而挡住别的提交排在后面的同一目标的事实，所以重复到没有新的提交被挡住为止：
// 每多一轮至少多挡住一笔提交，轮数不超过快照里的提交数加一（通常一轮，有连带时两轮）。被丢弃的事实不算（随时完成）。
// 不会因此等成死锁：事实就绪只取决于提交门（提交确认 / 拒绝），提交门从不等 Interest 应用；就绪是单调的，前面的
// 提交确认之后，被它连带挡住的提交在之后的一步里整笔可取用。
func waitingFacts(batch []logEntry) []bool {
	wait := make([]bool, len(batch))
	held := make(map[*entity.SyncMutation]bool)
	keys := make(map[subjectGen]bool)
	for changed := true; changed; {
		changed = false
		clear(keys)
		for i := range batch {
			fact := &batch[i].fact
			if batch[i].op != nil || fact.discarded {
				continue
			}
			key := subjectGen{id: fact.id, gen: fact.gen}
			wait[i] = !fact.ready || keys[key] || (fact.commit != nil && held[fact.commit])
			if !wait[i] {
				continue
			}
			keys[key] = true
			if fact.commit != nil && !held[fact.commit] {
				held[fact.commit] = true
				changed = true
			}
		}
	}
	return wait
}

// settleQueued 是 Manager Flush 的政策阶段。同步模式先在这里执行日志（一步接一步，直到没有可执行的条目或预计执行
// 下一步加上结算会超出预算，见 planSyncStep；剩余的留给下一次，on_change 下尽快再来一轮）；异步模式不执行（那是
// 应用线程的事）。然后两种模式相同：把 AOI
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
	started := in.now()
	settleFrom := started
	if in.applyMode == ApplySync {
		settleFrom = in.runSyncStepsLocked(started)
	}
	units := in.unsettledUnits
	refusals := in.applyLocked()
	if in.onRefusal != nil {
		for _, refusal := range refusals {
			in.onRefusal(refusal)
		}
	}
	finished := in.now()
	if in.applyMode == ApplySync {
		in.costs.settleTook(finished.Sub(settleFrom), units)
	}
	metrics.ObserveHistogram(settleDurationMetric, nil, finished.Sub(settleFrom))
	metrics.ObserveHistogram(policyDurationMetric, nil, finished.Sub(started))
	err := in.applyErr
	in.applyErr = nil
	return err
}

// runSyncStepsLocked 是同步模式政策阶段的执行部分：按 planSyncStep 一步接一步执行日志，直到没有可执行的条目或
// 预计执行下一步 + 结算会超出预算。预算停止时记 budgetStopMetric 并提醒 Manager（on_change 下尽快再来一轮）；
// 结束时记下日志里剩下的条目数。返回执行部分结束的时刻（结算从这里开始计时）。调用方持有 mu、不持 registry.mu。
func (in *Interest) runSyncStepsLocked(started time.Time) time.Time {
	now := started
	for first := true; ; first = false {
		limit, run := planSyncStep(in.syncBudget, now.Sub(started), in.unsettledUnits, first, in.costs)
		if !run {
			metrics.IncCounter(budgetStopMetric, nil, 1)
			in.wake()
			break
		}
		before := in.unsettledUnits
		_, more, _ := in.runLogLocked(limit)
		stepEnd := in.now()
		in.costs.stepTook(stepEnd.Sub(now), in.unsettledUnits-before)
		now = stepEnd
		if !more {
			break
		}
	}
	in.registry.mu.Lock()
	left := len(in.registry.log)
	in.registry.mu.Unlock()
	metrics.SetGauge(logBacklogMetric, nil, int64(left))
	return now
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

// queuedPending 告诉 Drain 政策还有没做完的事：日志里的条目、应用中与已应用未结算的事实、应用方已执行但还没结算的
// 条目（unsettled），以及还没停滞的重试 pair。
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
	if pending || in.unsettled {
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
	if in.unsettled && r.facts == 0 {
		add(entitysync.DrainStall{Source: "interest", Reason: "applied_unsettled", Detail: "applied commands wait for the next settle"})
	}
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
