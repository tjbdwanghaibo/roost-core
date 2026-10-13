package driver

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	gonats "github.com/nats-io/nats.go"
	gojs "github.com/nats-io/nats.go/jetstream"
	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
)

func TestNatsClientNilBoundaryFailsClosed(t *testing.T) {
	var client *Client
	if err := client.Publish("topic", nil); !errors.Is(err, fnats.ErrClosed) {
		t.Fatalf("Publish error=%v, want fnats.ErrClosed", err)
	}
	client.Close()
}

// FEATURE_LOGIC M8 item 2: a handler panic is contained AND reported. The
// previous version of this test set a flag after the call and asserted the
// flag — an assertion that cannot fail once the line is reached, so it proved
// containment only by not crashing and proved reporting not at all. Reverting
// the recover branch to swallow the panic silently left it green. The counter
// is the observable.
func TestInvokeNatsHandlerContainsAndReportsPanic(t *testing.T) {
	before := panicCounter()
	invokeNatsHandler(func(*fnats.Msg) { panic("boom") }, &fnats.Msg{Subject: "test"})
	if got := panicCounter() - before; got != 1 {
		t.Fatalf("handler_panic counter moved by %d, want 1: the panic was swallowed without being reported", got)
	}
}

func panicCounter() int64 {
	for _, metric := range metrics.Snapshot() {
		if metric.Name == "nats.subscription.handler_panic.total" && len(metric.Labels) == 0 {
			return metric.Value
		}
	}
	return 0
}

// M8 item 1: a subscription with no handler is refused before anything is
// registered with the server. Reverting the check made no test red before
// this one existed — the nil-boundary test above covers a nil client, not a
// nil callback.
func TestSubscriptionValidationRejectsANilHandler(t *testing.T) {
	client := &Client{conn: &gonats.Conn{}}
	if err := client.validateSubscription("subject", "", nil); err == nil || !strings.Contains(err.Error(), "handler") {
		t.Fatalf("nil handler accepted: %v", err)
	}
	if err := client.validateSubscription("", "", func(*fnats.Msg) {}); err == nil {
		t.Fatal("empty subject accepted")
	}
	if err := client.validateSubscription("subject", "queue", func(*fnats.Msg) {}); err != nil {
		t.Fatalf("a valid subscription was refused: %v", err)
	}
}

// Every gonats failure that callers branch on is translated to a roost-core
// sentinel; a translation that stops matching would make callers fall into
// their generic-error path (retry a timeout that was a closed connection, or
// give up on a "no responders" that a retry would have served).
func TestClientTranslatesEachTransportErrorToItsSentinel(t *testing.T) {
	c := &Client{}
	cases := map[error]error{
		gonats.ErrTimeout:            fnats.ErrTimeout,
		gonats.ErrNoResponders:       fnats.ErrNoResponders,
		gonats.ErrConnectionClosed:   fnats.ErrClosed,
		gonats.ErrConnectionDraining: fnats.ErrClosed,
	}
	for in, want := range cases {
		if got := c.wrapError(in); !errors.Is(got, want) {
			t.Fatalf("wrapError(%v) = %v, want %v", in, got, want)
		}
	}
	other := errors.New("something else")
	if got := c.wrapError(other); !errors.Is(got, other) {
		t.Fatalf("unknown errors must pass through: %v", got)
	}
}

// Subjects and queues are validated before touching the connection: a padded
// or empty subject would silently subscribe to nothing, and a client with no
// connection reports fnats.ErrClosed rather than panicking.
func TestClientRefusesInvalidSubjectsQueuesAndHandlers(t *testing.T) {
	var closed *Client
	if err := closed.validateSubject("roost.x"); !errors.Is(err, fnats.ErrClosed) {
		t.Fatalf("nil client = %v", err)
	}
	if err := (&Client{}).validateSubject("roost.x"); !errors.Is(err, fnats.ErrClosed) {
		t.Fatalf("client without connection = %v", err)
	}
	c := &Client{conn: &gonats.Conn{}}
	for _, subject := range []string{"", "  ", " roost.x", "roost.x "} {
		if err := c.validateSubject(subject); err == nil || !strings.Contains(err.Error(), "invalid subject") {
			t.Fatalf("subject %q = %v", subject, err)
		}
	}
	handler := func(*fnats.Msg) {}
	if err := c.validateSubscription("roost.x", "", nil); err == nil || !strings.Contains(err.Error(), "handler is nil") {
		t.Fatalf("nil handler = %v", err)
	}
	for _, queue := range []string{" ", " workers", "workers "} {
		if err := c.validateSubscription("roost.x", queue, handler); err == nil || !strings.Contains(err.Error(), "invalid queue") {
			t.Fatalf("queue %q = %v", queue, err)
		}
	}
	if err := c.validateSubscription("roost.x", "workers", handler); err != nil {
		t.Fatalf("valid subscription refused: %v", err)
	}
	if _, err := c.Request("roost.x", nil, 0); err == nil || !strings.Contains(err.Error(), "request timeout must be positive") {
		t.Fatalf("non-positive request timeout = %v", err)
	}
}

// 请求在上下文已到期 / 已取消时翻译成 ErrTimeout / ErrCancelled（gonats 对已结束的上下文先返回 ctx.Err()，不碰连接，所以零值
// 连接就能触发）；队列订阅缺队列名在触达连接前拒绝；RPC 对不可重试的错误立刻返回、不做 MaxAttempts 次退避；JetStream 客户端
// 对 nil 客户端 / 未初始化 / nil 处理器各自报错而不是解引用。

