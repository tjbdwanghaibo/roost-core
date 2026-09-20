package global

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func expectGuard(t *testing.T, err error, sentinel error, text string) {
	t.Helper()
	if err == nil || !errors.Is(err, sentinel) || !strings.Contains(err.Error(), text) {
		t.Fatalf("error = %v, want %v containing %q", err, sentinel, text)
	}
}

// Migration and lease requests are checked before the compare-and-set runs;
// each refusal is pinned by sentinel and message so a dropped one turns one
// case red. A migration "to where it already is" is the one that would leave
// a binding permanently in Migrating with no move to complete.
func TestMigrationAndLeaseRequestsRefuseEachInvalidShape(t *testing.T) {
	service, _ := newService(t)
	ctx := context.Background()
	binding := bind(t, service, 100, 1)

	_, err := service.BeginMigration(ctx, 100, 0, binding.Epoch)
	expectGuard(t, err, ErrRouteInvalid, "target global sid must be positive")
	_, err = service.BeginMigration(ctx, 100, 1, binding.Epoch)
	expectGuard(t, err, ErrRouteInvalid, "game 100 is already served by 1")
	_, err = service.BeginMigration(ctx, 404, 2, 1)
	expectGuard(t, err, ErrRouteMissing, "game 404")

	resolved, err := service.Resolve(ctx, 100)
	if err != nil || resolved.State != RouteActive || resolved.Epoch != binding.Epoch {
		t.Fatalf("refused migrations must leave the binding untouched: %+v, %v", resolved, err)
	}

	expectGuard(t, service.ReleaseLease(ctx, 100, ""), ErrLeaseInvalid, "incarnation is required")
}

func TestLiveGamesRefusesEachOutOfRangeRequest(t *testing.T) {
	service, _ := newService(t)
	ctx := context.Background()
	_, err := service.LiveGames(ctx, "group-a", []int32{1}, 0)
	expectGuard(t, err, ErrRangeInvalid, "limit must be positive, got 0")
	_, err = service.LiveGames(ctx, "group-a", []int32{1}, MaxPageSize+1)
	expectGuard(t, err, ErrRangeInvalid, "exceeds")
	_, err = service.LiveGames(ctx, "group-a", make([]int32, MaxPageSize+1), 10)
	expectGuard(t, err, ErrRangeInvalid, "candidates exceeds")
	if _, err := service.LiveGames(ctx, "group-a", nil, MaxPageSize); err != nil {
		t.Fatalf("the maximum page is legal: %v", err)
	}
}
