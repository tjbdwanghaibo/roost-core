package nettransport

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	quic "github.com/quic-go/quic-go"
	core "github.com/tjbdwanghaibo/cube-core/statesync"
)

const DefaultQUICALPN = "cube-replication-v1"

type QUICTransportConfig struct {
	MaxDatagramBytes    int
	MaxReliableBytes    int
	PreserveConnections bool
	CloseErrorCode      quic.ApplicationErrorCode
}

type quicRoute struct {
	registered bool
	connection *quic.Conn

	sendMu     sync.Mutex
	sendStream *quic.SendStream
	receiveMu  sync.Mutex
	receive    *quic.ReceiveStream
}

type QUICTransport struct {
	mu     sync.RWMutex
	config QUICTransportConfig
	routes map[core.SessionID]*quicRoute
	closed bool
	stats  quicCounters
}

type quicCounters struct {
	datagramsSent     atomic.Uint64
	datagramBytesSent atomic.Uint64
	reliableSent      atomic.Uint64
	reliableBytesSent atomic.Uint64
	datagramsReceived atomic.Uint64
	reliableReceived  atomic.Uint64
	sendErrors        atomic.Uint64
	receiveErrors     atomic.Uint64
}

type QUICTransportStats struct {
	ActiveRoutes             int
	DatagramsSent            uint64
	DatagramBytesSent        uint64
	ReliableMessagesSent     uint64
	ReliableBytesSent        uint64
	DatagramsReceived        uint64
	ReliableMessagesReceived uint64
	SendErrors               uint64
	ReceiveErrors            uint64
}

func NewQUICTransport(config QUICTransportConfig) *QUICTransport {
	if config.MaxDatagramBytes <= 0 {
		config.MaxDatagramBytes = core.DefaultMaxDatagram
	}
	if config.MaxReliableBytes <= 0 {
		config.MaxReliableBytes = 1 << 20
	}
	if config.CloseErrorCode == 0 {
		config.CloseErrorCode = quic.ApplicationErrorCode(0xc001)
	}
	return &QUICTransport{config: config, routes: make(map[core.SessionID]*quicRoute)}
}

func (transport *QUICTransport) RegisterSession(info core.SessionInfo) error {
	if transport == nil || info.ID == 0 {
		return ErrSessionNotRegistered
	}
	transport.mu.Lock()
	defer transport.mu.Unlock()
	if transport.closed {
		return ErrTransportClosed
	}
	route := transport.routes[info.ID]
	if route == nil {
		route = &quicRoute{}
		transport.routes[info.ID] = route
	}
	if route.registered {
		return ErrSessionAlreadyExists
	}
	route.registered = true
	return nil
}

// BindSession associates an authenticated QUIC connection with a replication
// session. QUIC DATAGRAM support must have been negotiated by both peers.
func (transport *QUICTransport) BindSession(session core.SessionID, connection *quic.Conn) error {
	if transport == nil || session == 0 || connection == nil {
		return ErrProtocolConfig
	}
	datagramSupport := connection.ConnectionState().SupportsDatagrams
	if !datagramSupport.Local || !datagramSupport.Remote {
		return fmt.Errorf("%w: QUIC peer did not negotiate DATAGRAM support", ErrProtocolConfig)
	}
	transport.mu.Lock()
	if transport.closed {
		transport.mu.Unlock()
		return ErrTransportClosed
	}
	route := transport.routes[session]
	if route == nil {
		route = &quicRoute{}
		transport.routes[session] = route
	}
	if route.connection != nil && route.connection != connection {
		transport.mu.Unlock()
		return ErrSessionAlreadyExists
	}
	if route.connection == connection {
		transport.mu.Unlock()
		return nil
	}
	for otherSession, otherRoute := range transport.routes {
		if otherSession != session && otherRoute.connection == connection {
			transport.mu.Unlock()
			return fmt.Errorf("%w: QUIC connection is already bound to session %d", ErrSessionAlreadyExists, otherSession)
		}
	}
	route.connection = connection
	route.sendStream = nil
	route.receive = nil
	transport.mu.Unlock()
	return nil
}

func (transport *QUICTransport) RemoveSession(session core.SessionID) bool {
	if transport == nil || session == 0 {
		return false
	}
	transport.mu.Lock()
	route, exists := transport.routes[session]
	delete(transport.routes, session)
	transport.mu.Unlock()
	if exists && route.connection != nil && !transport.config.PreserveConnections {
		_ = route.connection.CloseWithError(transport.config.CloseErrorCode, "replication session removed")
	}
	return exists
}

