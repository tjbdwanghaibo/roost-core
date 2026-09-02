package nettransport

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	core "github.com/tjbdwanghaibo/cube-core/statesync"
)

const DefaultUDPMaxPacketBytes = 1232 // IPv6 minimum MTU minus IPv6 + UDP headers

type UDPTransportConfig struct {
	PacketConn            net.PacketConn
	MaxPacketBytes        int
	AllowAddressMigration bool
	OwnPacketConn         bool
	ReadPollInterval      time.Duration
	OnReceiveError        func(error, net.Addr)
}

type udpRoute struct {
	registered bool
	address    net.Addr
	protector  *AEADSessionProtector
}

type UDPTransport struct {
	mu       sync.RWMutex
	config   UDPTransportConfig
	routes   map[core.SessionID]*udpRoute
	closed   bool
	serving  atomic.Bool
	closeOne sync.Once
	stats    udpCounters
}

type udpCounters struct {
	packetsSent     atomic.Uint64
	bytesSent       atomic.Uint64
	packetsReceived atomic.Uint64
	bytesReceived   atomic.Uint64
	authFailures    atomic.Uint64
	unknownSessions atomic.Uint64
	migrations      atomic.Uint64
	sendErrors      atomic.Uint64
	receiveErrors   atomic.Uint64
}

type UDPTransportStats struct {
	ActiveRoutes      int
	PacketsSent       uint64
	BytesSent         uint64
	PacketsReceived   uint64
	BytesReceived     uint64
	AuthFailures      uint64
	UnknownSessions   uint64
	AddressMigrations uint64
	SendErrors        uint64
	ReceiveErrors     uint64
}

type UDPReceiveHandler func(context.Context, core.SessionID, []byte, net.Addr) error

func NewUDPTransport(config UDPTransportConfig) (*UDPTransport, error) {
	if isNilInterface(config.PacketConn) {
		return nil, ErrProtocolConfig
	}
	if config.MaxPacketBytes <= 0 {
		config.MaxPacketBytes = DefaultUDPMaxPacketBytes
	}
	if config.MaxPacketBytes < 256 || config.MaxPacketBytes > 64<<10 {
		return nil, ErrProtocolConfig
	}
	if config.ReadPollInterval <= 0 {
		config.ReadPollInterval = time.Second
	}
	return &UDPTransport{config: config, routes: make(map[core.SessionID]*udpRoute)}, nil
}

func (transport *UDPTransport) RegisterSession(info core.SessionInfo) error {
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
		route = &udpRoute{}
		transport.routes[info.ID] = route
	}
	if route.registered {
		return ErrSessionAlreadyExists
	}
	route.registered = true
	return nil
}

// BindSession installs the authenticated endpoint and directional AEAD state.
// It may be called before RegisterSession, which is useful during handshake.
func (transport *UDPTransport) BindSession(session core.SessionID, address net.Addr, protector *AEADSessionProtector) error {
	if transport == nil || session == 0 || isNilInterface(address) || protector == nil || protector.Overhead() == 0 {
		return ErrProtocolConfig
	}
	transport.mu.Lock()
	defer transport.mu.Unlock()
	if transport.closed {
		return ErrTransportClosed
	}
	route := transport.routes[session]
	if route == nil {
		route = &udpRoute{}
		transport.routes[session] = route
	}
	if route.protector != nil && route.protector != protector {
		return ErrSessionAlreadyExists
	}
	route.address = cloneAddr(address)
	route.protector = protector
	return nil
}

func (transport *UDPTransport) RemoveSession(session core.SessionID) bool {
	if transport == nil || session == 0 {
		return false
	}
	transport.mu.Lock()
	_, exists := transport.routes[session]
	delete(transport.routes, session)
	transport.mu.Unlock()
	return exists
}

