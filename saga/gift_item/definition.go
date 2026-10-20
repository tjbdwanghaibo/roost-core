package gift_item

import (
	"context"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/saga"
)

const (
	Type           = "gift_item"
	Version uint32 = 1
)

// The step topics. steps.go (generated next to this file) registers a
// consumer for each of them with roost-core wiring/saga Steps: Steps.Native
// for a step whose business is a Nest transaction (its Nest handler takes a
// saga.NativeStep and calls step.Complete inside the transaction), Steps.Mongo
// for any other step. Both inboxes let at most one attempt of an operation
// take effect (roost-core SAGA.md, 原生步骤执行契约 / Mongo 步骤). For a Mongo
// step that covers what its handler writes through the Mongo transaction it
// is given; a call into another service is outside that transaction and still
// has to be idempotent by Command.IdempotencyKey.
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
// roost-core docs/framework/SAGA.md, step budgets). One operation of a step may take up
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
// A saga is identified by (Type, businessKey) — one saga per key — and its id is
// assigned when the coordinator picks the intent up, so make businessKey
// something the initiator can hand back to the client (a request id, say): the
// client later asks with it and Lookup finds the outcome.
func EmitStart(businessKey string, state []byte, deadline time.Time) error {
	return saga.EmitStart(saga.StartRequest{Type: Type, DefinitionVersion: Version, BusinessKey: businessKey, Data: state, DeadlineAt: deadline})
}

// Lookup finds the saga EmitStart / Start started for businessKey on the
// process hosting the coordinator (app.Lookup[*saga.Engine](registry, mods.ModSaga)).
// saga.ErrNotFound means it has not started yet: the intent is still on its way
// through the outbox, so treat it as pending, not as a failure. A terminal
// record.Status (Completed / Compensated / Failed) is the outcome.
func Lookup(ctx context.Context, engine *saga.Engine, businessKey string) (saga.Record, error) {
	return engine.GetByBusinessKey(ctx, Type, businessKey)
}

// Start is for durable consumers, administration and recovery paths which are
// already outside a Nest transaction. Calls are idempotent for one intent.
func Start(ctx context.Context, engine *saga.Engine, businessKey string, state []byte, deadline time.Time) (saga.Record, error) {
	return engine.StartSaga(ctx, saga.StartRequest{Type: Type, DefinitionVersion: Version, BusinessKey: businessKey, Data: state, DeadlineAt: deadline})
}
