// Package transport is the robot client's wire layer: a Packet framing
// shared by TCP and WebSocket (length-prefixed, little-endian), the Conn
// abstraction, and pluggable dialers. Ported from the cube robot service and
// de-coupled from any business protocol: payloads are opaque bytes, codecs
// live in robot/protocol.
package transport

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/websocket"
)

const (
	defaultMaxPayloadSize = 1 << 20
	packetHeaderSize      = 12
	packetBodyHeaderSize  = 8
)

var (
	ErrInvalidPacket = errors.New("robot transport: invalid packet")
	ErrPacketTooBig  = errors.New("robot transport: packet too big")
)

// Packet is the unified client protocol packet.
//
// Wire format:
//
//	[4B body_len][4B msg_id][4B seq][payload]
//
// body_len is msg_id + seq + payload length, little-endian. Seq 0 marks a
// server push; non-zero seq correlates a response to its request.
type Packet struct {
	MsgID   uint32
	Seq     uint32
	Payload []byte
}

// Conn abstracts a client transport. TCP and WebSocket share the same
// Packet framing above this layer; custom transports (KCP, QUIC — see
// roost-kit's robot package) only need to implement this interface.
type Conn interface {
	ReadPacket() (*Packet, error)
	WritePackets([]*Packet) error
	Close() error
	RemoteAddr() string
}

// Dialer opens a Conn for one robot. Custom transports register under a
// type name via RegisterDialer.
type Dialer func(context.Context, Config) (Conn, error)

// Config shapes one robot connection.
type Config struct {
	// Type selects the registered dialer: "tcp" (default), "ws"/"websocket",
	// or any custom-registered type.
	Type           string
	Endpoint       string
	MaxPayloadSize int
	DialTimeout    time.Duration
	// Origin and Headers apply to the websocket dialer.
	Origin  string
	Headers map[string]string
}

func (c Config) Normalize() Config {
	if strings.TrimSpace(c.Type) == "" {
		c.Type = "tcp"
	}
	c.Type = strings.ToLower(strings.TrimSpace(c.Type))
	if c.MaxPayloadSize <= 0 {
		c.MaxPayloadSize = defaultMaxPayloadSize
	}
	if c.DialTimeout <= 0 {
		c.DialTimeout = 5 * time.Second
	}
	if c.Origin == "" {
		c.Origin = "http://localhost/"
	}
	return c
}

var (
	dialersMu sync.RWMutex
	dialers   = map[string]Dialer{
		"tcp":       dialTCP,
		"ws":        dialWebSocket,
		"websocket": dialWebSocket,
	}
)

// RegisterDialer adds (or overrides) a dialer for a transport type — the
// hook roost-kit uses to plug KCP/QUIC client transports in.
func RegisterDialer(transportType string, dialer Dialer) error {
	transportType = strings.ToLower(strings.TrimSpace(transportType))
	if transportType == "" {
		return errors.New("robot transport: dialer type is required")
	}
	if dialer == nil {
		return fmt.Errorf("robot transport: dialer for %q is nil", transportType)
	}
	dialersMu.Lock()
	dialers[transportType] = dialer
	dialersMu.Unlock()
	return nil
}

// Dial opens a connection using the dialer registered for cfg.Type.
func Dial(ctx context.Context, cfg Config) (Conn, error) {
	cfg = cfg.Normalize()
	dialersMu.RLock()
	dialer := dialers[cfg.Type]
	dialersMu.RUnlock()
	if dialer == nil {
		return nil, fmt.Errorf("robot transport: unsupported type %q (registered: %v)", cfg.Type, DialerTypes())
	}
	return dialer(ctx, cfg)
}

// DialerTypes lists the registered transport types.
func DialerTypes() []string {
	dialersMu.RLock()
	defer dialersMu.RUnlock()
	types := make([]string, 0, len(dialers))
	for t := range dialers {
		types = append(types, t)
	}
	sort.Strings(types)
	return types
}

