package session

import domain "github.com/tjbdwanghaibo/roost-core/service/session"

import (
	"context"
	"testing"
)

func TestWithSweepOwnersCarriesDeploymentRoster(t *testing.T) {
	calls := 0
	mod := NewMod(nil, nil, WithSweepOwners(domain.OwnerSourceFunc(func(context.Context, int) ([]int64, error) { calls++; return []int64{42}, nil })))
	if mod.owners == nil {
		t.Fatal("constructor discarded owner source")
	}
	owners, err := mod.owners.SweepOwners(context.Background(), 1)
	if err != nil || calls != 1 || len(owners) != 1 || owners[0] != 42 {
		t.Fatal(owners, calls, err)
	}
}
