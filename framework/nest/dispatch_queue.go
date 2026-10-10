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

// DefaultFastQueueCapacity 是快池共享等待容量的缺省值，不随 worker 数倍增。
const DefaultFastQueueCapacity = 65536

// WorkerPoolConfig 配置整个池的等待预算，QueueCap 不再按 worker 倍增。
type WorkerPoolConfig struct{ Workers, QueueCap int }

const (
	dispatchFastLane = iota
	dispatchSlowLane
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
	slow                     bool
	continuation             bool
	waitedForPredecessor     bool
}

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

// dispatchQueue 先登记 ID 顺序，再交给三种执行资源。慢池没有 ID 分槽；
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
	// 原点在构造时固定，转慢、续行及停机重试均不重置；保留 time.Now 的单调分量。
	clockOrigin       time.Time
	started, stopping bool
}

func newDispatchQueue(name string, fast, slow WorkerPoolConfig, handler, slowHandler func(*Msg), longConfig ...WorkerPoolConfig) *dispatchQueue {
	q := &dispatchQueue{
		name:        name,
		clockOrigin: time.Now(),
		lanes: [dispatchLaneCount]dispatchLane{
			dispatchFastLane: {config: fast, handler: handler},
			dispatchSlowLane: {config: slow, handler: slowHandler},
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
func newDispatchJob(msg *Msg, slow bool) *dispatchJob {
	j := &dispatchJob{msg: msg, slow: slow}
	switch {
	case len(msg.Tids) == 0 && len(msg.GroupTIds) == 0:
		// 没有目标时仍用ID 0保持原来的顺序域。
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

func (q *dispatchQueue) admit(msg *Msg, slow bool) error {
	if q == nil {
		return worker.ErrWorkerClosed
	}
	lane := businessLane(msg)
	if slow {
		lane = dispatchSlowLane
	}
	state := &q.lanes[lane]
	j := newDispatchJob(msg, slow)
	j.lane = lane
	q.mu.Lock()
	defer q.mu.Unlock()
	if !q.started || q.stopping {
		return worker.ErrWorkerClosed
	}
	for _, id := range j.ids {
		if q.tails[id] != nil {
			j.predecessors++
		}
	}
	// 先使用尚未占用的外部执行额度。否则 1024 worker / 16 等待位
	// 会在 worker 尚未被调度时，仅准入 16 条就误报满队列。
	// Await 的预留与普通 I/O 准入共享容量，不能在段结束前被其他生产者占走。
	reserved := 0
	if lane == dispatchSlowLane {
		reserved = q.awaitReserved
	}
	hasExecutionSlot := j.predecessors == 0 && q.busyWorkers(lane)+state.readyCount+reserved < state.config.Workers
	if !hasExecutionSlot && q.waitingCount(lane) >= state.config.QueueCap {
		state.observation.Rejected++
		return worker.ErrWorkerQueueFull
	}
	for _, id := range j.ids {
		if prev := q.tails[id]; prev != nil {
			prev.successors = append(prev.successors, j)
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
	state.observation.PeakWaiting = max(state.observation.PeakWaiting, q.waitingCount(lane))
	return nil
}

// continueFast 只供占有一个慢 worker 的已准入请求同步调用。每个慢 worker
// 同时最多一个信封，所以容量天然不超过慢 worker 数；外部满队列不阻断收尾。
func (q *dispatchQueue) continueFast(msg *Msg) {
	lane := businessLane(msg)
	q.mu.Lock()
	defer q.mu.Unlock()
	q.lanes[lane].continuations.push(&dispatchJob{msg: msg, continuation: true, lane: lane})
	q.lanes[lane].continuationCount++
	q.pending++
	q.lanes[lane].wake.Signal()
}

// tryContinueFast 是 continueFast 给框架外部入口（NestMgr.RunLocal）用的版本：队列未启动或已开始
// 停止时拒绝，避免信封落在一个不会再有 worker 取走的队列里、调用方永远等不到。
func (q *dispatchQueue) tryContinueFast(msg *Msg) bool {
	lane := businessLane(msg)
	q.mu.Lock()
	defer q.mu.Unlock()
	if !q.started || q.stopping {
		return false
	}
	q.lanes[lane].continuations.push(&dispatchJob{msg: msg, continuation: true, lane: lane})
	q.lanes[lane].continuationCount++
	q.pending++
	q.lanes[lane].wake.Signal()
	return true
}

func (q *dispatchQueue) take(lane int) *dispatchJob {
	state := &q.lanes[lane]
	if lane != dispatchSlowLane && q.lanes[lane].continuations.head != nil && (q.lanes[lane].preferContinuation || state.ready.head == nil) {
		q.lanes[lane].preferContinuation = false
		q.lanes[lane].continuationCount--
		q.lanes[lane].continuationRunning++
		state.observation.PeakWaiting = max(state.observation.PeakWaiting, q.waitingCount(lane))
		return q.lanes[lane].continuations.pop()
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
		if lane != dispatchSlowLane {
			q.lanes[lane].preferContinuation = true
		}
		return j
	}
	return nil
}
func (q *dispatchQueue) work(lane int) {
	state := &q.lanes[lane]
	defer q.wg.Done()
	for {
		q.mu.Lock()
		j := q.take(lane)
		for j == nil && !(q.stopping && q.pending == 0) {
			state.wake.Wait()
			j = q.take(lane)
		}
		q.mu.Unlock()
		if j == nil {
			return
		}
		rerouted := false
		var after func()
		goroutine.SafeFunc(func() {
			phase := fctx.WithIOWorker()
			if lane != dispatchSlowLane {
				phase = fctx.WithFastWorker()
				if lane == dispatchLongLane {
					phase = fctx.WithLongWorker()
				}
			}
			_, release := fctx.NewContext(fctx.WithSource("worker"), fctx.WithHandler(q.name), phase)
			defer release()
			// 转慢的作业仍归队列所有，消息引用留给慢池那一次执行释放。
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
			rerouted = lane != dispatchSlowLane && !j.continuation && j.msg.slowReroute
		})
		q.mu.Lock()
		if rerouted {
			q.rerouteToSlow(j)
			q.mu.Unlock()
			continue
		}
		if !j.continuation {
			state.running--
		} else {
			q.lanes[lane].continuationRunning--
		}
		for _, id := range j.ids {
			if q.tails[id] == j {
				delete(q.tails, id)
			}
		}
		for _, next := range j.successors {
			next.predecessors--
			if next.predecessors == 0 {
				target := next.lane
				q.enqueueReady(target, next)
			}
		}
		q.pending--
		if q.stopping && q.pending == 0 {
			for i := range q.lanes {
				q.lanes[i].wake.Broadcast()
			}
		}
		q.mu.Unlock()
		if after != nil {
			after()
		}
	}
}

// rerouteToSlow 把快池首跑时发现声明目标变冷的外部作业原位转到慢池。作业保持在
// tails / successors 里的位置和 pending 计数：同 ID 后继继续等它完成，停机排空也继续等它，
// 所以既不重新排到自身之后，也不占用新的准入预算（它早已准入；慢池等待数可暂时超过
// QueueCap，上界仍是已准入的快池作业数）。handler 从未开始，迁移的只是“慢准备”这一步。
// 调用方持有 q.mu。
func (q *dispatchQueue) rerouteToSlow(j *dispatchJob) {
	slow := &q.lanes[dispatchSlowLane]
	q.lanes[j.lane].running--
	j.lane = dispatchSlowLane
	j.msg.slowReroute = false
	j.slow = true
	j.waitedForPredecessor = false
	slow.queued++
	slow.waiting.push(j)
	q.enqueueReady(dispatchSlowLane, j)
	slow.observation.PeakWaiting = max(slow.observation.PeakWaiting, q.waitingCount(dispatchSlowLane))
	metrics.IncCounter("nest.dispatch.slow_reroute.total", metrics.Labels{"dispatcher": q.name}, 1)
}

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
func (q *dispatchQueue) stats() (fast, slow worker.PoolStats, continuations int) {
	fast, slow, continuations, _ = q.snapshotStats()
	return
}

func (q *dispatchQueue) snapshotStats() (fast, slow worker.PoolStats, continuations int, details DispatchQueueStats) {
	if q == nil {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	pools := [dispatchLaneCount]worker.PoolStats{}
	observed := [dispatchLaneCount]DispatchLaneStats{}
	now := time.Since(q.clockOrigin)
	for i, name := range []string{"fast", "slow", "long"} {
		state := &q.lanes[i]
		pools[i] = worker.PoolStats{Name: q.name + "_" + name, WorkerNum: state.config.Workers, QueueCap: state.config.QueueCap, QueueLen: q.waitingCount(i), Started: q.started, Stopped: q.stopping}
		observed[i] = state.observation
		observed[i].Running, observed[i].Ready = state.running, state.readyCount
		observed[i].BlockedOnPredecessor = state.queued - state.readyCount
		observed[i].WaitingForWorker = max(0, state.readyCount-max(0, state.config.Workers-q.busyWorkers(i)))
		if head := state.waiting.head; head != nil {
			observed[i].OldestWaiting = now - head.admittedAt
		}
	}
	return pools[dispatchFastLane], pools[dispatchSlowLane], q.lanes[dispatchFastLane].continuationCount, DispatchQueueStats{Fast: observed[dispatchFastLane], Slow: observed[dispatchSlowLane], Long: observed[dispatchLongLane], LongPool: pools[dispatchLongLane], LongContinuations: q.lanes[dispatchLongLane].continuationCount, ContinuationRunning: q.lanes[dispatchFastLane].continuationRunning + q.lanes[dispatchLongLane].continuationRunning}
}

// waitingCount 排除已预留外部执行额度的就绪消息。前驱未完成的消息
// 始终算等待，不能用空闲 worker 数掩盖同 ID 的积压。调用方持有 q.mu。
func (q *dispatchQueue) waitingCount(lane int) int {
	state := &q.lanes[lane]
	await := 0
	if lane == dispatchSlowLane {
		await = q.awaitReserved
	}
	reserved := min(state.readyCount+await, max(0, state.config.Workers-q.busyWorkers(lane)))
	return state.queued + await - reserved
}

// 续行也占实际快 worker；准入和观测必须使用同一执行容量。调用方持有 q.mu。
func (q *dispatchQueue) busyWorkers(lane int) int {
	state := &q.lanes[lane]
	busy := state.running
	if lane != dispatchSlowLane {
		busy += q.lanes[lane].continuationRunning
	}
	return busy
}
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

// 等待链按准入顺序维护，启动时 O(1) 删除；Stats 不扫描全部 ID 或依赖链。
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
		return dispatchFastLane
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
	for _, id := range msg.Tids {
		if long(id) {
			return dispatchLongLane
		}
	}
	for _, group := range msg.GroupTIds {
		for _, id := range group {
			if long(id) {
				return dispatchLongLane
			}
		}
	}
	return dispatchFastLane
}
