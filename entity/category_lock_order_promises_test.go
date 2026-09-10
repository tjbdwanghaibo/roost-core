package entity

import (
	"strings"
	"testing"
)

// M-03 · 声明了 category 之后,category 的值就是锁序,不再需要 GetEntityGroupFunc,
// 而远程托管实体恒定排在最前。
//
// 最前这一条不是业务约定而是物理约束:远程托管实体在 dispatch 顶层要拿分布式所有权锁,
// 持着本地互斥去等一次网络往返会把那把锁挡在整条路径上。其余档位之间怎么排都只是业务约定。
func TestRegisteredCategoriesMakeTheCategoryValueTheLockOrder(t *testing.T) {
	t.Cleanup(resetEntityCategoriesForTest)
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

// An application that has not declared its categories keeps the legacy
// mapping hook untouched, so this change cannot move an existing deployment.
func TestUndeclaredCategoriesKeepTheLegacyGroupHook(t *testing.T) {
	t.Cleanup(resetEntityCategoriesForTest)
	resetEntityCategoriesForTest()

	const kind EntityKind = 176
	MustRegisterEntityKindCategory(kind, 2)
	id, err := BuildEntityID(9, kind)
	if err != nil {
		t.Fatal(err)
	}

	previous := GetEntityGroupFunc
	t.Cleanup(func() { GetEntityGroupFunc = previous })

	GetEntityGroupFunc = nil
	if got := GetEntityGroup(id); got != EntityGroupOther {
		t.Fatalf("with no hook and no declared categories, group = %d, want the legacy default %d", got, EntityGroupOther)
	}

	GetEntityGroupFunc = func(category EntityCategory) int {
		if category == 2 {
			return EntityGroupAlliance
		}
		return EntityGroupOther
	}
	if got := GetEntityGroup(id); got != EntityGroupAlliance {
		t.Fatalf("with the legacy hook, group = %d, want %d", got, EntityGroupAlliance)
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
