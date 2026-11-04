package bus

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"

	"github.com/tjbdwanghaibo/roost-core/infra/base/goroutine"
	"github.com/tjbdwanghaibo/roost-core/infra/base/worker"
)

// RPC 请求在接收实例上的派发规则（轻量 RPC 与 JetStream RPC 相同）：
//
//   - 带顺序键（信封的 OrderKey）的请求按键排队：同一个键的请求按到达本实例的顺序逐个进 handler，前一个返回、答复发出
//     之后才轮到下一个；不同键之间互不等待。路由 RPC（//roost:rpc route）的键是目标实体 ID，affinity 方法的键是 affinity
//     键，由 servicerpc 的客户端放进请求（WithOrderKey）。
//   - 不带顺序键的请求（不标 route / affinity 的方法，例如建盟 Found、mail）各自执行，同一方法的请求也并行，不承诺顺序。
//   - 本实例在跑与排队的请求合计不超过 rpcDispatcher.limit（Config.RPCMaxPending，缺省 WorkerNum × QueueCap），
//     超出的请求不进 handler：轻量 RPC 答复 errcode.ErrRetryable（确定没执行），JetStream RPC 交还 broker 重投。
//
// 为什么不是“按键哈希到固定 worker”：handler 同步跑完（在 Nest 里等事务提交），还可能同步调用别的实例（联盟入会
// Join 调守卫所在实例的 ReserveGuard）。固定 worker 让无关的键共用一个 worker——一个键的 handler 卡住会挡住同 worker 上
// 的其他键；两个实例互相调用时，A 上卡住的 worker 等 B、B 上卡住的 worker 等 A 派到同一个 worker 上的请求，就成了跨实例
// 的互等，只能等超时。按键排队的做法里一个 handler 只挡住自己的键。此前按方法名选 worker 的做法同一方法完全串行
// （联盟阶段 5 实测 Info 约 100 次/秒、Join 约 13 次/秒、Found 约 9 个/秒，16 个不同联盟也一样）。
//
// 剩下的边界：同一实例上，一个键的 handler 同步调用发往本实例、同一个键的 RPC，会排在自己后面，等到调用方超时——
// 同键请求逐个处理的直接后果，业务不要在 handler 里对同一实体发同步 RPC 回到自己。
//
// goroutine 经 Bus 的调度池 TryGo 起（受池的生命周期管理，Bus 停止排空池时一并等待）：不带键的请求一个 goroutine，
// 带键的请求每个活跃键一个 goroutine，依次处理这个键排队的请求，队列空了就退出。

// errRPCOverloaded 是本实例在跑与排队的 RPC 请求达到上限时的拒绝原因。
var errRPCOverloaded = errors.New("bus: too many rpc requests in flight on this instance")

type orderKeyContextKey struct{}

// WithOrderKey 返回带顺序键的 ctx：用它发出的 RPC（Call / CallTo / CallReliable / CallToReliable）在请求信封里带上 key，
// 接收实例上同一个键的请求按到达顺序逐个进 handler，不同键与不带键的请求并行（派发规则见本文件开头）。
// servicerpc 的路由 RPC 用目标实体 ID、affinity 方法用 affinity 键自动设置，业务一般不必直接调用。空 key 等于不设置。
//
// 顺序只在一个接收实例上成立：发往不同实例的请求、调用方并发发出的请求（到达顺序本身不确定）不在承诺之内。
func WithOrderKey(ctx context.Context, key string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, orderKeyContextKey{}, key)
}

// OrderKeyFromContext 读出 WithOrderKey 设置的顺序键。
func OrderKeyFromContext(ctx context.Context) (string, bool) {
	if ctx == nil {
		return "", false
	}
	key, ok := ctx.Value(orderKeyContextKey{}).(string)
	return key, ok && key != ""
}

// rpcDispatcher 按顺序键派发 RPC 任务（规则见本文件开头）。零值不可用，由 newRPCDispatcher 建立。
type rpcDispatcher struct {
	limit   int64
	pending atomic.Int64 // 已接纳、还没处理完（在跑与排队）的请求数

	mu    sync.Mutex
	lanes map[string]*rpcLane // 活跃的键；条目存在 ⇔ 这个键有一个 goroutine 在处理它的队列
}

// rpcLane 是一个活跃键排队等候的请求（不含正在处理的那个），受 rpcDispatcher.mu 保护。
type rpcLane struct {
	queue []*incomingTask
}

func newRPCDispatcher(limit int) *rpcDispatcher {
	if limit <= 0 {
		limit = 1
	}
	return &rpcDispatcher{limit: int64(limit), lanes: make(map[string]*rpcLane)}
}

// dispatch 接纳一个任务交给 run 执行；返回 nil 表示接纳，任务一定会被执行（池停止也会等它）。拒绝时任务没有执行，
// 由调用方答复或交还 broker。
func (d *rpcDispatcher) dispatch(pool *worker.Pool[*incomingTask], key string, task *incomingTask, run func(*incomingTask)) error {
	if d.pending.Add(1) > d.limit {
		d.pending.Add(-1)
		return errRPCOverloaded
	}
	if key == "" {
		if err := pool.TryGo(task, func(task *incomingTask) { d.runOne(task, run) }); err != nil {
			d.pending.Add(-1)
			return err
		}
		return nil
	}
	// 持 mu 起 goroutine 再登记：新 goroutine 处理完第一个任务后要取 mu 检查队列，这时登记一定已经完成；
	// 排到已有键后面的任务一定会被那个 goroutine 取走（它在 mu 下确认队列为空才删除条目、退出）。
	d.mu.Lock()
	defer d.mu.Unlock()
	if lane := d.lanes[key]; lane != nil {
		lane.queue = append(lane.queue, task)
		return nil
	}
	lane := &rpcLane{}
	if err := pool.TryGo(task, func(first *incomingTask) { d.drain(key, lane, first, run) }); err != nil {
		d.pending.Add(-1)
		return err
	}
	d.lanes[key] = lane
	return nil
}

// drain 依次处理一个键的请求：先处理 first，再取这个键排队的下一个，队列空了删除条目并退出。
func (d *rpcDispatcher) drain(key string, lane *rpcLane, first *incomingTask, run func(*incomingTask)) {
	task := first
	for {
		d.runOne(task, run)
		d.mu.Lock()
		if len(lane.queue) == 0 {
			delete(d.lanes, key)
			d.mu.Unlock()
			return
		}
		task = lane.queue[0]
		lane.queue[0] = nil
		lane.queue = lane.queue[1:]
		d.mu.Unlock()
	}
}

// runOne 执行一个任务。panic 只结束这一个任务：排在同一个键后面的请求照常处理（否则条目留在表里，后到的请求永远排队）。
func (d *rpcDispatcher) runOne(task *incomingTask, run func(*incomingTask)) {
	defer d.pending.Add(-1)
	goroutine.SafeFunc(func() { run(task) })
}
