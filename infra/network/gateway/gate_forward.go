package gateway

import (
	"context"
	"errors"
	"fmt"

	"github.com/tjbdwanghaibo/roost-core/client/wire"
)

// Forward 总预算从 Gate 准入建立，包含并发槽等待、NATS、Game 队列及 Nest。
// RequestContext 固定只调用一次；已进入 dispatcher 的超时不能自动重试。
func (gate *Gate) Forward(parent context.Context, session Session, messageID, sequence uint32, kind wire.PayloadKind, payload []byte) (any, error) {
	connectionSession, ok := session.(ConnectionSession)
	if !ok {
		return nil, ErrInvalidRequest
	}
	ctx, cancel := context.WithTimeout(parent, gate.config.RequestTimeout)
	defer cancel()
	gate.mu.Lock()
	connection := gate.connections[connectionSession.ConnectionID()]
	size := int64(len(payload)) + 1024 // 编码后核对并按实际大小结算；先保守预留身份开销。
	if connection == nil || !connection.active || gate.closing || !gate.ready() {
		gate.mu.Unlock()
		return nil, ErrBindingStale
	}
	binding := connection.binding
	if gate.residentRequests >= gate.config.MaxForwardRequests || size > gate.config.MaxForwardBytes-gate.residentBytes {
		gate.mu.Unlock()
		return nil, ErrAdmissionFull
	}
	slots := gate.gameSlots[binding.Game]
	if slots == nil {
		slots = make(chan struct{}, gate.config.PerGameInFlight)
		gate.gameSlots[binding.Game] = slots
	}
	gate.gameRequests[binding.Game]++
	gate.residentRequests++
	gate.residentBytes += size
	gate.forwards.Add(1)
	gate.mu.Unlock()
	defer func() {
		gate.mu.Lock()
		gate.gameRequests[binding.Game]--
		if gate.gameRequests[binding.Game] == 0 && !gate.hasGameLocked(binding.Game) {
			delete(gate.gameRequests, binding.Game)
			delete(gate.gameSlots, binding.Game)
		}
		gate.residentRequests--
		gate.residentBytes -= size
		gate.mu.Unlock()
		gate.forwards.Done()
	}()
	// 先等待目标 Game 容量，避免慢 Game 占满全局实际执行槽。
	select {
	case slots <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-slots }()
	select {
	case gate.forwardSlots <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-gate.forwardSlots }()
	budget, err := BudgetFromContext(ctx)
	if err != nil {
		return nil, err
	}
	request := ForwardRequest{Version: InternalVersion, Binding: binding, RequestID: newBindingToken(), Budget: budget, MessageID: messageID, Sequence: sequence, Kind: kind, Payload: payload}
	if err = request.Validate(gate.config.MaxPayloadBytes); err != nil {
		return nil, err
	}
	data, err := gate.codec.Marshal(request)
	if err != nil {
		return nil, err
	}
	gate.mu.Lock()
	extra := int64(len(data)) - size
	if extra > gate.config.MaxForwardBytes-gate.residentBytes {
		gate.mu.Unlock()
		return nil, ErrAdmissionFull
	}
	gate.residentBytes += extra
	size += extra
	gate.mu.Unlock()
	subject, _ := ChannelSubject(gate.config.Namespace, "game", binding.Game, "gate", gate.identity, "forward")
	data, err = gate.deps.Client.RequestContext(ctx, subject, data)
	if err != nil {
		gate.closeConnection(connection, err)
		return nil, err
	}
	var result ExecutionResult
	if err = gate.codec.Unmarshal(data, &result); err != nil || result.Version != InternalVersion {
		gate.closeConnection(connection, ErrInvalidRequest)
		return nil, ErrInvalidRequest
	}
	if result.Outcome != Completed {
		err = fmt.Errorf("gateway: forward outcome %d: %s", result.Outcome, result.Reason)
		gate.closeConnection(connection, err)
		return nil, err
	}
	// Completed 仅承诺 Game 本地出站；保留在途资格直到其水位在 Gate 实际准入。
	// 不在 Game handler/Guard 等 NATS，socket 写出由 Gate 队列独立完成。
	for {
		gate.mu.Lock()
		if gate.receivers[connection.receiverID] != connection {
			gate.mu.Unlock()
			return nil, ErrBindingStale
		}
		if connection.lastOut >= result.OutSeq {
			gate.mu.Unlock()
			return nil, nil
		}
		changed := connection.changed
		gate.mu.Unlock()
		select {
		case <-changed:
		case <-ctx.Done():
			gate.closeConnection(connection, ctx.Err())
			return nil, errors.Join(ctx.Err(), ErrSessionClosed)
		}
	}
}
