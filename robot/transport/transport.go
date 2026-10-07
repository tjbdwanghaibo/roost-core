// Package transport is the robot client's wire layer: a Packet framing
// shared by TCP and WebSocket (RS v2, big-endian), the Conn
// abstraction, and pluggable dialers. Ported from the cube robot service and
// de-coupled from any business protocol: payloads are opaque bytes, codecs
// live in robot/protocol.
package transport

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/tjbdwanghaibo/roost-core/client/wire"
	"golang.org/x/net/websocket"
)

const defaultMaxPayloadSize = wire.DefaultMaxPayload

var (
	ErrInvalidPacket = wire.ErrInvalidPacket
	ErrPacketTooBig  = wire.ErrPacketTooBig
)

// Packet 是公共RS v2包，Flags区分服务器推送与PB/Sync载荷。
type Packet = wire.Packet

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

// dialWebSocket 的 DialTimeout 覆盖 TCP 建连加 HTTP 升级握手，并服从调用方 ctx
// （RR-20261005-NC-163）。旧实现用 websocket.DialConfig，两者都不看：端点接受 TCP 却不回升级
// 响应时握手无限阻塞，connect 不返回，runner.Stop 也停不下这个机器人。
func dialWebSocket(ctx context.Context, cfg Config) (Conn, error) {
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
	if ctx == nil {
		ctx = context.Background()
	}
	if cfg.DialTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, cfg.DialTimeout)
		defer cancel()
	}
	wsCfg.Dialer = &net.Dialer{}
	conn, err := wsCfg.DialContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("robot transport: dial websocket %s: %w", cfg.Endpoint, err)
	}
	return NewWebSocketConn(conn, cfg.MaxPayloadSize), nil
}

// DecodePackets 只读公共RS v2包，不猜旧包格式。
func DecodePackets(data []byte, maxPayloadSize int) ([]*Packet, error) {
	return wire.Decode(data, maxPayloadSize)
}
func EncodePackets(packets []*Packet) ([]byte, error) { return wire.Encode(packets, 0) }
func ReadPacketFrom(r io.Reader, maxPayloadSize int) (*Packet, error) {
	return wire.Read(r, maxPayloadSize)
}
func WritePacketsTo(w io.Writer, packets []*Packet) error { return wire.Write(w, packets, 0) }
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
	return wire.Write(c.conn, packets, c.maxPayloadSize)
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
	data, err := wire.Encode(packets, c.maxPayloadSize)
	if err != nil {
		return err
	}
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
