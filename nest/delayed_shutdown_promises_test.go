package nest

import (
	"context"
	"errors"
	"testing"
	"time"
)

// U-0172 · C5 · RR-20260911-04:停机时被回收的延迟消息,必须先把终态回给等待者。
//
// 一个已被接受的同步 Request 带 delay 进了延迟队列还没到期,这时 Shutdown:
// OnDestroyWithContext 取走队列后直接 recycleMsg,没有向 RetChan 发任何东西。Shutdown 返回
// 成功,而调用方还在等自己的 context 或同步超时 —— 一个已经被接受的请求既没有执行结果,也没有
// 明确的停止错误,错误语义退化成"取消/超时"。
//
// 入场时被拒的路径一直是给答案的(返回 ErrNestStopped),所以这是同一件事的两条路径判据不同。
func TestShutdownAnswersDelayedRequestsInsteadOfDroppingThem(t *testing.T) {
	dispatcher := NewDispatcher("nest", 2, 1, 64, func(*Msg) {
		t.Error("a delayed message that never came due must not be dispatched")
	})
	dispatcher.OnInit()

	waiting, ch := GenSyncMsg(MsgTypeSingle)
	dispatcher.delaySendMsg(time.Hour, waiting)
	if got := dispatcher.delayedCount(); got != 1 {
		t.Fatalf("control: delayed count = %d, want 1", got)
	}

	if err := dispatcher.OnDestroyWithContext(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	select {
	case got := <-ch:
		err, ok := got.(error)
		if !ok {
			t.Fatalf("a shut-down dispatcher answered %#v, want an error", got)
		}
		if !errors.Is(err, ErrNestStopped) {
			t.Fatalf("answer = %v, want ErrNestStopped", err)
		}
	case <-time.After(time.Second):
		t.Fatal("the accepted request got no answer at all; the caller is left to its own timeout")
	}
}

// An asynchronous delayed message has no one waiting, so shutting down must
// not try to answer it — and must not block or panic on a nil channel.
func TestShutdownDropsDelayedFireAndForgetQuietly(t *testing.T) {
	dispatcher := NewDispatcher("nest", 2, 1, 64, func(*Msg) {
		t.Error("a delayed message that never came due must not be dispatched")
	})
	dispatcher.OnInit()
	dispatcher.delaySendMsg(time.Hour, GenMsg(MsgTypeSingle))
	if err := dispatcher.OnDestroyWithContext(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if got := dispatcher.delayedCount(); got != 0 {
		t.Fatalf("delayed count after shutdown = %d, want 0", got)
	}
}
