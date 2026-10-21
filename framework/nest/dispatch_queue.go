package nest

import (
	"context"
	"slices"
	"sync"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/infra/base/fctx"
	"github.com/tjbdwanghaibo/roost-core/infra/base/goroutine"
	"github.com/tjbdwanghaibo/roost-core/infra/base/worker"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
)

// DefaultShortQueueCapacity 是短业务池共享等待容量的缺省值，不随 worker 数倍增。
const DefaultShortQueueCapacity = 65536

// WorkerPoolConfig 配置整个池的等待预算，QueueCap 不再按 worker 倍增。
type WorkerPoolConfig struct{ Workers, QueueCap int }

// 三个执行通道：short 业务池、io 准备池、long 业务池（按实体 kind 注册的业务池分流）。
const (
	dispatchShortLane = iota
	dispatchIOLane
	dispatchLongLane
	dispatchLaneCount
)

// dispatchJob 的时间是相对所属队列 clockOrigin 的单调偏移，不存储墙钟时间。
// 指针、切片集中在前部，标志集中在末尾，减少填充及每个排队对象的体积。
// ID 和依赖计数保持原类型，不以窄整数限制多实体请求。
type dispatchJob struct {
	waitingPrev, waitingNext *dispatchJob
	msg                      *Msg
	ids                      []int64
	successors               []*dispatchJob
	next                     *dispatchJob
	inlineID                 [1]int64 // 单目标直接存在job内；ids只读引用，不交给外部函数填充。
	admittedAt, readyAt      time.Duration
	predecessors             int
	lane                     int
	ioStage                  bool
	continuation             bool
	waitedForPredecessor     bool
}

// readyJobs 是经 dispatchJob.next 串起的 FIFO 单链表。
type readyJobs struct{ head, tail *dispatchJob }

func (q *readyJobs) push(j *dispatchJob) {
	if q.tail == nil {
		q.head = j
	} else {
		q.tail.next = j
	}
	q.tail = j
}

func (q *readyJobs) pop() *dispatchJob {
	j := q.head
	if j != nil {
		q.head = j.next
		j.next = nil
		if q.head == nil {
			q.tail = nil
		}
	}
	return j
}

// dispatchLane 聚合同一执行通道的状态，避免多组数组靠下标隐式对应。
// 可变状态由所属 dispatchQueue.mu 保护；wake 也绑定这把共享锁。
// 不得按值复制正在使用的通道，修改时通过 q.lanes 中的原项访问。
type dispatchLane struct {
	continuations                          readyJobs
	continuationCount, continuationRunning int
	preferContinuation                     bool
	wake                                   *sync.Cond
	config                                 WorkerPoolConfig
	handler                                func(*Msg)
	ready                                  readyJobs
	waiting                                readyWaiting
	queued                                 int // 尚未开始执行的外部请求，包括 ID 前驱等待。
	readyCount                             int // 其中已经满足 ID 依赖的请求。
	running                                int // 正在执行的外部请求；内部延续单独预算。
	observation                            DispatchLaneStats
}

// dispatchQueue 先登记 ID 顺序，再交给三种执行资源。I/O 池没有 ID 分槽；
// 等待前驱只占准入预算，不占任何 worker。内部业务阶段继承父请求的顺序位置。
// ID 依赖和 pending 跨通道共享，不能随 lane 拆成两套独立顺序或锁。
type dispatchQueue struct {
	mu            sync.Mutex
	lanes         [dispatchLaneCount]dispatchLane
	tails         map[int64]*dispatchJob
	pending       int
	awaitReserved int
	done          chan struct{}
	wg            sync.WaitGroup
	name          string
	// 原点在构造时固定，转入 I/O、续行及停机重试均不重置；保留 time.Now 的单调分量。
	clockOrigin       time.Time
	started, stopping bool
}

// newDispatchQueue 构造三通道队列；longConfig 省略时 long 池没有 worker（只在测试中省略）。
func newDispatchQueue(name string, short, io WorkerPoolConfig, handler, ioHandler func(*Msg), longConfig ...WorkerPoolConfig) *dispatchQueue {
	q := &dispatchQueue{
		name:        name,
		clockOrigin: time.Now(),
		lanes: [dispatchLaneCount]dispatchLane{
			dispatchShortLane: {config: short, handler: handler},
			dispatchIOLane:    {config: io, handler: ioHandler},
		},
		tails: make(map[int64]*dispatchJob),
		done:  make(chan struct{}),
	}
	if len(longConfig) > 0 {
		q.lanes[dispatchLongLane].config = longConfig[0]
		q.lanes[dispatchLongLane].handler = handler
	}
	for i := range q.lanes {
		q.lanes[i].wake = sync.NewCond(&q.mu)
	}
	return q
}