func (transport *UDPTransport) SendDatagram(ctx context.Context, session core.SessionID, payload []byte) error {
	if transport == nil {
		return ErrTransportClosed
	}
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	if len(payload) == 0 {
		return ErrPayloadTooLarge
	}
	address, protector, err := transport.route(session)
	if err != nil {
		return err
	}
	packet, err := protector.Seal(session, payload)
	if err != nil {
		return err
	}
	if len(packet) > transport.config.MaxPacketBytes {
		return fmt.Errorf("%w: protected UDP packet %d > %d", ErrPayloadTooLarge, len(packet), transport.config.MaxPacketBytes)
	}
	written, err := transport.config.PacketConn.WriteTo(packet, address)
	if err != nil {
		transport.stats.sendErrors.Add(1)
		return err
	}
	if written != len(packet) {
		transport.stats.sendErrors.Add(1)
		return fmt.Errorf("replication transport: short UDP write %d/%d", written, len(packet))
	}
	transport.stats.packetsSent.Add(1)
	transport.stats.bytesSent.Add(uint64(written))
	return nil
}

func (transport *UDPTransport) SendDatagramBatch(ctx context.Context, session core.SessionID, packets [][]byte) error {
	for _, packet := range packets {
		if err := transport.SendDatagram(ctx, session, packet); err != nil {
			return err
		}
	}
	return nil
}

func (*UDPTransport) SendReliable(context.Context, core.SessionID, []byte) error {
	return fmt.Errorf("%w: plain UDP has no reliable lane; compose another reliable sender", ErrProtocolConfig)
}

func (transport *UDPTransport) Serve(ctx context.Context, handler UDPReceiveHandler) error {
	if transport == nil || handler == nil {
		return ErrProtocolConfig
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if !transport.serving.CompareAndSwap(false, true) {
		return fmt.Errorf("%w: UDP receive loop already running", ErrProtocolConfig)
	}
	defer transport.serving.Store(false)
	buffer := make([]byte, transport.config.MaxPacketBytes)
	for {
		transport.mu.RLock()
		closed := transport.closed
		transport.mu.RUnlock()
		if closed {
			return ErrTransportClosed
		}
		if err := transport.config.PacketConn.SetReadDeadline(time.Now().Add(transport.config.ReadPollInterval)); err != nil {
			return err
		}
		length, address, err := transport.config.PacketConn.ReadFrom(buffer)
		if err != nil {
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
			}
			var networkError net.Error
			if errors.As(err, &networkError) && networkError.Timeout() {
				continue
			}
			transport.mu.RLock()
			closed := transport.closed
			transport.mu.RUnlock()
			if closed {
				return ErrTransportClosed
			}
			transport.receiveError(err, address)
			return err
		}
		if length < udpEnvelopeHeaderSize {
			transport.stats.authFailures.Add(1)
			transport.receiveError(ErrAuthentication, address)
			continue
		}
		packet := append([]byte(nil), buffer[:length]...)
		session := core.SessionID(binary.BigEndian.Uint64(packet[0:8]))
		route, routeErr := transport.receiveRoute(session)
		if routeErr != nil {
			transport.stats.unknownSessions.Add(1)
			transport.receiveError(routeErr, address)
			continue
		}
		if !sameAddr(route.address, address) && !transport.config.AllowAddressMigration {
			transport.stats.authFailures.Add(1)
			transport.receiveError(ErrAuthentication, address)
			continue
		}
		payload, openErr := route.protector.Open(session, packet)
		if openErr != nil {
			transport.stats.authFailures.Add(1)
			transport.receiveError(openErr, address)
			continue
		}
		if !transport.isCurrentRoute(session, route.protector) {
			transport.stats.unknownSessions.Add(1)
			transport.receiveError(ErrSessionNotRegistered, address)
			continue
		}
		if !sameAddr(route.address, address) {
			if !transport.migrate(session, route.protector, address) {
				transport.stats.unknownSessions.Add(1)
				continue
			}
		}
		transport.stats.packetsReceived.Add(1)
		transport.stats.bytesReceived.Add(uint64(length))
		if err := handler(ctx, session, payload, address); err != nil {
			return err
		}
	}
}

