package saga

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/nestwal"
	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
	"github.com/tjbdwanghaibo/roost-core/infra/storage/mongo/mongotest"
)

// 消费者一侧的承诺：启动中继（Nest 启动 effect → StartSaga）与两种步骤消费者对坏信封、准入、过期命令的处理，以及在途槽的结算。
// 步骤消费者的发布与 ack 顺序见 step_consumer_test.go。

type startCapture struct {
	requests []StartRequest
}

func (s *startCapture) StartSaga(_ context.Context, request StartRequest) (Record, error) {
	s.requests = append(s.requests, request)
	return Record{}, nil
}

type startJetStream struct {
	config  fnats.JetStreamConsumerConfig
	handler fnats.JetStreamHandler
	subject string
	data    []byte
	msgID   string
}

func (*startJetStream) EnsureStream(context.Context, fnats.JetStreamConfig) error { return nil }
func (s *startJetStream) Publish(_ context.Context, subject string, data []byte, options fnats.JetStreamPublishOptions) (fnats.JetStreamPublishAck, error) {
	s.subject, s.data, s.msgID = subject, append([]byte(nil), data...), options.MsgID
	return fnats.JetStreamPublishAck{}, nil
}
func (s *startJetStream) Subscribe(_ context.Context, config fnats.JetStreamConsumerConfig, handler fnats.JetStreamHandler) (fnats.IJetStreamSubscription, error) {
	s.config, s.handler = config, handler
	return closedSubscription{}, nil
}

type closedSubscription struct{}

func (closedSubscription) Stop()  {}
func (closedSubscription) Drain() {}
func (closedSubscription) Closed() <-chan struct{} {
	closed := make(chan struct{})
	close(closed)
	return closed
}

func TestSubscribeNestStartsUsesSharedDurableAndDecodesIntent(t *testing.T) {
	client, capture := &startJetStream{}, &startCapture{}
	_, err := SubscribeNestStarts(context.Background(), client, NestStartConsumerConfig{
		Stream: "ROOST_EFFECTS", Durable: "game-saga-start", EffectPrefix: "roost.effect",
	}, capture)
	if err != nil {
		t.Fatal(err)
	}
	if client.config.FilterSubject != "roost.effect.saga.start" || client.config.Durable != "game-saga-start" || client.config.MaxAckPending != 256 || client.config.MaxDeliver != 25_000 || client.config.NakBackoffMin <= 0 {
		t.Fatalf("consumer config=%+v", client.config)
	}
	effect, err := NewStartEffect(StartRequest{Type: "rally", DefinitionVersion: 1, BusinessKey: "r-1", Data: []byte("state")})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(nestwal.EffectEnvelope{TransactionID: "tx-1", EffectID: effect.ID, Topic: effect.Topic, Key: effect.Key, Payload: effect.Payload})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.handler(context.Background(), &fnats.JetStreamMsg{Data: raw}); err != nil {
		t.Fatal(err)
	}
	if len(capture.requests) != 1 || capture.requests[0].DefinitionVersion != 1 || capture.requests[0].BusinessKey != "r-1" || string(capture.requests[0].Data) != "state" {
		t.Fatalf("requests=%+v", capture.requests)
	}
}

func expectKitErr(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("error = %v, want it to contain %q", err, want)
	}
}

// broker 无法满足的限额、或处理超时不短于 AckWait 的配置会让仍在执行的工作被重投：每条拒绝都断言，被拒的配置不订阅。
func TestSubscribeNestStartsRefusesEachUnsafeConfig(t *testing.T) {
	valid := NestStartConsumerConfig{Stream: "ROOST_EFFECTS", Durable: "game-saga-start", EffectPrefix: "roost.effect"}
	cases := []struct {
		name   string
		mutate func(*NestStartConsumerConfig)
		want   string
	}{
		{"blank stream", func(c *NestStartConsumerConfig) { c.Stream = " " }, "stream, durable and effect prefix are required"},
		{"blank durable", func(c *NestStartConsumerConfig) { c.Durable = "" }, "stream, durable and effect prefix are required"},
		{"prefix with wildcard", func(c *NestStartConsumerConfig) { c.EffectPrefix = "roost.>" }, "stream, durable and effect prefix are required"},
		{"process timeout not below ack wait", func(c *NestStartConsumerConfig) { c.AckWait, c.ProcessTimeout = 5*time.Second, 5*time.Second }, "unsafe Nest start consumer limits"},
		{"max deliver above broker cap", func(c *NestStartConsumerConfig) { c.MaxDeliver = 1_000_001 }, "unsafe Nest start consumer limits"},
		{"max ack pending above broker cap", func(c *NestStartConsumerConfig) { c.MaxAckPending = 65_537 }, "unsafe Nest start consumer limits"},
		{"nak backoff above a day", func(c *NestStartConsumerConfig) { c.NakBackoffMin, c.NakBackoffMax = time.Second, 25*time.Hour }, "unsafe Nest start consumer limits"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := &startJetStream{}
			cfg := valid
			tc.mutate(&cfg)
			_, err := SubscribeNestStarts(context.Background(), client, cfg, &startCapture{})
			expectKitErr(t, err, tc.want)
			if client.handler != nil {
				t.Fatal("a refused config must not subscribe")
			}
		})
	}
	if _, err := SubscribeNestStarts(context.Background(), nil, valid, &startCapture{}); err == nil {
		t.Fatal("nil client accepted")
	}
}

