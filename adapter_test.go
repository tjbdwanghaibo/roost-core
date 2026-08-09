package syncstream

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	coresync "github.com/tjbdwanghaibo/cube-core/sync"
	corestream "github.com/tjbdwanghaibo/cube-core/syncstream"
)

func TestPublisherAndSubscriberRoundTrip(t *testing.T) {
	bus := &memoryBus{}
	publisher, err := NewPublisher(bus, 12, nil)
	if err != nil {
		t.Fatal(err)
	}
	var received corestream.Packet
	if _, err := Subscribe(bus, "skill.presentation", func(packet corestream.Packet) error {
		received = packet
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	packet := corestream.Packet{
		Observer: corestream.Observer{ID: 5, Scope: "match"},
		Stream:   corestream.Stream{Topic: "skill.presentation", Key: 7},
		Sequence: 3, BaseSequence: 2, SchemaVersion: 1, Payload: []byte("payload"),
	}
	if err := publisher.Publish(packet); err != nil {
		t.Fatal(err)
	}
	if received.Sequence != 3 || received.BaseSequence != 2 || string(received.Payload) != "payload" || bus.last.FromSid != 12 {
		t.Fatalf("received=%#v transport=%#v", received, bus.last)
	}
}

func TestSubscriberRejectsEnvelopeMismatch(t *testing.T) {
	bus := &memoryBus{}
	_, err := Subscribe(bus, "state", func(corestream.Packet) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	publisher, _ := NewPublisher(bus, 1, nil)
	packet := corestream.Packet{Stream: corestream.Stream{Topic: "other", Key: 1}, Sequence: 1}
	payloadBus := &capturingPublisher{}
	encoder, _ := NewPublisher(payloadBus, 1, nil)
	if err := encoder.Publish(packet); err != nil {
		t.Fatal(err)
	}
	payloadBus.last.Topic = "state"
	if err := bus.Publish(payloadBus.last); !errors.Is(err, ErrEnvelopeMismatch) {
		t.Fatalf("mismatch error = %v", err)
	}
	_ = publisher
}

func TestEnqueueReportsPublishErrors(t *testing.T) {
	want := errors.New("publish failed")
	bus := &capturingPublisher{err: want}
	var got error
	publisher, _ := NewPublisher(bus, 1, func(err error) { got = err })
	publisher.Enqueue(corestream.Packet{Stream: corestream.Stream{Topic: "state"}})
	if !errors.Is(got, want) {
		t.Fatalf("reported error = %v", got)
	}
}

func TestObserverAndPayloadGuards(t *testing.T) {
	bus := &memoryBus{}
	observer := corestream.Observer{ID: 7, Scope: "match"}
	publisher, err := NewPublisherWithOptions(bus, PublisherOptions{ExpectedObserver: &observer, MaxPayloadBytes: 4})
	if err != nil {
		t.Fatal(err)
	}
	if err := publisher.Publish(corestream.Packet{Observer: corestream.Observer{ID: 8}, Stream: corestream.Stream{Topic: "state"}}); !errors.Is(err, ErrObserverMismatch) {
		t.Fatalf("publisher observer error = %v", err)
	}
	if err := publisher.Publish(corestream.Packet{Observer: observer, Stream: corestream.Stream{Topic: "state"}, Payload: []byte("large")}); !errors.Is(err, ErrPayloadTooLarge) {
		t.Fatalf("publisher payload error = %v", err)
	}

	if _, err := SubscribeWithOptions(bus, "state", SubscribeOptions{ExpectedObserver: &observer, MaxPayloadBytes: 4}, func(corestream.Packet) error { return nil }); err != nil {
		t.Fatal(err)
	}
	foreign := corestream.Packet{Observer: corestream.Observer{ID: 9}, Stream: corestream.Stream{Topic: "state"}, Sequence: 1}
	payload, err := json.Marshal(foreign)
	if err != nil {
		t.Fatal(err)
	}
	if err := bus.Publish(&coresync.SyncMsg{Topic: "state", Version: 1, Data: payload}); !errors.Is(err, ErrObserverMismatch) {
		t.Fatalf("subscriber observer error = %v", err)
	}
}

type blockingPublisher struct {
	started chan struct{}
	release chan struct{}
}

func (publisher *blockingPublisher) Publish(corestream.Packet) error {
	select {
	case publisher.started <- struct{}{}:
	default:
	}
	<-publisher.release
	return nil
}

func TestBufferedPublisherSignalsBackpressureAndDrains(t *testing.T) {
	downstream := &blockingPublisher{started: make(chan struct{}, 1), release: make(chan struct{})}
	publisher, err := NewBufferedPublisher(downstream, BufferedPublisherOptions{Capacity: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := publisher.Publish(corestream.Packet{Sequence: 1}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-downstream.started:
	case <-time.After(time.Second):
		t.Fatal("worker did not start")
	}
	if err := publisher.Publish(corestream.Packet{Sequence: 2}); err != nil {
		t.Fatal(err)
	}
	if err := publisher.Publish(corestream.Packet{Sequence: 3}); !errors.Is(err, ErrBackpressure) {
		t.Fatalf("backpressure error = %v", err)
	}
	close(downstream.release)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := publisher.Close(ctx); err != nil {
		t.Fatal(err)
	}
	metrics := publisher.Metrics()
	if metrics.Queued != 2 || metrics.Published != 2 || metrics.Backpressure != 1 {
		t.Fatalf("metrics = %#v", metrics)
	}
	if err := publisher.Publish(corestream.Packet{}); !errors.Is(err, ErrPublisherClosed) {
		t.Fatalf("closed error = %v", err)
	}
}

type memoryBus struct {
	handler coresync.Handler
	last    *coresync.SyncMsg
}

func (bus *memoryBus) Publish(message *coresync.SyncMsg) error {
	copyMessage := *message
	copyMessage.Data = append([]byte(nil), message.Data...)
	bus.last = &copyMessage
	if bus.handler != nil {
		return bus.handler(&copyMessage)
	}
	return nil
}

func (bus *memoryBus) Subscribe(_ string, handler coresync.Handler) (func(), error) {
	bus.handler = handler
	return func() {}, nil
}

type capturingPublisher struct {
	last *coresync.SyncMsg
	err  error
}

func (publisher *capturingPublisher) Publish(message *coresync.SyncMsg) error {
	copyMessage := *message
	copyMessage.Data = append([]byte(nil), message.Data...)
	publisher.last = &copyMessage
	return publisher.err
}
