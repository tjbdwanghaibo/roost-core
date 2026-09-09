package bus

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/admin"
	fnats "github.com/tjbdwanghaibo/roost-core/nats"
)

// U-0132 · C2（空洞测试）· nightly gap map core `bus` 10/20。
//
// 死信重投：没有 NATS 客户端时要说"客户端为空"，而不是在发布处 nil 解引用；存储
// 只会整桶清除（无逐条删除能力）时，带 Limit / Stop 的部分重投要在发布之后、清
// 桶之前停下并报"不支持部分删除"——否则未被选中的死信会随整桶一起被清掉。
// 管理命令注册对缺注册表 / 缺总线各报其错且不留下半注册。nil 总线的 Handle /
// HandleRpc / EnableJetStreamRPC 报错不 panic。`HandleRpc` 的 stopping 检查在入口
// （564）与订阅处（612 / JetStream 186）各一份、`EnableJetStreamRPC` 的 nil js 检查
// 与 `ensureJetStreamRPCStreams` 的启用检查同哨兵，均记冗余。

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
	clientless := New(nil, nil, JSONCodec{}, Config{Sid: 1, SvcType: "game", Prefix: "roost"})
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
	b := New(client, nil, JSONCodec{}, Config{Sid: 1, SvcType: "game", Prefix: "roost"})
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
	b := New(&lifecycleClient{}, nil, JSONCodec{}, Config{Sid: 1, SvcType: "game"})
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
