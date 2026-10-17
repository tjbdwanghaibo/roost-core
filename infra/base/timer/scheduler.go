// Package timer 是实体自己的定时器调度：一个不加锁的最小堆，由宿主在自己的锁 / 事务里调用 Tick 驱动。
//
// 触发顺序（维护者决定，2026-10-06）：先比期限 End，期限相同再比 Node.Priority（数值小的先触发，缺省 0），
// priority 也相同时按登记顺序，也就是节点 ID（同一个 Scheduler 的 ID 单调递增；ChangeTimer 改期、handler
// 按返回值重排都保留原 ID，所以“登记顺序”指最初登记的顺序）。宿主从存储重建时同样按这三个键排序，与存储
// 的遍历顺序无关。需要在同一时刻先于 / 后于别的定时器触发的类型用 NewTimerWithPriority 指定；不指定的保持 0。
//
// 没有 handler 的类型（维护者决定）：到期节点的类型没有注册 handler 时，节点照旧删除（并发持久化删除），
// 同时打一条 Warn 并计 timer.unhandled_dropped_total{kind=<类型号>}——下线一种定时器类型时运维能看见丢了什么。
// 宿主在加载后、注册完 handler 时调用 ReportUnhandledTypes，对没有 handler 的存量类型每种告警一次。类型号是
// 代码里的常量，标签基数等于定义过的类型数。
//
// 持久化：typed 定时器的每次增、删、改经 ChangeFunc 交给宿主写存储；闭包定时器纯内存，不持久化、不进快照。
package timer

import (
	"container/heap"
	"github.com/tjbdwanghaibo/roost-core/infra/base/clock"
	"log/slog"
	"slices"
	"strconv"
	"time"

	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
)

const TypeClosure int32 = 0

// UnhandledDroppedMetric 计数到期时因类型没有注册 handler 而被删除的节点，标签 kind 为类型号。
// 宿主的事务回滚后重试同一次 Tick 会再计一次。
const UnhandledDroppedMetric = "timer.unhandled_dropped_total"

// InvalidDroppedMetric 计正常 Tick 清理的非法持久节点；加载只诊断，不写存储。
const InvalidDroppedMetric = "timer.invalid_dropped_total"

type ChangeType uint8

const (
	ChangeUpsert ChangeType = iota + 1
	ChangeDelete
)

type Node struct {
	ID   int64
	Type int32
	// Priority 只在期限相同时起作用：数值小的先触发，相同时按登记顺序（ID）。缺省 0；
	// 存储里没有这个字段的旧节点按 0 处理。
	Priority int32
	Param1   int64
	Param2   int64
	Payload  []byte
	End      time.Time
	Delay    time.Duration

	handler Handler
	// index 是堆下标（不在堆中为 -1）。用 int32 与下面三个字段共占 8 字节，Node 不因状态字段变大
	// （回调与钩子按值复制 Node，基准见 bench_test.go）。
	index int32
	state nodeState
	// persisted：持久化钩子当前持有这个节点（最近一次回调是 upsert）。只对 typed 节点有意义。
	persisted bool
	// announced：钩子至少收到过一次这个 ID 的 upsert。只有 Tick 内新建、尚未收尾的节点为假。
	announced bool
}

// nodeState 是节点相对调度器的位置。不变量：
//   - byID[id] == n 当且仅当 n.state 是 stateHeap / stateFiring / stateQueued；
//   - n.state == stateHeap 当且仅当 n 在堆的 n.index 位置；
//   - stateFiring / stateQueued 只在 running 期间存在，Tick 外 byID 里全是 stateHeap、queued 为空；
//   - 一次最外层 Tick 内一个节点至多进 queued 一次（queued 只会入堆或 detach，不会回到堆 / 触发中）。
//
// 因为活节点总能按 ID 找到，取消、改期、新建都在调用当下改节点本身，不需要按 ID 推迟到收尾再查。
type nodeState uint8

