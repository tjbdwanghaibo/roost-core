package timer

import (
	"container/heap"
	"time"
)

const TypeClosure int32 = 0

type ChangeType uint8

const (
	ChangeUpsert ChangeType = iota + 1
	ChangeDelete
)

type Node struct {
	ID      int64
	Type    int32
	Param1  int64
	Param2  int64
	Payload []byte
	End     time.Time
	Delay   time.Duration

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
		if node.ID <= 0 || node.Type == TypeClosure || node.End.IsZero() {
			continue
		}
		if node.ID > s.seed {
			s.seed = node.ID
		}
		s.push(&node)
	}
	return s
}

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

// SetClock replaces the time source used to stamp new timers' End (default
// time.Now). Hosts that drive Tick with an offset-aware clock (e.g.
// clock.Now under time.logic_offset) must inject the same source here,
// otherwise every new timer is shifted by the offset relative to the ticks.
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
	return time.Now()
}

func (s *Scheduler) RegisterHandler(timerType int32, h Handler) {
	if s == nil || timerType == TypeClosure || h == nil {
		return
	}
	s.handlers[timerType] = h
}

func (s *Scheduler) NewTimer(delay time.Duration, timerType int32, param1 int64, param2 int64, payload []byte) int64 {
	if s == nil || delay <= 0 || timerType == TypeClosure {
		return 0
	}
	return s.add(delay, timerType, param1, param2, payload, nil)
}

func (s *Scheduler) NewClosureTimer(delay time.Duration, h Handler) int64 {
	if s == nil || delay <= 0 || h == nil {
		return 0
	}
	return s.add(delay, TypeClosure, 0, 0, nil, h)
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

func (s *Scheduler) add(delay time.Duration, timerType int32, param1 int64, param2 int64, payload []byte, h Handler) int64 {
	s.seed++
	id := s.seed
	if s.running {
		s.deferred = append(s.deferred, func() {
			s.addNode(id, delay, timerType, param1, param2, payload, h)
		})
		return id
	}
	s.addNode(id, delay, timerType, param1, param2, payload, h)
	return id
}

func (s *Scheduler) addNode(id int64, delay time.Duration, timerType int32, param1 int64, param2 int64, payload []byte, h Handler) {
	node := &Node{
		ID:      id,
		Type:    timerType,
		Param1:  param1,
		Param2:  param2,
		Payload: append([]byte(nil), payload...),
		End:     s.now().Add(delay),
		Delay:   delay,
		handler: h,
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

type timerHeap []*Node

func (h timerHeap) Len() int { return len(h) }
func (h timerHeap) Less(i, j int) bool {
	return h[i].End.Before(h[j].End)
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
