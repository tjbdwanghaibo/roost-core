//go:build attributeruntime

package combat

// The attribute feature's runtime gate. The generator's output leans on a
// framework contract — AttrID, AttrValue, AttributeMeta, AttributeProfile,
// Selector, Snapshot, Container — that nothing shipped: the scaffold produced
// an empty package, so `roost add attribute` + a profile generated code that
// could not compile (RR-20260917-06). This file is compiled by
// scripts/attribute-runtime.sh in a throwaway module with the pinned
// roost-core and the scaffold's runtime file, so both halves are checked
// together.

import (
	"testing"

	coreattr "github.com/tjbdwanghaibo/roost-core/attribute"
)

func TestGeneratedProfileCarriesIDsMetadataAndDerivedFormula(t *testing.T) {
	profile := NewCombatProfile()
	typed, ok := profile.(*Combat)
	if !ok {
		t.Fatalf("NewCombatProfile returned %T", profile)
	}
	// Typed setters mark their own bit and nothing else.
	if !typed.SetHP(100) || typed.DirtyMask() != AttrMaskHP {
		t.Fatalf("after SetHP: mask=%d want %d", typed.DirtyMask(), AttrMaskHP)
	}
	if typed.SetHP(100) {
		t.Error("setting the same value reported a change")
	}
	typed.SetAttack(7)
	// The derived attribute is recomputed from the inputs that moved.
	if !typed.Update() {
		t.Fatal("Update reported no change although two inputs moved")
	}
	if typed.Power != 7*2+100/10 {
		t.Fatalf("derived power = %d", typed.Power)
	}
	if typed.DirtyMask()&AttrMaskPower == 0 {
		t.Error("the derived attribute did not mark itself dirty")
	}

	// Metadata is addressable by id and by both names.
	meta, ok := typed.MetaByID(AttrHP)
	if !ok || meta.Name != "hp" || meta.Field != "HP" || meta.Mask != AttrMaskHP || meta.Derived {
		t.Fatalf("meta by id = %+v ok=%v", meta, ok)
	}
	if derived, ok := typed.MetaByName("power"); !ok || !derived.Derived {
		t.Fatalf("power is not marked derived: %+v ok=%v", derived, ok)
	}

	// Values round-trip through the map form the framework moves them in.
	values := map[AttrID]AttrValue{}
	typed.ExportValues(values)
	if values[AttrHP] != 100 || values[AttrAttack] != 7 {
		t.Fatalf("exported %v", values)
	}
	fresh := NewCombatProfile()
	if mask := fresh.LoadValues(values); mask == 0 {
		t.Fatal("loading values marked nothing dirty")
	}
	loaded, _ := fresh.(*Combat)
	if loaded.Power != typed.Power {
		t.Fatalf("derived attribute did not survive a load: %d vs %d", loaded.Power, typed.Power)
	}

	typed.ClearDirty()
	if typed.DirtyMask() != 0 {
		t.Error("ClearDirty left bits set")
	}
}

// The container is the framework's half: layers addressed by selector, and a
// snapshot that hands out a copy so a reader cannot mutate the live profile.
func TestContainerSnapshotIsACopyAndTypedAccessorsFindIt(t *testing.T) {
	container := coreattr.NewContainer()
	live, _ := NewCombatProfile().(*Combat)
	live.SetHP(50)
	live.Update()
	container.Install(coreattr.Base, live)

	profile, ok := CombatIn(container, coreattr.Base)
	if !ok {
		t.Fatal("the typed accessor did not find the installed profile")
	}
	if profile.HP != 50 {
		t.Fatalf("snapshot HP = %d", profile.HP)
	}
	// Mutating the snapshot must not touch the container's profile.
	profile.SetHP(999)
	again, _ := CombatIn(container, coreattr.Base)
	if again.HP != 50 {
		t.Fatalf("the snapshot shares state with the container: %d", again.HP)
	}
	// An empty layer answers politely rather than panicking.
	if _, ok := CombatIn(container, coreattr.Selector{Layer: "nope"}); ok {
		t.Error("an unknown layer produced a profile")
	}
}
