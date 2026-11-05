package worker

import (
	"errors"
	"sync"
	"sync/atomic"

	"github.com/tjbdwanghaibo/roost-core/infra/base/goroutine"
)

// ErrKeyedDispatcherFull 是 KeyedDispatcher 已接纳、还没执行完的任务数达到上限时的拒绝原因。
var ErrKeyedDispatcherFull = errors.New("worker: keyed dispatcher is full")

// KeyedDispatcher 按键派发任务，给“同一个键按到达顺序、不同键互不等待”的接收端用（总线 RPC 的按顺序键派发、
// DataEngine effect 消费者的按实体派发）：
//
//   - 带键的任务按键排队：同一个键的任务按 Dispatch 接纳的顺序逐个执行，前一个返回之后才轮到下一个；不同键之间互不等待。
//   - 空键的任务各自执行，不承诺顺序。
//   - 已接纳、还没执行完（在跑与排队）的任务合计不超过 limit，超出的 Dispatch 返回 ErrKeyedDispatcherFull，任务没有执行。
//
// 为什么不是“按键哈希到固定 worker”：任务可能同步等别的实例（RPC handler 调另一个服务、effect handler 调联盟守卫），
// 固定 worker 让无关的键共用一个 worker——一个键卡住会挡住同 worker 上的其他键，两个实例互相调用时还会跨实例互等。
// 这里一个任务只挡住自己的键。同时在跑的任务数要再设上限时由调用方在 run 里自己限（例如 effect 消费者的 Workers）。
//
// goroutine 由调用方给的 start 起（例如经 Pool.TryGo，受池的生命周期管理）：空键的任务一个 goroutine，带键的任务每个
// 活跃键一个 goroutine，依次处理这个键排队的任务，队列空了就退出。零值不可用，由 NewKeyedDispatcher 建立。
type KeyedDispatcher struct {
	limit   int64
	pending atomic.Int64 // 已接纳、还没执行完（在跑与排队）的任务数

	mu    sync.Mutex
	lanes map[string]*keyedLane // 活跃的键；条目存在 ⇔ 这个键有一个 goroutine 在处理它的队列
}

// keyedLane 是一个活跃键排队等候的任务（不含正在执行的那个），受 KeyedDispatcher.mu 保护。
type keyedLane struct {
	queue []func()
}

// NewKeyedDispatcher 建立派发器；limit ≤ 0 按 1 处理。
func NewKeyedDispatcher(limit int) *KeyedDispatcher {
	if limit <= 0 {
		limit = 1
	}
	return &KeyedDispatcher{limit: int64(limit), lanes: make(map[string]*keyedLane)}
}

// Dispatch 接纳 run；返回 nil 表示接纳，run 一定会被执行（start 起的 goroutine 负责）。拒绝时（达到上限，或 start
// 返回错误）run 没有执行，由调用方答复或交还上游。start 起一个 goroutine 执行它收到的函数，起不了时返回错误。
//
// run 里的 panic 只结束这一个任务（记 ERROR 日志）：排在同一个键后面的任务照常执行（否则条目留在表里，后到的任务永远排队）。
func (d *KeyedDispatcher) Dispatch(key string, start func(func()) error, run func()) error {
	if d.pending.Add(1) > d.limit {
		d.pending.Add(-1)
		return ErrKeyedDispatcherFull
	}
	if key == "" {
		if err := start(func() { d.runOne(run) }); err != nil {
			d.pending.Add(-1)
			return err
		}
		return nil
	}
	// 持 mu 起 goroutine 再登记：新 goroutine 执行完第一个任务后要取 mu 检查队列，这时登记一定已经完成；
	// 排到已有键后面的任务一定会被那个 goroutine 取走（它在 mu 下确认队列为空才删除条目、退出）。
	d.mu.Lock()
	defer d.mu.Unlock()
	if lane := d.lanes[key]; lane != nil {
		lane.queue = append(lane.queue, run)
		return nil
	}
	lane := &keyedLane{}
	if err := start(func() { d.drain(key, lane, run) }); err != nil {
		d.pending.Add(-1)
		return err
	}
	d.lanes[key] = lane
	return nil
}

// Pending 返回已接纳、还没执行完的任务数。
func (d *KeyedDispatcher) Pending() int { return int(d.pending.Load()) }

// drain 依次执行一个键的任务：先执行 first，再取这个键排队的下一个，队列空了删除条目并退出。
func (d *KeyedDispatcher) drain(key string, lane *keyedLane, first func()) {
	run := first
	for {
		d.runOne(run)
		d.mu.Lock()
		if len(lane.queue) == 0 {
			delete(d.lanes, key)
			d.mu.Unlock()
			return
		}
		run = lane.queue[0]
		lane.queue[0] = nil
		lane.queue = lane.queue[1:]
		d.mu.Unlock()
	}
}

func (d *KeyedDispatcher) runOne(run func()) {
	defer d.pending.Add(-1)
	goroutine.SafeFunc(run)
}
