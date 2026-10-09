//go:build integration

package gate_test

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/client/wire"
	"github.com/tjbdwanghaibo/roost-core/infra/network/bus"
	"github.com/tjbdwanghaibo/roost-core/infra/network/gateway"
	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
)

// ACK 在真实 Gate 准入后由客户端适配器丢弃，准确注入“已经生效但发送者未知”的边界。
// 不把此适配器注入称为物理网络丢包。
type lostOutboundACK struct {
	fnats.RawClient
	attempts atomic.Int64
}

func (client *lostOutboundACK) RequestContext(ctx context.Context, subject string, data []byte) ([]byte, error) {
	result, err := client.RawClient.RequestContext(ctx, subject, data)
	if strings.HasSuffix(subject, ".outbound") && err == nil {
		client.attempts.Add(1)
		return nil, context.DeadlineExceeded
	}
	return result, err
}
func TestRealGateLostOutboundACKClosesLifetimeWithoutReplay(t *testing.T) {
	closed := make(chan uint64, 1)
	var loss *lostOutboundACK
	h := realGateHarness(t, nil, func(deps *gateway.GameDependencies) {
		loss = &lostOutboundACK{RawClient: deps.Client}
		deps.Client = loss
		deps.OnClosed = func(_ gateway.Binding, id uint64) { closed <- id }
	})
	conn := h.connect(t)
	if err := wire.Write(conn, []*wire.Packet{{MsgID: 101, Seq: 2, Payload: []byte("committed response")}}, h.config.MaxPayloadBytes); err != nil {
		t.Fatal(err)
	}
	// 接纳 ACK 丢失可能使连接在 writer 前关闭，不能要求网络响应必然可见。
	packet, readErr := wire.Read(conn, h.config.MaxPayloadBytes)
	if readErr == nil && string(packet.Payload) != "committed response" {
		t.Fatalf("packet=%+v", packet)
	}
	select {
	case id := <-closed:
		if id == 0 {
			t.Fatal("missing old lifetime")
		}
	case <-time.After(time.Second):
		t.Fatal("ACK loss did not close Game lifetime")
	}
	if h.calls.Load() != 1 || loss.attempts.Load() != 1 {
		t.Fatalf("business=%d outbound=%d", h.calls.Load(), loss.attempts.Load())
	}
	if readErr == nil {
		if _, err := wire.Read(conn, h.config.MaxPayloadBytes); err == nil {
			t.Fatal("outbound replayed or socket kept alive after unknown ACK")
		}
	}
	if h.game.Stats().Bindings != 0 {
		t.Fatal("failed binding still active")
	}
}

