package dataengine

import (
	"errors"
	"testing"
)

func TestTrackerAcceptVersionIsConsecutiveAndCompareAndSwap(t *testing.T) {
	var tracker Tracker
	tracker.SetVersion(4)
	if err := tracker.AcceptVersion(4, 5); err != nil {
		t.Fatal(err)
	}
	if got := tracker.Version(); got != 5 {
		t.Fatalf("version = %d, want 5", got)
	}
	if err := tracker.AcceptVersion(4, 5); !errors.Is(err, ErrInvalidVersion) {
		t.Fatalf("stale accept err = %v, want ErrInvalidVersion", err)
	}
	if err := tracker.AcceptVersion(5, 7); !errors.Is(err, ErrInvalidVersion) {
		t.Fatalf("skipped accept err = %v, want ErrInvalidVersion", err)
	}
}

func TestTrackerPersistenceAcceptDoesNotConsumeSyncDirty(t *testing.T) {
	var tracker Tracker
	tracker.SetVersion(8)
	tracker.MarkSync(0b101)
	if err := tracker.AcceptVersion(8, 9); err != nil {
		t.Fatal(err)
	}
	if got := tracker.TakeSyncDirty(); got != 0b101 {
		t.Fatalf("sync dirty = %b, want 101", got)
	}
}

func TestTrackerRollbackSyncRestoresMask(t *testing.T) {
	var tracker Tracker
	tracker.MarkSync(0b001)
	got := tracker.TakeSyncDirty()
	tracker.MarkSync(0b100)
	tracker.RollbackSync(got)
	if dirty := tracker.TakeSyncDirty(); dirty != 0b101 {
		t.Fatalf("sync dirty = %b, want 101", dirty)
	}
}
