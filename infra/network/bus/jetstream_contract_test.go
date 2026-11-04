package bus

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
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

// deferredJetStreamRequest 是支持推迟结算的投递（与 driver 的 DeferSettle 同一约定）：settled 收到这次投递的结算结果。
func deferredJetStreamRequest(t *testing.T, b *Bus, method, requestID, orderKey string, settled chan<- string) *fnats.JetStreamMsg {
	t.Helper()
	data, err := b.codec.Marshal(&fnats.NatsMsg{
		FromSid:      2001,
		MsgName:      method,
		SessionId:    requestID,
		MsgID:        requestID,
		ReplySubject: "roost.rpc_resp.2001." + requestID,
		DeadlineAt:   time.Now().Add(5 * time.Second).UnixMilli(),
		OrderKey:     orderKey,
	})
	if err != nil {
		t.Fatal(err)
	}
	msg := &fnats.JetStreamMsg{Subject: "roost.rpc.mail." + method, Data: data, NumDelivered: 1}
	var once atomic.Bool
	msg.DeferSettle = func() func(error) {
		if !once.CompareAndSwap(false, true) {
			return nil
		}
		return func(err error) { settled <- fmt.Sprintf("%s %v", requestID, err) }
	}
	return msg
}

// JetStream RPC 的派发与轻量 RPC 同一规则：consumer 回调推迟结算、交给派发后立即返回（一个 consumer 上的请求不再串行），
// 不同顺序键（以及不带键）的请求并行，同一个键按投递顺序逐个处理；每次投递在 handler 返回、应答发布之后才确认，
// Bus 停止等推迟中的投递结算完。
func TestJetStreamRPCDispatchesByOrderKeyAndSettlesAfterTheReply(t *testing.T) {
	b, js := jetStreamStopTestBus(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var mu sync.Mutex
	var events []string
	if err := b.HandleRpc("mail.Apply", func(ctx *RpcContext) (any, error) {
		mu.Lock()
		events = append(events, "start "+ctx.MsgID)
		mu.Unlock()
		if ctx.MsgID == "a1" {
			close(entered)
			<-release
		}
		mu.Lock()
		events = append(events, "end "+ctx.MsgID)
		mu.Unlock()
		return map[string]int{"code": 0}, nil
	}); err != nil {
		t.Fatal(err)
	}
	consume := js.handlerForFilter("roost.rpc.mail.mail.Apply")
	settled := make(chan string, 8)
	deliver := func(requestID, key string) {
		t.Helper()
		returned := make(chan error, 1)
		go func() {
			returned <- consume(context.Background(), deferredJetStreamRequest(t, b, "mail.Apply", requestID, key, settled))
		}()
		select {
		case err := <-returned:
			if err != nil {
				t.Fatalf("delivery %s returned %v; want nil after handing it to the dispatcher", requestID, err)
			}
		case <-time.After(3 * time.Second):
			close(release)
			t.Fatalf("the consumer callback for %s did not return: a blocked handler would hold every later request of this consumer", requestID)
		}
	}
	deliver("a1", "alliance-1")
	<-entered
	deliver("a2", "alliance-1")
	deliver("b1", "alliance-2")
	deliver("u1", "")
	got := map[string]bool{}
	for range 2 {
		select {
		case result := <-settled:
			got[result] = true
		case <-time.After(3 * time.Second):
			close(release)
			t.Fatalf("requests for another key / without a key were not settled while alliance-1 was blocked; settled %v", got)
		}
	}
	if !got["b1 <nil>"] || !got["u1 <nil>"] {
		t.Fatalf("settled %v; want b1 and u1 acknowledged", got)
	}
	for _, id := range []string{"b1", "u1"} {
		if !js.publishedTo("roost.rpc_resp.2001." + id) {
			t.Errorf("%s was settled without its response being published", id)
		}
	}
	mu.Lock()
	for _, event := range events {
		if event == "start a2" {
			mu.Unlock()
			close(release)
			t.Fatalf("a2 entered the handler while a1 (same order key) was still running: %v", events)
		}
	}
	mu.Unlock()

	// 停止等推迟中的投递：a1 还卡着时有预算的停止返回 ctx 错误。
	budget, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := stopWithin(t, b, budget, "budgeted stop"); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("stop with a deferred delivery in the handler = %v; want the budget's ctx error", err)
	}
	close(release)
	if err := stopWithin(t, b, context.Background(), "retry stop"); err != nil {
		t.Fatalf("retry stop = %v", err)
	}
	for _, want := range []string{"a1", "a2"} {
		select {
		case result := <-settled:
			if !strings.HasPrefix(result, want+" ") {
				t.Fatalf("settled %q; want %s next (same key, delivery order)", result, want)
			}
		case <-time.After(time.Second):
			t.Fatalf("%s was not settled after the stop drained", want)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	order := []string{}
	for _, event := range events {
		if strings.HasSuffix(event, " a1") || strings.HasSuffix(event, " a2") {
			order = append(order, event)
		}
	}
	if fmt.Sprint(order) != "[start a1 end a1 start a2 end a2]" {
		t.Fatalf("alliance-1 ran %v; want a1 then a2, one at a time", order)
	}
}
