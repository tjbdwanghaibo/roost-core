package nettransport

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	core "github.com/tjbdwanghaibo/cube-core/statesync"
	kcp "github.com/xtaci/kcp-go/v5"
)

type KCPTransportConfig struct {
	MaxDatagramBytes  int
	MaxReliableBytes  int
	MTU               int
	SendWindow        int
	ReceiveWindow     int
	NoDelay           int
	Interval          int
	FastResend        int
	DisableCongestion int
	ACKNoDelay        bool
	WriteDelay        bool
	DSCP              int
	RateLimitBytes    uint32
	PreserveSessions  bool
}

func DefaultKCPTransportConfig() KCPTransportConfig {
	return KCPTransportConfig{
		MaxDatagramBytes: core.DefaultMaxDatagram, MaxReliableBytes: 1 << 20,
		MTU: 1400, SendWindow: 128, ReceiveWindow: 128,
		NoDelay: 1, Interval: 20, FastResend: 2, DisableCongestion: 0,
		ACKNoDelay: true, WriteDelay: false,
	}
}

type kcpRoute struct {
	registered bool
	session    *kcp.UDPSession
	binding    *kcp.UDPSession
	sendMu     sync.Mutex
	receiveMu  sync.Mutex
}

type KCPTransport struct {
	mu     sync.RWMutex
	config KCPTransportConfig
	routes map[core.SessionID]*kcpRoute
	closed bool
	stats  kcpCounters
}

type kcpCounters struct {
	datagramsSent     atomic.Uint64
	datagramBytesSent atomic.Uint64
	reliableSent      atomic.Uint64
	reliableBytesSent atomic.Uint64
	datagramsReceived atomic.Uint64
	reliableReceived  atomic.Uint64
	sendErrors        atomic.Uint64
	receiveErrors     atomic.Uint64
}

type KCPTransportStats struct {
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

type KCPDatagramHandler func(core.SessionID, []byte)

func NewKCPTransport(config KCPTransportConfig) (*KCPTransport, error) {
	defaults := DefaultKCPTransportConfig()
	if config == (KCPTransportConfig{}) {
		config = defaults
	}
	if config.MaxDatagramBytes <= 0 {
		config.MaxDatagramBytes = defaults.MaxDatagramBytes
	}
	if config.MaxReliableBytes <= 0 {
		config.MaxReliableBytes = defaults.MaxReliableBytes
	}
	if config.MTU <= 0 {
		config.MTU = defaults.MTU
	}
	if config.SendWindow <= 0 {
		config.SendWindow = defaults.SendWindow
	}
	if config.ReceiveWindow <= 0 {
		config.ReceiveWindow = defaults.ReceiveWindow
	}
	if config.Interval <= 0 {
		config.Interval = defaults.Interval
	}
	if config.NoDelay < 0 || config.NoDelay > 1 || config.DisableCongestion < 0 || config.DisableCongestion > 1 || config.FastResend < 0 {
		return nil, ErrProtocolConfig
	}
	return &KCPTransport{config: config, routes: make(map[core.SessionID]*kcpRoute)}, nil
}

func (transport *KCPTransport) RegisterSession(info core.SessionInfo) error {
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
		route = &kcpRoute{}
		transport.routes[info.ID] = route
	}
	if route.registered {
		return ErrSessionAlreadyExists
	}
	route.registered = true
	return nil
}

