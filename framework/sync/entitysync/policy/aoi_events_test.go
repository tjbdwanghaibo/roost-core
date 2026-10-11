package policy

import (
	"github.com/tjbdwanghaibo/roost-core/infra/base/spatial"
	"reflect"
	"testing"
)

func TestAOIEventCollectionSerialAndParallelLifecycle(t *testing.T) {
	run := func(workers int) [][]InterestEvent {
		a := interestFixture(t, AOIConfig{EnterRadius: 100, LeaveRadius: 130, BlockSize: 50})
		check := func(err error) {
			t.Helper()
			if err != nil {
				t.Fatal(err)
			}
		}
		at := spatial.Point{X: 100, Y: 100}
		check(a.AddSubject(100, at))
		for id := int64(1); id <= 8; id++ {
			check(a.AddObserver(id, at))
		}
		a.Flush()
		(blockSource{a}).Flush()
		a.beginBatch()
		check(a.MoveSubject(100, spatial.Point{X: 900, Y: 900}))
		check(a.SetObservedBlocks(100, []int64{a.BlockAt(at)}))
		a.endBatch(workers)
		spatialEvents, blockEvents := a.Flush(), (blockSource{a}).Flush()
		if len(spatialEvents) != 8 || len(blockEvents) != 8 {
			t.Fatalf("lost batch events: %v %v", spatialEvents, blockEvents)
		}
		for i := range spatialEvents {
			if spatialEvents[i].Kind != InterestLeave || blockEvents[i].Kind != InterestEnter {
				t.Fatal("wrong transition")
			}
		}
		check(a.RemoveObserver(1))
		check(a.AddObserver(1, at))
		lifecycle := (blockSource{a}).Flush()
		if len(lifecycle) != 2 || lifecycle[0].Kind != InterestLeave || lifecycle[1].Kind != InterestEnter {
			t.Fatalf("reordered lifecycle: %v", lifecycle)
		}
		if len(a.Flush()) != 0 || len((blockSource{a}).Flush()) != 0 {
			t.Fatal("events delivered twice")
		}
		return [][]InterestEvent{spatialEvents, blockEvents, lifecycle}
	}
	if one, many := run(1), run(4); !reflect.DeepEqual(one, many) {
		t.Fatalf("serial=%v parallel=%v", one, many)
	}
}
