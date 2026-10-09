package gateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tjbdwanghaibo/roost-core/client/wire"
	"github.com/tjbdwanghaibo/roost-core/infra/network/bus"
	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
	"github.com/tjbdwanghaibo/roost-core/internal/operation"
)

type GameDependencies struct {
	Client        fnats.RawClient
	Authenticator Authenticator
	Dispatch      TCPDispatch
	Encoder       TCPEncoder
	QueueFactory  ReliableQueueFactory
	Ready         func() bool
	Matches       func(context.Context, string, ProcessIdentity) (bool, error)
	// 生命周期接线必须快速返回，通过正式 Sender/Sync lifecycle 处理后续工作；
	// 不在控制工作协程中直接取得 Entity local 锁。
	OnActive func(Binding, uint64) error
	OnClosed func(Binding, uint64)
}

// GameIngress 只拥有接入身份、准入及发送关系。业务执行仍由注入的正式 dispatcher 完成。
type controlJob struct {
	request ControlRequest
	reply   string
	ctx     context.Context
	cancel  context.CancelFunc
	bytes   int64
}

type GameIngress struct {
	lifecycle           *TCPRuntime
	config              Config
	identity            ProcessIdentity
	deps                GameDependencies
	codec               bus.MessagePackCodec
	bindings            *bindingTable
	queue               ReliableQueue
	ctx                 context.Context
	cancel              context.CancelFunc
	mu                  sync.Mutex
	started             bool
	closing             bool
	healthy             atomic.Bool
	subscriptions       []fnats.DrainSubscription
	controls            chan controlJob
	controlWorkers      drainGroup
	controlRequests     int
	controlBytes        int64
	controlsClosed      bool
	controlWait         drainGroup
	sweepDone           chan struct{}
	resources           map[uint64]*ingressBinding
	work                chan *ingressBinding
	workerWait          drainGroup
	residentRequests    int
	residentBytes       int64
	notifications       chan CloseRequest
	notificationDone    chan struct{}
	requestWait         drainGroup
	retireWait          drainGroup
	stopSerial          operation.Serial
	workClosed          bool
	notificationsClosed bool
}

func NewGameIngress(config Config, identity ProcessIdentity, deps GameDependencies) (*GameIngress, error) {
	if err := errors.Join(config.Validate(), identity.Validate()); err != nil {
		return nil, err
	}
	if deps.Client == nil || deps.Authenticator == nil || deps.Dispatch == nil || deps.QueueFactory == nil || deps.Ready == nil || deps.Matches == nil {
		return nil, errors.New("gateway: game ingress dependencies are required")
	}
	if err := checkChannelSize(config, deps.Client); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	game := &GameIngress{config: config, identity: identity, deps: deps, bindings: newBindingTable(config, identity), ctx: ctx, cancel: cancel, controls: make(chan controlJob, config.MaxControlRequests), sweepDone: make(chan struct{}), resources: make(map[uint64]*ingressBinding), work: make(chan *ingressBinding, config.MaxForwardRequests)}
	game.notifications = make(chan CloseRequest, config.MaxBindings)
	game.notificationDone = make(chan struct{})
	queue, err := deps.QueueFactory(config.Outbound, game.sendOutbound, game.outboundFailed)
	if err != nil {
		cancel()
		return nil, err
	}
	game.queue = queue
	game.lifecycle = NewTCPRuntime(nil, min(defaultLoginTimeout, config.RequestTimeout))
	return game, nil
}

