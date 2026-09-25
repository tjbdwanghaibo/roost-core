package entitysync

import (
	"github.com/tjbdwanghaibo/roost-core/entity"
	"testing"
)

func TestSessionReferenceCopiesAreIsolatedOnFirstWrite(t *testing.T) {
	original := newSession(1)
	ref, err := original.allocate(100, 10)
	if err != nil {
		t.Fatal(err)
	}
	readOnly := original.clone()
	changed := readOnly.clone()
	changed.release(100)
	replacement, err := changed.allocate(200, 10)
	if err != nil {
		t.Fatal(err)
	}
	if replacement.ID != ref.ID || replacement.Generation == ref.Generation {
		t.Fatal("reference not recycled with new generation")
	}
	for _, held := range []*session{original, readOnly} {
		if held.objects[100] != ref || len(held.objects) != 1 || held.generations[ref.ID] != ref.Generation {
			t.Fatal("later frame rewrote an earlier baseline")
		}
	}
	if readOnly.referencesOwned {
		t.Fatal("read-only clone copied references")
	}
}

func TestExplicitProfilePriorityAndConfigurationOwnership(t *testing.T) {
	sink := newRecordingTransport()
	public, owner := entity.SyncProfile{Key: "a-public"}, entity.SyncProfile{Key: "z-owner", LOD: 9}
	priorities := map[entity.SyncProfile]int{owner: -10}
	m := newTestManager(t, sink, ManagerConfig{ProfilePriorities: priorities})
	priorities[owner] = 100
	packs := 0
	if err := m.Register(testSubject(t, 50, &packs)); err != nil {
		t.Fatal(err)
	}
	open(t, m, 1)
	mustSubscribe(t, m, 1, 50, public)
	source := m.NewSubscriptionSource()
	if err := source.Subscribe(1, 50, owner); err != nil {
		t.Fatal(err)
	}
	mustFlush(t, m)
	if got := oneFrame(t, sink, 1).updates[50]; got.Profile != owner.Normalize() {
		t.Fatalf("priority ignored: %+v", got)
	}
	if err := source.Unsubscribe(1, 50); err != nil {
		t.Fatal(err)
	}
	mustFlush(t, m)
	if got := oneFrame(t, sink, 1).updates[50]; got.Profile != public.Normalize() || !got.Full {
		t.Fatalf("fallback needs a full view: %+v", got)
	}
	mustFlush(t, m)
	stats := m.Stats()
	if stats.FlushCalls != 3 || stats.EmptyFlushes != 1 || stats.CreatesAdmitted != 1 || stats.UpdatesAdmitted != 1 || stats.SnapshotsCaptured != 2 {
		t.Fatalf("incorrect counters: %+v", stats)
	}
}

func TestUnobservedCaptureStillRespectsDurabilityWatermark(t *testing.T) {
	packs := 0
	s := testSubject(t, 60, &packs)
	s.MarkDirty(1)
	s.SetLastCommitLSN(10)
	watermark := uint64(9)
	m := newTestManager(t, newRecordingTransport(), ManagerConfig{DurableWatermark: func() uint64 { return watermark }})
	if err := m.Register(s); err != nil {
		t.Fatal(err)
	}
	mustFlush(t, m)
	if !s.PendingDirty() || m.Stats().DurabilityDeferred != 1 || packs != 0 {
		t.Fatal("unobserved capture bypassed watermark or packed unused view")
	}
	watermark = 10
	mustFlush(t, m)
	if s.PendingDirty() {
		t.Fatal("dirty state not cleared after watermark advanced")
	}
}

func TestHiddenFieldDeltaStillAdvancesTheClientVersion(t *testing.T) {
	views, err := entity.NewSyncViewSet(entity.SyncView{Fields: 1})
	if err != nil {
		t.Fatal(err)
	}
	packer, err := entity.NewMaskedSyncPacker(views, 1, func(mask uint64) ([]byte, error) { return []byte{byte(mask)}, nil })
	if err != nil {
		t.Fatal(err)
	}
	s := entity.NewSubjectSyncState(entity.SubjectSyncCreateParam{Enabled: true, SubjectID: 61, Packer: packer})
	sink := newRecordingTransport()
	m := newTestManager(t, sink, ManagerConfig{})
	if err := m.Register(s); err != nil {
		t.Fatal(err)
	}
	open(t, m, 1)
	mustSubscribe(t, m, 1, 61, entity.SyncProfile{})
	mustFlush(t, m)
	oneFrame(t, sink, 1)
	s.MarkDirty(2)
	mustFlush(t, m)
	hidden := oneFrame(t, sink, 1).updates[61]
	if !hidden.Payload.Empty() || hidden.BaseVersion != 0 || hidden.Version != 1 {
		t.Fatalf("hidden delta lost version: %+v", hidden)
	}
	s.MarkDirty(1)
	mustFlush(t, m)
	visible := oneFrame(t, sink, 1).updates[61]
	if visible.Payload.Empty() || visible.BaseVersion != 1 || visible.Version != 2 {
		t.Fatalf("visible delta lost baseline: %+v", visible)
	}
}