func dialTCP(ctx context.Context, cfg Config) (Conn, error) {
	if cfg.Endpoint == "" {
		return nil, errors.New("robot transport: tcp endpoint is required")
	}
	dialer := net.Dialer{Timeout: cfg.DialTimeout}
	conn, err := dialer.DialContext(ctx, "tcp", cfg.Endpoint)
	if err != nil {
		return nil, fmt.Errorf("robot transport: dial tcp %s: %w", cfg.Endpoint, err)
	}
	return NewTCPConn(conn, cfg.MaxPayloadSize), nil
}

func dialWebSocket(_ context.Context, cfg Config) (Conn, error) {
	if cfg.Endpoint == "" {
		return nil, errors.New("robot transport: websocket endpoint is required")
	}
	wsCfg, err := websocket.NewConfig(cfg.Endpoint, cfg.Origin)
	if err != nil {
		return nil, fmt.Errorf("robot transport: websocket config %s: %w", cfg.Endpoint, err)
	}
	for k, v := range cfg.Headers {
		wsCfg.Header.Set(k, v)
	}
	conn, err := websocket.DialConfig(wsCfg)
	if err != nil {
		return nil, fmt.Errorf("robot transport: dial websocket %s: %w", cfg.Endpoint, err)
	}
	return NewWebSocketConn(conn, cfg.MaxPayloadSize), nil
}

// --- Packet codec ---

// DecodePackets decodes one or more concatenated packets from data.
func DecodePackets(data []byte, maxPayloadSize int) ([]*Packet, error) {
	maxPayloadSize = normalizeMaxPayloadSize(maxPayloadSize)
	packets := make([]*Packet, 0, 1)
	for len(data) > 0 {
		if len(data) < packetHeaderSize {
			return nil, fmt.Errorf("%w: short header", ErrInvalidPacket)
		}
		bodyLen := binary.LittleEndian.Uint32(data[:4])
		if bodyLen < packetBodyHeaderSize {
			return nil, fmt.Errorf("%w: body length %d", ErrInvalidPacket, bodyLen)
		}
		payloadLen := int(bodyLen) - packetBodyHeaderSize
		if payloadLen > maxPayloadSize {
			return nil, fmt.Errorf("%w: payload %d > %d", ErrPacketTooBig, payloadLen, maxPayloadSize)
		}
		frameLen := 4 + int(bodyLen)
		if len(data) < frameLen {
			return nil, fmt.Errorf("%w: incomplete frame", ErrInvalidPacket)
		}
		payload := append([]byte(nil), data[packetHeaderSize:frameLen]...)
		packets = append(packets, &Packet{
			MsgID:   binary.LittleEndian.Uint32(data[4:8]),
			Seq:     binary.LittleEndian.Uint32(data[8:12]),
			Payload: payload,
		})
		data = data[frameLen:]
	}
	return packets, nil
}

// EncodePackets encodes one or more packets into concatenated wire frames.
func EncodePackets(packets []*Packet) []byte {
	totalSize := 0
	for _, p := range packets {
		if p != nil {
			totalSize += packetHeaderSize + len(p.Payload)
		}
	}
	data := make([]byte, 0, totalSize)
	for _, p := range packets {
		if p == nil {
			continue
		}
		bodyLen := uint32(packetBodyHeaderSize + len(p.Payload))
		var header [packetHeaderSize]byte
		binary.LittleEndian.PutUint32(header[0:4], bodyLen)
		binary.LittleEndian.PutUint32(header[4:8], p.MsgID)
		binary.LittleEndian.PutUint32(header[8:12], p.Seq)
		data = append(data, header[:]...)
		data = append(data, p.Payload...)
	}
	return data
}

