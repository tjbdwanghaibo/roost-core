package policy

import (
	"cmp"
	"fmt"
	"math/rand/v2"
	"reflect"
	"slices"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/infra/base/spatial"
)

// 同档移动也会改变最近 N 个目标；不能依赖区域 Enter/Leave/BandChanged 才重排。
func TestClusterMaxVisibleReselectsOnSameBandMovement(t *testing.T) {
	for _, split := range []bool{false, true} {
		topology := "one_area"
		if split {
			topology = "mirrored_observer"
		}
		t.Run(topology, func(t *testing.T) {
			for _, operation := range []string{"suppressed_approaches", "selected_recedes", "observer_moves", "add_subject_moves", "add_observer_moves"} {
				t.Run(operation, func(t *testing.T) {
					c, err := NewAOICluster(AOIConfig{BlockSize: 500, EnterRadius: 600, LeaveRadius: 800, MaxVisible: 1})
					if err != nil {
						t.Fatal(err)
					}
					end := int64(20000)
					if split {
						end = 10000
					}
					if err := c.AddArea(1, spatial.Rect{Max: spatial.Point{X: end, Y: 10000}}); err != nil {
						t.Fatal(err)
					}
					if split {
						if err := c.AddArea(2, spatial.Rect{Min: spatial.Point{X: 10000}, Max: spatial.Point{X: 20000, Y: 10000}}); err != nil {
							t.Fatal(err)
						}
					}
					point := func(x int64) spatial.Point { return spatial.Point{X: x, Y: 5000} }
					if err := c.AddObserver(1, point(9950)); err != nil {
						t.Fatal(err)
					}
					if err := c.AddSubject(2, point(9800)); err != nil {
						t.Fatal(err)
					}
					if err := c.AddSubject(3, point(10150)); err != nil {
						t.Fatal(err)
					}
					c.Flush()
					if got := c.Visible(1); !reflect.DeepEqual(got, []int64{2}) {
						t.Fatalf("initial visible = %v", got)
					}
					switch operation {
					case "suppressed_approaches":
						err = c.MoveSubject(3, point(10050))
					case "selected_recedes":
						err = c.MoveSubject(2, point(9700))
					case "observer_moves":
						err = c.MoveObserver(1, point(10025))
					case "add_subject_moves":
						err = c.AddSubject(3, point(10050))
					case "add_observer_moves":
						err = c.AddObserver(1, point(10025))
					}
					if err != nil {
						t.Fatal(err)
					}
					if got := c.Visible(1); !reflect.DeepEqual(got, []int64{3}) {
						t.Fatalf("after same-band movement visible = %v, want [3]", got)
					}
					want := []InterestEvent{{Observer: 1, Subject: 2, Kind: InterestLeave, Band: -1}, {Observer: 1, Subject: 3, Kind: InterestEnter, Band: 0}}
					if got := c.Flush(); !reflect.DeepEqual(got, want) {
						t.Fatalf("events = %+v, want %+v", got, want)
					}
					if got := c.Flush(); len(got) != 0 {
						t.Fatalf("repeated events = %+v", got)
					}
				})
			}
		})
	}
}