func TestRealGateOldUnbindAndCloseCannotAffectReplacement(t *testing.T) {
	h := realGateHarness(t, nil)
	oldConn := h.connect(t)
	if err := wire.Write(oldConn, []*wire.Packet{{MsgID: 101, Seq: 2}}, h.config.MaxPayloadBytes); err != nil {
		t.Fatal(err)
	}
	if _, err := wire.Read(oldConn, h.config.MaxPayloadBytes); err != nil {
		t.Fatal(err)
	}
	old, _, ok := h.game.SessionReceiver("session")
	if !ok {
		t.Fatal("old binding missing")
	}
	conn := h.connect(t)
	if err := wire.Write(conn, []*wire.Packet{{MsgID: 101, Seq: 2}}, h.config.MaxPayloadBytes); err != nil {
		t.Fatal(err)
	}
	if _, err := wire.Read(conn, h.config.MaxPayloadBytes); err != nil {
		t.Fatal(err)
	}
	current, _, ok := h.game.SessionReceiver("session")
	if !ok || current == old {
		t.Fatal("replacement not installed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	budget, _ := gateway.BudgetFromContext(ctx)
	request := gateway.CloseRequest{Version: gateway.InternalVersion, Binding: old, Budget: budget, Reason: "late close"}
	codec := bus.MessagePackCodec{}
	bytes, err := codec.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	subject, _ := gateway.ChannelSubject(h.config.Namespace, "gate", old.Gate, "game", old.Game, "close")
	if err := h.gameClient.PublishOnce(subject, bytes); err != nil {
		t.Fatal(err)
	}
	raw, err := h.gateClient.ForInbox(mustInbox(h.config.Namespace, "gate", old.Gate))
	if err != nil {
		t.Fatal(err)
	}
	unbind := gateway.ControlRequest{Version: gateway.InternalVersion, Operation: gateway.Unbind, Binding: old, Budget: budget}
	bytes, err = codec.Marshal(unbind)
	if err != nil {
		t.Fatal(err)
	}
	subject, _ = gateway.ChannelSubject(h.config.Namespace, "game", old.Game, "gate", old.Gate, "control")
	if _, err := raw.RequestContext(ctx, subject, bytes); err != nil {
		t.Fatal(err)
	}
	if err := h.gameClient.FlushContext(ctx); err != nil {
		t.Fatal(err)
	}
	if err := wire.Write(conn, []*wire.Packet{{MsgID: 101, Seq: 3, Payload: []byte("new lifetime")}}, h.config.MaxPayloadBytes); err != nil {
		t.Fatal(err)
	}
	response, err := wire.Read(conn, h.config.MaxPayloadBytes)
	if err != nil || string(response.Payload) != "new lifetime" {
		t.Fatalf("new response=%+v error=%v", response, err)
	}
	if err := h.game.PushBound(ctx, old, 102, wire.PayloadSync, nil); !errors.Is(err, gateway.ErrBindingStale) {
		t.Fatalf("old queue admission=%v", err)
	}
}
func mustInbox(namespace, role string, id gateway.ProcessIdentity) string {
	prefix, err := gateway.InboxPrefix(namespace, role, id)
	if err != nil {
		panic(err)
	}
	return prefix
}

// 广播已经准入但 ACK 丢失时保留 Unknown；同 ID 查询首次结果，不再次写 socket。
type lostBroadcastACK struct {
	fnats.RawClient
	attempts atomic.Int64
}

func (client *lostBroadcastACK) RequestContext(ctx context.Context, subject string, data []byte) ([]byte, error) {
	result, err := client.RawClient.RequestContext(ctx, subject, data)
	if strings.HasSuffix(subject, ".broadcast") && err == nil {
		client.attempts.Add(1)
		return nil, context.DeadlineExceeded
	}
	return result, err
}
func TestRealBroadcastACKLossUnknownAndSameIDQuery(t *testing.T) {
	h := realGateHarness(t, nil)
	conn := h.connect(t)
	if err := wire.Write(conn, []*wire.Packet{{MsgID: 101, Seq: 2}}, h.config.MaxPayloadBytes); err != nil {
		t.Fatal(err)
	}
	if _, err := wire.Read(conn, h.config.MaxPayloadBytes); err != nil {
		t.Fatal(err)
	}
	binding, _, ok := h.game.SessionReceiver("session")
	if !ok {
		t.Fatal("binding missing")
	}
	raw, err := h.gameClient.ForInbox(mustInbox(h.config.Namespace, "game", binding.Game))
	if err != nil {
		t.Fatal(err)
	}
	loss := &lostBroadcastACK{RawClient: raw}
	sender := gateway.BroadcastSender{Config: h.config, Identity: binding.Game, Role: "game", Client: loss, Gates: func(context.Context) ([]gateway.ProcessIdentity, error) {
		return []gateway.ProcessIdentity{binding.Gate}, nil
	}}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	id := gateway.NewBroadcastID()
	audience := gateway.Audience{Kind: gateway.Players, PlayerIDs: []int64{binding.PlayerID}}
	result, err := sender.Send(ctx, id, audience, 201, []byte("once"))
	if err != nil || len(result.Gates) != 1 || result.Gates[0].Outcome != gateway.Unknown || loss.attempts.Load() != 1 {
		t.Fatalf("unknown=%+v error=%v attempts=%d", result, err, loss.attempts.Load())
	}
	packet, err := wire.Read(conn, h.config.MaxPayloadBytes)
	if err != nil || packet.MsgID != 201 || string(packet.Payload) != "once" {
		t.Fatalf("packet=%+v err=%v", packet, err)
	}
	sender.Client = raw
	result, err = sender.Send(ctx, id, audience, 201, []byte("once"))
	if err != nil || result.Gates[0].Outcome != gateway.Completed || result.Gates[0].Admission.Accepted != 1 {
		t.Fatalf("cached=%+v error=%v", result, err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(30 * time.Millisecond))
	if packet, err := wire.Read(conn, h.config.MaxPayloadBytes); err == nil {
		t.Fatalf("duplicate write=%+v", packet)
	}
}

// NATS 的错误回调可能在 Start 返回前发生，启动末尾不能把故障位重新置为健康。
type failedStartupSubscription struct{ fnats.RawClient }

func (client failedStartupSubscription) SubscribeBounded(subject string, limits fnats.PendingLimits, handler fnats.MsgHandler) (fnats.DrainSubscription, error) {
	if limits.OnError != nil {
		limits.OnError(fnats.ErrClosed)
	}
	return client.RawClient.SubscribeBounded(subject, limits, handler)
}
func TestRealStartupSubscriptionFailureCannotBeOverwritten(t *testing.T) {
	h := realGateHarness(t, nil, func(deps *gateway.GameDependencies) { deps.Client = failedStartupSubscription{RawClient: deps.Client} })
	conn, err := net.DialTimeout("tcp", h.server.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(time.Second))
	if err := wire.Write(conn, []*wire.Packet{{Seq: 1, Payload: []byte("ticket")}}, h.config.MaxPayloadBytes); err != nil {
		t.Fatal(err)
	}
	if packet, err := wire.Read(conn, h.config.MaxPayloadBytes); err == nil {
		t.Fatalf("fenced startup authenticated: %+v", packet)
	}
	if h.game.Stats().Bindings != 0 || h.calls.Load() != 0 {
		t.Fatal("fenced startup admitted binding/business")
	}
}
