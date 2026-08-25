package saga

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	fnats "github.com/tjbdwanghaibo/cube-core/nats"
	coresaga "github.com/tjbdwanghaibo/cube-core/saga"
	"github.com/tjbdwanghaibo/cube-kit/nestwal"
)

// NestStartConsumerConfig configures the shared durable consumer which turns
// transactional Nest effects into Saga records. All coordinator replicas in
// one logical service must use the same Durable value.
type NestStartConsumerConfig struct {
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

type Starter interface {
	StartSaga(context.Context, coresaga.StartRequest) (coresaga.Record, error)
}

func SubscribeNestStarts(ctx context.Context, client fnats.IJetStream, config NestStartConsumerConfig, starter Starter) (fnats.IJetStreamSubscription, error) {
	if client == nil || starter == nil {
		return nil, fmt.Errorf("saga: Nest start subscriber dependencies are required")
	}
	config.Stream = strings.TrimSpace(config.Stream)
	config.Durable = strings.TrimSpace(config.Durable)
	config.EffectPrefix = strings.Trim(strings.TrimSpace(config.EffectPrefix), ".")
	if config.Stream == "" || config.Durable == "" || !validSubjectPath(config.EffectPrefix) {
		return nil, fmt.Errorf("saga: Nest start stream, durable and effect prefix are required")
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
		return nil, fmt.Errorf("saga: unsafe Nest start consumer limits")
	}
	return client.Subscribe(ctx, fnats.JetStreamConsumerConfig{
		Stream:        config.Stream,
		Name:          config.Durable,
		Durable:       config.Durable,
		FilterSubject: config.EffectPrefix + "." + coresaga.StartEffectTopic,
		DeliverPolicy: fnats.JetStreamDeliverAll,
		AckWait:       config.AckWait,
		MaxDeliver:    config.MaxDeliver,
		MaxAckPending: config.MaxAckPending,
		NakBackoffMin: config.NakBackoffMin,
		NakBackoffMax: config.NakBackoffMax,
	}, func(messageCtx context.Context, message *fnats.JetStreamMsg) error {
		processCtx, cancel := context.WithTimeout(messageCtx, config.ProcessTimeout)
		err := handleNestStart(processCtx, message, starter)
		cancel()
		if err != nil {
			logConsumerError("Nest start", message, err)
		}
		return err
	})
}

func handleNestStart(ctx context.Context, message *fnats.JetStreamMsg, starter Starter) error {
	if message == nil || starter == nil {
		return coresaga.ErrInvalidRecord
	}
	if len(message.Data) > maxWireEnvelopeBytes {
		return coresaga.ErrInvalidRecord
	}
	var envelope nestwal.EffectEnvelope
	if err := json.Unmarshal(message.Data, &envelope); err != nil {
		return err
	}
	if envelope.EffectID == "" || envelope.Topic != coresaga.StartEffectTopic {
		return coresaga.ErrInvalidRecord
	}
	request, err := coresaga.DecodeStartEffect(envelope.Payload)
	if err != nil {
		return err
	}
	_, err = starter.StartSaga(ctx, request)
	return err
}

var _ Starter = (*coresaga.Engine)(nil)

func logConsumerError(kind string, message *fnats.JetStreamMsg, err error) {
	if message == nil {
		slog.Error("saga: consumer rejected nil message", "kind", kind, "err", err)
		return
	}
	slog.Error("saga: consumer processing failed", "kind", kind, "subject", message.Subject, "stream_sequence", message.StreamSeq, "deliveries", message.NumDelivered, "err", err)
}
