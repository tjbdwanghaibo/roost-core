package saga

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	fnats "github.com/tjbdwanghaibo/cube-core/nats"
	coresaga "github.com/tjbdwanghaibo/cube-core/saga"
	"github.com/tjbdwanghaibo/cube-kit/nestwal"
)

type startCapture struct {
	requests []coresaga.StartRequest
}

func (s *startCapture) StartSaga(_ context.Context, request coresaga.StartRequest) (coresaga.Record, error) {
	s.requests = append(s.requests, request)
	return coresaga.Record{}, nil
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
	effect, err := coresaga.NewStartEffect(coresaga.StartRequest{Type: "rally", DefinitionVersion: 1, BusinessKey: "r-1", Data: []byte("state")})
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

func TestNestStartRejectsWrongEffectTopic(t *testing.T) {
	capture := &startCapture{}
	raw, _ := json.Marshal(nestwal.EffectEnvelope{EffectID: "e-1", Topic: "other", Payload: []byte(`{}`)})
	if err := handleNestStart(context.Background(), &fnats.JetStreamMsg{Data: raw}, capture); err == nil {
		t.Fatal("expected invalid record")
	}
	if len(capture.requests) != 0 {
		t.Fatal("invalid effect reached starter")
	}
}

func TestJetStreamPublisherUsesVersionedEnvelope(t *testing.T) {
	client := &startJetStream{}
	publisher, err := NewJetStreamPublisher(client, "roost.saga")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	command := coresaga.Command{ID: "s:1:0:1", IdempotencyKey: "s:1:0", SagaID: "s", SagaType: "rally", DefinitionVersion: 1, BusinessKey: "r-1", Step: 0, StepName: "reserve", Phase: coresaga.PhaseForward, Attempt: 1, Topic: "rally.reserve", DeadlineAt: now.Add(time.Second), CreatedAt: now}
	if err := publisher.PublishSagaCommand(context.Background(), command); err != nil {
		t.Fatal(err)
	}
	var envelope commandEnvelope
	if err := json.Unmarshal(client.data, &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Version != coresaga.WireVersion || envelope.Command.ID != command.ID || client.msgID != command.ID || client.subject != "roost.saga.command.rally.reserve" {
		t.Fatalf("envelope=%+v subject=%s msgID=%s", envelope, client.subject, client.msgID)
	}
}
