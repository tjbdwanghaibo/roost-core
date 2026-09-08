package saga

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	fnats "github.com/tjbdwanghaibo/roost-core/nats"
	coresaga "github.com/tjbdwanghaibo/roost-core/saga"
	kitnats "github.com/tjbdwanghaibo/roost-kit/nats"
)

const maxWireEnvelopeBytes = 8 << 20

type JetStreamPublisher struct {
	client fnats.IJetStream
	prefix string
}

type CompletionConsumerConfig struct {
	Stream, Durable, SubjectPrefix string
	AckWait                        time.Duration
	ProcessTimeout                 time.Duration
	MaxDeliver                     int
	MaxAckPending                  int
	NakBackoffMin                  time.Duration
	NakBackoffMax                  time.Duration
}

type commandEnvelope struct {
	Version uint16           `json:"version"`
	Command coresaga.Command `json:"command"`
}

type completionEnvelope struct {
	Version    uint16              `json:"version"`
	Completion coresaga.Completion `json:"completion"`
}

func NewJetStreamPublisher(client fnats.IJetStream, prefix string) (*JetStreamPublisher, error) {
	prefix = strings.Trim(strings.TrimSpace(prefix), ".")
	if client == nil || !validSubjectPath(prefix) {
		return nil, fmt.Errorf("saga: jetstream client and prefix are required")
	}
	return &JetStreamPublisher{client: client, prefix: prefix}, nil
}
func (p *JetStreamPublisher) PublishSagaCommand(ctx context.Context, command coresaga.Command) error {
	if err := command.Validate(); err != nil {
		return err
	}
	raw, err := json.Marshal(commandEnvelope{Version: coresaga.WireVersion, Command: command})
	if err != nil {
		return err
	}
	if len(raw) > maxWireEnvelopeBytes {
		return coresaga.ErrInvalidRecord
	}
	_, err = p.client.Publish(ctx, p.prefix+".command."+strings.Trim(command.Topic, "."), raw, fnats.JetStreamPublishOptions{MsgID: command.ID})
	return err
}
func (p *JetStreamPublisher) PublishCompletion(ctx context.Context, completion coresaga.Completion) error {
	if err := completion.Validate(); err != nil {
		return err
	}
	raw, err := json.Marshal(completionEnvelope{Version: coresaga.WireVersion, Completion: completion})
	if err != nil {
		return err
	}
	if len(raw) > maxWireEnvelopeBytes {
		return coresaga.ErrInvalidRecord
	}
	_, err = p.client.Publish(ctx, p.prefix+".result."+completion.SagaID, raw, fnats.JetStreamPublishOptions{MsgID: completion.CommandID + ":result"})
	return err
}

func SubscribeCompletions(ctx context.Context, client fnats.IJetStream, config CompletionConsumerConfig, engine *coresaga.Engine) (fnats.IJetStreamSubscription, error) {
	if client == nil || engine == nil {
		return nil, fmt.Errorf("saga: completion subscriber dependencies are required")
	}
	config.Stream = strings.TrimSpace(config.Stream)
	config.Durable = strings.TrimSpace(config.Durable)
	config.SubjectPrefix = strings.Trim(strings.TrimSpace(config.SubjectPrefix), ".")
	if config.Stream == "" || config.Durable == "" || !validSubjectPath(config.SubjectPrefix) {
		return nil, fmt.Errorf("saga: completion stream, durable and subject prefix are required")
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
		return nil, fmt.Errorf("saga: unsafe completion consumer limits")
	}
	return client.Subscribe(ctx, fnats.JetStreamConsumerConfig{Stream: config.Stream, Name: config.Durable, Durable: config.Durable, FilterSubject: config.SubjectPrefix + ".result.>", DeliverPolicy: fnats.JetStreamDeliverAll, AckWait: config.AckWait, MaxDeliver: config.MaxDeliver, MaxAckPending: config.MaxAckPending, NakBackoffMin: config.NakBackoffMin, NakBackoffMax: config.NakBackoffMax}, func(messageCtx context.Context, message *fnats.JetStreamMsg) error {
		if message == nil {
			return kitnats.Permanent(coresaga.ErrInvalidRecord)
		}
		if len(message.Data) > maxWireEnvelopeBytes {
			return kitnats.Permanent(coresaga.ErrInvalidRecord)
		}
		var envelope completionEnvelope
		if err := json.Unmarshal(message.Data, &envelope); err != nil {
			logConsumerError("completion decode", message, err)
			return kitnats.Permanent(err)
		}
		if envelope.Version != coresaga.WireVersion {
			return kitnats.Permanent(coresaga.ErrInvalidRecord)
		}
		completion := envelope.Completion
		if err := completion.Validate(); err != nil {
			return kitnats.Permanent(err)
		}
		processCtx, cancel := context.WithTimeout(messageCtx, config.ProcessTimeout)
		_, err := engine.Complete(processCtx, completion)
		cancel()
		if err != nil {
			logConsumerError("completion", message, err)
		}
		return err
	})
}

var _ coresaga.Publisher = (*JetStreamPublisher)(nil)

func validSubjectPath(subject string) bool {
	if subject == "" || len(subject) > 256 || strings.TrimSpace(subject) != subject || strings.ContainsAny(subject, "*> \t\r\n") {
		return false
	}
	for _, token := range strings.Split(subject, ".") {
		if token == "" {
			return false
		}
	}
	return true
}

func validDeliveryLimits(maxDeliver, maxAckPending int, nakMin, nakMax time.Duration) bool {
	return maxDeliver > 0 && maxDeliver <= 1_000_000 && maxAckPending > 0 && maxAckPending <= 65_536 && nakMin > 0 && nakMax >= nakMin && nakMax <= 24*time.Hour
}
