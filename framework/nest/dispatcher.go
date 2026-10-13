package nest

import (
	"container/heap"
	"context"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	fctx "github.com/tjbdwanghaibo/roost-core/infra/base/fctx"
	"github.com/tjbdwanghaibo/roost-core/infra/base/misc"
	"github.com/tjbdwanghaibo/roost-core/infra/base/worker"
	flog "github.com/tjbdwanghaibo/roost-core/infra/observe/log"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
)

// Dispatcher 是 Nest 的统一准入与派发器：在这里按声明目标建立同 ID 顺序，再把消息交给 short / long / io 三池
// （dispatchQueue）；延时消息先进入按到期时间排序的堆，到期后再走同一准入。
type Dispatcher struct {
	Name          string
	MsgCap        int
	DelayedMsgCap int
	MaxDelay      time.Duration
	queue         *dispatchQueue
	ioConfig      WorkerPoolConfig
	longConfig    WorkerPoolConfig
	remoteHandler func(*Msg)
	workerNum     int
	handler       func(*Msg)
	stageMetrics  bool
	mu            sync.Mutex
	delayed       map[*delayedMsg]struct{}
	delayedHeap   delayedMsgHeap
	delayNotify   chan struct{}
	delayStop     chan struct{}
	delayDone     chan struct{}
	delaySeq      uint64
	stopped       bool
	fenceErr      error
	observeSeq    uint64
	processed     atomic.Uint64
	slow200ms     atomic.Uint64

	// coldTargets 由 NestMgr 装配：统一准入时只读内存判断声明目标是否需要 I/O 阶段预加载。
	coldTargets func(*Msg) bool

	// seriesName 非 nil 表示本派发器以这个名字持有 nest.dispatch.*{dispatcher} 序列
	// （见 dispatcher_series.go）；由 dispatcherSeries.mu 保护。
	seriesName *string
}

// delayedMsg 是延时队列中的一条消息；seq 让同一到期时间的消息保持提交顺序，idx 是堆内下标。
type delayedMsg struct {
	due time.Time
	seq uint64
	msg *Msg
	idx int
}

// delayedMsgHeap 是按 (due, seq) 排序的最小堆，实现 container/heap 接口。
type delayedMsgHeap []*delayedMsg

func (h delayedMsgHeap) Len() int { return len(h) }
func (h delayedMsgHeap) Less(i, j int) bool {
	if h[i].due.Equal(h[j].due) {
		return h[i].seq < h[j].seq
	}
	return h[i].due.Before(h[j].due)
}
func (h delayedMsgHeap) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
	h[i].idx = i
	h[j].idx = j
}
func (h *delayedMsgHeap) Push(x any) {
	item := x.(*delayedMsg)
	item.idx = len(*h)
	*h = append(*h, item)
}
func (h *delayedMsgHeap) Pop() any {
	old := *h
	n := len(old)
	item := old[n-1]
	item.idx = -1
	old[n-1] = nil
	*h = old[:n-1]
	return item
}

// NewDispatcher 构造派发器：workerNum <= 0 取 GOMAXPROCS，msgCap <= 0 取 short 池默认容量；
// 延时队列容量默认跟随 msgCap（未配置时为 10000），最长延时默认 24 小时。
func NewDispatcher(name string, workerNum, msgCap int, handler func(*Msg)) *Dispatcher {
	ret := &Dispatcher{
		Name:      name,
		MsgCap:    msgCap,
		workerNum: workerNum,
		handler:   handler,
	}
	if ret.workerNum <= 0 {
		ret.workerNum = runtime.GOMAXPROCS(0)
	}
	// 即时业务池预算与延时任务预算分开；提高业务池缺省不连带扩大延时驻留。
	ret.DelayedMsgCap = ret.MsgCap
	if ret.DelayedMsgCap <= 0 {
		ret.DelayedMsgCap = 10000
	}
	if ret.MsgCap <= 0 {
		ret.MsgCap = DefaultShortQueueCapacity
	}
	ret.MaxDelay = 24 * time.Hour
	return ret
}

// ConfigureDelayedAdmission 设置延时队列容量与最长延时；非正值保持原值。
func (m *Dispatcher) ConfigureDelayedAdmission(capacity int, maxDelay time.Duration) {
	if m == nil {
		return
	}
	if capacity > 0 {
		m.DelayedMsgCap = capacity
	}
	if maxDelay > 0 {
		m.MaxDelay = maxDelay
	}
}

