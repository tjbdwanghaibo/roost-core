package policy_test

import (
	"cmp"
	"fmt"
	"math/rand/v2"
	"runtime"
	"slices"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/framework/sync/entitysync/policy"
	"github.com/tjbdwanghaibo/roost-core/infra/base/spatial"
)

// From the worktree root:
//
//	export GOWORK=off GOPROXY=off GOCACHE="$MESHY_WORK/activity-season-fix-gocache"
//	go test ./framework/sync/entitysync/policy -run '^TestAOIModes' -count=1
//	go test ./framework/sync/entitysync/policy -run '^$' -bench '^BenchmarkAOIModesTick$' -benchmem -benchtime=256x -count=5 -cpu=4
//	go test ./framework/sync/entitysync/policy -run '^$' -bench '^BenchmarkAOIModesBuild$' -benchmem -benchtime=16x -count=5 -cpu=4
//
// Compare medians on one machine; no pass/fail timing threshold. One Tick op = one
// simulation tick, not one query. Use multiples of eight to sample the entire cycle equally.
// Build B/op is ALL allocations including transient ones; live-B/world is a separate
// approximate post-GC HeapAlloc delta with only the constructed world held live.
const (
	modeViewCount = 8
	modeCycle     = 8
	modeSeed1     = 731
	modeSeed2     = 109
	modeEnter     = 256
	modeLeave     = 320
	modeBand      = 128
)

type modeScene struct {
	name        string
	n, movers   int
	side, block int64
}

var modeScenes = []modeScene{
	{"DenseLarge", 16384, 2048, 8192, 128},
	{"SparseMoving", 4096, 2048, 65536, 256},
	{"SparseMostlyStill", 4096, 32, 65536, 256},
	{"SmallStatic", 256, 0, 8192, 128},
	{"SmallRareMove", 256, 1, 8192, 128},
}

type modeFrame struct { // movers are subject IDs [1, len(moves)], each moved exactly once
	moves []spatial.Point
	views [modeViewCount]spatial.Point
}
type modeTrace struct {
	scene   modeScene
	initial []spatial.Point
	frames  [modeCycle]modeFrame
}

// The two sparse cases share the SAME initial positions and the same trajectories for the
// first 32 subjects. No RNG, trace generation, cloning, or brute-force checks in timed ticks.
func newModeTrace(s modeScene) *modeTrace {
	tr := &modeTrace{scene: s, initial: make([]spatial.Point, s.n)}
	rng := rand.New(rand.NewPCG(modeSeed1, modeSeed2))
	velocity := make([]spatial.Point, s.n)
	for i := range tr.initial {
		tr.initial[i] = xy(rng.Int64N(s.side), rng.Int64N(s.side))
		velocity[i] = xy(rng.Int64N(64)+1, rng.Int64N(65)-32)
	}
	// Small maps: cameras, not subjects, cross eight hysteresis boundaries. The
	// rare-change variant moves one distinct subject on two of the eight ticks.
	if s.movers < modeViewCount {
		for i := 0; i < modeViewCount; i++ {
			center := xy(int64(2*(i%4)+1)*s.side/8, int64(2*(i/4)+1)*s.side/4)
			tr.initial[1+i] = xy(center.X+280, center.Y)
			for f := range tr.frames {
				dx := int64(64)
				if f%2 != 0 {
					dx = -64
				}
				tr.frames[f].views[i] = xy(center.X+dx, center.Y)
			}
		}
		if s.movers == 1 {
			p := tr.initial[0]
			tr.frames[2].moves = []spatial.Point{xy((p.X+48)%s.side, p.Y)}
			tr.frames[6].moves = []spatial.Point{p}
		}
		return tr
	}
	// Eight distributed cameras, with one boundary-crossing mover per camera. The rest
	// are uniform over the map: even the sparse scenarios cannot silently bench empty views.
	for f := range tr.frames {
		fr := &tr.frames[f]
		wave := int64(min(f, modeCycle-f))
		fr.moves = make([]spatial.Point, s.movers)
		for i := range fr.moves {
			p, v := tr.initial[i], velocity[i]
			fr.moves[i] = xy((p.X+v.X*wave+s.side)%s.side, (p.Y+v.Y*wave+s.side)%s.side)
		}
		for i := range fr.views {
			center := xy(int64(2*(i%4)+1)*s.side/8, int64(2*(i/4)+1)*s.side/4)
			fr.views[i] = xy(center.X+8*wave, center.Y-3*wave)
			x := int64(modeEnter - 64)
			if f%2 != 0 {
				x = modeLeave + 128
			}
			fr.moves[i] = xy(center.X+x, center.Y)
		}
	}
	copy(tr.initial[:s.movers], tr.frames[0].moves)
	return tr
}

