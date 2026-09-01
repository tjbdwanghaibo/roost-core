package bus

import (
	"context"
	"errors"
	"fmt"
	fnats "github.com/tjbdwanghaibo/cube-core/nats"
	"testing"
	"time"
)

type lifecycleClient struct {
	failAt int
	subs   []*lifecycleSub
}

func (c *lifecycleClient) Publish(subject string, data []byte) error { return nil }

func (c *lifecycleClient) Request(subject string, data []byte, timeout time.Duration) ([]byte, error) {
	return nil, nil
}

func (c *lifecycleClient) Subscribe(subject string, handler fnats.MsgHandler) (fnats.ISubscription, error) {
	if c.failAt > 0 && len(c.subs)+1 == c.failAt {
		return nil, fmt.Errorf("subscribe failed")
	}
	sub := &lifecycleSub{valid: true, subject: subject, handler: handler}
	c.subs = append(c.subs, sub)
	return sub, nil
}

func (c *lifecycleClient) QueueSubscribe(subject string, queue string, handler fnats.MsgHandler) (fnats.ISubscription, error) {
	if c.failAt > 0 && len(c.subs)+1 == c.failAt {
		return nil, fmt.Errorf("subscribe failed")
	}
	sub := &lifecycleSub{valid: true, subject: subject, queue: queue, handler: handler}
	c.subs = append(c.subs, sub)
	return sub, nil
}

func (c *lifecycleClient) Drain() error { return nil }

func (c *lifecycleClient) Close() {}

type lifecycleSub struct {
	valid       bool
	unsubscribe int
	subject     string
	queue       string
	handler     fnats.MsgHandler
}

func (s *lifecycleSub) Unsubscribe() error {
	s.valid = false
	s.unsubscribe++
	return nil
}

func (s *lifecycleSub) IsValid() bool { return s.valid }

func TestBusStopBeforeStartIsSafe(t *testing.T) {
	b := New(&lifecycleClient{}, nil, nil, Config{Sid: 1, SvcType: "game"})
	b.Stop()
	b.Stop()
}

func TestBusStartFailureCleansPartialSubscriptions(t *testing.T) {
	client := &lifecycleClient{failAt: 2}
	b := New(client, nil, nil, Config{Sid: 1, SvcType: "game"})

	if err := b.Start(); err == nil {
		t.Fatal("expected start failure")
	}
	if b.pool != nil {
		t.Fatal("expected pool to be cleared after start failure")
	}
	if len(b.subs) != 0 {
		t.Fatalf("expected subscriptions to be cleared, got %d", len(b.subs))
	}
	if len(client.subs) != 1 || client.subs[0].unsubscribe != 1 {
		t.Fatalf("expected partial subscription to be unsubscribed once, got %#v", client.subs)
	}

	b.Stop()
}

func TestBusRejectsRestartAfterStop(t *testing.T) {
	// Regression: Stop tears down RPC subscriptions registered via HandleRpc
	// but Start only rebuilds the base subjects, so a restarted Bus silently
	// lost every RPC subject. Restart is rejected instead of half-working.
	client := &lifecycleClient{}
	b := New(client, nil, nil, Config{Sid: 1, SvcType: "game"})
	if err := b.Start(); err != nil {
		t.Fatal(err)
	}
	b.Stop()
	if err := b.Start(); err == nil {
		t.Fatal("restart after stop must be rejected")
	}
	// Cleanup of a failed Start keeps the Bus eligible for a retry.
	retryClient := &lifecycleClient{failAt: 1}
	retry := New(retryClient, nil, nil, Config{Sid: 2, SvcType: "game"})
	if err := retry.Start(); err == nil {
		t.Fatal("expected start failure")
	}
	retryClient.failAt = 0
	if err := retry.Start(); err != nil {
		t.Fatalf("retry after failed start must stay possible: %v", err)
	}
	retry.Stop()
}

