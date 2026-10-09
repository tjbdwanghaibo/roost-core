//go:build integration

package gate_test

import (
	"context"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/client/wire"
	"github.com/tjbdwanghaibo/roost-core/infra/network/gateway"
	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
	"github.com/tjbdwanghaibo/roost-core/infra/network/nats/driver"
	gatewiring "github.com/tjbdwanghaibo/roost-core/wiring/gate"
)

// 四个运行实例使用四个真实连接；这是本机集群，不冒充独立机器/进程故障。
func TestRealTwoGatesTwoGamesRouteBroadcastAndFence(t *testing.T) {
	url := os.Getenv("ROOST_DATAENGINE_IT_NATS_URL")
	if url == "" {
		t.Skip("private NATS URL not configured")
	}
	config := gateway.DefaultConfig()
	config.Namespace = fmt.Sprintf("roost.cluster.n%d", time.Now().UnixNano())
	config.RequestTimeout = 200 * time.Millisecond
	config.Lease = 800 * time.Millisecond
	config.RenewInterval = 200 * time.Millisecond
	config.ClockSkew = 0
	config.TombstoneTTL = 3 * time.Second
	gateIDs := []gateway.ProcessIdentity{{ServerID: 11, Incarnation: "gate000000000001"}, {ServerID: 12, Incarnation: "gate000000000002"}}
	gameIDs := []gateway.ProcessIdentity{{ServerID: 22, Incarnation: "game000000000001"}, {ServerID: 23, Incarnation: "game000000000002"}}
	systemID := gateway.ProcessIdentity{ServerID: 99, Incarnation: "system0000000001"}
	var authority sync.Map
	for _, id := range gateIDs {
		authority.Store("gate"+strconv.Itoa(int(id.ServerID)), id)
	}
	for _, id := range gameIDs {
		authority.Store("game"+strconv.Itoa(int(id.ServerID)), id)
	}
	authority.Store("system99", systemID)
	matches := func(_ context.Context, role string, id gateway.ProcessIdentity) (bool, error) {
		current, ok := authority.Load(role + strconv.Itoa(int(id.ServerID)))
		return ok && current == id, nil
	}
	raw := func(role string, id gateway.ProcessIdentity) fnats.RawClient {
		client, err := driver.NewClient(fnats.DefaultConfig(url), driver.ClientOptions{ReconnectBufferBytes: -1})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(client.Close)
		prefix, _ := gateway.InboxPrefix(config.Namespace, role, id)
		scoped, err := client.ForInbox(prefix)
		if err != nil {
			t.Fatal(err)
		}
		return scoped
	}
	auth := gateway.AuthenticatorFunc(func(_ context.Context, ticket string, _ net.Addr) (gateway.Principal, error) {
		parts := strings.Split(ticket, ":")
		if len(parts) != 3 {
			return gateway.Principal{}, gateway.ErrUnauthenticated
		}
		player, err := strconv.ParseInt(parts[0], 10, 64)
		if err != nil {
			return gateway.Principal{}, err
		}
		sid, err := strconv.ParseInt(parts[1], 10, 32)
		return gateway.Principal{PlayerID: player, ServerID: int32(sid), SessionID: parts[2]}, err
	})
	games := make([]*gateway.GameIngress, 2)
	gameRaw := make([]fnats.RawClient, 2)
	for index, id := range gameIDs {
		gameRaw[index] = raw("game", id)
		var err error
		games[index], err = gateway.NewGameIngress(config, id, gateway.GameDependencies{Client: gameRaw[index], Authenticator: auth, QueueFactory: gatewiring.AsyncQueueFactory, Ready: func() bool { return true }, Matches: matches, Dispatch: func(_ context.Context, _ gateway.Session, id, seq uint32, kind wire.PayloadKind, payload []byte) (any, error) {
			return &gateway.TCPResponse{MessageID: id, Sequence: seq, Payload: payload}, nil
		}})
		if err != nil {
			t.Fatal(err)
		}
		if err = games[index].Start(); err != nil {
			t.Fatal(err)
		}
	}
	gates := make([]*gateway.Gate, 2)
	servers := make([]*gateway.TCPServer, 2)
	for index, id := range gateIDs {
		client := raw("gate", id)
		var err error
		gates[index], err = gateway.NewGate(config, id, gateway.GateDependencies{Client: client, QueueFactory: gatewiring.AsyncQueueFactory, Ready: func() bool { return true }, Matches: matches, ResolveGame: func(_ context.Context, sid int32) (gateway.ProcessIdentity, error) {
			for _, candidate := range gameIDs {
				if candidate.ServerID == sid {
					return candidate, nil
				}
			}
			return gateway.ProcessIdentity{}, gateway.ErrBindingStale
		}})
		if err != nil {
			t.Fatal(err)
		}
		if err = gates[index].Start(); err != nil {
			t.Fatal(err)
		}
		tcp := gateway.DefaultTCPConfig()
		tcp.Addr = "127.0.0.1:0"
		tcp.MaxPayloadBytes = uint32(config.MaxPayloadBytes)
		servers[index], err = gateway.NewTCPServer(tcp, gates[index].Forward, auth)
		if err != nil {
			t.Fatal(err)
		}
		if err = servers[index].ConnectHandshake(gates[index]); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		if err = client.FlushContext(ctx); err != nil {
			cancel()
			t.Fatal(err)
		}
		cancel()
		if err = servers[index].Start(); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		for i := range gates {
			if err := gates[i].Stop(ctx); err != nil {
				t.Error(err)
			}
			if err := servers[i].Stop(ctx); err != nil {
				t.Error(err)
			}
		}
		for _, game := range games {
			if err := game.Stop(ctx); err != nil {
				t.Error(err)
			}
		}
	})
	connect := func(gateIndex int, ticket string) net.Conn {
		t.Helper()
		conn, err := net.DialTimeout("tcp", servers[gateIndex].Addr().String(), time.Second)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = conn.Close() })
		_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
		if err := wire.Write(conn, []*wire.Packet{{Seq: 1, Payload: []byte(ticket)}}, 8192); err != nil {
			t.Fatal(err)
		}
		if _, err := wire.Read(conn, 8192); err != nil {
			t.Fatal(err)
		}
		if err := wire.Write(conn, []*wire.Packet{{MsgID: 101, Seq: 2, Payload: []byte(ticket)}}, config.MaxPayloadBytes); err != nil {
			t.Fatal(err)
		}
		response, err := wire.Read(conn, config.MaxPayloadBytes)
		if err != nil || string(response.Payload) != ticket {
			t.Fatalf("route response=%v err=%v", response, err)
		}
		return conn
	}
	a, b, c := connect(0, "111:22:a"), connect(1, "222:22:b"), connect(1, "333:23:c")
	sender := gateway.BroadcastSender{Config: config, Identity: gameIDs[0], Role: "game", Client: gameRaw[0], Gates: func(context.Context) ([]gateway.ProcessIdentity, error) { return gateIDs, nil }}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	result, err := sender.Send(ctx, gateway.NewBroadcastID(), gateway.Audience{Kind: gateway.GameOnline, GameID: 22}, 201, []byte("game22"))
	if err != nil || len(result.Gates) != 2 {
		t.Fatalf("broadcast=%+v %v", result, err)
	}
	for _, item := range result.Gates {
		if item.Outcome != gateway.Completed || item.Admission.Accepted != 1 {
			t.Fatalf("gate result=%+v", item)
		}
	}
	for _, conn := range []net.Conn{a, b} {
		packet, err := wire.Read(conn, config.MaxPayloadBytes)
		if err != nil || string(packet.Payload) != "game22" {
			t.Fatalf("game broadcast=%+v %v", packet, err)
		}
	}
	_ = c.SetReadDeadline(time.Now().Add(30 * time.Millisecond))
	if packet, err := wire.Read(c, config.MaxPayloadBytes); err == nil {
		t.Fatalf("broadcast crossed fixed Game SID: %+v", packet)
	}
	_ = c.SetDeadline(time.Now().Add(3 * time.Second))
	systemRaw := raw("system", systemID)
	system := gateway.BroadcastSender{Config: config, Identity: systemID, Role: "system", Client: systemRaw, Gates: sender.Gates}
	if _, err := system.Send(ctx, gateway.NewBroadcastID(), gateway.Audience{Kind: gateway.AllOnline}, 202, []byte("system")); err != nil {
		t.Fatal(err)
	}
	for _, conn := range []net.Conn{a, b, c} {
		packet, err := wire.Read(conn, config.MaxPayloadBytes)
		if err != nil || string(packet.Payload) != "system" {
			t.Fatalf("system broadcast=%+v %v", packet, err)
		}
	}
	// 模拟当前 Game 持锁代次改变。旧 Gate/Game 的续期拒绝，SID23 继续服务。
	authority.Store("game22", gateway.ProcessIdentity{ServerID: 22, Incarnation: "game000000000003"})
	_ = a.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := wire.Read(a, config.MaxPayloadBytes); err == nil {
		t.Fatal("old game binding survived authority fence")
	}
	if err := wire.Write(c, []*wire.Packet{{MsgID: 101, Seq: 3, Payload: []byte("unaffected game23")}}, config.MaxPayloadBytes); err != nil {
		t.Fatal(err)
	}
	response, err := wire.Read(c, config.MaxPayloadBytes)
	if err != nil || string(response.Payload) != "unaffected game23" {
		t.Fatalf("other Game stalled: %+v %v", response, err)
	}
}
