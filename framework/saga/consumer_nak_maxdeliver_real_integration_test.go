//go:build integration

package saga

// v1.23.0 发版前验证（2026-10-06，记录 docs/bugfix/PRERELEASE-VERIFICATION-2026-10-06.md 第 4 项）：
// saga 结果消费者在真实 NATS JetStream 上的 nak 退避与 MaxDeliver。单测只在替身上断言了 handler 返回的
// 错误分类（completion_definition_rollout_promises_test.go），真正的重投节奏和终止由 broker 与
// nats/driver 的 settle 决定，这里在真实 JetStream 上核对：
//
//  1. 结果先到了还没有这个定义版本的协调器（ErrDefinitionMissing，可重试）：按 NakBackoffMin 起翻倍的延迟
//     重投，第 MaxDeliver 次仍失败时 Term，之后不再投递、没有在途未确认；记录保持在等第 0 步。
//  2. 定义在两次投递之间上线：重投被接收并 ack，记录推进到第 1 步，不再投递。
//
// 资源：流 SAGANAK_<pid>_<ns>、subject 前缀 sagaknak<pid>x<ns>，用后删除。协调器存储用 mongotest。
// 运行：source ~/.roost-it/roost-dataengine-it/env.sh 后
//   GOWORK=off go test -tags integration -count=1 -run '^TestRealNatsCompletionNakBackoffAndMaxDeliver$' ./framework/saga/

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	gonats "github.com/nats-io/nats.go"
	gojs "github.com/nats-io/nats.go/jetstream"
	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
	ndriver "github.com/tjbdwanghaibo/roost-core/infra/network/nats/driver"
	"github.com/tjbdwanghaibo/roost-core/framework/nestwal"
)

// recordingCompleter 记下每次投递的时间与结果；afterFirst 在第一次投递返回之后执行一次（模拟定义上线）。
type recordingCompleter struct {
	inner      Completer
	afterFirst func()

	mu         sync.Mutex
	deliveries []time.Time
	errs       []error
}

func (c *recordingCompleter) Complete(ctx context.Context, completion Completion) (Record, error) {
	record, err := c.inner.Complete(ctx, completion)
	c.mu.Lock()
	c.deliveries = append(c.deliveries, time.Now())
	c.errs = append(c.errs, err)
	first := len(c.deliveries) == 1
	c.mu.Unlock()
	if first && c.afterFirst != nil {
		c.afterFirst()
	}
	return record, err
}

func (c *recordingCompleter) snapshot() ([]time.Time, []error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]time.Time(nil), c.deliveries...), append([]error(nil), c.errs...)
}