// BindSession configures an authenticated KCP session. The KCP listener/dialer
// must enable FEC; kcp-go's OOB channel uses that framing to carry unreliable
// state datagrams while the normal KCP path carries reliable messages.
func (transport *KCPTransport) BindSession(sessionID core.SessionID, session *kcp.UDPSession) error {
	if transport == nil || sessionID == 0 || session == nil {
		return ErrProtocolConfig
	}
	transport.mu.Lock()
	if transport.closed {
		transport.mu.Unlock()
		return ErrTransportClosed
	}
	route := transport.routes[sessionID]
	if route == nil {
		route = &kcpRoute{}
		transport.routes[sessionID] = route
	}
	if route.session != nil || route.binding != nil {
		transport.mu.Unlock()
		if route.session == session {
			return nil
		}
		return ErrSessionAlreadyExists
	}
	for otherSession, otherRoute := range transport.routes {
		if otherSession != sessionID && (otherRoute.session == session || otherRoute.binding == session) {
			transport.mu.Unlock()
			return fmt.Errorf("%w: KCP connection is already bound to session %d", ErrSessionAlreadyExists, otherSession)
		}
	}
	route.binding = session
	transport.mu.Unlock()

	if !session.SetMtu(transport.config.MTU) {
		transport.abortBind(sessionID, route)
		return fmt.Errorf("%w: KCP MTU %d rejected", ErrProtocolConfig, transport.config.MTU)
	}
	// kcp-go's UDPSession.Write splits large buffers into MSS-sized KCP
	// messages. Stream mode plus our own length prefix preserves one logical
	// reliable message across those writes.
	session.SetStreamMode(true)
	session.SetWindowSize(transport.config.SendWindow, transport.config.ReceiveWindow)
	session.SetNoDelay(transport.config.NoDelay, transport.config.Interval, transport.config.FastResend, transport.config.DisableCongestion)
	session.SetACKNoDelay(transport.config.ACKNoDelay)
	session.SetWriteDelay(transport.config.WriteDelay)
	if transport.config.RateLimitBytes > 0 {
		session.SetRateLimit(transport.config.RateLimitBytes)
	}
	if transport.config.DSCP > 0 {
		if err := session.SetDSCP(transport.config.DSCP); err != nil {
			transport.abortBind(sessionID, route)
			return err
		}
	}
	if session.GetOOBMaxSize() < transport.config.MaxDatagramBytes {
		transport.abortBind(sessionID, route)
		return fmt.Errorf("%w: KCP OOB max %d < configured datagram %d (FEC must be enabled)", ErrProtocolConfig, session.GetOOBMaxSize(), transport.config.MaxDatagramBytes)
	}
	transport.mu.Lock()
	if transport.closed {
		transport.mu.Unlock()
		return ErrTransportClosed
	}
	route = transport.routes[sessionID]
	if route == nil || route.session != nil || route.binding != session {
		transport.mu.Unlock()
		return ErrSessionAlreadyExists
	}
	route.session = session
	route.binding = nil
	transport.mu.Unlock()
	return nil
}

func (transport *KCPTransport) abortBind(sessionID core.SessionID, expected *kcpRoute) {
	transport.mu.Lock()
	if route := transport.routes[sessionID]; route == expected && route.session == nil {
		route.binding = nil
		if !route.registered {
			delete(transport.routes, sessionID)
		}
	}
	transport.mu.Unlock()
}

func (transport *KCPTransport) BindDatagramHandler(sessionID core.SessionID, handler KCPDatagramHandler) error {
	if handler == nil {
		return ErrProtocolConfig
	}
	route, err := transport.route(sessionID)
	if err != nil {
		return err
	}
	return route.session.SetOOBHandler(func(payload []byte) {
		copyOfPayload := append([]byte(nil), payload...)
		transport.stats.datagramsReceived.Add(1)
		func() {
			defer func() { _ = recover() }()
			handler(sessionID, copyOfPayload)
		}()
	})
}

func (transport *KCPTransport) RemoveSession(sessionID core.SessionID) bool {
	if transport == nil || sessionID == 0 {
		return false
	}
	transport.mu.Lock()
	route, exists := transport.routes[sessionID]
	delete(transport.routes, sessionID)
	transport.mu.Unlock()
	if exists && route.session != nil && !transport.config.PreserveSessions {
		_ = route.session.Close()
	}
	return exists
}

func (transport *KCPTransport) SendDatagram(ctx context.Context, sessionID core.SessionID, payload []byte) error {
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
	route, err := transport.route(sessionID)
	if err != nil {
		return err
	}
	if err := route.session.SendOOB(payload); err != nil {
		transport.stats.sendErrors.Add(1)
		return err
	}
	transport.stats.datagramsSent.Add(1)
	transport.stats.datagramBytesSent.Add(uint64(len(payload)))
	return nil
}

func (transport *KCPTransport) SendDatagramBatch(ctx context.Context, sessionID core.SessionID, packets [][]byte) error {
	for _, packet := range packets {
		if err := transport.SendDatagram(ctx, sessionID, packet); err != nil {
			return err
		}
	}
	return nil
}

func (transport *KCPTransport) SendReliable(ctx context.Context, sessionID core.SessionID, payload []byte) error {
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
	route, err := transport.route(sessionID)
	if err != nil {
		return err
	}
	route.sendMu.Lock()
	defer route.sendMu.Unlock()
	if deadline, ok := ctx.Deadline(); ok {
		if err := route.session.SetWriteDeadline(deadline); err != nil {
			return err
		}
		defer route.session.SetWriteDeadline(time.Time{})
	}
	defer interruptWriteOnCancel(ctx, route.session)()
	header := make([]byte, 4)
	binary.BigEndian.PutUint32(header, uint32(len(payload)))
	if err := writeAll(route.session, header); err != nil {
		transport.stats.sendErrors.Add(1)
		transport.failRoute(sessionID, route)
		return err
	}
	if err := writeAll(route.session, payload); err != nil {
		transport.stats.sendErrors.Add(1)
		transport.failRoute(sessionID, route)
		return err
	}
	transport.stats.reliableSent.Add(1)
	transport.stats.reliableBytesSent.Add(uint64(len(payload)))
	return nil
}

