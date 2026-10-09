package bus

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
)

// RR-20261006-70：JetStream RPC handler 执行期间按 AckWait/2 发 in-progress，只持续到请求期限。
// 真实 broker 上的重复执行红绿见 kit/nats/jetstream_rpc_ackwait_real_promises_test.go；这里固定心跳的边界：
// 期限之前持续、期限之后停（让 broker 重投，接收方按 DeadlineAt 判过期不执行）、stop 之后停。
func TestJetStreamRPCInProgressHeartbeatStopsAtTheRequestDeadline(t *testing.T) {
	b := &Bus{jsRPC: &jetStreamRPC{cfg: JetStreamRPCConfig{AckWait: 40 * time.Millisecond}}}
	var beats atomic.Int32
	msg := &fnats.JetStreamMsg{Subject: "x", InProgress: func() error { beats.Add(1); return nil }}

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	stop := b.keepJetStreamRPCInProgress(msg, ctx)
	<-ctx.Done()
	atDeadline := beats.Load()
	time.Sleep(120 * time.Millisecond)
	stop()
	if atDeadline < 3 {
		t.Fatalf("only %d in-progress acks within 150ms at AckWait 40ms; the broker would redeliver", atDeadline)
	}
	if after := beats.Load(); after > atDeadline+1 {
		t.Fatalf("in-progress continued past the request deadline: %d -> %d", atDeadline, after)
	}

	beats.Store(0)
	stop = b.keepJetStreamRPCInProgress(msg, context.Background())
	stop()
	time.Sleep(60 * time.Millisecond)
	if got := beats.Load(); got != 0 {
		t.Fatalf("in-progress fired %d times after stop", got)
	}
	// 没有 in-progress 能力的投递（测试替身）不出错。
	b.keepJetStreamRPCInProgress(&fnats.JetStreamMsg{}, context.Background())()
}
