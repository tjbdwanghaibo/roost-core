package saga

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/nestwal"
	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
	"github.com/tjbdwanghaibo/roost-core/infra/storage/mongo/mongotest"
)

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

func TestJetStreamPublisherUsesVersionedEnvelope(t *testing.T) {
	client := &startJetStream{}
	publisher, err := NewJetStreamPublisher(client, "roost.saga")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	command := Command{ID: "s:1:0:1", IdempotencyKey: "s:1:0", SagaID: "s", SagaType: "rally", DefinitionVersion: 1, BusinessKey: "r-1", Step: 0, StepName: "reserve", Phase: PhaseForward, Attempt: 1, Topic: "rally.reserve", DeadlineAt: now.Add(time.Second), CreatedAt: now}
	if err := publisher.PublishSagaCommand(context.Background(), command); err != nil {
		t.Fatal(err)
	}
	var envelope commandEnvelope
	if err := json.Unmarshal(client.data, &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Version != WireVersion || envelope.Command.ID != command.ID || client.msgID != command.ID || client.subject != "roost.saga.command.rally.reserve" {
		t.Fatalf("envelope=%+v subject=%s msgID=%s", envelope, client.subject, client.msgID)
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

// 步骤侧解码：超长帧、未知线上版本、校验不过的命令都归为 ErrInvalidRecord，不交给步骤 handler。
func TestDecodeStepCommandRefusesOversizedForeignAndInvalidEnvelopes(t *testing.T) {
	now := time.Now()
	good := Command{ID: "c-1", IdempotencyKey: "k-1", SagaID: "s", SagaType: "rally", DefinitionVersion: 1, BusinessKey: "b", StepName: "reserve", Phase: PhaseForward, Attempt: 1, Topic: "rally.reserve", DeadlineAt: now.Add(time.Second), CreatedAt: now}
	encode := func(t *testing.T, version uint16, command Command) []byte {
		t.Helper()
		raw, err := json.Marshal(commandEnvelope{Version: version, Command: command})
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	if got, err := decodeStepCommand(&fnats.JetStreamMsg{Data: encode(t, WireVersion, good)}); err != nil || got.ID != "c-1" {
		t.Fatalf("valid envelope: %+v, %v", got, err)
	}
	if _, err := decodeStepCommand(nil); !errors.Is(err, ErrInvalidRecord) {
		t.Fatalf("nil message = %v", err)
	}
	if _, err := decodeStepCommand(&fnats.JetStreamMsg{Data: make([]byte, maxWireEnvelopeBytes+1)}); !errors.Is(err, ErrInvalidRecord) {
		t.Fatalf("oversized frame = %v", err)
	}
	if _, err := decodeStepCommand(&fnats.JetStreamMsg{Data: encode(t, WireVersion+1, good)}); !errors.Is(err, ErrInvalidRecord) {
		t.Fatalf("foreign wire version = %v", err)
	}
	broken := good
	broken.Attempt = 0
	if _, err := decodeStepCommand(&fnats.JetStreamMsg{Data: encode(t, WireVersion, broken)}); !errors.Is(err, ErrInvalidRecord) {
		t.Fatalf("invalid command = %v", err)
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

// 回执 TTL 放不进 Mongo TTL 索引（亚秒或超出 int32 秒）时，EnsureInfrastructure 带上取值拒绝。
func TestMongoCommandInboxRefusesUnrepresentableReceiptTTL(t *testing.T) {
	for _, ttl := range []time.Duration{500 * time.Millisecond, time.Duration(int64(^uint32(0)>>1)+1) * time.Second} {
		inbox, err := NewMongoCommandInbox(newInboxMongoFake(), "game", "", CommandInboxOptions{ReceiptTTL: ttl})
		if err != nil {
			t.Fatal(err)
		}
		expectKitErr(t, inbox.EnsureInfrastructure(context.Background()), "invalid receipt ttl")
	}
}

// 五个 saga 消费者对坏信封同一口径：空消息、超长帧、损坏 JSON、未知 WireVersion、内容校验不过都是确定性错误，
// 返回 Permanent（驱动 Term，不 nak 到 MaxDeliver 占着共享 durable 的 MaxAckPending），并计 saga.consumer.rejected_total。
// 历史：RR-20261006-44。
func TestSagaConsumersTermEveryBadEnvelopeAndAlarm(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	command := Command{ID: "cmd-1", IdempotencyKey: "idem-1", SagaID: "saga1", SagaType: "order", DefinitionVersion: 1, BusinessKey: "bk-1", StepName: "charge", Phase: PhaseForward, Attempt: 1, Topic: "charge", DeadlineAt: now.Add(time.Hour), CreatedAt: now}
	invalid := command
	invalid.Attempt = 0
	commandBody := func(version uint16, command Command) []byte {
		raw, _ := json.Marshal(commandEnvelope{Version: version, Command: command})
		return raw
	}
	completion := Completion{CommandID: "cmd-1", IdempotencyKey: "idem-1", SagaID: "saga1", Success: true, CompletedAt: now}
	completionBody := func(version uint16, completion Completion) []byte {
		raw, _ := json.Marshal(completionEnvelope{Version: version, Completion: completion})
		return raw
	}
	effectBody := func(topic string, payload []byte) []byte {
		raw, _ := json.Marshal(nestwal.EffectEnvelope{EffectID: "effect-1", Topic: topic, Payload: payload})
		return raw
	}
	badCompletion := completion
	badCompletion.CommandID = ""
	badCompletionPayload, _ := json.Marshal(completionEffectPayload{Version: WireVersion, Completion: badCompletion})

	engine, _ := idleEngine(t)
	handled := 0
	stepHandler := func(context.Context, Command) (Completion, error) { handled++; return Completion{}, nil }
	subscribe := func(t *testing.T, install func(*startJetStream, *JetStreamPublisher) error) fnats.JetStreamHandler {
		t.Helper()
		js := &startJetStream{}
		transport, err := NewJetStreamPublisher(js, "roost.saga")
		if err != nil {
			t.Fatal(err)
		}
		if err := install(js, transport); err != nil {
			t.Fatal(err)
		}
		return js.handler
	}
	consumers := []struct {
		name    string
		handler func(*testing.T) fnats.JetStreamHandler
		bad     map[string][]byte
	}{
		{"native_step", func(t *testing.T) fnats.JetStreamHandler {
			inbox, err := NewDataEngineStepInbox(mongotest.NewClient(), "game", DataEngineStepInboxOptions{Owner: "worker", LeaseDuration: 2 * time.Minute})
			if err != nil {
				t.Fatal(err)
			}
			return subscribe(t, func(js *startJetStream, transport *JetStreamPublisher) error {
				_, err := SubscribeDataEngineStep(ctx, js, transport, inbox, StepConsumerConfig{Stream: "SAGA", Durable: "native", Topic: "charge"}, stepHandler)
				return err
			})
		}, map[string][]byte{"oversized": make([]byte, maxWireEnvelopeBytes+1), "corrupt json": []byte("{"), "foreign version": commandBody(WireVersion+1, command), "invalid command": commandBody(WireVersion, invalid)}},
		{"mongo_step", func(t *testing.T) fnats.JetStreamHandler {
			inbox, err := NewMongoCommandInbox(mongotest.NewClient(), "game", "")
			if err != nil {
				t.Fatal(err)
			}
			return subscribe(t, func(js *startJetStream, transport *JetStreamPublisher) error {
				_, err := SubscribeMongoStep(ctx, js, transport, inbox, StepConsumerConfig{Stream: "SAGA", Durable: "mongo", Topic: "charge"}, stepHandler)
				return err
			})
		}, map[string][]byte{"oversized": make([]byte, maxWireEnvelopeBytes+1), "corrupt json": []byte("{"), "foreign version": commandBody(WireVersion+1, command), "invalid command": commandBody(WireVersion, invalid)}},
		{"result", func(t *testing.T) fnats.JetStreamHandler {
			return subscribe(t, func(js *startJetStream, _ *JetStreamPublisher) error {
				_, err := SubscribeCompletions(ctx, js, CompletionConsumerConfig{Stream: "SAGA", Durable: "result", SubjectPrefix: "roost.saga"}, engine)
				return err
			})
		}, map[string][]byte{"oversized": make([]byte, maxWireEnvelopeBytes+1), "corrupt json": []byte("{"), "foreign version": completionBody(WireVersion+1, completion), "invalid completion": completionBody(WireVersion, badCompletion)}},
		{"native_result", func(t *testing.T) fnats.JetStreamHandler {
			return subscribe(t, func(js *startJetStream, _ *JetStreamPublisher) error {
				_, err := SubscribeNestCompletions(ctx, js, NestCompletionConsumerConfig{Stream: "EFFECTS", Durable: "native-result", EffectPrefix: "roost.effect"}, engine)
				return err
			})
		}, map[string][]byte{"oversized": make([]byte, maxWireEnvelopeBytes+1), "corrupt json": []byte("{"), "foreign topic": effectBody("other", nil), "invalid completion": effectBody(CompletionEffectTopicPrefix+"saga1", badCompletionPayload)}},
		{"start", func(t *testing.T) fnats.JetStreamHandler {
			return subscribe(t, func(js *startJetStream, _ *JetStreamPublisher) error {
				_, err := SubscribeNestStarts(ctx, js, NestStartConsumerConfig{Stream: "EFFECTS", Durable: "start", EffectPrefix: "roost.effect"}, engine)
				return err
			})
		}, map[string][]byte{"oversized": make([]byte, maxWireEnvelopeBytes+1), "corrupt json": []byte("{"), "foreign topic": effectBody("other", nil), "undecodable start": effectBody(StartEffectTopic, []byte(`{"version":99}`))}},
	}
	for _, consumer := range consumers {
		t.Run(consumer.name, func(t *testing.T) {
			handler := consumer.handler(t)
			for name, body := range consumer.bad {
				before := counterValue("saga.consumer.rejected_total")
				err := handler(ctx, &fnats.JetStreamMsg{Subject: "test", Data: body})
				if err == nil || !fnats.IsPermanent(err) {
					t.Errorf("%s: bad envelope (%s) = %v (permanent=%v), want a permanent refusal (Term) instead of nak until MaxDeliver", consumer.name, name, err, fnats.IsPermanent(err))
				}
				if grown := counterValue("saga.consumer.rejected_total") - before; grown != 1 {
					t.Errorf("%s: bad envelope (%s) grew saga.consumer.rejected_total by %d, want 1", consumer.name, name, grown)
				}
			}
			if err := handler(ctx, nil); err == nil || !fnats.IsPermanent(err) {
				t.Errorf("%s: nil message = %v, want permanent", consumer.name, err)
			}
		})
	}
	if handled != 0 {
		t.Fatalf("a bad envelope reached a step handler %d time(s)", handled)
	}
}

// 两个步骤消费者与 Nest 启动消费者同样拒绝不安全配置；被拒的配置不会走到 Subscribe。
func TestSubscribeMongoStepRefusesEachUnsafeConfig(t *testing.T) {
	valid := StepConsumerConfig{Stream: "ROOST_SAGA", Durable: "game-reserve", Topic: "reserve"}
	cases := []struct {
		name   string
		mutate func(*StepConsumerConfig)
		want   string
	}{
		{"blank stream", func(c *StepConsumerConfig) { c.Stream = "" }, "invalid step consumer configuration"},
		{"blank durable", func(c *StepConsumerConfig) { c.Durable = "" }, "invalid step consumer configuration"},
		{"topic with wildcard", func(c *StepConsumerConfig) { c.Topic = "reserve.>" }, "invalid step consumer configuration"},
		{"max deliver above broker cap", func(c *StepConsumerConfig) { c.MaxDeliver = 1_000_001 }, "unsafe step consumer limits"},
		{"max ack pending above broker cap", func(c *StepConsumerConfig) { c.MaxAckPending = 65_537 }, "unsafe step consumer limits"},
		{"nak backoff above a day", func(c *StepConsumerConfig) { c.NakBackoffMin, c.NakBackoffMax = time.Second, 25*time.Hour }, "unsafe step consumer limits"},
	}
	handler := func(context.Context, Command) (Completion, error) { return Completion{}, nil }
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := &startJetStream{}
			transport, err := NewJetStreamPublisher(client, "roost.saga")
			if err != nil {
				t.Fatal(err)
			}
			inbox, err := NewMongoCommandInbox(mongotest.NewClient(), "game", "")
			if err != nil {
				t.Fatal(err)
			}
			cfg := valid
			tc.mutate(&cfg)
			_, err = SubscribeMongoStep(context.Background(), client, transport, inbox, cfg, handler)
			expectKitErr(t, err, tc.want)
			if client.handler != nil {
				t.Fatal("a refused config must not subscribe")
			}
		})
	}
	client := &startJetStream{}
	transport, _ := NewJetStreamPublisher(client, "roost.saga")
	inbox, _ := NewMongoCommandInbox(mongotest.NewClient(), "game", "")
	for name, call := range map[string]func() error{
		"nil client": func() error {
			_, err := SubscribeMongoStep(context.Background(), nil, transport, inbox, valid, handler)
			return err
		},
		"nil transport": func() error {
			_, err := SubscribeMongoStep(context.Background(), client, nil, inbox, valid, handler)
			return err
		},
		"nil inbox": func() error {
			_, err := SubscribeMongoStep(context.Background(), client, transport, nil, valid, handler)
			return err
		},
		"nil handler": func() error {
			_, err := SubscribeMongoStep(context.Background(), client, transport, inbox, valid, nil)
			return err
		},
	} {
		t.Run(name, func(t *testing.T) { expectKitErr(t, call(), "invalid step consumer configuration") })
	}
	if _, err := SubscribeMongoStep(context.Background(), client, transport, inbox, valid, handler); err != nil {
		t.Fatalf("valid config refused: %v", err)
	}
}

func TestSubscribeDataEngineStepRefusesEachUnsafeConfig(t *testing.T) {
	valid := StepConsumerConfig{Stream: "ROOST_SAGA", Durable: "game-reserve", Topic: "reserve", AckWait: 30 * time.Second}
	newInbox := func(t *testing.T) *DataEngineStepInbox {
		inbox, err := NewDataEngineStepInbox(mongotest.NewClient(), "game", DataEngineStepInboxOptions{Owner: "game-1", LeaseDuration: 2 * time.Minute})
		if err != nil {
			t.Fatal(err)
		}
		return inbox
	}
	handler := func(context.Context, Command) (Completion, error) { return Completion{}, nil }
	cases := []struct {
		name   string
		mutate func(*StepConsumerConfig)
		want   string
	}{
		{"blank stream", func(c *StepConsumerConfig) { c.Stream = "" }, "invalid dataengine step consumer configuration"},
		{"topic with wildcard", func(c *StepConsumerConfig) { c.Topic = "reserve.*" }, "invalid dataengine step consumer configuration"},
		{"max deliver above broker cap", func(c *StepConsumerConfig) { c.MaxDeliver = 1_000_001 }, "unsafe dataengine step consumer limits"},
		{"lease not longer than ack wait", func(c *StepConsumerConfig) { c.AckWait = 2 * time.Minute }, "unsafe dataengine step consumer limits"},
		{"receipt waiters above max ack pending", func(c *StepConsumerConfig) { c.MaxAckPending, c.ReceiptWaiters = 8, 9 }, "unsafe dataengine step consumer limits"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := &startJetStream{}
			transport, _ := NewJetStreamPublisher(client, "roost.saga")
			cfg := valid
			tc.mutate(&cfg)
			_, err := SubscribeDataEngineStep(context.Background(), client, transport, newInbox(t), cfg, handler)
			expectKitErr(t, err, tc.want)
			if client.handler != nil {
				t.Fatal("a refused config must not subscribe")
			}
		})
	}
	client := &startJetStream{}
	transport, _ := NewJetStreamPublisher(client, "roost.saga")
	if _, err := SubscribeDataEngineStep(context.Background(), client, transport, newInbox(t), valid, handler); err != nil {
		t.Fatalf("valid config refused: %v", err)
	}
	if _, err := NewDataEngineStepInbox(mongotest.NewClient(), "game", DataEngineStepInboxOptions{}); err == nil {
		t.Fatal("inbox without owner accepted")
	}
	if _, err := NewDataEngineStepInbox(nil, "game", DataEngineStepInboxOptions{Owner: "game-1"}); err == nil {
		t.Fatal("inbox without client accepted")
	}
}

// Replay 只把完成结果交还给赢得它的那条命令：同 ID 不同摘要是身份冲突，未知 ID 只是“没有可回放的”。
func TestMongoCommandInboxReplayRefusesForeignReceipt(t *testing.T) {
	if _, err := NewMongoCommandInbox(nil, "game", ""); err == nil {
		t.Fatal("inbox without client accepted")
	}
	if _, err := NewMongoCommandInbox(mongotest.NewClient(), " ", ""); err == nil {
		t.Fatal("inbox without database accepted")
	}
	inbox, err := NewMongoCommandInbox(mongotest.NewClient(), "game", "")
	if err != nil {
		t.Fatal(err)
	}
	handler := func(context.Context, Command) (Completion, error) {
		return Completion{Success: true, Data: []byte("ok")}, nil
	}
	now := time.Now()
	command := Command{ID: "one", IdempotencyKey: "op", SagaID: "saga", SagaType: "rally", DefinitionVersion: 1, BusinessKey: "r-3", Step: 0, StepName: "reserve", Phase: PhaseForward, Attempt: 1, Topic: "reserve", Payload: []byte("a"), CreatedAt: now, DeadlineAt: now.Add(time.Second)}
	if _, _, err := inbox.Handle(context.Background(), command, handler); err != nil {
		t.Fatal(err)
	}
	completion, found, err := inbox.Replay(context.Background(), command)
	if err != nil || !found || !completion.Success || string(completion.Data) != "ok" {
		t.Fatalf("replay of the committed command = %+v, %v, %v", completion, found, err)
	}
	foreign := command
	foreign.Payload = []byte("different")
	if _, _, err := inbox.Replay(context.Background(), foreign); !errors.Is(err, ErrIdentityConflict) {
		t.Fatalf("replay with a foreign digest err = %v, want ErrIdentityConflict", err)
	}
	unknown := command
	unknown.ID = "two"
	if _, found, err := inbox.Replay(context.Background(), unknown); err != nil || found {
		t.Fatalf("replay of an unknown command = %v, %v; want not found without error", found, err)
	}
	var nilInbox *MongoCommandInbox
	if _, _, err := nilInbox.Replay(context.Background(), command); !errors.Is(err, ErrInvalidRecord) {
		t.Fatalf("nil inbox replay err = %v", err)
	}
	if _, _, err := nilInbox.Handle(context.Background(), command, handler); !errors.Is(err, ErrInvalidRecord) {
		t.Fatalf("nil inbox handle err = %v", err)
	}
}

// 多进程：命令投递到不该执行它的进程时，必须在动任何状态之前拒绝。StepConsumerConfig.Admit 在 Reserve 之前调用；
// 它返回错误时不留下认领 / 回执、handler 不执行、错误原样返回（nak 交给下一个消费者），否则错误进程拿走租约，
// 真正的所有者要等租约过期。历史：game-demo 实跑（礼物 saga 248 次投递、24 条卡住）。
func TestStepConsumersAdmitBeforeTakingTheClaim(t *testing.T) {
	refused := errors.New("not this process's player")
	now := time.Now()
	command := Command{
		ID: "cmd-1", IdempotencyKey: "op-1", SagaID: "saga-1", SagaType: "gift", DefinitionVersion: 1,
		BusinessKey: "gift-1", Step: 0, StepName: "debit", Phase: PhaseForward, Attempt: 1,
		Topic: "debit", Payload: []byte("x"), CreatedAt: now, DeadlineAt: now.Add(time.Minute),
	}
	raw, err := json.Marshal(commandEnvelope{Version: WireVersion, Command: command})
	if err != nil {
		t.Fatal(err)
	}

	t.Run("dataengine", func(t *testing.T) {
		mongoClient := mongotest.NewClient()
		inbox, err := NewDataEngineStepInbox(mongoClient, "game", DataEngineStepInboxOptions{Owner: "game-2", LeaseDuration: 2 * time.Minute})
		if err != nil {
			t.Fatal(err)
		}
		client := &startJetStream{}
		transport, _ := NewJetStreamPublisher(client, "roost.saga")
		var ran bool
		handler := func(context.Context, Command) (Completion, error) {
			ran = true
			return Completion{}, nil
		}
		config := StepConsumerConfig{
			Stream: "ROOST_SAGA", Durable: "game-gift-debit", Topic: "debit", AckWait: 30 * time.Second,
			Admit: func(_ context.Context, c Command) error {
				if c.ID != command.ID {
					t.Fatalf("admit saw command %q", c.ID)
				}
				return refused
			},
		}
		if _, err := SubscribeDataEngineStep(context.Background(), client, transport, inbox, config, handler); err != nil {
			t.Fatal(err)
		}
		err = client.handler(context.Background(), &fnats.JetStreamMsg{Data: raw})
		if !errors.Is(err, refused) {
			t.Fatalf("delivery to a process that may not run the command = %v, want the admission refusal", err)
		}
		if ran {
			t.Fatal("the handler ran on a process that was refused admission")
		}
		if count := mongoClient.Collection("game", dataEngineOperationCollection).Len(); count != 0 {
			t.Fatalf("a refused delivery left %d claim(s) behind; the owner cannot run the command until they expire", count)
		}
	})

	t.Run("mongo", func(t *testing.T) {
		mongoClient := mongotest.NewClient()
		inbox, err := NewMongoCommandInbox(mongoClient, "game", "")
		if err != nil {
			t.Fatal(err)
		}
		client := &startJetStream{}
		transport, _ := NewJetStreamPublisher(client, "roost.saga")
		var ran bool
		handler := func(context.Context, Command) (Completion, error) {
			ran = true
			return Completion{Success: true}, nil
		}
		config := StepConsumerConfig{
			Stream: "ROOST_SAGA", Durable: "game-gift-deliver", Topic: "debit",
			Admit: func(context.Context, Command) error { return refused },
		}
		if _, err := SubscribeMongoStep(context.Background(), client, transport, inbox, config, handler); err != nil {
			t.Fatal(err)
		}
		err = client.handler(context.Background(), &fnats.JetStreamMsg{Data: raw})
		if !errors.Is(err, refused) {
			t.Fatalf("delivery to a process that may not run the command = %v, want the admission refusal", err)
		}
		if ran {
			t.Fatal("the handler ran on a process that was refused admission")
		}
		if _, found, err := inbox.Replay(context.Background(), command); err != nil || found {
			t.Fatalf("a refused delivery left a receipt: found=%v err=%v", found, err)
		}
	})

	// An admitting Admit changes nothing: the command runs as before.
	t.Run("admitted", func(t *testing.T) {
		mongoClient := mongotest.NewClient()
		inbox, _ := NewMongoCommandInbox(mongoClient, "game", "")
		client := &startJetStream{}
		transport, _ := NewJetStreamPublisher(client, "roost.saga")
		var ran bool
		handler := func(context.Context, Command) (Completion, error) {
			ran = true
			return Completion{Success: true}, nil
		}
		config := StepConsumerConfig{
			Stream: "ROOST_SAGA", Durable: "game-gift-deliver", Topic: "debit",
			Admit: func(context.Context, Command) error { return nil },
		}
		if _, err := SubscribeMongoStep(context.Background(), client, transport, inbox, config, handler); err != nil {
			t.Fatal(err)
		}
		if err := client.handler(context.Background(), &fnats.JetStreamMsg{Data: raw}); err != nil {
			t.Fatal(err)
		}
		if !ran {
			t.Fatal("an admitted command did not run")
		}
	})
}

// 已过截止、又没有回执的步骤命令被终结（ack），不无限 nak：截止后协调器自己按超时推进，不再等这条旧消息。
// 无回执时返回 nil，不调用 Admit、不 Reserve、不执行 handler、不发布 completion，并计 saga.step.expired_unexecuted_total；
// 有回执时原生路径 ack、Mongo 路径重发已提交的 completion；读回执遇到暂时性错误仍返回错误交给重投（可能有回执）。
// 历史：U-0281（演练中共享 durable 被过期旧尝试占满 MaxAckPending）。
func TestExpiredStepCommandWithoutReceiptIsAcknowledgedNotRedelivered(t *testing.T) {
	now := time.Now().UTC()
	expired := Command{
		ID: "gift-1:1:0:2", IdempotencyKey: "gift-1:1:0", SagaID: "gift-1", SagaType: "gift_item", DefinitionVersion: 1,
		BusinessKey: "gift-1", Step: 0, StepName: "debit", Phase: PhaseForward, Attempt: 2,
		Topic: "gift_item.debit", Payload: []byte("x"), CreatedAt: now.Add(-time.Minute), DeadlineAt: now.Add(-time.Second),
	}
	raw, err := json.Marshal(commandEnvelope{Version: WireVersion, Command: expired})
	if err != nil {
		t.Fatal(err)
	}
	refuseEverywhere := func(context.Context, Command) error {
		t.Fatal("Admit was asked about an expired command; admission must not decide whether a stale message lives on")
		return nil
	}
	noHandler := func(context.Context, Command) (Completion, error) {
		t.Fatal("the handler ran for a command past its deadline")
		return Completion{}, nil
	}

	t.Run("dataengine without receipt", func(t *testing.T) {
		mongoClient := mongotest.NewClient()
		inbox, err := NewDataEngineStepInbox(mongoClient, "game", DataEngineStepInboxOptions{Owner: "game-1", LeaseDuration: 2 * time.Minute})
		if err != nil {
			t.Fatal(err)
		}
		client := &startJetStream{}
		transport, _ := NewJetStreamPublisher(client, "roost.saga")
		config := StepConsumerConfig{Stream: "ROOST_SAGA", Durable: "game-gift-debit", Topic: "gift_item.debit", AckWait: 30 * time.Second, Admit: refuseEverywhere}
		if _, err := SubscribeDataEngineStep(context.Background(), client, transport, inbox, config, noHandler); err != nil {
			t.Fatal(err)
		}
		before := expiredUnexecuted()
		if err := client.handler(context.Background(), &fnats.JetStreamMsg{Data: raw}); err != nil {
			t.Fatalf("expired command without a receipt = %v, want nil (ack): any error is nak'd and redelivered until MaxDeliver, holding a MaxAckPending slot all the while", err)
		}
		if grown := expiredUnexecuted() - before; grown != 1 {
			t.Fatalf("saga.step.expired_unexecuted_total grew by %d, want 1: an acknowledged-without-running command must be countable", grown)
		}
		if count := mongoClient.Collection("game", dataEngineOperationCollection).Len(); count != 0 {
			t.Fatalf("an expired command left %d claim(s) behind", count)
		}
		if client.subject != "" {
			t.Fatalf("an expired command without a receipt published %q", client.subject)
		}
	})

	t.Run("dataengine with receipt", func(t *testing.T) {
		mongoClient := mongotest.NewClient()
		inbox, _ := NewDataEngineStepInbox(mongoClient, "game", DataEngineStepInboxOptions{Owner: "game-1", LeaseDuration: 2 * time.Minute})
		completion := Completion{CommandID: expired.ID, IdempotencyKey: expired.IdempotencyKey, SagaID: expired.SagaID, Success: true, CompletedAt: now}
		effect, _ := NewCompletionEffect(completion)
		if err := inboxReceipts(mongoClient).Seed(dataEngineReceipt{ID: dataEngineStepNamespace + "/" + expired.ID, Digest: mustCommandDigest(t, expired), Payload: effect.Payload}); err != nil {
			t.Fatal(err)
		}
		client := &startJetStream{}
		transport, _ := NewJetStreamPublisher(client, "roost.saga")
		config := StepConsumerConfig{Stream: "ROOST_SAGA", Durable: "game-gift-debit", Topic: "gift_item.debit", AckWait: 30 * time.Second, Admit: refuseEverywhere}
		if _, err := SubscribeDataEngineStep(context.Background(), client, transport, inbox, config, noHandler); err != nil {
			t.Fatal(err)
		}
		if err := client.handler(context.Background(), &fnats.JetStreamMsg{Data: raw}); err != nil {
			t.Fatalf("expired command with a receipt = %v, want nil", err)
		}
	})

	t.Run("dataengine receipt unreadable", func(t *testing.T) {
		mongoClient := mongotest.NewClient()
		inbox, _ := NewDataEngineStepInbox(mongoClient, "game", DataEngineStepInboxOptions{Owner: "game-1", LeaseDuration: 2 * time.Minute})
		outage := errors.New("mongo unavailable")
		inboxReceipts(mongoClient).Errors["FindOne"] = outage
		client := &startJetStream{}
		transport, _ := NewJetStreamPublisher(client, "roost.saga")
		config := StepConsumerConfig{Stream: "ROOST_SAGA", Durable: "game-gift-debit", Topic: "gift_item.debit", AckWait: 30 * time.Second, Admit: refuseEverywhere}
		if _, err := SubscribeDataEngineStep(context.Background(), client, transport, inbox, config, noHandler); err != nil {
			t.Fatal(err)
		}
		if err := client.handler(context.Background(), &fnats.JetStreamMsg{Data: raw}); !errors.Is(err, outage) {
			t.Fatalf("expired command whose receipt could not be read = %v, want the read error (redelivered: a receipt may exist)", err)
		}
	})

	t.Run("mongo without receipt", func(t *testing.T) {
		mongoClient := mongotest.NewClient()
		inbox, err := NewMongoCommandInbox(mongoClient, "saga", "_game_gift_step_inbox")
		if err != nil {
			t.Fatal(err)
		}
		client := &startJetStream{}
		transport, _ := NewJetStreamPublisher(client, "roost.saga")
		config := StepConsumerConfig{Stream: "ROOST_SAGA", Durable: "game-gift-deliver", Topic: "gift_item.debit", Admit: refuseEverywhere}
		if _, err := SubscribeMongoStep(context.Background(), client, transport, inbox, config, noHandler); err != nil {
			t.Fatal(err)
		}
		if err := client.handler(context.Background(), &fnats.JetStreamMsg{Data: raw}); err != nil {
			t.Fatalf("expired command without a receipt = %v, want nil (ack)", err)
		}
		if client.subject != "" {
			t.Fatalf("an expired command without a receipt published %q", client.subject)
		}
		if count := mongoClient.Collection("saga", "_game_gift_step_inbox").Len(); count != 0 {
			t.Fatalf("an expired command left %d receipt(s) behind", count)
		}
	})

	t.Run("mongo with receipt", func(t *testing.T) {
		mongoClient := mongotest.NewClient()
		inbox, _ := NewMongoCommandInbox(mongoClient, "saga", "_game_gift_step_inbox")
		completion := Completion{CommandID: expired.ID, IdempotencyKey: expired.IdempotencyKey, SagaID: expired.SagaID, Success: true, CompletedAt: now}
		encoded, _ := json.Marshal(completion)
		if err := mongoClient.Collection("saga", "_game_gift_step_inbox").Seed(commandReceiptDoc{ID: expired.ID, Digest: mustCommandDigest(t, expired), Completion: encoded, CreatedAt: now}); err != nil {
			t.Fatal(err)
		}
		client := &startJetStream{}
		transport, _ := NewJetStreamPublisher(client, "roost.saga")
		config := StepConsumerConfig{Stream: "ROOST_SAGA", Durable: "game-gift-deliver", Topic: "gift_item.debit", Admit: refuseEverywhere}
		if _, err := SubscribeMongoStep(context.Background(), client, transport, inbox, config, noHandler); err != nil {
			t.Fatal(err)
		}
		if err := client.handler(context.Background(), &fnats.JetStreamMsg{Data: raw}); err != nil {
			t.Fatalf("expired command with a receipt = %v, want nil", err)
		}
		if !strings.HasPrefix(client.subject, "roost.saga.result.") {
			t.Fatalf("an expired command with a committed completion published %q, want the completion replayed to the coordinator", client.subject)
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