// ReadPacketFrom reads one packet from r.
func ReadPacketFrom(r io.Reader, maxPayloadSize int) (*Packet, error) {
	maxPayloadSize = normalizeMaxPayloadSize(maxPayloadSize)
	var header [packetHeaderSize]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return nil, err
	}
	bodyLen := binary.LittleEndian.Uint32(header[0:4])
	if bodyLen < packetBodyHeaderSize {
		return nil, fmt.Errorf("%w: body length %d", ErrInvalidPacket, bodyLen)
	}
	payloadLen := int(bodyLen) - packetBodyHeaderSize
	if payloadLen > maxPayloadSize {
		return nil, fmt.Errorf("%w: payload %d > %d", ErrPacketTooBig, payloadLen, maxPayloadSize)
	}
	payload := make([]byte, payloadLen)
	if _, err := io.ReadFull(r, payload); err != nil {
		return nil, err
	}
	return &Packet{
		MsgID:   binary.LittleEndian.Uint32(header[4:8]),
		Seq:     binary.LittleEndian.Uint32(header[8:12]),
		Payload: payload,
	}, nil
}

// WritePacketsTo writes packets to w in the unified packet format.
func WritePacketsTo(w io.Writer, packets []*Packet) error {
	data := EncodePackets(packets)
	for len(data) > 0 {
		n, err := w.Write(data)
		if err != nil {
			return err
		}
		if n <= 0 {
			return io.ErrShortWrite
		}
		data = data[n:]
	}
	return nil
}

func normalizeMaxPayloadSize(size int) int {
	if size <= 0 {
		return defaultMaxPayloadSize
	}
	return size
}

// --- TCP ---

type TCPConn struct {
	conn           net.Conn
	maxPayloadSize int
	writeMu        sync.Mutex
	closeOnce      sync.Once
}

func NewTCPConn(conn net.Conn, maxPayloadSize int) *TCPConn {
	return &TCPConn{conn: conn, maxPayloadSize: normalizeMaxPayloadSize(maxPayloadSize)}
}

func (c *TCPConn) ReadPacket() (*Packet, error) {
	return ReadPacketFrom(c.conn, c.maxPayloadSize)
}

func (c *TCPConn) WritePackets(packets []*Packet) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return WritePacketsTo(c.conn, packets)
}

func (c *TCPConn) Close() error {
	var err error
	c.closeOnce.Do(func() { err = c.conn.Close() })
	return err
}

func (c *TCPConn) RemoteAddr() string {
	if c == nil || c.conn == nil || c.conn.RemoteAddr() == nil {
		return ""
	}
	return c.conn.RemoteAddr().String()
}

// --- WebSocket ---

type WebSocketConn struct {
	conn           *websocket.Conn
	maxPayloadSize int
	pending        []*Packet
	writeMu        sync.Mutex
	closeOnce      sync.Once
}

func NewWebSocketConn(conn *websocket.Conn, maxPayloadSize int) *WebSocketConn {
	return &WebSocketConn{conn: conn, maxPayloadSize: normalizeMaxPayloadSize(maxPayloadSize)}
}

func (c *WebSocketConn) ReadPacket() (*Packet, error) {
	for len(c.pending) == 0 {
		var data []byte
		if err := websocket.Message.Receive(c.conn, &data); err != nil {
			return nil, err
		}
		packets, err := DecodePackets(data, c.maxPayloadSize)
		if err != nil {
			return nil, err
		}
		c.pending = packets
	}
	packet := c.pending[0]
	c.pending = c.pending[1:]
	return packet, nil
}

func (c *WebSocketConn) WritePackets(packets []*Packet) error {
	data := EncodePackets(packets)
	if len(data) == 0 {
		return nil
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return websocket.Message.Send(c.conn, data)
}

func (c *WebSocketConn) Close() error {
	var err error
	c.closeOnce.Do(func() {
		if c.conn != nil {
			err = c.conn.Close()
		}
	})
	return err
}

func (c *WebSocketConn) RemoteAddr() string {
	if c == nil || c.conn == nil {
		return ""
	}
	if addr := c.conn.RemoteAddr(); addr != nil {
		return addr.String()
	}
	return ""
}
