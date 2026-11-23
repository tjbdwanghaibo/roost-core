package policy

import (
	"fmt"
	"maps"
	"slices"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/infra/base/spatial"
)

func denseCoverageScene(t testing.TB, subjects, observers int) *AOI {
	t.Helper()
	a, err := NewAOI(AOIConfig{Bounds: spatial.Rect{Max: spatial.Point{X: 600, Y: 600}}, BlockSize: 100, EnterRadius: 45, LeaveRadius: 60, MaxVisible: 8})
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= subjects; i++ {
		id := int64(i)
		if err := a.AddSubject(id, spatial.Point{X: 410, Y: 420}); err != nil {
			t.Fatal(err)
		}
		if err := a.SetObservedBlocks(id, []int64{a.BlockAt(spatial.Point{X: 150, Y: 150})}); err != nil {
			t.Fatal(err)
		}
	}
	for i := 1; i <= observers; i++ {
		if err := a.AddObserver(int64(i), 0, spatial.Point{X: 150, Y: 150}, ObserverView{}); err != nil {
			t.Fatal(err)
		}
	}
	a.Flush()
	a.FlushBlocks()
	return a
}

// Force the original full union after each batch in the reference AOI. Check
// the emitted events as well as the authoritative final map for each observer.
func TestCoverageChangesMatchFullUnion(t *testing.T) {
	for _, workers := range []int{1, 4} {
		t.Run(fmt.Sprint(workers), func(t *testing.T) {
			a, r := denseCoverageScene(t, 120, 8), denseCoverageScene(t, 120, 8)
			p := func(x int64) spatial.Point { return spatial.Point{X: x, Y: 150} }
			b1, b2, out := a.BlockAt(p(150)), a.BlockAt(p(201)), a.BlockAt(p(450))
			check := func(step int) {
				t.Helper()
				if g, w := a.FlushBlocks(), r.FlushBlocks(); !slices.Equal(g, w) {
					t.Fatalf("step %d coverage events differ: %v / %v", step, g, w)
				}
				if !slices.Equal(a.Flush(), r.Flush()) {
					t.Fatalf("step %d distance events differ", step)
				}
				for id, o := range a.observers {
					if !maps.Equal(o.blockVisible, r.observers[id].blockVisible) {
						t.Fatalf("step %d observer %d", step, id)
					}
				}
			}
			for step := 0; step < 9; step++ {
				for _, m := range []*AOI{a, r} {
					m.beginBatch()
					set := func(id int64, bs ...int64) {
						t.Helper()
						if err := m.SetObservedBlocks(id, bs); err != nil {
							t.Fatal(err)
						}
					}
					must := func(err error) {
						if err != nil {
							t.Fatal(err)
						}
					}
					switch step {
					case 0:
						set(1, b1, b2)
						set(2, out)
						set(2, b1)
						set(3, out) // keep another block, cancel an edit, real leave
					case 1:
						set(1, b2)
						set(3, b1)
						must(m.AddObserver(1, 3, p(150), ObserverView{})) // new self after pending changes
					case 2:
						set(4, out)
						must(m.MoveObserver(2, p(460)))
						set(5, out) // block set changes inside the batch
					case 3:
						must(m.RemoveSubject(1))
						must(m.AddSubject(1, p(405)))
						set(1, b2)
						must(m.RemoveObserver(3))
						must(m.AddObserver(3, 1, p(150), ObserverView{}))
					case 4:
						for id := int64(1); id <= 120; id++ {
							set(id, out)
						} // many edits, final union shrinks
					case 5:
						for id := int64(1); id <= 120; id++ {
							set(id, b1, b2)
						}
					case 6:
						set(11, b1)
						set(11, out)
						set(11, b2)
						set(2)
						set(5)
						must(m.MoveObserver(2, p(150)))
					case 7:
						must(m.SetObserverLimit(4, 0, true))
						set(15, out)
					case 8:
						set(3, b2)
						set(8, out)
						must(m.AddObserver(1, 8, p(150), ObserverView{}))
					}
					if m == r {
						// Recompute even observers not marked by the candidate: a lost
						// affected observer must not also disappear from the oracle.
						for id, o := range m.observers {
							o.dirty |= dirtyCoverage
							m.dirty[id] = o
						}
					}
					m.endBatch(workers)
				}
				check(step)
			}
		})
	}
}

func TestCoverageSerialRemovalsRetainUnchangedMembers(t *testing.T) {
	a := denseCoverageScene(t, 1024, 1)
	o := a.observers[1]
	for id := int64(1); id <= 1000; id++ {
		if err := a.SetObservedBlocks(id, nil); err != nil {
			t.Fatal(err)
		}
	}
	if got := len(a.FlushBlocks()); got != 1000 || len(o.blockVisible) != 24 {
		t.Fatalf("events=%d remaining=%d", got, len(o.blockVisible))
	}
	for id := int64(1001); id <= 1024; id++ {
		if _, ok := o.blockVisible[id]; !ok {
			t.Fatalf("lost unchanged source %d", id)
		}
	}
}

// Repeatedly move one coverage source among 128 observers with independent
// final maps. Both event counts are checked; count=1 is the sparse control.
func BenchmarkCoverageSourceChange(b *testing.B) {
	for _, n := range []int{1, 1000} {
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			a := denseCoverageScene(b, n, 128)
			inside := []int64{a.BlockAt(spatial.Point{X: 150, Y: 150})}
			outside := []int64{a.BlockAt(spatial.Point{X: 450, Y: 450})}
			b.ReportAllocs()
			for b.Loop() {
				for _, bs := range [][]int64{outside, inside} {
					a.beginBatch()
					if err := a.SetObservedBlocks(1, bs); err != nil {
						b.Fatal(err)
					}
					a.endBatch(1)
					if got := len(a.FlushBlocks()); got != 128 {
						b.Fatalf("%d events, want 128", got)
					}
				}
			}
		})
	}
}
