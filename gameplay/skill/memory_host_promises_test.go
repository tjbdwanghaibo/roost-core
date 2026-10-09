package skill

import (
	"errors"
	"strings"
	"testing"
)

// The memory host is the reference host every skill test runs against; its
// refusals are the contract real hosts are compared to. Time only moves
// forward, costs are non-negative and name a known resource, and an entity
// pays only what it has — atomically.
func TestMemoryHostRefusesBackwardTicksAndIllegalPayments(t *testing.T) {
	host := NewMemoryHost(AuthorityIdentity{Revision: "test", Digest: "test"})
	if _, err := host.Advance(10); err != nil {
		t.Fatal(err)
	}
	revision := host.CurrentRevision()
	if got, err := host.Advance(10); err != nil || got != revision {
		t.Fatalf("same tick must be a no-op: revision=%v err=%v", got, err)
	}
	if _, err := host.Advance(9); err == nil || !strings.Contains(err.Error(), "tick moved backwards") {
		t.Fatalf("Advance backwards = %v", err)
	}

	host.UpsertEntity(MemoryEntity{ID: 1, Alive: true, Health: 100, MaxHealth: 100, Resources: map[string]int64{"mana": 50}})
	if _, err := host.PayCosts(CostPayment{Entity: 99, Entries: []CostEntry{{Resource: "mana", Amount: 1}}}); !errors.Is(err, ErrEntityNotFound) {
		t.Fatalf("unknown entity = %v", err)
	}
	if _, err := host.PayCosts(CostPayment{Entity: 1, Entries: []CostEntry{{Resource: "mana", Amount: -1}}}); err == nil || !strings.Contains(err.Error(), "negative cost") {
		t.Fatalf("negative cost = %v", err)
	}
	if _, err := host.PayCosts(CostPayment{Entity: 1, Entries: []CostEntry{{Handle: 77, Amount: 1}}}); !errors.Is(err, ErrCombatHandleInvalid) {
		t.Fatalf("unmapped handle and no resource name = %v", err)
	}
	if _, err := host.PayCosts(CostPayment{Entity: 1, Entries: []CostEntry{{Resource: "mana", Amount: 30}, {Resource: "mana", Amount: 30}}}); !errors.Is(err, ErrInsufficientResource) {
		t.Fatalf("total over the pool = %v", err)
	}
	if got := host.ResourceForTest(1, "mana"); got != 50 {
		t.Fatalf("a refused payment deducted: mana=%d", got)
	}
	if _, err := host.PayCosts(CostPayment{Entity: 1, Entries: []CostEntry{{Resource: "mana", Amount: 20}}}); err != nil {
		t.Fatal(err)
	}
	if got := host.ResourceForTest(1, "mana"); got != 30 {
		t.Fatalf("legal payment left mana=%d, want 30", got)
	}
}
