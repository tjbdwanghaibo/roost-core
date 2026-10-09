//go:build integration

package gate_test

import (
	"context"
	"fmt"
	"net"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/client/wire"
	"github.com/tjbdwanghaibo/roost-core/infra/network/bus"
	"github.com/tjbdwanghaibo/roost-core/infra/network/gateway"
	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
	"github.com/tjbdwanghaibo/roost-core/infra/network/nats/driver"
	gatewiring "github.com/tjbdwanghaibo/roost-core/wiring/gate"
)

type gateHarness struct {
	config                 gateway.Config
	gate                   *gateway.Gate
	game                   *gateway.GameIngress
	server                 *gateway.TCPServer
	gateClient, gameClient *driver.Client
	calls                  atomic.Int64
}

func realGateHarness(t *testing.T, dispatch gateway.TCPDispatch, configure ...func(*gateway.GameDependencies)) *gateHarness {
	t.Helper()
	url := os.Getenv("ROOST_DATAENGINE_IT_NATS_URL")
	if url == "" {
		t.Skip("private NATS URL not configured")
	}
	cfg := gateway.DefaultConfig()
	cfg.Namespace = fmt.Sprintf("roost.gate.test.n%d", time.Now().UnixNano())
	gateID := gateway.ProcessIdentity{ServerID: 11, Incarnation: "gate000000000001"}
	gameID := gateway.ProcessIdentity{ServerID: 22, Incarnation: "game000000000001"}
	client := func(role string, id gateway.ProcessIdentity) *driver.Client {
		prefix, _ := gateway.InboxPrefix(cfg.Namespace, role, id)
		c, err := driver.NewClient(fnats.DefaultConfig(url), driver.ClientOptions{InboxPrefix: prefix, ReconnectBufferBytes: -1})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(c.Close)
		return c
	}
	h := &gateHarness{config: cfg, gateClient: client("gate", gateID), gameClient: client("game", gameID)}
	auth := gateway.AuthenticatorFunc(func(ctx context.Context, ticket string, _ net.Addr) (gateway.Principal, error) {
		if ticket != "ticket" {
			return gateway.Principal{}, gateway.ErrUnauthenticated
		}
		return gateway.Principal{PlayerID: 123, ServerID: 22, SessionID: "session"}, nil
	})
	match := func(ctx context.Context, role string, id gateway.ProcessIdentity) (bool, error) {
		return (role == "gate" && id == gateID) || (role == "game" && id == gameID), nil
	}
	var err error
	deps := gateway.GameDependencies{Client: h.gameClient, Authenticator: auth, QueueFactory: gatewiring.AsyncQueueFactory, Ready: func() bool { return true }, Matches: match, Dispatch: func(ctx context.Context, s gateway.Session, id, seq uint32, k wire.PayloadKind, payload []byte) (any, error) {
		h.calls.Add(1)
		if dispatch != nil {
			return dispatch(ctx, s, id, seq, k, payload)
		}
		return &gateway.TCPResponse{MessageID: id, Sequence: seq, Payload: payload}, nil
	}}
	for _, option := range configure {
		option(&deps)
	}
	h.game, err = gateway.NewGameIngress(cfg, gameID, deps)
	if err != nil {
		t.Fatal(err)
	}
	if err = h.game.Start(); err != nil {
		t.Fatal(err)
	}
	h.gate, err = gateway.NewGate(cfg, gateID, gateway.GateDependencies{Client: h.gateClient, QueueFactory: gatewiring.AsyncQueueFactory, Ready: func() bool { return true }, ResolveGame: func(context.Context, int32) (gateway.ProcessIdentity, error) { return gameID, nil }, Matches: match})
	if err != nil {
		t.Fatal(err)
	}
	if err = h.gate.Start(); err != nil {
		t.Fatal(err)
	}
	tcp := gateway.DefaultTCPConfig()
	tcp.Enabled = true
	tcp.Addr = "127.0.0.1:0"
	tcp.MaxPayloadBytes = uint32(cfg.MaxPayloadBytes)
	h.server, err = gateway.NewTCPServer(tcp, h.gate.Forward, auth)
	if err != nil {
		t.Fatal(err)
	}
	if err = h.server.ConnectHandshake(h.gate); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	for _, c := range []*driver.Client{h.gateClient, h.gameClient} {
		if err = c.FlushContext(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if err = h.server.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := h.gate.Stop(ctx); err != nil {
			t.Error(err)
		}
		if err := h.server.Stop(ctx); err != nil {
			t.Error(err)
		}
		if err := h.game.Stop(ctx); err != nil {
			t.Error(err)
		}
	})
	return h
}
func (h *gateHarness) connect(t *testing.T) net.Conn {
	t.Helper()
	conn, err := net.DialTimeout("tcp", h.server.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	if err = wire.Write(conn, []*wire.Packet{{MsgID: 0, Seq: 1, Payload: []byte("ticket")}}, 8192); err != nil {
		t.Fatal(err)
	}
	ack, err := wire.Read(conn, 8192)
	if err != nil || ack.MsgID != 0 || ack.Seq != 1 {
		t.Fatalf("ACK=%v err=%v", ack, err)
	}
	return conn
}
func TestRealGateTCPForwardAndUnifiedPush(t *testing.T) {
	h := realGateHarness(t, nil)
	conn := h.connect(t)
	if err := wire.Write(conn, []*wire.Packet{{MsgID: 101, Seq: 2, Payload: []byte("formal bytes")}}, 1<<20); err != nil {
		t.Fatal(err)
	}
	response, err := wire.Read(conn, 1<<20)
	if err != nil || response.MsgID != 101 || response.Seq != 2 || string(response.Payload) != "formal bytes" {
		t.Fatalf("response=%v err=%v", response, err)
	}
	binding, receiver, ok := h.game.SessionReceiver("session")
	if !ok || receiver == 0 || binding.BindID == "" {
		t.Fatal("missing game binding")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	for _, push := range []struct {
		id   uint32
		kind wire.PayloadKind
		send func(context.Context, string, uint32, []byte) error
	}{{102, wire.PayloadSync, h.game.PushSyncSession}, {103, wire.PayloadLockstep, h.game.PushLockstepSession}} {
		if err = push.send(ctx, "session", push.id, []byte("complete packet")); err != nil {
			t.Fatal(err)
		}
		packet, err := wire.Read(conn, 1<<20)
		if err != nil || packet.MsgID != push.id || (wire.Header{Flags: packet.Flags}).Kind() != push.kind || packet.Flags&1 == 0 {
			t.Fatalf("push=%v err=%v", packet, err)
		}
	}
	if h.calls.Load() != 1 {
		t.Fatalf("dispatch calls=%d", h.calls.Load())
	}
}
func TestRealGateUnknownNeverReplaysAndDrainWaitsActualDispatch(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var released atomic.Bool
	t.Cleanup(func() {
		if released.CompareAndSwap(false, true) {
			close(release)
		}
	})
	h := realGateHarness(t, func(context.Context, gateway.Session, uint32, uint32, wire.PayloadKind, []byte) (any, error) {
		close(entered)
		<-release
		return nil, nil
	})
	conn := h.connect(t)
	if err := wire.Write(conn, []*wire.Packet{{MsgID: 101, Seq: 2, Payload: []byte("write")}}, 1<<20); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("dispatch never entered")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	err := h.game.Stop(ctx)
	cancel()
	if err == nil {
		t.Fatal("drain invented actual dispatch completion")
	}
	if released.CompareAndSwap(false, true) {
		close(release)
	}
	ctx, cancel = context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err = h.game.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if h.calls.Load() != 1 {
		t.Fatalf("write replayed %d times", h.calls.Load())
	}
}

func TestRealGateBroadcastDedupeAndSequenceGap(t *testing.T) {
	h := realGateHarness(t, nil)
	conn := h.connect(t)
	if err := wire.Write(conn, []*wire.Packet{{MsgID: 101, Seq: 2, Payload: []byte("ready")}}, 1<<20); err != nil {
		t.Fatal(err)
	}
	if _, err := wire.Read(conn, 1<<20); err != nil {
		t.Fatal(err)
	}
	binding, _, ok := h.game.SessionReceiver("session")
	if !ok {
		t.Fatal("missing binding")
	}
	cfg := gateway.DefaultConfig()
	// 从真实主题取得同一隔离命名空间（harness 显式保留配置）。
	cfg = h.config
	sender := gateway.BroadcastSender{Config: cfg, Identity: binding.Game, Role: "game", Client: h.gameClient, Gates: func(context.Context) ([]gateway.ProcessIdentity, error) {
		return []gateway.ProcessIdentity{binding.Gate, binding.Gate}, nil
	}}
	id := gateway.NewBroadcastID()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	for range 2 {
		result, err := sender.Send(ctx, id, gateway.Audience{Kind: gateway.Players, PlayerIDs: []int64{123, 123}}, 201, []byte("announcement"))
		if err != nil || len(result.Gates) != 1 || result.Gates[0].Outcome != gateway.Completed || result.Gates[0].Admission.Accepted != 1 {
			t.Fatalf("broadcast=%+v err=%v", result, err)
		}
	}
	packet, err := wire.Read(conn, 1<<20)
	if err != nil || packet.MsgID != 201 {
		t.Fatalf("broadcast packet=%v err=%v", packet, err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(30 * time.Millisecond))
	if _, err = wire.Read(conn, 1<<20); err == nil {
		t.Fatal("duplicate broadcast delivered twice")
	}
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	result, err := sender.Send(ctx, id, gateway.Audience{Kind: gateway.Players, PlayerIDs: []int64{123}}, 201, []byte("changed"))
	if err != nil || result.Gates[0].Outcome != gateway.NotAdmitted {
		t.Fatalf("ID reuse=%+v err=%v", result, err)
	}
	if _, err = sender.Send(ctx, gateway.NewBroadcastID(), gateway.Audience{Kind: gateway.AllOnline}, 201, nil); err == nil {
		t.Fatal("game acquired system broadcast authority")
	}
	budget, _ := gateway.BudgetFromContext(ctx)
	outbound := gateway.OutboundPacket{Version: gateway.InternalVersion, Binding: binding, OutSeq: 3, Budget: budget, MessageID: 202, Push: true, Payload: []byte("gap")}
	data, err := (bus.MessagePackCodec{}).Marshal(outbound)
	if err != nil {
		t.Fatal(err)
	}
	subject, _ := gateway.ChannelSubject(cfg.Namespace, "gate", binding.Gate, "game", binding.Game, "outbound")
	data, err = h.gameClient.RequestContext(ctx, subject, data)
	if err != nil {
		t.Fatal(err)
	}
	var admission gateway.ExecutionResult
	if err = (bus.MessagePackCodec{}).Unmarshal(data, &admission); err != nil || admission.Outcome != gateway.NotAdmitted {
		t.Fatalf("gap admission=%+v err=%v", admission, err)
	}
	if _, err = wire.Read(conn, 1<<20); err == nil {
		t.Fatal("sequence gap continued socket stream")
	}
}
