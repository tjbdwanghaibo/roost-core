package nettransport

import (
	"context"
	"net"

	core "github.com/tjbdwanghaibo/roost-core/statesync"
)

type ControlTarget interface {
	HandleControl(core.SessionID, []byte) error
}

// ControlPlane terminates replication ACK/resync messages below the business
// layer. TryHandle is suitable for a shared QUIC/KCP reliable-message demux;
// UDPHandler plugs directly into UDPTransport.Serve.
type ControlPlane struct {
	target ControlTarget
}

func NewControlPlane(target ControlTarget) (*ControlPlane, error) {
	if target == nil {
		return nil, ErrProtocolConfig
	}
	return &ControlPlane{target: target}, nil
}

func (plane *ControlPlane) TryHandle(session core.SessionID, payload []byte) (bool, error) {
	if plane == nil || plane.target == nil || session == 0 {
		return false, ErrProtocolConfig
	}
	if !core.IsControlPayload(payload) {
		return false, nil
	}
	return true, plane.target.HandleControl(session, payload)
}

func (plane *ControlPlane) UDPHandler() UDPReceiveHandler {
	return func(_ context.Context, session core.SessionID, payload []byte, _ net.Addr) error {
		handled, err := plane.TryHandle(session, payload)
		if err != nil {
			return err
		}
		if !handled {
			return core.ErrInvalidControl
		}
		return nil
	}
}

func (plane *ControlPlane) ServeUDP(ctx context.Context, transport *UDPTransport) error {
	if plane == nil || transport == nil {
		return ErrProtocolConfig
	}
	return transport.Serve(ctx, plane.UDPHandler())
}
