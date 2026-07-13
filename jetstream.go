package nats

import (
	"context"
	"errors"

	fnats "github.com/tjbdwanghaibo/cube-core/nats"

	gojs "github.com/nats-io/nats.go/jetstream"
)

type jetStreamClient struct {
	js gojs.JetStream
}

func newJetStreamClient(client *natsClient) (*jetStreamClient, error) {
	if client == nil || client.natsConn() == nil {
		return nil, errors.New("nats jetstream: client is nil")
	}
	js, err := gojs.New(client.natsConn())
	if err != nil {
		return nil, err
	}
	return &jetStreamClient{js: js}, nil
}

func (c *jetStreamClient) EnsureStream(ctx context.Context, cfg fnats.JetStreamConfig) error {
	if c == nil || c.js == nil {
		return errors.New("nats jetstream: not initialized")
	}
	_, err := c.js.CreateOrUpdateStream(ctx, toJetStreamStreamConfig(cfg))
	return err
}

func (c *jetStreamClient) Publish(ctx context.Context, subject string, data []byte, opts fnats.JetStreamPublishOptions) (fnats.JetStreamPublishAck, error) {
	if c == nil || c.js == nil {
		return fnats.JetStreamPublishAck{}, errors.New("nats jetstream: not initialized")
	}
	publishOpts := make([]gojs.PublishOpt, 0, 1)
	if opts.MsgID != "" {
		publishOpts = append(publishOpts, gojs.WithMsgID(opts.MsgID))
	}
	ack, err := c.js.Publish(ctx, subject, data, publishOpts...)
	if err != nil {
		return fnats.JetStreamPublishAck{}, err
	}
	if ack == nil {
		return fnats.JetStreamPublishAck{}, nil
	}
	return fnats.JetStreamPublishAck{
		Stream:    ack.Stream,
		Sequence:  ack.Sequence,
		Duplicate: ack.Duplicate,
	}, nil
}

func (c *jetStreamClient) Subscribe(ctx context.Context, cfg fnats.JetStreamConsumerConfig, handler fnats.JetStreamHandler) (fnats.IJetStreamSubscription, error) {
	if c == nil || c.js == nil {
		return nil, errors.New("nats jetstream: not initialized")
	}
	consumer, err := c.js.CreateOrUpdateConsumer(ctx, cfg.Stream, toJetStreamConsumerConfig(cfg))
	if err != nil {
		return nil, err
	}
	cc, err := consumer.Consume(func(msg gojs.Msg) {
		if handler == nil {
			_ = msg.Ack()
			return
		}
		wrapped := jetStreamMsg(msg)
		if err := handler(ctx, wrapped); err != nil {
			_ = msg.Nak()
			return
		}
		_ = msg.Ack()
	})
	if err != nil {
		return nil, err
	}
	return &jetStreamSubscription{cc: cc}, nil
}

func toJetStreamStreamConfig(cfg fnats.JetStreamConfig) gojs.StreamConfig {
	return gojs.StreamConfig{
		Name:       cfg.Name,
		Subjects:   append([]string(nil), cfg.Subjects...),
		Storage:    toJetStreamStorage(cfg.Storage),
		MaxAge:     cfg.MaxAge,
		Duplicates: cfg.Duplicates,
		Replicas:   cfg.Replicas,
		MaxBytes:   cfg.MaxBytes,
	}
}

func toJetStreamConsumerConfig(cfg fnats.JetStreamConsumerConfig) gojs.ConsumerConfig {
	name := cfg.Name
	if name == "" {
		name = cfg.Durable
	}
	durable := cfg.Durable
	if durable == "" {
		durable = name
	}
	return gojs.ConsumerConfig{
		Name:          name,
		Durable:       durable,
		FilterSubject: cfg.FilterSubject,
		DeliverPolicy: toJetStreamDeliverPolicy(cfg.DeliverPolicy),
		AckPolicy:     gojs.AckExplicitPolicy,
		AckWait:       cfg.AckWait,
		MaxDeliver:    cfg.MaxDeliver,
	}
}

func toJetStreamStorage(storage fnats.JetStreamStorage) gojs.StorageType {
	switch storage {
	case fnats.JetStreamStorageMemory:
		return gojs.MemoryStorage
	default:
		return gojs.FileStorage
	}
}

func toJetStreamDeliverPolicy(policy fnats.JetStreamDeliverPolicy) gojs.DeliverPolicy {
	switch policy {
	case fnats.JetStreamDeliverNew:
		return gojs.DeliverNewPolicy
	default:
		return gojs.DeliverAllPolicy
	}
}

func jetStreamMsg(msg gojs.Msg) *fnats.JetStreamMsg {
	wrapped := &fnats.JetStreamMsg{
		Subject: msg.Subject(),
		Data:    append([]byte(nil), msg.Data()...),
	}
	meta, err := msg.Metadata()
	if err == nil && meta != nil {
		wrapped.Stream = meta.Stream
		wrapped.Consumer = meta.Consumer
		wrapped.StreamSeq = meta.Sequence.Stream
		wrapped.ConsumerSeq = meta.Sequence.Consumer
		wrapped.NumDelivered = meta.NumDelivered
	}
	return wrapped
}

type jetStreamSubscription struct {
	cc gojs.ConsumeContext
}

func (s *jetStreamSubscription) Stop() {
	if s != nil && s.cc != nil {
		s.cc.Stop()
	}
}

func (s *jetStreamSubscription) Drain() {
	if s != nil && s.cc != nil {
		s.cc.Drain()
	}
}

func (s *jetStreamSubscription) Closed() <-chan struct{} {
	if s == nil || s.cc == nil {
		ch := make(chan struct{})
		close(ch)
		return ch
	}
	return s.cc.Closed()
}

var _ fnats.IJetStream = (*jetStreamClient)(nil)
