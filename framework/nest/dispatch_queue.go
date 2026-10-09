package nest

import (
	"context"
	"slices"
	"sync"
	"time"

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
	dispatchLaneCount
)

type dispatchJob struct {
	singleID                 [1]int64
	admittedAt, readyAt      time.Time
	waitingPrev, waitingNext *dispatchJob
	msg                      *Msg
	ids                      []int64
	slow                     bool
	continuation             bool
	predecessors             int
	waitedForPredecessor     bool
	successors               []*dispatchJob
	next                     *dispatchJob
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
	wake        *sync.Cond
	config      WorkerPoolConfig
	handler     func(*Msg)
	ready       readyJobs
	waiting     readyWaiting
	queued      int // 尚未开始执行的外部请求，包括 ID 前驱等待。
	readyCount  int // 其中已经满足 ID 依赖的请求。
	running     int // 正在执行的外部请求；内部延续单独预算。
	observation DispatchLaneStats
}

// dispatchQueue 先登记 ID 顺序，再交给两种执行资源。慢池没有 ID 分槽；
// 等待前驱只占准入预算，不占任何 worker。内部快阶段继承父请求的顺序位置。
// ID 依赖和 pending 跨通道共享，不能随 lane 拆成两套独立顺序或锁。
type dispatchQueue struct {
	mu                  sync.Mutex
	lanes               [dispatchLaneCount]dispatchLane
	continuations       readyJobs
	preferContinuation  bool
	continuationCount   int
	tails               map[int64]*dispatchJob
	pending             int
	started, stopping   bool
	done                chan struct{}
	wg                  sync.WaitGroup
	name                string
	continuationRunning int
}

