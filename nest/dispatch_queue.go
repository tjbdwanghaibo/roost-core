package nest

import (
	"context"
	"slices"
	"sync"

	"github.com/tjbdwanghaibo/roost-core/fctx"
	"github.com/tjbdwanghaibo/roost-core/goroutine"
	"github.com/tjbdwanghaibo/roost-core/worker"
)

// WorkerPoolConfig 配置整个池的等待预算，QueueCap 不再按 worker 倍增。
type WorkerPoolConfig struct{ Workers, QueueCap int }

type dispatchJob struct {
	msg          *Msg
	ids          []int64
	slow         bool
	continuation bool
	predecessors int
	successors   []*dispatchJob
	next         *dispatchJob
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
	mu                 sync.Mutex
	wake               [2]*sync.Cond
	config             [2]WorkerPoolConfig
	ready              [2]readyJobs
	continuations      readyJobs
	preferContinuation bool
	queued             [2]int // 尚未开始执行的外部请求，包括 ID 前驱等待。
	readyCount         [2]int // 其中已经满足 ID 依赖的请求。
	running            [2]int // 正在执行的外部请求；内部延续单独预算。
	continuationCount  int
	tails              map[int64]*dispatchJob
	pending            int
	started, stopping  bool
	done               chan struct{}
	wg                 sync.WaitGroup
	handlers           [2]func(*Msg)
	name               string
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
func dispatchIDs(msg *Msg) []int64 {
	ids := make([]int64, 0, 1+len(msg.Tids))
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
	j := &dispatchJob{msg: msg, ids: dispatchIDs(msg), slow: slow}
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
	hasExecutionSlot := j.predecessors == 0 && q.running[lane]+q.readyCount[lane] < q.config[lane].Workers
	if !hasExecutionSlot && q.waitingCount(lane) >= q.config[lane].QueueCap {
		return worker.ErrWorkerQueueFull
	}
	for _, id := range j.ids {
		if prev := q.tails[id]; prev != nil {
			prev.successors = append(prev.successors, j)
		}
		q.tails[id] = j
	}
	q.pending++
	q.queued[lane]++
	if j.predecessors == 0 {
		q.enqueueReady(lane, j)
	}
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
		return q.continuations.pop()
	}
	if j := q.ready[lane].pop(); j != nil {
		q.queued[lane]--
		q.readyCount[lane]--
		q.running[lane]++
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
		goroutine.SafeFunc(func() {
			_, release := fctx.NewContext(fctx.WithSource("worker"), fctx.WithHandler(q.name))
			defer release()
			defer j.msg.OnRelease()
			if handler := q.handlers[lane]; handler != nil {
				handler(j.msg)
			}
		})
		q.mu.Lock()
		if !j.continuation {
			q.running[lane]--
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
	if q == nil {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	stats := func(i int, name string) worker.PoolStats {
		return worker.PoolStats{Name: q.name + "_" + name, WorkerNum: q.config[i].Workers, QueueCap: q.config[i].QueueCap, QueueLen: q.waitingCount(i), Started: q.started, Stopped: q.stopping}
	}
	return stats(0, "fast"), stats(1, "slow"), q.continuationCount
}

// waitingCount 排除已预留外部执行额度的就绪消息。前驱未完成的消息
// 始终算等待，不能用空闲 worker 数掩盖同 ID 的积压。调用方持有 q.mu。
func (q *dispatchQueue) waitingCount(lane int) int {
	reserved := min(q.readyCount[lane], q.config[lane].Workers-q.running[lane])
	return q.queued[lane] - reserved
}
func (q *dispatchQueue) enqueueReady(lane int, j *dispatchJob) {
	q.readyCount[lane]++
	q.ready[lane].push(j)
	q.wake[lane].Signal()
}