func TestHandleRpcReturnsSubscribeError(t *testing.T) {
	client := &lifecycleClient{failAt: 1}
	b := New(client, nil, nil, Config{Sid: 1, SvcType: "rank"})

	if err := b.HandleRpc("rank.GetTop", func(*RpcContext) (any, error) { return nil, nil }); err == nil {
		t.Fatal("expected HandleRpc to return subscribe error")
	}
	if len(b.subs) != 0 {
		t.Fatalf("failed rpc subscription should not be retained, got %d", len(b.subs))
	}
}

func TestHandleRejectsNilAndDuplicateRegistration(t *testing.T) {
	b := New(&lifecycleClient{}, nil, nil, Config{Sid: 1, SvcType: "game"})
	if err := b.Handle("mail", "Changed", nil); err == nil {
		t.Fatal("nil message handler was accepted")
	}
	if err := b.Handle("mail", "Changed", func(*MsgContext) {}); err != nil {
		t.Fatalf("first Handle: %v", err)
	}
	if err := b.Handle("mail", "Changed", func(*MsgContext) {}); err == nil {
		t.Fatal("duplicate message handler was accepted")
	}
}

func TestHandleRpcSubscribesServiceQueueAndInstanceSubject(t *testing.T) {
	client := &lifecycleClient{}
	b := New(client, nil, nil, Config{Sid: 8001, SvcType: "instance"})

	if err := b.HandleRpc("instance.GetState", func(*RpcContext) (any, error) { return nil, nil }); err != nil {
		t.Fatalf("HandleRpc: %v", err)
	}
	if len(client.subs) != 2 {
		t.Fatalf("subscription count = %d, want 2", len(client.subs))
	}
	if client.subs[0].subject != "cube.rpc.instance.instance.GetState" || client.subs[0].queue != "instance_rpc" {
		t.Fatalf("service rpc subscription = subject:%q queue:%q", client.subs[0].subject, client.subs[0].queue)
	}
	if client.subs[1].subject != "cube.rpc.instance.8001.instance.GetState" || client.subs[1].queue != "" {
		t.Fatalf("instance rpc subscription = subject:%q queue:%q", client.subs[1].subject, client.subs[1].queue)
	}
}

type lifecycleRpc struct {
	replies  chan []byte
	subject  string
	request  []byte
	response []byte
	callErr  error
}

func (r *lifecycleRpc) Call(_ context.Context, subject string, request []byte) ([]byte, error) {
	r.subject = subject
	r.request = append([]byte(nil), request...)
	if r.callErr != nil || r.response != nil {
		return r.response, r.callErr
	}
	return encodeRPCSuccess(JSONCodec{}, struct{}{})
}

func (r *lifecycleRpc) CallWithTimeout(string, []byte, time.Duration) ([]byte, error) {
	return nil, nil
}

func (r *lifecycleRpc) CallAsync(string, []byte, fnats.RpcCallback) {}

func (r *lifecycleRpc) Reply(_ string, resp []byte) error {
	r.replies <- resp
	return nil
}

func TestBusRpcEnvelopeUsesDeclaredDottedMethod(t *testing.T) {
	rpc := &lifecycleRpc{replies: make(chan []byte, 1)}
	b := New(&lifecycleClient{}, rpc, nil, Config{Sid: 5001, SvcType: "mail", WorkerNum: 1})
	handled := make(chan struct{})
	if err := b.HandleRpc("mail.Summary", func(ctx *RpcContext) (any, error) {
		var req struct {
			PlayerID int64 `json:"player_id"`
		}
		if err := ctx.Decode(&req); err != nil {
			t.Fatalf("Decode: %v", err)
		}
		if req.PlayerID != 100 {
			t.Fatalf("PlayerID: %d", req.PlayerID)
		}
		close(handled)
		return map[string]int{"code": 0}, nil
	}); err != nil {
		t.Fatalf("HandleRpc: %v", err)
	}
	if err := b.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer b.Stop()

	payload, err := b.codec.Marshal(struct {
		PlayerID int64 `json:"player_id"`
	}{PlayerID: 100})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	wire, err := b.codec.Marshal(&fnats.NatsMsg{
		FromSid: 1001, MsgName: "mail.Summary", Payload: payload,
		SessionId: "request-1", MsgID: "request-1", Attempt: 1,
	})
	if err != nil {
		t.Fatalf("Marshal envelope: %v", err)
	}
	b.onRpcMessage(&fnats.Msg{
		Subject: "cube.rpc.mail.mail.Summary",
		Reply:   "reply",
		Data:    wire,
	})

	select {
	case <-handled:
	case <-time.After(time.Second):
		t.Fatal("rpc handler was not called")
	}
	select {
	case resp := <-rpc.replies:
		if len(resp) == 0 {
			t.Fatal("rpc reply should not be empty")
		}
	case <-time.After(time.Second):
		t.Fatal("rpc reply was not sent")
	}
}

