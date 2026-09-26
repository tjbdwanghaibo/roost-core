package gift_item

import (
	"context"
	"time"

	fnats "github.com/tjbdwanghaibo/roost-core/nats"
	"github.com/tjbdwanghaibo/roost-core/saga"
)

const (
	Type           = "gift_item"
	Version uint32 = 1
)

// The step topics. A step whose business is a Nest transaction subscribes
// with saga.SubscribeDataEngineStep and binds its receipt inside that
// transaction; one whose business is a call into another service uses the
// Mongo inbox helpers below. Both address the same topics.
const (
	// TopicDebit is the forward topic of step debit.
	TopicDebit = "gift_item.debit"
	// TopicDebitCompensation is its compensation topic.
	TopicDebitCompensation = "gift_item.debit.compensate"
	// TopicDeliver is the forward topic of step deliver.
	TopicDeliver = "gift_item.deliver"
	// TopicDeliverCompensation is its compensation topic.
	TopicDeliverCompensation = "gift_item.deliver.compensate"
)

func Definition() saga.Definition {
	return saga.Definition{Type: Type, Version: Version, Steps: []saga.Step{
		{Name: "debit", ForwardTopic: TopicDebit, CompensateTopic: TopicDebitCompensation, Timeout: 5 * time.Second, MaxAttempts: 5, BackoffMin: 100 * time.Millisecond, BackoffMax: 5 * time.Second},
		{Name: "deliver", ForwardTopic: TopicDeliver, CompensateTopic: TopicDeliverCompensation, Timeout: 5 * time.Second, MaxAttempts: 5, BackoffMin: 100 * time.Millisecond, BackoffMax: 5 * time.Second},
	}}
}

// Definitions must retain every version which still has non-terminal records.
// When changing step order or compensation semantics, keep the old definition
// here and make Definition return the new version.
func Definitions() []saga.Definition { return []saga.Definition{Definition()} }

func Register(engine *saga.Engine) error {
	for _, definition := range Definitions() {
		if err := engine.Register(definition); err != nil {
			return err
		}
	}
	return nil
}

// EmitStart is the production entry point from a Nest handler: the Saga start
// intent and current Entity mutations are committed in the same Nest WAL record.
func EmitStart(businessKey string, state []byte, deadline time.Time) error {
	return saga.EmitStart(saga.StartRequest{Type: Type, DefinitionVersion: Version, BusinessKey: businessKey, Data: state, DeadlineAt: deadline})
}

// Start is for durable consumers, administration and recovery paths which are
// already outside a Nest transaction. Calls are idempotent for one intent.
func Start(ctx context.Context, engine *saga.Engine, businessKey string, state []byte, deadline time.Time) (saga.Record, error) {
	return engine.StartSaga(ctx, saga.StartRequest{Type: Type, DefinitionVersion: Version, BusinessKey: businessKey, Data: state, DeadlineAt: deadline})
}

func SubscribeDebit(ctx context.Context, client fnats.IJetStream, transport *saga.JetStreamPublisher, inbox *saga.MongoCommandInbox, stream, durable string, handler saga.StepHandler) (fnats.IJetStreamSubscription, error) {
	return saga.SubscribeMongoStep(ctx, client, transport, inbox, saga.StepConsumerConfig{Stream: stream, Durable: durable, Topic: "gift_item.debit"}, handler)
}

func SubscribeDebitCompensation(ctx context.Context, client fnats.IJetStream, transport *saga.JetStreamPublisher, inbox *saga.MongoCommandInbox, stream, durable string, handler saga.StepHandler) (fnats.IJetStreamSubscription, error) {
	return saga.SubscribeMongoStep(ctx, client, transport, inbox, saga.StepConsumerConfig{Stream: stream, Durable: durable, Topic: "gift_item.debit.compensate"}, handler)
}

func SubscribeDeliver(ctx context.Context, client fnats.IJetStream, transport *saga.JetStreamPublisher, inbox *saga.MongoCommandInbox, stream, durable string, handler saga.StepHandler) (fnats.IJetStreamSubscription, error) {
	return saga.SubscribeMongoStep(ctx, client, transport, inbox, saga.StepConsumerConfig{Stream: stream, Durable: durable, Topic: "gift_item.deliver"}, handler)
}

func SubscribeDeliverCompensation(ctx context.Context, client fnats.IJetStream, transport *saga.JetStreamPublisher, inbox *saga.MongoCommandInbox, stream, durable string, handler saga.StepHandler) (fnats.IJetStreamSubscription, error) {
	return saga.SubscribeMongoStep(ctx, client, transport, inbox, saga.StepConsumerConfig{Stream: stream, Durable: durable, Topic: "gift_item.deliver.compensate"}, handler)
}
