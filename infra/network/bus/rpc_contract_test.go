package bus

import (
	"context"
	"errors"
	"fmt"
	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
	"strings"
	"testing"
	"time"
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

func TestRPCBudgetJetStreamResponsesUseTheClientEnvelope(t *testing.T) {
	for _, mode := range []string{"missing", "success", "business_error", "panic"} {
		t.Run(mode, func(t *testing.T) {
			js := &captureJetStreamRPC{}
			b := New(nil, nil, nil, Config{Sid: 6, SvcType: "game"})
			if err := b.EnableJetStreamRPC(js, JetStreamRPCConfig{}); err != nil {
				t.Fatal(err)
			}
			defer b.Stop()
			if mode != "missing" {
				b.rpcHandlers["queue.join"] = func(*RpcContext) (any, error) {
					switch mode {
					case "business_error":
						return nil, errors.New("business failure")
					case "panic":
						panic("handler failed")
					default:
						return map[string]int{"value": 42}, nil
					}
				}
			}
			req := &fnats.NatsMsg{MsgName: "queue.join", SessionId: "id", ReplySubject: "roost.rpc_resp.8.id", DeadlineAt: time.Now().Add(time.Minute).UnixMilli()}
			raw, err := b.codec.Marshal(req)
			if err != nil {
				t.Fatal(err)
			}
			if err := b.onJetStreamRPCRequest(context.Background(), &fnats.JetStreamMsg{Subject: b.subject.Rpc("game", "queue.join"), Data: raw}); err != nil {
				t.Fatal(err)
			}
			published := js.lastPublish()
			var envelope rpcResponseEnvelope
			if err := b.codec.Unmarshal(published.data, &envelope); err != nil {
				t.Fatal(err)
			}
			var response map[string]int
			decodeErr := decodeRPCResponse(b.codec, published.data, &response)
			t.Logf("wire=%s decode_error=%v", published.data, decodeErr)
			if envelope.Version != rpcWireVersion {
				t.Fatalf("client cannot decode terminal response: version=%d", envelope.Version)
			}
			if mode == "success" {
				if decodeErr != nil || response["value"] != 42 {
					t.Fatalf("response=%v err=%v", response, decodeErr)
				}
			} else if decodeErr == nil || strings.Contains(decodeErr.Error(), "unsupported rpc response") {
				t.Fatalf("expected remote refusal, got %v", decodeErr)
			}
		})
	}
}

func TestRPCBudgetJetStreamExpiredAndMalformedDoNotCallBusiness(t *testing.T) {
	for _, mode := range []string{"expired", "malformed", "nil"} {
		t.Run(mode, func(t *testing.T) {
			js := &captureJetStreamRPC{}
			b := New(nil, nil, nil, Config{})
			if err := b.EnableJetStreamRPC(js, JetStreamRPCConfig{}); err != nil {
				t.Fatal(err)
			}
			defer b.Stop()
			calls := 0
			b.rpcHandlers["join"] = func(*RpcContext) (any, error) { calls++; return nil, nil }
			var msg *fnats.JetStreamMsg
			if mode != "nil" {
				msg = &fnats.JetStreamMsg{Data: []byte("bad json")}
				if mode == "expired" {
					msg.Data, _ = b.codec.Marshal(fnats.NatsMsg{MsgName: "join", DeadlineAt: time.Now().Add(-time.Hour).UnixMilli()})
				}
			}
			err := b.onJetStreamRPCRequest(context.Background(), msg)
			if mode == "malformed" && err == nil {
				t.Fatal("malformed request accepted")
			}
			if calls != 0 || len(js.publishes) != 0 {
				t.Fatalf("calls=%d publishes=%d", calls, len(js.publishes))
			}
		})
	}
}

func TestRPCBudgetReliableCallerReceivesMissingHandlerRefusal(t *testing.T) {
	js := &captureJetStreamRPC{}
	b := New(nil, nil, nil, Config{Sid: 6, SvcType: "game"})
	if err := b.EnableJetStreamRPC(js, JetStreamRPCConfig{}); err != nil {
		t.Fatal(err)
	}
	defer b.Stop()
	js.publishHook = func(subject string, data []byte, _ fnats.JetStreamPublishOptions) {
		msg := &fnats.JetStreamMsg{Subject: subject, Data: data}
		var err error
		if strings.HasPrefix(subject, "roost.rpc_resp.") {
			err = b.onJetStreamRPCResponse(context.Background(), msg)
		} else {
			err = b.onJetStreamRPCRequest(context.Background(), msg)
		}
		if err != nil {
			t.Error(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var resp map[string]int
	err := b.CallReliable(ctx, "game", "missing", nil, &resp)
	if err == nil || strings.Contains(err.Error(), "unsupported rpc response") || errors.Is(err, fnats.ErrTimeout) {
		t.Fatalf("formal CallReliable lost remote refusal: %v", err)
	}
}

type failingEnvelopeCodec struct {
	Codec
	cause error
}

func (c failingEnvelopeCodec) Marshal(value any) ([]byte, error) {
	if _, ok := value.(rpcResponseEnvelope); ok {
		return nil, c.cause
	}
	return c.Codec.Marshal(value)
}

func TestRPCBudgetMissingHandlerRetainsPublishAndMarshalFailures(t *testing.T) {
	for _, stage := range []string{"publish", "marshal"} {
		t.Run(stage, func(t *testing.T) {
			cause := errors.New(stage + " refused")
			js := &captureJetStreamRPC{}
			b := New(nil, nil, nil, Config{})
			if err := b.EnableJetStreamRPC(js, JetStreamRPCConfig{}); err != nil {
				t.Fatal(err)
			}
			defer b.Stop()
			raw, _ := b.codec.Marshal(fnats.NatsMsg{MsgName: "missing", ReplySubject: "roost.rpc_resp.6.id", SessionId: "id"})
			if stage == "publish" {
				js.publishErr = cause
			} else {
				b.codec = failingEnvelopeCodec{Codec: b.codec, cause: cause}
			}
			if err := b.onJetStreamRPCRequest(context.Background(), &fnats.JetStreamMsg{Data: raw}); !errors.Is(err, cause) {
				t.Fatalf("cause lost: %v", err)
			}
		})
	}
}

func methodLabels(name string) map[string]bool {
	out := map[string]bool{}
	for _, metric := range metrics.Snapshot() {
		if metric.Name == name {
			out[metric.Labels["method"]] = true
		}
	}
	return out
}

func TestCallerRPCMethodLabelsAreBounded(t *testing.T) {
	metrics.DefaultRegistry().Reset()
	t.Cleanup(func() { metrics.DefaultRegistry().Reset() })
	js := &captureJetStreamRPC{}
	b := New(nil, nil, nil, Config{Sid: 2001, SvcType: "game"})
	if err := b.EnableJetStreamRPC(js, JetStreamRPCConfig{CallTimeout: time.Second, RequestTTL: time.Second}); err != nil {
		t.Fatalf("EnableJetStreamRPC: %v", err)
	}
	js.publishHook = func(_ string, data []byte, _ fnats.JetStreamPublishOptions) {
		var req fnats.NatsMsg
		if err := b.codec.Unmarshal(data, &req); err != nil {
			t.Errorf("unmarshal request: %v", err)
			return
		}
		if handler := js.handlerForFilter("roost.rpc_resp.2001.>"); handler != nil {
			_ = handler(context.Background(), &fnats.JetStreamMsg{Subject: req.ReplySubject, Data: rpcTestSuccessBytes(b, map[string]int{"code": 0})})
		}
	}

	calls := jetStreamRPCMethodLabelLimit + 44
	for i := range calls {
		var resp struct{}
		if err := b.CallReliable(context.Background(), "svc", fmt.Sprintf("svc.M%03d", i), nil, &resp); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	for _, name := range []string{"bus_rpc_pending", "bus_rpc_call_total"} {
		if got := len(methodLabels(name)); got > jetStreamRPCMethodLabelLimit+1 {
			t.Fatalf("%s has %d method labels after %d distinct methods; the bound is %d", name, got, calls, jetStreamRPCMethodLabelLimit+1)
		}
	}
	tracked := 0
	b.jsRPC.pendingByMethod.Range(func(any, any) bool { tracked++; return true })
	if tracked > jetStreamRPCMethodLabelLimit+1 {
		t.Fatalf("the bus tracks %d pending counters after %d distinct methods; the bound is %d", tracked, calls, jetStreamRPCMethodLabelLimit+1)
	}
	if got := busMetricValue("bus_rpc_call_total", map[string]string{"method": "svc.M000", "result": "ok"}); got != 1 {
		t.Fatalf("a method within the bound lost its own label: ok calls = %d, want 1", got)
	}
	if got := busMetricValue("bus_rpc_call_total", map[string]string{"method": jetStreamRPCOtherMethod, "result": "ok"}); got != int64(calls-jetStreamRPCMethodLabelLimit) {
		t.Fatalf("calls past the bound counted under %q = %d, want %d", jetStreamRPCOtherMethod, got, calls-jetStreamRPCMethodLabelLimit)
	}
}

func TestServedRPCMethodLabelsAreTheRegisteredMethods(t *testing.T) {
	metrics.DefaultRegistry().Reset()
	t.Cleanup(func() { metrics.DefaultRegistry().Reset() })
	js := &captureJetStreamRPC{}
	b := New(nil, nil, nil, Config{Sid: 5001, SvcType: "mail"})
	if err := b.EnableJetStreamRPC(js, JetStreamRPCConfig{RequestTTL: time.Second}); err != nil {
		t.Fatalf("EnableJetStreamRPC: %v", err)
	}
	if err := b.HandleRpc("mail.List", func(*RpcContext) (any, error) { return map[string]int{"code": 0}, nil }); err != nil {
		t.Fatalf("HandleRpc: %v", err)
	}
	handler := js.handlerForFilter("roost.rpc.mail.mail.List")
	if handler == nil {
		t.Fatalf("service rpc subscription was not registered: %+v", js.consumers)
	}
	deliver := func(method, id string) {
		t.Helper()
		data, err := b.codec.Marshal(&fnats.NatsMsg{
			FromSid: 2001, MsgName: method, SessionId: id, MsgID: id,
			ReplySubject: "roost.rpc_resp.2001." + id, DeadlineAt: time.Now().Add(time.Second).UnixMilli(),
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := handler(context.Background(), &fnats.JetStreamMsg{Subject: "roost.rpc.mail.mail.List", Data: data, NumDelivered: 1}); err != nil {
			t.Fatalf("handler(%s): %v", method, err)
		}
	}
	deliver("mail.List", "req-ok")
	for i := range 300 {
		deliver(fmt.Sprintf("forged.M%03d", i), fmt.Sprintf("req-%d", i))
	}
	want := map[string]bool{"mail.List": true, jetStreamRPCUnregisteredMethod: true}
	for _, name := range []string{"bus_rpc_request_total", "bus_rpc_consumer_delivery"} {
		labels := methodLabels(name)
		for label := range labels {
			if !want[label] {
				t.Fatalf("%s has %d method labels (e.g. %q) after 300 requests naming unregistered methods; want only %v", name, len(labels), label, want)
			}
		}
	}
	if got := busMetricValue("bus_rpc_request_total", map[string]string{"method": jetStreamRPCUnregisteredMethod, "result": "no_handler"}); got != 300 {
		t.Fatalf("requests for unregistered methods counted %d, want 300", got)
	}
	if got := busMetricValue("bus_rpc_request_total", map[string]string{"method": "mail.List", "result": "ok"}); got != 1 {
		t.Fatalf("the registered method's requests counted %d, want 1", got)
	}
}
