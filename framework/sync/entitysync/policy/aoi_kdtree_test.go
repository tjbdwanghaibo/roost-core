package policy

import (
	"fmt"
	"math"
	"math/rand/v2"
	"slices"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/infra/base/spatial"
)

func kdTestRect(x1, y1, x2, y2 int64) spatial.Rect {
	return spatial.Rect{Min: spatial.Point{X: x1, Y: y1}, Max: spatial.Point{X: x2, Y: y2}}
}

func kdCheckRect(t *testing.T, tree *aoiKDTree, box spatial.Rect, want ...int64) {
	t.Helper()
	prefix := []int64{-2048, -4096}
	got := tree.appendRect(slices.Clone(prefix), box)
	if len(got) < len(prefix) || !slices.Equal(got[:len(prefix)], prefix) {
		t.Fatalf("box %v: lost prefix: %v", box, got)
	}
	got = got[len(prefix):]
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("box %v: got %v, want %v", box, got, want)
	}
}

func TestAOIKDTreeBatchUpdates(t *testing.T) {
	tree := newAOIKDTree()
	origin, moved := spatial.Point{}, spatial.Point{X: 7, Y: 7}
	near, far := kdTestRect(0, 0, 1, 1), kdTestRect(7, 7, 8, 8)
	tree.prepare()
	kdCheckRect(t, tree, near)
	tree.put(0, origin)
	tree.put(0, moved)
	tree.put(0, origin)
	tree.put(-1, origin)
	if len(tree.nodes) != 0 {
		t.Fatal("put built a tree before prepare")
	}
	tree.prepare()
	kdCheckRect(t, tree, near, -1, 0)
	tree.put(0, origin)
	tree.drop(404)
	tree.put(0, moved)
	tree.put(0, origin)
	tree.drop(-1)
	tree.put(-1, origin)
	tree.put(42, moved)
	tree.drop(42)
	if len(tree.pending) != 0 {
		t.Fatal("no-op writes or cancelled edits dirtied a prepared tree")
	}
	tree.prepare()
	tree.drop(-1)
	tree.drop(-1)
	tree.put(0, moved)
	tree.put(-1, moved)
	tree.put(42, origin)
	tree.drop(42)
	kdCheckRect(t, tree, near, -1, 0) // 查询不懒建，不发布未 prepare 的变更。
	tree.prepare()
	kdCheckRect(t, tree, near)
	kdCheckRect(t, tree, far, -1, 0)
	tree.drop(0)
	tree.drop(-1)
	tree.prepare()
	kdCheckRect(t, tree, far)
	tree.put(-1, origin)
	tree.prepare()
	kdCheckRect(t, tree, near, -1)
}

func TestAOIKDTreeCoincidentAndLimits(t *testing.T) {
	const lo, hi = math.MinInt64, math.MaxInt64
	tree := newAOIKDTree()
	ids := make([]int64, 1024)
	for i := range ids {
		ids[i] = int64(i)
		tree.put(ids[i], spatial.Point{X: lo, Y: hi - 1})
	}
	tree.put(lo, spatial.Point{X: hi - 1, Y: lo})
	tree.put(hi, spatial.Point{X: hi, Y: lo})
	tree.put(-1, spatial.Point{X: lo, Y: hi})
	tree.put(-2, spatial.Point{X: lo + 1, Y: hi - 1})
	tree.prepare()
	kdCheckRect(t, tree, kdTestRect(lo, hi-1, lo+1, hi), ids...)
	kdCheckRect(t, tree, kdTestRect(hi, lo+1, hi-1, lo), lo) // 反向输入沿用 spatial.Rect 语义。
	kdCheckRect(t, tree, kdTestRect(lo, lo, hi, hi), append(slices.Clone(ids), -2, lo)...)
	kdCheckRect(t, tree, kdTestRect(lo, lo, lo, hi))
	kdCheckRect(t, tree, kdTestRect(lo, hi-1, hi, hi-1))
	kdCheckRect(t, tree, spatial.Rect{})
}

func TestAOIKDTreeRandomAgainstScan(t *testing.T) {
	rng := rand.New(rand.NewPCG(31, 109))
	tree := newAOIKDTree()
	points := make(map[int64]spatial.Point)
	coord := func() int64 { return rng.Int64N(41) - 20 }
	for range 60 {
		for range 35 {
			id := rng.Int64N(300) - 150
			if rng.IntN(4) == 0 {
				delete(points, id)
				tree.drop(id)
			} else {
				p := spatial.Point{X: coord(), Y: coord()}
				points[id] = p
				tree.put(id, p)
			}
		}
		tree.prepare()
		for range 24 {
			box := kdTestRect(coord(), coord(), coord(), coord())
			var want []int64
			for id, p := range points {
				if box.Contains(p) {
					want = append(want, id)
				}
			}
			kdCheckRect(t, tree, box, want...)
		}
	}
}

func TestAOIKDTreeParallelRead(t *testing.T) {
	tree := newAOIKDTree()
	for id := range int64(4096) {
		tree.put(id, spatial.Point{X: id % 64, Y: id / 64})
	}
	tree.prepare()
	for worker := range int64(16) {
		t.Run(fmt.Sprint(worker), func(t *testing.T) {
			t.Parallel()
			dst := make([]int64, 1, 17)
			dst[0] = -1
			for i := range int64(80) {
				x, y := (worker+i)%61, (worker*7+i*3)%61
				got := tree.appendRect(dst[:1], kdTestRect(x, y, x+4, y+4))
				if len(got) != 17 || got[0] != -1 {
					t.Fatalf("read %d: %v", i, got)
				}
				slices.Sort(got[1:])
				for j, id := range got[1:] {
					if want := (y+int64(j/4))*64 + x + int64(j%4); id != want {
						t.Fatalf("read %d: got %d, want %d", i, id, want)
					}
				}
			}
		})
	}
}

func BenchmarkAOIKDTree(b *testing.B) {
	tree := newAOIKDTree()
	points := make([]spatial.Point, 256*256)
	for i := range points {
		points[i] = spatial.Point{X: int64(i % 256), Y: int64(i / 256)}
		tree.put(int64(i), points[i])
	}
	tree.prepare()
	box := kdTestRect(103, 97, 107, 101)
	b.Run("Rect16Of65536", func(b *testing.B) {
		dst := make([]int64, 0, 16)
		for b.Loop() {
			if got := tree.appendRect(dst, box); len(got) != 16 {
				b.Fatal(len(got))
			}
		}
	})
	b.Run("Scan16Of65536", func(b *testing.B) {
		dst := make([]int64, 0, 16)
		for b.Loop() {
			dst = dst[:0]
			for i, p := range points {
				if p.X >= box.Min.X && p.X < box.Max.X && p.Y >= box.Min.Y && p.Y < box.Max.Y {
					dst = append(dst, int64(i))
				}
			}
			if len(dst) != 16 {
				b.Fatal(len(dst))
			}
		}
	})
	b.Run("UnchangedPrepare", func(b *testing.B) {
		for b.Loop() {
			tree.prepare()
		}
	})
	b.Run("MoveAndPrepare65536", func(b *testing.B) {
		x := ^tree.points[0].X
		for b.Loop() {
			tree.put(0, spatial.Point{X: x})
			tree.prepare()
			x = ^x // 在 -1 与 0 之间反复移动，确保每轮确实重建。
		}
	})
}
