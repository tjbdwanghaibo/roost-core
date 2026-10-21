package nest

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/infra/base/fctx"
	"github.com/tjbdwanghaibo/roost-core/infra/base/goroutine"
	"github.com/tjbdwanghaibo/roost-core/infra/base/worker"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
)

// 批量作业（DESIGN-TODO D20 选项 C，设计见 docs/maintenance/FEATURE-2026-10-21-nest-batch-heartbeat.md）：一个 Nest
// 作业带一批互不相干的单目标投递，worker 逐个目标在该目标自己的 Guard 作用域与事务里执行。为心跳设计：按批付请求
// 上下文、当前消息登记、准入与指标，逐目标付事务与实体锁。
//
// 同 ID 顺序（维护者 2026-10-21 的硬约束）：
//   - 准入时不登记同 ID 顺序；执行到某个目标时，若该 ID 有任何已准入、尚未完成的作业（q.tails 有登记，含尚未开始的），
//     或实体锁 / 锁组取不到，就在当场把这个目标准入成一条单目标作业（与 Dispatch 相同的 admitLocked），排在那条
//     冲突作业之后执行——不交回调用方等下一轮，也不在 worker 上等；
//   - 没有冲突时先在 q.mu 下把目标“认领”为该 ID 的尾作业，再 TryRequire 实体锁、执行；执行期间准入的同 ID 作业
//     排在认领后面（等在依赖队列里，不占 worker、不阻塞在实体锁上），目标结束后撤销认领、唤醒后继。

// MaxTryBatchTargets 是一次 DispatchTryBatch 的目标数上限。
const MaxTryBatchTargets = 1024

// tryBatchGroup 是一次持 q.mu 安排（认领 / 转单目标 / 交回）的目标数上限：一组执行完后在同一次持锁里撤销本组
// 认领并安排下一组，把逐目标的队列锁摊到一组上。代价是认领了还没轮到执行的目标上，新准入的同 ID 消息最多多等本组
// 其余目标的执行时间（等在依赖队列里，不占 worker）。
const tryBatchGroup = 8

// tryBatchSlice 是一个 worker 连续执行同一批量作业的时间片：每个目标结束后检查，超过且还有剩余目标时把作业放回
// 就绪链尾，让链上的普通作业先走（见设计 §5）。
const tryBatchSlice = 200 * time.Microsecond

var (
	// ErrTryBatchUnsupportedTarget 表示目标不能放进批量作业：Remote 托管实体每次写都要在 I/O 池取得带 fence 的写批次，
	// 批量作业只在业务池执行。调用方改用单目标 Dispatch。
	ErrTryBatchUnsupportedTarget = errors.New("nest: target cannot run in a try-batch job (Remote-managed entity); use Dispatch")
	// errTryBatchAborted 是批量作业的执行循环意外退出（框架缺陷）时剩余目标的交回原因。
	errTryBatchAborted = errors.New("nest: try-batch job aborted before reaching this target")
	// errTryBatchFenced 是在 q.mu 下发现引擎已 fence 时的占位原因；交回时换成 FenceError（不能持 q.mu 取 lifecycleMu）。
	errTryBatchFenced = errors.New("nest: engine fenced")
)

// TryBatchTarget 是批量作业里的一个目标：在 ID 对应实体的 Guard 作用域与事务里执行一次 Handler(Params)。
type TryBatchTarget struct {
	Handler HandlerName
	ID      int64
	Params  Params
}

// TryBatchDispatcher 是批量投递能力（*NestMgr 实现）。不在 Client 接口里：测试替身、包装客户端可以不实现，调用方
// 据此退回单目标 Dispatch。
type TryBatchDispatcher interface {
	DispatchTryBatch(ctx context.Context, targets []TryBatchTarget, handBack func(TryBatchTarget, error)) error
}

var _ TryBatchDispatcher = (*NestMgr)(nil)

