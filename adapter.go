// Package syncstream adapts cube-core/syncstream packets to cube-kit sync
// transports. The adapter works with both the NATS and JetStream ISyncBus
// implementations.
package syncstream

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"sync"
	"sync/atomic"
	"time"

	coresync "github.com/tjbdwanghaibo/cube-core/sync"
	corestream "github.com/tjbdwanghaibo/cube-core/syncstream"
)

var (
	ErrPublisherRequired  = errors.New("syncstream adapter: publisher is required")
	ErrSubscriberRequired = errors.New("syncstream adapter: subscriber is required")
	ErrHandlerRequired    = errors.New("syncstream adapter: handler is required")
	ErrSequenceOverflow   = errors.New("syncstream adapter: sequence exceeds transport version range")
	ErrEnvelopeMismatch   = errors.New("syncstream adapter: transport and packet envelopes differ")
	ErrObserverMismatch   = errors.New("syncstream adapter: packet observer mismatch")
	ErrPayloadTooLarge    = errors.New("syncstream adapter: payload exceeds configured limit")
	ErrBackpressure       = errors.New("syncstream adapter: publish queue is full")
	ErrPublisherClosed    = errors.New("syncstream adapter: publisher is closed")
)

type ErrorHandler func(error)

// Publisher is both an explicit error-returning publisher and a corestream.Sink.
// Enqueue reports asynchronous-style errors through ErrorHandler.
type Publisher struct {
	bus              coresync.IPublisher
	fromSid          int32
	onError          ErrorHandler
	expectedObserver *corestream.Observer
	maxPayloadBytes  int
	published        atomic.Uint64
	failures         atomic.Uint64
}

type PublisherOptions struct {
	FromSID          int32
	OnError          ErrorHandler
	ExpectedObserver *corestream.Observer
	MaxPayloadBytes  int
}

func NewPublisher(bus coresync.IPublisher, fromSid int32, onError ErrorHandler) (*Publisher, error) {
	return NewPublisherWithOptions(bus, PublisherOptions{FromSID: fromSid, OnError: onError})
}

func NewPublisherWithOptions(bus coresync.IPublisher, options PublisherOptions) (*Publisher, error) {
	if bus == nil {
		return nil, ErrPublisherRequired
	}
	var observer *corestream.Observer
	if options.ExpectedObserver != nil {
		value := *options.ExpectedObserver
		observer = &value
	}
	return &Publisher{bus: bus, fromSid: options.FromSID, onError: options.OnError, expectedObserver: observer, maxPayloadBytes: options.MaxPayloadBytes}, nil
}

func (publisher *Publisher) Publish(packet corestream.Packet) error {
	if publisher == nil || publisher.bus == nil {
		return ErrPublisherRequired
	}
	if packet.Sequence > math.MaxInt64 {
		return ErrSequenceOverflow
	}
	if publisher.expectedObserver != nil && packet.Observer != *publisher.expectedObserver {
		return ErrObserverMismatch
	}
	if publisher.maxPayloadBytes > 0 && len(packet.Payload) > publisher.maxPayloadBytes {
		return ErrPayloadTooLarge
	}
	payload, err := json.Marshal(packet)
	if err != nil {
		publisher.failures.Add(1)
		return err
	}
	err = publisher.bus.Publish(&coresync.SyncMsg{
		Topic: packet.Stream.Topic, Key: packet.Stream.Key, Version: int64(packet.Sequence),
		Data: payload, FromSid: publisher.fromSid,
	})
	if err != nil {
		publisher.failures.Add(1)
		return err
	}
	publisher.published.Add(1)
	return nil
}

func (publisher *Publisher) Enqueue(packet corestream.Packet) {
	if err := publisher.Publish(packet); err != nil && publisher.onError != nil {
		publisher.onError(err)
	}
}

func (publisher *Publisher) EnqueueBatch(packets []corestream.Packet) {
	for _, packet := range packets {
		publisher.Enqueue(packet)
	}
}

type Handler func(corestream.Packet) error

type SubscribeOptions struct {
	ExpectedObserver *corestream.Observer
	MaxEnvelopeBytes int
	MaxPayloadBytes  int
}

// Subscribe decodes packets and rejects an inner envelope that disagrees with
// the transport envelope. This prevents routing/version metadata from being
// silently spoofed by payload data.
func Subscribe(bus coresync.ISubscriber, topic string, handler Handler) (func(), error) {
	return SubscribeWithOptions(bus, topic, SubscribeOptions{}, handler)
}

func SubscribeForObserver(bus coresync.ISubscriber, topic string, observer corestream.Observer, handler Handler) (func(), error) {
	return SubscribeWithOptions(bus, topic, SubscribeOptions{ExpectedObserver: &observer}, handler)
}