func (transport *QUICTransport) SendDatagram(ctx context.Context, session core.SessionID, payload []byte) error {
	if transport == nil {
		return ErrTransportClosed
	}
	if len(payload) == 0 || len(payload) > transport.config.MaxDatagramBytes {
		return ErrPayloadTooLarge
	}
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	connection, err := transport.connection(session)
	if err != nil {
		return err
	}
	if err := connection.SendDatagram(payload); err != nil {
		transport.stats.sendErrors.Add(1)
		return err
	}
	transport.stats.datagramsSent.Add(1)
	transport.stats.datagramBytesSent.Add(uint64(len(payload)))
	return nil
}

func (transport *QUICTransport) SendDatagramBatch(ctx context.Context, session core.SessionID, packets [][]byte) error {
	for _, packet := range packets {
		if err := transport.SendDatagram(ctx, session, packet); err != nil {
			return err
		}
	}
	return nil
}

func (transport *QUICTransport) SendReliable(ctx context.Context, session core.SessionID, payload []byte) error {
	if transport == nil {
		return ErrTransportClosed
	}
	if len(payload) == 0 || len(payload) > transport.config.MaxReliableBytes {
		return ErrReliableMessageTooBig
	}
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	route, err := transport.route(session)
	if err != nil {
		return err
	}
	route.sendMu.Lock()
	defer route.sendMu.Unlock()
	if route.sendStream == nil {
		stream, openErr := route.connection.OpenUniStreamSync(ctx)
		if openErr != nil {
			transport.stats.sendErrors.Add(1)
			return openErr
		}
		route.sendStream = stream
	}
	stream := route.sendStream
	if deadline, ok := ctx.Deadline(); ok {
		if err := stream.SetWriteDeadline(deadline); err != nil {
			return err
		}
		defer stream.SetWriteDeadline(time.Time{})
	}
	defer interruptWriteOnCancel(ctx, stream)()
	header := make([]byte, 4)
	binary.BigEndian.PutUint32(header, uint32(len(payload)))
	if err := writeAll(stream, header); err != nil {
		transport.stats.sendErrors.Add(1)
		stream.CancelWrite(quic.StreamErrorCode(0xc101))
		route.sendStream = nil
		return err
	}
	if err := writeAll(stream, payload); err != nil {
		transport.stats.sendErrors.Add(1)
		stream.CancelWrite(quic.StreamErrorCode(0xc101))
		route.sendStream = nil
		return err
	}
	transport.stats.reliableSent.Add(1)
	transport.stats.reliableBytesSent.Add(uint64(len(payload)))
	return nil
}

func (transport *QUICTransport) ReceiveDatagram(ctx context.Context, session core.SessionID) ([]byte, error) {
	if transport == nil {
		return nil, ErrTransportClosed
	}
	if ctx == nil {
		ctx = context.Background()
	}
	connection, err := transport.connection(session)
	if err != nil {
		return nil, err
	}
	payload, err := connection.ReceiveDatagram(ctx)
	if err != nil {
		transport.stats.receiveErrors.Add(1)
		return nil, err
	}
	if len(payload) == 0 || len(payload) > transport.config.MaxDatagramBytes {
		transport.stats.receiveErrors.Add(1)
		return nil, ErrPayloadTooLarge
	}
	transport.stats.datagramsReceived.Add(1)
	return append([]byte(nil), payload...), nil
}

func (transport *QUICTransport) ReceiveReliable(ctx context.Context, session core.SessionID) ([]byte, error) {
	if transport == nil {
		return nil, ErrTransportClosed
	}
	if ctx == nil {
		ctx = context.Background()
	}
	route, err := transport.route(session)
	if err != nil {
		return nil, err
	}
	route.receiveMu.Lock()
	defer route.receiveMu.Unlock()
	if route.receive == nil {
		stream, acceptErr := route.connection.AcceptUniStream(ctx)
		if acceptErr != nil {
			transport.stats.receiveErrors.Add(1)
			return nil, acceptErr
		}
		route.receive = stream
	}
	stream := route.receive
	if deadline, ok := ctx.Deadline(); ok {
		if err := stream.SetReadDeadline(deadline); err != nil {
			return nil, err
		}
		defer stream.SetReadDeadline(time.Time{})
	}
	defer interruptReadOnCancel(ctx, stream)()
	header := make([]byte, 4)
	if _, err := io.ReadFull(stream, header); err != nil {
		transport.stats.receiveErrors.Add(1)
		stream.CancelRead(quic.StreamErrorCode(0xc102))
		route.receive = nil
		return nil, err
	}
	length := binary.BigEndian.Uint32(header)
	if length == 0 || uint64(length) > uint64(transport.config.MaxReliableBytes) {
		transport.stats.receiveErrors.Add(1)
		stream.CancelRead(quic.StreamErrorCode(0xc103))
		route.receive = nil
		return nil, ErrReliableMessageTooBig
	}
	payload := make([]byte, int(length))
	if _, err := io.ReadFull(stream, payload); err != nil {
		transport.stats.receiveErrors.Add(1)
		stream.CancelRead(quic.StreamErrorCode(0xc104))
		route.receive = nil
		return nil, err
	}
	transport.stats.reliableReceived.Add(1)
	return payload, nil
}

