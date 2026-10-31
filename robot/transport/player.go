package transport

import (
	"context"
	"fmt"
	"net"
	"sync"

	"github.com/tjbdwanghaibo/roost-core/client/wire"
)

// PlayerTCPType selects the shared embedded TCP / Gate wire adapter.
const PlayerTCPType = "player-tcp"

// AuthMessageID is the message id the server reserves for the handshake
// frame and its acknowledgement.
const AuthMessageID uint32 = 0

// AuthPacket is what an AuthProvider returns: the token the server hands to
// the application authenticator. Its sequence is assigned on the wire.
func AuthPacket(token string) *Packet {
	return &Packet{MsgID: AuthMessageID, Payload: []byte(token)}
}

func DialPlayerTCP(ctx context.Context, cfg Config) (Conn, error) {
	cfg = cfg.Normalize()
	dialer := net.Dialer{Timeout: cfg.DialTimeout}
	raw, err := dialer.DialContext(ctx, "tcp", cfg.Endpoint)
	if err != nil {
		return nil, err
	}
	if tcp, ok := raw.(*net.TCPConn); ok {
		_ = tcp.SetNoDelay(true)
	}
	return &PlayerConn{conn: cfg.Traffic.wrap(raw), maxPayload: cfg.MaxPayloadSize, sequences: make(map[uint32]uint32)}, nil
}

// PlayerConn adapts one socket. The server demands a strictly increasing, non-zero
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
type PlayerConn struct {
	conn       net.Conn
	maxPayload int

	writeMu sync.Mutex
	next    uint32

	mu        sync.Mutex
	sequences map[uint32]uint32
}

func (c *PlayerConn) WritePackets(packets []*Packet) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	// 整批先校验再登记序列，坏输入不能留下永远没有应答的映射。
	next := c.next
	framed := make([]*wire.Packet, 0, len(packets))
	mappings := make(map[uint32]uint32)
	for _, packet := range packets {
		if packet == nil {
			continue
		}
		next++
		if next == 0 {
			_ = c.conn.Close()
			return fmt.Errorf("robot player transport: wire sequence exhausted; reconnect required")
		}
		framed = append(framed, &wire.Packet{Flags: packet.Flags, MsgID: packet.MsgID, Seq: next, Payload: packet.Payload})
		if packet.Seq != 0 {
			mappings[next] = packet.Seq
		}
	}
	data, err := wire.Encode(framed, c.maxPayload)
	if err != nil {
		return err
	}
	if len(data) == 0 {
		return nil
	}
	c.next = next
	c.mu.Lock()
	if len(c.sequences)+len(mappings) > 4096 {
		c.mu.Unlock()
		_ = c.conn.Close()
		return fmt.Errorf("robot player transport: too many pending wire sequences")
	}
	for sequence, original := range mappings {
		c.sequences[sequence] = original
	}
	c.mu.Unlock()
	buffers := net.Buffers{data}
	_, err = buffers.WriteTo(c.conn)
	if err != nil {
		_ = c.conn.Close()
	} // 半包后不能继续复用流。
	return err
}

func (c *PlayerConn) ReadPacket() (*Packet, error) {
	for {
		packet, err := wire.Read(c.conn, c.maxPayload)
		if err != nil {
			return nil, err
		}
		sequence := uint32(0)
		if packet.Flags&wire.FlagPush == 0 && packet.Seq != 0 {
			c.mu.Lock()
			original, found := c.sequences[packet.Seq]
			delete(c.sequences, packet.Seq)
			c.mu.Unlock()
			if !found && packet.MsgID != AuthMessageID {
				continue
			}
			sequence = original
		}
		packet.Seq = sequence
		return packet, nil
	}
}

// ForgetRequest 释放超时请求的关联；迟到回包直接丢弃，不能变成同 ID 的业务推送。
func (c *PlayerConn) ForgetRequest(original uint32) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for wireSeq, seq := range c.sequences {
		if seq == original {
			delete(c.sequences, wireSeq)
		}
	}
}

func (c *PlayerConn) Close() error { return c.conn.Close() }

func (c *PlayerConn) RemoteAddr() string { return c.conn.RemoteAddr().String() }

var _ Conn = (*PlayerConn)(nil)