func (transport *KCPTransport) ReceiveReliable(ctx context.Context, sessionID core.SessionID) ([]byte, error) {
	if transport == nil {
		return nil, ErrTransportClosed
	}
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}
	route, err := transport.route(sessionID)
	if err != nil {
		return nil, err
	}
	route.receiveMu.Lock()
	defer route.receiveMu.Unlock()
	if deadline, ok := ctx.Deadline(); ok {
		if err := route.session.SetReadDeadline(deadline); err != nil {
			return nil, err
		}
		defer route.session.SetReadDeadline(time.Time{})
	}
	defer interruptReadOnCancel(ctx, route.session)()
	header := make([]byte, 4)
	headerBytes, err := io.ReadFull(route.session, header)
	if err != nil {
		transport.stats.receiveErrors.Add(1)
		if headerBytes > 0 {
			transport.failRoute(sessionID, route)
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, err
	}
	length := binary.BigEndian.Uint32(header)
	if length == 0 || uint64(length) > uint64(transport.config.MaxReliableBytes) {
		transport.stats.receiveErrors.Add(1)
		transport.failRoute(sessionID, route)
		return nil, ErrReliableMessageTooBig
	}
	buffer := make([]byte, int(length))
	if _, err := io.ReadFull(route.session, buffer); err != nil {
		transport.stats.receiveErrors.Add(1)
		transport.failRoute(sessionID, route)
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, err
	}
	transport.stats.reliableReceived.Add(1)
	return buffer, nil
}

func (transport *KCPTransport) failRoute(sessionID core.SessionID, expected *kcpRoute) {
	transport.mu.Lock()
	if transport.routes[sessionID] == expected {
		delete(transport.routes, sessionID)
	}
	transport.mu.Unlock()
	if expected.session != nil {
		_ = expected.session.Close()
	}
}

func (transport *KCPTransport) Close() error {
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
	transport.routes = make(map[core.SessionID]*kcpRoute)
	transport.mu.Unlock()
	if !transport.config.PreserveSessions {
		for _, route := range routes {
			if route.session != nil {
				_ = route.session.Close()
			}
		}
	}
	return nil
}

func (transport *KCPTransport) Stats() KCPTransportStats {
	if transport == nil {
		return KCPTransportStats{}
	}
	transport.mu.RLock()
	active := 0
	for _, route := range transport.routes {
		if route.registered && route.session != nil {
			active++
		}
	}
	transport.mu.RUnlock()
	return KCPTransportStats{
		ActiveRoutes: active, DatagramsSent: transport.stats.datagramsSent.Load(), DatagramBytesSent: transport.stats.datagramBytesSent.Load(),
		ReliableMessagesSent: transport.stats.reliableSent.Load(), ReliableBytesSent: transport.stats.reliableBytesSent.Load(),
		DatagramsReceived: transport.stats.datagramsReceived.Load(), ReliableMessagesReceived: transport.stats.reliableReceived.Load(),
		SendErrors: transport.stats.sendErrors.Load(), ReceiveErrors: transport.stats.receiveErrors.Load(),
	}
}

func (transport *KCPTransport) route(sessionID core.SessionID) (*kcpRoute, error) {
	if transport == nil {
		return nil, ErrTransportClosed
	}
	transport.mu.RLock()
	defer transport.mu.RUnlock()
	if transport.closed {
		return nil, ErrTransportClosed
	}
	route := transport.routes[sessionID]
	if route == nil || !route.registered || route.session == nil {
		return nil, ErrRouteNotBound
	}
	return route, nil
}

func NewKCPAESGCM(key []byte) (kcp.BlockCrypt, error) {
	return kcp.NewAESGCMCrypt(key)
}

func ListenKCP(address string, block kcp.BlockCrypt, dataShards, parityShards int) (*kcp.Listener, error) {
	if address == "" || isNilInterface(block) || dataShards <= 0 || parityShards <= 0 {
		return nil, fmt.Errorf("%w: KCP requires encryption and FEC for the unreliable OOB lane", ErrProtocolConfig)
	}
	return kcp.ListenWithOptions(address, block, dataShards, parityShards)
}

func DialKCP(address string, block kcp.BlockCrypt, dataShards, parityShards int) (*kcp.UDPSession, error) {
	if address == "" || isNilInterface(block) || dataShards <= 0 || parityShards <= 0 {
		return nil, fmt.Errorf("%w: KCP requires encryption and FEC for the unreliable OOB lane", ErrProtocolConfig)
	}
	return kcp.DialWithOptions(address, block, dataShards, parityShards)
}

func KCPRemoteAddress(session *kcp.UDPSession) net.Addr {
	if session == nil {
		return nil
	}
	return session.RemoteAddr()
}

var _ core.Transport = (*KCPTransport)(nil)
var _ core.DatagramBatchTransport = (*KCPTransport)(nil)
var _ core.SessionTransport = (*KCPTransport)(nil)
