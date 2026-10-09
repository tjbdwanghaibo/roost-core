package ownerroute

import (
	"context"
	"strings"
	"testing"
)

// U-0149 · C2 · gap map core `ownerroute` 6/9：nil 总线传输不能发送；总线处理器注册缺任一参数拒绝；
// nil 路由器、缺路由解析器 / 本地执行器 / 远端传输的路由器各报其错。
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
