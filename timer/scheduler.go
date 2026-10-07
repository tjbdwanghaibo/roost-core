// Package timer 是实体自己的定时器调度：一个不加锁的最小堆，由宿主在自己的锁 / 事务里调用 Tick 驱动。
//
// 触发顺序（维护者决定 D-L1，2026-10-06）：先比期限 End，期限相同再比 Node.Priority（数值小的先触发，缺省 0），
// priority 也相同时按登记顺序，也就是节点 ID（同一个 Scheduler 的 ID 单调递增；ChangeTimer 改期、handler
// 按返回值重排都保留原 ID，所以“登记顺序”指最初登记的顺序）。宿主从存储重建时同样按这三个键排序，与存储
// 的遍历顺序无关。需要在同一时刻先于 / 后于别的定时器触发的类型用 NewTimerWithPriority 指定；不指定的保持 0。
//
// 没有 handler 的类型（维护者决定 D-L2）：到期节点的类型没有注册 handler 时，节点照旧删除（并发持久化删除），
// 同时打一条 Warn 并计 timer.unhandled_dropped_total{kind=<类型号>}——下线一种定时器类型时运维能看见丢了什么。
// 宿主在加载后、注册完 handler 时调用 ReportUnhandledTypes，对没有 handler 的存量类型每种告警一次。类型号是
// 代码里的常量，标签基数等于定义过的类型数。
//
// 持久化：typed 定时器的每次增、删、改经 ChangeFunc 交给宿主写存储；闭包定时器纯内存，不持久化、不进快照。
package timer

import (
	"container/heap"
	"github.com/tjbdwanghaibo/roost-core/clock"
	"log/slog"
	"slices"
	"strconv"
	"time"

	"github.com/tjbdwanghaibo/roost-core/metrics"
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

	index   int
	handler Handler
}

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

	running  bool
	deferred []func()
	clock    func() time.Time
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
		s.push(&node)
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
// timers run on the business clock (D-L3), so the default is the process
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
// Tick 期间（handler 里）调用时：目标仍在堆里（包括本次 Tick 也已到期、还没轮到的）就立即出堆，
// 不能推迟到 Tick 结束——那样它会先按旧期限触发（RR-20261005-NC-141）。目标不在堆里时——正在触发的
// 定时器自己，或本次 Tick 里刚新建、尚未入堆的——推迟到最外层 Tick 结束执行，此时返回 true 只表示“已接受”。
func (s *Scheduler) RemoveTimer(id int64) bool {
	if s == nil || id <= 0 {
		return false
	}
	node := s.byID[id]
	if node == nil {
		if s.running {
			s.deferred = append(s.deferred, func() { s.RemoveTimer(id) })
			return true
		}
		return false
	}
	heap.Remove(&s.nodes, node.index)
	delete(s.byID, id)
	if node.Type != TypeClosure {
		s.emit(ChangeDelete, *node)
	}
	return true
}

