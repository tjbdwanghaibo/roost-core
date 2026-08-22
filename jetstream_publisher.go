package nestwal

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	fnats "github.com/tjbdwanghaibo/cube-core/nats"
	corenest "github.com/tjbdwanghaibo/cube-core/nest"
)

type JetStreamEffectPublisher struct {
	client fnats.IJetStream
	prefix string
}

type effectEnvelope struct {
	TransactionID string            `json:"transaction_id"`
	EffectID      string            `json:"effect_id"`
	Topic         string            `json:"topic"`
	Key           string            `json:"key,omitempty"`
	Headers       map[string]string `json:"headers,omitempty"`
	Payload       []byte            `json:"payload"`
}

func NewJetStreamEffectPublisher(client fnats.IJetStream, prefix string) (*JetStreamEffectPublisher, error) {
	prefix = strings.Trim(strings.TrimSpace(prefix), ".")
	if client == nil || prefix == "" {
		return nil, fmt.Errorf("nestwal: JetStream client and effect prefix are required")
	}
	return &JetStreamEffectPublisher{client: client, prefix: prefix}, nil
}

func (p *JetStreamEffectPublisher) PublishEffect(ctx context.Context, txID corenest.TransactionID, effect corenest.Effect) error {
	if p == nil || p.client == nil || effect.ID == "" || effect.Topic == "" {
		return ErrEffectPublisherRequired
	}
	payload, err := json.Marshal(effectEnvelope{
		TransactionID: txID.String(), EffectID: effect.ID, Topic: effect.Topic,
		Key: effect.Key, Headers: effect.Headers, Payload: effect.Payload,
	})
	if err != nil {
		return err
	}
	subject := p.prefix + "." + strings.Trim(effect.Topic, ".")
	_, err = p.client.Publish(ctx, subject, payload, fnats.JetStreamPublishOptions{MsgID: effect.ID})
	return err
}

var _ EffectPublisher = (*JetStreamEffectPublisher)(nil)
