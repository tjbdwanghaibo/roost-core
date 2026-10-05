package bus

// RR-20261005-NC-92：服务端开了 JetStream RPC 时，请求流收 <prefix>.rpc.>，轻量 Call / CallTo 的
// core request 正落在里面。旧行为：JetStream 用 PubAck 回复 reply inbox，调用方把它当 RPC 响应，报
// “unsupported rpc response version 0”；同一请求随后被 JetStream 消费者执行——轻量信封没有
// DeadlineAt（不过期、handler 无期限）也没有 ReplySubject（回包静默跳过、消息 ACK）。调用方看到失败、
// 服务端实际执行，重试即重复副作用。承诺：没有 ReplySubject 的 JetStream RPC 请求不进入业务；轻量调用
// 收到 PubAck 时返回可识别的 ErrRPCCapturedByJetStream。真实 NATS 见
// kit/nats/jetstream_capture_real_promises_test.go（integration tag）。

import (
	"context"
	"errors"
	"testing"
	"time"

	fnats "github.com/tjbdwanghaibo/roost-core/nats"
)

func TestJetStreamRPCRequestWithoutReplySubjectIsNotExecuted(t *testing.T) {
	js := &captureJetStreamRPC{}
	b := New(nil, nil, nil, Config{Sid: 5001, SvcType: "mail"})
	if err := b.EnableJetStreamRPC(js, JetStreamRPCConfig{RequestTTL: time.Second}); err != nil {
		t.Fatal(err)
	}
	runs := 0
	if err := b.HandleRpc("mail.Grant", func(*RpcContext) (any, error) {
		runs++
		return map[string]int{"code": 0}, nil
	}); err != nil {
		t.Fatal(err)
	}
	// The envelope a lightweight Call sends: session and message id, no reply
	// subject and no deadline.
	data, err := b.codec.Marshal(&fnats.NatsMsg{FromSid: 2001, MsgName: "mail.Grant", SessionId: "lw-1", MsgID: "lw-1", Attempt: 1, CreatedAt: time.Now().UnixMilli()})
	if err != nil {
		t.Fatal(err)
	}
	handler := js.handlerForFilter("roost.rpc.mail.mail.Grant")
	if err := handler(context.Background(), &fnats.JetStreamMsg{Subject: "roost.rpc.mail.mail.Grant", Data: data, NumDelivered: 1}); err != nil {
		t.Errorf("a captured lightweight request must be settled (acknowledged), got %v", err)
	}
	if runs != 0 {
		t.Errorf("business ran %d times for a request no caller can receive the answer of", runs)
	}
}

func TestLightweightCallReportsJetStreamCapture(t *testing.T) {
	rpc := &lifecycleRpc{response: []byte(`{"stream":"ROOST_RPC_REQUESTS","seq":7}`)}
	b := New(&lifecycleClient{}, rpc, nil, Config{Sid: 2001, SvcType: "game"})
	var resp struct{}
	err := b.Call(context.Background(), "mail", "mail.Grant", map[string]int{}, &resp)
	if !errors.Is(err, ErrRPCCapturedByJetStream) {
		t.Errorf("lightweight call answered by a JetStream PubAck = %v, want ErrRPCCapturedByJetStream", err)
	}

	got := make(chan error, 1)
	asyncRPC := &capturingAsyncRpc{response: rpc.response}
	b = New(&lifecycleClient{}, asyncRPC, nil, Config{Sid: 2001, SvcType: "game"})
	b.CallAsync("mail", "mail.Grant", map[string]int{}, func(_ []byte, err error) { got <- err })
	if err := <-got; !errors.Is(err, ErrRPCCapturedByJetStream) {
		t.Errorf("async lightweight call answered by a JetStream PubAck = %v, want ErrRPCCapturedByJetStream", err)
	}

	// A genuine envelope with a bad version keeps its own error.
	rpc = &lifecycleRpc{response: []byte(`{"version":9,"ok":true}`)}
	b = New(&lifecycleClient{}, rpc, nil, Config{Sid: 2001, SvcType: "game"})
	if err := b.Call(context.Background(), "mail", "mail.Grant", map[string]int{}, &resp); err == nil || errors.Is(err, ErrRPCCapturedByJetStream) {
		t.Errorf("unsupported envelope version = %v, want the version error", err)
	}
}

type capturingAsyncRpc struct {
	lifecycleRpc
	response []byte
}

func (r *capturingAsyncRpc) CallAsync(_ string, _ []byte, cb fnats.RpcCallback) { cb(r.response, nil) }
