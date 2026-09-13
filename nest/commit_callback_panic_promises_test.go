package nest

import (
	"errors"
	"testing"
)

// U-0183 · C5 · RR-20260911-06:业务 AfterCommit 回调 panic 不能取消框架的收尾义务。
//
// Commit 先把事务标成 committed,再逐个执行回调,没有逐项异常隔离:一个业务回调 panic,后面的
// 回调全部跳过 —— 包括框架自己追加的 TransactionReleased —— 调用方等不到回复,只能等自己的
// 取消或超时,而事务已经持久化。worker 的 SafeFunc 只在最外层恢复并打日志,恢复了 goroutine,
// 恢复不了被跳过的义务。
//
// 持久化事实不回滚;每个回调都必须被执行(释放通知恰好一次);业务异常变成一个可诊断的错误
// 交给调用方,而不是沉默。
func TestCommitRunsEveryCallbackAndReportsAPanickingOne(t *testing.T) {
	tx := NewRollbackTx(RollbackUndo)
	order := make([]string, 0, 3)
	tx.AfterCommit(func() { order = append(order, "first") })
	tx.AfterCommit(func() { panic("business hook exploded") })
	tx.AfterCommit(func() { order = append(order, "release") })

	err := tx.Commit()
	if err == nil {
		t.Fatal("Commit swallowed a panicking callback without reporting it")
	}
	if !errors.Is(err, ErrAfterCommitFailed) {
		t.Fatalf("Commit error = %v, want ErrAfterCommitFailed", err)
	}
	if len(order) != 2 || order[0] != "first" || order[1] != "release" {
		t.Fatalf("callbacks that ran = %v; the one after the panic must still run", order)
	}
}

// A second Commit on an already committed transaction is a no-op that does
// not re-run callbacks and reports nothing.
func TestCommitIsIdempotent(t *testing.T) {
	tx := NewRollbackTx(RollbackUndo)
	runs := 0
	tx.AfterCommit(func() { runs++ })
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if runs != 1 {
		t.Fatalf("callback ran %d times across two Commits, want once", runs)
	}
}