// start 为每个通道启动配置数量的 worker；重复调用或停止后调用无副作用。
func (q *dispatchQueue) start() {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.started || q.stopping {
		return
	}
	q.started = true
	for lane := range q.lanes {
		for range q.lanes[lane].config.Workers {
			q.wg.Add(1)
			go q.work(lane)
		}
	}
}

// dispatchIDs 复制消息目标后排序去重；返回值独立拥有存储，不修改消息中的切片。
func dispatchIDs(msg *Msg) []int64 {
	size := len(msg.Tids)
	if msg.Tid != 0 {
		size++
	}
	for _, group := range msg.GroupTIds {
		size += len(group)
	}
	ids := make([]int64, 0, max(1, size))
	if msg.Tid != 0 {
		ids = append(ids, msg.Tid)
	}
	ids = append(ids, msg.Tids...)
	for _, group := range msg.GroupTIds {
		ids = append(ids, group...)
	}
	if len(ids) == 0 {
		ids = append(ids, 0)
	}
	slices.Sort(ids)
	return slices.Compact(ids)
}

// newDispatchJob 在发布到队列前固定目标集合，之后直到完成都不再修改。
func newDispatchJob(msg *Msg, prepareIO bool) *dispatchJob {
	j := &dispatchJob{msg: msg, ioStage: prepareIO}
	switch {
	case len(msg.Tids) == 0 && len(msg.GroupTIds) == 0:
		// 没有目标时仍用 ID 0 保持原来的顺序域。
		j.inlineID[0] = msg.Tid
		j.ids = j.inlineID[:]
	case msg.Tid == 0 && len(msg.Tids) == 1 && len(msg.GroupTIds) == 0:
		j.inlineID[0] = msg.Tids[0]
		j.ids = j.inlineID[:]
	default:
		j.ids = dispatchIDs(msg)
	}
	return j
}

// admit 是统一准入：按消息的全部声明目标登记同 ID 顺序（对每个 ID 的当前尾作业建一条依赖边），再按容量决定
// 接受或以 ErrWorkerQueueFull 拒绝。prepareIO 时进入 io 通道，否则按实体 kind 进入 short 或 long 通道。
func (q *dispatchQueue) admit(msg *Msg, prepareIO bool) error {
	if q == nil {
		return worker.ErrWorkerClosed
	}
	lane := businessLane(msg)
	if prepareIO {
		lane = dispatchIOLane
	}
	state := &q.lanes[lane]
	j := newDispatchJob(msg, prepareIO)
	j.lane = lane
	q.mu.Lock()
	defer q.mu.Unlock()
	if !q.started || q.stopping {
		return worker.ErrWorkerClosed
	}
	blocked := false
	for _, id := range j.ids {
		if q.tails[id] != nil {
			blocked = true
			break
		}
	}
	// 先使用尚未占用的外部执行额度。否则 1024 worker / 16 等待位
	// 会在 worker 尚未被调度时，仅准入 16 条就误报满队列。
	// Await 的预留与普通 I/O 准入共享容量，不能在段结束前被其他生产者占走。
	reserved := 0
	if lane == dispatchIOLane {
		reserved = q.awaitReserved
	}
	hasExecutionSlot := !blocked && q.busyWorkers(lane, state)+state.readyCount+reserved < state.config.Workers
	if !hasExecutionSlot && q.waitingCount(lane, state) >= state.config.QueueCap {
		state.observation.Rejected++
		return worker.ErrWorkerQueueFull
	}
	for _, id := range j.ids {
		if prev := q.tails[id]; prev != nil {
			// 准入全程持有 q.mu；本轮若已登记 j，它必是该前驱的最后一个后继。
			// 多个 ID 指向同一前驱时只建一条边，计数与登记在一起，不另建去重表。
			if n := len(prev.successors); n == 0 || prev.successors[n-1] != j {
				prev.successors = append(prev.successors, j)
				j.predecessors++
			}
		}
		q.tails[id] = j
	}
	j.waitedForPredecessor = j.predecessors > 0
	q.pending++
	state.queued++
	j.admittedAt = time.Since(q.clockOrigin)
	state.waiting.push(j)
	if j.predecessors == 0 {
		q.enqueueReady(lane, j)
	}
	state.observation.PeakWaiting = max(state.observation.PeakWaiting, q.waitingCount(lane, state))
	return nil
}

