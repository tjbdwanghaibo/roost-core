package driver

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	gonats "github.com/nats-io/nats.go"
	gojs "github.com/nats-io/nats.go/jetstream"
	fnats "github.com/tjbdwanghaibo/roost-core/nats"
)

// U-0135 · C2（空洞测试）· nightly gap map core `nats/driver` 9/18。
//
// 请求在上下文已到期 / 已取消时要翻译成 ErrTimeout / ErrCancelled（gonats 对已结束
// 的上下文先返回 ctx.Err()，不碰连接，所以零值连接就能触发）；队列订阅缺队列名
// 在触达连接前拒绝；RPC 对不可重试的错误立刻返回、不做 MaxAttempts 次退避；
// JetStream 客户端对 nil 客户端 / 未初始化 / nil 处理器各自报错而不是解引用。

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
	if _, err := c.requestWithContext(cancelled, "roost.x", nil); !errors.Is(err, fnats.ErrCancelled) {
		t.Fatalf("request with a cancelled context = %v, want ErrCancelled", err)
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
	// 对照：可重试的超时确实会进入下一轮（下一轮看到父上下文已结束 → ErrCancelled）。
	expired, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	<-expired.Done()
	if _, err := r.Call(expired, "roost.x", nil); !errors.Is(err, fnats.ErrCancelled) {
		t.Fatalf("Call whose retryable attempt ran out of context = %v, want ErrCancelled", err)
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
