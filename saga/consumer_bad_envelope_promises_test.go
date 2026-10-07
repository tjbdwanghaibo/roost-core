package saga

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/mongo/mongotest"
	fnats "github.com/tjbdwanghaibo/roost-core/nats"
	"github.com/tjbdwanghaibo/roost-core/nestwal"
)

// RR-20261006-44（F06-S2）：五个 saga 消费者对坏信封同一口径——Term（Permanent）并告警（saga.consumer.rejected_total{consumer}
// 与 ERROR 日志）。
//
// 旧行为：原生步骤消费者（SubscribeDataEngineStep）的 decodeStepCommand 对超长帧、损坏的 JSON、未知 WireVersion、
// Command.Validate 失败返回普通错误，驱动按非永久错误 nak，直到 MaxDeliver（缺省 25000、退避封顶 30s，约 8.7 天），
// 期间占着这个共享 durable 的 MaxAckPending 名额；另外四个消费者直接 Term，但都没有指标。SAGA.md 写的是“进入受退避约束的
// 重新投递”，只与原生步骤消费者一致。
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
