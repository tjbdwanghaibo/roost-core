package nest

import (
	"container/heap"
	"context"
	"github.com/tjbdwanghaibo/roost-core/entity"
	fctx "github.com/tjbdwanghaibo/roost-core/fctx"
	flog "github.com/tjbdwanghaibo/roost-core/log"
	"github.com/tjbdwanghaibo/roost-core/metrics"
	"github.com/tjbdwanghaibo/roost-core/misc"
	"github.com/tjbdwanghaibo/roost-core/worker"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
)

type Dispatcher struct {
	Name          string
	MsgCap        int
	DelayedMsgCap int
	MaxDelay      time.Duration
	queue         *dispatchQueue
	slowConfig    WorkerPoolConfig
	remoteWorkers int
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

	// coldTargets 由 NestMgr 装配：统一准入时只读内存判断声明目标是否需要慢阶段预加载。
	coldTargets func(*Msg) bool
}

type delayedMsg struct {
	due time.Time
	seq uint64
	msg *Msg
	idx int
}

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

func NewDispatcher(name string, workerNum, hbWorkerNum int, msgCap int, handler func(*Msg)) *Dispatcher {
	ret := &Dispatcher{
		Name:      name,
		MsgCap:    msgCap,
		workerNum: workerNum,
		handler:   handler,
	}
	if ret.workerNum <= 0 {
		ret.workerNum = runtime.GOMAXPROCS(0)
	}
	if ret.MsgCap <= 0 {
		ret.MsgCap = 10000
	}
	ret.DelayedMsgCap = ret.MsgCap
	ret.MaxDelay = 24 * time.Hour
	return ret
}

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

	slow := m.slowConfig
	if slow.Workers <= 0 {
		slow.Workers = m.remoteWorkers
	}
	if slow.Workers <= 0 {
		slow.Workers = max(32, m.workerNum*4)
	}
	if slow.QueueCap <= 0 {
		slow.QueueCap = 64
	}
	m.queue = newDispatchQueue(m.Name, WorkerPoolConfig{Workers: m.workerNum, QueueCap: m.MsgCap}, slow, m.handler, m.remoteHandler)

}

// Fence stops admission while leaving worker shutdown to OnDestroy. Messages
// already queued are rejected by NestDispatch before entity loading/mutation.
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

func (m *Dispatcher) OnRun() {
	m.queue.start()
}

func (m *Dispatcher) OnDestroy() {
	if err := m.OnDestroyWithContext(fctx.BaseContext()); err != nil {
		flog.NewELog().Title("nest").Warn("dispatcher stop interrupted", "err", err)
	}
}

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
	// The delay pump has exited, and taking the queue under m.mu made this
	// snapshot exclusive: a message is either here or was already popped, never
	// both. So answering here is the only reply this message will ever get, and
	// it has to happen — an accepted synchronous request that is merely
	// recycled leaves its caller waiting for its own timeout, with the failure
	// reported as a cancellation instead of "the nest stopped"
	// (RR-20260911-04). The admission path has always answered
	// ErrNestStopped; this is the same answer for a message that got in
	// before the stop.
	for dm := range delayed {
		if dm.msg != nil && dm.msg.RetChan != nil {
			// Buffered with room for exactly this one value (GenSyncMsg), so
			// the send cannot block, and clearing it keeps the reply at most
			// once even if the message is reused.
			dm.msg.RetChan <- ErrNestStopped
			dm.msg.RetChan = nil
		} else if dm.msg != nil {
			logAsyncDispatchFailure(dm.msg, ErrNestStopped)
		}
		recycleMsg(dm.msg)
	}

	return m.queue.stop(ctx)
}

func hashKey(key int64) uint64 {
	return misc.Hash64(uint64(key))
}

const (
	MaxBroadcastIdNum     = 32
	spliceDenseGroupLimit = 8
)

// TrySendMsg transfers ownership of msg to the dispatcher. It returns an
// admission error when the selected worker cannot accept the message. A failed
// message is released before the method returns.
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
	// 慢阶段三个来源：显式 SendOptionSlow、Remote，以及声明目标中有未加载的冷实体
	// （RR-20260926-25：业务不必知道目标冷热）。同 ID 顺序在这里统一建立，与走哪个池无关。
	slow := m.remoteHandler != nil && (msg.Cost || needsRemoteStage(msg) || (m.coldTargets != nil && m.coldTargets(msg)))
	err := m.queue.admit(msg, slow)
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

const dispatcherObserveStatsEvery = 1024

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
	fast, slow, continuations := m.queue.stats()
	m.observePoolStats("fast", fast)
	m.observePoolStats("slow", slow)
	metrics.SetGauge("nest.dispatch.fast_continuations", metrics.Labels{"dispatcher": m.Name}, int64(continuations))
	metrics.SetGauge("nest.dispatch.delayed_messages", metrics.Labels{
		"dispatcher": m.Name,
	}, int64(m.delayedCount()))
}

func (m *Dispatcher) observePoolStats(poolName string, stats worker.PoolStats) {
	if m == nil {
		return
	}
	labels := metrics.Labels{
		"dispatcher": m.Name,
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

func (m *Dispatcher) delaySendMsg(delay time.Duration, msg *Msg) {
	if msg == nil {
		return
	}
	retChan := msg.RetChan
	if err := m.TryDelaySendMsg(delay, msg); err != nil && retChan != nil {
		retChan <- err
	}
}

// TryDelaySendMsg transfers ownership of msg to the delay queue. Admission to
// the worker pool happens when the delay expires; a later overload is observed
// through Nest metrics/logging because the original caller is no longer
// waiting at that point.
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
		dm, wait, stop, notify := m.nextDelayedWait()
		if stop != nil && dm == nil && wait < 0 {
			select {
			case <-notify:
				continue
			case <-stop:
				return
			}
		}
		if stop == nil {
			return
		}
		if wait > 0 {
			timer := time.NewTimer(wait)
			select {
			case <-timer.C:
			case <-notify:
			case <-stop:
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				return
			}
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			continue
		}
		if dm == nil {
			continue
		}
		m.sendMsg(dm.msg)
	}
}

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

type spliceSparseGroupSlots struct {
	group    int
	slots    [][]int64
	idxSlots [][]int
}

type spliceGroupBuckets struct {
	dense       [spliceDenseGroupLimit][][]int64
	denseIdx    [spliceDenseGroupLimit][][]int
	sparse      []spliceSparseGroupSlots
	sparseIndex map[int]int
}

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

// ForEachSpliceBatch partitions broadcast IDs into batches by entity group and worker hash.
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