// DispatchTryBatch 把一批互不相干的单目标投递交给批量作业：按目标的业务池分成至多两个作业（short / long），各占
// 一个准入名额。返回 nil 表示已准入；返回错误（引擎停止 / fence、ctx 已取消、在 handler 内调用、参数非法）时
// 没有任何目标被准入，也不调用 handBack。
//
// 每个目标的结局恰好是三者之一：
//   - 在本批里执行了 handler（提交或回滚，错误按异步消息记日志）；
//   - 同 ID 冲突，转成一条单目标作业排在冲突作业之后执行（语义与 Dispatch 相同，计 nest.dispatch.try_batch.converted.total）；
//   - 没有执行，handBack(target, 原因) 恰好调用一次：ErrInvalidMessage（ID / handler 名非法）、
//     ErrTryBatchUnsupportedTarget（Remote 托管）、ErrQueueFull / ErrNestStopped（准入或冲突转单目标被拒）、
//     ErrEntityNotFound（目标不在内存，批量作业不冷加载）、ErrNestFenced、ErrNestCanceled、ErrHandlerNotFound。
//
// handBack 可能在调用方 goroutine 上同步调用（准入前即可判定的原因），也可能在 Nest worker 上调用：必须不阻塞、
// 不调用 Nest。Params 的所有权规则与 Dispatch 相同（准入时复制切片，值视为不可再改）。
func (mgr *NestMgr) DispatchTryBatch(ctx context.Context, targets []TryBatchTarget, handBack func(TryBatchTarget, error)) error {
	if len(targets) == 0 {
		return nil
	}
	if len(targets) > MaxTryBatchTargets {
		return fmt.Errorf("%w: %d targets exceed the try-batch limit %d", ErrInvalidMessage, len(targets), MaxTryBatchTargets)
	}
	if handBack == nil {
		return fmt.Errorf("%w: try-batch needs a hand-back callback", ErrInvalidMessage)
	}
	if err := validateClientDispatch(mgr, ctx, "DispatchTryBatch", tryBatchAPIName, ErrAsyncInHandler); err != nil {
		return err
	}
	snapshot := asyncMessageContextSnapshot(fctx.CaptureSnapshot())
	var jobs [dispatchLaneCount]*tryBatchJob
	for _, target := range targets {
		if target.Handler.String() == "" {
			handBack(target, fmt.Errorf("%w: empty handler name", ErrInvalidMessage))
			continue
		}
		fullID, err := normalizeClientID(target.ID)
		if err != nil {
			handBack(target, err)
			continue
		}
		meta := entity.ResolveEntityID(fullID)
		if shouldPrepareRemoteID(meta) {
			handBack(target, ErrTryBatchUnsupportedTarget)
			continue
		}
		lane := laneOfID(fullID)
		b := jobs[lane]
		if b == nil {
			b = newTryBatchJob(mgr, lane, handBack, snapshot)
			jobs[lane] = b
		}
		b.add(target, fullID, meta.Category)
	}
	for _, b := range jobs {
		if b == nil {
			continue
		}
		if err := mgr.dispatcher.trySendTryBatch(b); err != nil {
			err = normalizeAdmissionError(err)
			for i := range b.items {
				handBack(b.items[i].target, err)
			}
			b.recycle()
		}
	}
	return nil
}

// tryBatchAPIName 只用于前置检查与错误文本（validateClientDispatch 要求非空的 handler 名）。
var tryBatchAPIName = NewHandlerName("nest.try_batch")

// laneOfID 是单个目标的业务池，规则与 businessLane 相同。
func laneOfID(id int64) int {
	if entity.EntityBusinessPoolOfKind(entity.ResolveEntityID(id).Kind) == entity.BusinessPoolLong {
		return dispatchLongLane
	}
	return dispatchShortLane
}

// tryBatchItem 是批量作业里已规范化的一个目标；target.Params 指向作业自己的参数存储。
type tryBatchItem struct {
	target   TryBatchTarget
	id       int64
	category entity.EntityCategory
}