func newDispatchQueue(name string, fast, slow WorkerPoolConfig, handler, slowHandler func(*Msg)) *dispatchQueue {
	q := &dispatchQueue{
		name: name,
		lanes: [dispatchLaneCount]dispatchLane{
			dispatchFastLane: {config: fast, handler: handler},
			dispatchSlowLane: {config: slow, handler: slowHandler},
		},
		tails: make(map[int64]*dispatchJob),
		done:  make(chan struct{}),
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
func dispatchIDs(msg *Msg) []int64 { return dispatchIDsInto(nil, msg) }

func dispatchIDsInto(ids []int64, msg *Msg) []int64 {
	size := len(msg.Tids)
	if msg.Tid != 0 {
		size++
	}
	for _, group := range msg.GroupTIds {
		size += len(group)
	}
	if cap(ids) < max(1, size) {
		ids = make([]int64, 0, max(1, size))
	}
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

func (q *dispatchQueue) admit(msg *Msg, slow bool) error {
	if q == nil {
		return worker.ErrWorkerClosed
	}
	lane := dispatchFastLane
	if slow {
		lane = dispatchSlowLane
	}
	state := &q.lanes[lane]
	j := &dispatchJob{msg: msg, slow: slow}
	j.ids = dispatchIDsInto(j.singleID[:0], msg)
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
	hasExecutionSlot := j.predecessors == 0 && q.busyWorkers(lane)+state.readyCount < state.config.Workers
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
	j.admittedAt = time.Now()
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
	q.mu.Lock()
	defer q.mu.Unlock()
	q.continuations.push(&dispatchJob{msg: msg, continuation: true})
	q.continuationCount++
	q.pending++
	q.lanes[dispatchFastLane].wake.Signal()
}

// tryContinueFast 是 continueFast 给框架外部入口（NestMgr.RunLocal）用的版本：队列未启动或已开始
// 停止时拒绝，避免信封落在一个不会再有 worker 取走的队列里、调用方永远等不到。
func (q *dispatchQueue) tryContinueFast(msg *Msg) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if !q.started || q.stopping {
		return false
	}
	q.continuations.push(&dispatchJob{msg: msg, continuation: true})
	q.continuationCount++
	q.pending++
	q.lanes[dispatchFastLane].wake.Signal()
	return true
}

func (q *dispatchQueue) take(lane int) *dispatchJob {
	state := &q.lanes[lane]
	if lane == dispatchFastLane && q.continuations.head != nil && (q.preferContinuation || state.ready.head == nil) {
		q.preferContinuation = false
		q.continuationCount--
		q.continuationRunning++
		state.observation.PeakWaiting = max(state.observation.PeakWaiting, q.waitingCount(lane))
		return q.continuations.pop()
	}
	if j := state.ready.pop(); j != nil {
		state.queued--
		state.readyCount--
		state.running++
		state.waiting.remove(j)
		stats := &state.observation
		stats.Started++
		waited := time.Since(j.readyAt)
		stats.WorkerWait += waited
		stats.MaxWorkerWait = max(stats.MaxWorkerWait, waited)
		if lane == dispatchFastLane {
			q.preferContinuation = true
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
		goroutine.SafeFunc(func() {
			var phase fctx.Option
			if lane == dispatchFastLane {
				phase = fctx.WithFastWorker()
			}
			_, release := fctx.NewContext(fctx.WithSource("worker"), fctx.WithHandler(q.name), phase)
			defer release()
			// 转慢的作业仍归队列所有，消息引用留给慢池那一次执行释放。
			defer func() {
				if !rerouted {
					j.msg.OnRelease()
				}
			}()
			if handler := state.handler; handler != nil {
				handler(j.msg)
			}
			rerouted = lane == dispatchFastLane && !j.continuation && j.msg.slowReroute
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
			q.continuationRunning--
		}
		for _, id := range j.ids {
			if q.tails[id] == j {
				delete(q.tails, id)
			}
		}
		for _, next := range j.successors {
			next.predecessors--
			if next.predecessors == 0 {
				target := dispatchFastLane
				if next.slow {
					target = dispatchSlowLane
				}
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
	}
}

// rerouteToSlow 把快池首跑时发现声明目标变冷的外部作业原位转到慢池。作业保持在
// tails / successors 里的位置和 pending 计数：同 ID 后继继续等它完成，停机排空也继续等它，
// 所以既不重新排到自身之后，也不占用新的准入预算（它早已准入；慢池等待数可暂时超过
// QueueCap，上界仍是已准入的快池作业数）。handler 从未开始，迁移的只是“慢准备”这一步。
// 调用方持有 q.mu。
func (q *dispatchQueue) rerouteToSlow(j *dispatchJob) {
	slow := &q.lanes[dispatchSlowLane]
	q.lanes[dispatchFastLane].running--
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
	now := time.Now()
	for i, name := range []string{"fast", "slow"} {
		state := &q.lanes[i]
		pools[i] = worker.PoolStats{Name: q.name + "_" + name, WorkerNum: state.config.Workers, QueueCap: state.config.QueueCap, QueueLen: q.waitingCount(i), Started: q.started, Stopped: q.stopping}
		observed[i] = state.observation
		observed[i].Running, observed[i].Ready = state.running, state.readyCount
		observed[i].BlockedOnPredecessor = state.queued - state.readyCount
		observed[i].WaitingForWorker = max(0, state.readyCount-max(0, state.config.Workers-q.busyWorkers(i)))
		if head := state.waiting.head; head != nil {
			observed[i].OldestWaiting = now.Sub(head.admittedAt)
		}
	}
	return pools[dispatchFastLane], pools[dispatchSlowLane], q.continuationCount, DispatchQueueStats{Fast: observed[dispatchFastLane], Slow: observed[dispatchSlowLane], ContinuationRunning: q.continuationRunning}
}

// waitingCount 排除已预留外部执行额度的就绪消息。前驱未完成的消息
// 始终算等待，不能用空闲 worker 数掩盖同 ID 的积压。调用方持有 q.mu。
func (q *dispatchQueue) waitingCount(lane int) int {
	state := &q.lanes[lane]
	reserved := min(state.readyCount, max(0, state.config.Workers-q.busyWorkers(lane)))
	return state.queued - reserved
}

// 续行也占实际快 worker；准入和观测必须使用同一执行容量。调用方持有 q.mu。
func (q *dispatchQueue) busyWorkers(lane int) int {
	state := &q.lanes[lane]
	busy := state.running
	if lane == dispatchFastLane {
		busy += q.continuationRunning
	}
	return busy
}
func (q *dispatchQueue) enqueueReady(lane int, j *dispatchJob) {
	state := &q.lanes[lane]
	j.readyAt = time.Now()
	if j.waitedForPredecessor {
		waited := j.readyAt.Sub(j.admittedAt)
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
