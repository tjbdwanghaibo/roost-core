package policy

import (
	"fmt"
	"github.com/tjbdwanghaibo/roost-core/spatial"
	"reflect"
	"testing"
)

func interestFixture(t *testing.T, config AOIConfig) *AOI {
	t.Helper()
	if config.Bounds.Empty() {
		config.Bounds = spatial.Rect{Min: spatial.Point{X: 0, Y: 0}, Max: spatial.Point{X: 10000, Y: 10000}}
	}
	if config.BlockSize == 0 {
		config.BlockSize = 500
	}
	if config.EnterRadius == 0 {
		config.EnterRadius = 600
	}
	if config.LeaveRadius == 0 {
		config.LeaveRadius = 800
	}
	manager, err := NewAOI(config)
	if err != nil {
		t.Fatal(err)
	}
	return manager
}

func requireEvents(t *testing.T, got, want []InterestEvent) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("events = %+v\nwant %+v", got, want)
	}
}

func TestInterestEnterLeaveWithHysteresis(t *testing.T) {
	manager := interestFixture(t, AOIConfig{})
	if err := manager.AddObserver(1, spatial.Point{X: 5000, Y: 5000}); err != nil {
		t.Fatal(err)
	}
	if err := manager.AddSubject(2, spatial.Point{X: 5000, Y: 5700}); err != nil { // 700: between enter(600) and leave(800)
		t.Fatal(err)
	}
	requireEvents(t, manager.Flush(), nil) // outside enter radius: nothing
	if err := manager.MoveSubject(2, spatial.Point{X: 5000, Y: 5550}); err != nil {
		t.Fatal(err)
	}
	requireEvents(t, manager.Flush(), []InterestEvent{{Observer: 1, Subject: 2, Kind: InterestEnter, Band: 0}})
	// Oscillating inside the hysteresis window emits nothing.
	for i := 0; i < 100; i++ {
		target := spatial.Point{X: 5000, Y: 5700}
		if i%2 == 1 {
			target = spatial.Point{X: 5000, Y: 5550}
		}
		if err := manager.MoveSubject(2, target); err != nil {
			t.Fatal(err)
		}
	}
	requireEvents(t, manager.Flush(), nil)
	if err := manager.MoveSubject(2, spatial.Point{X: 5000, Y: 5900}); err != nil { // beyond leave radius
		t.Fatal(err)
	}
	requireEvents(t, manager.Flush(), []InterestEvent{{Observer: 1, Subject: 2, Kind: InterestLeave, Band: -1}})
	// Removal of a visible subject emits Leave.
	if err := manager.MoveSubject(2, spatial.Point{X: 5000, Y: 5100}); err != nil {
		t.Fatal(err)
	}
	manager.Flush()
	if err := manager.RemoveSubject(2); err != nil {
		t.Fatal(err)
	}
	requireEvents(t, manager.Flush(), []InterestEvent{{Observer: 1, Subject: 2, Kind: InterestLeave, Band: -1}})
}

func TestInterestBandsAndObserverMovement(t *testing.T) {
	manager := interestFixture(t, AOIConfig{Bands: []int64{200, 400}})
	if err := manager.AddSubject(9, spatial.Point{X: 5000, Y: 5000}); err != nil {
		t.Fatal(err)
	}
	if err := manager.AddObserver(1, spatial.Point{X: 5000, Y: 5150}); err != nil { // band 0 (<=200)
		t.Fatal(err)
	}
	requireEvents(t, manager.Flush(), []InterestEvent{{Observer: 1, Subject: 9, Kind: InterestEnter, Band: 0}})
	if err := manager.MoveObserver(1, spatial.Point{X: 5000, Y: 5300}); err != nil { // band 1 (<=400)
		t.Fatal(err)
	}
	requireEvents(t, manager.Flush(), []InterestEvent{{Observer: 1, Subject: 9, Kind: InterestBandChanged, Band: 1}})
	if err := manager.MoveObserver(1, spatial.Point{X: 5000, Y: 5500}); err != nil { // band 2 (beyond last edge)
		t.Fatal(err)
	}
	requireEvents(t, manager.Flush(), []InterestEvent{{Observer: 1, Subject: 9, Kind: InterestBandChanged, Band: 2}})
	if err := manager.RemoveObserver(1); err != nil {
		t.Fatal(err)
	}
	requireEvents(t, manager.Flush(), []InterestEvent{{Observer: 1, Subject: 9, Kind: InterestLeave, Band: -1}})
}