func (c *recordingCompleter) waitFor(t *testing.T, n int, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		if deliveries, _ := c.snapshot(); len(deliveries) >= n {
			return
		}
		if time.Now().After(deadline) {
			deliveries, errs := c.snapshot()
			t.Fatalf("got %d deliveries within %s, want %d (errors %v)", len(deliveries), within, n, errs)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestRealNatsCompletionNakBackoffAndMaxDeliver(t *testing.T) {
	natsURL := os.Getenv("ROOST_DATAENGINE_IT_NATS_URL")
	if natsURL == "" {
		t.Skip("ROOST_DATAENGINE_IT_NATS_URL is not set; source ~/.roost-it/roost-dataengine-it/env.sh")
	}
	natsClient, err := ndriver.NewClient(fnats.DefaultConfig(natsURL), ndriver.ClientOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(natsClient.Close)
	jetStream, err := ndriver.NewJetStreamClient(natsClient)
	if err != nil {
		t.Fatal(err)
	}
	suffix := fmt.Sprintf("%d_%d", os.Getpid(), time.Now().UnixNano())
	stream, prefix := "SAGANAK_"+suffix, fmt.Sprintf("sagaknak%dx%d", os.Getpid(), time.Now().UnixNano())
	ctx := context.Background()
	if err := jetStream.EnsureStream(ctx, fnats.JetStreamConfig{Name: stream, Subjects: []string{prefix + ".>"}, Storage: fnats.JetStreamStorageFile, MaxAge: 10 * time.Minute, Duplicates: time.Minute, Replicas: 1, MaxBytes: 8 << 20}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { deleteRevn06s5Stream(t, natsURL, stream) })

	const (
		nakMin  = 200 * time.Millisecond
		nakMax  = time.Second
		ackWait = 3 * time.Second
	)
	run := func(t *testing.T, durable string, maxDeliver int, completer *recordingCompleter, completion Completion) fnats.IJetStreamSubscription {
		t.Helper()
		sub, err := SubscribeNestCompletions(ctx, jetStream, NestCompletionConsumerConfig{
			// 每个子用例自己的 effect 前缀：同一个流里另一个子用例的消息不能被这个消费者读到。
			Stream: stream, Durable: durable, EffectPrefix: prefix + "." + durable,
			AckWait: ackWait, ProcessTimeout: time.Second, MaxDeliver: maxDeliver, NakBackoffMin: nakMin, NakBackoffMax: nakMax,
		}, completer)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			sub.Drain()
			select {
			case <-sub.Closed():
			case <-time.After(10 * time.Second):
				t.Errorf("subscription %s did not close", durable)
			}
		})
		effect, err := NewCompletionEffect(completion)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := json.Marshal(nestwal.EffectEnvelope{TransactionID: "tx-" + durable, EffectID: effect.ID, Topic: effect.Topic, Key: effect.Key, Payload: effect.Payload})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := jetStream.Publish(ctx, prefix+"."+durable+"."+effect.Topic, raw, fnats.JetStreamPublishOptions{MsgID: durable}); err != nil {
			t.Fatal(err)
		}
		return sub
	}
	ackPending := func(t *testing.T, durable string) (int, int) {
		t.Helper()
		nc, err := gonats.Connect(natsURL, gonats.Timeout(2*time.Second))
		if err != nil {
			t.Fatal(err)
		}
		defer nc.Close()
		js, err := gojs.New(nc)
		if err != nil {
			t.Fatal(err)
		}
		consumer, err := js.Consumer(ctx, stream, durable)
		if err != nil {
			t.Fatal(err)
		}
		info, err := consumer.Info(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return info.NumAckPending, int(info.NumPending)
	}

	t.Run("definition never arrives: nak with backoff, terminated at MaxDeliver", func(t *testing.T) {
		store, _, rollingIn, waiting := waitingOnANewDefinition(t)
		completer := &recordingCompleter{inner: rollingIn}
		const maxDeliver = 3
		run(t, "never", maxDeliver, completer, Completion{CommandID: waiting.CommandID, IdempotencyKey: waiting.OperationKey, SagaID: waiting.ID, Success: true})

		completer.waitFor(t, maxDeliver, 10*time.Second)
		// 第 MaxDeliver 次之后 Term：等过一个 AckWait 也不该有第 4 次。
		time.Sleep(ackWait + time.Second)
		deliveries, errs := completer.snapshot()
		if len(deliveries) != maxDeliver {
			t.Fatalf("deliveries = %d, want exactly MaxDeliver=%d (errors %v)", len(deliveries), maxDeliver, errs)
		}
		for i, err := range errs {
			if !errors.Is(err, ErrDefinitionMissing) {
				t.Fatalf("delivery %d returned %v, want ErrDefinitionMissing", i+1, err)
			}
		}
		// nak 退避：第 1 次失败后 nakMin，第 2 次后 2×nakMin（broker 计时，留 10% 余量）。
		for i, want := range []time.Duration{nakMin, 2 * nakMin} {
			if gap := deliveries[i+1].Sub(deliveries[i]); gap < want*9/10 {
				t.Fatalf("gap before delivery %d = %s, want ≥ %s (nak with delay, not an immediate redelivery)", i+2, gap, want)
			}
			t.Logf("gap before delivery %d = %s (nak delay %s)", i+2, deliveries[i+1].Sub(deliveries[i]), want)
		}
		if pending, unseen := ackPending(t, "never"); pending != 0 || unseen != 0 {
			t.Fatalf("after Term: ack pending %d, pending %d; want 0 / 0", pending, unseen)
		}
		if current, err := store.Get(ctx, waiting.ID); err != nil || current.Status != StatusWaiting || current.OperationKey != waiting.OperationKey {
			t.Fatalf("record after the refused deliveries = %+v, %v; want still waiting on step 0", current, err)
		}
	})

	t.Run("definition arrives between deliveries: the redelivery is accepted and acked", func(t *testing.T) {
		store, _, rollingIn, waiting := waitingOnANewDefinition(t)
		completer := &recordingCompleter{inner: rollingIn}
		completer.afterFirst = func() {
			if err := rollingIn.Register(testDefinition()); err != nil {
				t.Error(err)
			}
		}
		run(t, "rollout", 5, completer, Completion{CommandID: waiting.CommandID, IdempotencyKey: waiting.OperationKey, SagaID: waiting.ID, Success: true})

		completer.waitFor(t, 2, 10*time.Second)
		time.Sleep(ackWait + time.Second)
		deliveries, errs := completer.snapshot()
		if len(deliveries) != 2 || !errors.Is(errs[0], ErrDefinitionMissing) || errs[1] != nil {
			t.Fatalf("deliveries = %d with errors %v; want ErrDefinitionMissing then accepted, nothing after", len(deliveries), errs)
		}
		if gap := deliveries[1].Sub(deliveries[0]); gap < nakMin*9/10 {
			t.Fatalf("gap before the redelivery = %s, want ≥ %s", gap, nakMin)
		}
		if pending, unseen := ackPending(t, "rollout"); pending != 0 || unseen != 0 {
			t.Fatalf("after the accepted redelivery: ack pending %d, pending %d; want 0 / 0", pending, unseen)
		}
		if current, err := store.Get(ctx, waiting.ID); err != nil || current.Step != 1 || current.CompletedSteps != 1 {
			t.Fatalf("record after the accepted redelivery = %+v, %v; want step 0 completed", current, err)
		}
	})
}