func (tr *modeTrace) build(tb testing.TB, mode policy.AOIIndexMode) *policy.AOI {
	s := tr.scene
	a, err := policy.NewAOI(policy.AOIConfig{IndexMode: mode, Bounds: spatial.Rect{Max: xy(s.side, s.side)}, BlockSize: s.block, EnterRadius: modeEnter, LeaveRadius: modeLeave, Bands: []int64{modeBand}})
	if err != nil {
		tb.Fatal(err)
	}
	for i, p := range tr.initial {
		if err := a.AddSubject(int64(i+1), p); err != nil {
			tb.Fatal(err)
		}
	}
	for i, p := range tr.frames[0].views {
		if err := a.AddObserver(int64(i+1), 0, p, policy.ObserverView{}); err != nil {
			tb.Fatal(err)
		}
	}
	return a
}

// Public MoveObserver means rediscover the whole current view; using a changing position
// makes that real work. With fixed cameras only point moves/affected pairs are evaluated.
func (fr *modeFrame) advance(tb testing.TB, a *policy.AOI, full bool) []policy.InterestEvent {
	for i, p := range fr.moves {
		if err := a.MoveSubject(int64(i+1), p); err != nil {
			tb.Fatal(err)
		}
	}
	if full {
		for i, p := range fr.views {
			if err := a.MoveObserver(int64(i+1), p); err != nil {
				tb.Fatal(err)
			}
		}
	}
	return a.Flush()
}

// No block or distance helpers from the library: for these small coordinates, brute scanning
// every observer × every subject with signed 64-bit squared distance is exact. Preserve
// the two phases: a pair may change twice in one tick (subject move, then camera move).
// Neither the event stream nor prior membership comes from an implementation under test.
type modeOracle struct {
	positions []spatial.Point
	views     [modeViewCount]spatial.Point
	bands     [modeViewCount][]int8
}

func (o *modeOracle) scan() (events []policy.InterestEvent) {
	for i, v := range o.views {
		for j, p := range o.positions {
			dx, dy := v.X-p.X, v.Y-p.Y
			d := dx*dx + dy*dy
			prior := o.bands[i][j]
			radius := int64(modeEnter)
			if prior >= 0 {
				radius = modeLeave
			}
			band := int8(-1)
			if d <= radius*radius {
				band = 0
				if d > modeBand*modeBand {
					band = 1
				}
			}
			if prior != band {
				oid, sid := int64(i+1), int64(j+1)
				var e policy.InterestEvent
				switch {
				case band < 0:
					e = leave(oid, sid)
				case prior < 0:
					e = enter(oid, sid, int(band))
				default:
					e = bandChange(oid, sid, int(band))
				}
				events = append(events, e)
				o.bands[i][j] = band
			}
		}
	}
	return events
}

