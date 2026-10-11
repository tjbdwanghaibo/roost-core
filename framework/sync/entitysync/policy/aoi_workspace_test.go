package policy

import (
	"fmt"
	"github.com/tjbdwanghaibo/roost-core/infra/base/spatial"
	"math/rand/v2"
	"reflect"
	"testing"
)

// 强制全量重算作为分类重算的对照，覆盖同批多原因、退出和 ID 重建。
func TestAOISelectiveRecomputeMatchesFullRecompute(t *testing.T) {
	for _, workers := range []int{1, 4} {
		t.Run(fmt.Sprint(workers), func(t *testing.T) {
			config := AOIConfig{Bounds: spatial.Rect{Max: spatial.Point{X: 2000, Y: 2000}}, BlockSize: 100, EnterRadius: 160, LeaveRadius: 200, MaxVisible: 12, Bands: []int64{50, 100}}
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
				reference.endBatch(1)
				compare()
			}
		})
	}
}

func TestAOIWorkspaceDropsOversizedScratch(t *testing.T) {
	w := aoiWorkspace{visible: make([]int64, 0, aoiWorkspaceLimit+1), blocks: make([]int64, 0, aoiWorkspaceLimit+1), query: make([]int64, 0, aoiWorkspaceLimit+1), candidates: make([]entryCandidate, 0, aoiWorkspaceLimit+1), seen: map[int64]struct{}{1: {}}, seenPeak: aoiWorkspaceLimit + 1}
	w.release()
	if w.visible != nil || w.blocks != nil || w.query != nil || w.candidates != nil || w.seen != nil {
		t.Fatal("oversized scratch retained")
	}
}