// tryBatchJob 是一个批量作业：作业本体、认领标记、目标与执行进度。准入后归派发队列所有，结束后归还池。
type tryBatchJob struct {
	job dispatchJob
	// group 是当前一组目标（b.next 起至多 tryBatchGroup 个）的安排；claims[i] 是 group[i] 的认领标记：执行期间临时
	// 登记为该 ID 的尾作业（q.tails[id] = &claims[i]），之后准入的同 ID 作业对它建依赖边。由持有作业的 worker 在
	// q.mu 下写入与撤销。
	group  [tryBatchGroup]tryBatchPlan
	claims [tryBatchGroup]dispatchJob
	groupN int

	mgr      *NestMgr
	lane     int
	handBack func(TryBatchTarget, error)
	context  fctx.ContextSnapshot
	items    []tryBatchItem
	params   []any // 各目标 Params 的复制存储，items[i].target.Params 是它的子切片
	// next 是下一个要处理的目标下标：时间片放回就绪链后，下一个取到作业的 worker 从这里接着。只由持有作业的 worker 读写。
	next int
}

var tryBatchPool = sync.Pool{New: func() any { return &tryBatchJob{} }}

func newTryBatchJob(mgr *NestMgr, lane int, handBack func(TryBatchTarget, error), snapshot fctx.ContextSnapshot) *tryBatchJob {
	b := tryBatchPool.Get().(*tryBatchJob)
	b.mgr, b.lane, b.handBack, b.context = mgr, lane, handBack, snapshot
	b.job = dispatchJob{tryBatch: b, lane: lane}
	return b
}

func (b *tryBatchJob) add(target TryBatchTarget, id int64, category entity.EntityCategory) {
	start := len(b.params)
	b.params = append(b.params, target.Params...)
	target.ID = id
	target.Params = b.params[start:len(b.params):len(b.params)]
	b.items = append(b.items, tryBatchItem{target: target, id: id, category: category})
}

// recycle 清空作业并归还池；保留目标与参数存储的容量。
func (b *tryBatchJob) recycle() {
	clear(b.items)
	clear(b.params)
	items, params := b.items[:0], b.params[:0]
	var successors [tryBatchGroup][]*dispatchJob
	for i := range b.claims {
		successors[i] = b.claims[i].successors[:0]
	}
	*b = tryBatchJob{items: items, params: params}
	for i := range b.claims {
		b.claims[i].successors = successors[i]
	}
	tryBatchPool.Put(b)
}

// callHandBack 交回一个没有执行的目标；回调的 panic 只记日志，不影响同批的其他目标。不持 q.mu 调用。
func (b *tryBatchJob) callHandBack(target TryBatchTarget, reason error) {
	if errors.Is(reason, errTryBatchFenced) {
		reason = b.mgr.FenceError()
	}
	defer func() {
		if r := recover(); r != nil {
			slog.Error("nest try-batch hand-back callback panic", "handler", target.Handler.String(), "id", target.ID, "err", r)
		}
	}()
	b.handBack(target, reason)
}

// trySendTryBatch 是批量作业的准入：引擎停止 / fence 时拒绝，否则交给派发队列（占一个准入名额，不登记同 ID 顺序）。
func (m *Dispatcher) trySendTryBatch(b *tryBatchJob) error {
	m.mu.Lock()
	stopped, fenced := m.stopped, m.fenceErr
	m.mu.Unlock()
	if fenced != nil {
		return fenced
	}
	if stopped {
		return ErrNestStopped
	}
	err := m.queue.admitTryBatch(b)
	m.observeStatsIfDue()
	return err
}

// admitTryBatch 准入一个批量作业：容量判定与普通作业相同（有空闲执行额度直接接受，否则看等待容量），不写 tails、
// 不建依赖边，直接进通道就绪链尾。
func (q *dispatchQueue) admitTryBatch(b *tryBatchJob) error {
	if q == nil {
		return worker.ErrWorkerClosed
	}
	j := &b.job
	state := &q.lanes[j.lane]
	q.mu.Lock()
	defer q.mu.Unlock()
	if !q.started || q.stopping {
		return worker.ErrWorkerClosed
	}
	hasExecutionSlot := q.busyWorkers(j.lane, state)+state.readyCount < state.config.Workers
	if !hasExecutionSlot && q.waitingCount(j.lane, state) >= state.config.QueueCap {
		state.observation.Rejected++
		return worker.ErrWorkerQueueFull
	}
	q.pending++
	state.queued++
	state.observation.TryBatchJobs++
	j.admittedAt = time.Since(q.clockOrigin)
	state.waiting.push(j)
	q.enqueueReady(j.lane, j)
	state.observation.PeakWaiting = max(state.observation.PeakWaiting, q.waitingCount(j.lane, state))
	return nil
}

