package policy

import (
	"hash/maphash"
	"math/bits"

	"github.com/tjbdwanghaibo/roost-core/infra/base/spatial"
)

// 每轴的第 0 层是完整双向链表。上层是它的稀疏子链，用作查询和远移的搜索锚点。
// 两轴各按 (本轴坐标, ID) 排序；同一实体的位置、ID 和各层链接都在同一节点上。
const aoiCrossLevels = 16

type aoiCrossLink struct{ prev, next *aoiCrossNode }

type aoiCrossNode struct {
	id    int64
	at    spatial.Point
	links [][2]aoiCrossLink // [层][轴：0=X，1=Y]
}

type aoiCrossList struct {
	head  aoiCrossNode // 哨兵不是坐标或 ID 的极值；所有 int64 都可登记。
	nodes map[int64]*aoiCrossNode
	seed  maphash.Seed
}

func newAOICrossList() *aoiCrossList {
	return &aoiCrossList{
		head:  aoiCrossNode{links: make([][2]aoiCrossLink, aoiCrossLevels)},
		nodes: make(map[int64]*aoiCrossNode),
		seed:  maphash.MakeSeed(),
	}
}

func crossCoord(at spatial.Point, axis int) int64 {
	if axis == 0 {
		return at.X
	}
	return at.Y
}

func crossLess(left, right *aoiCrossNode, axis int) bool {
	x, y := crossCoord(left.at, axis), crossCoord(right.at, axis)
	return x < y || x == y && left.id < right.id
}

func (n *aoiCrossNode) spliceAfter(prev *aoiCrossNode, level, axis int) {
	next := prev.links[level][axis].next
	n.links[level][axis] = aoiCrossLink{prev: prev, next: next}
	prev.links[level][axis].next = n
	if next != nil {
		next.links[level][axis].prev = n
	}
}

func (n *aoiCrossNode) unlink(level, axis int) {
	link := n.links[level][axis]
	link.prev.links[level][axis].next = link.next
	if link.next != nil {
		link.next.links[level][axis].prev = link.prev
	}
}

func (c *aoiCrossList) insertAxis(n *aoiCrossNode, axis int) {
	prev := &c.head
	for level := len(c.head.links) - 1; level >= 0; level-- {
		for next := prev.links[level][axis].next; next != nil && crossLess(next, n, axis); next = prev.links[level][axis].next {
			prev = next
		}
		if level < len(n.links) {
			n.spliceAfter(prev, level, axis)
		}
	}
}

// 相邻次序没变时不用改链。只修正被越过的层；远移则摘下本轴的节点，
// 从稀疏层重新定位，不沿底链从旧位置一直走到新位置。
func (c *aoiCrossList) moveAxis(n *aoiCrossNode, axis int) {
	const localSteps = 8
	for level := range n.links {
		link := n.links[level][axis]
		prev, next := link.prev, link.next
		steps := 0
		for steps < localSteps && prev != &c.head && crossLess(n, prev, axis) {
			prev = prev.links[level][axis].prev
			steps++
		}
		for steps < localSteps && next != nil && crossLess(next, n, axis) {
			prev, next = next, next.links[level][axis].next
			steps++
		}
		if steps == 0 {
			return // 上层是本层子序列，也无需移动。
		}
		if steps == localSteps {
			for h := range n.links {
				n.unlink(h, axis)
			}
			c.insertAxis(n, axis)
			return
		}
		n.unlink(level, axis)
		n.spliceAfter(prev, level, axis)
	}
}

func (c *aoiCrossList) put(id int64, at spatial.Point) {
	if n := c.nodes[id]; n != nil {
		old := n.at
		n.at = at
		if old.X != at.X {
			c.moveAxis(n, 0)
		}
		if old.Y != at.Y {
			c.moveAxis(n, 1)
		}
		return
	}
	// 每四个节点期望有一个进入下一层，索引占用期望 O(n)，不需批末重建。
	height := min(aoiCrossLevels, 1+bits.TrailingZeros64(maphash.Comparable(c.seed, id))/2)
	n := &aoiCrossNode{id: id, at: at, links: make([][2]aoiCrossLink, height)}
	c.insertAxis(n, 0)
	c.insertAxis(n, 1)
	c.nodes[id] = n
}

func (c *aoiCrossList) drop(id int64) {
	if n := c.nodes[id]; n != nil {
		for level := range n.links {
			for axis := range 2 {
				n.unlink(level, axis)
			}
		}
		delete(c.nodes, id)
	}
}

// 写入已同步维护两条链和锚点。协调者负责隔开写阶段和并发只读阶段。
func (c *aoiCrossList) prepare() {}

func (c *aoiCrossList) lowerBound(axis int, value int64) *aoiCrossNode {
	prev := &c.head
	for level := len(c.head.links) - 1; level >= 0; level-- {
		for next := prev.links[level][axis].next; next != nil && crossCoord(next.at, axis) < value; next = prev.links[level][axis].next {
			prev = next
		}
	}
	return prev.links[0][axis].next
}

// 从两轴的下界同步探到较短投影的末端，只枚举该投影并用另一轴过滤。
// 用实际候选数而非坐标跨度选轴，对密集共线点同样有效。查询游标完全在栈上；
// 各读者独占 dst，仅追加本次命中的 ID（一次一个），不修改原有前缀。
func (c *aoiCrossList) appendRect(dst []int64, box spatial.Rect) []int64 {
	box = spatial.NormalizeRect(box)
	if box.Min.X == box.Max.X || box.Min.Y == box.Max.Y {
		return dst
	}
	start := [2]*aoiCrossNode{c.lowerBound(0, box.Min.X), c.lowerBound(1, box.Min.Y)}
	x, y := start[0], start[1]
	for x != nil && x.at.X < box.Max.X && y != nil && y.at.Y < box.Max.Y {
		x, y = x.links[0][0].next, y.links[0][1].next
	}
	axis := 0
	if x != nil && x.at.X < box.Max.X {
		axis = 1
	}
	upper := crossCoord(box.Max, axis)
	lo, hi := crossCoord(box.Min, 1-axis), crossCoord(box.Max, 1-axis)
	for n := start[axis]; n != nil && crossCoord(n.at, axis) < upper; n = n.links[0][axis].next {
		if v := crossCoord(n.at, 1-axis); v >= lo && v < hi {
			dst = append(dst, n.id)
		}
	}
	return dst
}
