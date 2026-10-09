// RR-20261004-NC-08：JetStream 所有终态回包遵守同一版本 envelope。
package bus

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
)

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
