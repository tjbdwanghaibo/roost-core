package saga

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	fnats "github.com/tjbdwanghaibo/roost-core/nats"
	kitnats "github.com/tjbdwanghaibo/roost-core/nats"
	"github.com/tjbdwanghaibo/roost-core/nestwal"
)

// NestCompletionConsumerConfig configures the durable consumer that turns a
// native step's transactional completion effect into Engine.Complete.
//
// A native Nest step (SubscribeDataEngineStep + inbox.Bind + EmitCompletion)
// commits its completion as a Nest effect, so it travels the Data Engine
// outbox and arrives on the EFFECT stream as
// `<EffectPrefix>.saga.result.<sagaID>` — not on the saga stream the
// ordinary completion consumer watches. Without this consumer nothing read
// it: the saga stayed waiting, the step was redelivered on timeout, and the
// inbox replayed the same receipt forever (RR-20260917-07).
//
// All coordinator replicas in one logical service share the Durable, so one
// of them completes each saga.
type NestCompletionConsumerConfig struct {
	Stream         string
	Durable        string
	EffectPrefix   string
	AckWait        time.Duration
	ProcessTimeout time.Duration
	MaxDeliver     int
	MaxAckPending  int
	NakBackoffMin  time.Duration
	NakBackoffMax  time.Duration
}

// Completer is the half of the Engine this consumer drives.
type Completer interface {
	Complete(context.Context, Completion) (Record, error)
}

// SubscribeNestCompletions installs that consumer. It is the mirror of
// SubscribeNestStarts: same stream, same prefix, its own durable and its own
// topic.
func SubscribeNestCompletions(ctx context.Context, client fnats.IJetStream, config NestCompletionConsumerConfig, completer Completer) (fnats.IJetStreamSubscription, error) {
	if client == nil || completer == nil {
		return nil, fmt.Errorf("saga: Nest completion subscriber dependencies are required")
	}
	config.Stream = strings.TrimSpace(config.Stream)
	config.Durable = strings.TrimSpace(config.Durable)
	config.EffectPrefix = strings.Trim(strings.TrimSpace(config.EffectPrefix), ".")
	if config.Stream == "" || config.Durable == "" || !validSubjectPath(config.EffectPrefix) {
		return nil, fmt.Errorf("saga: Nest completion stream, durable and effect prefix are required")
	}
	if config.AckWait <= 0 {
		config.AckWait = 30 * time.Second
	}
	if config.ProcessTimeout <= 0 {
		config.ProcessTimeout = 10 * time.Second
	}
	if config.MaxDeliver <= 0 {
		config.MaxDeliver = 25_000
	}
	if config.MaxAckPending <= 0 {
		config.MaxAckPending = 256
	}
	if config.NakBackoffMin <= 0 {
		config.NakBackoffMin = 250 * time.Millisecond
	}
	if config.NakBackoffMax < config.NakBackoffMin {
		config.NakBackoffMax = 30 * time.Second
	}
	if config.ProcessTimeout >= config.AckWait || !validDeliveryLimits(config.MaxDeliver, config.MaxAckPending, config.NakBackoffMin, config.NakBackoffMax) {
		return nil, fmt.Errorf("saga: unsafe Nest completion consumer limits")
	}
	// One subject per saga id, so the filter is a wildcard — unlike the start
	// topic, which is a single subject.
	return client.Subscribe(ctx, fnats.JetStreamConsumerConfig{
		Stream:        config.Stream,
		Name:          config.Durable,
		Durable:       config.Durable,
		FilterSubject: config.EffectPrefix + "." + strings.TrimSuffix(CompletionEffectTopicPrefix, ".") + ".>",
		DeliverPolicy: fnats.JetStreamDeliverAll,
		AckWait:       config.AckWait,
		MaxDeliver:    config.MaxDeliver,
		MaxAckPending: config.MaxAckPending,
		NakBackoffMin: config.NakBackoffMin,
		NakBackoffMax: config.NakBackoffMax,
	}, func(messageCtx context.Context, message *fnats.JetStreamMsg) error {
		processCtx, cancel := context.WithTimeout(messageCtx, config.ProcessTimeout)
		err := handleNestCompletion(processCtx, message, completer)
		cancel()
		if err != nil {
			logConsumerError("Nest completion", message, err)
		}
		return err
	})
}

func handleNestCompletion(ctx context.Context, message *fnats.JetStreamMsg, completer Completer) error {
	if message == nil || completer == nil {
		return kitnats.Permanent(ErrInvalidRecord)
	}
	if len(message.Data) > maxWireEnvelopeBytes {
		return kitnats.Permanent(ErrInvalidRecord)
	}
	var envelope nestwal.EffectEnvelope
	if err := json.Unmarshal(message.Data, &envelope); err != nil {
		return kitnats.Permanent(err)
	}
	if envelope.EffectID == "" || !strings.HasPrefix(envelope.Topic, CompletionEffectTopicPrefix) {
		return kitnats.Permanent(ErrInvalidRecord)
	}
	completion, err := DecodeCompletionEffect(envelope.Payload)
	if err != nil {
		return kitnats.Permanent(err)
	}
	_, err = completer.Complete(ctx, completion)
	if err == nil {
		return nil
	}
	// A completion for a saga this coordinator no longer owns, or for a step
	// that has already moved on, is not a delivery to retry: the effect
	// stream keeps it until it is acked, and redelivering it forever would
	// stall the consumer behind one stale message. Engine.Complete is
	// idempotent for a recorded receipt, so reaching ErrNotWaiting means the
	// result is already accounted for.
	if isTerminalCompletionError(err) {
		return kitnats.Permanent(err)
	}
	return err
}

// isTerminalCompletionError names the outcomes a redelivery cannot improve.
func isTerminalCompletionError(err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, ErrNotWaiting), errors.Is(err, ErrNotFound), errors.Is(err, ErrInvalidRecord), errors.Is(err, ErrDefinitionMissing):
		return true
	default:
		return false
	}
}

var _ Completer = (*Engine)(nil)
