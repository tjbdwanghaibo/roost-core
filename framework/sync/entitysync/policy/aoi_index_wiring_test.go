package policy

import (
	"errors"
	"math"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/infra/base/spatial"
)

func TestInterestConfiguredIndex(t *testing.T) {
	for _, mode := range []AOIIndexMode{"", IndexBlock, IndexCrossList, IndexKDTree, "unknown"} {
		t.Run(string(mode), func(t *testing.T) {
			c := interestConfig()
			c.IndexMode = mode
			in, err := NewInterest(InterestConfig{AOI: c, Manager: newManager(t)})
			if mode == "unknown" {
				if !errors.Is(err, ErrInterestConfig) {
					t.Fatalf("Interest accepted unknown mode: %v", err)
				}
				if _, err := NewAOICluster(c); !errors.Is(err, ErrInterestConfig) {
					t.Fatalf("Cluster accepted unknown mode: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(in.Close)
			want := mode
			if want == "" {
				want = IndexBlock
			}
			if in.IndexMode() != want || (in.aoi.index != nil) != (want == IndexBlock) {
				t.Fatalf("mode=%q dense index=%t; wanted %q", in.IndexMode(), in.aoi.index != nil, want)
			}
		})
	}
}

// Probe the real selected implementation, not a replacement search algorithm.
type aoiIndexProbe struct {
	aoiPointIndex
	prepared          bool // writes separated from parallel batch reads
	prepares, queries atomic.Int64
}

func (p *aoiIndexProbe) put(id int64, at spatial.Point) {
	p.prepared = false
	p.aoiPointIndex.put(id, at)
}
func (p *aoiIndexProbe) drop(id int64) { p.prepared = false; p.aoiPointIndex.drop(id) }
func (p *aoiIndexProbe) prepare()      { p.aoiPointIndex.prepare(); p.prepared = true; p.prepares.Add(1) }
func (p *aoiIndexProbe) appendRect(dst []int64, box spatial.Rect) []int64 {
	if !p.prepared {
		panic("AOI queried an unprepared backend")
	}
	p.queries.Add(1)
	return p.aoiPointIndex.appendRect(dst, box)
}

func TestAOIPointReadBarriers(t *testing.T) {
	for _, mode := range []AOIIndexMode{IndexCrossList, IndexKDTree} {
		t.Run(string(mode), func(t *testing.T) {
			cfg := interestConfig()
			cfg.IndexMode = mode
			a := interestFixture(t, cfg)
			physical, reverse := &aoiIndexProbe{aoiPointIndex: a.points.subjects.backend}, &aoiIndexProbe{aoiPointIndex: a.points.observers.backend}
			a.points.subjects.backend, a.points.observers.backend = physical, reverse
			check := func(err error) {
				t.Helper()
				if err != nil {
					t.Fatal(err)
				}
			}
			at := spatial.Point{X: 301, Y: 300}
			check(a.AddSubject(100, at))
			for i := range 20 {
				check(a.MoveSubject(100, spatial.Point{X: 301 + int64(i), Y: 300}))
			}
			if physical.prepares.Load() != 0 || reverse.prepares.Load() != 0 {
				t.Fatal("unqueried index prepared during serial writes")
			}
			a.beginBatch()
			for id := int64(1); id <= 16; id++ {
				check(a.AddObserver(id, 0, at, ObserverView{}))
				check(a.MoveSubject(100, spatial.Point{X: 301 + id, Y: 300}))
			}
			if physical.prepares.Load() != 0 || reverse.prepares.Load() != 0 {
				t.Fatal("premature batch rebuild")
			}
			a.endBatch(4)
			if physical.prepares.Load() != 1 || physical.queries.Load() != 16 || reverse.prepares.Load() != 1 || reverse.queries.Load() != 1 {
				t.Fatalf("first batch: subject %d prepares/%d queries, reverse %d/%d", physical.prepares.Load(), physical.queries.Load(), reverse.prepares.Load(), reverse.queries.Load())
			}
			a.beginBatch()
			for i := range 20 {
				check(a.MoveSubject(100, spatial.Point{X: 301 + int64(i), Y: 300}))
			}
			a.endBatch(4)
			check(a.MoveSubject(100, at))
			if physical.prepares.Load() != 1 || physical.queries.Load() != 16 || reverse.prepares.Load() != 1 || reverse.queries.Load() != 3 {
				t.Fatal("settled move prepared/scanned subjects or failed to reuse observer snapshot")
			}
			if len(a.blocks) != 0 {
				t.Fatalf("ordinary point subscriptions allocated %d sparse block buckets", len(a.blocks))
			}
			for i := range 60 {
				check(a.SetObservedBlocks(100, []int64{int64(i)}))
			}
			check(a.SetObservedBlocks(100, nil))
			if len(a.blocks) != 0 {
				t.Fatalf("leaked %d cleared explicit coverage buckets", len(a.blocks))
			}
		})
	}
}

// A subject outside the circle but in a subscribed block must still trigger
// batch refill. Exercise both saturated world edges and a truncated last block.
func TestAOIPointReverseBlockTriggerAtExtremes(t *testing.T) {
	for _, mode := range []AOIIndexMode{IndexBlock, IndexCrossList, IndexKDTree} {
		for _, lo := range []int64{math.MinInt64, math.MaxInt64 - 65} {
			cfg := AOIConfig{IndexMode: mode, Bounds: spatial.Rect{Min: spatial.Point{X: lo, Y: lo}, Max: spatial.Point{X: lo + 65, Y: lo + 65}}, BlockSize: 16, EnterRadius: 3, LeaveRadius: 4, MaxVisible: 1}
			a := interestFixture(t, cfg)
			check := func(err error) {
				t.Helper()
				if err != nil {
					t.Fatal(err)
				}
			}
			pos := func(x int64) spatial.Point { return spatial.Point{X: lo + x, Y: lo + x} }
			x, d := int64(0), int64(1)
			if lo != math.MinInt64 {
				x, d = 64, -1
			}
			check(a.AddSubject(1, pos(x)))
			check(a.AddSubject(2, pos(x+d)))
			check(a.AddObserver(7, 0, pos(x), ObserverView{}))
			check(a.RemoveSubject(1)) // serial pair evaluation intentionally does not refill
			if len(a.Visible(7)) != 0 {
				t.Fatal("unexpected serial refill")
			}
			a.beginBatch()
			check(a.AddSubject(3, pos(x+8*d)))
			a.endBatch(4)
			if !slices.Equal(a.Visible(7), []int64{2}) {
				t.Fatalf("%s @%d missed block-triggered refill: %v", mode, lo, a.Visible(7))
			}
			check(a.RemoveObserver(7))
			if len(a.blocks) != 0 {
				t.Fatalf("%s retained unsubscribed blocks", mode)
			}
		}
	}
}
