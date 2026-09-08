package bus

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/errcode"
)

// The RPC response envelope is a wire contract between processes that may run
// different versions. Every shape the decoder refuses must stay refused: a
// future version, a failure without an error body, a success without a
// payload. Only "decode failed" was exercised before.
func TestDecodeRPCResponseRefusesEveryMalformedEnvelope(t *testing.T) {
	codec := JSONCodec{}
	encode := func(t *testing.T, envelope rpcResponseEnvelope) []byte {
		t.Helper()
		raw, err := codec.Marshal(envelope)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	var target struct {
		Name string `json:"name"`
	}
	cases := []struct {
		name     string
		envelope rpcResponseEnvelope
		want     string
	}{
		{"future version", rpcResponseEnvelope{Version: rpcWireVersion + 1, OK: true, Payload: []byte(`{"name":"x"}`)}, "unsupported rpc response version"},
		{"failure without error body", rpcResponseEnvelope{Version: rpcWireVersion, OK: false}, "malformed rpc failure response"},
		{"success without payload", rpcResponseEnvelope{Version: rpcWireVersion, OK: true}, "successful rpc response has no payload"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := encode(t, tc.envelope)
			if err := decodeRPCResponse(codec, raw, &target); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("decodeRPCResponse = %v, want %q", err, tc.want)
			}
			if tc.name == "success without payload" {
				return // the bytes variant hands the (empty) payload to the caller
			}
			if _, err := decodeRPCResponseBytes(codec, raw); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("decodeRPCResponseBytes = %v, want %q", err, tc.want)
			}
		})
	}

	// A well-formed failure surfaces as a remote errcode, not as a decode error.
	raw, err := encodeRPCFailure(codec, errcode.Remote(4242, "nope", "remote failed"))
	if err != nil {
		t.Fatal(err)
	}
	decodeErr := decodeRPCResponse(codec, raw, &target)
	if decodeErr == nil || errcode.CodeOf(decodeErr) != 4242 {
		t.Fatalf("failure envelope decoded to %v, want errcode 4242", decodeErr)
	}
	// A success with a nil target does not need a payload.
	if err := decodeRPCResponse(codec, encode(t, rpcResponseEnvelope{Version: rpcWireVersion, OK: true}), nil); err != nil {
		t.Fatalf("nil target must not require a payload: %v", err)
	}
}

// Stop is final. Start on a stopped bus must refuse, not silently build a new
// worker pool on top of released subscriptions.
func TestBusRefusesToRestartAfterStop(t *testing.T) {
	b := New(&lifecycleClient{}, nil, JSONCodec{}, Config{Sid: 1, SvcType: "game"})
	if err := b.Start(); err != nil {
		t.Fatal(err)
	}
	if err := b.StopWithContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := b.Start(); err == nil || !strings.Contains(err.Error(), "cannot restart a stopped bus") {
		t.Fatalf("Start after Stop = %v", err)
	}
	if b.pool != nil {
		t.Fatal("a refused restart must not leave a worker pool behind")
	}
}

// Registration on a stopped bus would subscribe on a client that is being
// drained; both registration paths refuse.
func TestBusRefusesHandlerRegistrationAfterStop(t *testing.T) {
	b := New(&lifecycleClient{}, nil, JSONCodec{}, Config{Sid: 1, SvcType: "game"})
	if err := b.Start(); err != nil {
		t.Fatal(err)
	}
	if err := b.StopWithContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := b.HandleRpc("Ping", func(*RpcContext) (any, error) { return nil, nil }); err == nil || !strings.Contains(err.Error(), "stopping or stopped") {
		t.Fatalf("HandleRpc after Stop = %v", err)
	}
	if err := b.Handle("", "Ping", func(*MsgContext) {}); err == nil || !strings.Contains(err.Error(), "module and name are required") {
		t.Fatalf("Handle with empty module = %v", err)
	}
	if err := b.Handle("game", "", func(*MsgContext) {}); err == nil || !strings.Contains(err.Error(), "module and name are required") {
		t.Fatalf("Handle with empty name = %v", err)
	}
}

