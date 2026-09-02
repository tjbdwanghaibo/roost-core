package nats

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/tjbdwanghaibo/cube-core/metrics"
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
	if handler == nil {
		return nil, errors.New("nats jetstream: handler is nil")
	}
	consumer, err := c.js.CreateOrUpdateConsumer(ctx, cfg.Stream, toJetStreamConsumerConfig(cfg))
	if err != nil {
		return nil, err
	}
	handlerCtx, cancel := context.WithCancel(context.Background())
	cc, err := consumer.Consume(func(msg gojs.Msg) {
		wrapped := jetStreamMsg(msg)
		err := invokeJetStreamHandler(handlerCtx, handler, wrapped)
		if err != nil {
			if reason := terminalReason(err, cfg.MaxDeliver, wrapped.NumDelivered); reason != "" {
				slog.Error("nats jetstream: terminating failed delivery", "subject", wrapped.Subject, "stream", wrapped.Stream, "consumer", wrapped.Consumer, "stream_sequence", wrapped.StreamSeq, "deliveries", wrapped.NumDelivered, "reason", reason, "err", err)
				metrics.IncCounter("nats.jetstream.terminal.total", metrics.Labels{"reason": reason}, 1)
				_ = msg.Term()
				return
			}
			if delay := nakBackoff(cfg, wrapped.NumDelivered); delay > 0 {
				_ = msg.NakWithDelay(delay)
			} else {
				_ = msg.Nak()
			}
			return
		}
		_ = msg.Ack()
	})
	if err != nil {
		cancel()
		return nil, err
	}
	return &jetStreamSubscription{cc: cc, cancel: cancel}, nil
}

func terminalReason(err error, maxDeliver int, deliveries uint64) string {
	if isPermanent(err) {
		return "permanent"
	}
	if err != nil && maxDeliver > 0 && deliveries >= uint64(maxDeliver) {
		return "max_deliver"
	}
	return ""
}

func invokeJetStreamHandler(ctx context.Context, handler fnats.JetStreamHandler, msg *fnats.JetStreamMsg) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("nats jetstream: handler panic: %v", recovered)
		}
	}()
	return handler(ctx, msg)
}

func nakBackoff(config fnats.JetStreamConsumerConfig, deliveries uint64) time.Duration {
	minimum, maximum := config.NakBackoffMin, config.NakBackoffMax
	if minimum <= 0 {
		return 0
	}
	if maximum < minimum {
		maximum = minimum
	}
	delay := minimum
	for attempt := uint64(1); attempt < deliveries && delay < maximum; attempt++ {
		if delay > maximum/2 {
			return maximum
		}
		delay *= 2
	}
	if delay > maximum {
		return maximum
	}
	return delay
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
		MaxAckPending: cfg.MaxAckPending,
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
	cc         gojs.ConsumeContext
	cancel     context.CancelFunc
	mu         sync.Mutex
	cancelOnce sync.Once
	draining   bool
	stopped    bool
}

func (s *jetStreamSubscription) Stop() {
	if s != nil && s.cc != nil {
		s.mu.Lock()
		if s.stopped {
			s.mu.Unlock()
			return
		}
		s.stopped = true
		s.mu.Unlock()
		s.cancelHandler()
		s.cc.Stop()
	}
}

func (s *jetStreamSubscription) Drain() {
	if s != nil && s.cc != nil {
		s.mu.Lock()
		if s.stopped || s.draining {
			s.mu.Unlock()
			return
		}
		s.draining = true
		s.mu.Unlock()
		s.cc.Drain()
		go func() {
			<-s.cc.Closed()
			s.cancelHandler()
			s.mu.Lock()
			s.stopped = true
			s.mu.Unlock()
		}()
	}
}

func (s *jetStreamSubscription) cancelHandler() {
	if s.cancel != nil {
		s.cancelOnce.Do(s.cancel)
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