func SubscribeWithOptions(bus coresync.ISubscriber, topic string, options SubscribeOptions, handler Handler) (func(), error) {
	if bus == nil {
		return nil, ErrSubscriberRequired
	}
	if handler == nil {
		return nil, ErrHandlerRequired
	}
	return bus.Subscribe(topic, func(message *coresync.SyncMsg) error {
		if message == nil {
			return nil
		}
		if options.MaxEnvelopeBytes > 0 && len(message.Data) > options.MaxEnvelopeBytes {
			return ErrPayloadTooLarge
		}
		var packet corestream.Packet
		decoder := json.NewDecoder(bytes.NewReader(message.Data))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&packet); err != nil {
			return fmt.Errorf("syncstream adapter: decode: %w", err)
		}
		if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
			return fmt.Errorf("syncstream adapter: decode trailing content")
		}
		if packet.Stream.Topic != message.Topic || packet.Stream.Key != message.Key || packet.Sequence != uint64(message.Version) || message.Version < 0 {
			return ErrEnvelopeMismatch
		}
		if options.ExpectedObserver != nil && packet.Observer != *options.ExpectedObserver {
			return ErrObserverMismatch
		}
		if options.MaxPayloadBytes > 0 && len(packet.Payload) > options.MaxPayloadBytes {
			return ErrPayloadTooLarge
		}
		return handler(packet.Clone())
	})
}

type PublisherMetrics struct {
	Published uint64
	Failures  uint64
}

func (publisher *Publisher) Metrics() PublisherMetrics {
	if publisher == nil {
		return PublisherMetrics{}
	}
	return PublisherMetrics{Published: publisher.published.Load(), Failures: publisher.failures.Load()}
}

type PacketPublisher interface {
	Publish(corestream.Packet) error
}

type BufferedPublisherOptions struct {
	Capacity    int
	MaxAttempts int
	RetryDelay  time.Duration
	OnError     ErrorHandler
}

type BufferedPublisherMetrics struct {
	Queued       uint64
	Published    uint64
	Failures     uint64
	Backpressure uint64
}

// BufferedPublisher adds a bounded, non-dropping admission queue. Publish
// returns ErrBackpressure when full, allowing the caller to recover from its
// durable History instead of silently losing critical state.
type BufferedPublisher struct {
	mutex       sync.RWMutex
	publisher   PacketPublisher
	queue       chan corestream.Packet
	maxAttempts int
	retryDelay  time.Duration
	onError     ErrorHandler
	closed      bool
	wait        sync.WaitGroup
	queued      atomic.Uint64
	published   atomic.Uint64
	failures    atomic.Uint64
	pressure    atomic.Uint64
}

func NewBufferedPublisher(publisher PacketPublisher, options BufferedPublisherOptions) (*BufferedPublisher, error) {
	if publisher == nil {
		return nil, ErrPublisherRequired
	}
	if options.Capacity <= 0 {
		options.Capacity = 256
	}
	if options.MaxAttempts <= 0 {
		options.MaxAttempts = 1
	}
	buffered := &BufferedPublisher{
		publisher: publisher, queue: make(chan corestream.Packet, options.Capacity),
		maxAttempts: options.MaxAttempts, retryDelay: options.RetryDelay, onError: options.OnError,
	}
	buffered.wait.Add(1)
	go buffered.run()
	return buffered, nil
}

func (publisher *BufferedPublisher) Publish(packet corestream.Packet) error {
	if publisher == nil {
		return ErrPublisherRequired
	}
	publisher.mutex.RLock()
	defer publisher.mutex.RUnlock()
	if publisher.closed {
		return ErrPublisherClosed
	}
	select {
	case publisher.queue <- packet.Clone():
		publisher.queued.Add(1)
		return nil
	default:
		publisher.pressure.Add(1)
		return ErrBackpressure
	}
}

func (publisher *BufferedPublisher) run() {
	defer publisher.wait.Done()
	for packet := range publisher.queue {
		var err error
		for attempt := 0; attempt < publisher.maxAttempts; attempt++ {
			err = publisher.publisher.Publish(packet)
			if err == nil {
				publisher.published.Add(1)
				break
			}
			if publisher.retryDelay > 0 && attempt+1 < publisher.maxAttempts {
				time.Sleep(publisher.retryDelay)
			}
		}
		if err != nil {
			publisher.failures.Add(1)
			if publisher.onError != nil {
				publisher.onError(err)
			}
		}
	}
}

// Close stops admission and drains all accepted packets. The context bounds
// only the caller's wait; draining continues so accepted packets are not lost.
func (publisher *BufferedPublisher) Close(ctx context.Context) error {
	if publisher == nil {
		return nil
	}
	publisher.mutex.Lock()
	if !publisher.closed {
		publisher.closed = true
		close(publisher.queue)
	}
	publisher.mutex.Unlock()
	done := make(chan struct{})
	go func() {
		publisher.wait.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (publisher *BufferedPublisher) Metrics() BufferedPublisherMetrics {
	if publisher == nil {
		return BufferedPublisherMetrics{}
	}
	return BufferedPublisherMetrics{
		Queued: publisher.queued.Load(), Published: publisher.published.Load(),
		Failures: publisher.failures.Load(), Backpressure: publisher.pressure.Load(),
	}
}

var _ corestream.Sink = (*Publisher)(nil)
var _ corestream.BatchSink = (*Publisher)(nil)
