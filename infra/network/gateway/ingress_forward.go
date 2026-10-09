package gateway

import (
	"context"
	"sync"

	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
)

type ingressBinding struct {
	requests       int
	record         *bindingRecord
	principal      Principal
	ctx            context.Context
	cancel         context.CancelFunc
	pending        []forwardJob
	scheduled      bool
	closed         bool
	activated      bool
	activationDone chan struct{}
	activationErr  error
	lastSequence   uint32
	outMu          sync.Mutex
	outSeq         uint64
}

type forwardJob struct {
	request ForwardRequest
	reply   string
	ctx     context.Context
	cancel  context.CancelFunc
	bytes   int64
}

func (game *GameIngress) register(record *bindingRecord, principal Principal) error {
	if err := game.queue.Register(record.receiverID, principal.PlayerID); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(game.ctx)
	resource := &ingressBinding{record: record, principal: clonePrincipal(principal), ctx: ctx, cancel: cancel}
	game.mu.Lock()
	defer game.mu.Unlock()
	if game.closing {
		cancel()
		game.queue.Remove(record.receiverID)
		return ErrDraining
	}
	game.resources[record.receiverID] = resource
	return nil
}

func (game *GameIngress) activate(ctx context.Context, record *bindingRecord) error {
	game.mu.Lock()
	resource := game.resources[record.receiverID]
	if resource == nil || resource.closed {
		game.mu.Unlock()
		return ErrBindingStale
	}
	if resource.activated {
		game.mu.Unlock()
		return nil
	}
	if resource.activationDone != nil {
		done := resource.activationDone
		game.mu.Unlock()
		select {
		case <-done:
			game.mu.Lock()
			err := resource.activationErr
			game.mu.Unlock()
			return err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	resource.activationDone = make(chan struct{})
	game.mu.Unlock()
	var err error
	func() {
		defer func() {
			if recover() != nil {
				err = ErrEndpointPanic
			}
		}()
		if game.deps.OnActive != nil {
			err = game.deps.OnActive(record.identity, record.receiverID)
		}
	}()
	game.mu.Lock()
	if err == nil && resource.closed {
		err = ErrBindingStale
	}
	resource.activationErr = err
	resource.activated = err == nil && !resource.closed
	close(resource.activationDone)
	game.mu.Unlock()
	if err != nil {
		game.bindings.close(record.identity)
		game.retire(record)
		return err
	}
	return nil
}

func (game *GameIngress) retire(record *bindingRecord) {
	if record == nil {
		return
	}
	game.mu.Lock()
	resource := game.resources[record.receiverID]
	if resource == nil || resource.closed {
		game.mu.Unlock()
		return
	}
	resource.closed = true
	resource.cancel()
	delete(game.resources, record.receiverID)
	// 通知只是加速关闭，满队列不阻塞业务/Guard；租期依旧负责最终收口。
	select {
	case game.notifications <- CloseRequest{Version: InternalVersion, Binding: record.identity, Reason: "binding closed"}:
	default:
	}
	done := resource.activationDone
	game.retireWait.Add(1)
	game.mu.Unlock()
	game.queue.Remove(record.receiverID)
	finish := func() {
		defer game.retireWait.Done()
		defer func() {
			if recover() != nil {
				game.healthy.Store(false)
			}
		}()
		// 激活回调可能正在建 Sync/Room lifetime；先等其真实返回，再执行唯一旧代退出。
		if done != nil {
			<-done
		}
		game.lifecycle.publishClosed(TCPSessionClosed{PlayerID: record.identity.PlayerID, SessionID: record.identity.SessionID, Binding: record.identity, ReceiverID: record.receiverID})
		if game.deps.OnClosed != nil {
			game.deps.OnClosed(record.identity, record.receiverID)
		}
	}
	if done != nil {
		go finish()
	} else {
		finish()
	}

}

func (game *GameIngress) receiveForward(msg *fnats.Msg) {
	if msg == nil || len(msg.Data) > game.config.Outbound.MaxPacketBytes {
		return
	}
	var request ForwardRequest
	if err := game.codec.Unmarshal(msg.Data, &request); err != nil || request.Validate(game.config.MaxPayloadBytes) != nil {
		return
	}
	subject, err := ChannelSubject(game.config.Namespace, "game", game.identity, "gate", request.Binding.Gate, "forward")
	if err != nil || subject != msg.Subject || request.Binding.Game != game.identity || !replyMatches(game.config.Namespace, "gate", request.Binding.Gate, msg.Reply) {
		return
	}
	ctx, cancel, err := request.Budget.Context(game.ctx, game.config.RequestTimeout, game.config.ClockSkew)
	if err != nil {
		game.replyExecution(msg.Reply, NotAdmitted, "expired request", 0)
		return
	}
	game.mu.Lock()
	record, lookupErr := game.bindings.lookup(request.Binding, true)
	var resource *ingressBinding
	if record != nil {
		resource = game.resources[record.receiverID]
	}
	if lookupErr != nil || resource == nil || resource.closed || !resource.activated || game.closing || !game.ready() {
		game.mu.Unlock()
		cancel()
		game.replyExecution(msg.Reply, NotAdmitted, "inactive binding", 0)
		return
	}
	if resource.lastSequence != 0 && int32(request.Sequence-resource.lastSequence) <= 0 {
		game.mu.Unlock()
		cancel()
		game.replyExecution(msg.Reply, Unknown, "sequence already admitted", 0)
		return
	}
	bytes := int64(len(msg.Data))
	if resource.requests >= game.config.PerBindingRequests || game.residentRequests >= game.config.MaxForwardRequests || bytes > game.config.MaxForwardBytes-game.residentBytes {
		game.mu.Unlock()
		cancel()
		game.replyExecution(msg.Reply, NotAdmitted, "forward capacity", 0)
		return
	}
	resource.lastSequence = request.Sequence
	resource.pending = append(resource.pending, forwardJob{request: request, reply: msg.Reply, ctx: ctx, cancel: cancel, bytes: bytes})
	resource.requests++
	game.residentRequests++
	game.residentBytes += bytes
	game.requestWait.Add(1)
	if !resource.scheduled {
		resource.scheduled = true
		game.work <- resource
	}
	game.mu.Unlock()
}

// forwardWorker 每次只取一个绑定的一项，然后重新排到就绪队列尾部；
// 同一绑定只有一个 worker 在执行，其他绑定可以并行进入正式 Sender。
func (game *GameIngress) forwardWorker() {
	for resource := range game.work {
		game.mu.Lock()
		if len(resource.pending) == 0 {
			resource.scheduled = false
			game.mu.Unlock()
			continue
		}
		job := resource.pending[0]
		resource.pending[0] = forwardJob{}
		resource.pending = resource.pending[1:]
		closed := resource.closed
		game.mu.Unlock()
		if closed || job.ctx.Err() != nil {
			game.replyExecution(job.reply, NotAdmitted, "expired before dispatch", 0)
		} else {
			game.dispatch(resource, job)
		}
		job.cancel()
		game.mu.Lock()
		resource.requests--
		game.residentRequests--
		game.residentBytes -= job.bytes
		if len(resource.pending) > 0 {
			game.work <- resource
		} else {
			resource.scheduled = false
		}
		game.mu.Unlock()
		game.requestWait.Done()
	}
}

func (game *GameIngress) dispatch(resource *ingressBinding, job forwardJob) {
	// 一旦交给 dispatcher，错误/超时不能冒充“没有进入 Nest”。
	outcome, reason := Unknown, "dispatch failed"
	var sequence uint64
	defer func() {
		if value := recover(); value != nil {
			outcome, reason = Unknown, "dispatch failed"
		}
		game.replyExecution(job.reply, outcome, reason, sequence)
	}()
	session := gameSession{game: game, resource: resource}
	ctx, cancel := context.WithCancel(job.ctx)
	defer cancel()
	stop := context.AfterFunc(resource.ctx, cancel)
	defer stop()
	if resource.ctx.Err() != nil || ctx.Err() != nil {
		return
	}
	response, err := game.deps.Dispatch(ctx, session, job.request.MessageID, job.request.Sequence, job.request.Kind, job.request.Payload)
	if err != nil || ctx.Err() != nil {
		return
	}
	if response != nil {
		if err := session.Reply(ctx, response); err != nil {
			return
		}
	}
	resource.outMu.Lock()
	sequence = resource.outSeq
	resource.outMu.Unlock()
	outcome, reason = Completed, ""
}

func (game *GameIngress) replyExecution(reply string, outcome Outcome, reason string, sequence uint64) {
	observeResult("game", "forward", outcome)
	data, err := game.codec.Marshal(ExecutionResult{Version: InternalVersion, Outcome: outcome, Reason: reason, OutSeq: sequence})
	if err == nil {
		if game.deps.Client.PublishOnce(reply, data) != nil {
			observeFailure("game", "reply")
		}
	}
}

type gameSession struct {
	game     *GameIngress
	resource *ingressBinding
}

func (session gameSession) Principal() Principal { return clonePrincipal(session.resource.principal) }
func (session gameSession) Reply(ctx context.Context, value any) error {
	reply, ok := value.(TCPReply)
	if !ok {
		return ErrInvalidRequest
	}
	packet := reply.TCPResponse()
	if packet == nil {
		return ErrInvalidRequest
	}
	return session.game.enqueueOutbound(ctx, session.resource, packet.MessageID, packet.Sequence, 0, false, packet.Payload)
}
func (session gameSession) Close(reason error) error {
	if _, changed := session.game.bindings.close(session.resource.record.identity); changed {
		session.game.retire(session.resource.record)
	}
	return nil
}

var _ Session = gameSession{}
