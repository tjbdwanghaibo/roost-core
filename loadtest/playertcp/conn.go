// Package playertcp is the client half of this project's player TCP framing,
// shaped as a roost-core/robot transport so the robot runner can drive the
// real listener.
//
// roost-core's robot ships a 12-byte little-endian frame (body length, msg
// id, seq) that generic echo servers speak; the generated player TCP server
// speaks a 16-byte big-endian frame with a magic, a version and a flags byte,
// and requires the handshake first. This package translates between the two,
// and it is the only place that knows both — keep the constants below in step
// with internal/access/player/tcp/server_gen.go.
package playertcp

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"

	"github.com/tjbdwanghaibo/roost-core/robot/transport"
)

// TransportType is the value for transport.Config.Type.
const TransportType = "player-tcp"

// AuthMessageID is the message id the server reserves for the handshake
// frame and its acknowledgement.
const AuthMessageID uint32 = 0

const (
	headerSize           = 16
	protocolVersion byte = 1
	flagServerPush  byte = 1
)

var (
	frameMagic      = [2]byte{'R', 'S'}
	errInvalidFrame = errors.New("player tcp: invalid frame")
)

// Register installs the dialer. Call it once before building a runner.
func Register() error { return transport.RegisterDialer(TransportType, dial) }

// MustRegister is Register panicking on error.
func MustRegister() {
	if err := Register(); err != nil {
		panic(err)
	}
}

// AuthPacket is what an AuthProvider returns: the token the server hands to
// the application authenticator. Its sequence is assigned on the wire.
func AuthPacket(token string) *transport.Packet {
	return &transport.Packet{MsgID: AuthMessageID, Payload: []byte(token)}
}

func dial(ctx context.Context, cfg transport.Config) (transport.Conn, error) {
	dialer := net.Dialer{Timeout: cfg.DialTimeout}
	raw, err := dialer.DialContext(ctx, "tcp", cfg.Endpoint)
	if err != nil {
		return nil, err
	}
	if tcp, ok := raw.(*net.TCPConn); ok {
		_ = tcp.SetNoDelay(true)
	}
	return &Conn{conn: raw, maxPayload: cfg.MaxPayloadSize, sequences: make(map[uint32]uint32)}, nil
}

// Conn adapts one socket. The server demands a strictly increasing, non-zero
// sequence on every client frame — including the handshake and fire-and-forget
// notifies, which the robot sends with Seq 0 — so the wire sequence is this
// Conn's own counter, and a map remembers which robot sequence each wire
// sequence carried so the response can be handed back under the sequence the
// robot session is waiting on. A server push is told apart by the flags byte,
// not by its sequence: the server numbers pushes from its own per-session
// counter, which starts at 1 like this Conn's, so a world-chat push arriving
// while a call is in flight can carry exactly the number that call is waiting
// on. Pushes surface to the robot with Seq 0, which is what its session
// routes to wait_push.
type Conn struct {
	conn       net.Conn
	maxPayload int

	writeMu sync.Mutex
	next    uint32

	mu        sync.Mutex
	sequences map[uint32]uint32
}

func (c *Conn) WritePackets(packets []*transport.Packet) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	var buffers net.Buffers
	for _, packet := range packets {
		if packet == nil {
			continue
		}
		c.next++
		if c.next == 0 {
			c.next = 1
		}
		wire := c.next
		if packet.Seq != 0 {
			c.mu.Lock()
			c.sequences[wire] = packet.Seq
			c.mu.Unlock()
		}
		header := make([]byte, headerSize)
		header[0], header[1], header[2] = frameMagic[0], frameMagic[1], protocolVersion
		binary.BigEndian.PutUint32(header[4:8], packet.MsgID)
		binary.BigEndian.PutUint32(header[8:12], wire)
		binary.BigEndian.PutUint32(header[12:16], uint32(len(packet.Payload)))
		buffers = append(buffers, header, packet.Payload)
	}
	if len(buffers) == 0 {
		return nil
	}
	_, err := buffers.WriteTo(c.conn)
	return err
}

func (c *Conn) ReadPacket() (*transport.Packet, error) {
	var header [headerSize]byte
	if _, err := io.ReadFull(c.conn, header[:]); err != nil {
		return nil, err
	}
	if header[0] != frameMagic[0] || header[1] != frameMagic[1] || header[2] != protocolVersion || header[3]&^flagServerPush != 0 {
		return nil, errInvalidFrame
	}
	length := binary.BigEndian.Uint32(header[12:16])
	if c.maxPayload > 0 && int(length) > c.maxPayload {
		return nil, fmt.Errorf("%w: payload %d exceeds %d", errInvalidFrame, length, c.maxPayload)
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(c.conn, payload); err != nil {
		return nil, err
	}
	wire := binary.BigEndian.Uint32(header[8:12])
	seq := uint32(0)
	if header[3]&flagServerPush == 0 && wire != 0 {
		c.mu.Lock()
		if robotSeq, ok := c.sequences[wire]; ok {
			seq = robotSeq
			delete(c.sequences, wire)
		}
		c.mu.Unlock()
	}
	// The handshake acknowledgement comes back as msg 0 under the handshake's
	// wire sequence, which no robot call is waiting on, so it surfaces as a
	// push for msg 0 that nothing subscribes to — and is dropped there.
	return &transport.Packet{MsgID: binary.BigEndian.Uint32(header[4:8]), Seq: seq, Payload: payload}, nil
}

func (c *Conn) Close() error { return c.conn.Close() }

func (c *Conn) RemoteAddr() string { return c.conn.RemoteAddr().String() }

var _ transport.Conn = (*Conn)(nil)
