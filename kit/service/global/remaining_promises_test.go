package global

import (
	"context"
	"errors"
	"testing"
)

func TestCompleteMigrationReplaysTheOriginalEpoch(t *testing.T) {
	s, _ := newService(t)
	ctx := context.Background()
	initial := bind(t, s, 1, 10)
	moving, err := s.BeginMigration(ctx, 1, 20, initial.Epoch)
	if err != nil {
		t.Fatal(err)
	}
	first, err := s.CompleteMigration(ctx, 1, moving.Epoch)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := s.CompleteMigration(ctx, 1, moving.Epoch)
	if err != nil || replay != first {
		t.Fatalf("original completion retry = %+v %v, first=%+v", replay, err, first)
	}
	second, err := s.BeginMigration(ctx, 1, 30, first.Epoch)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.CompleteMigration(ctx, 1, moving.Epoch); !errors.Is(err, ErrRouteStale) {
		t.Fatalf("old completion crossed new migration: %v", err)
	}
	if _, err = s.AbortMigration(ctx, 1, second.Epoch); err != nil {
		t.Fatal(err)
	}
	if _, err = s.CompleteMigration(ctx, 1, second.Epoch); !errors.Is(err, ErrRouteStale) {
		t.Fatalf("aborted migration reported completed: %v", err)
	}
}

func TestRouteCodecRequiresCompletionReceiptFormat(t *testing.T) {
	codec := routeCodec{}
	if _, err := codec.Decode([]byte(`{"GameSID":1,"Epoch":2}`)); err == nil {
		t.Fatal("legacy route accepted")
	}
	want := RouteBinding{Epoch: 3, CompletedFromEpoch: 2}
	raw, err := codec.Encode(want)
	if err != nil {
		t.Fatal(err)
	}
	got, err := codec.Decode(raw)
	if err != nil || got != want {
		t.Fatalf("round trip: %+v %v", got, err)
	}
}