const (
	// stateDetached：不属于调度器——已取消、到期结束、回调 panic 后丢弃，或调用方 / 克隆出来的值（零值）。
	// 取消不单设状态：detach 之后按 ID 找不到，Tick 期间的操作无效果，所以取消后的改期不会复活节点。
	stateDetached nodeState = iota
	// stateHeap：在堆中等待到期。
	stateHeap
	// stateFiring：已出堆、回调正在执行（含外层重入帧）；出堆时已向钩子发过删除。
	stateFiring
	// stateQueued：等最外层 Tick 收尾入堆。来源：Tick 内新建、Tick 内对堆中节点显式改期、触发中节点显式改期。
	stateQueued
)

type Context struct {
	OwnerID int64
	Node    Node
	Now     time.Time
}

type Handler func(ctx Context) time.Duration
type ChangeFunc func(change ChangeType, node Node)

type Scheduler struct {
	ownerID int64
	seed    int64
	nodes   timerHeap
	invalid []Node
	byID    map[int64]*Node

	handlers map[int32]Handler
	onChange ChangeFunc

	// running 只由最外层 Tick 置位和清除；重入的内层 Tick 不收尾。
	running bool
	// queued 是本次最外层 Tick 里进入 stateQueued 的节点，按进入顺序；其中之后被取消的已 detach，收尾跳过。
	queued []*Node
	clock  func() time.Time
}

func NewScheduler(ownerID int64, seed int64, saved []Node, onChange ChangeFunc) *Scheduler {
	s := &Scheduler{
		ownerID:  ownerID,
		seed:     seed,
		byID:     make(map[int64]*Node, len(saved)),
		handlers: make(map[int32]Handler),
		onChange: onChange,
	}
	for i := range saved {
		node := saved[i].clone()
		if node.ID > s.seed {
			s.seed = node.ID
		}
		reason := ""
		switch {
		case node.ID <= 0:
			reason = "id"
		case node.Type == TypeClosure:
			reason = "type"
		case node.End.IsZero():
			reason = "deadline"
		}
		if reason != "" {
			// 实体加载也会构造 scheduler；只暂存清理，不能在构造期改持久状态。
			s.invalid = append(s.invalid, node)
			slog.Warn("timer: invalid stored timer pending cleanup", "owner_id", ownerID, "timer_id", node.ID, "reason", reason)
			continue
		}
		stored := &node
		stored.persisted, stored.announced = true, true // 从存储加载：钩子已经持有它
		s.byID[stored.ID] = stored
		s.pushHeap(stored)
	}
	return s
}

// NeedsCleanup 表示有非法存量节点等待正常 Tick 删除。宿主即使没有合法到期节点，
// 也应安排一次事务内 Tick；NewScheduler 本身不触发持久化回调。
func (s *Scheduler) NeedsCleanup() bool { return s != nil && len(s.invalid) > 0 }

func (s *Scheduler) OwnerID() int64 {
	if s == nil {
		return 0
	}
	return s.ownerID
}

func (s *Scheduler) Seed() int64 {
	if s == nil {
		return 0
	}
	return s.seed
}

// SetClock replaces the time source used to stamp new timers' End. Game
// timers run on the business clock, so the default is the process
// business clock clock.Now (real time + time.logic_offset). A host that drives
// Tick with its own time must inject the same source here — the World's
// TimerComponent pins both to the tick's / transaction's time — otherwise
// every new timer is shifted relative to the ticks.
func (s *Scheduler) SetClock(now func() time.Time) {
	if s == nil || now == nil {
		return
	}
	s.clock = now
}

func (s *Scheduler) now() time.Time {
	if s != nil && s.clock != nil {
		return s.clock()
	}
	return clock.Now()
}

func (s *Scheduler) RegisterHandler(timerType int32, h Handler) {
	if s == nil || timerType == TypeClosure || h == nil {
		return
	}
	s.handlers[timerType] = h
}

// NewTimer 登记一个 now+delay 到期的 typed 定时器，priority 为 0。
func (s *Scheduler) NewTimer(delay time.Duration, timerType int32, param1 int64, param2 int64, payload []byte) int64 {
	return s.NewTimerWithPriority(delay, timerType, 0, param1, param2, payload)
}

