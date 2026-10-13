package bus

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/infra/base/errcode"
	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/admin"
)

// The RPC response envelope is a wire contract between processes that may run
// different versions. Every shape the decoder refuses must stay refused: a
// future version, a failure without an error body, a success without a
// payload. Only "decode failed" was exercised before.
func TestDecodeRPCResponseRefusesEveryMalformedEnvelope(t *testing.T) {
	codec := MessagePackCodec{}
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

// Registration on a stopped bus would subscribe on a client that is being
// drained; both registration paths refuse.
func TestBusRefusesHandlerRegistrationAfterStop(t *testing.T) {
	b := New(&lifecycleClient{}, nil, MessagePackCodec{}, Config{Sid: 1, SvcType: "game"})
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
	b := New(&lifecycleClient{}, nil, MessagePackCodec{}, Config{Sid: 1, SvcType: "game"})
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
	b := New(&lifecycleClient{}, nil, MessagePackCodec{}, Config{Sid: 1, SvcType: "game"})
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

// 重投递 ID 是死信条目的摘要：对同一条目稳定（发布成功但删除失败后，运维重试不能绕过收件箱去重），对不同条目不同，
// 条目无法序列化时报错而不是全部落到 sha256(nil) 一个 ID 下。当前 DeadLetterEntry 全是纯值字段、不会序列化失败，
// 最后一条只作为护栏。
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

// 死信重投：没有 NATS 客户端时报“客户端为空”，而不是在发布处 nil 解引用；存储只会整桶清除（无逐条删除能力）时，
// 带 Limit / Stop 的部分重投在发布之后、清桶之前停下并报“不支持部分删除”——否则未被选中的死信会随整桶一起被清掉。
// 管理命令注册对缺注册表 / 缺总线各报其错且不留下半注册。nil 总线的 Handle / HandleRpc / EnableJetStreamRPC 报错不 panic。

// purgeOnlyStore can list and purge dead letters but cannot delete a chosen
// subset — the shape of a store whose DLQ is a plain list.
type purgeOnlyStore struct {
	ReliableStore
	inner *reliableMemoryStore
}

func (s purgeOnlyStore) ListDeadLetters(ctx context.Context, query DeadLetterQuery) ([]DeadLetterEntry, error) {
	return s.inner.ListDeadLetters(ctx, query)
}

func (s purgeOnlyStore) PurgeDeadLetters(ctx context.Context, query DeadLetterQuery) (int64, error) {
	return s.inner.PurgeDeadLetters(ctx, query)
}

func deadLetterTwice(b *Bus) {
	for _, id := range []string{"dead-1", "dead-2"} {
		b.deadLetter(&fnats.NatsMsg{ToSid: 2, ToModule: "mail", MsgName: "Changed", MsgID: id, Payload: []byte(`{"x":1}`)}, "handler failed")
	}
}

func TestRequeueDeadLettersRefusesWithoutAClientAndPartialRequeueOnAPurgeOnlyStore(t *testing.T) {
	ctx := context.Background()
	store := newReliableMemoryStore()
	clientless := New(nil, nil, MessagePackCodec{}, Config{Sid: 1, SvcType: "game", Prefix: "roost"})
	_ = clientless.Handle("mail", "Changed", func(*MsgContext) {}) // 死信桶只按注册过的名字建（RR-20261006-72）
	clientless.EnableReliable(store, ReliableConfig{Enabled: true})
	deadLetterTwice(clientless)
	if n, err := clientless.RequeueDeadLetters(ctx, DeadLetterQuery{Module: "mail", MsgName: "Changed"}); err == nil || n != 0 || !strings.Contains(err.Error(), "nats client is nil") {
		t.Fatalf("RequeueDeadLetters without a client = (%d, %v)", n, err)
	}
	if store.deadLetterCount() != 2 {
		t.Fatalf("a refused requeue touched the dead letters: %d left", store.deadLetterCount())
	}

	purgeOnly := newReliableMemoryStore()
	client := &captureNatsClient{}
	b := New(client, nil, MessagePackCodec{}, Config{Sid: 1, SvcType: "game", Prefix: "roost"})
	_ = b.Handle("mail", "Changed", func(*MsgContext) {}) // 死信桶只按注册过的名字建（RR-20261006-72）
	b.EnableReliable(purgeOnlyStore{ReliableStore: purgeOnly, inner: purgeOnly}, ReliableConfig{Enabled: true})
	deadLetterTwice(b)
	n, err := b.RequeueDeadLetters(ctx, DeadLetterQuery{Module: "mail", MsgName: "Changed", Limit: 1})
	if err == nil || !strings.Contains(err.Error(), "does not support partial dead letter deletion") {
		t.Fatalf("partial requeue on a purge-only store = (%d, %v)", n, err)
	}
	if n != 1 || len(client.published) != 1 {
		t.Fatalf("partial requeue published %d (n=%d), want exactly the selected entry", len(client.published), n)
	}
	if purgeOnly.deadLetterCount() != 2 {
		t.Fatalf("partial requeue purged the bucket: %d left, want 2", purgeOnly.deadLetterCount())
	}
	// 对照：整桶重投在只会清桶的存储上成立。
	n, err = b.RequeueDeadLetters(ctx, DeadLetterQuery{Module: "mail", MsgName: "Changed"})
	if err != nil || n != 2 || len(client.published) != 3 || purgeOnly.deadLetterCount() != 0 {
		t.Fatalf("whole-bucket requeue = (%d, %v), published=%d left=%d", n, err, len(client.published), purgeOnly.deadLetterCount())
	}
}

func TestAdminRegistrationAndNilBusEntryPointsRefuse(t *testing.T) {
	b := New(&lifecycleClient{}, nil, MessagePackCodec{}, Config{Sid: 1, SvcType: "game"})
	reg := admin.NewRegistry()
	if err := RegisterAdminCommands(nil, b); err == nil || !strings.Contains(err.Error(), "admin registry nil") {
		t.Fatalf("RegisterAdminCommands(nil registry) = %v", err)
	}
	if err := RegisterAdminCommands(reg, nil); err == nil || !strings.Contains(err.Error(), "nil bus") {
		t.Fatalf("RegisterAdminCommands(nil bus) = %v", err)
	}
	if names := reg.Names(); len(names) != 0 {
		t.Fatalf("a refused registration left commands behind: %v", names)
	}
	if err := RegisterAdminCommands(reg, b); err != nil || len(reg.Names()) == 0 {
		t.Fatalf("RegisterAdminCommands = %v, names=%v", err, reg.Names())
	}

	var none *Bus
	if err := none.Handle("mail", "Changed", func(*MsgContext) {}); err == nil || !strings.Contains(err.Error(), "nil bus") {
		t.Fatalf("Handle on a nil bus = %v", err)
	}
	if err := none.HandleRpc("Ping", func(*RpcContext) (any, error) { return nil, nil }); err == nil || !strings.Contains(err.Error(), "nil bus") {
		t.Fatalf("HandleRpc on a nil bus = %v", err)
	}
	// 传一个真的 JetStream 替身：nil 总线的拒绝不能靠"js 也是 nil"顺带成立。
	if err := none.EnableJetStreamRPC(&captureJetStreamRPC{}, JetStreamRPCConfig{}); !errors.Is(err, ErrJetStreamRPCUnavailable) {
		t.Fatalf("EnableJetStreamRPC on a nil bus = %v", err)
	}
}
