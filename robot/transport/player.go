package transport

import (
	"context"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tjbdwanghaibo/roost-core/client/wire"
)

// PlayerTCPType selects the shared embedded TCP / Gate wire adapter.
const PlayerTCPType = "player-tcp"

// AuthMessageID is the message id the server reserves for the handshake
// frame and its acknowledgement.
const AuthMessageID uint32 = 0

// AuthPacket 是 AuthProvider 返回的握手首包：票据外面套一层连接代号信封（wire.EncodeAuth），代号取
// NextConnectionGeneration，所以每次连接（包括重连）都要重新调用一次，不能缓存复用同一个包。服务端在鉴权前剥掉
// 信封，业务 authenticator 只看到票据原文；同一 SessionID 的两条连接落在不同 Gate 实例时，Game 按代号认出迟到
// 完成的旧握手（GATEWAY §3）。序列号在写出时分配。票据为空或以 0x00 开头时原样发出，由服务端拒绝。
func AuthPacket(token string) *Packet {
	payload, err := wire.EncodeAuth(token, NextConnectionGeneration())
	if err != nil {
		payload = []byte(token)
	}
	return &Packet{MsgID: AuthMessageID, Payload: payload}
}

// lastConnectionGeneration 是本进程最近一次发出的连接代号。
var lastConnectionGeneration atomic.Uint64

// NextConnectionGeneration 返回一个新的连接代号：max(上一个 + 1, 当前 Unix 纳秒)。
//
// 进程内严格递增，与时钟无关（时钟回拨时继续 +1）。进程重启后不需要持久状态也仍然更大：重启至少耗时毫秒级，
// 新进程的第一个代号取自当时的时钟，大于旧进程发出过的任何代号——除非旧进程每纳秒发起一次以上连接（不可能），
// 或重启前后时钟被往回拨过。往回拨的后果有界：新进程的握手只在与旧进程仍在途的握手重叠时可能被当成旧的拒掉一次，
// 重连即恢复；已确认的旧绑定挡不住之后到达的新连接（bindingTable.finish）。
func NextConnectionGeneration() uint64 {
	for {
		last := lastConnectionGeneration.Load()
		next := max(last+1, uint64(time.Now().UnixNano()))
		if lastConnectionGeneration.CompareAndSwap(last, next) {
			return next
		}
	}
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