func TestInterestMaxVisibleEvictsFarthest(t *testing.T) {
	manager := interestFixture(t, AOIConfig{MaxVisible: 2})
	if err := manager.AddObserver(1, spatial.Point{X: 5000, Y: 5000}); err != nil {
		t.Fatal(err)
	}
	manager.AddSubject(10, spatial.Point{X: 5000, Y: 5400}) // distance 400
	manager.AddSubject(11, spatial.Point{X: 5000, Y: 5500}) // distance 500
	manager.Flush()
	// A farther subject is ignored at capacity.
	manager.AddSubject(12, spatial.Point{X: 5000, Y: 5550})
	requireEvents(t, manager.Flush(), nil)
	// A closer subject evicts the farthest member.
	manager.AddSubject(13, spatial.Point{X: 5000, Y: 5100})
	requireEvents(t, manager.Flush(), []InterestEvent{
		{Observer: 1, Subject: 11, Kind: InterestLeave, Band: -1},
		{Observer: 1, Subject: 13, Kind: InterestEnter, Band: 0},
	})
	if got := manager.Visible(1); !reflect.DeepEqual(got, []int64{10, 13}) {
		t.Fatalf("visible = %v", got)
	}
}

func TestInterestObserverIsAlsoSubjectSymmetry(t *testing.T) {
	manager := interestFixture(t, AOIConfig{})
	for _, id := range []int64{1, 2} {
		at := spatial.Point{X: 5000, Y: 5000 + 100*(id-1)}
		if err := manager.AddObserver(id, at); err != nil {
			t.Fatal(err)
		}
		if err := manager.AddSubject(id, at); err != nil {
			t.Fatal(err)
		}
	}
	requireEvents(t, manager.Flush(), []InterestEvent{
		{Observer: 1, Subject: 2, Kind: InterestEnter, Band: 0},
		{Observer: 2, Subject: 1, Kind: InterestEnter, Band: 0},
	})
}

func TestInterestDeterministicEventStream(t *testing.T) {
	run := func() []InterestEvent {
		manager := interestFixture(t, AOIConfig{Bands: []int64{300}, MaxVisible: 8})
		var all []InterestEvent
		for id := int64(1); id <= 20; id++ {
			manager.AddObserver(id, spatial.Point{X: 5000 + id*37%900, Y: 5000 + id*53%900})
			manager.AddSubject(id, spatial.Point{X: 5000 + id*37%900, Y: 5000 + id*53%900})
		}
		all = append(all, manager.Flush()...)
		for step := int64(0); step < 50; step++ {
			for id := int64(1); id <= 20; id++ {
				to := spatial.Point{X: 5000 + (id*37+step*91)%1200, Y: 5000 + (id*53+step*71)%1200}
				manager.MoveSubject(id, to)
				manager.MoveObserver(id, to)
			}
			all = append(all, manager.Flush()...)
		}
		return all
	}
	if !reflect.DeepEqual(run(), run()) {
		t.Fatal("event stream differs across identical runs")
	}
}

