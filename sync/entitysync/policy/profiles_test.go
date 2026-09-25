package policy

import (
	"context"
	"errors"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/entity"
	"github.com/tjbdwanghaibo/roost-core/sync/entitysync"
)

func TestProfileAssemblyChecksFallbackSchemaAndPriority(t *testing.T) {
	near := entity.SyncProfile{Key: "near"}
	views, _ := entity.NewSyncViewSet(entity.SyncView{Profile: near, Fields: 1, Priority: -5})
	priorities, err := entity.MergeSyncViewPriorities(views)
	if err != nil {
		t.Fatal(err)
	}
	m, err := entitysync.NewManager(entitysync.ManagerConfig{Transport: &sink{}, ProfilePriorities: priorities})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close(context.Background())
	cfg := InterestConfig{Manager: m, AOI: interestConfig(), ViewSets: map[string]*entity.SyncViewSet{"player": views}, SourceProfiles: map[string]map[int]entity.SyncProfile{SourceSelf: {0: near}, SourceSpatial: {0: near}}}
	if _, err := NewInterest(cfg); !errors.Is(err, entity.ErrSyncViewUnknown) {
		t.Fatalf("missing fallback: %v", err)
	}
	cfg.SourceProfiles[SourceSpatial][1] = near
	cfg.SourceProfiles[SourceSpatial][2] = near
	in, err := NewInterest(cfg)
	if err != nil {
		t.Fatal(err)
	}
	in.Close()
	cfg.SourceProfiles[SourceSpatial][2] = entity.SyncProfile{Key: "near", SchemaVersion: 2}
	if _, err := NewInterest(cfg); !errors.Is(err, entity.ErrSyncViewUnknown) {
		t.Fatalf("schema mismatch: %v", err)
	}
	cfg.SourceProfiles[SourceSpatial][2] = near
	conflict, _ := entity.NewSyncViewSet(entity.SyncView{Profile: near, Priority: 2})
	if _, err := entity.MergeSyncViewPriorities(views, conflict); err == nil {
		t.Fatal("conflicting priorities accepted")
	}
	cfg.ViewSets["monster"] = conflict
	if _, err := NewInterest(cfg); err == nil {
		t.Fatal("manager/view priority mismatch accepted")
	}
}

func TestProfileAssemblyCopiesSetsAndRejectsDynamicUnknownView(t *testing.T) {
	known := entity.SyncProfile{Key: "public"}
	views, _ := entity.NewSyncViewSet(entity.SyncView{Profile: known, Fields: 1})
	m, err := entitysync.NewManager(entitysync.ManagerConfig{Transport: &sink{}})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close(context.Background())
	sets := map[string]*entity.SyncViewSet{"player": views}
	dynamic := known
	in, err := NewInterest(InterestConfig{Manager: m, AOI: interestConfig(), ViewSets: sets, Profile: func(int) entity.SyncProfile { return dynamic }})
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	delete(sets, "player")
	player(t, m, 1)
	dynamic = entity.SyncProfile{Key: "not-declared"}
	var refusals []Refusal
	in.applyEvent(SourceSpatial, InterestEvent{Observer: 1, Subject: 1, Kind: InterestEnter}, &refusals)
	if len(refusals) != 1 || !errors.Is(refusals[0].Err, entity.ErrSyncViewUnknown) {
		t.Fatalf("refusals: %+v", refusals)
	}
	if len(m.Subscribers(1)) != 0 {
		t.Fatal("invalid profile reached manager")
	}
}
