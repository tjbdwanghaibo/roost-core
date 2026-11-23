package policy

import (
	"cmp"
	"fmt"
	"maps"
	"math/rand/v2"
	"slices"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/infra/base/spatial"
)

// The full-sort oracle also reproduces the first rejection, which is an input to
// the incremental moved-only path. Compare events before Flush can reorder them.
func TestNearestCandidatesMatchFullSort(t *testing.T) {
	rng := rand.New(rand.NewPCG(71, 19))
	for _, k := range []int{-1, 0, 1, 2, 7, 49, 200} {
		for _, count := range []int{0, 1, 8, 32, 201, 1000} {
			for trial := range 4 {
				setup := func() (*AOI, *interestObserver) {
					return &AOI{subjects: make(map[int64]spatial.Point)}, &interestObserver{id: 3, limited: k >= 0, limit: int32(max(k, 0)), enter: 9000000000, visible: make(map[int64]int), bands: []int64{2, 8}}
				}
				a, actual := setup()
				r, ref := setup()
				for i := range min(max(k, 0), trial*3) {
					id := int64(10000 + i)
					p := spatial.Point{X: int64(4 + i)}
					a.subjects[id], r.subjects[id] = p, p
					actual.visible[id], ref.visible[id] = a.band(actual, p), r.band(ref, p)
				}
				var input []entryCandidate
				for i := range count {
					id := int64(i+1) * int64(1-2*(i%2))
					p := spatial.Point{X: rng.Int64N(9), Y: rng.Int64N(9)}
					if trial == 3 {
						p.X += 4000000000
					} // saturated d², still inside enter radius
					a.subjects[id], r.subjects[id] = p, p
					input = append(input, entryCandidate{id, p, spatial.DistanceSquared(spatial.Point{}, p)})
				}
				rng.Shuffle(len(input), func(i, j int) { input[i], input[j] = input[j], input[i] })
				a.admitNearest(actual, slices.Clone(input))
				slices.SortFunc(input, func(x, y entryCandidate) int {
					return cmp.Or(cmp.Compare(x.distance, y.distance), cmp.Compare(x.subject, y.subject))
				})
				for _, x := range input {
					if k >= 0 && len(ref.visible) >= k && !r.evictFarther(ref, x.at) {
						noteRefused(ref, x.distance)
						break
					}
					r.evaluatePair(ref, x.subject, x.at, false)
				}
				if !slices.Equal(a.pending, r.pending) || !maps.Equal(actual.visible, ref.visible) || actual.refused != ref.refused || actual.refusedMin != ref.refusedMin {
					t.Fatalf("limit=%d count=%d trial=%d: visibility, ordered events or refusal bound differed", k, count, trial)
				}
			}
		}
	}
}

// One deterministic shrink + reopen of a dense Block observer, including the
// resulting events. Use the same setup and compiled package for each binary.
func BenchmarkBlockObserverReopen(b *testing.B) {
	for _, n := range []int{32, 1000} {
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			m, err := NewAOI(AOIConfig{Bounds: spatial.Rect{Max: spatial.Point{X: 600, Y: 600}}, BlockSize: 100, EnterRadius: 150, LeaveRadius: 180, MaxVisible: 49})
			if err != nil {
				b.Fatal(err)
			}
			rng := rand.New(rand.NewPCG(72, 18))
			for i := range n {
				if err := m.AddSubject(int64(i+1), spatial.Point{X: 200 + rng.Int64N(200), Y: 200 + rng.Int64N(200)}); err != nil {
					b.Fatal(err)
				}
			}
			if err := m.AddObserver(1, 0, spatial.Point{X: 300, Y: 300}, ObserverView{}); err != nil {
				b.Fatal(err)
			}
			m.Flush()
			b.ReportAllocs()
			for b.Loop() {
				if err := m.SetObserverLimit(1, 0, true); err != nil {
					b.Fatal(err)
				}
				if err := m.SetObserverLimit(1, 49, true); err != nil {
					b.Fatal(err)
				}
				if got, want := len(m.Flush()), 2*min(49, n); got != want {
					b.Fatalf("events %d != %d", got, want)
				}
			}
		})
	}
}