func clusterFixture(t *testing.T) *AOICluster {
	t.Helper()
	cluster, err := NewAOICluster(AOIConfig{
		BlockSize: 500, EnterRadius: 600, LeaveRadius: 800, Bands: []int64{300},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Two rooms sharing the x=10000 seam on one world plane.
	if err := cluster.AddArea(1, spatial.Rect{Min: spatial.Point{X: 0, Y: 0}, Max: spatial.Point{X: 10000, Y: 10000}}); err != nil {
		t.Fatal(err)
	}
	if err := cluster.AddArea(2, spatial.Rect{Min: spatial.Point{X: 10000, Y: 0}, Max: spatial.Point{X: 20000, Y: 10000}}); err != nil {
		t.Fatal(err)
	}
	return cluster
}

func TestClusterBoundaryVisibilityIsSeamless(t *testing.T) {
	cluster := clusterFixture(t)
	// Observer in room 1 near the seam sees a subject in room 2's border strip.
	if err := cluster.AddObserver(1, spatial.Point{X: 9800, Y: 5000}); err != nil {
		t.Fatal(err)
	}
	if err := cluster.AddSubject(2, spatial.Point{X: 10200, Y: 5000}); err != nil { // room 2, distance 400
		t.Fatal(err)
	}
	requireEvents(t, cluster.Flush(), []InterestEvent{{Observer: 1, Subject: 2, Kind: InterestEnter, Band: 1}})
	// And symmetrically: an observer in room 2 sees into room 1.
	if err := cluster.AddObserver(3, spatial.Point{X: 10100, Y: 5000}); err != nil {
		t.Fatal(err)
	}
	if err := cluster.AddSubject(4, spatial.Point{X: 9900, Y: 5000}); err != nil {
		t.Fatal(err)
	}
	events := cluster.Flush()
	seen := map[string]bool{}
	for _, event := range events {
		seen[fmt.Sprintf("%d->%d:%d", event.Observer, event.Subject, event.Kind)] = true
	}
	if !seen["3->4:0"] {
		t.Fatalf("reverse boundary visibility missing: %+v", events)
	}
}

func TestClusterMigrationDoesNotBlink(t *testing.T) {
	cluster := clusterFixture(t)
	if err := cluster.AddObserver(1, spatial.Point{X: 9900, Y: 5000}); err != nil {
		t.Fatal(err)
	}
	if err := cluster.AddSubject(2, spatial.Point{X: 9700, Y: 5000}); err != nil {
		t.Fatal(err)
	}
	requireEvents(t, cluster.Flush(), []InterestEvent{{Observer: 1, Subject: 2, Kind: InterestEnter, Band: 0}})
	// The subject shuttles across the seam 100 times while staying inside the
	// observer's radii: not a single event may surface.
	for i := 0; i < 100; i++ {
		target := spatial.Point{X: 10200, Y: 5000} // room 2
		if i%2 == 1 {
			target = spatial.Point{X: 9700, Y: 5000} // room 1
		}
		if err := cluster.MoveSubject(2, target); err != nil {
			t.Fatal(err)
		}
	}
	requireEvents(t, cluster.Flush(), nil)
	// The observer itself migrates across the seam: still no blink.
	if err := cluster.MoveObserver(1, spatial.Point{X: 10100, Y: 5000}); err != nil {
		t.Fatal(err)
	}
	events := cluster.Flush()
	for _, event := range events {
		if event.Subject == 2 && (event.Kind == InterestLeave || event.Kind == InterestEnter) {
			t.Fatalf("observer migration blinked: %+v", events)
		}
	}
	// Walking far away in room 2 finally leaves.
	if err := cluster.MoveObserver(1, spatial.Point{X: 15000, Y: 5000}); err != nil {
		t.Fatal(err)
	}
	requireEvents(t, cluster.Flush(), []InterestEvent{{Observer: 1, Subject: 2, Kind: InterestLeave, Band: -1}})
}

func TestClusterRejectsOverlapAndOrphanPoints(t *testing.T) {
	cluster := clusterFixture(t)
	if err := cluster.AddArea(3, spatial.Rect{Min: spatial.Point{X: 9000, Y: 0}, Max: spatial.Point{X: 11000, Y: 10000}}); err == nil {
		t.Fatal("overlapping room accepted")
	}
	if err := cluster.AddSubject(5, spatial.Point{X: 30000, Y: 5000}); err == nil {
		t.Fatal("orphan point accepted")
	}
	if err := cluster.AddArea(1, spatial.Rect{Min: spatial.Point{X: 20000, Y: 0}, Max: spatial.Point{X: 21000, Y: 1000}}); err == nil {
		t.Fatal("duplicate room id accepted")
	}
}

func TestClusterDeterministicAcrossRuns(t *testing.T) {
	run := func() []InterestEvent {
		cluster := clusterFixture(t)
		var all []InterestEvent
		for id := int64(1); id <= 16; id++ {
			at := spatial.Point{X: 9000 + id*151%2000, Y: 4000 + id*97%2000}
			cluster.AddObserver(id, at)
			cluster.AddSubject(id, at)
		}
		all = append(all, cluster.Flush()...)
		for step := int64(0); step < 40; step++ {
			for id := int64(1); id <= 16; id++ {
				to := spatial.Point{X: 9000 + (id*151+step*113)%2000, Y: 4000 + (id*97+step*77)%2000}
				cluster.MoveSubject(id, to)
				cluster.MoveObserver(id, to)
			}
			all = append(all, cluster.Flush()...)
		}
		return all
	}
	if !reflect.DeepEqual(run(), run()) {
		t.Fatal("cluster event stream differs across identical runs")
	}
}

func BenchmarkClusterFlushRandomWalk(b *testing.B) {
	cluster, err := NewAOICluster(AOIConfig{BlockSize: 500, EnterRadius: 600, LeaveRadius: 800, Bands: []int64{300}})
	if err != nil {
		b.Fatal(err)
	}
	for room := int64(0); room < 4; room++ {
		if err := cluster.AddArea(AreaID(room+1), spatial.Rect{Min: spatial.Point{X: room * 10000, Y: 0}, Max: spatial.Point{X: (room + 1) * 10000, Y: 10000}}); err != nil {
			b.Fatal(err)
		}
	}
	for id := int64(1); id <= 1000; id++ {
		cluster.AddSubject(id, spatial.Point{X: (id * 397) % 40000, Y: (id * 269) % 10000})
	}
	for id := int64(2001); id <= 2100; id++ {
		cluster.AddObserver(id, spatial.Point{X: (id * 397) % 40000, Y: (id * 269) % 10000})
	}
	cluster.Flush()
	b.ReportAllocs()
	b.ResetTimer()
	for step := 0; step < b.N; step++ {
		offset := int64(step%97) * 41
		for id := int64(1); id <= 1000; id++ {
			_ = cluster.MoveSubject(id, spatial.Point{X: (id*397 + offset) % 40000, Y: (id*269 + offset) % 10000})
		}
		cluster.Flush()
	}
}

// Regression (review): per-room MaxVisible multiplied a border observer's
// budget by the number of touched rooms; the cap is now enforced globally at
// the cluster level.
func TestClusterMaxVisibleHoldsAcrossSeams(t *testing.T) {
	cluster, err := NewAOICluster(AOIConfig{BlockSize: 500, EnterRadius: 600, LeaveRadius: 800, MaxVisible: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := cluster.AddArea(1, spatial.Rect{Min: spatial.Point{X: 0, Y: 0}, Max: spatial.Point{X: 10000, Y: 10000}}); err != nil {
		t.Fatal(err)
	}
	if err := cluster.AddArea(2, spatial.Rect{Min: spatial.Point{X: 10000, Y: 0}, Max: spatial.Point{X: 20000, Y: 10000}}); err != nil {
		t.Fatal(err)
	}
	if err := cluster.AddObserver(1, spatial.Point{X: 9900, Y: 5000}); err != nil {
		t.Fatal(err)
	}
	cluster.AddSubject(2, spatial.Point{X: 9800, Y: 5000})  // room 1, distance 100
	cluster.AddSubject(3, spatial.Point{X: 10100, Y: 5000}) // room 2, distance 200
	cluster.Flush()
	if got := cluster.Visible(1); !reflect.DeepEqual(got, []int64{2}) {
		t.Fatalf("cap leaked across the seam: visible = %v, want [2]", got)
	}
	// The nearer newcomer displaces the incumbent, downstream sees the swap.
	cluster.AddSubject(4, spatial.Point{X: 9950, Y: 5000}) // distance 50
	events := cluster.Flush()
	want := []InterestEvent{
		{Observer: 1, Subject: 2, Kind: InterestLeave, Band: -1},
		{Observer: 1, Subject: 4, Kind: InterestEnter, Band: 0},
	}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("displacement events = %+v, want %+v", events, want)
	}
	if got := cluster.Visible(1); !reflect.DeepEqual(got, []int64{4}) {
		t.Fatalf("visible = %v, want [4]", got)
	}
}

// Regression (review): a room added after observers existed left stationary
// border observers with a permanent visibility hole into it (mirrors were
// only rebuilt on observer movement). AddArea now retrofits mirrors.
func TestClusterAddRoomBackfillsStationaryObservers(t *testing.T) {
	cluster, err := NewAOICluster(AOIConfig{BlockSize: 500, EnterRadius: 600, LeaveRadius: 800})
	if err != nil {
		t.Fatal(err)
	}
	if err := cluster.AddArea(1, spatial.Rect{Min: spatial.Point{X: 0, Y: 0}, Max: spatial.Point{X: 10000, Y: 10000}}); err != nil {
		t.Fatal(err)
	}
	if err := cluster.AddObserver(1, spatial.Point{X: 9900, Y: 5000}); err != nil { // stationary turret at the future seam
		t.Fatal(err)
	}
	if err := cluster.AddArea(2, spatial.Rect{Min: spatial.Point{X: 10000, Y: 0}, Max: spatial.Point{X: 20000, Y: 10000}}); err != nil {
		t.Fatal(err)
	}
	if err := cluster.AddSubject(2, spatial.Point{X: 10200, Y: 5000}); err != nil { // room 2, distance 300
		t.Fatal(err)
	}
	events := cluster.Flush()
	if len(events) != 1 || events[0].Kind != InterestEnter || events[0].Subject != 2 {
		t.Fatalf("stationary observer blind to the new room: %+v", events)
	}
}

// Regression (review): id 0 was half-tracked (spatial.BlockIndex silently refuses
// it) — visibility split between observers. Now rejected outright, and a
// giant LeaveRadius that would wrap the subscription box is rejected too.
func TestInterestRejectsZeroIDAndWrappingRadius(t *testing.T) {
	manager := interestFixture(t, AOIConfig{})
	if err := manager.AddSubject(0, spatial.Point{X: 5000, Y: 5000}); err == nil {
		t.Fatal("subject id 0 accepted")
	}
	if err := manager.AddObserver(0, spatial.Point{X: 5000, Y: 5000}); err == nil {
		t.Fatal("observer id 0 accepted")
	}
	if _, err := NewAOI(AOIConfig{
		Bounds: spatial.Rect{Min: spatial.Point{X: 0, Y: 0}, Max: spatial.Point{X: 1000, Y: 1000}}, BlockSize: 100,
		EnterRadius: 1, LeaveRadius: int64(1) << 62,
	}); err == nil {
		t.Fatal("wrapping LeaveRadius accepted")
	}
}

// Regression (review): block-order admission could emit a transient
// Enter+Leave pair for a farther subject during one observer evaluation.
func TestInterestObserverEvaluationAdmitsNearestFirst(t *testing.T) {
	manager := interestFixture(t, AOIConfig{MaxVisible: 1})
	// Farther subject sits in a lower block index than the nearer one.
	manager.AddSubject(5, spatial.Point{X: 4600, Y: 4600}) // farther
	manager.AddSubject(9, spatial.Point{X: 5050, Y: 5050}) // nearer
	if err := manager.AddObserver(1, spatial.Point{X: 5000, Y: 5000}); err != nil {
		t.Fatal(err)
	}
	events := manager.Flush()
	want := []InterestEvent{{Observer: 1, Subject: 9, Kind: InterestEnter, Band: 0}}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("admission emitted transients: %+v, want %+v", events, want)
	}
}
