package gateway

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/tjbdwanghaibo/roost-core/client/wire"
)

// enqueueOutbound 的序号与准入在同一临界区；失败不消耗序号。这里不等待 NATS。
func (game *GameIngress) enqueueOutbound(ctx context.Context, resource *ingressBinding, messageID, sequence uint32, kind wire.PayloadKind, push bool, payload []byte) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if _, err := game.bindings.lookup(resource.record.identity, true); err != nil {
		return err
	}
	resource.outMu.Lock()
	defer resource.outMu.Unlock()
	if resource.outSeq == ^uint64(0) {
		return ErrAdmissionFull
	}
	budgetCtx, cancel := context.WithTimeout(ctx, game.config.Outbound.MaxAge)
	defer cancel()
	budget, err := BudgetFromContext(budgetCtx)
	if err != nil {
		return err
	}
	packet := OutboundPacket{Version: InternalVersion, Binding: resource.record.identity, OutSeq: resource.outSeq + 1, Budget: budget, MessageID: messageID, Sequence: sequence, Kind: kind, Push: push, Payload: payload}
	if err := packet.Validate(game.config.MaxPayloadBytes); err != nil {
		return err
	}
	data, err := game.codec.Marshal(packet)
	if err != nil {
		return err
	}
	if err := game.queue.Enqueue(ctx, resource.record.receiverID, data); err != nil {
		return err
	}
	resource.outSeq++
	return nil
}

func (game *GameIngress) sendOutbound(ctx context.Context, receiverID uint64, data []byte) error {
	game.mu.Lock()
	resource := game.resources[receiverID]
	game.mu.Unlock()
	if resource == nil {
		return ErrBindingStale
	}
	var packet OutboundPacket
	if err := game.codec.Unmarshal(data, &packet); err != nil {
		return err
	}
	if packet.Binding != resource.record.identity {
		return ErrBindingStale
	}
	sendCtx, cancel, err := packet.Budget.Context(ctx, game.config.Outbound.SendTimeout, game.config.ClockSkew)
	if err != nil {
		return err
	}
	defer cancel()
	subject, err := ChannelSubject(game.config.Namespace, "gate", packet.Binding.Gate, "game", game.identity, "outbound")
	if err != nil {
		return err
	}
	response, err := game.deps.Client.RequestContext(sendCtx, subject, data)
	if err != nil {
		return err
	} // ACK 丢失不能重发同一 socket 帧；由 OnError 结束旧 lifetime。
	var result ExecutionResult
	if err := game.codec.Unmarshal(response, &result); err != nil {
		return err
	}
	if result.Version != InternalVersion || result.Outcome != Completed || result.OutSeq != packet.OutSeq {
		return errors.New("gateway: outbound not acknowledged")
	}
	return nil
}

func (game *GameIngress) outboundFailed(receiverID uint64, err error) {
	slog.Warn("game ingress outbound failed; retiring binding", "receiver", receiverID, "err", err)
	observeFailure("game", "outbound")
	game.mu.Lock()
	resource := game.resources[receiverID]
	game.mu.Unlock()
	if resource != nil {
		if record, changed := game.bindings.close(resource.record.identity); changed {
			game.retire(record)
		}
	}
}

func (game *GameIngress) sendNotifications() {
	defer close(game.notificationDone)
	for request := range game.notifications {
		ctx, cancel := context.WithTimeout(context.Background(), game.config.RequestTimeout)
		budget, err := BudgetFromContext(ctx)
		if err == nil {
			request.Budget = budget
			subject, err := ChannelSubject(game.config.Namespace, "gate", request.Binding.Gate, "game", game.identity, "close")
			if err == nil {
				if data, err := game.codec.Marshal(request); err == nil {
					_ = game.deps.Client.PublishOnce(subject, data)
				}
			}
		}
		cancel()
	}
}

func (game *GameIngress) resourceForSession(sessionID string) (*ingressBinding, error) {
	game.mu.Lock()
	defer game.mu.Unlock()
	game.bindings.mu.Lock()
	record := game.bindings.bySession[sessionID]
	game.bindings.mu.Unlock()
	if record == nil {
		return nil, ErrSessionNotFound
	}
	resource := game.resources[record.receiverID]
	if resource == nil || resource.closed {
		return nil, ErrSessionNotFound
	}
	return resource, nil
}

func (game *GameIngress) PushSession(ctx context.Context, sessionID string, messageID uint32, value any) error {
	if game.deps.Encoder == nil {
		return ErrTransportUnavailable
	}
	payload, err := game.deps.Encoder(messageID, value)
	if err != nil {
		return err
	}
	return game.pushSession(ctx, sessionID, messageID, wire.PayloadProtobuf, payload)
}
func (game *GameIngress) PushSyncSession(ctx context.Context, sessionID string, messageID uint32, payload []byte) error {
	return game.pushSession(ctx, sessionID, messageID, wire.PayloadSync, payload)
}
func (game *GameIngress) PushLockstepSession(ctx context.Context, sessionID string, messageID uint32, payload []byte) error {
	return game.pushSession(ctx, sessionID, messageID, wire.PayloadLockstep, payload)
}
func (game *GameIngress) pushSession(ctx context.Context, sessionID string, messageID uint32, kind wire.PayloadKind, payload []byte) error {
	resource, err := game.resourceForSession(sessionID)
	if err != nil {
		return err
	}
	return game.enqueueOutbound(ctx, resource, messageID, 0, kind, true, payload)
}

func (game *GameIngress) LoginTimeout() time.Duration {
	return min(defaultLoginTimeout, game.config.RequestTimeout)
}

// SessionReceiver 返回当前绑定的数值 ID。旧发送未结束也不会复用该 ID。
func (game *GameIngress) SessionReceiver(sessionID string) (Binding, uint64, bool) {
	resource, err := game.resourceForSession(sessionID)
	if err != nil {
		return Binding{}, 0, false
	}
	return resource.record.identity, resource.record.receiverID, true
}