// tryBatchStep 是 q.mu 下对一个目标做出的安排。
type tryBatchStep int

const (
	tryBatchRun       tryBatchStep = iota // 已认领，执行
	tryBatchConverted                     // 同 ID 冲突，已转成单目标作业
	tryBatchHandBack                      // 不执行，交回
)

// tryBatchPlan 是组内一个目标的安排与结果。claimed 由 q.mu 保护；busy / handBack 由执行它的 worker 写。
type tryBatchPlan struct {
	step     tryBatchStep
	claimed  bool
	busy     bool  // 执行时实体锁 / 锁组取不到：撤销认领前转成单目标作业
	handBack error // 非 nil 时交回
}

// prepareTryBatchGroupLocked 为 b.next 起至多 tryBatchGroup 个目标做安排：引擎已 fence 则交回；该 ID 有已准入、尚未
// 完成的作业（q.tails 有登记）则当场转成单目标作业排在它后面（约束 1）；否则认领为该 ID 的尾作业（约束 2）。
// 遇到本组已认领的同一个 ID（同一实体的两个登记）时本组到此为止，留给下一组，免得它被当成冲突。调用方持有 q.mu。
func (q *dispatchQueue) prepareTryBatchGroupLocked(b *tryBatchJob) {
	fenced := b.mgr.fenced.Load()
	n := 0
	for ; n < tryBatchGroup && b.next+n < len(b.items); n++ {
		item := &b.items[b.next+n]
		plan := &b.group[n]
		*plan = tryBatchPlan{}
		switch tail := q.tails[item.id]; {
		case fenced:
			plan.step, plan.handBack = tryBatchHandBack, errTryBatchFenced
		case tail != nil && b.ownsClaim(tail):
			b.groupN = n
			return
		case tail != nil:
			if err := q.convertTryBatchTargetLocked(b, item); err != nil {
				plan.step, plan.handBack = tryBatchHandBack, err
			} else {
				plan.step = tryBatchConverted
			}
		default:
			claim := &b.claims[n]
			claim.inlineID[0] = item.id
			claim.ids = claim.inlineID[:]
			claim.lane = b.lane
			q.tails[item.id] = claim
			plan.step, plan.claimed = tryBatchRun, true
		}
	}
	b.groupN = n
}

// ownsClaim 报告 tail 是不是本作业当前组里的一个认领标记。
func (b *tryBatchJob) ownsClaim(tail *dispatchJob) bool {
	for i := range b.claims {
		if tail == &b.claims[i] {
			return true
		}
	}
	return false
}

// releaseTryBatchClaimsLocked 撤销本组仍登记着的认领，认领期间挂上来的后继就绪（与作业结束相同的 releaseIDsLocked）。
// 调用方持有 q.mu。
func (q *dispatchQueue) releaseTryBatchClaimsLocked(b *tryBatchJob) {
	for i := range b.groupN {
		plan := &b.group[i]
		if !plan.claimed {
			continue
		}
		claim := &b.claims[i]
		q.releaseIDsLocked(claim)
		clear(claim.successors)
		claim.successors = claim.successors[:0]
		plan.claimed = false
	}
}

// finishTryBatchGroupLocked 收尾一组：执行时取不到锁的目标转成单目标作业（先挂在自己的认领后面），再撤销本组全部
// 认领，认领期间挂上来的后继就绪（与作业结束相同的 releaseIDsLocked）。调用方持有 q.mu。
func (q *dispatchQueue) finishTryBatchGroupLocked(b *tryBatchJob, start int) (converted int) {
	for i := range b.groupN {
		plan := &b.group[i]
		if plan.busy {
			if err := q.convertTryBatchTargetLocked(b, &b.items[start+i]); err != nil {
				plan.handBack = err
			} else {
				converted++
			}
		}
	}
	q.releaseTryBatchClaimsLocked(b)
	return converted
}

