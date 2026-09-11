package remoteentity

import (
	"context"
	"testing"
	"time"
)

// U-0173 · C8 · RR-20260911-03:finalizer 停止之后,延迟关闭不能再交接给已经没人排空的队列。
//
// deferRemoteClose 的 select 同时面对一个还写得进去的缓冲队列和一个已经关闭的
// finalizeCtx.Done,两个都就绪时 Go 随机选,于是停止完成之后它仍然可能选择入队并返回 nil。
// 调用方(batch.Close)据此认为交接成功、不再自己清理,而最终 drain 只等 finalizer workers 与
// retryWG —— 外部发送者不在这道屏障里。结果是 entries、writeGate、ownership 读锁与 finalize
// slot 都留在那儿,finalizeOnce 又不让重启。
//
// 停止与所有外部发送者必须共用同一道准入屏障:要么在屏障内注册、由 drain 负责,要么被拒绝、
// 由调用方自己同步清理。每份 entries 和 slot 恰好一个清理者。
func TestDeferredCloseIsRefusedOnceTheFinalizerHasStopped(t *testing.T) {
	manager := NewManager(newMockVersionedLockFactory(), DefaultConfig(), 1000)
	manager.StartFinalizer()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := manager.StopFinalizer(ctx); err != nil {
		t.Fatal(err)
	}

	// One attempt could pick the Done branch by chance; many cannot all be
	// chance. Every one of them has to be refused.
	const attempts = 64
	accepted := 0
	for i := range attempts {
		item := deferredRemoteClose{txID: remoteTestTxID(byte(i))}
		if err := manager.deferRemoteClose(item); err == nil {
			accepted++
		}
	}
	if accepted != 0 {
		t.Fatalf("%d of %d deferred closes were handed to a queue nobody drains; their entries and finalize slots leak",
			accepted, attempts)
	}
	if queued := len(manager.remote.finalizeQueue); queued != 0 {
		t.Fatalf("%d items are sitting in the finalize queue after the stop completed", queued)
	}
}

// A handoff that starts before the stop must still be drained: refusing it
// would be just as wrong as accepting one too late, because the caller has
// already given up ownership of the entries.
func TestDeferredCloseAcceptedBeforeTheStopIsStillDrained(t *testing.T) {
	manager := NewManager(newMockVersionedLockFactory(), DefaultConfig(), 1000)
	manager.StartFinalizer()

	if err := manager.deferRemoteClose(deferredRemoteClose{txID: remoteTestTxID(7)}); err != nil {
		t.Fatalf("a handoff before the stop must be accepted: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := manager.StopFinalizer(ctx); err != nil {
		t.Fatal(err)
	}
	if queued := len(manager.remote.finalizeQueue); queued != 0 {
		t.Fatalf("%d items were left in the queue after the stop drained it", queued)
	}
}
