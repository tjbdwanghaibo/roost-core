package entity

import (
	"errors"
	"sync"
	"testing"
	"time"

	flock "github.com/tjbdwanghaibo/cube-core/lock"
)

func TestFrozenSyncPayloadOwnership(t *testing.T) {
	source := []byte{1, 2, 3}
	copied := CopyFrozenSyncPayload(7, source)
	source[0] = 9
	if got := copied.BytesCopy(); copied.Codec() != 7 || len(got) != 3 || got[0] != 1 {
		t.Fatalf("copied payload changed: codec=%d data=%v", copied.Codec(), got)
	}
	read := copied.BytesCopy()
	read[1] = 9
	if copied.BytesCopy()[1] != 2 {
		t.Fatal("BytesCopy exposed mutable storage")
	}
}

func TestSubjectSyncPrepareCommitProfiles(t *testing.T) {
	var packed []SyncProfile
	state := NewSubjectSyncState(SubjectSyncCreateParam{
		Enabled: true, SubjectID: 42,
		Packer: SubjectSyncPackFunc{Delta: func(profile SyncProfile, mask uint64) (FrozenSyncPayload, error) {
			packed = append(packed, profile)
			return TakeFrozenSyncPayload(1, []byte{byte(profile.LOD), byte(mask)}), nil
		}},
	})
	state.MarkDirty(3)
	prepared, err := state.Prepare([]SyncProfile{{Key: "near", LOD: 1}, {Key: "far", LOD: 2}, {Key: "near", LOD: 1}})
	if err != nil {
		t.Fatal(err)
	}
	updates := prepared.Updates()
	if len(packed) != 2 || len(updates) != 2 {
		t.Fatalf("profiles were not deduplicated: packed=%d updates=%d", len(packed), len(updates))
	}
	for _, update := range updates {
		if update.SubjectID != 42 || update.Version != 1 || update.BaseVersion != 0 || update.Mask != 3 {
			t.Fatalf("invalid prepared update: %+v", update)
		}
	}
	if state.Version() != 0 || !state.PendingDirty() {
		t.Fatal("prepare committed state before delivery")
	}
	if err := prepared.Commit(); err != nil {
		t.Fatal(err)
	}
	if state.Version() != 1 || state.PendingDirty() {
		t.Fatalf("commit failed: version=%d dirty=%v", state.Version(), state.PendingDirty())
	}
}

func TestSubjectSyncReclaimsAbandonedPrepare(t *testing.T) {
	// Regression: a PreparedSubjectSync dropped without Commit/Abort held the
	// in-flight token forever and permanently stalled the subject's sync.
	state := NewSubjectSyncState(SubjectSyncCreateParam{
		Enabled: true, SubjectID: 44,
		Packer: SubjectSyncPackFunc{Delta: func(SyncProfile, uint64) (FrozenSyncPayload, error) {
			return TakeFrozenSyncPayload(1, []byte{1}), nil
		}},
	})
	state.MarkDirty(1)
	abandoned, err := state.Prepare(nil)
	if err != nil {
		t.Fatal(err)
	}
	// A fresh in-flight prepare still blocks new prepares.
	if _, err := state.Prepare(nil); !errors.Is(err, ErrSubjectSyncInFlight) {
		t.Fatalf("fresh in-flight token must block: %v", err)
	}
	// Simulate the abandoned prepare aging past the stale threshold.
	state.mu.Lock()
	state.inflightSince = time.Now().Add(-subjectSyncInFlightStaleAfter - time.Second)
	state.mu.Unlock()

	reclaimed, err := state.Prepare(nil)
	if err != nil {
		t.Fatalf("stale in-flight token was not reclaimed: %v", err)
	}
	// The abandoned prepare lost its reservation: its commit must fail stale
	// instead of racing the reclaimed one.
	if err := abandoned.Commit(); !errors.Is(err, ErrSubjectSyncStalePrepare) {
		t.Fatalf("abandoned commit must be stale, got %v", err)
	}
	if err := reclaimed.Commit(); err != nil {
		t.Fatal(err)
	}
	if state.Version() != 1 {
		t.Fatalf("reclaimed prepare did not commit: version=%d", state.Version())
	}
}

