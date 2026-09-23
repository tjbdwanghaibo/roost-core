package nettransport

import (
	"context"
	"errors"
)

// ErrTransportMissing is returned by TransportFunc when the lane asked for
// has no function bound.
var ErrTransportMissing = errors.New("nettransport: transport is not configured")

// Transport is the two-lane send contract every session transport here
// implements: a datagram lane (unreliable, latest wins) and a reliable lane
// (ordered, backpressured). Payloads are opaque bytes.
type Transport interface {
	SendDatagram(context.Context, SessionID, []byte) error
	SendReliable(context.Context, SessionID, []byte) error
}

// DatagramBatchTransport admits all fragments of one frame as a unit. A
// transport with a latest-only queue should implement this interface so it
// never replaces or drops an individual fragment from a frame.
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
