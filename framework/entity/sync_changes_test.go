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

// opLogPacker 像生成的变化树 packer：增量是确认之后的操作记录（列表追加之类不能重放的操作），快照是全部操作。
type opLogPacker struct {
	ops   []byte
	acked int
	packs int
}

func (p *opLogPacker) PackSubjectSnapshot(SyncProfile) (FrozenSyncPayload, error) {
	p.packs++
	return CopyFrozenSyncPayload(3, p.ops), nil
}
func (p *opLogPacker) PackSubjectDelta(SyncProfile, uint64) (FrozenSyncPayload, error) {
	p.packs++
	return CopyFrozenSyncPayload(3, p.ops[p.acked:]), nil
}
func (p *opLogPacker) SubjectSyncChangeVersion() uint64       { return uint64(len(p.ops)) }
func (p *opLogPacker) AcknowledgeSubjectSyncChanges(v uint64) { p.acked = int(v) }

// 在途捕获期间提交的变化冻结成从当时基线起的增量（不再一律升成全量）。在途捕获提交之后这份增量的基线已过时：
// 它含有在途捕获刚交付的操作，直接当新版本的增量会把这些操作重放一次，所以必须按当前基线重新捕获，只带确认之后的操作。
func TestAFreezeDuringAnInFlightCaptureNeverReplaysTheCapturedOperations(t *testing.T) {
	p := &opLogPacker{ops: []byte{'a'}}
	s := NewSubjectSyncState(SubjectSyncCreateParam{Enabled: true, SubjectID: 43, Packer: p})
	profile := SyncProfile{Key: "self"}
	s.MarkDirty(1)
	inflight, err := s.PrepareViews([]SyncProfile{profile}, nil)
	if err != nil {
		t.Fatal(err)
	}
	p.ops = append(p.ops, 'b')
	s.MarkDirty(2)
	if err := s.FreezeSyncViews([]SyncProfile{profile}, nil, func(int) (func(), bool) { return func() {}, true }); err != nil {
		t.Fatal(err)
	}
	if err := inflight.Commit(); err != nil {
		t.Fatal(err)
	}
	next, err := s.PrepareViews([]SyncProfile{profile}, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := next.Updates()[0]
	if got.Full || got.BaseVersion != 1 || got.Version != 2 || string(got.Payload.BytesCopy()) != "b" {
		t.Fatalf("the change after the in-flight capture: full=%v %d→%d ops %q, want a delta 1→2 with only %q",
			got.Full, got.BaseVersion, got.Version, got.Payload.BytesCopy(), "b")
	}
	if err := next.Commit(); err != nil {
		t.Fatal(err)
	}
	if p.acked != 2 || s.PendingDirty() {
		t.Fatalf("acked=%d dirty=%v after delivering both changes", p.acked, s.PendingDirty())
	}
}