// NewTimerWithPriority 与 NewTimer 相同，另外指定同一期限内的触发优先级：数值小的先触发，
// priority 相同按登记顺序。priority 随节点持久化，改期与重排都保留。
// 期限与 payload 在本次调用中固定；Tick 回调内创建时只推迟入堆，不推迟取时钟和复制输入。
func (s *Scheduler) NewTimerWithPriority(delay time.Duration, timerType int32, priority int32, param1 int64, param2 int64, payload []byte) int64 {
	if s == nil || delay <= 0 || timerType == TypeClosure {
		return 0
	}
	return s.add(delay, timerType, priority, param1, param2, payload, nil)
}

func (s *Scheduler) NewClosureTimer(delay time.Duration, h Handler) int64 {
	if s == nil || delay <= 0 || h == nil {
		return 0
	}
	return s.add(delay, TypeClosure, 0, 0, 0, nil, h)
}

// ReportUnhandledTypes 对堆里没有注册 handler 的 typed 节点类型每种打一条 Warn（带节点数），按类型号升序
// 返回这些类型。只报告：不删除、不计数，节点到期时才按 Tick 的规则删除并计数。宿主在加载存量节点、
// 注册完全部 handler 之后调用一次。
func (s *Scheduler) ReportUnhandledTypes() []int32 {
	if s == nil {
		return nil
	}
	counts := make(map[int32]int)
	for _, node := range s.nodes {
		if node.Type == TypeClosure || s.handlers[node.Type] != nil {
			continue
		}
		counts[node.Type]++
	}
	if len(counts) == 0 {
		return nil
	}
	types := make([]int32, 0, len(counts))
	for timerType := range counts {
		types = append(types, timerType)
	}
	slices.Sort(types)
	for _, timerType := range types {
		slog.Warn("timer: stored timers have no handler registered for their type; they will be dropped when due",
			"owner_id", s.ownerID, "type", timerType, "nodes", counts[timerType])
	}
	return types
}

// RemoveTimer 取消一个定时器；返回 true 之后它不再触发。
//
// 取消在调用当下生效：仍在堆里的（包括本次 Tick 也已到期、还没轮到的）立即出堆并发删除；
// 正在触发的回调节点不再按返回间隔或显式改期放回（出堆时已发过删除）；本次 Tick 里新建或改期、
// 等待收尾放回的节点直接移除，不再放回。对已经不存在的 ID（本轮已触发结束、已取消、从未登记）
// 返回 false，Tick 内外一致：byID 持有全部活节点，“不存在”可以当下准确判断。
func (s *Scheduler) RemoveTimer(id int64) bool {
	if s == nil || id <= 0 {
		return false
	}
	node := s.byID[id]
	if node == nil {
		return false
	}
	if node.state == stateHeap {
		heap.Remove(&s.nodes, int(node.index))
	}
	s.detach(node)
	if !node.announced {
		// Tick 内新建即取消：仍让钩子见到这个 ID 一次。宿主（World）按 upsert 推进持久 seed，
		// 否则重建后会再次分配这个已经交给调用方的 ID。
		s.emitUpsert(node)
	}
	if node.persisted {
		s.emitDelete(node)
	}
	return true
}

// ChangeTimer 把定时器改到 now+delay（now 为零值时取注入时钟）。期限在调用当下计算。
//
// Tick 外：直接调整堆并发一次 upsert。Tick 期间：目标仍在堆里时立即移出堆，等最外层 Tick 收尾再按
// 新期限放回——既不会在本次 Tick 按旧期限触发，也不会按新期限触发，两个 handler 互相改期不会在一次
// Tick 里循环；移出期间不发持久化变化，放回时发一次 upsert（多次改期只发最后一次）。正在触发的回调
// 节点被显式改期时覆盖回调返回值（包括 <=0），同样等收尾放回，重入 Tick 不会提前触发它。Tick 内新建
// 尚未放回的节点只改期限。已取消或已结束的节点不会被改期复活：返回 false，Tick 内外一致。
func (s *Scheduler) ChangeTimer(id int64, now time.Time, delay time.Duration) bool {
	if s == nil || id <= 0 || delay <= 0 {
		return false
	}
	if now.IsZero() {
		now = s.now()
	}
	node := s.byID[id]
	if node == nil {
		return false
	}
	node.Delay = delay
	node.End = now.Add(delay)
	switch node.state {
	case stateHeap:
		if !s.running {
			heap.Fix(&s.nodes, int(node.index))
			s.emitUpsert(node)
			return true
		}
		heap.Remove(&s.nodes, int(node.index))
		s.enqueue(node)
	case stateFiring:
		s.enqueue(node) // 回调返回后看到 state != stateFiring，不再按返回值处理
	case stateQueued:
		// 已在收尾队列：最后一次改期生效。
	}
	return true
}

