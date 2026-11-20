package policy

import (
	"cmp"
	"slices"

	"github.com/tjbdwanghaibo/roost-core/infra/base/spatial"
)

// aoiKDTree 为低移动场景索引点。put/drop/prepare 属于独占写阶段；
// prepare 后可并发查询，调用方须在下次写阶段前结束读阶段。
type aoiKDTree struct {
	points  map[int64]spatial.Point // 上次 prepare 时的位置。
	pending map[int64]aoiKDChange   // 只保留相对 points 的净变化。
	nodes   []aoiKDNode             // 每段的中项为根，左右两段分别是它的子树。
}

type aoiKDChange struct {
	at   spatial.Point
	drop bool
}

type aoiKDPoint struct {
	id int64
	at spatial.Point
}

type aoiKDNode struct {
	aoiKDPoint
	lo, hi spatial.Point // 子树实际点的闭包围盒；不必给 MaxInt64 加一。
}

func newAOIKDTree() *aoiKDTree {
	return &aoiKDTree{points: make(map[int64]spatial.Point), pending: make(map[int64]aoiKDChange)}
}

func (t *aoiKDTree) put(id int64, at spatial.Point) {
	if prev, ok := t.points[id]; ok && prev == at {
		delete(t.pending, id)
		return
	}
	t.pending[id] = aoiKDChange{at: at}
}

func (t *aoiKDTree) drop(id int64) {
	if _, ok := t.points[id]; !ok {
		delete(t.pending, id)
		return
	}
	t.pending[id] = aoiKDChange{drop: true}
}

// 同一轴上的重合坐标按唯一 ID 破平，使中位数两边的元素数相差至多一。
func aoiKDCompare(a, b aoiKDPoint, byY bool) int {
	v, w := a.at.X, b.at.X
	if byY {
		v, w = a.at.Y, b.at.Y
	}
	if c := cmp.Compare(v, w); c != 0 {
		return c
	}
	return cmp.Compare(a.id, b.id)
}

func (t *aoiKDTree) prepare() {
	if len(t.pending) == 0 {
		return
	}
	for id, change := range t.pending {
		if change.drop {
			delete(t.points, id)
		} else {
			t.points[id] = change.at
		}
	}
	// 不在以读为主的阶段保留首次全量登记的大哈希表。
	t.pending = make(map[int64]aoiKDChange)
	n := len(t.points)
	if n == 0 {
		t.nodes = nil
		return
	}
	// 两种轴序各排一次。每层只稳定分区，不对每个子树重新排序。
	x := make([]aoiKDPoint, 0, n)
	for id, at := range t.points {
		x = append(x, aoiKDPoint{id: id, at: at})
	}
	y := slices.Clone(x)
	slices.SortFunc(x, func(a, b aoiKDPoint) int { return aoiKDCompare(a, b, false) })
	slices.SortFunc(y, func(a, b aoiKDPoint) int { return aoiKDCompare(a, b, true) })
	t.nodes = slices.Grow(t.nodes[:0], n)[:n]
	aoiKDBuild(t.nodes, x, y, make([]aoiKDPoint, n), false)
}

// axis、cross 分别按当前轴、另一轴有序。交替轴时，稳定分区后的 scratch
// 成为下一层的 axis，原 axis 的左右段成为 cross，旧 cross 可用作暂存。
func aoiKDBuild(nodes []aoiKDNode, axis, cross, scratch []aoiKDPoint, byY bool) {
	n := len(nodes)
	if n == 0 {
		return
	}
	mid := n / 2
	pivot := axis[mid]
	x, y := axis, cross
	if byY {
		x, y = y, x
	}
	nodes[mid] = aoiKDNode{
		aoiKDPoint: pivot,
		lo:         spatial.Point{X: x[0].at.X, Y: y[0].at.Y},
		hi:         spatial.Point{X: x[n-1].at.X, Y: y[n-1].at.Y},
	}
	if n == 1 {
		return
	}
	left, right := 0, mid+1
	for _, p := range cross {
		switch c := aoiKDCompare(p, pivot, byY); {
		case c < 0:
			scratch[left] = p
			left++
		case c > 0:
			scratch[right] = p
			right++
		}
	}
	aoiKDBuild(nodes[:mid], scratch[:mid], axis[:mid], cross[:mid], !byY)
	aoiKDBuild(nodes[mid+1:], scratch[mid+1:], axis[mid+1:], cross[mid+1:], !byY)
}

// appendRect 保留 dst 前缀，新增部分每个 ID 至多一次，不承诺结果顺序。
// 查询只访问已构建的数组，dst 由调用方独占。
func (t *aoiKDTree) appendRect(dst []int64, box spatial.Rect) []int64 {
	box = spatial.NormalizeRect(box)
	if box.Min.X == box.Max.X || box.Min.Y == box.Max.Y {
		return dst
	}
	return aoiKDAppend(dst, t.nodes, box)
}

func aoiKDAppend(dst []int64, nodes []aoiKDNode, box spatial.Rect) []int64 {
	if len(nodes) == 0 {
		return dst
	}
	mid := len(nodes) / 2
	p := &nodes[mid]
	if p.hi.X < box.Min.X || p.hi.Y < box.Min.Y || p.lo.X >= box.Max.X || p.lo.Y >= box.Max.Y {
		return dst // 整棵子树不交。
	}
	if p.lo.X >= box.Min.X && p.lo.Y >= box.Min.Y && p.hi.X < box.Max.X && p.hi.Y < box.Max.Y {
		for i := range nodes {
			dst = append(dst, nodes[i].id)
		}
		return dst // 完全覆盖，只枚举命中结果。
	}
	if p.at.X >= box.Min.X && p.at.X < box.Max.X && p.at.Y >= box.Min.Y && p.at.Y < box.Max.Y {
		dst = append(dst, p.id)
	}
	dst = aoiKDAppend(dst, nodes[:mid], box)
	return aoiKDAppend(dst, nodes[mid+1:], box)
}