// continueBusiness 只供占有一个 I/O worker 的已准入请求同步调用。每个 I/O worker
// 同时最多一个信封，所以容量天然不超过 I/O worker 数；外部满队列不阻断收尾。
func (q *dispatchQueue) continueBusiness(msg *Msg) {
	lane := businessLane(msg)
	q.mu.Lock()
	defer q.mu.Unlock()
	q.pushContinuationLocked(lane, msg)
}

// tryContinueBusiness 是 continueBusiness 给框架外部入口（NestMgr.RunLocal）用的版本：队列未启动或已开始
// 停止时拒绝，避免信封落在一个不会再有 worker 取走的队列里、调用方永远等不到。
func (q *dispatchQueue) tryContinueBusiness(msg *Msg) bool {
	lane := businessLane(msg)
	q.mu.Lock()
	defer q.mu.Unlock()
	if !q.started || q.stopping {
		return false
	}
	q.pushContinuationLocked(lane, msg)
	return true
}

// pushContinuationLocked 把内部业务续行放进业务通道 lane 的续行队列并唤醒一个 worker。续行继承父请求的
// 准入资格与顺序位置，不经 admit、不受外部容量限制，但计入 pending 让停机排空等它。调用方持有 q.mu。
func (q *dispatchQueue) pushContinuationLocked(lane int, msg *Msg) {
	state := &q.lanes[lane]
	state.continuations.push(&dispatchJob{msg: msg, continuation: true, lane: lane})
	state.continuationCount++
	q.pending++
	state.wake.Signal()
}

// take 取通道的下一项工作，没有时返回 nil。业务通道在续行与就绪外部作业之间交替：刚开始一个外部作业后优先续行，
// 避免续行在持续的外部负载下饥饿。调用方持有 q.mu。
func (q *dispatchQueue) take(lane int) *dispatchJob {
	state := &q.lanes[lane]
	if lane != dispatchIOLane && state.continuations.head != nil && (state.preferContinuation || state.ready.head == nil) {
		state.preferContinuation = false
		state.continuationCount--
		state.continuationRunning++
		state.observation.PeakWaiting = max(state.observation.PeakWaiting, q.waitingCount(lane, state))
		return state.continuations.pop()
	}
	if j := state.ready.pop(); j != nil {
		state.queued--
		state.readyCount--
		state.running++
		state.waiting.remove(j)
		stats := &state.observation
		stats.Started++
		waited := time.Since(q.clockOrigin) - j.readyAt
		stats.WorkerWait += waited
		stats.MaxWorkerWait = max(stats.MaxWorkerWait, waited)
		if lane != dispatchIOLane {
			state.preferContinuation = true
		}
		return j
	}
	return nil
}

// work 是通道 worker 的主循环：取工作 → 在新的请求上下文中执行 → 结束作业（释放同 ID 顺序、唤醒后继）→
// 执行消息登记的出队后动作。结束作业后在同一次持锁里接着取下一项，不先放锁再抢一次；没有工作时才在 wake 上
// 停放。出队后动作（await 的 I/O 启动）总在本项结束之后、不持锁、在本 worker 执行下一项之前运行。
// 停止且全部 pending 排空后退出。
func (q *dispatchQueue) work(lane int) {
	state := &q.lanes[lane]
	phase := fctx.WithIOWorker()
	if lane != dispatchIOLane {
		phase = fctx.WithShortWorker()
		if lane == dispatchLongLane {
			phase = fctx.WithLongWorker()
		}
	}
	defer q.wg.Done()
	// 每项工作的请求上下文选项对本 worker 固定，建一次；在 run 里逐次构造会让选项闭包每条消息逃逸到堆上。
	contextOptions := []fctx.Option{fctx.WithSource("worker"), fctx.WithHandler(q.name), phase}
	q.mu.Lock()
	for {
		j := q.take(lane)
		for j == nil && !(q.stopping && q.pending == 0) {
			state.wake.Wait()
			j = q.take(lane)
		}
		if j == nil {
			q.mu.Unlock()
			return
		}
		q.mu.Unlock()
		rerouted, after := q.run(lane, state, contextOptions, j)
		q.mu.Lock()
		if rerouted {
			q.rerouteToIO(j)
			continue
		}
		q.finishJobLocked(state, j)
		if after != nil {
			q.mu.Unlock()
			after()
			q.mu.Lock()
		}
	}
}