func checkChannelSize(config Config, client fnats.RawClient) error {
	maxPayload, err := client.MaxPayload()
	if err != nil {
		return err
	}
	// 最长合法身份和完整业务包真实编码；不是只比较裸 payload。
	longIdentity := ProcessIdentity{ServerID: 2147483647, Incarnation: strings.Repeat("x", 128)}
	binding := Binding{PlayerID: 9223372036854775807, SessionID: strings.Repeat("s", 128), Gate: longIdentity, Game: longIdentity, ConnectionNonce: strings.Repeat("n", 32), BindID: strings.Repeat("b", 32)}
	packet := OutboundPacket{Version: InternalVersion, Binding: binding, OutSeq: ^uint64(0), Budget: Budget{DeadlineUnixNano: 9223372036854775807, RemainingNanos: int64(time.Minute)}, MessageID: ^uint32(0), Kind: wire.PayloadLockstep, Push: true, Payload: make([]byte, config.MaxPayloadBytes)}
	forward := ForwardRequest{Version: InternalVersion, Binding: binding, RequestID: strings.Repeat("r", 32), Budget: packet.Budget, MessageID: ^uint32(0), Sequence: ^uint32(0), Kind: wire.PayloadLockstep, Payload: packet.Payload}
	control := ControlRequest{Version: InternalVersion, Operation: Bind, Binding: binding, Budget: packet.Budget, Ticket: strings.Repeat("t", 8192)}
	broadcast := BroadcastRequest{Version: InternalVersion, Source: longIdentity, Target: longIdentity, BroadcastID: strings.Repeat("b", 32), Budget: packet.Budget, Audience: Audience{Kind: Players, PlayerIDs: make([]int64, config.MaxBroadcastPlayers)}, MessageID: ^uint32(0), Payload: packet.Payload}
	for index := range broadcast.Audience.PlayerIDs {
		broadcast.Audience.PlayerIDs[index] = 9223372036854775807
	}
	for _, value := range []any{packet, forward, control, broadcast} {
		data, err := (bus.MessagePackCodec{}).Marshal(value)
		if err != nil {
			return err
		}
		if int64(len(data)) > maxPayload || len(data) > config.SubscriptionBytes {
			return fmt.Errorf("gateway: complete %T requires %d bytes; NATS max=%d subscription bytes=%d", value, len(data), maxPayload, config.SubscriptionBytes)
		}
		if _, isBroadcast := value.(BroadcastRequest); !isBroadcast && len(data) > config.Outbound.MaxPacketBytes {
			return fmt.Errorf("gateway: complete %T requires %d bytes; outbound max=%d", value, len(data), config.Outbound.MaxPacketBytes)
		}
	}

	return nil
}

func channelFilter(config Config, role string, identity ProcessIdentity, sourceRole, channel string) string {
	return strings.Join([]string{config.Namespace, role, strconv.Itoa(int(identity.ServerID)), identity.Incarnation, "from", sourceRole, "*", "*", channel}, ".")
}

func replyMatches(namespace, role string, identity ProcessIdentity, reply string) bool {
	prefix := strings.Join([]string{namespace, "reply", role, strconv.Itoa(int(identity.ServerID)), identity.Incarnation}, ".") + "."
	if !strings.HasPrefix(reply, prefix) || len(reply) > 512 {
		return false
	}
	return ValidateNamespace(reply[len(prefix):]) == nil
}

func (game *GameIngress) Start() error {
	game.mu.Lock()
	defer game.mu.Unlock()
	if game.started || game.closing {
		return errors.New("gateway: game ingress cannot restart")
	}
	// 订阅错误可以在启动返回前到达，之后不能重新置 healthy 覆盖故障。
	game.healthy.Store(true)
	for _, channel := range []struct {
		name    string
		handler fnats.MsgHandler
	}{{"control", game.receiveControl}, {"forward", game.receiveForward}} {
		sub, err := game.deps.Client.SubscribeBounded(channelFilter(game.config, "game", game.identity, "gate", channel.name), fnats.PendingLimits{Messages: game.config.SubscriptionMessages, Bytes: game.config.SubscriptionBytes, OnError: func(err error) {
			if game.healthy.Swap(false) {
				slog.Warn("game ingress fenced after NATS subscription failure", "channel", channel.name, "err", err)
			}
		}}, channel.handler)
		if err != nil {
			for _, prior := range game.subscriptions {
				_ = prior.Unsubscribe()
			}
			game.healthy.Store(false)
			game.closing = true
			return err
		}
		game.subscriptions = append(game.subscriptions, sub)
	}
	game.started = true
	for range game.config.ControlWorkers {
		game.controlWorkers.Go(game.controlWorker)
	}
	for range game.config.ForwardWorkers {
		game.workerWait.Go(game.forwardWorker)
	}
	go game.sweep()
	go game.sendNotifications()
	return nil
}

func (game *GameIngress) ready() (ready bool) {
	defer func() {
		if recover() != nil {
			ready = false
		}
	}()
	return game.healthy.Load() && game.deps.Ready()
}

