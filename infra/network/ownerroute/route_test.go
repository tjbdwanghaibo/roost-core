package ownerroute

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type testRoute struct {
	ownerSid int32
}

func (r testRoute) OwnerRouteSid() int32 { return r.ownerSid }

type testResolver map[int64]testRoute

func (r testResolver) GetRoute(_ context.Context, key int64) (testRoute, bool, error) {
	route, ok := r[key]
	return route, ok, nil
}

type testCommand struct {
	PlayerID  int64
	SourceSid int32
}

type testTransport struct {
	sid int32
	cmd *testCommand
}

func (t *testTransport) Send(_ context.Context, sid int32, cmd *testCommand) error {
	t.sid = sid
	t.cmd = cmd
	return nil
}

func TestRouterExecutesLocalCommand(t *testing.T) {
	called := false
	router := &Router[testCommand, int64, testRoute]{
		LocalSid: 1,
		Routes:   testResolver{7: {ownerSid: 1}},
		KeyOf:    func(cmd *testCommand) int64 { return cmd.PlayerID },
		Prepare:  func(cmd *testCommand, sid int32) { cmd.SourceSid = sid },
		Executor: func(_ context.Context, cmd *testCommand) error {
			called = cmd.SourceSid == 1
			return nil
		},
	}
	if err := router.Route(context.Background(), &testCommand{PlayerID: 7}); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("expected local executor")
	}
}

func TestRouterSendsRemoteCommand(t *testing.T) {
	transport := &testTransport{}
	router := &Router[testCommand, int64, testRoute]{
		LocalSid:  1,
		Routes:    testResolver{7: {ownerSid: 2}},
		Transport: transport,
		KeyOf:     func(cmd *testCommand) int64 { return cmd.PlayerID },
	}
	if err := router.Route(context.Background(), &testCommand{PlayerID: 7}); err != nil {
		t.Fatal(err)
	}
	if transport.sid != 2 || transport.cmd == nil || transport.cmd.PlayerID != 7 {
		t.Fatalf("transport = %+v", transport)
	}
}

// Routing decides which process mutates an entity; the guards in front of it
// are what keep a command with a bad key, or one whose route has no owner,
// from being executed locally by default.
func TestRouterRefusesInvalidKeysAndOwnerlessRoutes(t *testing.T) {
	invalid := errors.New("invalid")
	notFound := errors.New("not found")
	executed := false
	router := &Router[testCommand, int64, testRoute]{
		LocalSid:          1,
		Routes:            testResolver{7: {ownerSid: 1}, 9: {ownerSid: 0}},
		KeyOf:             func(cmd *testCommand) int64 { return cmd.PlayerID },
		ValidKey:          func(key int64) bool { return key > 0 },
		Executor:          func(context.Context, *testCommand) error { executed = true; return nil },
		ErrInvalidCommand: invalid,
		ErrRouteNotFound:  notFound,
	}
	ctx := context.Background()
	if err := router.Route(ctx, &testCommand{PlayerID: 0}); !errors.Is(err, invalid) {
		t.Fatalf("invalid key = %v", err)
	}
	if err := router.Route(ctx, nil); !errors.Is(err, invalid) {
		t.Fatalf("nil command = %v", err)
	}
	if err := router.Route(ctx, &testCommand{PlayerID: 9}); !errors.Is(err, notFound) {
		t.Fatalf("route with owner sid 0 = %v", err)
	}
	if err := router.Route(ctx, &testCommand{PlayerID: 8}); !errors.Is(err, notFound) {
		t.Fatalf("unknown route = %v", err)
	}
	if executed {
		t.Fatal("a refused command must not reach the local executor")
	}
	if err := router.Route(ctx, &testCommand{PlayerID: 7}); err != nil || !executed {
		t.Fatalf("legal local route = %v executed=%v", err, executed)
	}
}

// nil 总线传输不能发送；总线处理器注册缺任一参数拒绝；nil 路由器、缺路由解析器 / 本地执行器 / 远端传输的路由器各报其错。
func TestTransportAndRouterRefuseMissingParts(t *testing.T) {
	ctx := context.Background()
	var noTransport *BusTransport[testCommand]
	if err := noTransport.Send(ctx, 2, &testCommand{}); err == nil || !strings.Contains(err.Error(), "bus transport is nil") {
		t.Fatalf("Send on a nil transport = %v", err)
	}
	if err := (&BusTransport[testCommand]{}).Send(ctx, 2, &testCommand{}); err == nil || !strings.Contains(err.Error(), "bus transport is nil") {
		t.Fatalf("Send without a bus = %v", err)
	}
	execute := func(context.Context, *testCommand) error { return nil }
	for name, call := range map[string]func() error{
		"nil bus":      func() error { return RegisterBusHandler[testCommand](nil, "m", "n", execute) },
		"empty module": func() error { return RegisterBusHandler[testCommand](nil, "", "n", execute) },
	} {
		if err := call(); err == nil || !strings.Contains(err.Error(), "invalid bus handler registration") {
			t.Fatalf("RegisterBusHandler(%s) = %v", name, err)
		}
	}
	var none *Router[testCommand, int64, testRoute]
	if err := none.Route(ctx, &testCommand{PlayerID: 7}); err == nil || !strings.Contains(err.Error(), "router is nil") {
		t.Fatalf("Route on a nil router = %v", err)
	}
	keyOf := func(cmd *testCommand) int64 { return cmd.PlayerID }
	if err := (&Router[testCommand, int64, testRoute]{LocalSid: 1, KeyOf: keyOf}).Route(ctx, &testCommand{PlayerID: 7}); err == nil || !strings.Contains(err.Error(), "route resolver is nil") {
		t.Fatalf("Route without a resolver = %v", err)
	}
	routes := testResolver{7: {ownerSid: 1}, 8: {ownerSid: 2}}
	if err := (&Router[testCommand, int64, testRoute]{LocalSid: 1, KeyOf: keyOf, Routes: routes}).Route(ctx, &testCommand{PlayerID: 7}); err == nil || !strings.Contains(err.Error(), "local executor is nil") {
		t.Fatalf("Route to a local owner without an executor = %v", err)
	}
	if err := (&Router[testCommand, int64, testRoute]{LocalSid: 1, KeyOf: keyOf, Routes: routes}).Route(ctx, &testCommand{PlayerID: 8}); err == nil || !strings.Contains(err.Error(), "transport is nil") {
		t.Fatalf("Route to a remote owner without a transport = %v", err)
	}
}
