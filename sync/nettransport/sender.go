// Package nettransport is the network side of entity sync: the UDP / KCP /
// QUIC session transports, the AEAD datagram wrapper, and AsyncTransport, the
// per-session reliable queue entitysync pushes frames through (backpressure
// closes the session). Unreliable datagrams go straight to a DatagramSender;
// lockstep does that. Payloads are opaque bytes here; what a frame means is
// frame's and entitysync's job.
package nettransport

import (
	"context"
	"errors"
	"reflect"
	"time"
)

var (
	ErrTransportRequired     = errors.New("nettransport: downstream transport is required")
	ErrTransportClosed       = errors.New("nettransport: transport is closed")
	ErrSessionNotRegistered  = errors.New("nettransport: session is not registered")
	ErrSessionAlreadyExists  = errors.New("nettransport: session is already registered")
	ErrSessionFailed         = errors.New("nettransport: session transport has failed")
	ErrSessionLimit          = errors.New("nettransport: session limit exceeded")
	ErrReliableBackpressure  = errors.New("nettransport: reliable queue is full")
	ErrReliableMessageTooBig = errors.New("nettransport: reliable message is too large")
	ErrRouteNotBound         = errors.New("nettransport: session network route is not bound")
	ErrProtocolConfig        = errors.New("nettransport: invalid protocol configuration")
	ErrPayloadTooLarge       = errors.New("nettransport: protocol payload is too large")
	ErrAuthentication        = errors.New("nettransport: packet authentication failed")
)

// DefaultMaxDatagram is the payload bound a conservative path MTU leaves for
// one unreliable datagram; the protocol transports default to it.
const DefaultMaxDatagram = 1200

type DatagramSender interface {
	SendDatagram(context.Context, SessionID, []byte) error
}

type DatagramBatchSender interface {
	SendDatagramBatch(context.Context, SessionID, [][]byte) error
}

type ReliableSender interface {
	SendReliable(context.Context, SessionID, []byte) error
}

// CompositeTransport joins independent unreliable and reliable network lanes.
// A QUIC adapter, for example, can supply a DATAGRAM sender and a stream sender.
type CompositeTransport struct {
	Datagrams DatagramSender
	Reliable  ReliableSender
}

func (transport CompositeTransport) SendDatagram(ctx context.Context, session SessionID, payload []byte) error {
	if isNilInterface(transport.Datagrams) {
		return ErrTransportRequired
	}
	return transport.Datagrams.SendDatagram(ctx, session, payload)
}

func (transport CompositeTransport) SendDatagramBatch(ctx context.Context, session SessionID, packets [][]byte) error {
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

func (transport CompositeTransport) SendReliable(ctx context.Context, session SessionID, payload []byte) error {
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

var _ Transport = CompositeTransport{}
var _ DatagramBatchTransport = CompositeTransport{}
