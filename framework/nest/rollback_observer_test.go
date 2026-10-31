package nest

import (
	"errors"
	"testing"
)

func TestRollbackObserversRunAfterAllRestores(t *testing.T) {
	tx := NewRollbackTx(RollbackUndo)
	value := 2
	seen := -1
	calls := 0
	if err := tx.AfterRollback(&value, func() error {
		calls++
		seen = value
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	tx.DeferRollback(func() error { value = 0; return nil })
	_ = tx.AfterRollback(&value, func() error { t.Fatal("duplicate owner"); return nil })
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if seen != 0 || calls != 1 {
		t.Fatal("observer ran before restore")
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal("observer repeated")
	}
}
func TestRollbackObserverSkippedAfterFailedRestore(t *testing.T) {
	tx := NewRollbackTx(RollbackState)
	owner := 0
	_ = tx.AfterRollback(&owner, func() error { t.Fatal("partial state observed"); return nil })
	tx.DeferRollback(func() error { return errors.New("restore failed") })
	if tx.Rollback() == nil {
		t.Fatal("missing restore error")
	}
}

func TestRollbackObserverPanicDoesNotHideOtherRebuilds(t *testing.T) {
	tx := NewRollbackTx(RollbackUndo)
	first, second := 0, 0
	_ = tx.AfterRollback(&first, func() error { panic("rebuild failed") })
	_ = tx.AfterRollback(&second, func() error { second++; return nil })
	if err := tx.Rollback(); err == nil || second != 1 {
		t.Fatalf("rollback error=%v rebuilt=%d", err, second)
	}
	if err := tx.AfterRollback(&first, func() error { return nil }); !errors.Is(err, ErrTransactionClosed) {
		t.Fatal(err)
	}
}
