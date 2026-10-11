package policy

import (
	"cmp"
	"fmt"
	"github.com/tjbdwanghaibo/roost-core/infra/base/spatial"
	"math/rand/v2"
	"reflect"
	"slices"
	"testing"
)

// 以优化前完整候选遍历作对照，覆盖容量、同批多原因、退出和 ID 重建。
func TestAOICandidatesMatchReference(t *testing.T) {
	for _, limit := range []int{0, 1, 12, 64} {
		for _, workers := range []int{1, 4} {
			t.Run(fmt.Sprintf("limit=%d/workers=%d", limit, workers), func(t *testing.T) {
				config := AOIConfig{Bounds: spatial.Rect{Max: spatial.Point{X: 2000, Y: 2000}}, BlockSize: 100, EnterRadius: 160, LeaveRadius: 200, MaxVisible: limit, Bands: []int64{50, 100}}
				actual, err := NewAOI(config)
				if err != nil {
					t.Fatal(err)
				}
				reference, err := NewAOI(config)
				if err != nil {
					t.Fatal(err)
				}
				check := func(err error) {
					t.Helper()
					if err != nil {
						t.Fatal(err)
					}
				}
				rng := rand.New(rand.NewPCG(13, 29))
				point := func() spatial.Point { return spatial.Point{X: 400 + rng.Int64N(600), Y: 400 + rng.Int64N(600)} }
				for id := int64(1); id <= 80; id++ {
					at := point()
					for _, a := range []*AOI{actual, reference} {
						check(a.AddSubject(id, at))
					}
				}
				for id := int64(1); id <= 24; id++ {
					at := point()
					for _, a := range []*AOI{actual, reference} {
						check(a.AddObserver(id, at))
					}
				}
				compare := func() {
					t.Helper()
					if a, r := actual.Flush(), reference.Flush(); !reflect.DeepEqual(a, r) {
						t.Fatalf("spatial events differ:\n%v\n%v", a, r)
					}
					if a, r := (blockSource{actual}).Flush(), (blockSource{reference}).Flush(); !reflect.DeepEqual(a, r) {
						t.Fatalf("coverage events differ:\n%v\n%v", a, r)
					}
					for id := int64(1); id <= 24; id++ {
						if !reflect.DeepEqual(actual.Visible(id), reference.Visible(id)) || !reflect.DeepEqual(actual.observers[id].blockVisible, reference.observers[id].blockVisible) {
							t.Fatalf("observer %d differs", id)
						}
					}
				}
				compare()
				for batch := range 80 {
					actual.beginBatch()
					reference.beginBatch()
					for operation := range 12 {
						sid, oid := 1+rng.Int64N(80), 1+rng.Int64N(24)
						at := point()
						blocks := []int64{actual.BlockAt(at), actual.BlockAt(point())}
						for _, a := range []*AOI{actual, reference} {
							switch (batch + operation) % 5 {
							case 0:
								check(a.MoveSubject(sid, at))
							case 1:
								check(a.MoveObserver(oid, at))
							case 2:
								check(a.SetObservedBlocks(sid, blocks))
							case 3:
								check(a.RemoveObserver(oid))
								check(a.AddObserver(oid, at))
							case 4:
								check(a.RemoveSubject(sid))
								check(a.AddSubject(sid, at))
							}
						}
					}
					for _, o := range reference.dirty {
						o.dirty = dirtySpatial | dirtyCoverage
					}
					reference.serialWork = aoiWorkspace{}
					reference.workerWork = nil
					actual.endBatch(workers)
					reference.batching = false
					for _, o := range reference.dirty {
						reference.evaluateObserverReference(o, &aoiWorkspace{}, o.dirty)
						o.dirty = 0
					}
					clear(reference.dirty)
					compare()
				}
			})
		}
	}
}

