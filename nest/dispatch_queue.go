package nest

import (
	"context"
	"slices"
	"sync"
	"time"

	"github.com/tjbdwanghaibo/roost-core/fctx"
	"github.com/tjbdwanghaibo/roost-core/goroutine"
	"github.com/tjbdwanghaibo/roost-core/metrics"
	"github.com/tjbdwanghaibo/roost-core/worker"
)

// WorkerPoolConfig 配置整个池的等待预算，QueueCap 不再按 worker 倍增。
type WorkerPoolConfig struct{ Workers, QueueCap int }

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

// dispatchQueue 先登记 ID 顺序，再交给两种执行资源。慢池没有 ID 分槽；
// 等待前驱只占准入预算，不占任何 worker。内部快阶段继承父请求的顺序位置。
type dispatchQueue struct {
	mu                  sync.Mutex
	wake                [2]*sync.Cond
	config              [2]WorkerPoolConfig
	ready               [2]readyJobs
	continuations       readyJobs
	preferContinuation  bool
	queued              [2]int // 尚未开始执行的外部请求，包括 ID 前驱等待。
	readyCount          [2]int // 其中已经满足 ID 依赖的请求。
	running             [2]int // 正在执行的外部请求；内部延续单独预算。
	continuationCount   int
	tails               map[int64]*dispatchJob
	pending             int
	started, stopping   bool
	done                chan struct{}
	wg                  sync.WaitGroup
	handlers            [2]func(*Msg)
	name                string
	waiting             [2]readyWaiting
	observation         [2]DispatchLaneStats
	continuationRunning int
}

