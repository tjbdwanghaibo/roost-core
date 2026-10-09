package bus

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/internal/stopcontract"
	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
)

// A3 / RR-20261005-NC-90：Bus.StopWithContext 的 JetStream RPC 部分套共用停机契约骨架。卡住的工作是 consume
// 回调里运行的请求 handler；“资源”是响应消费者——在途 handler 可能还在等嵌套可靠调用的回包，排空之后
// 才停（stopJetStreamRPCResponses），随后调用方才交还连接。

// responseStopRecordingJetStream 记录响应消费者（rpc_resp 过滤主题）是否被停掉。
type responseStopRecordingJetStream struct {
	*captureJetStreamRPC
	responseStopped atomic.Bool
}

type recordingJetStreamSub struct {
	captureJetStreamSub
	onStop func()
}

func (s recordingJetStreamSub) Stop() { s.onStop() }

func (r *responseStopRecordingJetStream) Subscribe(ctx context.Context, cfg fnats.JetStreamConsumerConfig, handler fnats.JetStreamHandler) (fnats.IJetStreamSubscription, error) {
	if _, err := r.captureJetStreamRPC.Subscribe(ctx, cfg, handler); err != nil {
		return nil, err
	}
	onStop := func() {}
	if strings.Contains(cfg.FilterSubject, ".rpc_resp.") {
		onStop = func() { r.responseStopped.Store(true) }
	}
	return recordingJetStreamSub{onStop: onStop}, nil
}

func TestJetStreamRPCStopContract(t *testing.T) {
	js := &responseStopRecordingJetStream{captureJetStreamRPC: &captureJetStreamRPC{respectPublishContext: true}}
	var b *Bus
	entered, release := make(chan struct{}), make(chan struct{})
	settled := make(chan error, 1)
	stopcontract.Check(t, stopcontract.Hooks{
		Start: func(testing.TB) {
			b = New(&lifecycleClient{}, &lifecycleRpc{}, nil, Config{Sid: 8001, SvcType: "mail", WorkerNum: 1})
			if err := b.Start(); err != nil {
				t.Fatal(err)
			}
			if err := b.EnableJetStreamRPC(js, JetStreamRPCConfig{RequestTTL: 5 * time.Second}); err != nil {
				t.Fatal(err)
			}
			if err := b.HandleRpc("mail.Slow", func(*RpcContext) (any, error) {
				close(entered)
				<-release // 不看 ctx 的业务，例如阻塞的存储写
				return map[string]int{"code": 0}, nil
			}); err != nil {
				t.Fatal(err)
			}
		},
		Block: func(testing.TB) {
			handler := js.handlerForFilter("roost.rpc.mail.mail.Slow")
			go func() {
				settled <- handler(context.Background(), jetStreamStopTestRequest(t, b, "mail.Slow", "req-contract"))
			}()
			select {
			case <-entered:
			case <-time.After(3 * time.Second):
				t.Fatal("handler never entered")
			}
		},
		Stop:     func(ctx context.Context) error { return b.StopWithContext(ctx) },
		Release:  func() { close(release) },
		Released: js.responseStopped.Load,
	})
	if err := <-settled; err != nil {
		t.Errorf("handler that finished within the stop returned %v: the delivery is NAKed and redelivered", err)
	}
	if !js.publishedTo("roost.rpc_resp.2001.req-contract") {
		t.Error("the response of a handler that finished within the stop was never published")
	}
}