func (game *GameIngress) receiveControl(msg *fnats.Msg) {
	if msg == nil || len(msg.Data) > 16<<10 {
		return
	}
	var request ControlRequest
	if err := game.codec.Unmarshal(msg.Data, &request); err != nil || request.Validate() != nil {
		return
	}
	subject, err := ChannelSubject(game.config.Namespace, "game", game.identity, "gate", request.Binding.Gate, "control")
	if err != nil || subject != msg.Subject || !replyMatches(game.config.Namespace, "gate", request.Binding.Gate, msg.Reply) {
		return
	}
	ctx, cancel, err := request.Budget.Context(game.ctx, game.config.RequestTimeout, game.config.ClockSkew)
	if err != nil {
		game.replyControl(msg.Reply, ControlResponse{ExecutionResult: ExecutionResult{Version: InternalVersion, Outcome: NotAdmitted, Reason: "expired control"}})
		return
	}
	game.mu.Lock()
	if game.closing {
		game.mu.Unlock()
		cancel()
		game.replyControl(msg.Reply, ControlResponse{ExecutionResult: ExecutionResult{Version: InternalVersion, Outcome: NotAdmitted, Reason: "draining"}})
		return
	}
	// 控制请求也有有限等待队列，避免同批续租短暂超过 worker 数就关闭健康绑定。
	// 预算在接收时恢复，排队不会获得新的完整期限；条目和字节均含正在执行的请求。
	if game.controlRequests >= game.config.MaxControlRequests || int64(len(msg.Data)) > game.config.MaxControlBytes-game.controlBytes {
		game.mu.Unlock()
		cancel()
		game.replyControl(msg.Reply, ControlResponse{ExecutionResult: ExecutionResult{Version: InternalVersion, Outcome: NotAdmitted, Reason: "control capacity"}})
		return
	}
	game.controlWait.Add(1)
	game.controlRequests++
	game.controlBytes += int64(len(msg.Data))
	game.controls <- controlJob{request: request, reply: msg.Reply, ctx: ctx, cancel: cancel, bytes: int64(len(msg.Data))}
	game.mu.Unlock()
}

func (game *GameIngress) controlWorker() {
	for job := range game.controls {
		response := game.handleControl(job.ctx, job.request)
		game.replyControl(job.reply, response)
		job.cancel()
		game.mu.Lock()
		game.controlRequests--
		game.controlBytes -= job.bytes
		game.mu.Unlock()
		game.controlWait.Done()
	}
}

func (game *GameIngress) handleControl(ctx context.Context, request ControlRequest) (response ControlResponse) {
	response.ExecutionResult = ExecutionResult{Version: InternalVersion, Outcome: NotAdmitted, Reason: "control refused"}
	defer func() {
		if value := recover(); value != nil {
			response = ControlResponse{ExecutionResult: ExecutionResult{Version: InternalVersion, Outcome: Unknown, Reason: "control failed"}}
		}
	}()
	if !game.ready() || ctx.Err() != nil {
		return response
	}
	own, err := game.deps.Matches(ctx, "game", game.identity)
	if err != nil || !own {
		return response
	}
	match, err := game.deps.Matches(ctx, "gate", request.Binding.Gate)
	if err != nil || !match {
		return response
	}
	if request.Operation != Bind {
		record, result, err := game.bindings.control(request)
		if err != nil {
			if record != nil {
				game.retire(record)
			}
			return response
		}
		if request.Operation == Unbind {
			game.retire(record)
		}
		if request.Operation == Activate {
			// OnActive 幂等由资源记录守护，不重复建立 Sync/Room 生命周期。
			if err := game.activate(ctx, record); err != nil {
				response.Outcome = Unknown
				return response
			}
		}
		return result
	}
	record, owner, err := game.bindings.reserve(request)
	if err != nil {
		return response
	}
	if !owner {
		result, err := game.bindings.await(ctx, record)
		if err == nil {
			return result
		}
		response.Outcome = Unknown
		return response
	}
	finished := false
	defer func() {
		if !finished {
			game.bindings.finish(record, false)
			game.retire(record)
		}
	}()
	principal, err := game.deps.Authenticator.Authenticate(ctx, request.Ticket, nil)
	if err != nil || principal.PlayerID != request.Binding.PlayerID || principal.ServerID != game.identity.ServerID || !principal.Authenticated() || ctx.Err() != nil || !game.ready() {
		return response
	}
	principal.SessionID = request.Binding.SessionID
	if err := game.register(record, principal); err != nil {
		return response
	}
	previous, result := game.bindings.finish(record, true)
	finished = true
	if previous != nil {
		game.retire(previous)
	}
	if result.Outcome != Completed {
		game.retire(record)
	}
	return result
}

func (game *GameIngress) replyControl(reply string, response ControlResponse) {
	observeResult("game", "control", response.Outcome)
	data, err := game.codec.Marshal(response)
	if err == nil {
		if game.deps.Client.PublishOnce(reply, data) != nil {
			observeFailure("game", "reply")
		}
	}
}

func (game *GameIngress) sweep() {
	defer close(game.sweepDone)
	ticker := time.NewTicker(min(time.Second, game.config.RenewInterval))
	defer ticker.Stop()
	for {
		select {
		case <-game.ctx.Done():
			return
		case <-ticker.C:
			for _, record := range game.bindings.expire(!game.ready()) {
				game.retire(record)
			}
		}
	}
}