func TestSubjectSyncAbortAndConcurrentDirty(t *testing.T) {
	state := NewSubjectSyncState(SubjectSyncCreateParam{
		Enabled: true, SubjectID: 43,
		Packer: SubjectSyncPackFunc{Delta: func(SyncProfile, uint64) (FrozenSyncPayload, error) {
			return TakeFrozenSyncPayload(1, []byte("delta")), nil
		}},
	})
	state.MarkDirty(1)
	prepared, err := state.Prepare(nil)
	if err != nil {
		t.Fatal(err)
	}
	state.MarkDirty(2)
	if err := prepared.Commit(); err != nil {
		t.Fatal(err)
	}
	if state.Version() != 1 || !state.PendingDirty() {
		t.Fatalf("new dirty was lost: version=%d dirty=%v", state.Version(), state.PendingDirty())
	}
	second, err := state.Prepare(nil)
	if err != nil {
		t.Fatal(err)
	}
	wantErr := errors.New("downstream unavailable")
	if err := second.AbortWithError(wantErr); err != nil {
		t.Fatal(err)
	}
	if state.Version() != 1 || !state.PendingDirty() || !errors.Is(state.LastError(), wantErr) {
		t.Fatalf("abort changed content state: version=%d dirty=%v err=%v", state.Version(), state.PendingDirty(), state.LastError())
	}
}

func TestSubjectSyncSnapshotDoesNotAdvanceVersion(t *testing.T) {
	state := NewSubjectSyncState(SubjectSyncCreateParam{
		Enabled: true, SubjectID: 44,
		Packer: SubjectSyncPackFunc{
			Snapshot: func(profile SyncProfile) (FrozenSyncPayload, error) {
				return TakeFrozenSyncPayload(1, []byte(profile.Key)), nil
			},
			Delta: func(SyncProfile, uint64) (FrozenSyncPayload, error) {
				return TakeFrozenSyncPayload(1, []byte("delta")), nil
			},
		},
	})
	state.MarkDirty(1)
	prepared, err := state.Prepare(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := prepared.Commit(); err != nil {
		t.Fatal(err)
	}
	snapshots, err := state.CaptureSnapshot([]SyncProfile{{Key: "near"}}, SyncFullReasonResync)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshots) != 1 || snapshots[0].Version != 1 || snapshots[0].BaseVersion != 1 || state.Version() != 1 {
		t.Fatalf("snapshot advanced shared version: snapshot=%+v state=%d", snapshots, state.Version())
	}
}

func TestSubjectSyncEntityLockOrderAvoidsGuardInversion(t *testing.T) {
	id := mustBuildTestEntityID(t, 901, EntityCategory(1), EntityKind(2))
	base := NewEntityBase(id, EntityCategory(1), false, EntityKind(2))
	mu := flock.NewDefaultMutex(id)
	base.mu = mu
	base.EnableSync(EntitySyncCreateParam{
		Enabled: true, EntityID: id,
		Packer: SubjectSyncPackFunc{Delta: func(SyncProfile, uint64) (FrozenSyncPayload, error) {
			return TakeFrozenSyncPayload(1, []byte("delta")), nil
		}},
	})
	state := base.Sync()
	state.MarkDirty(1)

	mu.Lock()
	asyncStarted := make(chan struct{})
	asyncDone := make(chan struct{})
	var asyncErr error
	go func() {
		close(asyncStarted)
		prepared, err := state.Prepare(nil)
		asyncErr = err
		if err == nil {
			_ = prepared.Abort()
		}
		close(asyncDone)
	}()
	<-asyncStarted

	// The async caller must be waiting for the Entity lock WITHOUT holding
	// prepareMu, so a guarded caller can take prepareMu while the entity lock
	// is still held. Both halves are asserted: the earlier version of this
	// test had no assertions at all, so it also passed when Prepare returned
	// immediately (never waiting for the lock) and its error was discarded.
	prepareMuTaken := make(chan struct{})
	go func() {
		state.prepareMu.Lock()
		state.prepareMu.Unlock()
		close(prepareMuTaken)
	}()
	select {
	case <-prepareMuTaken:
	case <-time.After(2 * time.Second):
		t.Fatal("guarded caller could not take prepareMu while the entity lock was held: lock order inverted")
	}

	// Prepare must still be blocked on the entity lock at this point.
	select {
	case <-asyncDone:
		t.Fatalf("Prepare completed while the entity lock was held (err=%v); it did not wait for Entity", asyncErr)
	case <-time.After(50 * time.Millisecond):
	}

	mu.Unlock()
	select {
	case <-asyncDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Prepare did not complete after the entity lock was released")
	}
	if asyncErr != nil {
		t.Fatalf("Prepare after lock release: %v", asyncErr)
	}
}

