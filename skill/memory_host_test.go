package skill

import (
	"errors"
	"reflect"
	"testing"
)

func TestHostCommandPayloadsAreNarrowAndTyped(t *testing.T) {
	var _ EffectCommandPayload = TeleportCommand{}
	var _ EffectCommandPayload = ResourceCommand{}
	if reflect.TypeOf(TeleportCommand{}).NumField() > 3 || reflect.TypeOf(ResourceCommand{}).NumField() > 4 {
		t.Fatal("typed command payload became a wide union")
	}
}

func TestMemoryHostPayCostsIsAtomic(t *testing.T) {
	host := NewMemoryHost(AuthorityIdentity{Revision: "test", Digest: "test"})
	host.UpsertEntity(MemoryEntity{ID: 1, Alive: true, Resources: map[string]int64{"mana": 10, "rage": 0}})
	beforeRevision := host.CurrentRevision()
	_, err := host.PayCosts(CostPayment{Entity: 1, Entries: []CostEntry{{Resource: "mana", Amount: 10}, {Resource: "rage", Amount: 1}}})
	if !errors.Is(err, ErrInsufficientResource) {
		t.Fatalf("got %v", err)
	}
	if got := host.ResourceForTest(1, "mana"); got != 10 {
		t.Fatalf("mana changed after failed atomic payment: %d", got)
	}
	if host.CurrentRevision() != beforeRevision {
		t.Fatal("failed cost payment advanced revision")
	}
}

func TestMemoryHostSelectIsDeterministic(t *testing.T) {
	host := NewMemoryHost(AuthorityIdentity{Revision: "test", Digest: "test"})
	for _, id := range []EntityID{9, 2, 5} {
		host.UpsertEntity(MemoryEntity{ID: id, Alive: true, Position: Position{X: 10, Y: 0}})
	}
	request := SelectRequest{Shape: CircleSelectShape{Center: Position{}, Radius: 20}, Filters: []SelectFilter{AliveSelectFilter{}}, Order: SelectOrder{By: SelectOrderDistance, Direction: SelectAscending}, Limit: 3}
	first, err := host.Select(request)
	if err != nil {
		t.Fatal(err)
	}
	second, err := host.Select(request)
	if err != nil {
		t.Fatal(err)
	}
	want := []EntityID{2, 5, 9}
	if !reflect.DeepEqual(first.Selection.EntityIDs(), want) || !reflect.DeepEqual(second.Selection.EntityIDs(), want) {
		t.Fatalf("unstable selection: %v %v", first.Selection.EntityIDs(), second.Selection.EntityIDs())
	}
	mutated := first.Selection.EntityIDs()
	mutated[0] = 99
	if reflect.DeepEqual(mutated, first.Selection.EntityIDs()) {
		t.Fatal("selection exposed mutable element storage")
	}
}

func TestMemoryHostRaycastUsesHitDistanceThenStableCollider(t *testing.T) {
	host := NewMemoryHost(AuthorityIdentity{Revision: "test", Digest: "test"})
	host.UpsertEntity(MemoryEntity{ID: 9, Alive: true, Position: Position{X: 8}})
	host.UpsertEntity(MemoryEntity{ID: 5, Alive: true, Position: Position{X: 2}})
	host.UpsertEntity(MemoryEntity{ID: 2, Alive: true, Position: Position{X: 2}})
	result, err := host.Select(SelectRequest{Shape: RaycastSelectShape{Origin: Position{}, Direction: Direction{X: 1}, Length: 10}, Order: SelectOrder{By: SelectOrderEntityID, Direction: SelectDescending}, Limit: 3})
	if err != nil {
		t.Fatal(err)
	}
	if want := []EntityID{2, 5, 9}; !reflect.DeepEqual(result.Selection.EntityIDs(), want) {
		t.Fatalf("raycast order = %v, want %v", result.Selection.EntityIDs(), want)
	}
}

func TestMemoryHostStopProcessIsIdempotent(t *testing.T) {
	host := NewMemoryHost(AuthorityIdentity{Revision: "test", Digest: "test"})
	_, err := host.StepProcess(ProcessStepCommand{Meta: ProcessCommandMeta{ProcessID: 7}, Motion: StaticMotionStep{}}, ProcessHostState{ProcessID: 7})
	if err != nil {
		t.Fatal(err)
	}
	before := len(host.Events(0))
	first, err := host.StopProcess(ProcessStopCommand{Meta: ProcessCommandMeta{ProcessID: 7}}, ProcessHostState{ProcessID: 7})
	if err != nil || !first.Changed {
		t.Fatalf("first stop = %#v, %v", first, err)
	}
	second, err := host.StopProcess(ProcessStopCommand{Meta: ProcessCommandMeta{ProcessID: 7}}, ProcessHostState{ProcessID: 7})
	if err != nil || second.Changed || second.Revision != first.Revision {
		t.Fatalf("second stop = %#v, %v", second, err)
	}
	if got := len(host.Events(0)); got != before+1 {
		t.Fatalf("stop emitted duplicate events: before=%d after=%d", before, got)
	}
}

func TestMemoryHostRuntimeValueUsesPresentBitAndQuantities(t *testing.T) {
	missing := MissingRuntimeValue(valueType{Base: valueKindInt, Optional: true, Quantity: quantityCombatAmount})
	zero := IntRuntimeValue(0, quantityCombatAmount)
	if missing.Present() || !zero.Present() {
		t.Fatal("presence was inferred from payload zero value")
	}
	if _, err := CheckedAddRuntimeValues(zero, IntRuntimeValue(1, quantityWorldDistance)); !errors.Is(err, ErrRuntimeQuantityMismatch) {
		t.Fatalf("quantity mismatch = %v", err)
	}
}
