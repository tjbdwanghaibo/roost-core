package gateway

import (
	"context"
	"crypto/sha256"
	"errors"
	"sync"
	"time"

	"github.com/tjbdwanghaibo/roost-core/client/wire"
	"github.com/tjbdwanghaibo/roost-core/infra/network/bus"
	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
)

type broadcastKey struct {
	source   ProcessIdentity
	role, id string
}
type broadcastRecord struct {
	digest  [32]byte
	done    chan struct{}
	result  BroadcastResponse
	expires time.Time
}

func (gate *Gate) receiveBroadcast(role string, msg *fnats.Msg) {
	if msg == nil || len(msg.Data) > gate.config.SubscriptionBytes {
		return
	}
	var request BroadcastRequest
	if gate.codec.Unmarshal(msg.Data, &request) != nil || request.Version != InternalVersion || request.Source.Validate() != nil || request.Target != gate.identity || len(request.BroadcastID) != 32 || !subjectToken(request.BroadcastID) || request.MessageID == 0 || len(request.Payload) > gate.config.MaxPayloadBytes || request.Audience.Validate(gate.config.MaxBroadcastPlayers) != nil {
		return
	}
	subject, err := ChannelSubject(gate.config.Namespace, "gate", gate.identity, role, request.Source, "broadcast")
	if err != nil || subject != msg.Subject || !replyMatches(gate.config.Namespace, role, request.Source, msg.Reply) {
		return
	}
	ctx, cancel, err := request.Budget.Context(gate.ctx, gate.config.RequestTimeout, gate.config.ClockSkew)
	if err != nil {
		return
	}
	gate.mu.Lock()
	if gate.closing || !gate.ready() {
		gate.mu.Unlock()
		cancel()
		gate.replyBroadcast(msg.Reply, BroadcastResponse{ExecutionResult: ExecutionResult{Version: InternalVersion, Outcome: NotAdmitted, Reason: "draining"}})
		return
	}
	select {
	case gate.broadcastSlots <- struct{}{}:
	default:
		gate.mu.Unlock()
		cancel()
		gate.replyBroadcast(msg.Reply, BroadcastResponse{ExecutionResult: ExecutionResult{Version: InternalVersion, Outcome: NotAdmitted, Reason: "broadcast capacity"}})
		return
	}
	gate.broadcastWait.Add(1)
	gate.mu.Unlock()
	go func() {
		defer gate.broadcastWait.Done()
		defer func() { <-gate.broadcastSlots }()
		defer cancel()
		result := gate.admitBroadcast(ctx, role, request)
		gate.replyBroadcast(msg.Reply, result)
	}()
}
func (gate *Gate) admitBroadcast(ctx context.Context, role string, request BroadcastRequest) (result BroadcastResponse) {
	result.ExecutionResult = ExecutionResult{Version: InternalVersion, Outcome: NotAdmitted, Reason: "broadcast refused"}
	defer func() {
		if recover() != nil {
			result = BroadcastResponse{ExecutionResult: ExecutionResult{Version: InternalVersion, Outcome: Unknown, Reason: "broadcast failed"}}
		}
	}()
	if role == "game" && (request.Audience.Kind == AllOnline || (request.Audience.Kind == GameOnline && request.Audience.GameID != request.Source.ServerID)) {
		return result
	}
	if match, err := gate.deps.Matches(ctx, role, request.Source); err != nil || !match {
		return result
	}
	// Budget/Target 不属于内容身份，允许同 ID 在窗口内查询同一结果，拒绝换内容复用。
	encoded, err := gate.codec.Marshal(struct {
		Audience  Audience
		MessageID uint32
		Payload   []byte
	}{request.Audience, request.MessageID, request.Payload})
	if err != nil {
		return result
	}
	digest := sha256.Sum256(encoded)
	key := broadcastKey{request.Source, role, request.BroadcastID}
	gate.mu.Lock()
	now := time.Now()
	for key, record := range gate.broadcasts {
		if !now.Before(record.expires) {
			select {
			case <-record.done:
				delete(gate.broadcasts, key)
			default:
			}
		}
	}
	if prior := gate.broadcasts[key]; prior != nil {
		gate.mu.Unlock()
		if prior.digest != digest {
			return result
		}
		select {
		case <-prior.done:
			return prior.result
		case <-ctx.Done():
			return BroadcastResponse{ExecutionResult: ExecutionResult{Version: InternalVersion, Outcome: Unknown, Reason: "broadcast still executing"}}
		}
	}
	if len(gate.broadcasts) >= gate.config.MaxBroadcastRecords {
		gate.mu.Unlock()
		return result
	}
	record := &broadcastRecord{digest: digest, done: make(chan struct{}), expires: now.Add(gate.config.BroadcastTTL)}
	gate.broadcasts[key] = record
	defer func() {
		if recover() != nil {
			result = BroadcastResponse{ExecutionResult: ExecutionResult{Version: InternalVersion, Outcome: Unknown, Reason: "broadcast failed"}}
		}
		record.result = result
		record.expires = time.Now().Add(gate.config.BroadcastTTL)
		close(record.done)
		gate.mu.Unlock()
	}()
	// 完整临界区内形成受众并准入，与断线替换及普通出站共享同一连接表。
	players := make(map[int64]struct{}, len(request.Audience.PlayerIDs))
	for _, id := range request.Audience.PlayerIDs {
		players[id] = struct{}{}
	}
	result.ExecutionResult = ExecutionResult{Version: InternalVersion, Outcome: Completed}
	for _, connection := range gate.connections {
		if !connection.active {
			continue
		}
		if role == "game" && connection.binding.Game != request.Source {
			continue
		}
		if request.Audience.Kind == GameOnline && connection.binding.Game.ServerID != request.Audience.GameID {
			continue
		}
		if request.Audience.Kind == Players {
			if _, ok := players[connection.binding.PlayerID]; !ok {
				continue
			}
		}
		result.Matched++
		if ctx.Err() != nil {
			result.Refused++
			continue
		}
		packet := OutboundPacket{Version: InternalVersion, Binding: connection.binding, Budget: request.Budget, MessageID: request.MessageID, Kind: wire.PayloadProtobuf, Push: true, Payload: request.Payload}
		data, err := gate.codec.Marshal(packet)
		if err == nil {
			err = gate.queue.Enqueue(ctx, connection.receiverID, data)
		}
		if err != nil {
			result.Refused++
		} else {
			result.Accepted++
		}
	}
	return result
}
func (gate *Gate) replyBroadcast(reply string, result BroadcastResponse) {
	observeResult("gate", "broadcast", result.Outcome)
	if data, err := gate.codec.Marshal(result); err == nil {
		if gate.deps.Client.PublishOnce(reply, data) != nil {
			observeFailure("gate", "reply")
		}
	}
}

