package bus

// RR-20261005-NC-90：JetStream RPC 的 handler 在 nats.go consume 回调 goroutine 里执行，不在 Bus
// 的 pool 里。旧行为：停止只排空 pool，对 JetStream 订阅调用 Stop 后立即返回 nil，handler 还在跑，
// NatsMod 接着关闭连接；handler 的 processCtx 随 Bus 停止被取消，回包用它发布，于是即使 handler 在
// 停止预算内完成，回包也必然失败，消息在 AckWait 后重投到其他实例再执行一次。承诺：
//   - 在途 handler 未退出时 StopWithContext 只返回 ctx 错误并保留责任，重试继续等它；
//   - 在预算内完成的 handler 回包送达、消息 ACK；
//   - 因停止被取消而中断的 handler 不回“已取消”，消息交还 broker（NAK）；
//   - 停止开始后才到达的投递不进入业务。
// 真实 NATS 上的同一承诺见 kit/nats/jetstream_stop_real_promises_test.go（integration tag）。

import (
	"context"
	"errors"
	"testing"
	"time"

	fnats "github.com/tjbdwanghaibo/roost-core/nats"
)

func jetStreamStopTestBus(t *testing.T) (*Bus, *captureJetStreamRPC) {
	t.Helper()
	js := &captureJetStreamRPC{respectPublishContext: true}
	b := New(&lifecycleClient{}, &lifecycleRpc{}, nil, Config{Sid: 8001, SvcType: "mail", WorkerNum: 1})
	if err := b.Start(); err != nil {
		t.Fatal(err)
	}
	if err := b.EnableJetStreamRPC(js, JetStreamRPCConfig{RequestTTL: 5 * time.Second}); err != nil {
		t.Fatal(err)
	}
	return b, js
}

func jetStreamStopTestRequest(t *testing.T, b *Bus, method, requestID string) *fnats.JetStreamMsg {
	t.Helper()
	data, err := b.codec.Marshal(&fnats.NatsMsg{
		FromSid:      2001,
		MsgName:      method,
		SessionId:    requestID,
		MsgID:        requestID,
		ReplySubject: "roost.rpc_resp.2001." + requestID,
		DeadlineAt:   time.Now().Add(5 * time.Second).UnixMilli(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return &fnats.JetStreamMsg{Subject: "roost.rpc.mail." + method, Data: data, NumDelivered: 1}
}

func (c *captureJetStreamRPC) publishedTo(subject string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, p := range c.publishes {
		if p.subject == subject {
			return true
		}
	}
	return false
}

func stopWithin(t *testing.T, b *Bus, ctx context.Context, label string) error {
	t.Helper()
	result := make(chan error, 1)
	go func() { result <- b.StopWithContext(ctx) }()
	select {
	case err := <-result:
		return err
	case <-time.After(3 * time.Second):
		t.Fatalf("%s did not return within 3s", label)
		return nil
	}
}

func TestJetStreamStopWaitsForInFlightHandlerAndDeliversItsResponse(t *testing.T) {
	b, js := jetStreamStopTestBus(t)
	entered, release := make(chan struct{}), make(chan struct{})
	if err := b.HandleRpc("mail.Slow", func(*RpcContext) (any, error) {
		close(entered)
		<-release // business that does not watch ctx, e.g. a blocking store write
		return map[string]int{"code": 0}, nil
	}); err != nil {
		t.Fatal(err)
	}
	handler := js.handlerForFilter("roost.rpc.mail.mail.Slow")
	settled := make(chan error, 1)
	go func() {
		settled <- handler(context.Background(), jetStreamStopTestRequest(t, b, "mail.Slow", "req-slow"))
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("handler never entered")
	}

	budget, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	first := stopWithin(t, b, budget, "budgeted stop")
	if !errors.Is(first, context.DeadlineExceeded) {
		t.Errorf("stop with a JetStream handler still running = %v, want the budget's ctx error", first)
	}

	close(release)
	if retry := stopWithin(t, b, context.Background(), "retry stop"); retry != nil {
		t.Errorf("retry stop after the handler exited = %v, want nil", retry)
	}
	select {
	case err := <-settled:
		if err != nil {
			t.Errorf("handler that finished within the stop returned %v: the delivery is NAKed and redelivered", err)
		}
	case <-time.After(time.Second):
		t.Fatal("handler did not return")
	}
	if !js.publishedTo("roost.rpc_resp.2001.req-slow") {
		t.Error("the response of a handler that finished within the stop was never published")
	}
}

func TestJetStreamStopHandsInterruptedRequestBackToBroker(t *testing.T) {
	b, js := jetStreamStopTestBus(t)
	entered := make(chan struct{})
	if err := b.HandleRpc("mail.Wait", func(ctx *RpcContext) (any, error) {
		close(entered)
		<-ctx.Context().Done()
		return nil, ctx.Context().Err()
	}); err != nil {
		t.Fatal(err)
	}
	handler := js.handlerForFilter("roost.rpc.mail.mail.Wait")
	settled := make(chan error, 1)
	go func() {
		settled <- handler(context.Background(), jetStreamStopTestRequest(t, b, "mail.Wait", "req-wait"))
	}()
	<-entered
	if err := stopWithin(t, b, context.Background(), "stop"); err != nil {
		t.Fatalf("stop = %v", err)
	}
	if err := <-settled; err == nil {
		t.Error("a handler interrupted by the stop was acknowledged; it must go back to the broker for another instance")
	}
	if js.publishedTo("roost.rpc_resp.2001.req-wait") {
		t.Error("an interrupted handler published a cancellation as its business result")
	}
}

func TestJetStreamDeliveryAfterStopDoesNotRunBusiness(t *testing.T) {
	b, js := jetStreamStopTestBus(t)
	runs := 0
	if err := b.HandleRpc("mail.Late", func(*RpcContext) (any, error) {
		runs++
		return map[string]int{"code": 0}, nil
	}); err != nil {
		t.Fatal(err)
	}
	handler := js.handlerForFilter("roost.rpc.mail.mail.Late")
	if err := stopWithin(t, b, context.Background(), "stop"); err != nil {
		t.Fatalf("stop = %v", err)
	}
	if err := handler(context.Background(), jetStreamStopTestRequest(t, b, "mail.Late", "req-late")); err == nil {
		t.Error("a delivery that arrived after the stop was acknowledged")
	}
	if runs != 0 {
		t.Errorf("business ran %d times for a delivery after the stop", runs)
	}
}
