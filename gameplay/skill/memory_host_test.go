package skill

import (
	"errors"
	"reflect"
	"strings"
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
	catalog := defaultGameplayCatalog()
	catalog.Resources.Entries = append(catalog.Resources.Entries, ResourceCatalogEntry{Handle: 2, Key: "rage", Maximum: 100})
	host.ConfigureGameplayCatalog(catalog)
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

func TestMemoryHostStopSpawnIsIdempotent(t *testing.T) {
	host := NewMemoryHost(AuthorityIdentity{Revision: "test", Digest: "test"})
	_, err := host.StepSpawn(SpawnStepCommand{Meta: SpawnCommandMeta{SpawnID: 7}, Motion: StaticMotionStep{}}, SpawnHostState{SpawnID: 7})
	if err != nil {
		t.Fatal(err)
	}
	before := len(host.Events(0))
	first, err := host.StopSpawn(SpawnStopCommand{Meta: SpawnCommandMeta{SpawnID: 7}}, SpawnHostState{SpawnID: 7})
	if err != nil || !first.Changed {
		t.Fatalf("first stop = %#v, %v", first, err)
	}
	second, err := host.StopSpawn(SpawnStopCommand{Meta: SpawnCommandMeta{SpawnID: 7}}, SpawnHostState{SpawnID: 7})
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

func TestMemoryHostRevisionAndEventCursorContract(t *testing.T) {
	host := NewMemoryHost(AuthorityIdentity{Revision: "test", Digest: "test"})
	host.UpsertEntity(MemoryEntity{ID: 1, Alive: true, Position: Position{}, Resources: map[string]int64{"mana": 5}})
	current := host.CurrentRevision()
	if _, err := host.Read(ReadRequest{Meta: QueryMeta{RequiredRevision: current + 1}, Payload: ResourceRead{Entity: 1, Resource: "mana"}}); !errors.Is(err, ErrRevisionUnavailable) {
		t.Fatalf("future read = %v", err)
	}
	if _, err := host.Select(SelectRequest{Meta: QueryMeta{RequiredRevision: current + 1}, Shape: CircleSelectShape{Radius: 10}, Limit: 1}); !errors.Is(err, ErrRevisionUnavailable) {
		t.Fatalf("future select = %v", err)
	}

	failedRevision := host.CurrentRevision()
	failed, err := host.Apply(EffectCommand{Payload: TeleportCommand{Target: 999, Destination: Position{X: 1}}})
	payload, ok := failed.Payload.(TeleportEffectResult)
	if err != nil || !ok || payload.Succeeded || payload.FailureReason != ExpectedFailureInvalidTarget {
		t.Fatalf("missing target = %#v, %v", failed, err)
	}
	if host.CurrentRevision() != failedRevision {
		t.Fatal("expected failure advanced revision")
	}

	result, err := host.Apply(EffectCommand{Payload: TeleportCommand{Target: 1, Destination: Position{X: 4}}})
	if err != nil {
		t.Fatal(err)
	}
	events := host.Events(0)
	last := events[len(events)-1]
	if result.Commit.Revision != last.Revision || result.Commit.Revision != host.CurrentRevision() {
		t.Fatalf("commit/event/current revisions differ: %#v %#v %d", result.Commit, last, host.CurrentRevision())
	}
	read, err := host.Read(ReadRequest{Meta: QueryMeta{RequiredRevision: result.Commit.Revision}, Payload: PositionRead{Entity: 1}})
	if err != nil || read.Meta.Revision != result.Commit.Revision {
		t.Fatalf("read did not use required committed snapshot: %#v %v", read, err)
	}
	if got := host.Events(last.Cursor); len(got) != 0 {
		t.Fatalf("Events(after) returned stale events: %#v", got)
	}
	if len(events) > 1 {
		after := host.Events(events[len(events)-2].Cursor)
		if len(after) != 1 || after[0].Cursor != last.Cursor {
			t.Fatalf("event cursor is not strict: %#v", after)
		}
	}
}
