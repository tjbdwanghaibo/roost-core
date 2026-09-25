package entitysync

import (
	"testing"
	"time"
)

func TestSnapshotWindowDoesNotDriftOrAccumulate(t *testing.T) {
	m := newTestManager(t, newRecordingTransport(), ManagerConfig{Mode: ModeOnChange, Interval: 10 * time.Millisecond,
		SnapshotBudget: SnapshotBudget{MaxObjects: 2, PerSessionObjects: 1}})
	start := time.Unix(100, 0)
	m.refreshSnapshotWindowAt(start)
	m.windowAllowance.objects, m.windowSessions[1] = 2, 1
	m.refreshSnapshotWindowAt(start.Add(9 * time.Millisecond))
	if m.windowAllowance.objects != 2 || m.windowSessions[1] != 1 {
		t.Fatal("budget refilled before its boundary")
	}
	m.refreshSnapshotWindowAt(start.Add(17 * time.Millisecond))
	if !m.budgetWindow.Equal(start.Add(10*time.Millisecond)) || m.windowAllowance.objects != 0 || len(m.windowSessions) != 0 {
		t.Fatal("late wake shifted next boundary or retained usage")
	}
	m.windowAllowance.objects = 2
	m.refreshSnapshotWindowAt(start.Add(20 * time.Millisecond))
	if m.windowAllowance.objects != 0 {
		t.Fatal("late wake delayed next refill")
	}
	m.refreshSnapshotWindowAt(start.Add(time.Hour + time.Millisecond))
	if !m.budgetWindow.Equal(start.Add(time.Hour)) || m.windowAllowance != (snapshotAllowance{}) {
		t.Fatal("idle windows accumulated credit")
	}
}
