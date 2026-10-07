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
// transaction; any other step uses the Mongo inbox helpers below. Both
// address the same topics, and both inboxes let at most one attempt of an
// operation take effect (roost-core SAGA.md, 原生步骤执行契约 / Mongo 步骤).
// For a Mongo step that covers what its handler writes through the Mongo
// transaction it is given; a call into another service is outside that
// transaction and still has to be idempotent by Command.IdempotencyKey.
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

// Definition lists the steps. Their timeout and retry budget is configuration,
// not code: saga.step_defaults gives every step its Timeout, MaxAttempts and
// backoff, and saga.steps.gift_item.<step>.<field> overrides one step (see the
// roost-core USER_GUIDE, "Saga 步骤预算"). One operation of a step may take up
// to MaxAttempts attempts; the framework makes sure at most one of them takes
// effect (roost-core SAGA.md, 原生步骤执行契约; Mongo steps since 2026-10-06).
func Definition() saga.Definition {
	return saga.Definition{Type: Type, Version: Version, Steps: []saga.Step{
		{Name: "debit", ForwardTopic: TopicDebit, CompensateTopic: TopicDebitCompensation},
		{Name: "deliver", ForwardTopic: TopicDeliver, CompensateTopic: TopicDeliverCompensation},
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
