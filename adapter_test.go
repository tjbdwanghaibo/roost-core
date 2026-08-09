package syncstream

import (
	"errors"
	"testing"

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
