package bus

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
	"github.com/tjbdwanghaibo/roost-core/internal/stopcontract"
)

// Bus.StopWithContext 的 JetStream RPC 部分套共用停机契约骨架（维护者决定 A3）。卡住的工作是 consume 回调里运行的请求
// handler；“资源”是响应消费者——在途 handler 可能还在等嵌套可靠调用的回包，排空之后才停（stopJetStreamRPCResponses），
// 随后调用方才交还连接。

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

// Bus.StopWithContext 超出预算后只是调用方不再等待，停止本身仍由 Bus 持有：之后的调用继续等同一次 pool 排空，排空完成才
// 返回终态结果，不能把第一次的 ctx 错误缓存成终态。停止已经发起时，业务 handler 里的 Stop() 不能等待正在等它自己的那次排空。

type stopRetryResult struct {
	err             error
	handlerFinished bool
}

// startBlockedBus starts a Bus with one worker and parks a business handler in
// it. after runs once the handler has been released, still inside the handler.
func startBlockedBus(t *testing.T, after func(*Bus)) (b *Bus, release func(), finished *atomic.Bool) {
	t.Helper()
	b = New(&lifecycleClient{}, &lifecycleRpc{replies: make(chan []byte, 1)}, nil, Config{Sid: 7, SvcType: "game", WorkerNum: 1})
	entered, released := make(chan struct{}), make(chan struct{})
	finished = &atomic.Bool{}
	if err := b.Handle("mail", "Changed", func(*MsgContext) {
		close(entered)
		<-released
		if after != nil {
			after(b)
		}
		finished.Store(true)
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
	var once sync.Once
	release = func() { once.Do(func() { close(released) }) }
	t.Cleanup(release)
	return b, release, finished
}

func TestBusStopRetryWaitsForTheRetainedDrain(t *testing.T) {
	b, release, finished := startBlockedBus(t, nil)

	budget, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := b.StopWithContext(budget); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("budgeted stop = %v, want DeadlineExceeded", err)
	}
	again, cancelAgain := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancelAgain()
	if err := b.StopWithContext(again); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second budgeted stop while the handler still runs = %v, want DeadlineExceeded", err)
	}

	retried := make(chan stopRetryResult, 1)
	go func() {
		err := b.StopWithContext(context.Background())
		retried <- stopRetryResult{err: err, handlerFinished: finished.Load()}
	}()
	release()
	var got stopRetryResult
	select {
	case got = <-retried:
	case <-time.After(2 * time.Second):
		t.Fatal("retry stop did not return after the handler exited")
	}
	t.Logf("retry_err=%v handler_finished_before_return=%v", got.err, got.handlerFinished)
	if got.err != nil || !got.handlerFinished {
		t.Fatal("retry stop did not wait for the retained pool drain to finish")
	}
	if err := b.StopWithContext(context.Background()); err != nil {
		t.Fatalf("stop after the drain finished = %v", err)
	}
}

func TestBusStopInsideHandlerAfterBudgetedStopReturns(t *testing.T) {
	inner := make(chan struct{})
	b, release, _ := startBlockedBus(t, func(b *Bus) {
		b.Stop()
		close(inner)
	})
	budget, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := b.StopWithContext(budget); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("budgeted stop = %v, want DeadlineExceeded", err)
	}
	release()
	select {
	case <-inner:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop inside the handler waited for the drain that is waiting for this handler")
	}
	done, cancelDone := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelDone()
	if err := b.StopWithContext(done); err != nil {
		t.Fatalf("final stop = %v", err)
	}
}
