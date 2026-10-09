package gateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tjbdwanghaibo/roost-core/infra/network/bus"
	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
	"github.com/tjbdwanghaibo/roost-core/internal/operation"
)

type GateDependencies struct {
	Client       fnats.RawClient
	QueueFactory ReliableQueueFactory
	Ready        func() bool
	// ResolveGame 仅返回固定 SID 的候选；Matches 核对持锁代次，发现本身不授予权限。
	ResolveGame func(context.Context, int32) (ProcessIdentity, error)
	Matches     func(context.Context, string, ProcessIdentity) (bool, error)
}

type gateConnection struct {
	binding    Binding
	session    ConnectionSession
	receiverID uint64
	active     bool
	lease      time.Time
	renewing   bool
	lastOut    uint64
	changed    chan struct{}
}

type gateControl struct {
	binding   Binding
	operation ControlOperation
}

// Gate 只拥有连接、身份和有界网络等待，不执行业务 Nest 或访问 Entity。
// TCPServer 注入 Forward 与握手钩子；Game 的统一出站在此按完整代次和连续序号准入。
type Gate struct {
	gameRequests     map[ProcessIdentity]int
	broadcasts       map[broadcastKey]*broadcastRecord
	broadcastSlots   chan struct{}
	broadcastWait    drainGroup
	config           Config
	identity         ProcessIdentity
	deps             GateDependencies
	codec            bus.MessagePackCodec
	queue            ReliableQueue
	mu               sync.Mutex
	started, closing bool
	healthy          atomic.Bool
	connections      map[string]*gateConnection
	receivers        map[uint64]*gateConnection
	nextReceiver     uint64
	subscriptions    []fnats.DrainSubscription
	forwardSlots     chan struct{}
	gameSlots        map[ProcessIdentity]chan struct{}
	residentBytes    int64
	residentRequests int
	forwards         drainGroup
	controls         chan gateControl
	controlWait      drainGroup
	ctx              context.Context
	cancel           context.CancelFunc
	sweepDone        chan struct{}
	stopSerial       operation.Serial
	controlsClosed   bool
}

func NewGate(config Config, identity ProcessIdentity, deps GateDependencies) (*Gate, error) {
	if err := errors.Join(config.Validate(), identity.Validate()); err != nil {
		return nil, err
	}
	if deps.Client == nil || deps.QueueFactory == nil || deps.Ready == nil || deps.ResolveGame == nil || deps.Matches == nil {
		return nil, errors.New("gateway: gate dependencies are required")
	}
	if err := checkChannelSize(config, deps.Client); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	gate := &Gate{config: config, identity: identity, deps: deps, ctx: ctx, cancel: cancel, connections: make(map[string]*gateConnection), receivers: make(map[uint64]*gateConnection), gameSlots: make(map[ProcessIdentity]chan struct{}), forwardSlots: make(chan struct{}, config.ForwardWorkers), controls: make(chan gateControl, config.MaxBindings), sweepDone: make(chan struct{})}
	queue, err := deps.QueueFactory(config.Outbound, gate.writeOutbound, gate.outboundFailed)
	if err != nil {
		cancel()
		return nil, err
	}
	gate.gameRequests = make(map[ProcessIdentity]int)
	gate.queue = queue
	gate.broadcasts = make(map[broadcastKey]*broadcastRecord)
	gate.broadcastSlots = make(chan struct{}, config.BroadcastWorkers)
	return gate, nil
}

// InboxPrefix 必须用于两端专用 RawClient 的 NATS 连接；ACL 按角色与 incarnation 隔离。
func InboxPrefix(namespace, role string, identity ProcessIdentity) (string, error) {
	if err := errors.Join(ValidateNamespace(namespace), identity.Validate()); err != nil {
		return "", err
	}
	if role != "gate" && role != "game" && role != "system" {
		return "", ErrInvalidRequest
	}
	return namespace + ".reply." + role + "." + strconv.Itoa(int(identity.ServerID)) + "." + identity.Incarnation, nil
}

