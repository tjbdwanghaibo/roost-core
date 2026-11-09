package entity

import (
	"errors"
	"testing"
)

// 新观察者中途取得即时快照后，旧观察者的变化仍等待周期消费；如果快照后继续修改，
// 下一次普通捕获必须携带最新完整状态，不能复用同一个版本或重放旧列表操作。
func TestSnapshotViewsPreserveChangesUntilPeriodicCapture(t *testing.T) {
	p := &trackedPacker{version: 1}
	s := NewSubjectSyncState(SubjectSyncCreateParam{Enabled: true, SubjectID: 42, Packer: p})
	profile := SyncProfile{Key: "public"}
	s.MarkDirty(1)
	s.SetLastCommitLSN(7)
	snapshot, err := s.PrepareSnapshotViews([]SyncProfile{profile, profile})
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Updates()) != 0 || len(snapshot.Snapshots()) != 1 || snapshot.CommitLSN() != 7 {
		t.Fatal("not an isolated snapshot")
	}
	if err := snapshot.Commit(); err != nil {
		t.Fatal(err)
	}
	if !s.PendingDirty() || p.acked != 0 || s.Version() != 1 {
		t.Fatal("snapshot consumed periodic work")
	}
	p.version = 2
	s.MarkDirty(2)
	next, err := s.PrepareViews([]SyncProfile{profile}, nil)
	if err != nil {
		t.Fatal(err)
	}
	updates := next.Updates()
	if len(updates) != 1 || !updates[0].Full || updates[0].Version <= snapshot.Version() || updates[0].Payload.BytesCopy()[0] != 2 {
		t.Fatalf("periodic update lost content or reused snapshot version: %+v", updates)
	}
	if err := next.Commit(); err != nil {
		t.Fatal(err)
	}
	if s.PendingDirty() || p.acked != 2 {
		t.Fatal("periodic commit did not settle accumulated changes")
	}
}

func TestSnapshotViewsAbortDoesNotForceFullOrConsumeChanges(t *testing.T) {
	p := &trackedPacker{version: 1}
	s := NewSubjectSyncState(SubjectSyncCreateParam{Enabled: true, SubjectID: 42, Packer: p})
	s.MarkDirty(1)
	snapshot, err := s.PrepareSnapshotViews([]SyncProfile{{}})
	if err != nil {
		t.Fatal(err)
	}
	if err := snapshot.Abort(); err != nil {
		t.Fatal(err)
	}
	if s.Version() != 0 || !s.PendingDirty() || p.acked != 0 {
		t.Fatal("abort changed state")
	}
	next, err := s.PrepareViews([]SyncProfile{{}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if next.Updates()[0].Full {
		t.Fatal("aborted snapshot forced a full update")
	}
	if err := next.Commit(); err != nil {
		t.Fatal(err)
	}
}

func TestCleanSnapshotViewsDoNotCreateDirtyWork(t *testing.T) {
	p := &trackedPacker{version: 1}
	s := NewSubjectSyncState(SubjectSyncCreateParam{Enabled: true, SubjectID: 42, Packer: p})
	snapshot, err := s.PrepareSnapshotViews([]SyncProfile{{}})
	if err != nil {
		t.Fatal(err)
	}
	if err := snapshot.Commit(); err != nil {
		t.Fatal(err)
	}
	if s.PendingDirty() || s.Version() != 0 || p.acked != 0 {
		t.Fatal("clean snapshot changed dirty state")
	}
	if _, err := s.PrepareViews(nil, nil); !errors.Is(err, ErrSubjectSyncNotDirty) {
		t.Fatal(err)
	}
	if _, err := s.PrepareSnapshotViews(nil); err == nil {
		t.Fatal("accepted an empty snapshot")
	}
}

func TestSnapshotViewsPreserveLaterWritesAndFrozenOwnership(t *testing.T) {
	p := &trackedPacker{version: 1}
	s := NewSubjectSyncState(SubjectSyncCreateParam{Enabled: true, SubjectID: 42, Packer: p})
	s.MarkDirty(1)
	released := 0
	if err := s.FreezeSyncViews(nil, []SyncProfile{{}}, func(int) (func(), bool) {
		return func() { released++ }, true
	}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := s.PrepareSnapshotViews([]SyncProfile{{}})
	if err != nil {
		t.Fatal(err)
	}
	p.version = 2
	s.MarkDirty(2)
	if err := snapshot.Commit(); err != nil {
		t.Fatal(err)
	}
	if released != 1 || p.acked != 0 || !s.PendingDirty() {
		t.Fatal("frozen snapshot lost ownership or later work")
	}
	// A second immediate snapshot must also retain pending changes.
	second, err := s.PrepareSnapshotViews([]SyncProfile{{}})
	if err != nil {
		t.Fatal(err)
	}
	if second.Version() <= snapshot.Version() {
		t.Fatal("snapshot version did not advance")
	}
	if err := second.Commit(); err != nil {
		t.Fatal(err)
	}
	if p.acked != 0 || !s.PendingDirty() {
		t.Fatal("second snapshot consumed work")
	}
	next, err := s.PrepareViews([]SyncProfile{{}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !next.Updates()[0].Full || next.Updates()[0].Payload.BytesCopy()[0] != 2 {
		t.Fatal("lost newer state")
	}
	if err := next.Commit(); err != nil {
		t.Fatal(err)
	}
}
