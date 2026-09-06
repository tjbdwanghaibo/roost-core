package ownerroute

import (
	"context"
	"errors"
	"testing"
)

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