// BroadcastSender 显式声明发起角色；系统全服广播需要自己的 singleton 身份与 NATS ACL。
// 各 Gate 只尝试一次；Unknown 可能已投递，结果不会被伪装成零或自动重投。
type BroadcastSender struct {
	Config   Config
	Identity ProcessIdentity
	Role     string
	Client   fnats.RawClient
	Gates    func(context.Context) ([]ProcessIdentity, error)
}

func (sender BroadcastSender) Send(ctx context.Context, id string, audience Audience, messageID uint32, payload []byte) (BroadcastResult, error) {
	if err := errors.Join(sender.Config.Validate(), sender.Identity.Validate(), audience.Validate(sender.Config.MaxBroadcastPlayers)); err != nil {
		return BroadcastResult{}, err
	}
	if sender.Client == nil || sender.Gates == nil || (sender.Role != "game" && sender.Role != "system") || len(id) != 32 || !subjectToken(id) || messageID == 0 || len(payload) > sender.Config.MaxPayloadBytes {
		return BroadcastResult{}, ErrInvalidRequest
	}
	if sender.Role == "game" && (audience.Kind == AllOnline || (audience.Kind == GameOnline && audience.GameID != sender.Identity.ServerID)) {
		return BroadcastResult{}, ErrInvalidRequest
	}
	ctx, cancel := context.WithTimeout(ctx, sender.Config.RequestTimeout)
	defer cancel()
	gates, err := sender.Gates(ctx)
	if err != nil {
		return BroadcastResult{}, err
	}
	if len(gates) > sender.Config.MaxBindings {
		return BroadcastResult{}, ErrAdmissionFull
	}
	unique := make(map[ProcessIdentity]struct{}, len(gates))
	result := BroadcastResult{}
	for _, gate := range gates {
		if gate.Validate() != nil {
			return BroadcastResult{}, ErrInvalidRequest
		}
		if _, ok := unique[gate]; !ok {
			unique[gate] = struct{}{}
			result.Gates = append(result.Gates, BroadcastGateResult{Gate: gate, Outcome: Unknown})
		}
	}
	jobs := make(chan int, len(result.Gates))
	for index := range result.Gates {
		jobs <- index
	}
	close(jobs)
	var wait sync.WaitGroup
	for range min(sender.Config.BroadcastWorkers, len(result.Gates)) {
		wait.Go(func() {
			for index := range jobs {
				target := result.Gates[index].Gate
				budget, err := BudgetFromContext(ctx)
				if err != nil {
					continue
				}
				request := BroadcastRequest{Version: InternalVersion, Source: sender.Identity, Target: target, BroadcastID: id, Budget: budget, Audience: audience, MessageID: messageID, Payload: payload}
				codec := bus.MessagePackCodec{}
				data, err := codec.Marshal(request)
				if err != nil {
					continue
				}
				subject, _ := ChannelSubject(sender.Config.Namespace, "gate", target, sender.Role, sender.Identity, "broadcast")
				data, err = sender.Client.RequestContext(ctx, subject, data)
				if err != nil {
					continue
				}
				var response BroadcastResponse
				if codec.Unmarshal(data, &response) != nil || response.Version != InternalVersion || (response.Outcome != Completed && response.Outcome != NotAdmitted) {
					continue
				}
				result.Gates[index].Outcome = response.Outcome
				result.Gates[index].Admission = response
			}
		})
	}
	wait.Wait()
	return result, nil
}
func NewBroadcastID() string { return newBindingToken() }
