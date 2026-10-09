package nettransport

import (
	"context"
	"errors"
)

// ErrTransportMissing is returned by TransportFunc when the lane asked for
// has no function bound.
var ErrTransportMissing = errors.New("nettransport: transport is not configured")

// Transport is the two-lane send contract the protocol transports (UDP, KCP,
// QUIC, CompositeTransport) implement: an unreliable datagram send and a
// reliable ordered send. Payloads are opaque bytes. AsyncTransport is not a
// Transport — it is the reliable queue in front of one.
type Transport interface {
	SendDatagram(context.Context, SessionID, []byte) error
	SendReliable(context.Context, SessionID, []byte) error
}

// DatagramBatchTransport sends several datagrams for one session in one call,
// for a protocol that can hand a burst to the socket more cheaply than one
// packet at a time.
type DatagramBatchTransport interface {
	SendDatagramBatch(context.Context, SessionID, [][]byte) error
}

// SessionTransport is an optional lifecycle extension: a transport that keeps
// per-session state learns here when a session is registered and removed.
type SessionTransport interface {
	RegisterSession(SessionInfo) error
	RemoveSession(SessionID) bool
}

// TransportFunc adapts two functions to Transport.
type TransportFunc struct {
	Datagram func(context.Context, SessionID, []byte) error
	Reliable func(context.Context, SessionID, []byte) error
}

func (f TransportFunc) SendDatagram(ctx context.Context, session SessionID, payload []byte) error {
	if f.Datagram == nil {
		return ErrTransportMissing
	}
	return f.Datagram(ctx, session, payload)
}

func (f TransportFunc) SendReliable(ctx context.Context, session SessionID, payload []byte) error {
	if f.Reliable == nil {
		return ErrTransportMissing
	}
	return f.Reliable(ctx, session, payload)
}
