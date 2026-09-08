// Package replication contains reusable transport-side building blocks for
// roost-core/replication. Protocol state and delta algorithms stay in core;
// queueing and concrete network adaptation belong in kit.
package nettransport

import (
	"context"
	"errors"
	"reflect"
	"time"

	core "github.com/tjbdwanghaibo/roost-core/statesync"
)

var (
	ErrTransportRequired     = errors.New("replication transport: downstream transport is required")
	ErrTransportClosed       = errors.New("replication transport: transport is closed")
	ErrSessionNotRegistered  = errors.New("replication transport: session is not registered")
	ErrSessionAlreadyExists  = errors.New("replication transport: session is already registered")
	ErrSessionFailed         = errors.New("replication transport: session transport has failed")
	ErrSessionLimit          = errors.New("replication transport: session limit exceeded")
	ErrReliableBackpressure  = errors.New("replication transport: reliable queue is full")
	ErrInvalidDatagramBatch  = errors.New("replication transport: invalid datagram batch")
	ErrReliableMessageTooBig = errors.New("replication transport: reliable message is too large")
	ErrRouteNotBound         = errors.New("replication transport: session network route is not bound")
	ErrProtocolConfig        = errors.New("replication transport: invalid protocol configuration")
	ErrPayloadTooLarge       = errors.New("replication transport: protocol payload is too large")
	ErrAuthentication        = errors.New("replication transport: packet authentication failed")
)

type DatagramSender interface {
	SendDatagram(context.Context, core.SessionID, []byte) error
}

type DatagramBatchSender interface {
	SendDatagramBatch(context.Context, core.SessionID, [][]byte) error
}

type ReliableSender interface {
	SendReliable(context.Context, core.SessionID, []byte) error
}

// OutboundFrame is one already-framed replication message. Exactly one of
// Datagrams and Reliable must be populated. Datagram fragments belong to one
// complete frame and are admitted/replaced as a unit.
type OutboundFrame struct {
	Session   core.SessionID
	Datagrams [][]byte
	Reliable  []byte
}

// AtomicBatchTransport accepts all frames or none of them. Higher-level
// schedulers use this boundary so their sequence counters and dirty state are
// committed only after transport ownership has been transferred.
type AtomicBatchTransport interface {
	AdmitBatch(context.Context, []OutboundFrame) error
}

// CompositeTransport joins independent unreliable and reliable network lanes.
// A QUIC adapter, for example, can supply a DATAGRAM sender and a stream sender.
type CompositeTransport struct {
	Datagrams DatagramSender
	Reliable  ReliableSender
}

func (transport CompositeTransport) SendDatagram(ctx context.Context, session core.SessionID, payload []byte) error {
	if isNilInterface(transport.Datagrams) {
		return ErrTransportRequired
	}
	return transport.Datagrams.SendDatagram(ctx, session, payload)
}

func (transport CompositeTransport) SendDatagramBatch(ctx context.Context, session core.SessionID, packets [][]byte) error {
	if isNilInterface(transport.Datagrams) {
		return ErrTransportRequired
	}
	if batch, ok := transport.Datagrams.(DatagramBatchSender); ok {
		return batch.SendDatagramBatch(ctx, session, packets)
	}
	for _, packet := range packets {
		if err := transport.Datagrams.SendDatagram(ctx, session, packet); err != nil {
			return err
		}
	}
	return nil
}

func (transport CompositeTransport) SendReliable(ctx context.Context, session core.SessionID, payload []byte) error {
	if isNilInterface(transport.Reliable) {
		return ErrTransportRequired
	}
	return transport.Reliable.SendReliable(ctx, session, payload)
}

func isNilInterface(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}

type writeDeadlineSetter interface {
	SetWriteDeadline(time.Time) error
}

type readDeadlineSetter interface {
	SetReadDeadline(time.Time) error
}

func interruptWriteOnCancel(ctx context.Context, setter writeDeadlineSetter) func() {
	done := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		defer close(done)
		_ = setter.SetWriteDeadline(time.Now())
	})
	return func() {
		if !stop() {
			<-done
		}
		_ = setter.SetWriteDeadline(time.Time{})
	}
}

func interruptReadOnCancel(ctx context.Context, setter readDeadlineSetter) func() {
	done := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		defer close(done)
		_ = setter.SetReadDeadline(time.Now())
	})
	return func() {
		if !stop() {
			<-done
		}
		_ = setter.SetReadDeadline(time.Time{})
	}
}

var _ core.Transport = CompositeTransport{}
var _ core.DatagramBatchTransport = CompositeTransport{}