// 全部目标始终位于进入半径内，用独立距离排序对照连续移动与接缝迁移。
func TestClusterMovementCapacityMatchesDistanceOrder(t *testing.T) {
	for _, limit := range []int{0, 1, 3} {
		t.Run(fmt.Sprintf("limit_%d", limit), func(t *testing.T) {
			c, err := NewAOICluster(AOIConfig{BlockSize: 100, EnterRadius: 600, LeaveRadius: 800, MaxVisible: limit})
			if err != nil {
				t.Fatal(err)
			}
			for id := AreaID(1); id <= 2; id++ {
				if err := c.AddArea(id, spatial.Rect{Min: spatial.Point{X: int64(id-1) * 10000}, Max: spatial.Point{X: int64(id) * 10000, Y: 10000}}); err != nil {
					t.Fatal(err)
				}
			}
			rng := rand.New(rand.NewPCG(47, 83))
			point := func() spatial.Point { return spatial.Point{X: 9800 + rng.Int64N(400), Y: 4900 + rng.Int64N(200)} }
			observers := map[int64]spatial.Point{100: point(), 101: point()}
			for _, id := range []int64{100, 101} {
				if err := c.AddObserver(id, observers[id]); err != nil {
					t.Fatal(err)
				}
			}
			subjects := make(map[int64]spatial.Point)
			for id := int64(2); id < 10; id++ {
				subjects[id] = point()
				if err := c.AddSubject(id, subjects[id]); err != nil {
					t.Fatal(err)
				}
			}
			// 初始加入可产生多次合法替换；从此处的实际可见集开始核对增量。
			c.Flush()
			stream := make(map[int64]map[int64]bool)
			for _, observer := range []int64{100, 101} {
				stream[observer] = make(map[int64]bool)
				for _, subject := range c.Visible(observer) {
					stream[observer][subject] = true
				}
			}
			for step := 0; step < 120; step++ {
				if step%3 == 0 {
					id := int64(100 + rng.IntN(2))
					observers[id] = point()
					err = c.MoveObserver(id, observers[id])
				} else {
					id := int64(2 + rng.IntN(8))
					subjects[id] = point()
					err = c.MoveSubject(id, subjects[id])
				}
				if err != nil {
					t.Fatal(err)
				}
				for _, event := range c.Flush() {
					switch event.Kind {
					case InterestEnter:
						if stream[event.Observer][event.Subject] {
							t.Fatalf("step %d: duplicate enter %+v", step, event)
						}
						stream[event.Observer][event.Subject] = true
					case InterestLeave:
						if !stream[event.Observer][event.Subject] {
							t.Fatalf("step %d: unmatched leave %+v", step, event)
						}
						delete(stream[event.Observer], event.Subject)
					default:
						t.Fatalf("same-band movement emitted %+v", event)
					}
				}
				for _, observer := range []int64{100, 101} {
					want := make([]int64, 0, len(subjects))
					for id := range subjects {
						want = append(want, id)
					}
					distance := func(id int64) int64 {
						dx, dy := subjects[id].X-observers[observer].X, subjects[id].Y-observers[observer].Y
						return dx*dx + dy*dy
					}
					slices.SortFunc(want, func(a, b int64) int {
						if order := cmp.Compare(distance(a), distance(b)); order != 0 {
							return order
						}
						return cmp.Compare(a, b)
					})
					if limit > 0 {
						want = want[:limit]
					}
					slices.Sort(want)
					if got := c.Visible(observer); !reflect.DeepEqual(got, want) {
						t.Fatalf("step %d observer %d: visible=%v want=%v", step, observer, got, want)
					}
					got := make([]int64, 0, len(stream[observer]))
					for id := range stream[observer] {
						got = append(got, id)
					}
					slices.Sort(got)
					if !reflect.DeepEqual(got, want) {
						t.Fatalf("step %d observer %d: stream=%v want=%v", step, observer, got, want)
					}
				}
			}
		})
	}
}

// 移动范围超过进入 / 离开半径并跨接缝，候选集由区域滞回决定；容量层必须始终等于
// 候选里按 (距离, ID) 最近的 MaxVisible 个。用于核对“只在跨过容量边界时重排”的捷径。
func TestClusterCapacityStaysNearestAmongCandidates(t *testing.T) {
	for seed := uint64(1); seed <= 40; seed++ {
		c, err := NewAOICluster(AOIConfig{BlockSize: 200, EnterRadius: 500, LeaveRadius: 700, Bands: []int64{250}, MaxVisible: 3})
		if err != nil {
			t.Fatal(err)
		}
		for id := AreaID(1); id <= 2; id++ {
			if err := c.AddArea(id, spatial.Rect{Min: spatial.Point{X: int64(id-1) * 2000}, Max: spatial.Point{X: int64(id) * 2000, Y: 2000}}); err != nil {
				t.Fatal(err)
			}
		}
		rng := rand.New(rand.NewPCG(seed, 7))
		point := func() spatial.Point { return spatial.Point{X: 1000 + rng.Int64N(2000), Y: 500 + rng.Int64N(1000)} }
		step := func(at spatial.Point) spatial.Point {
			at.X = min(max(at.X+rng.Int64N(301)-150, 0), 3999)
			at.Y = min(max(at.Y+rng.Int64N(301)-150, 0), 1999)
			return at
		}
		observers := []int64{100, 101, 102}
		at := make(map[int64]spatial.Point)
		for _, id := range observers {
			at[id] = point()
			if err := c.AddObserver(id, at[id]); err != nil {
				t.Fatal(err)
			}
		}
		for id := int64(1); id <= 12; id++ {
			at[id] = point()
			if err := c.AddSubject(id, at[id]); err != nil {
				t.Fatal(err)
			}
		}
		for round := 0; round < 200; round++ {
			if rng.IntN(4) == 0 {
				id := observers[rng.IntN(len(observers))]
				at[id] = step(at[id])
				err = c.MoveObserver(id, at[id])
			} else {
				id := 1 + rng.Int64N(12)
				at[id] = step(at[id])
				err = c.MoveSubject(id, at[id])
			}
			if err != nil {
				t.Fatal(err)
			}
			c.Flush()
			for _, observer := range observers {
				var candidates []int64
				for subject, state := range c.observerPairs[observer] {
					if len(state.bands) > 0 {
						candidates = append(candidates, subject)
					}
				}
				distance := func(id int64) int64 { return spatial.DistanceSquared(at[observer], at[id]) }
				slices.SortFunc(candidates, func(a, b int64) int {
					if order := cmp.Compare(distance(a), distance(b)); order != 0 {
						return order
					}
					return cmp.Compare(a, b)
				})
				want := candidates[:min(len(candidates), 3)]
				slices.Sort(want)
				if want == nil {
					want = []int64{}
				}
				got := c.Visible(observer)
				if got == nil {
					got = []int64{}
				}
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("seed %d round %d observer %d: visible=%v want nearest %v of candidates", seed, round, observer, got, want)
				}
			}
		}
	}
}
