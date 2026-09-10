package remoteentity

import (
	"context"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/entity"
)

// selectEntryBarrier pins a select at the moment it is entered.
//
// A select evaluates every channel operand before it blocks, so Done() runs
// once, at entry — after waitRemoteTransaction has already taken the tracker
// and its done channel, and before it can observe anything that happens next.
// Returning nil leaves ctx.Done() as a never-ready case, so the select
// resolves on the transaction's own done channel.
type selectEntryBarrier struct {
	context.Context
	entered chan struct{}
	resume  chan struct{}
}

func (c *selectEntryBarrier) Done() <-chan struct{} {
	close(c.entered)
	<-c.resume
	return nil
}

// U-0168 · C8 · RR-20260910-03：等待者必须拿到这笔事务的终态,即使记录已被容量淘汰。
//
// 唤醒后重新查 map(`m.remote.txs[id].status`)是两条路径对同一状态用了不同判据:完成侧刚把
// tracker 标成 closed,准入侧的容量淘汰恰好挑"最旧的已关闭"记录,于是等待者重新取锁时条目
// 已经不在,对 nil 指针取 status 直接 panic —— 而且 panic 发生在 txMu 临界区里,后面的 Unlock
// 不会执行,调用方即使 recover,之后所有事务跟踪都被这把锁挡死。
//
// 缓存淘汰只该影响后续的历史查询,不该影响已经在等的人:等待者从进入 select 之前就持有
// tracker 指针,map 删除只是移出索引,对象还在。
func TestTransactionWaitReturnsTheTerminalStatusAfterEviction(t *testing.T) {
	cfg := DefaultConfig()
	cfg.TransactionTrackLimit = 1
	manager := NewManager(newMockVersionedLockFactory(), cfg, 1000)
	first, second := remoteTestTxID(201), remoteTestTxID(202)

	barrier := &selectEntryBarrier{
		Context: context.Background(),
		entered: make(chan struct{}),
		resume:  make(chan struct{}),
	}
	type outcome struct {
		status   entity.RemoteCommitStatus
		err      error
		panicked any
	}
	done := make(chan outcome, 1)
	go func() {
		var result outcome
		defer func() {
			result.panicked = recover()
			done <- result
		}()
		result.status, result.err = manager.waitRemoteTransaction(barrier, first)
	}()

	select {
	case <-barrier.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("the waiter never reached its select")
	}

	// The transaction completes, then another admission evicts the record it
	// just closed. Both go through the real paths.
	manager.completeRemoteTransaction(first, entity.RemoteCommitStatus{
		TransactionID: first, State: entity.RemoteCommitCommitted,
	})
	if err := manager.trackRemoteTransaction(second); err != nil {
		t.Fatal(err)
	}
	manager.remote.txMu.Lock()
	_, retained := manager.remote.txs[first]
	manager.remote.txMu.Unlock()
	if retained {
		t.Fatal("control: the completed tracker was not evicted, so this test proves nothing")
	}

	close(barrier.resume)
	select {
	case got := <-done:
		if got.panicked != nil {
			t.Fatalf("the waiter panicked instead of answering: %v", got.panicked)
		}
		if got.err != nil {
			t.Fatalf("wait after eviction = %v, want the committed status", got.err)
		}
		if got.status.State != entity.RemoteCommitCommitted {
			t.Fatalf("wait after eviction returned state %v, want committed", got.status.State)
		}
		if got.status.TransactionID != first {
			t.Fatalf("wait returned the status of %v, want %v", got.status.TransactionID, first)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the waiter never returned")
	}

	// The manager is still usable: a panic inside the txMu critical section
	// would have left the lock held forever.
	locked := make(chan error, 1)
	go func() { locked <- manager.trackRemoteTransaction(remoteTestTxID(203)) }()
	select {
	case <-locked:
	case <-time.After(2 * time.Second):
		t.Fatal("txMu is still held; a panic in the critical section skipped its Unlock")
	}
}
