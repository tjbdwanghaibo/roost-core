package gateway

import (
	"context"
	"errors"
	"fmt"

	"github.com/tjbdwanghaibo/roost-core/client/wire"
)

// forwardAdmission 是一次 Forward 在 Gate 上占用的准入额度：目标连接与它此刻的绑定、目标 Game 的在途槽，以及
// 计入 residentBytes 的字节数。size 先按负载保守预留，编码后按实际大小结算；releaseForward 按最终值归还。
type forwardAdmission struct {
	connection *gateConnection
	binding    Binding
	slots      chan struct{}
	size       int64
}

// Forward 总预算从 Gate 准入建立，包含并发槽等待、NATS、Game 队列及 Nest。
// RequestContext 固定只调用一次；已进入 dispatcher 的超时不能自动重试。
//
// 步骤：准入并预留额度 → 等目标 Game 槽、再等全局执行槽 → 编码请求并按实际大小结算 → 请求 Game →
// 校验结论 → 等出站水位在 Gate 准入。额度与槽在返回时按取得的相反顺序归还。
func (gate *Gate) Forward(parent context.Context, session Session, messageID, sequence uint32, kind wire.PayloadKind, payload []byte) (any, error) {
	connectionSession, ok := session.(ConnectionSession)
	if !ok {
		return nil, ErrInvalidRequest
	}
	ctx, cancel := context.WithTimeout(parent, gate.config.RequestTimeout)
	defer cancel()
	// 编码后核对并按实际大小结算；先保守预留身份开销。
	admission, err := gate.admitForward(connectionSession.ConnectionID(), int64(len(payload))+1024)
	if err != nil {
		return nil, err
	}
	defer gate.releaseForward(&admission)
	// 先等待目标 Game 容量，避免慢 Game 占满全局实际执行槽。
	select {
	case admission.slots <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-admission.slots }()
	select {
	case gate.forwardSlots <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-gate.forwardSlots }()
	data, err := gate.encodeForwardRequest(ctx, admission.binding, messageID, sequence, kind, payload)
	if err != nil {
		return nil, err
	}
	if err := gate.settleForwardBytes(&admission, len(data)); err != nil {
		return nil, err
	}
	subject, _ := ChannelSubject(gate.config.Namespace, "game", admission.binding.Game, "gate", gate.identity, "forward")
	data, err = gate.deps.Client.RequestContext(ctx, subject, data)
	if err != nil {
		gate.closeConnection(admission.connection, err)
		return nil, err
	}
	result, err := gate.forwardResult(admission.connection, data)
	if err != nil {
		return nil, err
	}
	return nil, gate.awaitForwardOutbound(ctx, admission.connection, result.OutSeq)
}

// admitForward 在 Gate 锁内确认连接仍活跃、Gate 就绪，按全局请求数与字节上限准入，并预留 size 字节、
// 登记目标 Game 的在途请求（首次时建立该 Game 的槽）。
func (gate *Gate) admitForward(connectionID string, size int64) (forwardAdmission, error) {
	gate.mu.Lock()
	connection := gate.connections[connectionID]
	if connection == nil || !connection.active || gate.closing || !gate.ready() {
		gate.mu.Unlock()
		return forwardAdmission{}, ErrBindingStale
	}
	binding := connection.binding
	if gate.residentRequests >= gate.config.MaxForwardRequests || size > gate.config.MaxForwardBytes-gate.residentBytes {
		gate.mu.Unlock()
		return forwardAdmission{}, ErrAdmissionFull
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
	return forwardAdmission{connection: connection, binding: binding, slots: slots, size: size}, nil
}

// releaseForward 归还 admitForward 的额度；目标 Game 已无在途请求且不再有连接时回收它的计数与槽。
func (gate *Gate) releaseForward(admission *forwardAdmission) {
	game := admission.binding.Game
	gate.mu.Lock()
	gate.gameRequests[game]--
	if gate.gameRequests[game] == 0 && !gate.hasGameLocked(game) {
		delete(gate.gameRequests, game)
		delete(gate.gameSlots, game)
	}
	gate.residentRequests--
	gate.residentBytes -= admission.size
	gate.mu.Unlock()
	gate.forwards.Done()
}

// encodeForwardRequest 以剩余预算构造并编码转发请求。
func (gate *Gate) encodeForwardRequest(ctx context.Context, binding Binding, messageID, sequence uint32, kind wire.PayloadKind, payload []byte) ([]byte, error) {
	budget, err := BudgetFromContext(ctx)
	if err != nil {
		return nil, err
	}
	request := ForwardRequest{Version: InternalVersion, Binding: binding, RequestID: newBindingToken(), Budget: budget, MessageID: messageID, Sequence: sequence, Kind: kind, Payload: payload}
	if err = request.Validate(gate.config.MaxPayloadBytes); err != nil {
		return nil, err
	}
	return gate.codec.Marshal(request)
}

// settleForwardBytes 把预留按编码后的实际大小结算；超出字节上限时拒绝，预留保持原值由 releaseForward 归还。
func (gate *Gate) settleForwardBytes(admission *forwardAdmission, encoded int) error {
	gate.mu.Lock()
	extra := int64(encoded) - admission.size
	if extra > gate.config.MaxForwardBytes-gate.residentBytes {
		gate.mu.Unlock()
		return ErrAdmissionFull
	}
	gate.residentBytes += extra
	admission.size += extra
	gate.mu.Unlock()
	return nil
}

// forwardResult 解码 Game 的执行结论。无法解码、版本不符或不是 Completed 都关闭连接并返回错误。
func (gate *Gate) forwardResult(connection *gateConnection, data []byte) (ExecutionResult, error) {
	var result ExecutionResult
	if err := gate.codec.Unmarshal(data, &result); err != nil || result.Version != InternalVersion {
		gate.closeConnection(connection, ErrInvalidRequest)
		return result, ErrInvalidRequest
	}
	if result.Outcome != Completed {
		err := fmt.Errorf("gateway: forward outcome %d: %s", result.Outcome, result.Reason)
		gate.closeConnection(connection, err)
		return result, err
	}
	return result, nil
}

// awaitForwardOutbound 等连接的出站水位追上 outSeq。
//
// Completed 仅承诺 Game 本地出站；保留在途资格直到其水位在 Gate 实际准入。
// 不在 Game handler/Guard 等 NATS，socket 写出由 Gate 队列独立完成。
func (gate *Gate) awaitForwardOutbound(ctx context.Context, connection *gateConnection, outSeq uint64) error {
	for {
		gate.mu.Lock()
		if gate.receivers[connection.receiverID] != connection {
			gate.mu.Unlock()
			return ErrBindingStale
		}
		if connection.lastOut >= outSeq {
			gate.mu.Unlock()
			return nil
		}
		changed := connection.changed
		gate.mu.Unlock()
		select {
		case <-changed:
		case <-ctx.Done():
			gate.closeConnection(connection, ctx.Err())
			return errors.Join(ctx.Err(), ErrSessionClosed)
		}
	}
}