// convertTryBatchTargetLocked 把冲突的目标准入成一条单目标作业：消息与 Dispatch 建的相同（MsgTypeSingle、同一
// handler 与参数、批的上下文快照），走 admitLocked 登记到该 ID 的 tails 后面，所以紧跟在冲突作业之后执行。
// 不阻塞：容量不足返回 ErrWorkerQueueFull，队列停止中返回 ErrWorkerClosed，调用方据此交回（算跳拍）。
// 调用方持有 q.mu。
func (q *dispatchQueue) convertTryBatchTargetLocked(b *tryBatchJob, item *tryBatchItem) error {
	if q.stopping {
		return worker.ErrWorkerClosed
	}
	msg := GenMsg(MsgTypeSingle)
	msg.Tid = item.id
	msg.Name = item.target.Handler.String()
	msg.Params = append(msg.Params[:0], item.target.Params...)
	msg.Context = b.context.Clone()
	if d := b.mgr.dispatcher; d != nil {
		msg.stageMetrics = d.stageMetrics
		msg.queuedAt = startNestStage(d.stageMetrics)
	}
	msg.OnSend()
	j := newDispatchJob(msg, false)
	j.lane = b.lane
	if err := q.admitLocked(j); err != nil {
		recycleMsg(msg)
		return err
	}
	q.lanes[b.lane].observation.TryBatchConverted++
	return nil
}

// yieldTryBatchLocked 把用完时间片的批量作业放回通道就绪链尾：归还执行额度、重新计入等待与就绪，pending 不变。
// 调用方持有 q.mu。
func (q *dispatchQueue) yieldTryBatchLocked(lane int, state *dispatchLane, j *dispatchJob) {
	state.running--
	state.queued++
	j.waitedForPredecessor = false
	state.waiting.push(j)
	q.enqueueReady(lane, j)
}

// runTryBatchLocked 执行一个批量作业，从 b.next 接着。调用时与返回时都持有 q.mu；返回时作业已结束（finishJobLocked）
// 或已放回就绪链。按组推进：q.mu 下安排一组（交回 / 转单目标 / 认领）→ 不持锁逐个执行已认领的 → q.mu 下收尾本组
// （取锁失败的转单目标、撤销认领）并安排下一组 → 不持锁交回。
func (q *dispatchQueue) runTryBatchLocked(lane int, state *dispatchLane, j *dispatchJob, contextOptions []fctx.Option) {
	b := j.tryBatch
	q.prepareTryBatchGroupLocked(b)
	q.mu.Unlock()
	yield := false
	goroutine.SafeFunc(func() {
		_, release := fctx.NewContext(contextOptions...)
		defer release()
		exec := b.mgr.beginTryBatch(b)
		defer exec.end()
		for {
			start, n := b.next, b.groupN
			for i := range n {
				plan := &b.group[i]
				switch plan.step {
				case tryBatchRun:
					plan.busy, plan.handBack = exec.run(&b.items[start+i])
					exec.markItemDone()
				case tryBatchConverted:
					exec.converted++
				}
			}
			b.next += n
			now := time.Now()
			q.mu.Lock()
			exec.converted += q.finishTryBatchGroupLocked(b, start)
			done := b.next >= len(b.items)
			if !done {
				if now.Sub(exec.start) >= tryBatchSlice {
					yield = true
				} else {
					// 下一组的安排写进 b.group，先把本组的交回取出来。
					exec.collectHandBacks(b, start, n)
					q.prepareTryBatchGroupLocked(b)
					q.mu.Unlock()
					exec.flushHandBacks(b)
					continue
				}
			}
			exec.collectHandBacks(b, start, n)
			q.mu.Unlock()
			exec.flushHandBacks(b)
			return
		}
	})
	if !yield {
		// 执行循环只会在全部目标处理完或让出时退出；意外退出（框架缺陷）时剩余目标逐个交回，不让调用方的在途状态卡住。
		for b.next < len(b.items) {
			b.callHandBack(b.items[b.next].target, errTryBatchAborted)
			b.next++
		}
	}
	q.mu.Lock()
	q.releaseTryBatchClaimsLocked(b) // 正常退出时本组认领已撤销；只在执行循环意外退出时还有残留
	if yield {
		q.yieldTryBatchLocked(lane, state, j)
		return
	}
	q.finishJobLocked(state, j)
	b.recycle()
}