func newDispatchQueue(name string, fast, slow WorkerPoolConfig, handler, slowHandler func(*Msg)) *dispatchQueue {
	q := &dispatchQueue{name: name, config: [2]WorkerPoolConfig{fast, slow}, tails: make(map[int64]*dispatchJob), done: make(chan struct{}), handlers: [2]func(*Msg){handler, slowHandler}}
	for i := range q.wake {
		q.wake[i] = sync.NewCond(&q.mu)
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
	for lane, cfg := range q.config {
		for range cfg.Workers {
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
	lane := 0
	if slow {
		lane = 1
	}
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
	hasExecutionSlot := j.predecessors == 0 && q.busyWorkers(lane)+q.readyCount[lane] < q.config[lane].Workers
	if !hasExecutionSlot && q.waitingCount(lane) >= q.config[lane].QueueCap {
		q.observation[lane].Rejected++
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
	q.queued[lane]++
	j.admittedAt = time.Now()
	q.waiting[lane].push(j)
	if j.predecessors == 0 {
		q.enqueueReady(lane, j)
	}
	q.observation[lane].PeakWaiting = max(q.observation[lane].PeakWaiting, q.waitingCount(lane))
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
	q.wake[0].Signal()
}
func (q *dispatchQueue) take(lane int) *dispatchJob {
	if lane == 0 && q.continuations.head != nil && (q.preferContinuation || q.ready[0].head == nil) {
		q.preferContinuation = false
		q.continuationCount--
		q.continuationRunning++
		q.observation[0].PeakWaiting = max(q.observation[0].PeakWaiting, q.waitingCount(0))
		return q.continuations.pop()
	}
	if j := q.ready[lane].pop(); j != nil {
		q.queued[lane]--
		q.readyCount[lane]--
		q.running[lane]++
		q.waiting[lane].remove(j)
		stats := &q.observation[lane]
		stats.Started++
		waited := time.Since(j.readyAt)
		stats.WorkerWait += waited
		stats.MaxWorkerWait = max(stats.MaxWorkerWait, waited)
		if lane == 0 {
			q.preferContinuation = true
		}
		return j
	}
	return nil
}
func (q *dispatchQueue) work(lane int) {
	defer q.wg.Done()
	for {
		q.mu.Lock()
		j := q.take(lane)
		for j == nil && !(q.stopping && q.pending == 0) {
			q.wake[lane].Wait()
			j = q.take(lane)
		}
		q.mu.Unlock()
		if j == nil {
			return
		}
		rerouted := false
		goroutine.SafeFunc(func() {
			var phase fctx.Option
			if lane == 0 {
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
			if handler := q.handlers[lane]; handler != nil {
				handler(j.msg)
			}
			rerouted = lane == 0 && !j.continuation && j.msg.slowReroute
		})
		q.mu.Lock()
		if rerouted {
			q.rerouteToSlow(j)
			q.mu.Unlock()
			continue
		}
		if !j.continuation {
			q.running[lane]--
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
				target := 0
				if next.slow {
					target = 1
				}
				q.enqueueReady(target, next)
			}
		}
		q.pending--
		if q.stopping && q.pending == 0 {
			for _, wake := range q.wake {
				wake.Broadcast()
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
	q.running[0]--
	j.msg.slowReroute = false
	j.slow = true
	j.waitedForPredecessor = false
	q.queued[1]++
	q.waiting[1].push(j)
	q.enqueueReady(1, j)
	q.observation[1].PeakWaiting = max(q.observation[1].PeakWaiting, q.waitingCount(1))
	metrics.IncCounter("nest.dispatch.slow_reroute.total", metrics.Labels{"dispatcher": q.name}, 1)
}

func (q *dispatchQueue) stop(ctx context.Context) error {
	if q == nil {
		return nil
	}
	q.mu.Lock()
	if !q.stopping {
		q.stopping = true
		for _, wake := range q.wake {
			wake.Broadcast()
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
	pools := [2]worker.PoolStats{}
	lanes := [2]DispatchLaneStats{}
	now := time.Now()
	for i, name := range []string{"fast", "slow"} {
		pools[i] = worker.PoolStats{Name: q.name + "_" + name, WorkerNum: q.config[i].Workers, QueueCap: q.config[i].QueueCap, QueueLen: q.waitingCount(i), Started: q.started, Stopped: q.stopping}
		lanes[i] = q.observation[i]
		lanes[i].Running, lanes[i].Ready = q.running[i], q.readyCount[i]
		lanes[i].BlockedOnPredecessor = q.queued[i] - q.readyCount[i]
		lanes[i].WaitingForWorker = max(0, q.readyCount[i]-max(0, q.config[i].Workers-q.busyWorkers(i)))
		if head := q.waiting[i].head; head != nil {
			lanes[i].OldestWaiting = now.Sub(head.admittedAt)
		}
	}
	return pools[0], pools[1], q.continuationCount, DispatchQueueStats{Fast: lanes[0], Slow: lanes[1], ContinuationRunning: q.continuationRunning}
}

// waitingCount 排除已预留外部执行额度的就绪消息。前驱未完成的消息
// 始终算等待，不能用空闲 worker 数掩盖同 ID 的积压。调用方持有 q.mu。
func (q *dispatchQueue) waitingCount(lane int) int {
	reserved := min(q.readyCount[lane], max(0, q.config[lane].Workers-q.busyWorkers(lane)))
	return q.queued[lane] - reserved
}

// 续行也占实际快 worker；准入和观测必须使用同一执行容量。调用方持有 q.mu。
func (q *dispatchQueue) busyWorkers(lane int) int {
	busy := q.running[lane]
	if lane == 0 {
		busy += q.continuationRunning
	}
	return busy
}
func (q *dispatchQueue) enqueueReady(lane int, j *dispatchJob) {
	j.readyAt = time.Now()
	if j.waitedForPredecessor {
		waited := j.readyAt.Sub(j.admittedAt)
		q.observation[lane].DependencyWait += waited
		q.observation[lane].MaxDependencyWait = max(q.observation[lane].MaxDependencyWait, waited)
	}
	q.readyCount[lane]++
	q.ready[lane].push(j)
	q.wake[lane].Signal()
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
