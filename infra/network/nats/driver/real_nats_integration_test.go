//go:build integration

package driver

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	gonats "github.com/nats-io/nats.go"
	gojs "github.com/nats-io/nats.go/jetstream"
	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
)

func rawTestClient(t *testing.T) *Client {
	t.Helper()
	url := os.Getenv("ROOST_DATAENGINE_IT_NATS_URL")
	if url == "" {
		t.Skip("private NATS URL not configured")
	}
	client, err := NewClient(fnats.DefaultConfig(url), ClientOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)
	return client
}

func rawTestFlush(t *testing.T, client *Client) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := client.FlushContext(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestRealRawRequestAndSubscriptionDrain(t *testing.T) {
	client := rawTestClient(t)
	subject := fmt.Sprintf("roost.raw.%d", time.Now().UnixNano())
	sub, err := client.SubscribeBounded(subject, fnats.PendingLimits{Messages: 8, Bytes: 4096}, func(msg *fnats.Msg) {
		if err := client.PublishOnce(msg.Reply, append([]byte("reply:"), msg.Data...)); err != nil {
			t.Error(err)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	rawTestFlush(t, client)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	response, err := client.RequestContext(ctx, subject, []byte("packet"))
	if err != nil || string(response) != "reply:packet" {
		t.Fatalf("response=%q err=%v", response, err)
	}
	maxPayload, err := client.MaxPayload()
	if err != nil || maxPayload <= 0 {
		t.Fatalf("max payload=%d err=%v", maxPayload, err)
	}
	if err := sub.DrainContext(ctx); err != nil {
		t.Fatal(err)
	}
	if sub.IsValid() {
		t.Fatal("successful drain left a valid subscription")
	}
	if err := client.PublishOnce("invalid subject", nil); err == nil {
		t.Fatal("invalid subject admitted")
	}
}

func TestRealRawSlowConsumerAndCloseWaitForActualCallback(t *testing.T) {
	client := rawTestClient(t)
	subject := fmt.Sprintf("roost.raw.slow.%d", time.Now().UnixNano())
	entered, release, returned := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var enterOnce, releaseOnce sync.Once
	releaseHandler := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseHandler)
	faults := make(chan error, 1)
	sub, err := client.SubscribeBounded(subject, fnats.PendingLimits{Messages: 1, Bytes: 32, OnError: func(err error) {
		select {
		case faults <- err:
		default:
		}
	}}, func(*fnats.Msg) {
		enterOnce.Do(func() { close(entered) })
		<-release
		select {
		case <-returned:
		default:
			close(returned)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	rawTestFlush(t, client)
	if err := client.PublishOnce(subject, []byte("first")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("callback not entered")
	}
	for range 16 {
		if err := client.PublishOnce(subject, []byte("queue overflow")); err != nil {
			t.Fatal(err)
		}
	}
	rawTestFlush(t, client)
	select {
	case err := <-faults:
		if !errors.Is(err, gonats.ErrSlowConsumer) {
			t.Fatalf("fault=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("slow consumer was not reported")
	}
	budget, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	if err := sub.DrainContext(budget); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("first drain=%v", err)
	}
	cancel()
	client.Close()
	budget, cancel = context.WithTimeout(context.Background(), 20*time.Millisecond)
	if err := sub.DrainContext(budget); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("close must not invent callback completion: %v", err)
	}
	cancel()
	releaseHandler()
	budget, cancel = context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := sub.DrainContext(budget); err != nil {
		t.Fatal(err)
	}
	select {
	case <-returned:
	default:
		t.Fatal("drain returned before callback")
	}
	if err := client.PublishOnce(subject, nil); !errors.Is(err, fnats.ErrClosed) {
		t.Fatalf("publish after close=%v", err)
	}
}

func TestRealRawCancellationDoesNotReplayAnAdmittedRequest(t *testing.T) {
	client := rawTestClient(t)
	subject := fmt.Sprintf("roost.raw.cancel.%d", time.Now().UnixNano())
	entered, release := make(chan struct{}, 2), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	sub, err := client.SubscribeBounded(subject, fnats.PendingLimits{Messages: 4, Bytes: 4096}, func(msg *fnats.Msg) { entered <- struct{}{}; <-release; _ = client.PublishOnce(msg.Reply, nil) })
	if err != nil {
		t.Fatal(err)
	}
	rawTestFlush(t, client)
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { _, err := client.RequestContext(ctx, subject, nil); result <- err }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("request did not arrive")
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("request=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancel did not release waiter")
	}
	unblock()
	ctx, done := context.WithTimeout(context.Background(), time.Second)
	defer done()
	if err := sub.DrainContext(ctx); err != nil {
		t.Fatal(err)
	}
	if len(entered) != 0 {
		t.Fatal("canceled request was replayed")
	}
}

func TestRealScopedInboxReusesSubscriptionAndCancelsOnlyItsWaiter(t *testing.T) {
	client := rawTestClient(t)
	prefix := fmt.Sprintf("roost.raw.scoped.n%d", time.Now().UnixNano())
	scoped, err := client.ForInbox(prefix)
	if err != nil {
		t.Fatal(err)
	}
	again, err := client.ForInbox(prefix)
	if err != nil || scoped != again {
		t.Fatalf("inbox not reused: %v", err)
	}
	responder, err := client.SubscribeBounded(prefix+".request", fnats.PendingLimits{Messages: 4096, Bytes: 1 << 20}, func(msg *fnats.Msg) { _ = client.PublishOnce(msg.Reply, msg.Data) })
	if err != nil {
		t.Fatal(err)
	}
	rawTestFlush(t, client)
	var wait sync.WaitGroup
	for index := range 256 {
		wait.Go(func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			payload := []byte(fmt.Sprintf("request-%d", index))
			data, err := scoped.RequestContext(ctx, prefix+".request", payload)
			if err != nil || string(data) != string(payload) {
				t.Errorf("data=%q err=%v", data, err)
			}
		})
	}
	wait.Wait()
	inbox := scoped.(*inboxClient)
	inbox.mu.Lock()
	pending := len(inbox.pending)
	inbox.mu.Unlock()
	if pending != 0 {
		t.Fatalf("pending=%d", pending)
	}
	// 取消一个已发出请求只移除它的 waiter；迟到回信不能串到后面的请求。
	requests := make(chan *fnats.Msg, 2)
	delayed, err := client.SubscribeBounded(prefix+".service.delayed", fnats.PendingLimits{Messages: 8, Bytes: 4096}, func(msg *fnats.Msg) { requests <- msg })
	if err != nil {
		t.Fatal(err)
	}
	rawTestFlush(t, client)
	cancelCtx, cancelWait := context.WithCancel(context.Background())
	canceled := make(chan error, 1)
	go func() {
		_, err := scoped.RequestContext(cancelCtx, prefix+".service.delayed", []byte("cancel"))
		canceled <- err
	}()
	var late *fnats.Msg
	select {
	case late = <-requests:
	case <-time.After(time.Second):
		t.Fatal("delayed request not sent")
	}
	cancelWait()
	select {
	case err := <-canceled:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancel did not release scoped waiter")
	}
	if err := client.PublishOnce(late.Reply, []byte("late")); err != nil {
		t.Fatal(err)
	}
	rawTestFlush(t, client)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	data, err := scoped.RequestContext(ctx, prefix+".request", []byte("after cancel"))
	if err != nil || string(data) != "after cancel" {
		t.Fatalf("late ACK contaminated next request: %q %v", data, err)
	}
	if err := delayed.DrainContext(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := scoped.RequestContext(ctx, prefix+".service.absent", nil); !errors.Is(err, fnats.ErrNoResponders) {
		t.Fatalf("no responder=%v", err)
	}
	if err := responder.DrainContext(ctx); err != nil {
		t.Fatal(err)
	}
	client.Close()
	if _, err := scoped.RequestContext(ctx, prefix+".request", nil); !errors.Is(err, fnats.ErrClosed) {
		t.Fatalf("closed=%v", err)
	}
}

// 真实 NATS 验收：排空被预算截断、硬关之后，nats.go 的排空协程会把状态翻回 DRAINING_PUBS、最长约 5s 内 IsConnected 为 true。
// 就在这段窗口里把守卫表整张跑一遍：每个导出方法照样按驱动自己的已关闭状态回答（fnats.ErrClosed / nil / false），与 nats.go
// 此刻报什么无关。需要 ROOST_DATAENGINE_IT_NATS_URL（scripts/mirror-local.sh 的私有环境）。
func TestRealNatsEveryExportedMethodAnswersFromTheDriverStateAfterAnUndrainedClose(t *testing.T) {
	url := os.Getenv("ROOST_DATAENGINE_IT_NATS_URL")
	if url == "" {
		t.Skip("ROOST_DATAENGINE_IT_NATS_URL is not set; run against a private environment (scripts/mirror-local.sh)")
	}
	a, err := Assemble(fnats.DefaultConfig(url), ClientOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Client.Close)
	js, err := NewJetStreamClient(a.Client)
	if err != nil {
		t.Fatal(err)
	}
	prefix := fmt.Sprintf("natsstate.%d", time.Now().UnixNano())
	sub, err := a.Client.Subscribe(prefix+".idle", closedStateHandler)
	if err != nil {
		t.Fatal(err)
	}
	// 卡住的直连订阅：排空只能等它或被预算截断。
	entered, release := make(chan struct{}), make(chan struct{})
	var enterOnce, releaseOnce sync.Once
	releaseHandler := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseHandler)
	if _, err := a.Client.Subscribe(prefix+".stuck", func(*fnats.Msg) {
		enterOnce.Do(func() { close(entered) })
		<-release
	}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := a.Client.Publish(prefix+".stuck", []byte("x")); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("subscription handler never entered; not a real-dependency run")
	}
	budget, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if err := a.Close(budget); !errors.Is(err, ErrClosedUndrained) {
		t.Fatalf("budgeted Assembly.Close = %v, want ErrClosedUndrained", err)
	}
	// 等 nats.go 的排空协程把状态翻回 DRAINING_PUBS（RR-20261006-26 实测在硬关后约 10ms 内）。没等到就不是
	// 这条用例要的场景，按失败处理，不在别的状态上“通过”。
	flipped := false
	for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
		if a.Client.conn.IsDraining() && a.Client.conn.IsConnected() {
			flipped = true
			break
		}
	}
	if !flipped {
		t.Fatal("nats.go never flipped back to DRAINING_PUBS after the hard close; the window this test targets did not occur")
	}

	ctx, cancelCalls := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelCalls()
	started := time.Now()
	env := &closedStateEnv{ctx: ctx, cancel: cancelCalls, asm: a, js: js, sub: sub.(*subscription)}
	checkEveryMethodAfterClose(t, env, "an undrained Assembly.Close", func() {
		// 非关闭类方法全部答完时 nats.go 仍在翻回的状态里，才说明它们是在这段窗口里按驱动状态回答的。
		if !a.Client.conn.IsConnected() {
			t.Errorf("nats.go left the flipped DRAINING_PUBS state before the table finished (%s); the window was not covered", time.Since(started))
		}
		t.Logf("non-closing methods answered in %s while nats.go still reports IsConnected = true", time.Since(started).Round(time.Millisecond))
	})
	releaseHandler()
}

// 轻量传输的 RPC 按调用方期限计时，超时 / 取消带 ctx 语义：调用方带期限时就按它计时（没有期限才用 5s 的单次上限）；超时同时
// errors.Is 到 fnats.ErrTimeout 与 context.DeadlineExceeded，取消同时 errors.Is 到 fnats.ErrCancelled 与 context.Canceled。
// 需要 ROOST_DATAENGINE_IT_NATS_URL（scripts/mirror-local.sh 的私有环境）。历史：RR-20261006-73。
func TestRealNatsRPCHonoursTheCallerDeadlineBeyondFiveSeconds(t *testing.T) {
	url := os.Getenv("ROOST_DATAENGINE_IT_NATS_URL")
	if url == "" {
		t.Skip("ROOST_DATAENGINE_IT_NATS_URL is not set; run against a private environment (scripts/mirror-local.sh)")
	}
	a, err := Assemble(fnats.DefaultConfig(url), ClientOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close(context.Background()) })
	prefix := fmt.Sprintf("rpcdeadline.%d", time.Now().UnixNano())
	sub, err := a.Client.Subscribe(prefix+".slow", func(msg *fnats.Msg) {
		time.Sleep(6 * time.Second)
		_ = a.Client.Publish(msg.Reply, []byte("late-but-in-budget"))
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	silent, err := a.Client.Subscribe(prefix+".silent", func(*fnats.Msg) {})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = silent.Unsubscribe() })

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	started := time.Now()
	resp, err := a.RPC.Call(ctx, prefix+".slow", []byte("x"))
	cancel()
	if err != nil {
		t.Fatalf("a 6s handler under an 8s caller deadline failed after %v: %v", time.Since(started).Round(time.Millisecond), err)
	}
	if string(resp) != "late-but-in-budget" {
		t.Fatalf("resp = %q", resp)
	}

	ctx, cancel = context.WithTimeout(context.Background(), 300*time.Millisecond)
	_, err = a.RPC.Call(ctx, prefix+".silent", []byte("x"))
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, fnats.ErrTimeout) {
		t.Fatalf("timeout = %v; want errors.Is to both context.DeadlineExceeded and fnats.ErrTimeout", err)
	}

	ctx, cancel = context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	_, err = a.RPC.Call(ctx, prefix+".silent", []byte("x"))
	if !errors.Is(err, context.Canceled) || !errors.Is(err, fnats.ErrCancelled) {
		t.Fatalf("cancel = %v; want errors.Is to both context.Canceled and fnats.ErrCancelled", err)
	}
}

// 已存在的 durable 在新版本启动时怎样变（DESIGN-TODO D12 验证项 4）：D12 把升级邮件消费者 game-level-up-mail 的
// MaxDeliver 20 → 25000、MaxAckPending 不写（服务器缺省 1000）→ 256。Subscribe 用 CreateOrUpdateConsumer：
//
//   - 可变字段（MaxDeliver、MaxAckPending、AckWait）原地改成新值，投递进度保留——旧版本已 ack 的消息不重投；
//   - 不可变字段（这里用 DeliverPolicy）与已存在的 durable 不同时服务器拒绝，Subscribe 返回的错误点名流、durable
//     与处置命令，consumer 保持原配置。
//
// 资源：流 D12V_CONSUMER_<ns>（单副本），用后删除。
func TestRealJetStreamSubscribeUpdatesAnExistingDurableInPlace(t *testing.T) {
	url := os.Getenv("ROOST_DATAENGINE_IT_NATS_URL")
	if url == "" {
		t.Skip("ROOST_DATAENGINE_IT_NATS_URL is not set; run against a private environment (scripts/mirror-local.sh)")
	}
	client, err := NewClient(fnats.DefaultConfig(url), ClientOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)
	js, err := NewJetStreamClient(client)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	token := time.Now().UnixNano()
	stream, subject, durable := fmt.Sprintf("D12V_CONSUMER_%d", token), fmt.Sprintf("d12v%d.player.level_up", token), "game-level-up-mail"
	if err := js.EnsureStream(ctx, fnats.JetStreamConfig{Name: stream, Subjects: []string{fmt.Sprintf("d12v%d.>", token)}, Replicas: 1}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := js.js.DeleteStream(context.Background(), stream); err != nil {
			t.Errorf("cleanup: delete stream %s: %v", stream, err)
		}
	})
	received := make(chan uint64, 16)
	handler := func(_ context.Context, msg *fnats.JetStreamMsg) error {
		received <- msg.StreamSeq
		return nil
	}
	// 旧版本：D12 之前 nestwal.SubscribeJetStreamEffects 的参数。消费一条并 ack。
	old := fnats.JetStreamConsumerConfig{Stream: stream, Name: durable, Durable: durable, FilterSubject: subject,
		DeliverPolicy: fnats.JetStreamDeliverAll, AckWait: 30 * time.Second, MaxDeliver: 20}
	sub, err := js.Subscribe(ctx, old, handler)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := js.Publish(ctx, subject, []byte("first"), fnats.JetStreamPublishOptions{}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-received:
	case <-ctx.Done():
		t.Fatal("old consumer did not receive the first message")
	}
	sub.Drain()
	<-sub.Closed()
	info := func() gojs.ConsumerConfig {
		t.Helper()
		consumer, err := js.js.Consumer(ctx, stream, durable)
		if err != nil {
			t.Fatal(err)
		}
		return consumer.CachedInfo().Config
	}
	if got := info(); got.MaxDeliver != 20 || got.MaxAckPending != 1000 {
		t.Fatalf("premise: old consumer max_deliver=%d max_ack_pending=%d, want 20 / server default 1000", got.MaxDeliver, got.MaxAckPending)
	}
	// 新版本：wiring/dataengine.SubscribeEffects 的缺省。
	newer := old
	newer.MaxDeliver, newer.MaxAckPending = 25_000, 256
	sub, err = js.Subscribe(ctx, newer, handler)
	if err != nil {
		t.Fatalf("updating mutable consumer fields: %v", err)
	}
	if got := info(); got.MaxDeliver != 25_000 || got.MaxAckPending != 256 || got.AckWait != 30*time.Second || got.FilterSubject != subject {
		t.Fatalf("after the new version subscribed: max_deliver=%d max_ack_pending=%d ack_wait=%s filter=%s, want the new values applied in place",
			got.MaxDeliver, got.MaxAckPending, got.AckWait, got.FilterSubject)
	}
	ack, err := js.Publish(ctx, subject, []byte("second"), fnats.JetStreamPublishOptions{})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case seq := <-received:
		if seq != ack.Sequence {
			t.Fatalf("updated consumer delivered stream sequence %d first, want %d: the old delivery position was not kept", seq, ack.Sequence)
		}
	case <-ctx.Done():
		t.Fatal("updated consumer did not receive the second message")
	}
	sub.Drain()
	<-sub.Closed()
	// 不可变字段：报错，点名流、durable 与处置命令；consumer 不变。
	immutable := newer
	immutable.DeliverPolicy = fnats.JetStreamDeliverNew
	sub, err = js.Subscribe(ctx, immutable, handler)
	if err == nil {
		sub.Stop()
		t.Fatal("changing the deliver policy of an existing durable was accepted")
	}
	for _, want := range []string{stream, durable, "nats consumer rm " + stream + " " + durable, "deliver policy can not be updated"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("immutable conflict error %q does not name %q", err, want)
		}
	}
	if got := info(); got.DeliverPolicy != gojs.DeliverAllPolicy || got.MaxDeliver != 25_000 {
		t.Fatalf("a refused update changed the consumer: %+v", got)
	}
}
