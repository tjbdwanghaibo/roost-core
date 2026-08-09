// Package syncstream adapts cube-core/syncstream packets to cube-kit sync
// transports. The adapter works with both the NATS and JetStream ISyncBus
// implementations.
package syncstream

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"

	coresync "github.com/tjbdwanghaibo/cube-core/sync"
	corestream "github.com/tjbdwanghaibo/cube-core/syncstream"
)

var (
	ErrPublisherRequired  = errors.New("syncstream adapter: publisher is required")
	ErrSubscriberRequired = errors.New("syncstream adapter: subscriber is required")
	ErrHandlerRequired    = errors.New("syncstream adapter: handler is required")
	ErrSequenceOverflow   = errors.New("syncstream adapter: sequence exceeds transport version range")
	ErrEnvelopeMismatch   = errors.New("syncstream adapter: transport and packet envelopes differ")
)

type ErrorHandler func(error)

// Publisher is both an explicit error-returning publisher and a corestream.Sink.
// Enqueue reports asynchronous-style errors through ErrorHandler.
type Publisher struct {
	bus     coresync.IPublisher
	fromSid int32
	onError ErrorHandler
}

func NewPublisher(bus coresync.IPublisher, fromSid int32, onError ErrorHandler) (*Publisher, error) {
	if bus == nil {
		return nil, ErrPublisherRequired
	}
	return &Publisher{bus: bus, fromSid: fromSid, onError: onError}, nil
}

func (publisher *Publisher) Publish(packet corestream.Packet) error {
	if publisher == nil || publisher.bus == nil {
		return ErrPublisherRequired
	}
	if packet.Sequence > math.MaxInt64 {
		return ErrSequenceOverflow
	}
	payload, err := json.Marshal(packet)
	if err != nil {
		return err
	}
	return publisher.bus.Publish(&coresync.SyncMsg{
		Topic: packet.Stream.Topic, Key: packet.Stream.Key, Version: int64(packet.Sequence),
		Data: payload, FromSid: publisher.fromSid,
	})
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

// Subscribe decodes packets and rejects an inner envelope that disagrees with
// the transport envelope. This prevents routing/version metadata from being
// silently spoofed by payload data.
func Subscribe(bus coresync.ISubscriber, topic string, handler Handler) (func(), error) {
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
		var packet corestream.Packet
		if err := json.Unmarshal(message.Data, &packet); err != nil {
			return fmt.Errorf("syncstream adapter: decode: %w", err)
		}
		if packet.Stream.Topic != message.Topic || packet.Stream.Key != message.Key || packet.Sequence != uint64(message.Version) || message.Version < 0 {
			return ErrEnvelopeMismatch
		}
		return handler(packet.Clone())
	})
}

var _ corestream.Sink = (*Publisher)(nil)
var _ corestream.BatchSink = (*Publisher)(nil)