func TestBusCallToUsesInstanceRpcSubject(t *testing.T) {
	rpc := &lifecycleRpc{}
	b := New(&lifecycleClient{}, rpc, nil, Config{Sid: 2001, SvcType: "game"})

	var resp struct{}
	if err := b.CallTo(context.Background(), "instance", 8001, "instance.GetState", map[string]int{"id": 1}, &resp); err != nil {
		t.Fatalf("CallTo: %v", err)
	}
	if rpc.subject != "cube.rpc.instance.8001.instance.GetState" {
		t.Fatalf("rpc subject = %q", rpc.subject)
	}
	var envelope fnats.NatsMsg
	if err := b.codec.Unmarshal(rpc.request, &envelope); err != nil {
		t.Fatalf("decode request envelope: %v", err)
	}
	if envelope.MsgName != "instance.GetState" || envelope.ToSid != 8001 || envelope.SessionId == "" || envelope.MsgID != envelope.SessionId {
		t.Fatalf("request envelope = %+v", envelope)
	}
}

func TestBusCallReturnsRemoteBusinessError(t *testing.T) {
	rpc := &lifecycleRpc{}
	b := New(&lifecycleClient{}, rpc, nil, Config{Sid: 2001, SvcType: "game"})
	response, err := encodeRPCFailure(b.codec, errors.New("storage unavailable"))
	if err != nil {
		t.Fatal(err)
	}
	rpc.response = response

	var resp struct{ Balance int64 }
	err = b.Call(context.Background(), "wallet", "wallet.GetBalance", struct{}{}, &resp)
	if err == nil {
		t.Fatalf("remote business failure decoded as success: %+v", resp)
	}
}

func TestBusStopDoesNotDeadlockHandlerAttemptingRPCRegistration(t *testing.T) {
	b := New(&lifecycleClient{}, &lifecycleRpc{replies: make(chan []byte, 1)}, nil, Config{Sid: 7, SvcType: "game", WorkerNum: 1})
	entered := make(chan struct{})
	release := make(chan struct{})
	registration := make(chan error, 1)
	if err := b.Handle("mail", "Changed", func(*MsgContext) {
		close(entered)
		<-release
		registration <- b.HandleRpc("mail.Late", func(*RpcContext) (any, error) { return nil, nil })
	}); err != nil {
		t.Fatal(err)
	}
	if err := b.Start(); err != nil {
		t.Fatal(err)
	}
	wire, err := b.encodeMsg(7, "mail", "Changed", struct{}{}, fnats.BroadcastNone)
	if err != nil {
		t.Fatal(err)
	}
	b.onMessage(&fnats.Msg{Data: wire})
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("handler did not start")
	}

	stopDone := make(chan error, 1)
	stopCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	go func() { stopDone <- b.StopWithContext(stopCtx) }()
	deadline := time.Now().Add(time.Second)
	for {
		b.lifeMu.Lock()
		stopping := b.stopping
		b.lifeMu.Unlock()
		if stopping {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("bus did not enter stopping state")
		}
		time.Sleep(time.Millisecond)
	}
	close(release)
	if err := <-registration; err == nil {
		t.Fatal("rpc registration succeeded during stop")
	}
	if err := <-stopDone; err != nil {
		t.Fatalf("StopWithContext: %v", err)
	}
}