// OnInit 初始化延时队列并启动延时泵，按配置（未配置取默认值）建立 short / io / long 三池。
func (m *Dispatcher) OnInit() {
	m.mu.Lock()
	m.delayed = make(map[*delayedMsg]struct{})
	m.delayedHeap = nil
	m.delayNotify = make(chan struct{}, 1)
	m.delayStop = make(chan struct{})
	m.delayDone = make(chan struct{})
	m.stopped = false
	m.fenceErr = nil
	m.mu.Unlock()
	go m.delayLoop()

	io := m.ioConfig
	if io.Workers <= 0 {
		io.Workers = max(32, m.workerNum*4)
	}
	if io.QueueCap <= 0 {
		io.QueueCap = 64
	}
	long := m.longConfig
	if long.Workers <= 0 {
		long.Workers = max(1, m.workerNum)
	}
	if long.QueueCap <= 0 {
		long.QueueCap = 4096
	}
	m.queue = newDispatchQueue(m.Name, WorkerPoolConfig{Workers: m.workerNum, QueueCap: m.MsgCap}, io, m.handler, m.remoteHandler, long)
	m.holdSeries()
}

// Fence 停止准入，worker 的停止留给 OnDestroy。已排队的消息由 NestDispatch 在加载 / 修改实体之前拒绝。首个错误胜出。
func (m *Dispatcher) Fence(err error) {
	if m == nil || err == nil {
		return
	}
	m.mu.Lock()
	if m.fenceErr == nil {
		m.fenceErr = err
	}
	m.mu.Unlock()
}

// OnRun 启动三池 worker。
func (m *Dispatcher) OnRun() {
	m.queue.start()
}

// OnDestroy 以进程基础 ctx 停止派发器，中断时只记录告警。
func (m *Dispatcher) OnDestroy() {
	if err := m.OnDestroyWithContext(fctx.BaseContext()); err != nil {
		flog.NewELog().Title("nest").Warn("dispatcher stop interrupted", "err", err)
	}
}

