package entity

import (
	"testing"
)

func TestFullDirtyReusesSnapshotAndEmptyViewsStillCommit(t *testing.T) {
	calls := 0
	state := NewSubjectSyncState(SubjectSyncCreateParam{Enabled: true, SubjectID: 42, Packer: SubjectSyncPackFunc{
		Snapshot: func(SyncProfile) (FrozenSyncPayload, error) {
			calls++
			return TakeFrozenSyncPayload(1, []byte("full")), nil
		},
	}})
	state.MarkFullDirty(SyncFullReasonDirty)
	item, err := state.PrepareViews([]SyncProfile{{}}, []SyncProfile{{Key: "default"}})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || !item.Updates()[0].Payload.Equal(item.Snapshots()[0].Payload) {
		t.Fatalf("duplicate pack: %d", calls)
	}
	if item.Updates()[0].BaseVersion == item.Snapshots()[0].BaseVersion {
		t.Fatal("reused payload must not merge different version headers")
	}
	if err := item.Commit(); err != nil {
		t.Fatal(err)
	}
	state.MarkDirty(1)
	state.SetLastCommitLSN(77)
	item, err = state.PrepareViews(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || len(item.Updates()) != 0 || len(item.Snapshots()) != 0 || item.CommitLSN() != 77 {
		t.Fatal("empty views packed or lost watermark")
	}
	if err := item.Commit(); err != nil {
		t.Fatal(err)
	}
	if state.PendingDirty() {
		t.Fatal("unobserved dirty state not committed")
	}
}
