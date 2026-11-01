package dataengine

import "testing"

func TestSyncChangeAcknowledgementAndRollback(t *testing.T) {
	var s syncChanges
	parent := SyncField(2, nil)
	child := SyncField(2, SyncElement(3, SyncField(1, nil)))
	s.mark(parent)
	before := s.snapshot()
	s.mark(child)
	s.ack(before.Version)
	current := s.snapshot()
	if current.Root.Child(SyncStep{Field: 2}).Full() {
		t.Fatal("old replacement was not acknowledged")
	}
	if current.Root.Child(SyncStep{Field: 2}).Child(SyncStep{Key: 3}).Child(SyncStep{Field: 1}) == nil {
		t.Fatal("later child was lost")
	}
	s.restore(before)
	if s.snapshot().Root != nil {
		t.Fatal("rollback resurrected acknowledged changes")
	}
	s.mark(child)
	if s.snapshot().Version <= current.Version {
		t.Fatal("capture sequence was reused")
	}
}

func TestSyncChangeBudgetAndFrozenIsolation(t *testing.T) {
	var s syncChanges
	s.mark(SyncField(2, SyncElement(1, nil)))
	frozen := s.snapshot()
	for i := range maxSyncChangeNodes + 20 {
		s.mark(SyncField(2, SyncElement(i, nil)))
	}
	if !s.snapshot().Root.Full() {
		t.Fatal("budget did not fall back to replacement")
	}
	if len(frozen.Root.Child(SyncStep{Field: 2}).Children) != 1 {
		t.Fatal("frozen snapshot mutated")
	}
	if s.nodes > maxSyncChangeNodes {
		t.Fatal("unbounded metadata")
	}
	s.ack(s.snapshot().Version)
	if s.nodes != 0 || s.snapshot().Root != nil {
		t.Fatal("settlement retained metadata")
	}
}
