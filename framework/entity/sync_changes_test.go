package entity

import "testing"

type trackedPacker struct{ version, acked uint64 }

func (p *trackedPacker) PackSubjectSnapshot(SyncProfile) (FrozenSyncPayload, error) {
	return TakeFrozenSyncPayload(2, []byte{byte(p.version)}), nil
}
func (p *trackedPacker) PackSubjectDelta(profile SyncProfile, _ uint64) (FrozenSyncPayload, error) {
	return p.PackSubjectSnapshot(profile)
}
func (p *trackedPacker) SubjectSyncChangeVersion() uint64       { return p.version }
func (p *trackedPacker) AcknowledgeSubjectSyncChanges(v uint64) { p.acked = v }
func TestTrackedSyncCaptureCommitAndAbort(t *testing.T) {
	p := &trackedPacker{version: 1}
	s := NewSubjectSyncState(SubjectSyncCreateParam{Enabled: true, SubjectID: 42, Packer: p})
	profile := SyncProfile{Key: "self"}
	s.MarkDirty(1)
	first, err := s.PrepareViews([]SyncProfile{profile}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Abort(); err != nil {
		t.Fatal(err)
	}
	if p.acked != 0 {
		t.Fatal("abort acknowledged changes")
	}
	release := 0
	if err := s.FreezeSyncViews([]SyncProfile{profile}, nil, func(int) (func(), bool) { return func() { release++ }, true }); err != nil {
		t.Fatal(err)
	}
	frozen, err := s.PrepareViews([]SyncProfile{profile}, nil)
	if err != nil {
		t.Fatal(err)
	}
	p.version = 2
	s.MarkDirty(1)
	if err := frozen.Commit(); err != nil {
		t.Fatal(err)
	}
	if p.acked != 1 || release != 1 || !s.PendingDirty() {
		t.Fatal("frozen commit consumed later changes")
	}
	next, err := s.PrepareViews([]SyncProfile{profile}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := next.Commit(); err != nil {
		t.Fatal(err)
	}
	if p.acked != 2 {
		t.Fatal("latest change not acknowledged")
	}
}
