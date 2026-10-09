package gateway

import (
	"context"
	"errors"
	"time"

	"github.com/tjbdwanghaibo/roost-core/client/wire"
)

// PlayerRuntime 是业务使用的统一推送能力。embedded 与 Game Ingress 是部署选择，
// PB/Sync/Lockstep 调用方不持 socket 或 NATS；nil 仅承诺本地发送准入。
type PlayerRuntime interface {
	PushPlayer(context.Context, int64, uint32, any) error
	PushSession(context.Context, string, uint32, any) error
	PushSyncPlayer(context.Context, int64, uint32, []byte) error
	PushSyncSession(context.Context, string, uint32, []byte) error
	PushLockstepPlayer(context.Context, int64, uint32, []byte) error
	PushLockstepSession(context.Context, string, uint32, []byte) error
	LoginTimeout() time.Duration
	ActiveSessions(int64) int
	CloseSessions(int64, error) int
	OnSessionClosed(func(TCPSessionClosed)) func()
	StopNotifications(context.Context) error
}

func (game *GameIngress) playerResources(playerID int64) []*ingressBinding {
	game.mu.Lock()
	defer game.mu.Unlock()
	var result []*ingressBinding
	for _, resource := range game.resources {
		if resource.principal.PlayerID == playerID && resource.activated && !resource.closed {
			result = append(result, resource)
		}
	}
	return result
}
func (game *GameIngress) PushPlayer(ctx context.Context, playerID int64, messageID uint32, value any) error {
	if game.deps.Encoder == nil {
		return ErrTransportUnavailable
	}
	payload, err := game.deps.Encoder(messageID, value)
	if err != nil {
		return err
	}
	return game.pushPlayer(ctx, playerID, messageID, wire.PayloadProtobuf, payload)
}
func (game *GameIngress) PushSyncPlayer(ctx context.Context, playerID int64, messageID uint32, payload []byte) error {
	return game.pushPlayer(ctx, playerID, messageID, wire.PayloadSync, payload)
}
func (game *GameIngress) PushLockstepPlayer(ctx context.Context, playerID int64, messageID uint32, payload []byte) error {
	return game.pushPlayer(ctx, playerID, messageID, wire.PayloadLockstep, payload)
}
func (game *GameIngress) pushPlayer(ctx context.Context, playerID int64, messageID uint32, kind wire.PayloadKind, payload []byte) error {
	resources := game.playerResources(playerID)
	if len(resources) == 0 {
		return ErrSessionNotFound
	}
	var failures []error
	for _, resource := range resources {
		if err := game.enqueueOutbound(ctx, resource, messageID, 0, kind, true, payload); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}
func (game *GameIngress) ActiveSessions(playerID int64) int {
	return len(game.playerResources(playerID))
}
func (game *GameIngress) CloseSessions(playerID int64, reason error) int {
	count := 0
	for _, resource := range game.playerResources(playerID) {
		if record, changed := game.bindings.close(resource.record.identity); changed {
			game.retire(record)
			count++
		}
	}
	return count
}
func (game *GameIngress) OnSessionClosed(fn func(TCPSessionClosed)) func() {
	return game.lifecycle.OnSessionClosed(fn)
}
func (game *GameIngress) StopNotifications(ctx context.Context) error {
	return game.lifecycle.StopNotifications(ctx)
}

var _ PlayerRuntime = (*TCPRuntime)(nil)
var _ PlayerRuntime = (*GameIngress)(nil)

// PushBound 针对已冻结的完整 lifetime；resolver 与发送之间重绑也不能把旧帧送给新会话。
func (game *GameIngress) PushBound(ctx context.Context, binding Binding, messageID uint32, kind wire.PayloadKind, payload []byte) error {
	record, err := game.bindings.lookup(binding, true)
	if err != nil {
		return err
	}
	game.mu.Lock()
	resource := game.resources[record.receiverID]
	game.mu.Unlock()
	if resource == nil {
		return ErrBindingStale
	}
	return game.enqueueOutbound(ctx, resource, messageID, 0, kind, true, payload)
}
func (game *GameIngress) ReceiverBinding(receiverID uint64) (Binding, bool) {
	game.mu.Lock()
	resource := game.resources[receiverID]
	if resource == nil || resource.closed {
		game.mu.Unlock()
		return Binding{}, false
	}
	binding := resource.record.identity
	game.mu.Unlock()
	if _, err := game.bindings.lookup(binding, true); err != nil {
		return Binding{}, false
	}
	return binding, true
}

func (game *GameIngress) MaxPayloadBytes() int { return game.config.MaxPayloadBytes }