// OnDestroyWithContext 停止准入与延时泵，以 ErrNestStopped 回复尚未到期的延时消息，然后在 ctx 内排空三池。
// 排空超时返回 ctx 错误，worker 与指标序列保留，可用新 ctx 重试。
func (m *Dispatcher) OnDestroyWithContext(ctx context.Context) error {
	if ctx == nil {
		ctx = fctx.BaseContext()
	}
	m.mu.Lock()
	m.stopped = true
	delayed := m.delayed
	m.delayed = make(map[*delayedMsg]struct{})
	m.delayedHeap = nil
	stop := m.delayStop
	done := m.delayDone
	m.delayStop = nil
	m.mu.Unlock()

	if stop != nil {
		close(stop)
	}
	if done != nil {
		select {
		case <-done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	m.mu.Lock()
	if m.delayDone == done {
		m.delayDone = nil
	}
	m.mu.Unlock()
	// 延时泵已退出，且在 m.mu 下取走队列，这份快照是独占的：一条消息要么在这里，要么已被弹出，不会两者都是。
	// 所以这里的回复是它唯一会得到的回复，必须发出：只回收不回复的同步请求会让调用方等到自己超时，把“Nest 已停止”
	// 报成取消。与准入路径对停止后消息的回答相同，都是 ErrNestStopped。
	for dm := range delayed {
		if dm.msg != nil && dm.msg.RetChan != nil {
			// 回复通道恰好有一个缓冲位（GenSyncMsg），发送不会阻塞；清空它保证即使消息被复用也至多回复一次。
			dm.msg.RetChan <- ErrNestStopped
			dm.msg.RetChan = nil
		} else if dm.msg != nil {
			logAsyncDispatchFailure(dm.msg, ErrNestStopped)
		}
		recycleMsg(dm.msg)
	}

	if err := m.queue.stop(ctx); err != nil {
		// worker 仍在运行并上报指标：序列保留，直到重试看到排空。
		return err
	}
	m.releaseSeries()
	return nil
}

// hashKey 把实体 ID 散列到 worker 槽位。
func hashKey(key int64) uint64 {
	return misc.Hash64(uint64(key))
}

const (
	// MaxBroadcastIdNum 是广播拆批时每批的最大 ID 数。
	MaxBroadcastIdNum = 32
	// spliceDenseGroupLimit 以内的锁组用定长数组分桶，更大的组号用 map 分桶。
	spliceDenseGroupLimit = 8
)

// TrySendMsg 把 msg 的所有权交给派发器。选定的池不能接收时返回准入错误；失败的消息在返回前已释放，
// 异步消息（无 RetChan）的失败另记日志。
func (m *Dispatcher) TrySendMsg(msg *Msg) error {
	if msg == nil {
		return ErrInvalidMessage
	}
	trace := newNestTraceEventInfo(msg)
	m.mu.Lock()
	stopped := m.stopped
	fenced := m.fenceErr
	m.mu.Unlock()
	if fenced != nil {
		emitNestTraceEventInfo(trace, "enqueue", "fenced", 0)
		if msg.RetChan == nil {
			logAsyncDispatchFailure(msg, fenced)
		}
		recycleMsg(msg)
		return fenced
	}
	if stopped {
		emitNestTraceEventInfo(trace, "enqueue", "stopped", 0)
		if msg.RetChan == nil {
			logAsyncDispatchFailure(msg, ErrNestStopped)
		}
		recycleMsg(msg)
		return ErrNestStopped
	}
	msg.stageMetrics = m.stageMetrics
	msg.queuedAt = startNestStage(m.stageMetrics)
	msg.OnSend()
	// I/O 阶段三个来源：显式 SendOptionIO、Remote，以及声明目标中有未加载的冷实体（业务不必知道目标冷热；
	// 只经 Getter 的 LoadedChecker 判定、不调用 Get）。同 ID 顺序在这里统一建立，与走哪个池无关。
	prepareIO := m.remoteHandler != nil && (msg.Cost || needsRemoteStage(msg) || (m.coldTargets != nil && m.coldTargets(msg)))
	err := m.queue.admit(msg, prepareIO)
	if err != nil {
		emitNestTraceEventInfo(trace, "enqueue", "error", 0)
		if msg.RetChan == nil {
			logAsyncDispatchFailure(msg, err)
		}
		msg.OnRelease()
	} else {
		emitNestTraceEventInfo(trace, "enqueue", "ok", 0)
	}
	m.observeStatsIfDue()
	return err
}

// sendMsg 是 TrySendMsg 的内部版本：准入失败时把错误回复到同步调用方的 RetChan。
func (m *Dispatcher) sendMsg(msg *Msg) {
	if msg == nil {
		return
	}
	retChan := msg.RetChan
	if err := m.TrySendMsg(msg); err != nil {
		if retChan != nil {
			retChan <- err
		}
	}
}

// dispatcherObserveStatsEvery 是每多少次准入上报一次池指标。
const dispatcherObserveStatsEvery = 1024

// observeStatsIfDue 按准入计数抽样上报池指标，避免每条消息都取快照。
func (m *Dispatcher) observeStatsIfDue() {
	if m == nil {
		return
	}
	seq := atomic.AddUint64(&m.observeSeq, 1)
	if seq%dispatcherObserveStatsEvery == 0 {
		m.observeStats()
	}
}

func (m *Dispatcher) observeStats() {
	if m == nil {
		return
	}
	short, io, continuations, details := m.queue.snapshotStats()
	delayed := m.delayedCount()
	// 只在持有序列期间上报：与释放竞争的上报不能重新建出释放已删除的序列。
	m.reportSeries(func(name string) {
		observePoolStats(name, "short", short)
		observePoolStats(name, "io", io)
		observePoolStats(name, "long", details.LongPool)
		metrics.SetGauge("nest.dispatch.short_continuations", metrics.Labels{"dispatcher": name}, int64(continuations))
		metrics.SetGauge("nest.dispatch.delayed_messages", metrics.Labels{"dispatcher": name}, int64(delayed))
	})
}

func observePoolStats(dispatcher, poolName string, stats worker.PoolStats) {
	labels := metrics.Labels{
		"dispatcher": dispatcher,
		"pool":       poolName,
	}
	metrics.SetGauge("nest.dispatch.queue_len", labels, int64(stats.QueueLen))
	metrics.SetGauge("nest.dispatch.worker_num", labels, int64(stats.WorkerNum))
}

func (m *Dispatcher) delayedCount() int {
	if m == nil {
		return 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.delayed)
}

// logAsyncDispatchFailure 记录没有回复通道的异步消息的准入失败。
func logAsyncDispatchFailure(msg *Msg, err error) {
	if msg == nil || err == nil {
		return
	}
	flog.NewELog().Title("nest").Warn("async dispatch failed",
		"handler", msg.Name,
		"type", msg.Type.String(),
		"key", msg.Key(),
		"tid", msg.Tid,
		"tids", len(msg.Tids),
		"groups", len(msg.GroupTIds),
		"cost", msg.Cost,
		"remote", msg.HasRemote,
		"err", err,
	)
}

// delaySendMsg 是 TryDelaySendMsg 的内部版本：准入失败时把错误回复到同步调用方的 RetChan。
func (m *Dispatcher) delaySendMsg(delay time.Duration, msg *Msg) {
	if msg == nil {
		return
	}
	retChan := msg.RetChan
	if err := m.TryDelaySendMsg(delay, msg); err != nil && retChan != nil {
		retChan <- err
	}
}

// TryDelaySendMsg 把 msg 的所有权交给延时队列。到期时才进入 worker 池准入；那时原调用方已不在等待，
// 届时的过载只能经 Nest 指标与日志观察。delay <= 0 等同 TrySendMsg。
func (m *Dispatcher) TryDelaySendMsg(delay time.Duration, msg *Msg) error {
	if msg == nil {
		return ErrInvalidMessage
	}
	if delay <= 0 {
		return m.TrySendMsg(msg)
	}
	if m.MaxDelay > 0 && delay > m.MaxDelay {
		recycleMsg(msg)
		return ErrDelayTooLong
	}
	dm := &delayedMsg{
		due: time.Now().Add(delay),
		msg: msg,
	}
	m.mu.Lock()
	if m.fenceErr != nil {
		fenced := m.fenceErr
		m.mu.Unlock()
		if msg.RetChan == nil {
			logAsyncDispatchFailure(msg, fenced)
		}
		recycleMsg(msg)
		return fenced
	}
	if m.stopped {
		m.mu.Unlock()
		if msg.RetChan == nil {
			logAsyncDispatchFailure(msg, ErrNestStopped)
		}
		recycleMsg(msg)
		return ErrNestStopped
	}
	if m.DelayedMsgCap > 0 && len(m.delayed) >= m.DelayedMsgCap {
		m.mu.Unlock()
		if msg.RetChan == nil {
			logAsyncDispatchFailure(msg, ErrQueueFull)
		}
		recycleMsg(msg)
		return ErrQueueFull
	}
	m.delaySeq++
	dm.seq = m.delaySeq
	m.delayed[dm] = struct{}{}
	heap.Push(&m.delayedHeap, dm)
	notify := m.delayNotify
	m.mu.Unlock()
	notifyDelayLoop(notify)
	return nil
}

func notifyDelayLoop(ch chan struct{}) {
	if ch == nil {
		return
	}
	select {
	case ch <- struct{}{}:
	default:
	}
}

// delayLoop 是延时泵：取出到期消息送入准入；队列为空时等待新消息通知，最早的消息未到期时等到期或新消息通知。
// 收到停止信号后退出并关闭 delayDone。
func (m *Dispatcher) delayLoop() {
	defer func() {
		m.mu.Lock()
		done := m.delayDone
		m.mu.Unlock()
		if done != nil {
			close(done)
		}
	}()
	for {
		due, wait, stop, notify := m.nextDelayedWait()
		switch {
		case stop == nil:
			return
		case due != nil:
			m.sendMsg(due.msg)
		case wait < 0:
			select {
			case <-notify:
			case <-stop:
				return
			}
		case wait > 0:
			timer := time.NewTimer(wait)
			select {
			case <-timer.C:
			case <-notify:
			case <-stop:
				timer.Stop()
				return
			}
			timer.Stop()
		}
	}
}

// nextDelayedWait 在 m.mu 下决定延时泵的下一步，返回 (到期消息, 等待时长, 停止通道, 通知通道)：
//   - 已停止：stop 为 nil；
//   - 队列为空：wait 为 -1；
//   - 最早的消息未到期：wait 为剩余时长；
//   - 有到期消息：弹出并返回它，wait 为 0。
func (m *Dispatcher) nextDelayedWait() (*delayedMsg, time.Duration, chan struct{}, chan struct{}) {
	m.mu.Lock()
	defer m.mu.Unlock()
	stop := m.delayStop
	notify := m.delayNotify
	if m.stopped {
		return nil, 0, nil, notify
	}
	if len(m.delayedHeap) == 0 {
		return nil, -1, stop, notify
	}
	next := m.delayedHeap[0]
	wait := time.Until(next.due)
	if wait > 0 {
		return nil, wait, stop, notify
	}
	dm := heap.Pop(&m.delayedHeap).(*delayedMsg)
	delete(m.delayed, dm)
	return dm, 0, stop, notify
}

// spliceSparseGroupSlots 是组号不小于 spliceDenseGroupLimit 的锁组按 worker 槽位分的 ID 批与原始下标批。
type spliceSparseGroupSlots struct {
	group    int
	slots    [][]int64
	idxSlots [][]int
}

// spliceGroupBuckets 是 ForEachSpliceBatch 的分桶：小组号用定长数组，大组号用 sparse + 索引。
type spliceGroupBuckets struct {
	dense       [spliceDenseGroupLimit][][]int64
	denseIdx    [spliceDenseGroupLimit][][]int
	sparse      []spliceSparseGroupSlots
	sparseIndex map[int]int
}

// getGroupSlots 返回锁组 group 的槽位批（按需创建）。
func (m *Dispatcher) getGroupSlots(b *spliceGroupBuckets, group int) (*[][]int64, *[][]int) {
	if group >= 0 && group < spliceDenseGroupLimit {
		if b.dense[group] == nil {
			b.dense[group] = make([][]int64, m.workerNum)
			b.denseIdx[group] = make([][]int, m.workerNum)
		}
		return &b.dense[group], &b.denseIdx[group]
	}
	if b.sparseIndex == nil {
		b.sparseIndex = make(map[int]int, 2)
	}
	if idx, ok := b.sparseIndex[group]; ok {
		return &b.sparse[idx].slots, &b.sparse[idx].idxSlots
	}
	b.sparse = append(b.sparse, spliceSparseGroupSlots{
		group:    group,
		slots:    make([][]int64, m.workerNum),
		idxSlots: make([][]int, m.workerNum),
	})
	idx := len(b.sparse) - 1
	b.sparseIndex[group] = idx
	return &b.sparse[idx].slots, &b.sparse[idx].idxSlots
}

// flushGroupSlots 交出一个锁组中所有非空的槽位批并清空。
func (m *Dispatcher) flushGroupSlots(group int, slots [][]int64, idxSlots [][]int, emit func(group int, batch []int64, origIndices []int)) {
	for i := range slots {
		if len(slots[i]) == 0 {
			continue
		}
		emit(group, slots[i], idxSlots[i])
		slots[i] = nil
		idxSlots[i] = nil
	}
}

// ForEachSpliceBatch 按锁组与 worker 散列把广播 ID 拆成批（每批至多 MaxBroadcastIdNum 个），emit 同时收到每个 ID
// 在原列表中的下标。
func (m *Dispatcher) ForEachSpliceBatch(ids []int64, emit func(group int, batch []int64, origIndices []int)) {
	if len(ids) == 0 || emit == nil {
		return
	}
	var buckets spliceGroupBuckets
	for origIdx, id := range ids {
		group := entity.GetEntityGroup(id)
		slots, idxSlots := m.getGroupSlots(&buckets, group)
		slot := int(hashKey(id) % uint64(m.workerNum))
		batch := (*slots)[slot]
		idxBatch := (*idxSlots)[slot]
		if batch == nil {
			batch = make([]int64, 0, MaxBroadcastIdNum)
			idxBatch = make([]int, 0, MaxBroadcastIdNum)
		}
		batch = append(batch, id)
		idxBatch = append(idxBatch, origIdx)
		if len(batch) == MaxBroadcastIdNum {
			emit(group, batch, idxBatch)
			(*slots)[slot] = nil
			(*idxSlots)[slot] = nil
			continue
		}
		(*slots)[slot] = batch
		(*idxSlots)[slot] = idxBatch
	}
	for group := 0; group < spliceDenseGroupLimit; group++ {
		m.flushGroupSlots(group, buckets.dense[group], buckets.denseIdx[group], emit)
	}
	for i := range buckets.sparse {
		m.flushGroupSlots(buckets.sparse[i].group, buckets.sparse[i].slots, buckets.sparse[i].idxSlots, emit)
	}
}