func TestSubjectSyncConcurrentMarkDirtyRace(t *testing.T) {
	state := NewSubjectSyncState(SubjectSyncCreateParam{
		Enabled: true, SubjectID: 45,
		Packer: SubjectSyncPackFunc{Delta: func(SyncProfile, uint64) (FrozenSyncPayload, error) {
			return TakeFrozenSyncPayload(1, []byte("delta")), nil
		}},
	})
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(bit uint) {
			defer wg.Done()
			state.MarkDirty(uint64(1) << (bit % 16))
		}(uint(i))
	}
	wg.Wait()
	prepared, err := state.Prepare(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := prepared.Commit(); err != nil {
		t.Fatal(err)
	}
}

func TestSubjectSyncDirtyNotifierRunsOutsideStateLock(t *testing.T) {
	state := NewSubjectSyncState(SubjectSyncCreateParam{Enabled: true, SubjectID: 810})
	notified := make(chan struct{}, 1)
	state.SetDirtyNotifier(func(current *SubjectSyncState) {
		if !current.PendingDirty() {
			t.Error("notifier observed clean state")
		}
		notified <- struct{}{}
	})
	state.MarkDirty(1)
	select {
	case <-notified:
	case <-time.After(time.Second):
		t.Fatal("dirty notifier was not called")
	}
}

func TestSubjectSyncDirtyNotifierPanicIsContained(t *testing.T) {
	state := NewSubjectSyncState(SubjectSyncCreateParam{Enabled: true, SubjectID: 811})
	state.SetDirtyNotifier(func(*SubjectSyncState) { panic("notifier panic") })
	state.MarkFullDirty(SyncFullReasonDirty)
	if !state.PendingDirty() {
		t.Fatal("notifier panic lost dirty state")
	}
}

func TestPreparedSubjectSyncBatchOwnsCompletionAndCommitsAll(t *testing.T) {
	newState := func(id int64) *SubjectSyncState {
		return NewSubjectSyncState(SubjectSyncCreateParam{
			Enabled: true, SubjectID: id,
			Packer: SubjectSyncPackFunc{Delta: func(SyncProfile, uint64) (FrozenSyncPayload, error) {
				return TakeFrozenSyncPayload(1, []byte("delta")), nil
			}},
		})
	}
	first, second := newState(9011), newState(9012)
	first.MarkDirty(1)
	second.MarkDirty(2)
	firstPrepared, err := first.Prepare(nil)
	if err != nil {
		t.Fatal(err)
	}
	secondPrepared, err := second.Prepare(nil)
	if err != nil {
		t.Fatal(err)
	}
	batch, err := ReservePreparedSubjectSyncBatch([]*PreparedSubjectSync{secondPrepared, firstPrepared})
	if err != nil {
		t.Fatal(err)
	}
	if err := firstPrepared.Commit(); !errors.Is(err, ErrSubjectSyncFinished) {
		t.Fatalf("reserved item Commit error=%v", err)
	}
	if err := secondPrepared.Abort(); !errors.Is(err, ErrSubjectSyncFinished) {
		t.Fatalf("reserved item Abort error=%v", err)
	}
	if err := batch.Commit(); err != nil {
		t.Fatal(err)
	}
	if first.Version() != 1 || second.Version() != 1 || first.PendingDirty() || second.PendingDirty() {
		t.Fatalf("batch state first=(%d,%v) second=(%d,%v)", first.Version(), first.PendingDirty(), second.Version(), second.PendingDirty())
	}
}

func TestPreparedSubjectSyncBatchSurvivesCloseAfterReservation(t *testing.T) {
	state := NewSubjectSyncState(SubjectSyncCreateParam{
		Enabled: true, SubjectID: 9013,
		Packer: SubjectSyncPackFunc{Delta: func(SyncProfile, uint64) (FrozenSyncPayload, error) {
			return TakeFrozenSyncPayload(1, []byte("delta")), nil
		}},
	})
	state.MarkDirty(1)
	prepared, err := state.Prepare(nil)
	if err != nil {
		t.Fatal(err)
	}
	batch, err := ReservePreparedSubjectSyncBatch([]*PreparedSubjectSync{prepared})
	if err != nil {
		t.Fatal(err)
	}
	state.Close()
	if err := batch.Commit(); err != nil {
		t.Fatalf("reserved durable admission became stale during Close: %v", err)
	}
	if state.Version() != 1 || state.Enabled() {
		t.Fatalf("closed state version=%d enabled=%v", state.Version(), state.Enabled())
	}
}