// 保留优化前的完整候选遍历算法，作为事件与可见集的对照；共用已修正的单对状态机。
func (m *AOI) evaluateObserverReference(observer *interestObserver, work *aoiWorkspace, reason observerDirty) {
	if reason&dirtyCoverage != 0 {
		m.evaluateObserved(observer)
	}
	if reason&dirtySpatial == 0 {
		return
	}
	work.visible = work.visible[:0]
	for id := range observer.visible {
		work.visible = append(work.visible, id)
	}
	slices.Sort(work.visible)
	for _, subject := range work.visible {
		at, exists := m.subjects[subject]
		m.evaluatePair(observer, subject, at, !exists)
	}
	blocks := work.blocks[:0]
	for block := range observer.blocks {
		blocks = append(blocks, block)
	}
	slices.Sort(blocks)
	work.blocks = blocks
	// Admit new candidates nearest-first: block-order admission could let a
	// farther subject enter and be evicted by a nearer one within the same
	// evaluation, emitting a transient Enter+Leave pair downstream.
	candidates := work.candidates[:0]
	if work.seen == nil {
		work.seen = make(map[int64]struct{})
	}
	seen := work.seen
	clear(seen)
	query := work.query[:0]
	for _, block := range blocks {
		query = m.index.AppendBlockIDs(query[:0], block)
		for _, subject := range query {
			if _, alreadyVisible := observer.visible[subject]; alreadyVisible {
				continue
			}
			if _, duplicate := seen[subject]; duplicate {
				continue
			}
			seen[subject] = struct{}{}
			if at, exists := m.subjects[subject]; exists {
				candidates = append(candidates, entryCandidate{subject: subject, at: at, distance: spatial.DistanceSquared(observer.at, at)})
			}
		}
	}
	work.query, work.candidates = query, candidates
	work.seenPeak = max(work.seenPeak, len(seen))
	slices.SortFunc(candidates, func(a, b entryCandidate) int {
		if order := cmp.Compare(a.distance, b.distance); order != 0 {
			return order
		}
		return cmp.Compare(a.subject, b.subject)
	})
	for _, candidate := range candidates {
		m.evaluatePair(observer, candidate.subject, candidate.at, false)
	}
}

func TestAOIMaxVisibleEvictsNegativeID(t *testing.T) {
	m, err := NewAOI(AOIConfig{Bounds: spatial.Rect{Max: spatial.Point{X: 2000, Y: 2000}}, BlockSize: 100, EnterRadius: 100, LeaveRadius: 120, MaxVisible: 1})
	if err != nil {
		t.Fatal(err)
	}
	check := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	check(m.AddObserver(1001, spatial.Point{X: 1000, Y: 1000}))
	check(m.AddSubject(-2, spatial.Point{X: 1090, Y: 1000}))
	m.Flush()
	check(m.AddSubject(3, spatial.Point{X: 1010, Y: 1000}))
	if got := m.Visible(1001); !slices.Equal(got, []int64{3}) {
		t.Fatalf("closer subject must replace negative ID: got %v", got)
	}
	want := []InterestEvent{{Observer: 1001, Subject: -2, Kind: InterestLeave, Band: -1}, {Observer: 1001, Subject: 3, Kind: InterestEnter, Band: 0}}
	if got := m.Flush(); !slices.Equal(got, want) {
		t.Fatalf("events: got %v want %v", got, want)
	}
}

// 同距离保留旧成员；多个最远成员同距时，按较小 ID 淘汰。
func TestAOICandidatesPreserveDistanceTies(t *testing.T) {
	for _, workers := range []int{1, 4} {
		m, err := NewAOI(AOIConfig{Bounds: spatial.Rect{Max: spatial.Point{X: 2000, Y: 2000}}, BlockSize: 100, EnterRadius: 100, LeaveRadius: 120, MaxVisible: 2})
		if err != nil {
			t.Fatal(err)
		}
		check := func(err error) {
			t.Helper()
			if err != nil {
				t.Fatal(err)
			}
		}
		check(m.AddObserver(1001, spatial.Point{X: 1000, Y: 1000}))
		check(m.AddSubject(-2, spatial.Point{X: 1090, Y: 1000}))
		check(m.AddSubject(2, spatial.Point{X: 910, Y: 1000}))
		m.beginBatch()
		check(m.AddSubject(3, spatial.Point{X: 1000, Y: 1090}))
		m.endBatch(workers)
		if got := m.Visible(1001); !slices.Equal(got, []int64{-2, 2}) {
			t.Fatalf("ties replaced incumbents: %v", got)
		}
		m.beginBatch()
		check(m.AddSubject(4, spatial.Point{X: 1010, Y: 1000}))
		m.endBatch(workers)
		if got := m.Visible(1001); !slices.Equal(got, []int64{2, 4}) {
			t.Fatalf("wrong farthest tie eviction: %v", got)
		}
	}
}