func TestRequestTranslatesFinishedContextsAndQueueSubscribeRequiresAQueue(t *testing.T) {
	c := &Client{conn: &gonats.Conn{}}
	expired, cancelExpired := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancelExpired()
	<-expired.Done()
	if _, err := c.requestWithContext(expired, "roost.x", nil); !errors.Is(err, fnats.ErrTimeout) {
		t.Fatalf("request with an expired context = %v, want ErrTimeout", err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.requestWithContext(cancelled, "roost.x", nil); !errors.Is(err, fnats.ErrCancelled) || !errors.Is(err, context.Canceled) {
		t.Fatalf("request with a cancelled context = %v, want ErrCancelled wrapping context.Canceled", err)
	}
	handler := func(*fnats.Msg) {}
	if _, err := c.QueueSubscribe("roost.x", "", handler); err == nil || !strings.Contains(err.Error(), "queue is required") {
		t.Fatalf("QueueSubscribe without a queue = %v", err)
	}
	if _, err := c.QueueSubscribe("roost.x", " q ", handler); err == nil || !strings.Contains(err.Error(), "invalid queue") {
		t.Fatalf("QueueSubscribe with a padded queue = %v", err)
	}
}

func TestRPCCallDoesNotRetryANonRetryableError(t *testing.T) {
	policy := fnats.RetryPolicy{MaxAttempts: 4, BaseInterval: 200 * time.Millisecond, MaxInterval: 200 * time.Millisecond, Multiplier: 1}
	r := NewRPCClient(&Client{conn: &gonats.Conn{}}, policy, 1)
	defer r.Stop()
	started := time.Now()
	_, err := r.Call(context.Background(), " bad subject", nil)
	if err == nil || !strings.Contains(err.Error(), "invalid subject") || strings.Contains(err.Error(), "failed after") {
		t.Fatalf("Call with an invalid subject = %v, want the validation error itself, unretried", err)
	}
	if elapsed := time.Since(started); elapsed > policy.BaseInterval {
		t.Fatalf("a non-retryable error waited for a retry: %v", elapsed)
	}
	// 对照（RR-20261006-73 更正）：调用方期限已到时不再进入下一轮，直接回超时，并带 ctx 语义。之前这里
	// 期望下一轮看到父上下文结束后回 ErrCancelled——期限到达被报成“取消”，正是 F09-R2 的错误链问题。
	expired, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	<-expired.Done()
	started = time.Now()
	if _, err := r.Call(expired, "roost.x", nil); !errors.Is(err, fnats.ErrTimeout) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Call whose caller deadline already passed = %v, want ErrTimeout wrapping context.DeadlineExceeded", err)
	}
	if elapsed := time.Since(started); elapsed > policy.BaseInterval {
		t.Fatalf("an expired caller deadline still waited for a retry: %v", elapsed)
	}
}

// uninitialisedJetStream is a non-nil gojs.JetStream that must never be reached.
type uninitialisedJetStream struct{ gojs.JetStream }

func TestJetStreamClientRefusesMissingClientAndHandler(t *testing.T) {
	ctx := context.Background()
	if _, err := NewJetStreamClient(nil); err == nil || !strings.Contains(err.Error(), "client is nil") {
		t.Fatalf("NewJetStreamClient(nil) = %v", err)
	}
	if _, err := NewJetStreamClient(&Client{}); err == nil || !strings.Contains(err.Error(), "client is nil") {
		t.Fatalf("NewJetStreamClient(client without connection) = %v", err)
	}
	handler := func(context.Context, *fnats.JetStreamMsg) error { return nil }
	for name, c := range map[string]*JetStreamClient{"nil client": nil, "client without js": {}} {
		if err := c.EnsureStream(ctx, fnats.JetStreamConfig{Name: "s"}); err == nil || !strings.Contains(err.Error(), "not initialized") {
			t.Fatalf("%s: EnsureStream = %v", name, err)
		}
		if _, err := c.Publish(ctx, "roost.x", nil, fnats.JetStreamPublishOptions{}); err == nil || !strings.Contains(err.Error(), "not initialized") {
			t.Fatalf("%s: Publish = %v", name, err)
		}
		if _, err := c.Subscribe(ctx, fnats.JetStreamConsumerConfig{Stream: "s"}, handler); err == nil || !strings.Contains(err.Error(), "not initialized") {
			t.Fatalf("%s: Subscribe = %v", name, err)
		}
	}
	live := &JetStreamClient{js: uninitialisedJetStream{}}
	if _, err := live.Subscribe(ctx, fnats.JetStreamConsumerConfig{Stream: "s"}, nil); err == nil || !strings.Contains(err.Error(), "handler is nil") {
		t.Fatalf("Subscribe with a nil handler = %v", err)
	}
}

// P3b: Assemble needs a live server for anything beyond refusing a bad
// configuration; the kit Mod's integration test exercises the connected path.
func TestAssembleRefusesConfigurationWithoutURL(t *testing.T) {
	if _, err := Assemble(nil, ClientOptions{}); err == nil {
		t.Fatal("nil configuration was assembled")
	}
	if _, err := Assemble(&fnats.Config{}, ClientOptions{}); err == nil {
		t.Fatal("configuration without a URL was assembled")
	}
	var none *Assembly
	if none.Connected() {
		t.Fatal("nil assembly reports connected")
	}
	if err := none.Close(context.Background()); err != nil {
		t.Fatalf("nil assembly Close = %v", err)
	}
}