func (transport *QUICTransport) Close() error {
	if transport == nil {
		return nil
	}
	transport.mu.Lock()
	if transport.closed {
		transport.mu.Unlock()
		return nil
	}
	transport.closed = true
	routes := transport.routes
	transport.routes = make(map[core.SessionID]*quicRoute)
	transport.mu.Unlock()
	if !transport.config.PreserveConnections {
		for _, route := range routes {
			if route.connection != nil {
				_ = route.connection.CloseWithError(transport.config.CloseErrorCode, "replication transport closed")
			}
		}
	}
	return nil
}

func (transport *QUICTransport) Stats() QUICTransportStats {
	if transport == nil {
		return QUICTransportStats{}
	}
	transport.mu.RLock()
	active := 0
	for _, route := range transport.routes {
		if route.registered && route.connection != nil {
			active++
		}
	}
	transport.mu.RUnlock()
	return QUICTransportStats{
		ActiveRoutes: active, DatagramsSent: transport.stats.datagramsSent.Load(), DatagramBytesSent: transport.stats.datagramBytesSent.Load(),
		ReliableMessagesSent: transport.stats.reliableSent.Load(), ReliableBytesSent: transport.stats.reliableBytesSent.Load(),
		DatagramsReceived: transport.stats.datagramsReceived.Load(), ReliableMessagesReceived: transport.stats.reliableReceived.Load(),
		SendErrors: transport.stats.sendErrors.Load(), ReceiveErrors: transport.stats.receiveErrors.Load(),
	}
}

func (transport *QUICTransport) route(session core.SessionID) (*quicRoute, error) {
	if transport == nil {
		return nil, ErrTransportClosed
	}
	transport.mu.RLock()
	defer transport.mu.RUnlock()
	if transport.closed {
		return nil, ErrTransportClosed
	}
	route := transport.routes[session]
	if route == nil || !route.registered || route.connection == nil {
		return nil, ErrRouteNotBound
	}
	return route, nil
}

func (transport *QUICTransport) connection(session core.SessionID) (*quic.Conn, error) {
	route, err := transport.route(session)
	if err != nil {
		return nil, err
	}
	return route.connection, nil
}

func writeAll(writer io.Writer, payload []byte) error {
	for len(payload) > 0 {
		written, err := writer.Write(payload)
		if err != nil {
			return err
		}
		if written <= 0 {
			return io.ErrShortWrite
		}
		payload = payload[written:]
	}
	return nil
}

func QUICConfig(config *quic.Config) *quic.Config {
	if config == nil {
		config = &quic.Config{}
	} else {
		config = config.Clone()
	}
	config.EnableDatagrams = true
	return config
}

func ValidateQUICTLS(config *tls.Config) error {
	if config == nil || len(config.NextProtos) == 0 {
		return fmt.Errorf("%w: QUIC TLS NextProtos / ALPN is required", ErrProtocolConfig)
	}
	return nil
}

func ListenQUIC(address string, tlsConfig *tls.Config, config *quic.Config) (*quic.Listener, error) {
	if address == "" {
		return nil, ErrProtocolConfig
	}
	if err := ValidateQUICTLS(tlsConfig); err != nil {
		return nil, err
	}
	return quic.ListenAddr(address, tlsConfig, QUICConfig(config))
}

func DialQUIC(ctx context.Context, address string, tlsConfig *tls.Config, config *quic.Config) (*quic.Conn, error) {
	if address == "" {
		return nil, ErrProtocolConfig
	}
	if err := ValidateQUICTLS(tlsConfig); err != nil {
		return nil, err
	}
	return quic.DialAddr(ctx, address, tlsConfig, QUICConfig(config))
}

var _ core.Transport = (*QUICTransport)(nil)
var _ core.DatagramBatchTransport = (*QUICTransport)(nil)
var _ core.SessionTransport = (*QUICTransport)(nil)
