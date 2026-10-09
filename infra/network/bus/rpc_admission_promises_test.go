package bus

// RR-20261005-NC-91：轻量 RPC 请求被 Bus 派发池拒绝（队列满，或派发器不在运行）时，旧行为只记日志、
// 开了可靠总线就写进死信，不回包：调用方等满自己的超时并按“结果未知”处理，而服务端明确没有执行；
// 死信条目丢了 reply / SessionId，重放只会把它当普通消息再次落进“no handler”。承诺：被拒绝的 RPC
// 立即收到失败 envelope，且不进入死信；普通消息的派发失败照旧写死信。

import (
	"context"
	"fmt"
	"testing"
	"time"

	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
)

func refusedRPCTask(id string) *incomingTask {
	return &incomingTask{
		isRpc:        true,
		replySubject: "_INBOX." + id,
		natsMsg:      &fnats.NatsMsg{MsgName: "mail.Send", SessionId: id, MsgID: id},
	}
}

func expectRefusalReply(t *testing.T, b *Bus, rpc *lifecycleRpc, label string) {
	t.Helper()
	select {
	case raw := <-rpc.replies:
		if err := decodeRPCResponse(b.codec, raw, &struct{}{}); err == nil {
			t.Errorf("%s: refusal decoded as success", label)
		}
	case <-time.After(time.Second):
		t.Errorf("%s: no reply; the caller waits for its whole timeout and sees an unknown outcome", label)
	}
}

func TestRefusedRPCIsAnsweredAtOnceWhenTheDispatcherIsNotRunning(t *testing.T) {
	store := newReliableMemoryStore()
	rpc := &lifecycleRpc{replies: make(chan []byte, 4)}
	b := New(&lifecycleClient{}, rpc, nil, Config{Sid: 7, SvcType: "mail"})
	b.EnableReliable(store, ReliableConfig{Enabled: true})

	b.dispatchTask(1, refusedRPCTask("not-running"))
	expectRefusalReply(t, b, rpc, "dispatcher not running")
	entries, _ := store.ListDeadLetters(context.Background(), DeadLetterQuery{MsgName: "mail.Send"})
	if len(entries) != 0 {
		t.Errorf("%d refused RPC requests written to the dead-letter queue, first %+v", len(entries), entries[0])
	}
}

func TestRefusedRPCIsAnsweredAtOnceWhenTheQueueIsFull(t *testing.T) {
	store := newReliableMemoryStore()
	rpc := &lifecycleRpc{replies: make(chan []byte, 128)}
	b := New(&lifecycleClient{}, rpc, nil, Config{Sid: 7, SvcType: "mail", WorkerNum: 1, QueueCap: 1})
	b.EnableReliable(store, ReliableConfig{Enabled: true})
	entered, release := make(chan struct{}, 1), make(chan struct{})
	if err := b.HandleRpc("mail.Send", func(*RpcContext) (any, error) {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-release
		return struct{}{}, nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := b.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		close(release)
		_ = b.StopWithContext(context.Background())
	})

	b.dispatchTask(1, refusedRPCTask("running"))
	<-entered
	// The handler holds the only worker; past the queue capacity every request
	// is refused, and while the handler blocks any reply can only be a refusal.
	for i := 0; i < 64; i++ {
		b.dispatchTask(1, refusedRPCTask(fmt.Sprintf("queued-%d", i)))
	}
	expectRefusalReply(t, b, rpc, "queue full")
	entries, _ := store.ListDeadLetters(context.Background(), DeadLetterQuery{MsgName: "mail.Send"})
	if len(entries) != 0 {
		t.Errorf("%d refused RPC requests written to the dead-letter queue, first %+v", len(entries), entries[0])
	}
}