func TestAOIModesBenchmarkReplay(t *testing.T) {
	for _, s := range modeScenes {
		tr := newModeTrace(s)
		for _, full := range []bool{false, true} {
			if !full && s.movers < modeViewCount {
				continue
			} // static cameras on static points do not exercise discovery
			t.Run(fmt.Sprintf("%s/full=%t", s.name, full), func(t *testing.T) {
				o := modeOracle{positions: slices.Clone(tr.initial), views: tr.frames[0].views}
				for i := range o.bands {
					o.bands[i] = make([]int8, s.n)
					for j := range o.bands[i] {
						o.bands[i][j] = -1
					}
				}
				worlds := make([]*policy.AOI, len(consumerModes))
				for i, mode := range consumerModes {
					worlds[i] = tr.build(t, mode)
				}
				// Include setup and the wrap from the last frame back to the first one twice.
				for n := 0; n <= 2*modeCycle; n++ {
					fr := &tr.frames[n%modeCycle]
					if n != 0 {
						copy(o.positions, fr.moves)
					}
					want := o.scan()
					if full && n != 0 {
						o.views = fr.views
						want = append(want, o.scan()...)
						slices.SortStableFunc(want, func(a, b policy.InterestEvent) int {
							if c := cmp.Compare(a.Observer, b.Observer); c != 0 {
								return c
							}
							return cmp.Compare(a.Subject, b.Subject)
						})
					}
					if len(want) < modeViewCount {
						t.Fatalf("tick=%d trace did not exercise all boundary movers: %d events", n, len(want))
					}
					for i, a := range worlds {
						var got []policy.InterestEvent
						if n == 0 {
							got = a.Flush()
						} else {
							got = fr.advance(t, a, full)
						}
						checkEvents(t, fmt.Sprintf("%s tick %d", consumerModes[i], n), got, want...)
					}
					for j, bands := range o.bands {
						var ids []int64
						for k, band := range bands {
							if band >= 0 {
								ids = append(ids, int64(k+1))
							}
						}
						for i, a := range worlds {
							if got := a.Visible(int64(j + 1)); !slices.Equal(got, ids) {
								t.Fatalf("%s tick=%d view=%d visible=%v want %v", consumerModes[i], n, j+1, got, ids)
							}
						}
					}
				}
			})
		}
	}
}

func BenchmarkAOIModesTick(b *testing.B) {
	for _, s := range modeScenes {
		tr := newModeTrace(s)
		for _, p := range []struct {
			name string
			full bool
		}{{"Incremental", false}, {"Full", true}} {
			if !p.full && s.movers < modeViewCount {
				continue
			}
			for _, mode := range consumerModes {
				b.Run(fmt.Sprintf("%s/%s/%s", s.name, p.name, mode), func(b *testing.B) {
					a := tr.build(b, mode)
					a.Flush()
					var events, moves int64
					b.ReportAllocs()
					b.ResetTimer()
					for n := 0; n < b.N; n++ {
						fr := &tr.frames[(n+1)%modeCycle]
						events += int64(len(fr.advance(b, a, p.full)))
						moves += int64(len(fr.moves))
					}
					b.StopTimer()
					if events < int64(b.N)*modeViewCount {
						b.Fatal("unexpectedly empty replay")
					}
					b.ReportMetric(float64(events)/float64(b.N), "events/tick")
					b.ReportMetric(float64(moves)/float64(b.N), "moves/tick")
					queries := 0
					if p.full {
						queries = modeViewCount
					}
					b.ReportMetric(float64(queries), "full-queries/tick")
				})
			}
		}
	}
}

func BenchmarkAOIModesBuild(b *testing.B) {
	for _, s := range modeScenes {
		tr := newModeTrace(s)
		for _, mode := range consumerModes {
			b.Run(fmt.Sprintf("%s/%s", s.name, mode), func(b *testing.B) {
				// Sample once, outside timing. Settle/drain the initial enter events both here and
				// in timed builds. This is the retained AOI world incl. observers, not index-only bytes.
				var before, after runtime.MemStats
				runtime.GC()
				runtime.ReadMemStats(&before)
				sample := tr.build(b, mode)
				sample.Flush()
				runtime.GC()
				runtime.ReadMemStats(&after)
				runtime.KeepAlive(sample)
				sample = nil
				runtime.GC()
				b.ReportAllocs()
				b.ResetTimer()
				for n := 0; n < b.N; n++ {
					a := tr.build(b, mode)
					if len(a.Flush()) < modeViewCount {
						b.Fatal("empty initial views")
					}
				}
				b.StopTimer()
				b.ReportMetric(float64(int64(after.HeapAlloc)-int64(before.HeapAlloc)), "live-B/world")
			})
		}
	}
}
