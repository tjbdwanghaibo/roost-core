package gateway

import (
	"context"

	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
)

func (gate *Gate) receiveOutbound(msg *fnats.Msg) {
	if msg == nil || len(msg.Data) > gate.config.Outbound.MaxPacketBytes {
		return
	}
	var packet OutboundPacket
	if gate.codec.Unmarshal(msg.Data, &packet) != nil || packet.Validate(gate.config.MaxPayloadBytes) != nil {
		return
	}
	subject, err := ChannelSubject(gate.config.Namespace, "gate", gate.identity, "game", packet.Binding.Game, "outbound")
	if err != nil || subject != msg.Subject || packet.Binding.Gate != gate.identity || !replyMatches(gate.config.Namespace, "game", packet.Binding.Game, msg.Reply) {
		return
	}
	result := ExecutionResult{Version: InternalVersion, Outcome: NotAdmitted, Reason: "inactive binding"}
	ctx, cancel, err := packet.Budget.Context(gate.ctx, gate.config.Outbound.MaxAge, gate.config.ClockSkew)
	if err != nil {
		gate.replyExecution(msg.Reply, result)
		return
	}
	defer cancel()
	gate.mu.Lock()
	connection := gate.connections[packet.Binding.ConnectionNonce]
	if connection == nil || connection.binding != packet.Binding || !connection.active || !gate.ready() {
		gate.mu.Unlock()
		gate.replyExecution(msg.Reply, result)
		return
	}
	if packet.OutSeq <= connection.lastOut {
		// 重复仅确认已准入的前缀；不再写一次 socket。
		result = ExecutionResult{Version: InternalVersion, Outcome: Completed, OutSeq: packet.OutSeq}
		gate.mu.Unlock()
		gate.replyExecution(msg.Reply, result)
		return
	}
	if packet.OutSeq != connection.lastOut+1 {
		gate.mu.Unlock()
		gate.closeConnection(connection, ErrBindingStale)
		gate.replyExecution(msg.Reply, result)
		return
	}
	err = gate.queue.Enqueue(ctx, connection.receiverID, msg.Data)
	if err == nil {
		connection.lastOut = packet.OutSeq
		close(connection.changed)
		connection.changed = make(chan struct{})
		result = ExecutionResult{Version: InternalVersion, Outcome: Completed, OutSeq: packet.OutSeq}
	}
	gate.mu.Unlock()
	if err != nil {
		gate.closeConnection(connection, err)
	}
	gate.replyExecution(msg.Reply, result)
}
func (gate *Gate) replyExecution(reply string, result ExecutionResult) {
	observeResult("gate", "outbound", result.Outcome)
	if data, err := gate.codec.Marshal(result); err == nil {
		if gate.deps.Client.PublishOnce(reply, data) != nil {
			observeFailure("gate", "reply")
		}
	}
}
func (gate *Gate) writeOutbound(ctx context.Context, receiverID uint64, data []byte) error {
	gate.mu.Lock()
	connection := gate.receivers[receiverID]
	if connection == nil {
		gate.mu.Unlock()
		return ErrBindingStale
	}
	binding := connection.binding
	gate.mu.Unlock()
	var packet OutboundPacket
	if err := gate.codec.Unmarshal(data, &packet); err != nil {
		return err
	}
	if packet.Binding != binding {
		return ErrBindingStale
	}
	sendCtx, cancel, err := packet.Budget.Context(ctx, gate.config.Outbound.SendTimeout, gate.config.ClockSkew)
	if err != nil {
		return err
	}
	defer cancel()
	return connection.session.WritePacket(sendCtx, packet.MessageID, packet.Sequence, packet.Kind, packet.Push, packet.Payload)
}
func (gate *Gate) outboundFailed(receiverID uint64, err error) {
	observeFailure("gate", "socket")
	gate.mu.Lock()
	connection := gate.receivers[receiverID]
	gate.mu.Unlock()
	if connection != nil {
		gate.closeConnection(connection, err)
	}
}
func (gate *Gate) receiveClose(msg *fnats.Msg) {
	if msg == nil || len(msg.Data) > 16<<10 {
		return
	}
	var request CloseRequest
	if gate.codec.Unmarshal(msg.Data, &request) != nil || request.Version != InternalVersion || request.Binding.Validate() != nil || len(request.Reason) > 128 {
		return
	}
	subject, err := ChannelSubject(gate.config.Namespace, "gate", gate.identity, "game", request.Binding.Game, "close")
	if err != nil || subject != msg.Subject || request.Binding.Gate != gate.identity {
		return
	}
	ctx, cancel, err := request.Budget.Context(gate.ctx, gate.config.RequestTimeout, gate.config.ClockSkew)
	if err != nil {
		return
	}
	cancel()
	_ = ctx
	gate.mu.Lock()
	connection := gate.connections[request.Binding.ConnectionNonce]
	if connection != nil && connection.binding != request.Binding {
		connection = nil
	}
	gate.mu.Unlock()
	if connection != nil {
		gate.closeConnection(connection, ErrSessionClosed)
	}
}

func (gate *Gate) Stop(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := gate.stopSerial.Lock(ctx); err != nil {
		return err
	}
	defer gate.stopSerial.Unlock()
	gate.mu.Lock()
	gate.closing = true
	started := gate.started
	gate.mu.Unlock()
	// 保留 outbound 订阅，使已经进入 Game 的业务能完成响应准入。
	if err := waitRuntime(ctx, &gate.forwards); err != nil {
		return err
	}
	for _, sub := range gate.subscriptions {
		if err := sub.DrainContext(ctx); err != nil {
			return err
		}
	}
	if err := waitRuntime(ctx, &gate.broadcastWait); err != nil {
		return err
	}
	if err := gate.queue.Drain(ctx); err != nil {
		return err
	}
	gate.mu.Lock()
	remaining := make([]*gateConnection, 0, len(gate.connections))
	for _, connection := range gate.connections {
		remaining = append(remaining, connection)
	}
	gate.mu.Unlock()
	for _, connection := range remaining {
		gate.closeConnection(connection, ErrDraining)
	}
	gate.mu.Lock()
	if !gate.controlsClosed {
		close(gate.controls)
		gate.controlsClosed = true
	}
	gate.mu.Unlock()
	if started {
		if err := waitRuntime(ctx, &gate.controlWait); err != nil {
			return err
		}
	}
	gate.cancel()
	if started {
		select {
		case <-gate.sweepDone:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

// Snapshot 是低基数运维快照，不暴露票据或连接身份。
type GateStats struct {
	Bindings, ForwardRequests int
	ForwardBytes              int64
}

func (gate *Gate) Stats() GateStats {
	gate.mu.Lock()
	defer gate.mu.Unlock()
	return GateStats{len(gate.connections), gate.residentRequests, gate.residentBytes}
}
