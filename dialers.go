// Package robot plugs the kit's transports and lockstep client machinery
// into the roost-core robot framework: KCP/QUIC dialers registered through
// transport.RegisterDialer, and a LockstepBot that consumes redundant frame
// broadcasts, submits inputs, and reports keyframe hashes — the client half
// of the kit lockstep Room, usable both for desync regression tests and for
// lockstep load testing.
package robot

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/tjbdwanghaibo/roost-core/robot/transport"

	quic "github.com/quic-go/quic-go"

	"github.com/tjbdwanghaibo/roost-kit/nettransport"
)

// KCPDialerConfig shapes the KCP client dialer. Encryption and FEC are
// mandatory, mirroring nettransport.DialKCP: the kit refuses plaintext KCP.
type KCPDialerConfig struct {
	// Key derives the AES-GCM block cipher; must match the server.
	Key []byte
	// DataShards/ParityShards are the FEC geometry; must match the server.
	DataShards   int
	ParityShards int
}

// RegisterKCPDialer registers a KCP dialer under transportType (use "kcp"),
// so runner configs select it with Transport.Type. The KCP session is a
// stream: the shared length-prefix packet framing runs on top unchanged.
func RegisterKCPDialer(transportType string, cfg KCPDialerConfig) error {
	block, err := nettransport.NewKCPAESGCM(cfg.Key)
	if err != nil {
		return fmt.Errorf("robot kcp dialer: %w", err)
	}
	return transport.RegisterDialer(transportType, func(ctx context.Context, tc transport.Config) (transport.Conn, error) {
		session, err := nettransport.DialKCP(tc.Endpoint, block, cfg.DataShards, cfg.ParityShards)
		if err != nil {
			return nil, fmt.Errorf("robot kcp dialer: dial %s: %w", tc.Endpoint, err)
		}
		if err := ctx.Err(); err != nil {
			_ = session.Close()
			return nil, err
		}
		return transport.NewTCPConn(session, tc.MaxPayloadSize), nil
	})
}

// QUICDialerConfig shapes the QUIC client dialer. TLS with ALPN is
// mandatory, mirroring nettransport.DialQUIC.
type QUICDialerConfig struct {
	TLS  *tls.Config
	QUIC *quic.Config
}

// RegisterQUICDialer registers a QUIC dialer under transportType (use
// "quic"). Each robot connection opens one bidirectional stream and runs the
// shared length-prefix packet framing over it.
func RegisterQUICDialer(transportType string, cfg QUICDialerConfig) error {
	if err := nettransport.ValidateQUICTLS(cfg.TLS); err != nil {
		return fmt.Errorf("robot quic dialer: %w", err)
	}
	return transport.RegisterDialer(transportType, func(ctx context.Context, tc transport.Config) (transport.Conn, error) {
		dialCtx, cancel := context.WithTimeout(ctx, tc.DialTimeout)
		defer cancel()
		conn, err := nettransport.DialQUIC(dialCtx, tc.Endpoint, cfg.TLS, cfg.QUIC)
		if err != nil {
			return nil, fmt.Errorf("robot quic dialer: dial %s: %w", tc.Endpoint, err)
		}
		stream, err := conn.OpenStreamSync(dialCtx)
		if err != nil {
			_ = conn.CloseWithError(0, "open stream failed")
			return nil, fmt.Errorf("robot quic dialer: open stream %s: %w", tc.Endpoint, err)
		}
		return &quicConn{conn: conn, stream: stream, maxPayloadSize: tc.MaxPayloadSize}, nil
	})
}

// quicConn frames packets over one bidirectional QUIC stream.
type quicConn struct {
	conn           *quic.Conn
	stream         *quic.Stream
	maxPayloadSize int
	writeMu        sync.Mutex
	closeOnce      sync.Once
}

func (c *quicConn) ReadPacket() (*transport.Packet, error) {
	packet, err := transport.ReadPacketFrom(c.stream, c.maxPayloadSize)
	if err != nil {
		// A peer-closed stream surfaces as a stream error; normalize to EOF
		// semantics like the TCP conn so the session loop shuts down calmly.
		var streamErr *quic.StreamError
		if errors.As(err, &streamErr) {
			return nil, io.EOF
		}
		return nil, err
	}
	return packet, nil
}

func (c *quicConn) WritePackets(packets []*transport.Packet) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return transport.WritePacketsTo(c.stream, packets)
}

func (c *quicConn) Close() error {
	var err error
	c.closeOnce.Do(func() {
		if c.stream != nil {
			_ = c.stream.Close()
		}
		if c.conn != nil {
			err = c.conn.CloseWithError(0, "robot closed")
		}
	})
	return err
}

func (c *quicConn) RemoteAddr() string {
	if c == nil || c.conn == nil || c.conn.RemoteAddr() == nil {
		return ""
	}
	return c.conn.RemoteAddr().String()
}

var _ transport.Conn = (*quicConn)(nil)
