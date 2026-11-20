package policy

import (
	"cmp"
	"fmt"
	"math"
	"math/rand/v2"
	"slices"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/infra/base/spatial"
)

func crossTestBox(x0, y0, x1, y1 int64) spatial.Rect {
	return spatial.Rect{Min: spatial.Point{X: x0, Y: y0}, Max: spatial.Point{X: x1, Y: y1}}
}

// 真值模型仅用 map、Rect.Contains 与 ID 排序，不借用索引的过滤、去重或排序代码。
func crossTestQuery(t *testing.T, c *aoiCrossList, model map[int64]spatial.Point, box spatial.Rect) {
	t.Helper()
	prefix := []int64{-1111, -2222}
	dst := make([]int64, len(prefix), len(prefix)+len(model))
	copy(dst, prefix)
	got := c.appendRect(dst, box)
	if len(got) < len(prefix) || !slices.Equal(got[:len(prefix)], prefix) {
		t.Fatalf("box=%v: prefix changed: %v", box, got)
	}
	got = got[len(prefix):]
	var want []int64
	for id, at := range model {
		if box.Contains(at) {
			want = append(want, id)
		}
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("box=%v: got %v, want %v", box, got, want)
	}
}

// 从独立模型推导轴序，验证两条底链均无缺失/重复、引用同一节点、前后链接对称。
// 每个上层也必须恰为底链按节点高度取出的子序列。
func crossTestLinks(t *testing.T, c *aoiCrossList, model map[int64]spatial.Point) {
	t.Helper()
	if len(c.nodes) != len(model) {
		t.Fatalf("index size %d, want %d", len(c.nodes), len(model))
	}
	var ids []int64
	for id, at := range model {
		n := c.nodes[id]
		if n == nil || n.id != id || n.at != at || len(n.links) == 0 || len(n.links) > len(c.head.links) {
			t.Fatalf("missing/invalid node id=%d at=%v", id, at)
		}
		ids = append(ids, id)
	}
	for axis := range 2 {
		slices.SortFunc(ids, func(i, j int64) int {
			a, b := model[i], model[j]
			if axis == 0 {
				return cmp.Or(cmp.Compare(a.X, b.X), cmp.Compare(i, j))
			}
			return cmp.Or(cmp.Compare(a.Y, b.Y), cmp.Compare(i, j))
		})
		for level := range c.head.links {
			prev := &c.head
			for _, id := range ids {
				want := c.nodes[id]
				if len(want.links) <= level {
					continue
				}
				if prev.links[level][axis].next != want || want.links[level][axis].prev != prev {
					t.Fatalf("axis=%d level=%d: broken order/link at id=%d", axis, level, id)
				}
				prev = want
			}
			if prev.links[level][axis].next != nil {
				t.Fatalf("axis=%d level=%d: unexpected node beyond tail", axis, level)
			}
		}
	}
}

func TestAOICrossListExtremesAndTies(t *testing.T) {
	const lo, hi int64 = math.MinInt64, math.MaxInt64
	c := newAOICrossList()
	model := map[int64]spatial.Point{
		lo: {X: lo, Y: lo}, hi: {X: lo, Y: lo}, -1: {X: lo, Y: lo}, 0: {},
		1: {X: hi - 1, Y: lo}, 2: {X: hi, Y: lo}, 3: {X: lo, Y: hi}, 4: {X: hi - 1, Y: hi - 1},
	}
	for _, id := range []int64{hi, 3, 2, 4, 1, 0, -1, lo} {
		c.put(id, model[id])
	}
	c.prepare()
	crossTestLinks(t, c, model)
	for _, box := range []spatial.Rect{
		crossTestBox(lo, lo, lo+1, lo+1), crossTestBox(hi-1, lo, hi, hi),
		crossTestBox(lo, hi-1, hi, hi), crossTestBox(lo, lo, hi, hi),
		crossTestBox(hi, hi, lo, lo), crossTestBox(lo, lo, lo, hi), crossTestBox(lo, hi, hi, hi), {},
	} {
		crossTestQuery(t, c, model, box)
	}
	original := c.nodes[hi]
	model[hi] = spatial.Point{X: hi - 1, Y: hi - 1}
	c.put(hi, model[hi])
	c.put(hi, model[hi])
	if c.nodes[hi] != original {
		t.Fatal("moving must relink the same node")
	}
	c.drop(-1)
	c.drop(-1)
	c.drop(222)
	delete(model, -1)
	c.prepare()
	crossTestQuery(t, c, model, crossTestBox(lo, lo, lo+1, lo+1))
	model[-1] = spatial.Point{X: 0, Y: lo}
	c.put(-1, model[-1])
	c.prepare()
	crossTestLinks(t, c, model)
	crossTestQuery(t, c, model, crossTestBox(lo, lo, hi, hi))
	for id := range model {
		c.drop(id)
	}
	clear(model)
	c.prepare()
	crossTestLinks(t, c, model)
	crossTestQuery(t, c, model, crossTestBox(lo, lo, hi, hi))
}

func TestAOICrossListRandomModel(t *testing.T) {
	r := rand.New(rand.NewPCG(9031, 157))
	coord := func() int64 {
		switch r.IntN(18) {
		case 0:
			return math.MinInt64
		case 1:
			return math.MaxInt64
		}
		return int64(r.IntN(41) - 20)
	}
	c := newAOICrossList()
	model := make(map[int64]spatial.Point)
	for batch := range 300 {
		for range 11 {
			id := int64(r.IntN(181) - 90)
			at := model[id]
			switch r.IntN(7) {
			case 0:
				delete(model, id)
				c.drop(id)
				continue
			case 1:
				at.X = coord()
			case 2:
				at.Y = coord()
			case 3:
				at = spatial.Point{}
			default:
				at = spatial.Point{X: coord(), Y: coord()}
			}
			c.put(id, at)
			model[id] = at
		}
		c.prepare()
		crossTestLinks(t, c, model)
		for range 10 {
			crossTestQuery(t, c, model, crossTestBox(coord(), coord(), coord(), coord()))
		}
		if batch%10 == 0 {
			crossTestQuery(t, c, model, crossTestBox(-1, math.MinInt64, 1, math.MaxInt64))
			crossTestQuery(t, c, model, crossTestBox(math.MinInt64, -1, math.MaxInt64, 1))
		}
	}
}

func TestAOICrossListConcurrentQueries(t *testing.T) {
	c := newAOICrossList()
	model := make(map[int64]spatial.Point)
	for i := int64(0); i < 1024; i++ {
		model[i] = spatial.Point{X: (i * 37) % 101, Y: (i * 73) % 23}
		c.put(i, model[i])
	}
	c.prepare()
	for reader := range 8 {
		t.Run(fmt.Sprintf("reader%d", reader), func(t *testing.T) {
			t.Parallel()
			for i := range 100 {
				x, y := int64((reader+i)%101), int64((reader+i)%23)
				crossTestQuery(t, c, model, crossTestBox(x, 0, x+1, 23))
				crossTestQuery(t, c, model, crossTestBox(0, y, 101, y+1))
			}
		})
	}
}