func (s *Scheduler) Tick(now time.Time) {
	if s == nil {
		return
	}
	invalid := s.invalid
	s.invalid = nil
	for _, node := range invalid {
		s.emit(ChangeDelete, &node)
		metrics.IncCounter(InvalidDroppedMetric, nil, 1)
	}
	if now.IsZero() {
		now = s.now()
	}
	// handler 里可以再调 Tick（重入）：内层照常触发到期节点，只有最外层负责清 running 并收尾。
	// 内层若也收尾，外层回调里显式改期的自己会被提前放回堆、被后续重入 Tick 提前触发。
	if !s.running {
		s.running = true
		defer s.settle()
	}

	for {
		node := s.min()
		if node == nil || now.Before(node.End) {
			return
		}
		if node.Type != TypeClosure && !node.announced {
			// 收尾钩子可重入 Tick，遇到已入堆但尚未通知的子节点。
			// 先通知 ID，让宿主推进持久种子；钩子可能取消或改期，所以重新取堆顶。
			s.emitUpsert(node)
			continue
		}
		heap.Pop(&s.nodes)
		next := s.fire(node, now)
		if node.state != stateFiring {
			continue // 回调里被显式改期（已进收尾队列）或取消（已 detach）：返回值不再生效
		}
		if next <= 0 {
			s.detach(node)
			continue
		}
		node.Delay = next
		node.End = now.Add(next)
		s.pushHeap(node)
		s.emitUpsert(node)
	}
}

// settle 是最外层 Tick 的收尾（defer 执行，回调 panic 时同样执行）。
//
// 先落定再回调：running 清除、queued 取走、待放回的节点全部入堆之后，才按进入队列的顺序对仍在堆里的
// typed 节点各发一次 upsert（携带最终期限）。钩子 panic 只让剩余 upsert 不再回调，调度器状态已经一致，
// 没有可以重放的残留；宿主应把这次 Tick 当作失败（回滚事务并重建 Scheduler）。钩子里调用调度器按
// Tick 外规则立即生效，它移除的节点在这里被跳过。
func (s *Scheduler) settle() {
	s.running = false
	queued := s.queued
	s.queued = nil
	for _, node := range queued {
		if node.state == stateQueued {
			s.pushHeap(node)
		}
	}
	for _, node := range queued {
		if node.state == stateHeap {
			s.emitUpsert(node)
		}
	}
	if s.queued == nil { // 钩子里没有再跑一次最外层 Tick：复用底层数组
		clear(queued)
		s.queued = queued[:0]
	}
}

// fire 执行一个已出堆节点的回调。节点在回调期间是 stateFiring、仍在 byID 里，回调（及其中的重入 Tick）
// 对它的取消 / 改期直接改它的状态。
func (s *Scheduler) fire(node *Node, now time.Time) time.Duration {
	node.state = stateFiring
	returned := false
	defer func() {
		// 删除通知或回调 panic：与正常结束一样不放回（宿主事务负责回滚）；已经发生的显式改期 / 取消保留。
		if !returned && node.state == stateFiring {
			s.detach(node)
		}
	}()
	s.emitDelete(node)
	next := s.call(node, now)
	returned = true
	return next
}

func (s *Scheduler) call(node *Node, now time.Time) time.Duration {
	if node.Type == TypeClosure {
		if node.handler != nil {
			return node.handler(Context{OwnerID: s.ownerID, Node: node.clone(), Now: now})
		}
	} else if h := s.handlers[node.Type]; h != nil {
		return h(Context{OwnerID: s.ownerID, Node: node.clone(), Now: now})
	} else {
		// 没有 handler：节点已经删除，只报告，不让未知类型挡住后续定时器。
		slog.Warn("timer: dropped a due timer with no handler registered for its type",
			"owner_id", s.ownerID, "timer_id", node.ID, "type", node.Type, "end", node.End)
		metrics.IncCounter(UnhandledDroppedMetric, metrics.Labels{"kind": strconv.FormatInt(int64(node.Type), 10)}, 1)
	}
	return 0
}