// tryBatchExec 是一个 worker 执行一段批量作业（一个时间片）期间按批付一次的状态：请求上下文、当前消息登记、
// 开始时间、引擎依赖与计数。逐目标复用同一个消息对象（运行状态逐目标清零），所以当前消息只登记一次。
type tryBatchExec struct {
	mgr        *NestMgr
	msg        *Msg
	getter     entity.Getter
	locks      *entityLockGroupLockManager
	store      entityGroupStore
	releaseCtx func()
	releaseMsg func()
	// ctx / ctxSnapshot：本段的请求上下文与它建立时的快照。每个目标开始前恢复快照，前一个目标写进上下文的 KV、
	// Meta、Base 不带给下一个；请求时间 Now 不在快照里，整段共用（按批的时间快照）。
	ctx         *fctx.Context
	ctxSnapshot fctx.ContextSnapshot
	start       time.Time
	itemStart   time.Time
	// lastHandler / lastEntry 缓存上一个目标的 handler 表项：一批里通常只有一两个 handler。
	lastHandler HandlerName
	lastEntry   handlerEntry
	lastFound   bool
	executed    int
	failed      int
	converted   int
	handedBack  int
	// returned 是本组要交回的目标，持 q.mu 时收集、放锁后调用 handBack。
	returned []tryBatchReturned
}

type tryBatchReturned struct {
	target TryBatchTarget
	reason error
}

// collectHandBacks 收集组内要交回的目标（持 q.mu 时调用）。
func (exec *tryBatchExec) collectHandBacks(b *tryBatchJob, start, n int) {
	for i := range n {
		if err := b.group[i].handBack; err != nil {
			exec.returned = append(exec.returned, tryBatchReturned{target: b.items[start+i].target, reason: normalizeAdmissionError(err)})
		}
	}
}

// flushHandBacks 不持 q.mu 调用收集到的交回。
func (exec *tryBatchExec) flushHandBacks(b *tryBatchJob) {
	for i, r := range exec.returned {
		exec.handedBack++
		b.callHandBack(r.target, r.reason)
		exec.returned[i] = tryBatchReturned{}
	}
	exec.returned = exec.returned[:0]
}

// beginTryBatch 建立本段的请求上下文（批的上下文快照与当前帧号）并登记当前消息。
func (mgr *NestMgr) beginTryBatch(b *tryBatchJob) *tryBatchExec {
	exec := &tryBatchExec{mgr: mgr, getter: loadedGetter{mgr.getter}, locks: mgr.groupLockManager(), store: groupStoreOf(mgr.getter)}
	exec.msg = GenMsg(MsgTypeTryBatch)
	exec.msg.Context = b.context
	exec.releaseCtx = ensureNestContext(mgr, exec.msg)
	exec.msg.Context = fctx.ContextSnapshot{}
	exec.ctx = fctx.CurrentContext()
	exec.ctxSnapshot = exec.ctx.Snapshot()
	exec.resetMsg(nil)
	exec.releaseMsg = pushCurrentNestDispatchMsg(exec.msg)
	exec.start = time.Now()
	exec.itemStart = exec.start
	return exec
}