// run 在新的请求上下文里执行一项工作，返回它是否需要原位转 I/O 准备、以及消息登记的出队后动作。
// 转 I/O 的作业仍归队列所有，消息引用留给 I/O 池那一次执行释放。
func (q *dispatchQueue) run(lane int, state *dispatchLane, contextOptions []fctx.Option, j *dispatchJob) (rerouted bool, after func()) {
	goroutine.SafeFunc(func() {
		_, release := fctx.NewContext(contextOptions...)
		defer release()
		defer func() {
			if !rerouted {
				j.msg.OnRelease()
			}
		}()
		if j.msg.ioWork != nil {
			j.msg.ioWork()
		} else if handler := state.handler; handler != nil {
			handler(j.msg)
		}
		after = j.msg.afterQueue
		rerouted = lane != dispatchIOLane && !j.continuation && j.msg.ioReroute
	})
	return rerouted, after
}

// finishJobLocked 结束一项工作：归还执行额度，撤下它在各 ID 上的尾登记，后继的前驱全部完成即进入就绪；
// 停止中且全部排空时唤醒所有 worker 退出。调用方持有 q.mu。
func (q *dispatchQueue) finishJobLocked(state *dispatchLane, j *dispatchJob) {
	if j.continuation {
		state.continuationRunning--
	} else {
		state.running--
	}
	for _, id := range j.ids {
		if q.tails[id] == j {
			delete(q.tails, id)
		}
	}
	for _, next := range j.successors {
		next.predecessors--
		if next.predecessors == 0 {
			q.enqueueReady(next.lane, next)
		}
	}
	q.pending--
	if q.stopping && q.pending == 0 {
		for i := range q.lanes {
			q.lanes[i].wake.Broadcast()
		}
	}
}

// rerouteToIO 把业务池首跑时发现声明目标变冷的外部作业原位转到 I/O 池。作业保持在
// tails / successors 里的位置和 pending 计数：同 ID 后继继续等它完成，停机排空也继续等它，
// 所以既不重新排到自身之后，也不占用新的准入预算（它早已准入；I/O 池等待数可暂时超过
// QueueCap，上界仍是已准入的业务池作业数）。handler 从未开始，迁移的只是“I/O 准备”这一步。
// 调用方持有 q.mu。
func (q *dispatchQueue) rerouteToIO(j *dispatchJob) {
	io := &q.lanes[dispatchIOLane]
	q.lanes[j.lane].running--
	j.lane = dispatchIOLane
	j.msg.ioReroute = false
	j.ioStage = true
	j.waitedForPredecessor = false
	io.queued++
	io.waiting.push(j)
	q.enqueueReady(dispatchIOLane, j)
	io.observation.PeakWaiting = max(io.observation.PeakWaiting, q.waitingCount(dispatchIOLane, io))
	metrics.IncCounter("nest.dispatch.io_reroute.total", metrics.Labels{"dispatcher": q.name}, 1)
}

