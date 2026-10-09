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

// 同批控制请求可以有限等待；超出总额在鉴权前拒绝，排队仍消耗原始预算。
func TestRealControlQueueBoundedAndQueuedBudgetExpires(t *testing.T) {
	url := os.Getenv("ROOST_DATAENGINE_IT_NATS_URL")
	if url == "" {
		t.Skip("private NATS URL not configured")
	}
	config := gateway.DefaultConfig()
	config.Namespace = fmt.Sprintf("roost.control.n%d", time.Now().UnixNano())
	config.ControlWorkers = 1
	config.MaxControlRequests = 2
	gateID := gateway.ProcessIdentity{ServerID: 11, Incarnation: "gate000000000001"}
	gameID := gateway.ProcessIdentity{ServerID: 22, Incarnation: "game000000000001"}
	connect := func(role string, id gateway.ProcessIdentity) fnats.RawClient {
		prefix, _ := gateway.InboxPrefix(config.Namespace, role, id)
		client, err := driver.NewClient(fnats.DefaultConfig(url), driver.ClientOptions{InboxPrefix: prefix, ReconnectBufferBytes: -1})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(client.Close)
		raw, err := client.ForInbox(prefix)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	gateRaw, gameRaw := connect("gate", gateID), connect("game", gameID)
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	var authCalls atomic.Int64
	game, err := gateway.NewGameIngress(config, gameID, gateway.GameDependencies{Client: gameRaw, QueueFactory: gatewiring.AsyncQueueFactory, Ready: func() bool { return true }, Matches: func(context.Context, string, gateway.ProcessIdentity) (bool, error) { return true, nil }, Dispatch: func(context.Context, gateway.Session, uint32, uint32, wire.PayloadKind, []byte) (any, error) {
		return nil, nil
	}, Authenticator: gateway.AuthenticatorFunc(func(ctx context.Context, _ string, _ net.Addr) (gateway.Principal, error) {
		authCalls.Add(1)
		entered <- struct{}{}
		select {
		case <-release:
		case <-ctx.Done():
			return gateway.Principal{}, ctx.Err()
		}
		return gateway.Principal{PlayerID: 1, ServerID: 22, SessionID: "session"}, nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	if err = game.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := game.Stop(ctx); err != nil {
			t.Error(err)
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err = gameRaw.FlushContext(ctx); err != nil {
		t.Fatal(err)
	}
	subject, _ := gateway.ChannelSubject(config.Namespace, "game", gameID, "gate", gateID, "control")
	codec := bus.MessagePackCodec{}
	request := func(index int, budget time.Duration) gateway.ControlResponse {
		deadline, cancel := context.WithTimeout(context.Background(), budget)
		b, _ := gateway.BudgetFromContext(deadline)
		cancel()
		binding := gateway.Binding{PlayerID: 1, SessionID: fmt.Sprintf("session%d", index), Gate: gateID, Game: gameID, ConnectionNonce: fmt.Sprintf("%032d", index)}
		data, _ := codec.Marshal(gateway.ControlRequest{Version: gateway.InternalVersion, Operation: gateway.Bind, Binding: binding, Ticket: "ticket", Budget: b})
		response, err := gateRaw.RequestContext(ctx, subject, data)
		if err != nil {
			t.Error(err)
			return gateway.ControlResponse{}
		}
		var result gateway.ControlResponse
		if err := codec.Unmarshal(response, &result); err != nil {
			t.Error(err)
		}
		return result
	}
	first := make(chan gateway.ControlResponse, 1)
	go func() { first <- request(1, time.Second) }()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("first request did not enter auth")
	}
	queued := make(chan gateway.ControlResponse, 1)
	go func() { queued <- request(2, 50*time.Millisecond) }()
	// 第一请求占住唯一 worker；第二请求在有限队列中到期，不能进入鉴权。
	time.Sleep(100 * time.Millisecond)
	if result := request(3, time.Second); result.Outcome != gateway.NotAdmitted || result.Reason != "control capacity" {
		t.Fatalf("overflow=%+v", result)
	}
	close(release)
	if result := <-first; result.Outcome != gateway.Completed {
		t.Fatalf("first=%+v", result)
	}
	if result := <-queued; result.Outcome != gateway.NotAdmitted {
		t.Fatalf("expired queued=%+v", result)
	}
	if authCalls.Load() != 1 {
		t.Fatalf("expired/overflow controls entered auth: %d", authCalls.Load())
	}
}