// resetMsg 清零消息上一个目标留下的运行状态（提交事实、解锁后回调、Await 计划等），换成本目标的请求数据。
// 指针不变，当前消息登记继续有效。
func (exec *tryBatchExec) resetMsg(item *tryBatchItem) {
	*exec.msg = Msg{Type: MsgTypeTryBatch, getter: exec.getter, engine: exec.mgr}
	if item != nil {
		exec.ctx.ApplySnapshot(exec.ctxSnapshot)
		exec.msg.Name = item.target.Handler.String()
		exec.msg.Tid = item.id
		exec.msg.Params = item.target.Params
	}
}

// markItemDone 记下一个执行过的目标结束的时刻（也是下一个目标的开始）。
func (exec *tryBatchExec) markItemDone() {
	exec.itemStart = time.Now()
}

// end 撤销当前消息登记与请求上下文，按批记指标。
func (exec *tryBatchExec) end() {
	exec.releaseMsg()
	exec.releaseCtx()
	recycleMsg(exec.msg)
	exec.msg = nil
	mgr := exec.mgr
	name := ""
	if mgr.dispatcher != nil {
		name = mgr.dispatcher.Name
		mgr.dispatcher.processed.Add(uint64(exec.executed))
	}
	labels := metrics.Labels{"dispatcher": name}
	metrics.ObserveDuration("nest.dispatch.try_batch.cost", labels, time.Since(exec.start))
	if exec.executed > 0 {
		metrics.IncCounter("nest.dispatch.try_batch.executed.total", labels, int64(exec.executed))
	}
	if exec.failed > 0 {
		metrics.IncCounter("nest.dispatch.try_batch.failed.total", labels, int64(exec.failed))
	}
	if exec.converted > 0 {
		metrics.IncCounter("nest.dispatch.try_batch.converted.total", labels, int64(exec.converted))
	}
	if exec.handedBack > 0 {
		metrics.IncCounter("nest.dispatch.try_batch.handed_back.total", labels, int64(exec.handedBack))
	}
}

// entry 查 handler 表项，连续同名时用缓存。
func (exec *tryBatchExec) entry(name HandlerName) (handlerEntry, bool) {
	if !exec.lastFound || exec.lastHandler != name {
		exec.lastEntry, exec.lastFound = exec.mgr.getHandlerEntry(name)
		exec.lastFound = exec.lastFound && exec.lastEntry.handler != nil
		exec.lastHandler = name
	}
	return exec.lastEntry, exec.lastFound
}

// run 执行一个已认领的目标。busy 为 true 表示实体锁 / 锁组取不到或实体处于锁组迁移待定，调用方把它转成单目标作业；
// handBack 非 nil 表示不执行、交回；两者都为零值表示 handler 已执行（提交或回滚，错误已记日志）。
//
// 步骤与单目标派发相同（singleDispatch → dispatchLoadedEntities → invokeHandlerTransaction），区别：只读已加载的
// 实体（冷目标交回，不冷加载）；实体锁只 Try（快池不等锁）；pipelined 在锁内提交（不提前放锁，与广播相同）；
// 不逐目标观察持锁时长直方图，只计超过阈值的。
func (exec *tryBatchExec) run(item *tryBatchItem) (busy bool, handBack error) {
	exec.resetMsg(item)
	defer exec.resetMsg(nil)
	if cause := nestBaseContext().Err(); cause != nil {
		return false, errors.Join(ErrNestCanceled, cause)
	}
	entry, ok := exec.entry(item.target.Handler)
	if !ok {
		return false, ErrHandlerNotFound
	}
	e, err := exec.getter.Get(nestBaseContext(), item.id, item.category)
	if err != nil {
		return false, fmt.Errorf("%w: %w", ErrEntityNotFound, err)
	}
	if e == nil || !e.Touch() {
		return false, ErrEntityNotFound
	}
	defer e.UnTouch()
	if rejectPendingEntityGroupTransition([]entity.IThreadSafeEntity{e}) != nil {
		return true, nil
	}
	return exec.runLocked(entry, e, item)
}

