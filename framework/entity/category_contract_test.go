package entity

import (
	"strings"
	"testing"
)

func TestRegisteredCategoriesMakeTheCategoryValueTheLockOrder(t *testing.T) {
	// RR-20260926-83：收尾时原样放回注册表与锁档；原来的 t.Cleanup(resetEntityCategoriesForTest) 把所有 kind
	// 的锁档清零，而重复注册同一定义不会重算锁档，第二轮起这些 kind 的锁档都变成“未派生”。
	isolateEntityRegistry(t)
	resetEntityCategoriesForTest()

	const (
		catWorld        = EntityCategoryRemote + 1
		catPlayerScoped = EntityCategoryRemote + 2
		catPlayer       = EntityCategoryRemote + 3
		catOther        = EntityCategoryRemote + 4
	)
	MustRegisterEntityCategories(
		EntityCategoryDef{Category: EntityCategoryRemote, Name: "remote"},
		EntityCategoryDef{Category: catWorld, Name: "world"},
		EntityCategoryDef{Category: catPlayerScoped, Name: "player_scoped"},
		EntityCategoryDef{Category: catPlayer, Name: "player"},
		EntityCategoryDef{Category: catOther, Name: "other"},
	)

	const (
		remoteKind      EntityKind = 171
		worldKind       EntityKind = 172
		playerKind      EntityKind = 173
		otherKind       EntityKind = 174
		managedElseKind EntityKind = 175
	)
	MustRegisterEntityKindDefs(
		EntityKindDef{Kind: remoteKind, Category: EntityCategoryRemote, RemotePolicy: RemotePolicyManaged},
		EntityKindDef{Kind: worldKind, Category: catWorld},
		EntityKindDef{Kind: playerKind, Category: catPlayer},
		EntityKindDef{Kind: otherKind, Category: catOther},
		// A managed kind declared outside the remote category: the rank must
		// still put it first, because that ordering is a physical constraint,
		// and Seal must report the declaration as wrong.
		EntityKindDef{Kind: managedElseKind, Category: catPlayer, RemotePolicy: RemotePolicyManaged},
	)

	for _, tc := range []struct {
		name string
		kind EntityKind
		want int
	}{
		{"remote", remoteKind, int(EntityCategoryRemote)},
		{"world", worldKind, int(catWorld)},
		{"player", playerKind, int(catPlayer)},
		{"other", otherKind, int(catOther)},
		{"managed declared elsewhere still ranks first", managedElseKind, int(EntityCategoryRemote)},
	} {
		id, err := BuildEntityID(9, tc.kind)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got := GetEntityGroup(id); got != tc.want {
			t.Errorf("%s: GetEntityGroup = %d, want the category value %d", tc.name, got, tc.want)
		}
	}

	// Remote is the minimum, so a world object may be taken after a remote
	// entity but never before one.
	if int(EntityCategoryRemote) >= int(catWorld) {
		t.Fatal("EntityCategoryRemote must be the lowest rank")
	}

	// Names are for diagnostics only.
	if got := EntityCategoryName(catWorld); got != "world" {
		t.Errorf("EntityCategoryName = %q, want %q", got, "world")
	}

	// Seal reports the managed kind that was declared outside the remote
	// category, and says which kind it is.
	err := ValidateEntityRegistry()
	if err == nil {
		t.Fatal("ValidateEntityRegistry accepted a managed kind outside the remote category")
	}
	if !strings.Contains(err.Error(), "175") {
		t.Errorf("seal error does not name the offending kind: %v", err)
	}
}

// resetEntityCategoriesForTest drops the declared taxonomy and every derived
// rank, so a test can exercise the legacy path and the declared path in the
// same package run. It deliberately does NOT touch the kind registry, which
// this package's init populates for every other test.
func resetEntityCategoriesForTest() {
	categoryMu.Lock()
	defer categoryMu.Unlock()
	taxonomy.Store(nil)
	registryMu.Lock()
	defer registryMu.Unlock()
	for kind := range lockRankByKind {
		lockRankByKind[kind].Store(0)
	}
}

func TestEntityCategoriesBeyondTheIDMaskAreAllowed(t *testing.T) {
	// No reset: the package's init registers shared test kinds.
	const (
		fourth  EntityKind     = 161
		fifth   EntityKind     = 162
		catFour EntityCategory = EntityCategory(EntityCategoryMask) + 1
		catFive EntityCategory = EntityCategory(EntityCategoryMask) + 2
	)

	if err := RegisterEntityKindCategory(fourth, catFour); err != nil {
		t.Fatalf("registering category %d must be allowed once category left the ID: %v", catFour, err)
	}
	if err := RegisterEntityKindCategory(fifth, catFive); err != nil {
		t.Fatalf("registering category %d must be allowed: %v", catFive, err)
	}

	// The registry, not the ID's low bits, answers what a kind's category is.
	if got, ok := EntityCategoryOfKind(fourth); !ok || got != catFour {
		t.Fatalf("EntityCategoryOfKind = (%d, %v), want (%d, true)", got, ok, catFour)
	}

	// IDs still round-trip: kind and unique id survive, and the id resolves to
	// the registered category even though it cannot fit in the low two bits.
	id, err := BuildEntityID(1234, fourth)
	if err != nil {
		t.Fatalf("BuildEntityID for a kind above the mask: %v", err)
	}
	meta := ResolveEntityID(id)
	if meta.Kind != fourth {
		t.Fatalf("resolved kind = %d, want %d", meta.Kind, fourth)
	}
	if meta.UniqueID != 1234 {
		t.Fatalf("resolved unique id = %d, want 1234", meta.UniqueID)
	}
	if meta.Category != catFour {
		t.Fatalf("resolved category = %d, want the registered %d", meta.Category, catFour)
	}
	if _, err := NormalizeFullID(id, fourth); err != nil {
		t.Fatalf("NormalizeFullID must accept a kind whose category exceeds the mask: %v", err)
	}
}