// stop 停止准入并唤醒 worker，在 ctx 内等待全部 pending 排空、worker 退出；超时返回 ctx 错误，可重试。
func (q *dispatchQueue) stop(ctx context.Context) error {
	if q == nil {
		return nil
	}
	q.mu.Lock()
	if !q.stopping {
		q.stopping = true
		for i := range q.lanes {
			q.lanes[i].wake.Broadcast()
		}
		go func() { q.wg.Wait(); close(q.done) }()
	}
	q.mu.Unlock()
	select {
	case <-q.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// stats 是 snapshotStats 的精简版（只供包内测试）。
func (q *dispatchQueue) stats() (short, io worker.PoolStats, continuations int) {
	short, io, continuations, _ = q.snapshotStats()
	return
}

// snapshotStats 在 q.mu 下取三个通道的池指标与观测统计。
func (q *dispatchQueue) snapshotStats() (short, io worker.PoolStats, continuations int, details DispatchQueueStats) {
	if q == nil {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	pools := [dispatchLaneCount]worker.PoolStats{}
	observed := [dispatchLaneCount]DispatchLaneStats{}
	now := time.Since(q.clockOrigin)
	for i, name := range []string{"short", "io", "long"} {
		state := &q.lanes[i]
		pools[i] = worker.PoolStats{Name: q.name + "_" + name, WorkerNum: state.config.Workers, QueueCap: state.config.QueueCap, QueueLen: q.waitingCount(i, state), Started: q.started, Stopped: q.stopping}
		observed[i] = state.observation
		observed[i].Running, observed[i].Ready = state.running, state.readyCount
		observed[i].BlockedOnPredecessor = state.queued - state.readyCount
		observed[i].WaitingForWorker = max(0, state.readyCount-max(0, state.config.Workers-q.busyWorkers(i, state)))
		if head := state.waiting.head; head != nil {
			observed[i].OldestWaiting = now - head.admittedAt
		}
	}
	return pools[dispatchShortLane], pools[dispatchIOLane], q.lanes[dispatchShortLane].continuationCount, DispatchQueueStats{Short: observed[dispatchShortLane], IO: observed[dispatchIOLane], Long: observed[dispatchLongLane], LongPool: pools[dispatchLongLane], LongContinuations: q.lanes[dispatchLongLane].continuationCount, ContinuationRunning: q.lanes[dispatchShortLane].continuationRunning + q.lanes[dispatchLongLane].continuationRunning}
}

// waitingCount 排除已预留外部执行额度的就绪消息。前驱未完成的消息
// 始终算等待，不能用空闲 worker 数掩盖同 ID 的积压。调用方持有 q.mu。
func (q *dispatchQueue) waitingCount(lane int, state *dispatchLane) int {
	await := 0
	if lane == dispatchIOLane {
		await = q.awaitReserved
	}
	reserved := min(state.readyCount+await, max(0, state.config.Workers-q.busyWorkers(lane, state)))
	return state.queued + await - reserved
}

// busyWorkers 返回通道正在占用的 worker 数。续行也占实际业务 worker；准入和观测必须使用同一执行容量。
// 调用方持有 q.mu。
func (q *dispatchQueue) busyWorkers(lane int, state *dispatchLane) int {
	busy := state.running
	if lane != dispatchIOLane {
		busy += state.continuationRunning
	}
	return busy
}

// enqueueReady 把前驱已全部完成的作业放入通道就绪队列，记录依赖等待并唤醒一个 worker。调用方持有 q.mu。
func (q *dispatchQueue) enqueueReady(lane int, j *dispatchJob) {
	state := &q.lanes[lane]
	j.readyAt = time.Since(q.clockOrigin)
	if j.waitedForPredecessor {
		waited := j.readyAt - j.admittedAt
		state.observation.DependencyWait += waited
		state.observation.MaxDependencyWait = max(state.observation.MaxDependencyWait, waited)
	}
	state.readyCount++
	state.ready.push(j)
	state.wake.Signal()
}

// readyWaiting 是尚未开始执行的外部作业的双向链表，按准入顺序维护，启动时 O(1) 删除；
// Stats 据它的表头取最老等待时长，不扫描全部 ID 或依赖链。
type readyWaiting struct{ head, tail *dispatchJob }

func (w *readyWaiting) push(j *dispatchJob) {
	j.waitingPrev = w.tail
	if w.tail != nil {
		w.tail.waitingNext = j
	} else {
		w.head = j
	}
	w.tail = j
}

func (w *readyWaiting) remove(j *dispatchJob) {
	if j.waitingPrev != nil {
		j.waitingPrev.waitingNext = j.waitingNext
	} else {
		w.head = j.waitingNext
	}
	if j.waitingNext != nil {
		j.waitingNext.waitingPrev = j.waitingPrev
	} else {
		w.tail = j.waitingPrev
	}
	j.waitingPrev, j.waitingNext = nil, nil
}

// businessLane 只读取初始化时注册的 Kind 元数据，不受发送方的 worker 身份影响。
func businessLane(msg *Msg) int {
	if msg == nil {
		return dispatchShortLane
	}
	if msg.remoteLogic != nil {
		return businessLane(msg.remoteLogic.msg)
	}
	long := func(id int64) bool {
		return id != 0 && entity.EntityBusinessPoolOfKind(entity.ResolveEntityID(id).Kind) == entity.BusinessPoolLong
	}
	if long(msg.Tid) {
		return dispatchLongLane
	}
	if slices.ContainsFunc(msg.Tids, long) {
		return dispatchLongLane
	}
	for _, group := range msg.GroupTIds {
		if slices.ContainsFunc(group, long) {
			return dispatchLongLane
		}
	}
	return dispatchShortLane
}
