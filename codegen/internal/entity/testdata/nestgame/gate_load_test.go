package nestgame

import (
	"context"
	"fmt"
	"net"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/client/wire"
	"github.com/tjbdwanghaibo/roost-core/framework/nest"
	"github.com/tjbdwanghaibo/roost-core/infra/network/gateway"
	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
	"github.com/tjbdwanghaibo/roost-core/infra/network/nats/driver"
	gatewiring "github.com/tjbdwanghaibo/roost-core/wiring/gate"
)

// 可选正式接入负载：普通业务改走 TCP → Gate → Core NATS → Game Ingress → Nest，HB 保持原 ticker。
// Entity/DAO 由同一生成模块提供；SYNC=1 叠加正式 Interest/Sync。Saga/Remote 仍是独立专项。
type gateFrontend struct {
	loadClients
	server  *gateway.TCPServer
	gate    *gateway.Gate
	ingress *gateway.GameIngress
	nats    []*driver.Client
}

func newGateFrontend(t *testing.T, engine *nest.NestMgr, name nest.HandlerName, units []*Unit, c gameConfig, stateSync *syncGameLoad) *gateFrontend {
	t.Helper()
	url := os.Getenv("ROOST_DATAENGINE_IT_NATS_URL")
	if url == "" {
		t.Fatal("Gate load requires private ROOST_DATAENGINE_IT_NATS_URL")
	}
	f := &gateFrontend{loadClients: loadClients{stream: "gate"}}
	config := gateway.DefaultConfig()
	config.Namespace = fmt.Sprintf("roost.gate.load.n%d", time.Now().UnixNano())
	gateID, gameID := gateway.ProcessIdentity{ServerID: 11, Incarnation: "gate000000000001"}, gateway.ProcessIdentity{ServerID: 22, Incarnation: "game000000000001"}
	raw := func(role string, id gateway.ProcessIdentity) fnats.RawClient {
		client, err := driver.NewClient(fnats.DefaultConfig(url), driver.ClientOptions{ReconnectBufferBytes: -1})
		if err != nil {
			t.Fatal(err)
		}
		f.nats = append(f.nats, client)
		prefix, _ := gateway.InboxPrefix(config.Namespace, role, id)
		scoped, err := client.ForInbox(prefix)
		if err != nil {
			t.Fatal(err)
		}
		return scoped
	}
	gameRaw, gateRaw := raw("game", gameID), raw("gate", gateID)
	auth := loadAuthenticator(c.Players)
	matches := func(_ context.Context, role string, id gateway.ProcessIdentity) (bool, error) {
		return (role == "game" && id == gameID) || (role == "gate" && id == gateID), nil
	}
	var err error
	deps := gateway.GameDependencies{Client: gameRaw, Authenticator: auth, QueueFactory: gatewiring.AsyncQueueFactory, Ready: func() bool { return true }, Matches: matches, Dispatch: func(ctx context.Context, session gateway.Session, id, seq uint32, kind wire.PayloadKind, payload []byte) (any, error) {
		in, err := decodeLoadRequest(id, kind, payload)
		if err != nil {
			return nil, err
		}
		index := int(session.Principal().PlayerID) - 1
		if _, err = engine.Request(ctx, name, units[index].ID(), nest.NewParams(in)); err != nil {
			return nil, err
		}
		return &gateway.TCPResponse{MessageID: 2, Sequence: seq}, nil
	}}
	if stateSync != nil {
		deps.OnActive = stateSync.active
		deps.OnClosed = stateSync.closed
	}
	f.ingress, err = gateway.NewGameIngress(config, gameID, deps)
	if stateSync != nil {
		stateSync.gateTransport.Game = f.ingress
	}
	if err != nil {
		t.Fatal(err)
	}
	if err = f.ingress.Start(); err != nil {
		t.Fatal(err)
	}
	f.gate, err = gateway.NewGate(config, gateID, gateway.GateDependencies{Client: gateRaw, QueueFactory: gatewiring.AsyncQueueFactory, Ready: func() bool { return true }, Matches: matches, ResolveGame: func(context.Context, int32) (gateway.ProcessIdentity, error) { return gameID, nil }})
	if err != nil {
		t.Fatal(err)
	}
	if err = f.gate.Start(); err != nil {
		t.Fatal(err)
	}
	tcp := gateway.DefaultTCPConfig()
	tcp.Addr = "127.0.0.1:0"
	tcp.MaxConnectionsPerIP = c.Players
	tcp.MaxPayloadBytes = uint32(config.MaxPayloadBytes)
	tcp.RequestRate = 0
	f.server, err = gateway.NewTCPServer(tcp, f.gate.Forward, auth)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.server.ConnectHandshake(f.gate); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, client := range []fnats.RawClient{gameRaw, gateRaw} {
		if err = client.FlushContext(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if err = f.server.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		f.stopping.Store(true)
		_ = f.gate.Stop(ctx)
		_ = f.server.Stop(ctx)
		_ = f.ingress.Stop(ctx)
		f.close()
		for _, client := range f.nats {
			client.Close()
		}
	})
	var onSync func(int, []byte) error
	if stateSync != nil {
		onSync = stateSync.apply
	}
	f.connect(t, f.server.Addr().String(), c.Players, config.MaxPayloadBytes, onSync)
	return f
}

// loadAuthenticator 把票据（玩家序号 1..players）认成玩家；独立 Gate 与嵌入 TCP 共用。
func loadAuthenticator(players int) gateway.AuthenticatorFunc {
	return func(_ context.Context, ticket string, _ net.Addr) (gateway.Principal, error) {
		id, err := strconv.Atoi(ticket)
		if err != nil || id < 1 || id > players {
			return gateway.Principal{}, gateway.ErrUnauthenticated
		}
		return gateway.Principal{PlayerID: int64(id), SessionID: ticket, ServerID: 22}, nil
	}
}