// ChangeTimer 把定时器改到 now+delay（now 为零值时取注入时钟）。
//
// Tick 期间调用、目标仍在堆里时，立即把它移出堆，推迟到最外层 Tick 结束再按新期限放回：既不会在本次
// Tick 按旧期限触发（RR-20261005-NC-141），也不会在本次 Tick 按新期限触发——两个 handler 互相改期
// 不会在一次 Tick 里循环。移出期间不发持久化变化，放回时发一次 upsert。目标不在堆里（正在触发的自己、
// 本次 Tick 新建的）时整个操作推迟，与之前相同。
func (s *Scheduler) ChangeTimer(id int64, now time.Time, delay time.Duration) bool {
	if s == nil || id <= 0 || delay <= 0 {
		return false
	}
	if now.IsZero() {
		now = s.now()
	}
	node := s.byID[id]
	if node == nil {
		if s.running {
			s.deferred = append(s.deferred, func() { s.ChangeTimer(id, now, delay) })
			return true
		}
		return false
	}
	if s.running {
		heap.Remove(&s.nodes, node.index)
		delete(s.byID, id)
		s.deferred = append(s.deferred, func() {
			node.Delay = delay
			node.End = now.Add(delay)
			s.push(node)
			if node.Type != TypeClosure {
				s.emit(ChangeUpsert, *node)
			}
		})
		return true
	}
	node.Delay = delay
	node.End = now.Add(delay)
	heap.Fix(&s.nodes, node.index)
	if node.Type != TypeClosure {
		s.emit(ChangeUpsert, *node)
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
		s.emit(ChangeDelete, node)
		metrics.IncCounter(InvalidDroppedMetric, nil, 1)
	}
	if now.IsZero() {
		now = s.now()
	}
	// handler 里可以再调 Tick（重入）：内层照常触发到期节点，但只有最外层负责清 running 并执行推迟操作。
	// 内层若也清掉 running，回到外层 handler 后“取消正在触发的自己”就不再推迟、查不到节点而失效，
	// 外层随后又按返回值把它重新排上（RR-20261005-NC-147）。
	if !s.running {
		s.running = true
		defer func() {
			s.running = false
			// 推迟操作按登记顺序执行（推迟的改期之后再取消，结果是取消）；此时 running 已清，它们直接生效。
			for i := 0; i < len(s.deferred); i++ {
				s.deferred[i]()
			}
			clear(s.deferred)
			s.deferred = s.deferred[:0]
		}()
	}

	for {
		node := s.min()
		if node == nil || now.Before(node.End) {
			return
		}
		heap.Pop(&s.nodes)
		delete(s.byID, node.ID)
		if node.Type != TypeClosure {
			s.emit(ChangeDelete, *node)
		}

		next := time.Duration(0)
		if node.Type == TypeClosure {
			if node.handler != nil {
				next = node.handler(Context{OwnerID: s.ownerID, Node: node.clone(), Now: now})
			}
		} else if h := s.handlers[node.Type]; h != nil {
			next = h(Context{OwnerID: s.ownerID, Node: node.clone(), Now: now})
		} else {
			// 没有 handler：节点已经删除（上面的 ChangeDelete），这里只让它被看见（D-L2）。
			// 不保留节点：留在堆顶会挡住之后的所有节点，Tick 就要多一种“越过”状态。
			slog.Warn("timer: dropped a due timer with no handler registered for its type",
				"owner_id", s.ownerID, "timer_id", node.ID, "type", node.Type, "end", node.End)
			metrics.IncCounter(UnhandledDroppedMetric, metrics.Labels{"kind": strconv.FormatInt(int64(node.Type), 10)}, 1)
		}
		if next <= 0 {
			continue
		}
		node.Delay = next
		node.End = now.Add(next)
		s.push(node)
		if node.Type != TypeClosure {
			s.emit(ChangeUpsert, *node)
		}
	}
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
	if s.running {
		s.deferred = append(s.deferred, func() {
			s.addNode(id, delay, timerType, priority, param1, param2, payload, h)
		})
		return id
	}
	s.addNode(id, delay, timerType, priority, param1, param2, payload, h)
	return id
}

func (s *Scheduler) addNode(id int64, delay time.Duration, timerType int32, priority int32, param1 int64, param2 int64, payload []byte, h Handler) {
	node := &Node{
		ID:       id,
		Type:     timerType,
		Priority: priority,
		Param1:   param1,
		Param2:   param2,
		Payload:  append([]byte(nil), payload...),
		End:      s.now().Add(delay),
		Delay:    delay,
		handler:  h,
	}
	s.push(node)
	if timerType != TypeClosure {
		s.emit(ChangeUpsert, *node)
	}
}

func (s *Scheduler) push(node *Node) {
	if node == nil {
		return
	}
	heap.Push(&s.nodes, node)
	s.byID[node.ID] = node
}

func (s *Scheduler) min() *Node {
	if s == nil || len(s.nodes) == 0 {
		return nil
	}
	return s.nodes[0]
}

func (s *Scheduler) emit(change ChangeType, node Node) {
	if s != nil && s.onChange != nil {
		s.onChange(change, node.clone())
	}
}

func (n Node) clone() Node {
	n.Payload = append([]byte(nil), n.Payload...)
	n.index = -1
	return n
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
	h[i].index = i
	h[j].index = j
}
func (h *timerHeap) Push(x any) {
	node := x.(*Node)
	node.index = len(*h)
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
