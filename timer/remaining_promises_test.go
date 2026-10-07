package timer

import (
	"testing"
	"time"
)

func TestInvalidSavedNodesEmitPersistenceDeletion(t *testing.T) {
	now := time.Unix(100, 0)
	var removed []int64
	s := NewScheduler(1, 0, []Node{{ID: -1, Type: 1, End: now}, {ID: 2, Type: 0, End: now}, {ID: 3, Type: 1}, {ID: 4, Type: 1, End: now}}, func(change ChangeType, node Node) {
		if change != ChangeDelete {
			t.Errorf("invalid restore upserted")
		}
		removed = append(removed, node.ID)
	})
	if len(removed) != 0 {
		t.Fatal("constructor changed storage outside caller transaction")
	}
	s.Tick(now.Add(-time.Second))
	if len(removed) != 3 || len(s.Nodes()) != 1 || s.Nodes()[0].ID != 4 {
		t.Fatalf("invalid nodes remain in persistence: removed=%v live=%v", removed, s.Nodes())
	}
}