func (gate *Gate) Start() error {
	gate.mu.Lock()
	defer gate.mu.Unlock()
	if gate.started || gate.closing {
		return ErrDraining
	}
	// 订阅错误可以在启动返回前到达，之后不能重新置 healthy 覆盖故障。
	gate.healthy.Store(true)
	for _, channel := range []struct {
		name    string
		handler fnats.MsgHandler
	}{{"outbound", gate.receiveOutbound}, {"close", gate.receiveClose}} {
		sub, err := gate.deps.Client.SubscribeBounded(channelFilter(gate.config, "gate", gate.identity, "game", channel.name), fnats.PendingLimits{Messages: gate.config.SubscriptionMessages, Bytes: gate.config.SubscriptionBytes, OnError: func(err error) {
			if gate.healthy.Swap(false) {
				slog.Warn("gate fenced after NATS subscription failure", "channel", channel.name, "err", err)
			}
		}}, channel.handler)
		if err != nil {
			gate.healthy.Store(false)
			gate.closing = true
			for _, prior := range gate.subscriptions {
				_ = prior.Unsubscribe()
			}
			return err
		}
		gate.subscriptions = append(gate.subscriptions, sub)
	}
	for _, role := range []string{"game", "system"} {
		role := role
		sub, err := gate.deps.Client.SubscribeBounded(channelFilter(gate.config, "gate", gate.identity, role, "broadcast"), fnats.PendingLimits{Messages: gate.config.SubscriptionMessages, Bytes: gate.config.SubscriptionBytes, OnError: func(err error) {
			if gate.healthy.Swap(false) {
				slog.Warn("gate fenced after broadcast subscription failure", "role", role, "err", err)
			}
		}}, func(msg *fnats.Msg) { gate.receiveBroadcast(role, msg) })
		if err != nil {
			gate.healthy.Store(false)
			gate.closing = true
			for _, prior := range gate.subscriptions {
				_ = prior.Unsubscribe()
			}
			return err
		}
		gate.subscriptions = append(gate.subscriptions, sub)
	}
	gate.started = true
	for range gate.config.ControlWorkers {
		gate.controlWait.Go(gate.controlWorker)
	}
	go gate.sweep()
	return nil
}
func (gate *Gate) ready() (ready bool) {
	defer func() {
		if recover() != nil {
			ready = false
		}
	}()
	return gate.healthy.Load() && gate.deps.Ready()
}

func (gate *Gate) BeforeACK(ctx context.Context, session ConnectionSession, ticket string) error {
	principal := session.Principal()
	if !principal.Authenticated() || principal.ServerID <= 0 || !gate.ready() {
		return ErrUnauthenticated
	}
	if own, err := gate.deps.Matches(ctx, "gate", gate.identity); err != nil || !own {
		return ErrBindingStale
	}
	game, err := gate.deps.ResolveGame(ctx, principal.ServerID)
	if err != nil {
		return err
	}
	if game.ServerID != principal.ServerID || game.Validate() != nil {
		return ErrInvalidRequest
	}
	if match, err := gate.deps.Matches(ctx, "game", game); err != nil || !match {
		return ErrBindingStale
	}
	binding := Binding{PlayerID: principal.PlayerID, SessionID: principal.SessionID, Gate: gate.identity, Game: game, ConnectionNonce: session.ConnectionID()}
	gate.mu.Lock()
	if gate.closing || len(gate.connections) >= gate.config.MaxBindings || gate.nextReceiver == ^uint64(0) {
		gate.mu.Unlock()
		return ErrAdmissionFull
	}
	gate.nextReceiver++
	connection := &gateConnection{binding: binding, session: session, receiverID: gate.nextReceiver, changed: make(chan struct{})}
	gate.forwards.Add(1)
	gate.connections[session.ConnectionID()] = connection
	gate.receivers[connection.receiverID] = connection
	gate.mu.Unlock()
	defer gate.forwards.Done()
	// 先预留本地发送关系，再进行可能已生效但回执丢失的 Bind。失败由 nonce/租期收口。
	if err := gate.queue.Register(connection.receiverID, principal.PlayerID); err != nil {
		gate.closeConnection(connection, err)
		return err
	}
	result, err := gate.control(ctx, binding, Bind, ticket)
	if err != nil {
		gate.closeConnection(connection, err)
		return err
	}
	if result.Binding.validateConnection() != nil || result.Binding.BindID == "" {
		gate.closeConnection(connection, ErrBindingStale)
		return ErrBindingStale
	}
	expected := binding
	expected.BindID = result.Binding.BindID
	if result.Binding != expected {
		gate.closeConnection(connection, ErrBindingStale)
		return ErrBindingStale
	}
	gate.mu.Lock()
	if gate.connections[session.ConnectionID()] != connection || gate.closing {
		gate.mu.Unlock()
		gate.closeConnection(connection, ErrDraining)
		return ErrDraining
	}
	connection.binding = result.Binding
	connection.lease = time.Unix(0, result.LeaseUntilUnixNano)
	gate.mu.Unlock()
	return nil
}
func (gate *Gate) AfterACK(ctx context.Context, session ConnectionSession) error {
	gate.mu.Lock()
	connection := gate.connections[session.ConnectionID()]
	if connection == nil || gate.closing {
		gate.mu.Unlock()
		return ErrBindingStale
	}
	// AuthACK 已物理写出；先准备接收出站，随后 Activate 才允许 Game 主动发送。
	connection.active = true
	binding := connection.binding
	gate.forwards.Add(1)
	gate.mu.Unlock()
	defer gate.forwards.Done()
	result, err := gate.control(ctx, binding, Activate, "")
	if err != nil {
		gate.closeConnection(connection, err)
		return err
	}
	gate.mu.Lock()
	if gate.connections[session.ConnectionID()] != connection {
		gate.mu.Unlock()
		return ErrBindingStale
	}
	connection.lease = time.Unix(0, result.LeaseUntilUnixNano)
	gate.mu.Unlock()
	return nil
}
func (gate *Gate) Closed(session ConnectionSession) {
	gate.mu.Lock()
	connection := gate.connections[session.ConnectionID()]
	gate.mu.Unlock()
	if connection != nil {
		gate.closeConnection(connection, ErrSessionClosed)
	}
}