func (s *Scheduler) NextTime() time.Time {
	if s == nil {
		return time.Time{}
	}
	if node := s.min(); node != nil {
		return node.End
	}
	return time.Time{}
}

func (s *Scheduler) Nodes() []Node {
	if s == nil {
		return nil
	}
	out := make([]Node, 0, len(s.nodes))
	for _, node := range s.nodes {
		if node.Type == TypeClosure {
			continue
		}
		out = append(out, node.clone())
	}
	return out
}

func (s *Scheduler) add(delay time.Duration, timerType int32, priority int32, param1 int64, param2 int64, payload []byte, h Handler) int64 {
	s.seed++
	id := s.seed
	// 登记即取得参数快照：否则后续回调耗时会推迟期限，调用方复用缓冲也会污染持久化。
	node := &Node{
		ID:       id,
		Type:     timerType,
		Priority: priority,
		Param1:   param1,
		Param2:   param2,
		Payload:  append([]byte(nil), payload...),
		End:      s.now().Add(delay),
		Delay:    delay,
		index:    -1,
		handler:  h,
	}
	s.byID[id] = node
	if s.running {
		s.enqueue(node) // 本次 Tick 不触发；收尾入堆并发 upsert
		return id
	}
	s.pushHeap(node)
	s.emitUpsert(node)
	return id
}

func (s *Scheduler) pushHeap(node *Node) {
	node.state = stateHeap
	heap.Push(&s.nodes, node)
}

func (s *Scheduler) enqueue(node *Node) {
	node.state = stateQueued
	s.queued = append(s.queued, node)
}

// detach 把节点移出 byID；调用方已经把它移出堆（或它本来不在堆里）。
func (s *Scheduler) detach(node *Node) {
	delete(s.byID, node.ID)
	node.state = stateDetached
	node.index = -1
}

func (s *Scheduler) min() *Node {
	if s == nil || len(s.nodes) == 0 {
		return nil
	}
	return s.nodes[0]
}

// emitUpsert / emitDelete 先改钩子视图标志再回调：钩子 panic 时标志与已经发出的变化一致。闭包节点不持久化。
func (s *Scheduler) emitUpsert(node *Node) {
	if node.Type == TypeClosure {
		return
	}
	node.persisted, node.announced = true, true
	s.emit(ChangeUpsert, node)
}

func (s *Scheduler) emitDelete(node *Node) {
	if node.Type == TypeClosure {
		return
	}
	node.persisted = false
	s.emit(ChangeDelete, node)
}

func (s *Scheduler) emit(change ChangeType, node *Node) {
	if s != nil && s.onChange != nil {
		s.onChange(change, node.clone())
	}
}

// clone 复制交给回调和钩子的值：payload 独立，私有的调度位置清零（克隆出的值不属于调度器）。
func (n *Node) clone() Node {
	c := *n
	c.Payload = append([]byte(nil), n.Payload...)
	c.index = -1
	c.state = stateDetached
	c.persisted, c.announced = false, false
	return c
}

// timerHeap 按 (End, Priority, ID) 排序，见包注释。ID 唯一，所以顺序是全序，不取决于堆的形状或入堆顺序。
type timerHeap []*Node

func (h timerHeap) Len() int { return len(h) }
func (h timerHeap) Less(i, j int) bool {
	a, b := h[i], h[j]
	if !a.End.Equal(b.End) {
		return a.End.Before(b.End)
	}
	if a.Priority != b.Priority {
		return a.Priority < b.Priority
	}
	return a.ID < b.ID
}
func (h timerHeap) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
	h[i].index = int32(i)
	h[j].index = int32(j)
}
func (h *timerHeap) Push(x any) {
	node := x.(*Node)
	node.index = int32(len(*h))
	*h = append(*h, node)
}
func (h *timerHeap) Pop() any {
	old := *h
	n := len(old)
	node := old[n-1]
	node.index = -1
	old[n-1] = nil
	*h = old[:n-1]
	return node
}