// Nest 启动消费者：超长帧、缺 effect id、外来 topic、无法解码的启动意图都不到达 starter，且一律永久拒绝（不重投循环）。
func TestHandleNestStartRefusesEachMalformedEnvelopePermanently(t *testing.T) {
	encode := func(t *testing.T, env nestwal.EffectEnvelope) []byte {
		t.Helper()
		raw, err := json.Marshal(env)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	// A start payload that would be accepted on its own, so that only the
	// envelope-level rule under test can be the reason for the refusal.
	effect, err := NewStartEffect(StartRequest{Type: "rally", DefinitionVersion: 1, BusinessKey: "r-9"})
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string][]byte{
		"oversized frame":   make([]byte, maxWireEnvelopeBytes+1),
		"missing effect id": encode(t, nestwal.EffectEnvelope{Topic: StartEffectTopic, Payload: effect.Payload}),
		"foreign topic":     encode(t, nestwal.EffectEnvelope{EffectID: effect.ID, Topic: "other", Payload: effect.Payload}),
		"undecodable start": encode(t, nestwal.EffectEnvelope{EffectID: effect.ID, Topic: StartEffectTopic, Payload: []byte(`{"version":99}`)}),
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			capture := &startCapture{}
			err := handleNestStart(context.Background(), &fnats.JetStreamMsg{Data: raw}, capture)
			if err == nil {
				t.Fatal("malformed envelope accepted")
			}
			if !fnats.IsPermanent(err) {
				t.Fatalf("refusal must be permanent, got %v", err)
			}
			if len(capture.requests) != 0 {
				t.Fatal("malformed envelope reached the starter")
			}
		})
	}
	if err := handleNestStart(context.Background(), nil, &startCapture{}); err == nil || !fnats.IsPermanent(err) {
		t.Fatalf("nil message = %v", err)
	}
}

// 步骤侧解码：超长帧、未知线上版本、没有命令、命令校验不过、命令不属于事件里的 saga 或分区，都归为 ErrInvalidRecord，不交给步骤 handler。
// 历史：TestDecodeStepCommandRefusesOversizedForeignAndInvalidEnvelopes（命令信封 → 分区命令事件）。
func TestDecodeCommandEventRefusesOversizedForeignAndInvalidEnvelopes(t *testing.T) {
	command := stepCommand(0, 1, time.Minute)
	event := func(mutate func(*logEvent)) []byte {
		ev := logEvent{V: WireVersion, Part: int(command.Partition), Writer: "w", Kind: "dispatch", Command: &command}
		ev.Saga.Record.ID = command.SagaID
		if mutate != nil {
			mutate(&ev)
		}
		raw, _ := json.Marshal(ev)
		return raw
	}
	if got, err := decodeCommandEvent(&fnats.JetStreamMsg{Data: event(nil)}); err != nil || got.ID != command.ID {
		t.Fatalf("a valid command event = %+v %v", got, err)
	}
	invalid := command
	invalid.Attempt = 0
	otherSaga := command
	otherSaga.SagaID = "saga-2"
	for name, data := range map[string][]byte{
		"oversized":               make([]byte, maxWireEnvelopeBytes+1),
		"corrupt json":            []byte("{"),
		"foreign version":         event(func(ev *logEvent) { ev.V = WireVersion + 1 }),
		"no command":              event(func(ev *logEvent) { ev.Command = nil }),
		"invalid command":         event(func(ev *logEvent) { ev.Command = &invalid }),
		"command of another saga": event(func(ev *logEvent) { ev.Command = &otherSaga }),
		"another partition":       event(func(ev *logEvent) { ev.Part = 1 }),
	} {
		if _, err := decodeCommandEvent(&fnats.JetStreamMsg{Data: data}); !errors.Is(err, ErrInvalidRecord) {
			t.Fatalf("%s = %v, want ErrInvalidRecord", name, err)
		}
	}
	if _, err := decodeCommandEvent(nil); !errors.Is(err, ErrInvalidRecord) {
		t.Fatalf("nil message = %v", err)
	}
}

