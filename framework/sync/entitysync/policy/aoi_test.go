// AOI：进入离开滞回、带区与移动、容量淘汰与同距规则、集群接缝、候选与工作区对照参考实现、配置与非法输入拒绝。

package policy

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"reflect"
	"slices"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/entitysync"
	"github.com/tjbdwanghaibo/roost-core/infra/base/spatial"
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
	if err := manager.AddObserver(1, 1, spatial.Point{X: 5000, Y: 5000}, ObserverView{}); err != nil {
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
	if err := manager.AddObserver(1, 1, spatial.Point{X: 5000, Y: 5150}, ObserverView{}); err != nil { // band 0 (<=200)
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
	if err := manager.AddObserver(1, 1, spatial.Point{X: 5000, Y: 5000}, ObserverView{}); err != nil {
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
		if err := manager.AddObserver(id, id, at, ObserverView{}); err != nil {
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
			manager.AddObserver(id, id, spatial.Point{X: 5000 + id*37%900, Y: 5000 + id*53%900}, ObserverView{})
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
	// Two areas sharing the x=10000 seam on one world plane.
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
	// Observer in area 1 near the seam sees a subject in area 2's border strip.
	if err := cluster.AddObserver(1, 1, spatial.Point{X: 9800, Y: 5000}, ObserverView{}); err != nil {
		t.Fatal(err)
	}
	if err := cluster.AddSubject(2, spatial.Point{X: 10200, Y: 5000}); err != nil { // area 2, distance 400
		t.Fatal(err)
	}
	requireEvents(t, cluster.Flush(), []InterestEvent{{Observer: 1, Subject: 2, Kind: InterestEnter, Band: 1}})
	// And symmetrically: an observer in area 2 sees into area 1.
	if err := cluster.AddObserver(3, 3, spatial.Point{X: 10100, Y: 5000}, ObserverView{}); err != nil {
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
	if err := cluster.AddObserver(1, 1, spatial.Point{X: 9900, Y: 5000}, ObserverView{}); err != nil {
		t.Fatal(err)
	}
	if err := cluster.AddSubject(2, spatial.Point{X: 9700, Y: 5000}); err != nil {
		t.Fatal(err)
	}
	requireEvents(t, cluster.Flush(), []InterestEvent{{Observer: 1, Subject: 2, Kind: InterestEnter, Band: 0}})
	// The subject shuttles across the seam 100 times while staying inside the
	// observer's radii: not a single event may surface.
	for i := 0; i < 100; i++ {
		target := spatial.Point{X: 10200, Y: 5000} // area 2
		if i%2 == 1 {
			target = spatial.Point{X: 9700, Y: 5000} // area 1
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
	// Walking far away in area 2 finally leaves.
	if err := cluster.MoveObserver(1, spatial.Point{X: 15000, Y: 5000}); err != nil {
		t.Fatal(err)
	}
	requireEvents(t, cluster.Flush(), []InterestEvent{{Observer: 1, Subject: 2, Kind: InterestLeave, Band: -1}})
}

func TestClusterRejectsOverlapAndOrphanPoints(t *testing.T) {
	cluster := clusterFixture(t)
	if err := cluster.AddArea(3, spatial.Rect{Min: spatial.Point{X: 9000, Y: 0}, Max: spatial.Point{X: 11000, Y: 10000}}); err == nil {
		t.Fatal("overlapping area accepted")
	}
	if err := cluster.AddSubject(5, spatial.Point{X: 30000, Y: 5000}); err == nil {
		t.Fatal("orphan point accepted")
	}
	if err := cluster.AddArea(1, spatial.Rect{Min: spatial.Point{X: 20000, Y: 0}, Max: spatial.Point{X: 21000, Y: 1000}}); err == nil {
		t.Fatal("duplicate area id accepted")
	}
}

func TestClusterDeterministicAcrossRuns(t *testing.T) {
	run := func() []InterestEvent {
		cluster := clusterFixture(t)
		var all []InterestEvent
		for id := int64(1); id <= 16; id++ {
			at := spatial.Point{X: 9000 + id*151%2000, Y: 4000 + id*97%2000}
			cluster.AddObserver(id, id, at, ObserverView{})
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
	for area := int64(0); area < 4; area++ {
		if err := cluster.AddArea(AreaID(area+1), spatial.Rect{Min: spatial.Point{X: area * 10000, Y: 0}, Max: spatial.Point{X: (area + 1) * 10000, Y: 10000}}); err != nil {
			b.Fatal(err)
		}
	}
	for id := int64(1); id <= 1000; id++ {
		cluster.AddSubject(id, spatial.Point{X: (id * 397) % 40000, Y: (id * 269) % 10000})
	}
	for id := int64(2001); id <= 2100; id++ {
		cluster.AddObserver(id, id, spatial.Point{X: (id * 397) % 40000, Y: (id * 269) % 10000}, ObserverView{})
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

// 容量生效的密集随机行走：每个观察者约 40 个候选、上限 16，衡量移动重选的成本。
func BenchmarkClusterFlushRandomWalkCapped(b *testing.B) {
	cluster, err := NewAOICluster(AOIConfig{BlockSize: 500, EnterRadius: 600, LeaveRadius: 800, Bands: []int64{300}, MaxVisible: 16})
	if err != nil {
		b.Fatal(err)
	}
	for area := int64(0); area < 4; area++ {
		if err := cluster.AddArea(AreaID(area+1), spatial.Rect{Min: spatial.Point{X: area * 2500, Y: 0}, Max: spatial.Point{X: (area + 1) * 2500, Y: 2500}}); err != nil {
			b.Fatal(err)
		}
	}
	for id := int64(1); id <= 1000; id++ {
		cluster.AddSubject(id, spatial.Point{X: (id * 397) % 10000, Y: (id * 269) % 2500})
	}
	for id := int64(2001); id <= 2100; id++ {
		cluster.AddObserver(id, id, spatial.Point{X: (id * 397) % 10000, Y: (id * 269) % 2500}, ObserverView{})
	}
	cluster.Flush()
	b.ReportAllocs()
	b.ResetTimer()
	for step := 0; step < b.N; step++ {
		offset := int64(step%97) * 41
		for id := int64(1); id <= 1000; id++ {
			_ = cluster.MoveSubject(id, spatial.Point{X: (id*397 + offset) % 10000, Y: (id*269 + offset) % 2500})
		}
		cluster.Flush()
	}
}

// Regression (review): per-area MaxVisible multiplied a border observer's
// budget by the number of touched areas; the cap is now enforced globally at
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
	if err := cluster.AddObserver(1, 1, spatial.Point{X: 9900, Y: 5000}, ObserverView{}); err != nil {
		t.Fatal(err)
	}
	cluster.AddSubject(2, spatial.Point{X: 9800, Y: 5000})  // area 1, distance 100
	cluster.AddSubject(3, spatial.Point{X: 10100, Y: 5000}) // area 2, distance 200
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

// Regression (review): a area added after observers existed left stationary
// border observers with a permanent visibility hole into it (mirrors were
// only rebuilt on observer movement). AddArea now retrofits mirrors.
func TestClusterAddAreaBackfillsStationaryObservers(t *testing.T) {
	cluster, err := NewAOICluster(AOIConfig{BlockSize: 500, EnterRadius: 600, LeaveRadius: 800})
	if err != nil {
		t.Fatal(err)
	}
	if err := cluster.AddArea(1, spatial.Rect{Min: spatial.Point{X: 0, Y: 0}, Max: spatial.Point{X: 10000, Y: 10000}}); err != nil {
		t.Fatal(err)
	}
	if err := cluster.AddObserver(1, 1, spatial.Point{X: 9900, Y: 5000}, ObserverView{}); err != nil { // stationary turret at the future seam
		t.Fatal(err)
	}
	if err := cluster.AddArea(2, spatial.Rect{Min: spatial.Point{X: 10000, Y: 0}, Max: spatial.Point{X: 20000, Y: 10000}}); err != nil {
		t.Fatal(err)
	}
	if err := cluster.AddSubject(2, spatial.Point{X: 10200, Y: 5000}); err != nil { // area 2, distance 300
		t.Fatal(err)
	}
	events := cluster.Flush()
	if len(events) != 1 || events[0].Kind != InterestEnter || events[0].Subject != 2 {
		t.Fatalf("stationary observer blind to the new area: %+v", events)
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
	if err := manager.AddObserver(0, 0, spatial.Point{X: 5000, Y: 5000}, ObserverView{}); err == nil {
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
	if err := manager.AddObserver(1, 1, spatial.Point{X: 5000, Y: 5000}, ObserverView{}); err != nil {
		t.Fatal(err)
	}
	events := manager.Flush()
	want := []InterestEvent{{Observer: 1, Subject: 9, Kind: InterestEnter, Band: 0}}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("admission emitted transients: %+v, want %+v", events, want)
	}
}

func TestObserverBlockBudgetRefusesARatioThatSubscribesTheWholeMap(t *testing.T) {
	config := AOIConfig{
		Bounds:      spatial.Rect{Max: spatial.Point{X: 10000, Y: 10000}},
		BlockSize:   10,
		EnterRadius: 900,
		LeaveRadius: 1000,
	}
	if _, err := NewAOI(config); !errors.Is(err, ErrInterestBudget) {
		t.Fatalf("a configuration where one observer covers 40401 blocks was accepted: err=%v", err)
	}
}

// The budget is a configuration, not a constant: a deployment that really
// wants a wide view says so.
func TestObserverBlockBudgetIsConfigurable(t *testing.T) {
	config := AOIConfig{
		Bounds:            spatial.Rect{Max: spatial.Point{X: 10000, Y: 10000}},
		BlockSize:         10,
		EnterRadius:       900,
		LeaveRadius:       1000,
		MaxObserverBlocks: 100000,
	}
	if _, err := NewAOI(config); err != nil {
		t.Fatalf("an explicit budget did not admit the configuration: %v", err)
	}
}

// The ordinary shape — a block about the size of the view — is nowhere near
// the default budget, and the worst case is computed rather than sampled: a
// manager that only refused after an observer arrived would refuse in
// production and pass every start-up check.
func TestTheNineBlockShapeFitsTheDefaultBudget(t *testing.T) {
	manager, err := NewAOI(AOIConfig{
		Bounds:      spatial.Rect{Max: spatial.Point{X: 10000, Y: 10000}},
		BlockSize:   150,
		EnterRadius: 120,
		LeaveRadius: 150,
	})
	if err != nil {
		t.Fatalf("the classic nine-block configuration was refused: %v", err)
	}
	if err := manager.AddObserver(1, 1, spatial.Point{X: 5000, Y: 5000}, ObserverView{}); err != nil {
		t.Fatalf("add observer: %v", err)
	}
	if blocks := len(manager.observers[1].blocks); blocks > 16 {
		t.Fatalf("a block-sized view subscribed to %d blocks", blocks)
	}
}

// A radius so large that the box arithmetic would overflow must be refused by
// the budget rather than wrap into a small number.
func TestObserverBlockBudgetSaturatesInsteadOfWrapping(t *testing.T) {
	config := AOIConfig{
		Bounds:      spatial.Rect{Max: spatial.Point{X: 1 << 40, Y: 1 << 40}},
		BlockSize:   1,
		EnterRadius: 1 << 40,
		LeaveRadius: 1 << 41,
	}
	if _, err := NewAOI(config); err == nil {
		t.Fatal("a configuration whose observer box covers the whole map was accepted")
	}
}

func TestInterestRefusesUnknownObserversAndZeroBlocks(t *testing.T) {
	manager := interestFixture(t, AOIConfig{})
	if err := manager.MoveObserver(999, spatial.Point{}); !errors.Is(err, ErrInterestUnknown) {
		t.Fatalf("MoveObserver of an unknown observer = %v", err)
	}
	if _, err := NewAOICluster(AOIConfig{EnterRadius: 10, LeaveRadius: 20}); !errors.Is(err, spatial.ErrInvalidBounds) {
		t.Fatalf("NewAOICluster without a block size = %v", err)
	}
}

func interestConfig() AOIConfig {
	return AOIConfig{Bounds: spatial.Rect{Min: spatial.Point{X: 0, Y: 0}, Max: spatial.Point{X: 1000, Y: 1000}}, BlockSize: 100, EnterRadius: 60, LeaveRadius: 80, Bands: []int64{20, 40}}
}

// The interest manager's configuration is what keeps its box arithmetic from
// wrapping and its hysteresis from oscillating; every invalid shape is
// refused at construction.
func TestNewInterestManagerRefusesEachInvalidConfig(t *testing.T) {
	if _, err := NewAOI(interestConfig()); err != nil {
		t.Fatalf("baseline rejected: %v", err)
	}
	cases := []struct {
		name   string
		mutate func(*AOIConfig)
		want   error
	}{
		{"empty bounds", func(c *AOIConfig) {
			c.Bounds = spatial.Rect{Min: spatial.Point{X: 5, Y: 5}, Max: spatial.Point{X: 5, Y: 5}}
		}, spatial.ErrInvalidBounds},
		{"block size zero", func(c *AOIConfig) { c.BlockSize = 0 }, spatial.ErrInvalidBounds},
		{"enter radius zero", func(c *AOIConfig) { c.EnterRadius = 0 }, ErrInterestConfig},
		{"leave radius below enter", func(c *AOIConfig) { c.LeaveRadius = c.EnterRadius - 1 }, ErrInterestConfig},
		{"leave radius would wrap", func(c *AOIConfig) { c.LeaveRadius = maxInterestRadius }, ErrInterestConfig},
		{"band edge zero", func(c *AOIConfig) { c.Bands = []int64{0, 40} }, ErrInterestConfig},
		{"bands not ascending", func(c *AOIConfig) { c.Bands = []int64{40, 40} }, ErrInterestConfig},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := interestConfig()
			tc.mutate(&cfg)
			if _, err := NewAOI(cfg); !errors.Is(err, tc.want) {
				t.Fatalf("NewAOI = %v, want %v", err, tc.want)
			}
		})
	}
}

// Subjects and observers live inside the bounds and are addressed by id;
// a point outside the world or an id nobody registered is refused rather
// than indexed into a block that does not exist.
func TestInterestManagerRefusesOutOfBoundsAndUnknownIDs(t *testing.T) {
	m, err := NewAOI(interestConfig())
	if err != nil {
		t.Fatal(err)
	}
	outside := spatial.Point{X: 5000, Y: 5}
	if err := m.AddSubject(1, outside); !errors.Is(err, spatial.ErrInvalidBounds) {
		t.Fatalf("AddSubject outside = %v", err)
	}
	if err := m.AddObserver(9, 9, outside, ObserverView{}); !errors.Is(err, spatial.ErrInvalidBounds) {
		t.Fatalf("AddObserver outside = %v", err)
	}
	if err := m.MoveSubject(1, spatial.Point{X: 1, Y: 1}); !errors.Is(err, ErrInterestUnknown) {
		t.Fatalf("MoveSubject unknown = %v", err)
	}
	if err := m.RemoveSubject(1); !errors.Is(err, ErrInterestUnknown) {
		t.Fatalf("RemoveSubject unknown = %v", err)
	}
	if err := m.MoveObserver(9, spatial.Point{X: 1, Y: 1}); !errors.Is(err, ErrInterestUnknown) {
		t.Fatalf("MoveObserver unknown = %v", err)
	}
	if err := m.RemoveObserver(9); !errors.Is(err, ErrInterestUnknown) {
		t.Fatalf("RemoveObserver unknown = %v", err)
	}
	if err := m.AddSubject(1, spatial.Point{X: 10, Y: 10}); err != nil {
		t.Fatal(err)
	}
	if err := m.AddObserver(9, 9, spatial.Point{X: 12, Y: 12}, ObserverView{}); err != nil {
		t.Fatal(err)
	}
	if err := m.MoveSubject(1, outside); !errors.Is(err, spatial.ErrInvalidBounds) {
		t.Fatalf("MoveSubject outside = %v", err)
	}
	if err := m.MoveObserver(9, outside); !errors.Is(err, spatial.ErrInvalidBounds) {
		t.Fatalf("MoveObserver outside = %v", err)
	}
	if got := m.Visible(9); len(got) != 1 || got[0] != 1 {
		t.Fatalf("refused moves must leave positions intact; visible=%v", got)
	}
}

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
						check(a.AddObserver(id, id, at, ObserverView{}))
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
								check(a.AddObserver(oid, oid, at, ObserverView{}))
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
	check(m.AddObserver(1001, 1001, spatial.Point{X: 1000, Y: 1000}, ObserverView{}))
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
		check(m.AddObserver(1001, 1001, spatial.Point{X: 1000, Y: 1000}, ObserverView{}))
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
			check(a.AddObserver(id, id, at, ObserverView{}))
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
		check(a.AddObserver(1, 1, at, ObserverView{}))
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
					check(a.AddObserver(id, id, at, ObserverView{}))
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
							check(a.AddObserver(oid, oid, at, ObserverView{}))
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

func TestFarthestTieKeepsDeterministicEviction(t *testing.T) {
	for range 100 {
		a := interestFixture(t, AOIConfig{MaxVisible: 2})
		a.subjects[10] = spatial.Point{X: 400, Y: 500}
		a.subjects[20] = spatial.Point{X: 600, Y: 500}
		observer := &interestObserver{id: 1, at: spatial.Point{X: 500, Y: 500}, visible: map[int64]int{20: 0, 10: 0}}
		if a.evictFarther(observer, spatial.Point{X: 500, Y: 600}) {
			t.Fatal("equal distance replaced held object")
		}
		if !a.evictFarther(observer, spatial.Point{X: 500, Y: 550}) {
			t.Fatal("nearer object not admitted")
		}
		if _, exists := observer.visible[10]; exists {
			t.Fatal("tie no longer evicts lower ID")
		}
	}
}

func TestInterestSelectsSourceViewsBeforeComparingPriority(t *testing.T) {
	near, team := entity.SyncProfile{Key: "near"}, entity.SyncProfile{Key: "team", LOD: 8}
	m, err := entitysync.NewManager(entitysync.ManagerConfig{Transport: &sink{}, ProfilePriorities: map[entity.SyncProfile]int{team: -5}})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close(context.Background())
	player(t, m, 1)
	var captured entity.SyncProfile
	s := entity.NewSubjectSyncState(entity.SubjectSyncCreateParam{Enabled: true, SubjectID: 2, Packer: entity.SubjectSyncPackFunc{Snapshot: func(p entity.SyncProfile) (entity.FrozenSyncPayload, error) {
		captured = p
		return entity.TakeFrozenSyncPayload(1, []byte("view")), nil
	}}})
	if err := m.Register(s); err != nil {
		t.Fatal(err)
	}
	profiles := map[string]map[int]entity.SyncProfile{SourceSpatial: {0: near}, "team": {0: team}}
	in, err := NewInterest(InterestConfig{Manager: m, AOI: AOIConfig{Bounds: spatial.Rect{Max: spatial.Point{X: 1000, Y: 1000}}, BlockSize: 150, EnterRadius: 120, LeaveRadius: 150}, Relations: []string{"team"}, SourceProfiles: profiles})
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	profiles["team"][0] = near
	if err := in.Enter(1, 1, spatial.Point{X: 500, Y: 500}, ObserverView{}); err != nil {
		t.Fatal(err)
	}
	if err := in.Show(2, spatial.Point{X: 510, Y: 500}); err != nil {
		t.Fatal(err)
	}
	in.Relation("team").Set(1, []int64{2})
	if refusals := in.Apply(); len(refusals) != 0 {
		t.Fatal(refusals)
	}
	if err := m.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if captured != team.Normalize() {
		t.Fatalf("source/priority lost: %+v", captured)
	}
	in.Relation("team").Clear(1)
	in.Apply()
	if err := m.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if captured != near.Normalize() {
		t.Fatalf("source fallback lost: %+v", captured)
	}
}

// 观察者不看见自己的实体：距离入场、整体重评与额外覆盖块三条路径都排除自身（D14 改签名前后的回归基线）。
func TestAOIObserverNeverSeesItsOwnSubject(t *testing.T) {
	manager := interestFixture(t, AOIConfig{})
	at := spatial.Point{X: 5000, Y: 5000}
	if err := manager.AddSubject(1, at); err != nil {
		t.Fatal(err)
	}
	if err := manager.AddObserver(1, 1, at, ObserverView{}); err != nil {
		t.Fatal(err)
	}
	if err := manager.AddSubject(2, spatial.Point{X: 5100, Y: 5000}); err != nil {
		t.Fatal(err)
	}
	if err := manager.SetObservedBlocks(1, []int64{manager.BlockAt(spatial.Point{X: 5300, Y: 5000})}); err != nil {
		t.Fatal(err)
	}
	if err := manager.MoveObserver(1, spatial.Point{X: 5010, Y: 5000}); err != nil {
		t.Fatal(err)
	}
	if err := manager.MoveSubject(1, spatial.Point{X: 5010, Y: 5000}); err != nil {
		t.Fatal(err)
	}
	requireEvents(t, manager.Flush(), []InterestEvent{{Observer: 1, Subject: 2, Kind: InterestEnter, Band: 0}})
	if events := (blockSource{manager: manager}).Flush(); len(events) != 0 {
		t.Fatalf("the observer saw its own coverage block: %+v", events)
	}
}