func (gate *Gate) control(ctx context.Context, binding Binding, operation ControlOperation, ticket string) (ControlResponse, error) {
	budget, err := BudgetFromContext(ctx)
	if err != nil {
		return ControlResponse{}, err
	}
	request := ControlRequest{Version: InternalVersion, Binding: binding, Operation: operation, Ticket: ticket, Budget: budget}
	data, err := gate.codec.Marshal(request)
	if err != nil {
		return ControlResponse{}, err
	}
	subject, err := ChannelSubject(gate.config.Namespace, "game", binding.Game, "gate", gate.identity, "control")
	if err != nil {
		return ControlResponse{}, err
	}
	data, err = gate.deps.Client.RequestContext(ctx, subject, data)
	if err != nil {
		return ControlResponse{}, err
	}
	var result ControlResponse
	if err = gate.codec.Unmarshal(data, &result); err != nil {
		return result, err
	}
	if result.Version != InternalVersion || result.Outcome != Completed {
		return result, fmt.Errorf("%w: control outcome %d: %s", ErrBindingStale, result.Outcome, result.Reason)
	}
	if operation != Bind && result.Binding != binding {
		return result, ErrBindingStale
	}
	if operation != Unbind && time.Until(time.Unix(0, result.LeaseUntilUnixNano)) <= gate.config.ClockSkew {
		return result, ErrBindingStale
	}
	return result, nil
}

func (gate *Gate) closeConnection(connection *gateConnection, reason error) {
	gate.mu.Lock()
	if gate.receivers[connection.receiverID] != connection {
		gate.mu.Unlock()
		return
	}
	delete(gate.receivers, connection.receiverID)
	delete(gate.connections, connection.binding.ConnectionNonce)
	close(connection.changed)
	// Closed 钩子快速返回，控制消息只入有界网络队列。迟到 Unbind 匹配完整旧身份。
	if !gate.controlsClosed && connection.binding.BindID != "" {
		select {
		case gate.controls <- gateControl{connection.binding, Unbind}:
		default:
		}
	}
	if gate.gameRequests[connection.binding.Game] == 0 && !gate.hasGameLocked(connection.binding.Game) {
		delete(gate.gameSlots, connection.binding.Game)
	}
	gate.mu.Unlock()
	if !errors.Is(reason, ErrSessionClosed) && !errors.Is(reason, ErrDraining) {
		slog.Warn("gate closed binding after channel failure", "receiver", connection.receiverID, "game_sid", connection.binding.Game.ServerID, "err", reason)
	}
	gate.queue.Remove(connection.receiverID)
	_ = connection.session.Close(reason)
}
func (gate *Gate) hasGameLocked(game ProcessIdentity) bool {
	for _, connection := range gate.connections {
		if connection.binding.Game == game {
			return true
		}
	}
	return false
}
func (gate *Gate) controlWorker() {
	for job := range gate.controls {
		ctx, cancel := context.WithTimeout(gate.ctx, gate.config.RequestTimeout)
		var result ControlResponse
		var err error
		if job.operation == Renew {
			if match, matchErr := gate.deps.Matches(ctx, "game", job.binding.Game); matchErr != nil || !match {
				err = ErrBindingStale
			}
		}
		if err == nil {
			result, err = gate.control(ctx, job.binding, job.operation, "")
		}
		cancel()
		if job.operation == Renew {
			gate.mu.Lock()
			connection := gate.connections[job.binding.ConnectionNonce]
			if connection != nil && connection.binding == job.binding {
				connection.renewing = false
				if err == nil {
					connection.lease = time.Unix(0, result.LeaseUntilUnixNano)
				}
			} else {
				connection = nil
			}
			gate.mu.Unlock()
			if connection != nil && err != nil {
				gate.closeConnection(connection, err)
			}
		}
	}
}
func (gate *Gate) sweep() {
	defer close(gate.sweepDone)
	ticker := time.NewTicker(min(time.Second, gate.config.RenewInterval))
	defer ticker.Stop()
	for {
		select {
		case <-gate.ctx.Done():
			return
		case <-ticker.C:
			gate.mu.Lock()
			var closeList []*gateConnection
			for _, connection := range gate.connections {
				if !gate.ready() || (!connection.lease.IsZero() && time.Until(connection.lease) <= gate.config.ClockSkew) {
					closeList = append(closeList, connection)
					continue
				}
				if !gate.closing && connection.active && !connection.renewing && time.Until(connection.lease) <= gate.config.Lease-gate.config.RenewInterval {
					select {
					case gate.controls <- gateControl{connection.binding, Renew}:
						connection.renewing = true
					default:
						closeList = append(closeList, connection)
					}
				}
			}
			gate.mu.Unlock()
			for _, connection := range closeList {
				gate.closeConnection(connection, ErrBindingStale)
			}
		}
	}
}

var _ TCPHandshake = (*Gate)(nil)