// 三个 saga 消费者（原生步骤、Mongo 步骤、启动中继）对坏信封同一口径：空消息、超长帧、损坏 JSON、未知 WireVersion、内容校验不过
// 都是确定性错误，返回 Permanent（驱动 Term，不 nak 到 MaxDeliver 占着共享 durable 的 MaxAckPending），并计
// saga.consumer.rejected_total{consumer}；handler 不执行。历史：RR-20261006-44（当时五个消费者）。
func TestSagaConsumersTermEveryBadEnvelopeAndAlarm(t *testing.T) {
	ctx := context.Background()
	layout := stepLayout()
	command := stepCommand(0, 1, time.Minute)
	invalid := command
	invalid.Attempt = 0
	commandEvent := func(version uint16, command Command) []byte {
		ev := logEvent{V: version, Part: int(command.Partition), Writer: "w", Kind: "dispatch", Command: &command}
		ev.Saga.Record.ID = command.SagaID
		raw, _ := json.Marshal(ev)
		return raw
	}
	effectBody := func(topic string, payload []byte) []byte {
		raw, _ := json.Marshal(nestwal.EffectEnvelope{EffectID: "effect-1", Topic: topic, Payload: payload})
		return raw
	}
	handled := 0
	consumers := []struct {
		name    string
		handler func(*testing.T) fnats.JetStreamHandler
		bad     map[string][]byte
	}{
		{"native_step", func(t *testing.T) fnats.JetStreamHandler {
			js := newStepJetStream()
			body := func(context.Context, NativeStep) error { handled++; return nil }
			if _, err := SubscribeNativeStep(ctx, js, layout, StepConsumerConfig{Durable: "native", Topic: command.Topic}, body); err != nil {
				t.Fatal(err)
			}
			return js.handlers[layout.StreamOf(int(command.Partition))+" "+layout.CommandFilter(command.Topic)]
		}, map[string][]byte{"oversized": make([]byte, maxWireEnvelopeBytes+1), "corrupt json": []byte("{"), "foreign version": commandEvent(WireVersion+1, command), "invalid command": commandEvent(WireVersion, invalid)}},
		{"mongo_step", func(t *testing.T) fnats.JetStreamHandler {
			js := newStepJetStream()
			inbox, err := NewMongoStepInbox(mongotest.NewClient(), "game", "_bad_step_inbox", time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			handler := func(context.Context, Command) (Completion, error) { handled++; return Completion{Success: true}, nil }
			if _, err := SubscribeMongoStep(ctx, js, layout, inbox, StepConsumerConfig{Durable: "mongo", Topic: command.Topic}, handler); err != nil {
				t.Fatal(err)
			}
			return js.handlers[layout.StreamOf(int(command.Partition))+" "+layout.CommandFilter(command.Topic)]
		}, map[string][]byte{"oversized": make([]byte, maxWireEnvelopeBytes+1), "corrupt json": []byte("{"), "foreign version": commandEvent(WireVersion+1, command), "invalid command": commandEvent(WireVersion, invalid)}},
		{"start", func(t *testing.T) fnats.JetStreamHandler {
			js := &startJetStream{}
			if _, err := SubscribeNestStarts(ctx, js, NestStartConsumerConfig{Stream: "EFFECTS", Durable: "starts", EffectPrefix: "roost.effect"}, &startCapture{}); err != nil {
				t.Fatal(err)
			}
			return js.handler
		}, map[string][]byte{"oversized": make([]byte, maxWireEnvelopeBytes+1), "corrupt json": []byte("{"), "foreign topic": effectBody("other.topic", nil), "invalid start": effectBody(StartEffectTopic, []byte(`{"version":2,"start":{}}`))}},
	}
	for _, consumer := range consumers {
		t.Run(consumer.name, func(t *testing.T) {
			handler := consumer.handler(t)
			for name, data := range consumer.bad {
				before := rejectedEnvelopes(consumer.name)
				err := handler(ctx, &fnats.JetStreamMsg{Subject: "s", Data: data})
				if err == nil || !fnats.IsPermanent(err) {
					t.Fatalf("%s: %s = %v, want a permanent error (Term)", consumer.name, name, err)
				}
				if grown := rejectedEnvelopes(consumer.name) - before; grown != 1 {
					t.Fatalf("%s: %s grew saga.consumer.rejected_total by %d, want 1", consumer.name, name, grown)
				}
			}
		})
	}
	if handled != 0 {
		t.Fatalf("a bad envelope reached a step handler %d times", handled)
	}
}

func rejectedEnvelopes(consumer string) int64 {
	var total int64
	for _, metric := range metrics.Snapshot() {
		if metric.Name == "saga.consumer.rejected_total" && metric.Labels["consumer"] == consumer {
			total += metric.Value
		}
	}
	return total
}

// 多进程：命令投递到不该执行它的进程时，必须在执行任何东西之前拒绝。StepConsumerConfig.Admit 在执行之前调用；它返回错误时
// handler 不执行、不写回执、不发布，错误原样返回（nak 交给下一个消费者）。历史：game-demo 实跑（礼物 saga 248 次投递、24 条卡住）。
func TestStepConsumersAdmitBeforeExecuting(t *testing.T) {
	ctx := context.Background()
	layout := stepLayout()
	command := stepCommand(0, 1, time.Minute)
	refusal := errors.New("player is bound to another sid")
	admit := func(context.Context, Command) error { return refusal }
	t.Run("native", func(t *testing.T) {
		js, h := newStepJetStream(), newNativeHarness()
		if _, err := SubscribeNativeStep(ctx, js, layout, StepConsumerConfig{Durable: "native", Topic: command.Topic, Admit: admit}, h.body); err != nil {
			t.Fatal(err)
		}
		if err := js.deliver(t, layout, command, nil); !errors.Is(err, refusal) || fnats.IsPermanent(err) {
			t.Fatalf("refused delivery = %v, want the admission error (nak)", err)
		}
		if h.business != 0 || len(h.entity.receipts) != 0 || len(js.takePublished()) != 0 {
			t.Fatalf("a refused delivery ran the business %d times, left %d receipts or published", h.business, len(h.entity.receipts))
		}
	})
	t.Run("mongo", func(t *testing.T) {
		js := newStepJetStream()
		client := mongotest.NewClient()
		inbox, err := NewMongoStepInbox(client, "game", "_admit_step_inbox", time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		runs := 0
		handler := func(context.Context, Command) (Completion, error) { runs++; return Completion{Success: true}, nil }
		if _, err := SubscribeMongoStep(ctx, js, layout, inbox, StepConsumerConfig{Durable: "mongo", Topic: command.Topic, Admit: admit}, handler); err != nil {
			t.Fatal(err)
		}
		if err := js.deliver(t, layout, command, nil); !errors.Is(err, refusal) || fnats.IsPermanent(err) {
			t.Fatalf("refused delivery = %v, want the admission error (nak)", err)
		}
		if runs != 0 || client.Collection("game", "_admit_step_inbox").Len() != 0 || len(js.takePublished()) != 0 {
			t.Fatalf("a refused delivery ran the handler %d times or left a receipt / publish", runs)
		}
	})
}

// 已过截止、又没有回执的步骤命令被终结（ack），不无限 nak：截止后协调器自己按超时推进，不再等这条旧消息。无回执时返回 nil、
// 不执行 handler、不发布，计 saga.step.expired_unexecuted_total；有成功回执时照样把它发回协调器（过期投递重发成功）。
// 历史：U-0281（演练中共享 durable 被过期旧尝试占满 MaxAckPending）。
func TestExpiredStepCommandWithoutReceiptIsAcknowledgedNotRedelivered(t *testing.T) {
	ctx := context.Background()
	layout := stepLayout()
	t.Run("native", func(t *testing.T) {
		js, h := newStepJetStream(), newNativeHarness()
		if _, err := SubscribeNativeStep(ctx, js, layout, StepConsumerConfig{Durable: "native", Topic: "rally.reserve"}, h.body); err != nil {
			t.Fatal(err)
		}
		before := expiredUnexecuted()
		expired := stepCommand(0, 1, -time.Second)
		if err := js.deliver(t, layout, expired, nil); err != nil {
			t.Fatalf("expired command without a receipt = %v, want ack", err)
		}
		if h.business != 0 || len(js.takePublished()) != 0 || expiredUnexecuted()-before != 1 {
			t.Fatalf("expired command: business %d, counter +%d", h.business, expiredUnexecuted()-before)
		}
		// 有成功回执：较早一次尝试生效了，过期的这次投递回放它。
		live := stepCommand(0, 2, time.Minute)
		if err := js.deliver(t, layout, live, nil); err != nil {
			t.Fatal(err)
		}
		js.takePublished()
		late := stepCommand(0, 3, -time.Second)
		if err := js.deliver(t, layout, late, nil); err != nil {
			t.Fatal(err)
		}
		if published := js.takePublished(); len(published) != 1 || published[0].input.Completion.CommandID != live.ID || !published[0].input.Completion.Success {
			t.Fatalf("an expired delivery with a success receipt published %+v, want the success of %s", published, live.ID)
		}
	})
	t.Run("mongo", func(t *testing.T) {
		js := newStepJetStream()
		inbox, err := NewMongoStepInbox(mongotest.NewClient(), "game", "_expired_step_inbox", time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		runs := 0
		handler := func(context.Context, Command) (Completion, error) { runs++; return Completion{Success: true}, nil }
		if _, err := SubscribeMongoStep(ctx, js, layout, inbox, StepConsumerConfig{Durable: "mongo", Topic: "rally.reserve"}, handler); err != nil {
			t.Fatal(err)
		}
		before := expiredUnexecuted()
		if err := js.deliver(t, layout, stepCommand(0, 1, -time.Second), nil); err != nil {
			t.Fatalf("expired command without a receipt = %v, want ack", err)
		}
		if runs != 0 || len(js.takePublished()) != 0 || expiredUnexecuted()-before != 1 {
			t.Fatalf("expired command: handler %d, counter +%d", runs, expiredUnexecuted()-before)
		}
	})
}

func expiredUnexecuted() int64 {
	for _, metric := range metrics.Snapshot() {
		if metric.Name == "saga.step.expired_unexecuted_total" {
			return metric.Value
		}
	}
	return 0
}

// 工作槽在结算前不能复用；回调必须能释放订阅线程，取消排队者不能启动业务。
func TestConsumerWorkersBoundExecutionAndDeferSettlement(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		unblock := sync.OnceFunc(func() { close(release) })
		defer unblock()
		entered, returned, settled := make(chan struct{}, 3), make(chan error, 3), make(chan error, 3)
		handler := withConsumerWorkers(2, func(context.Context, *fnats.JetStreamMsg) error { entered <- struct{}{}; <-release; return nil })
		message := func() *fnats.JetStreamMsg {
			return &fnats.JetStreamMsg{DeferSettle: func() func(error) { return func(err error) { settled <- err } }}
		}
		for range 2 {
			go func() { returned <- handler(context.Background(), message()) }()
		}
		synctest.Wait()
		if len(entered) != 2 || len(returned) != 2 || len(settled) != 0 {
			t.Errorf("before release: entered=%d returned=%d settled=%d want 2/2/0", len(entered), len(returned), len(settled))
			return
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- handler(ctx, message()) }()
		synctest.Wait()
		if len(entered) != 2 {
			t.Fatal("worker budget exceeded")
		}
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("queued cancellation: %v", err)
		}
		unblock()
		synctest.Wait()
		if len(settled) != 2 {
			t.Fatalf("settled=%d want 2", len(settled))
		}
	})
}

func TestConsumerWorkersSettlePanicAndFallback(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		settled := make(chan error, 1)
		handler := withConsumerWorkers(2, func(context.Context, *fnats.JetStreamMsg) error { panic("business panic") })
		if err := handler(context.Background(), &fnats.JetStreamMsg{DeferSettle: func() func(error) { return func(err error) { settled <- err } }}); err != nil {
			t.Fatal(err)
		}
		if err := <-settled; err == nil || !strings.Contains(err.Error(), "business panic") {
			t.Fatalf("panic settlement: %v", err)
		}
		expected := errors.New("business error")
		handler = withConsumerWorkers(2, func(context.Context, *fnats.JetStreamMsg) error { return expected })
		if err := handler(context.Background(), &fnats.JetStreamMsg{}); err != expected {
			t.Fatalf("synchronous fallback: %v", err)
		}
	})
}