func (transport *UDPTransport) isCurrentRoute(session core.SessionID, protector *AEADSessionProtector) bool {
	transport.mu.RLock()
	defer transport.mu.RUnlock()
	route := transport.routes[session]
	return !transport.closed && route != nil && route.registered && route.protector == protector
}

func (transport *UDPTransport) Close() error {
	if transport == nil {
		return nil
	}
	var err error
	transport.closeOne.Do(func() {
		transport.mu.Lock()
		transport.closed = true
		clear(transport.routes)
		transport.mu.Unlock()
		if transport.config.OwnPacketConn {
			err = transport.config.PacketConn.Close()
		}
	})
	return err
}

func (transport *UDPTransport) Stats() UDPTransportStats {
	if transport == nil {
		return UDPTransportStats{}
	}
	transport.mu.RLock()
	active := 0
	for _, route := range transport.routes {
		if route.registered && route.address != nil && route.protector != nil {
			active++
		}
	}
	transport.mu.RUnlock()
	return UDPTransportStats{
		ActiveRoutes: active, PacketsSent: transport.stats.packetsSent.Load(), BytesSent: transport.stats.bytesSent.Load(),
		PacketsReceived: transport.stats.packetsReceived.Load(), BytesReceived: transport.stats.bytesReceived.Load(),
		AuthFailures: transport.stats.authFailures.Load(), UnknownSessions: transport.stats.unknownSessions.Load(),
		AddressMigrations: transport.stats.migrations.Load(), SendErrors: transport.stats.sendErrors.Load(),
		ReceiveErrors: transport.stats.receiveErrors.Load(),
	}
}

func (transport *UDPTransport) route(session core.SessionID) (net.Addr, *AEADSessionProtector, error) {
	transport.mu.RLock()
	defer transport.mu.RUnlock()
	if transport.closed {
		return nil, nil, ErrTransportClosed
	}
	route := transport.routes[session]
	if route == nil || !route.registered || route.address == nil || route.protector == nil {
		return nil, nil, ErrRouteNotBound
	}
	return cloneAddr(route.address), route.protector, nil
}

func (transport *UDPTransport) receiveRoute(session core.SessionID) (udpRoute, error) {
	transport.mu.RLock()
	defer transport.mu.RUnlock()
	route := transport.routes[session]
	if transport.closed || route == nil || !route.registered || route.protector == nil {
		return udpRoute{}, ErrSessionNotRegistered
	}
	return udpRoute{registered: true, address: cloneAddr(route.address), protector: route.protector}, nil
}

func (transport *UDPTransport) migrate(session core.SessionID, protector *AEADSessionProtector, address net.Addr) bool {
	transport.mu.Lock()
	defer transport.mu.Unlock()
	if route := transport.routes[session]; !transport.closed && route != nil && route.registered && route.protector == protector {
		route.address = cloneAddr(address)
		transport.stats.migrations.Add(1)
		return true
	}
	return false
}

func (transport *UDPTransport) receiveError(err error, address net.Addr) {
	transport.stats.receiveErrors.Add(1)
	if transport.config.OnReceiveError != nil {
		func() {
			defer func() { _ = recover() }()
			transport.config.OnReceiveError(err, address)
		}()
	}
}

func cloneAddr(address net.Addr) net.Addr {
	switch value := address.(type) {
	case *net.UDPAddr:
		clone := *value
		clone.IP = append(net.IP(nil), value.IP...)
		return &clone
	default:
		return address
	}
}

func sameAddr(left, right net.Addr) bool {
	return left != nil && right != nil && left.Network() == right.Network() && left.String() == right.String()
}

var _ core.Transport = (*UDPTransport)(nil)
var _ core.DatagramBatchTransport = (*UDPTransport)(nil)
var _ core.SessionTransport = (*UDPTransport)(nil)