// runLocked 在目标自己的 Guard 作用域里 Try 取锁、执行事务、释放。handler 与收尾的 panic 只影响本目标。
func (exec *tryBatchExec) runLocked(entry handlerEntry, e entity.IThreadSafeEntity, item *tryBatchItem) (busy bool, handBack error) {
	name := exec.msg.Name
	scope, releaseScope := entity.NewGuardScope("nest:try_batch")
	es := []entity.IThreadSafeEntity{e}
	var err error
	locked := false
	func() {
		defer func() {
			if r := recover(); r != nil {
				err = joinRecoveredError(err, r)
			}
		}()
		defer func() {
			releaseScope()
			exec.msg.runAfterUnlock()
		}()
		releaseLocks, ok := tryLockTryBatchTarget(exec.locks, scope.Guard(), e, exec.store)
		if !ok {
			return
		}
		locked = true
		defer func() {
			if r := recoverRelease(releaseLocks); r != nil {
				err = joinRecoveredError(err, r)
			}
		}()
		msg := exec.msg
		_, err = exec.mgr.invokeTryBatchTransaction(entry.meta, es, name, func() (any, error) {
			msg.handlerEntities = es
			defer func() { msg.handlerEntities = nil }()
			return entry.handler(es, item.target.Params)
		})
	}()
	if !locked {
		if err != nil {
			return false, err // 取锁本身 panic（框架缺陷）：不执行，交回
		}
		return true, nil
	}
	if plan := exec.msg.awaitPlan; plan != nil {
		// Await 在批量作业里被拒绝（ErrAwaitContext），这里只防御：计划不启动。
		exec.msg.awaitPlan = nil
		plan.cancel()
	}
	exec.executed++
	if hold := time.Since(exec.itemStart); exec.mgr.slowLockThreshold > 0 && hold >= exec.mgr.slowLockThreshold {
		exec.mgr.observeLockHold(name, hold)
	}
	if err != nil {
		exec.failed++
		logAsyncDispatchError(exec.msg, err)
	}
	return false, nil
}

// invokeTryBatchTransaction 是批量作业里一个目标的事务入口：与 invokeHandlerTransaction 相同的 pipelined 白名单检查、
// 认领消息的事务、结果未知时 fence 引擎；不传提前放锁闭包（pipelined 按锁内提交），不记持锁时长直方图。
func (mgr *NestMgr) invokeTryBatchTransaction(meta HandlerMeta, es []entity.IThreadSafeEntity, name string, call func() (any, error)) (any, error) {
	if meta.Durability == DurabilityPipelined && mgr.pipelinedAllow != nil {
		if _, ok := mgr.pipelinedAllow[name]; !ok {
			return nil, fmt.Errorf("%w: %q", ErrPipelinedNotAllowed, name)
		}
	}
	ret, err := invokeWithTransaction(meta, es, mgr.committer, name, nil, nil, call, mgr.entitySync)
	if errors.Is(err, ErrCommitIndeterminate) {
		mgr.Fence(err)
	}
	return ret, err
}

// tryLockTryBatchTarget 只 Try 取目标的实体锁（有锁组时组锁同样 Try，沿用 lockDispatchEntitiesWithGroup），取锁后核对
// 锁组没有变化。取不到返回 false，不重试、不等待。
func tryLockTryBatchTarget(locks *entityLockGroupLockManager, guard *entity.EntityGuard, e entity.IThreadSafeEntity, store entityGroupStore) (func(), bool) {
	lockEs := []entity.IThreadSafeEntity{e}
	var storage [1]dispatchGroupSnapshot
	snapshots := captureDispatchGroupSnapshots(lockEs, storage[:0])
	groupID, err := resolveDispatchGroupIDFromSnapshots(snapshots)
	if err != nil {
		return nil, false
	}
	var release func()
	if groupID == 0 {
		if !guard.TryRequireEntity(e) {
			return nil, false
		}
		release = func() { guard.ReleaseEntityInstance(e) }
	} else {
		_, release, err = lockDispatchEntitiesWithGroup(locks, guard, lockEs, groupID, store)
		if err != nil {
			return nil, false
		}
	}
	if validateDispatchGroupSnapshots(snapshots) != nil {
		release()
		return nil, false
	}
	return release, true
}
