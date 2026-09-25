package entitysync

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/entity"
)

func TestModeConfigurationRequiresProducerAndKeepsDefault(t *testing.T) {
	transport := newRecordingTransport()
	for _, config := range []ManagerConfig{{Mode: SyncMode(9)}, {Interval: -1}, {MaxFrozenBytes: -1}} {
		config.Transport = transport
		if _, err := NewManager(config); err == nil {
			t.Fatal("invalid configuration accepted")
		}
	}
	m := newTestManager(t, transport, ManagerConfig{Mode: ModeOnChange})
	if err := m.Start(context.Background()); err == nil {
		t.Fatal("unbound on_change started")
	}
	m.BindSyncProducer()
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if mode, err := ParseSyncMode(""); err != nil || mode != ModePeriodic {
		t.Fatalf("default %v %v", mode, err)
	}
}

func TestOnChangeSnapshotBudgetIsSharedAcrossFlushes(t *testing.T) {
	transport := newRecordingTransport()
	m := newTestManager(t, transport, ManagerConfig{Mode: ModeOnChange, Interval: time.Hour, SnapshotBudget: SnapshotBudget{MaxObjects: 1, PerSessionObjects: 1}})
	open(t, m, 1)
	for id := int64(1); id <= 3; id++ {
		packs := 0
		if err := m.Register(testSubject(t, id, &packs)); err != nil {
			t.Fatal(err)
		}
		mustSubscribe(t, m, 1, id, entity.SyncProfile{})
	}
	mustFlush(t, m)
	mustFlush(t, m)
	mustFlush(t, m)
	if got := m.Counters().CreatesAdmitted; got != 1 {
		t.Fatalf("immediate flush reset budget: %d", got)
	}
	m.budgetWindow = time.Now().Add(-2 * time.Hour)
	mustFlush(t, m)
	if got := m.Counters().CreatesAdmitted; got != 2 {
		t.Fatalf("new window did not refill: %d", got)
	}
}

func TestFrozenContentCoalescesAndRetainsBudgetUntilSettlement(t *testing.T) {
	m := newTestManager(t, newRecordingTransport(), ManagerConfig{Mode: ModeOnChange, MaxFrozenBytes: 2})
	value := byte(1)
	packs := 0
	pack := func(entity.SyncProfile) (entity.FrozenSyncPayload, error) {
		packs++
		return entity.CopyFrozenSyncPayload(1, []byte{value}), nil
	}
	s := entity.NewSubjectSyncState(entity.SubjectSyncCreateParam{Enabled: true, SubjectID: 1, Packer: entity.SubjectSyncPackFunc{Snapshot: pack, Delta: func(p entity.SyncProfile, _ uint64) (entity.FrozenSyncPayload, error) { return pack(p) }}})
	reserve := m.reserveFrozen
	s.MarkDirty(1)
	if err := s.FreezeSyncViews([]entity.SyncProfile{{}}, nil, reserve); err != nil {
		t.Fatal(err)
	}
	first, err := s.Prepare(nil)
	if err != nil {
		t.Fatal(err)
	}
	if m.Counters().FrozenBytes != 1 {
		t.Fatal("inflight bytes were released before settlement")
	}
	value = 2
	s.MarkDirty(1)
	if err = s.FreezeSyncViews([]entity.SyncProfile{{}}, nil, reserve); err != nil {
		t.Fatal(err)
	}
	value = 3
	s.MarkDirty(1)
	if err = s.FreezeSyncViews([]entity.SyncProfile{{}}, nil, reserve); err != nil {
		t.Fatal(err)
	}
	if m.Counters().FrozenBytes != 2 {
		t.Fatal("pending slot not bounded")
	}
	if err = first.Commit(); err != nil {
		t.Fatal(err)
	}
	next, err := s.Prepare(nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := next.Updates()[0]; !got.Full || got.Payload.BytesCopy()[0] != 3 || got.BaseVersion != 1 {
		t.Fatalf("bad coalesced content %+v", got)
	}
	if packs != 3 {
		t.Fatalf("frozen content repacked %d", packs)
	}
	if err = next.Commit(); err != nil {
		t.Fatal(err)
	}
	if m.Counters().FrozenBytes != 0 {
		t.Fatal("retained byte leak")
	}
}

func TestFrozenCapacityFailureLeavesDirtyForRecovery(t *testing.T) {
	m := newTestManager(t, newRecordingTransport(), ManagerConfig{Mode: ModeOnChange, MaxFrozenBytes: 1})
	packs := 0
	s := testSubject(t, 1, &packs)
	s.MarkDirty(1)
	err := s.FreezeSyncViews([]entity.SyncProfile{{}}, nil, m.reserveFrozen)
	if !errors.Is(err, entity.ErrSyncFrozenCapacity) || !s.PendingDirty() {
		t.Fatalf("capacity=%v dirty=%v", err, s.PendingDirty())
	}
	p, err := s.Prepare(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = p.Commit(); err != nil {
		t.Fatal(err)
	}
}

func TestFrozenNewProfileRecapturesOneConsistentVersion(t *testing.T) {
	m := newTestManager(t, newRecordingTransport(), ManagerConfig{Mode: ModeOnChange})
	packs := 0
	s := testSubject(t, 17, &packs)
	s.MarkDirty(1)
	if err := s.FreezeSyncViews([]entity.SyncProfile{{Key: "old"}}, nil, m.reserveFrozen); err != nil {
		t.Fatal(err)
	}
	p, err := s.PrepareViews([]entity.SyncProfile{{Key: "new"}}, []entity.SyncProfile{{Key: "new"}})
	if err != nil {
		t.Fatal(err)
	}
	if packs != 3 || p.Updates()[0].Version != p.Snapshots()[0].Version {
		t.Fatal("mixed profile generation")
	}
	if err = p.Commit(); err != nil {
		t.Fatal(err)
	}
	if m.Counters().FrozenBytes != 0 {
		t.Fatal("discarded profile cache retained bytes")
	}
}

func TestDrainWaitsForBudgetAndHonorsCancellation(t *testing.T) {
	m := newTestManager(t, newRecordingTransport(), ManagerConfig{Mode: ModeOnChange, Interval: time.Millisecond, SnapshotBudget: SnapshotBudget{MaxObjects: 1}})
	open(t, m, 1)
	for id := int64(1); id <= 3; id++ {
		packs := 0
		if err := m.Register(testSubject(t, id, &packs)); err != nil {
			t.Fatal(err)
		}
		mustSubscribe(t, m, 1, id, entity.SyncProfile{})
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := m.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	if got := m.Counters().CreatesAdmitted; got != 3 {
		t.Fatalf("drain admitted %d", got)
	}
	cancelled, stop := context.WithCancel(context.Background())
	stop()
	if err := m.Drain(cancelled); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel: %v", err)
	}
}

func TestDrainIncludesPolicyFactsWithoutDirtySubjects(t *testing.T) {
	m := newTestManager(t, newRecordingTransport(), ManagerConfig{Interval: time.Millisecond})
	cancelPolicy := m.RegisterPolicy(func() error { return nil }, func() bool { return true })
	defer cancelPolicy()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err := m.Drain(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("unconfirmed policy drained: %v", err)
	}
}
