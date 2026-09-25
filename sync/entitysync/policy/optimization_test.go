package policy

import (
	"context"
	"github.com/tjbdwanghaibo/roost-core/entity"
	"github.com/tjbdwanghaibo/roost-core/spatial"
	"github.com/tjbdwanghaibo/roost-core/sync/entitysync"
	"testing"
)

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
	if err := in.Enter(1, spatial.Point{X: 500, Y: 500}); err != nil {
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