// Calls on a bus without the corresponding transport must fail loudly with
// the reason, not panic or hang.
func TestBusCallsWithoutTransportNameTheMissingPiece(t *testing.T) {
	b := New(&lifecycleClient{}, nil, JSONCodec{}, Config{Sid: 1, SvcType: "game"})
	var resp struct{}
	if err := b.callSubject(context.Background(), "roost.rpc.game.Ping", "Ping", 0, struct{}{}, &resp); err == nil || !strings.Contains(err.Error(), "rpc client is nil") {
		t.Fatalf("callSubject without rpc = %v", err)
	}
	if err := b.callReliableSubject(context.Background(), "roost.rpc.game.Ping", "Ping", 0, struct{}{}, &resp); err == nil || !strings.Contains(err.Error(), "jetstream rpc is not enabled") {
		t.Fatalf("callReliableSubject without jetstream = %v", err)
	}
	if _, err := b.callJetStreamRPC(context.Background(), "s", "Ping", 0, nil); !errors.Is(err, ErrJetStreamRPCUnavailable) {
		t.Fatalf("callJetStreamRPC without jetstream = %v", err)
	}
	if err := b.EnableJetStreamRPC(nil, JetStreamRPCConfig{}); !errors.Is(err, ErrJetStreamRPCUnavailable) {
		t.Fatalf("EnableJetStreamRPC(nil) = %v", err)
	}
	if err := b.subscribeJetStreamRPCMethod("Ping"); !errors.Is(err, ErrJetStreamRPCUnavailable) {
		t.Fatalf("subscribeJetStreamRPCMethod without jetstream = %v", err)
	}
}

// Dead-letter operations need a reliable store that implements them; without
// one they must say so rather than report "0 requeued" as if they had run.
func TestBusDeadLetterOperationsRefuseWithoutACapableStore(t *testing.T) {
	b := New(&lifecycleClient{}, nil, JSONCodec{}, Config{Sid: 1, SvcType: "game"})
	ctx := context.Background()
	if _, err := b.DeadLetters(ctx, DeadLetterQuery{Module: "game"}); err == nil || !strings.Contains(err.Error(), "does not support dead letter queries") {
		t.Fatalf("DeadLetters = %v", err)
	}
	if n, err := b.RequeueDeadLetters(ctx, DeadLetterQuery{Module: "game"}); err == nil || n != 0 || !strings.Contains(err.Error(), "does not support dead letter requeue") {
		t.Fatalf("RequeueDeadLetters = %d, %v", n, err)
	}
	if n, err := b.PurgeDeadLetters(ctx, DeadLetterQuery{Module: "game"}); err == nil || n != 0 || !strings.Contains(err.Error(), "does not support dead letter purge") {
		t.Fatalf("PurgeDeadLetters = %d, %v", n, err)
	}
}

// U-0107（B-14）：重投递 ID 是死信条目的摘要。它必须对同一条目稳定（发布成功
// 但删除失败后，运维重试不能绕过收件箱去重），对不同条目不同，并且在条目
// 无法序列化时报错而不是把所有条目都发到 sha256(nil) 这一个 ID 下。当前的
// DeadLetterEntry 全是纯值字段、不会序列化失败，所以最后一条只能作为护栏。
func TestDeadLetterRequeueIDIsStableAndDistinct(t *testing.T) {
	entry := DeadLetterEntry{MsgID: "m-1", FromSid: 1, ToSid: 2, ToModule: "mail", MsgName: "Changed", Attempt: 3, Reason: "handler panic", Payload: []byte("p"), CreatedAt: 10, FailedAt: 20}
	first, err := entry.requeueMsgID()
	if err != nil || !strings.HasPrefix(first, "requeue:") {
		t.Fatalf("requeueMsgID = (%q, %v)", first, err)
	}
	again, _ := entry.requeueMsgID()
	if again != first {
		t.Fatalf("requeue id changed between calls: %q vs %q", first, again)
	}
	other := entry
	other.Payload = []byte("q")
	otherID, _ := other.requeueMsgID()
	if otherID == first {
		t.Fatal("entries with different payloads shared one requeue id")
	}
}
