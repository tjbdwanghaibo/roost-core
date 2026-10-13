// kind / category 注册表：批量注册不留半批、策略与生命周期同源、锁档就是 category、非法组合逐项拒绝、读写互不阻塞。

package entity

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func assertKindUnregistered(t *testing.T, kind EntityKind) {
	t.Helper()
	if category, ok := EntityCategoryOfKind(kind); ok {
		t.Fatalf("a refused batch left kind %d registered in category %d", kind, category)
	}
	if rank, ok := lockRankOf(kind); ok {
		t.Fatalf("a refused batch left kind %d with lock rank %d", kind, rank)
	}
}

func TestRegisterEntityKindDefsLeavesNoHalfBatch(t *testing.T) {
	isolateEntityRegistry(t)
	MustRegisterEntityKindCategory(157, 2)
	MustRegisterEntityKindDefs(EntityKindDef{Kind: 158, Category: EntityCategoryRemote})

	t.Run("new kind before a category mismatch", func(t *testing.T) {
		err := RegisterEntityKindDefs(
			EntityKindDef{Kind: 156, Category: EntityCategoryRemote, RemotePolicy: RemotePolicyManaged},
			EntityKindDef{Kind: 157, Category: 3},
		)
		expectErrContains(t, err, "entity kind 157 category mismatch: registered=2 new=3")
		assertKindUnregistered(t, 156)
		if category, _ := EntityCategoryOfKind(157); category != 2 {
			t.Fatalf("kind 157 category=%d, want 2", category)
		}
	})

	t.Run("policy upgrade before an invalid definition", func(t *testing.T) {
		err := RegisterEntityKindDefs(
			EntityKindDef{Kind: 158, Category: EntityCategoryRemote, RemotePolicy: RemotePolicyManaged},
			EntityKindDef{Kind: 159, Category: EntityCategoryNone},
		)
		expectErrContains(t, err, "entity category must not be none for kind 159")
		if policy := GetEntityKindRemotePolicy(158); policy != RemotePolicyNone {
			t.Fatalf("a refused batch upgraded kind 158 to policy %d", policy)
		}
		assertKindUnregistered(t, 159)
	})

	t.Run("same kind twice in one batch", func(t *testing.T) {
		err := RegisterEntityKindDefs(
			EntityKindDef{Kind: 160, Category: EntityCategoryRemote, RemotePolicy: RemotePolicyManaged},
			EntityKindDef{Kind: 160, Category: EntityCategoryRemote, RemotePolicy: RemotePolicyMirror},
		)
		expectErrContains(t, err, "entity kind 160 remote policy mismatch")
		assertKindUnregistered(t, 160)
	})

	t.Run("categories batch", func(t *testing.T) {
		err := RegisterEntityKindCategories(
			EntityKindCategory{Kind: 163, Category: 2},
			EntityKindCategory{Kind: 157, Category: 4},
		)
		expectErrContains(t, err, "entity kind 157 category mismatch: registered=2 new=4")
		assertKindUnregistered(t, 163)
	})

	t.Run("Must panics without a half batch", func(t *testing.T) {
		func() {
			defer func() {
				if recover() == nil {
					t.Fatal("MustRegisterEntityKindDefs did not panic on a refused batch")
				}
			}()
			MustRegisterEntityKindDefs(EntityKindDef{Kind: 156, Category: EntityCategoryRemote}, EntityKindDef{Kind: 157, Category: 3})
		}()
		assertKindUnregistered(t, 156)
	})

	t.Run("a valid batch is written whole", func(t *testing.T) {
		if err := RegisterEntityKindDefs(
			EntityKindDef{Kind: 156, Category: EntityCategoryRemote},
			EntityKindDef{Kind: 156, Category: EntityCategoryRemote, RemotePolicy: RemotePolicyManaged},
			EntityKindDef{Kind: 158, Category: EntityCategoryRemote, RemotePolicy: RemotePolicyMirror},
			EntityKindDef{Kind: 157, Category: 2},
		); err != nil {
			t.Fatal(err)
		}
		if !IsEntityKindRemoteManaged(156) || GetEntityKindRemotePolicy(158) != RemotePolicyMirror {
			t.Fatalf("policies after a valid batch: 156=%d 158=%d", GetEntityKindRemotePolicy(156), GetEntityKindRemotePolicy(158))
		}
		if rank, ok := lockRankOf(156); !ok || rank != int(EntityCategoryRemote) {
			t.Fatalf("kind 156 lock rank=(%d,%v), want the remote rank", rank, ok)
		}
	})
}

const (
	kindDefManagedPlainKind    EntityKind = 211 // kind 定义 managed；builder 省略策略，实体不实现 IThreadSafeRemoteEntity
	kindDefManagedLifetimeKind EntityKind = 212 // kind 定义 managed；builder 省略策略与生命周期
	kindDefMirrorLifetimeKind  EntityKind = 213 // kind 定义 mirror；builder 省略策略与生命周期
	lateManagedKind            EntityKind = 214 // builder 先注册（none、默认生命周期），kind 定义随后声明 managed
)

// 注册表是进程级的：每个 kind 只注册一次，-count>1 重复运行时沿用第一次的注册。
var kindPolicyBuilderOnce [4]sync.Once

func registerOnce(slot int, register func()) { kindPolicyBuilderOnce[slot].Do(register) }

func plainKindBuilder(kind EntityKind) *EntityBuilderParam {
	return &EntityBuilderParam{
		Category: 1, Kind: kind, NoPersist: true,
		Builder: func(param *EntityCreateParam) (IThreadSafeEntity, error) {
			return &testEntity{EntityBase: NewEntityBaseWithMutex(param.Id, param.Category, false, param.Mutex, param.Kind)}, nil
		},
	}
}

// 构建出的实体策略与 kind 策略一致（validateBuiltEntityPolicy）。
func TestBuiltEntityPolicyUsesKindPolicy(t *testing.T) {
	registerOnce(0, func() {
		MustRegisterEntityKindDefs(EntityKindDef{Kind: kindDefManagedPlainKind, Category: 1, RemotePolicy: RemotePolicyManaged})
		RegisterEntityBuilder(plainKindBuilder(kindDefManagedPlainKind))
	})
	id, err := BuildEntityID(9973, kindDefManagedPlainKind)
	if err != nil {
		t.Fatal(err)
	}
	e, err := BuildEntity(&EntityCreateParam{IsCreate: true, Category: 1, Kind: kindDefManagedPlainKind, Id: id})
	if err == nil {
		t.Fatalf("kind %d is remote-managed in the registry, but %T (not IThreadSafeRemoteEntity) was built without error", kindDefManagedPlainKind, e)
	}
	if !strings.Contains(err.Error(), "does not implement IThreadSafeRemoteEntity") {
		t.Fatalf("err=%v, want the remote=managed type check", err)
	}
}

// builder 的默认生命周期按 kind 的策略决定，不按 builder 字段（none）。
func TestBuilderDefaultLifetimeFollowsKindPolicy(t *testing.T) {
	for i, tc := range []struct {
		kind   EntityKind
		policy RemotePolicy
		want   EntityLifetime
	}{
		{kindDefManagedLifetimeKind, RemotePolicyManaged, EntityLifetimeRemoteManaged},
		{kindDefMirrorLifetimeKind, RemotePolicyMirror, EntityLifetimeMirrorCache},
	} {
		registerOnce(1+i, func() {
			MustRegisterEntityKindDefs(EntityKindDef{Kind: tc.kind, Category: 1, RemotePolicy: tc.policy})
			RegisterEntityBuilder(plainKindBuilder(tc.kind))
		})
		if got := GetEntityBuilderParam(tc.kind).Lifetime; got != tc.want {
			t.Errorf("kind %d (registry policy %d, builder omits it): default lifetime=%d, want %d", tc.kind, tc.policy, got, tc.want)
		}
	}
}

// 反方向的注册顺序：builder 先按 none 注册并取得默认生命周期，kind 定义随后把策略升级为 managed：生命周期与新策略矛盾，注册被拒绝。
func TestLateKindPolicyUpgradeConflictingWithBuilderIsRefused(t *testing.T) {
	registerOnce(3, func() { RegisterEntityBuilder(plainKindBuilder(lateManagedKind)) })
	err := RegisterEntityKindDefs(EntityKindDef{Kind: lateManagedKind, Category: 1, RemotePolicy: RemotePolicyManaged})
	if err == nil {
		t.Fatalf("kind definition upgraded kind %d to managed after its builder was registered with lifetime %d", lateManagedKind, GetEntityBuilderParam(lateManagedKind).Lifetime)
	}
	if got := GetEntityKindRemotePolicy(lateManagedKind); got != RemotePolicyNone {
		t.Fatalf("policy after the refused upgrade = %d, want none", got)
	}
}

func expectErrContains(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("error = %v, want it to contain %q", err, want)
	}
}

// The kind/category registry is the root of every entity id in the system:
// each refusal is pinned by the reason it names; re-registering an identical pair stays idempotent.
func TestRegisterEntityKindCategoryRefusesEachInvalidPair(t *testing.T) {
	// 用例断言 kind 201 还没注册（“被拒的注册没有留下登记”），重复运行时必须撤销上一轮的登记（RR-20260926-83）。
	isolateEntityRegistry(t)
	const kind EntityKind = 201
	expectErrContains(t, RegisterEntityKindCategory(EntityKindNone, 1), "entity kind must not be none")
	expectErrContains(t, RegisterEntityKindCategory(kind, EntityCategoryNone), "entity category must not be none for kind 201")
	// A category above the ID's two-bit field used to be refused. M-02 made the
	// registry the authority on a kind's category and demoted that field to
	// legacy padding, so the bound is gone: the target taxonomy needs five
	// categories and the field can express three. Checked on its own kind so
	// the pair rules below still run against a fresh one. 239 rather than the
	// original 231: snapshot_load_waiters_promises_test registers 231 in
	// category 1, and only the whole-registry reset that RR-20260926-83 removed
	// kept the two from colliding.
	const aboveField EntityKind = 239
	if err := RegisterEntityKindCategory(aboveField, EntityCategory(EntityCategoryMask)+1); err != nil {
		t.Fatalf("category above the legacy field must now be accepted: %v", err)
	}
	if got, ok := EntityCategoryOfKind(aboveField); !ok || got != EntityCategory(EntityCategoryMask)+1 {
		t.Fatalf("EntityCategoryOfKind = (%d, %v) after registering above the field", got, ok)
	}
	if _, ok := EntityCategoryOfKind(kind); ok {
		t.Fatal("a refused registration left the kind registered")
	}
	if err := RegisterEntityKindCategory(kind, 1); err != nil {
		t.Fatal(err)
	}
	if err := RegisterEntityKindCategory(kind, 1); err != nil {
		t.Fatalf("identical re-registration must be idempotent: %v", err)
	}
	expectErrContains(t, RegisterEntityKindCategory(kind, 2), "entity kind 201 category mismatch: registered=1 new=2")
	if category, _ := EntityCategoryOfKind(kind); category != 1 {
		t.Fatalf("a refused re-registration changed the category to %d", category)
	}
}

func TestResolveEntityKindCategoryRefusesNoneAndUnregistered(t *testing.T) {
	expectErrContains(t, func() error { _, err := ResolveEntityKindCategory(EntityKindNone); return err }(), "entity kind must not be none")
	_, err := ResolveEntityKindCategory(EntityKind(202))
	if !errors.Is(err, ErrInvalidEntityID) || !strings.Contains(err.Error(), "kind 202 category is not registered") {
		t.Fatalf("unregistered kind err = %v", err)
	}
}

// NormalizeID is where a create param's kind, category and id are reconciled
// against the registry before any builder runs.
func TestEntityCreateParamNormalizeIDRefusesEachInconsistency(t *testing.T) {
	const kind, other EntityKind = 203, 204
	MustRegisterEntityKindCategory(kind, 1)
	MustRegisterEntityKindCategory(other, 1)

	var nilParam *EntityCreateParam
	expectErrContains(t, nilParam.NormalizeID(kind), "entity create param is nil")
	expectErrContains(t, (&EntityCreateParam{}).NormalizeID(EntityKindNone), "entity kind must not be none")
	expectErrContains(t, (&EntityCreateParam{Kind: kind}).NormalizeID(other), "entity kind mismatch: param=203 builder=204")
	expectErrContains(t, (&EntityCreateParam{Kind: kind, Category: 2}).NormalizeID(kind), "entity category mismatch: param=2 kind=203 category=1")
	expectErrContains(t, (&EntityCreateParam{Kind: kind}).NormalizeID(kind), "entity load requires full Id or UniqueID")
	if err := (&EntityCreateParam{Kind: kind, IsCreate: true}).NormalizeID(kind); !errors.Is(err, ErrIDGeneratorRequired) {
		t.Fatalf("create without an id err = %v, want ErrIDGeneratorRequired", err)
	}

	param := &EntityCreateParam{UniqueID: 42}
	if err := param.NormalizeID(kind); err != nil {
		t.Fatal(err)
	}
	want, _ := BuildEntityID(42, kind)
	if param.Id != want || param.Kind != kind || param.Category != 1 {
		t.Fatalf("normalized param = %+v, want id %d kind %d category 1", param, want, kind)
	}
}

func TestResolveEntityBuilderRefusesMissingKindAndBuilder(t *testing.T) {
	const kind EntityKind = 205
	MustRegisterEntityKindCategory(kind, 1)
	_, err := resolveEntityBuilder(nil)
	expectErrContains(t, err, "entity create param is nil")
	_, err = resolveEntityBuilder(&EntityCreateParam{})
	expectErrContains(t, err, "entity kind must not be none")
	_, err = resolveEntityBuilder(&EntityCreateParam{Kind: kind, Category: 1})
	expectErrContains(t, err, "no entity builder registered for category 1 kind 205")
}

func TestKindRegistryReadsDoNotBlockOnRegistrationWrites(t *testing.T) {
	// The package's init registers shared test kinds, so this test must not
	// reset the registry; it uses kind values nothing else claims.
	const kind EntityKind = 151
	const category EntityCategory = 2
	MustRegisterEntityKindDefs(EntityKindDef{Kind: kind, Category: category, RemotePolicy: RemotePolicyManaged})

	id, err := BuildEntityID(1, kind)
	if err != nil {
		t.Fatal(err)
	}

	// Hold the registry the way a registration does, then read from another
	// goroutine. A reader that needs the same lock never answers.
	lockRegistryForTest()
	defer unlockRegistryForTest()

	type reads struct {
		category EntityCategory
		found    bool
		policy   RemotePolicy
		group    int
		builder  *EntityBuilderParam
	}
	done := make(chan reads, 1)
	go func() {
		var r reads
		r.category, r.found = EntityCategoryOfKind(kind)
		r.policy = GetEntityKindRemotePolicy(kind)
		r.group = GetEntityGroup(id)
		r.builder = GetEntityBuilderParam(kind)
		done <- r
	}()

	select {
	case r := <-done:
		if !r.found || r.category != category {
			t.Fatalf("EntityCategoryOfKind = (%d, %v), want (%d, true)", r.category, r.found, category)
		}
		if r.policy != RemotePolicyManaged {
			t.Fatalf("GetEntityKindRemotePolicy = %d, want managed", r.policy)
		}
		if r.group != int(EntityCategoryRemote) {
			t.Fatalf("GetEntityGroup = %d, want the remote rank %d", r.group, EntityCategoryRemote)
		}
		if r.builder != nil {
			t.Fatalf("GetEntityBuilderParam = %v, want nil for a kind with no builder", r.builder)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("registry reads blocked while a registration held the registry; the lock-ordering hot path can be stalled by a registration")
	}
}

// The registry's observable answers must not change with the storage swap:
// category, policy and builder identity per kind, the reconciliation rules for
// a second registration, and the full builder listing.
func TestKindRegistryKeepsItsRegistrationRules(t *testing.T) {
	// No reset here either: the package's init registers shared kinds that
	// later tests need, and reset clears every slot. The builder registration
	// below panics on a second run, so this test's own registrations are
	// undone on cleanup instead (RR-20260926-83).
	isolateEntityRegistry(t)
	const (
		plain    EntityKind = 152
		upgraded EntityKind = 153
		built    EntityKind = 154
	)

	// A category-only registration leaves the policy at none.
	MustRegisterEntityKindCategory(plain, 1)
	if got, ok := EntityCategoryOfKind(plain); !ok || got != 1 {
		t.Fatalf("category-only registration = (%d, %v)", got, ok)
	}
	if got := GetEntityKindRemotePolicy(plain); got != RemotePolicyNone {
		t.Fatalf("policy after a category-only registration = %d, want none", got)
	}

	// None is upgraded to a real policy; the reverse is ignored, not an error.
	MustRegisterEntityKindCategory(upgraded, 1)
	MustRegisterEntityKindDefs(EntityKindDef{Kind: upgraded, Category: 1, RemotePolicy: RemotePolicyManaged})
	if got := GetEntityKindRemotePolicy(upgraded); got != RemotePolicyManaged {
		t.Fatalf("policy after an upgrade = %d, want managed", got)
	}
	if err := RegisterEntityKindDefs(EntityKindDef{Kind: upgraded, Category: 1}); err != nil {
		t.Fatalf("re-registering with policy none must be ignored, got %v", err)
	}
	if got := GetEntityKindRemotePolicy(upgraded); got != RemotePolicyManaged {
		t.Fatalf("policy after a none re-registration = %d, want managed still", got)
	}

	// Conflicting facts are refused.
	if err := RegisterEntityKindDefs(EntityKindDef{Kind: upgraded, Category: 2}); err == nil {
		t.Fatal("a category mismatch must be refused")
	}
	if err := RegisterEntityKindDefs(EntityKindDef{Kind: upgraded, Category: 1, RemotePolicy: RemotePolicyMirror}); err == nil {
		t.Fatal("a policy mismatch must be refused")
	}
	if err := RegisterEntityKindDefs(EntityKindDef{Kind: EntityKindNone, Category: 1}); err == nil {
		t.Fatal("kind none must be refused")
	}
	if err := RegisterEntityKindDefs(EntityKindDef{Kind: 155, Category: EntityCategoryNone}); err == nil {
		t.Fatal("category none must be refused")
	}

	// A builder registration also declares the kind, and is listed once.
	param := &EntityBuilderParam{
		Kind:     built,
		Category: 3,
		Builder:  func(*EntityCreateParam) (IThreadSafeEntity, error) { return nil, nil },
	}
	RegisterEntityBuilder(param)
	if got := GetEntityBuilderParam(built); got != param {
		t.Fatalf("GetEntityBuilderParam = %v, want the registered param", got)
	}
	if got, ok := EntityCategoryOfKind(built); !ok || got != 3 {
		t.Fatalf("a builder registration must declare the category, got (%d, %v)", got, ok)
	}
	listed := 0
	for _, candidate := range GetAllEntityBuilders() {
		if candidate == param {
			listed++
		}
	}
	if listed != 1 {
		t.Fatalf("GetAllEntityBuilders listed the registered builder %d times, want once", listed)
	}
}

// lockRegistryForTest holds the registry the way a registration does, so the
// test above can prove readers are not blocked by it.
func lockRegistryForTest()   { registryMu.Lock() }
func unlockRegistryForTest() { registryMu.Unlock() }

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

// M-04 · 锁档就是 kind 的 category,不再经过任何应用层钩子。
//
// 这条测试**不声明 category**:声明与否不该改变锁档的来源。GetEntityGroupFunc 那个
// 包级可写变量删除后,唯一的来源是注册表里这个 kind 的 category;managed 恒定排在
// EntityCategoryRemote 档;本进程不认识的 kind 排最后,因为持有它之后什么都锁不了,
// 是最保守的答案。
func TestLockOrderIsTheCategoryWithNoApplicationHook(t *testing.T) {
	// RR-20260926-83：收尾时原样放回注册表与锁档；原来的 t.Cleanup(resetEntityCategoriesForTest) 把所有 kind
	// 的锁档清零，而重复注册同一定义不会重算锁档，第二轮起这些 kind 的锁档都变成“未派生”。
	isolateEntityRegistry(t)

	const (
		worldKind  EntityKind     = 181
		playerKind EntityKind     = 182
		lateKind   EntityKind     = 183
		catWorld   EntityCategory = EntityCategoryRemote + 1
		catPlayer  EntityCategory = EntityCategoryRemote + 3
	)
	MustRegisterEntityKindDefs(
		EntityKindDef{Kind: worldKind, Category: catWorld},
		EntityKindDef{Kind: playerKind, Category: catPlayer},
		// Remote-managed but declared in a late category: the rank must still
		// be remote's, because that ordering is the one physical constraint.
		EntityKindDef{Kind: lateKind, Category: catPlayer, RemotePolicy: RemotePolicyManaged},
	)

	for _, tc := range []struct {
		name string
		kind EntityKind
		want EntityCategory
	}{
		{"world", worldKind, catWorld},
		{"player", playerKind, catPlayer},
		{"managed declared late", lateKind, EntityCategoryRemote},
	} {
		id, err := BuildEntityID(5, tc.kind)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got := GetEntityGroup(id); got != int(tc.want) {
			t.Errorf("%s: lock rank = %d, want the category %d", tc.name, got, tc.want)
		}
	}

	// A kind this process does not link ranks last, and one whose id carries
	// the remote bit ranks with remote. 250 is claimed by no test in this
	// package; the original 200 is registered by remote_snapshot_test, and
	// only the whole-registry reset removed by RR-20260926-83 hid that.
	unknown := int64(1) << UniqueIDShift
	unknown |= int64(uint64(250) << EntityKindShift)
	if got := GetEntityGroup(unknown); got != int(EntityCategoryUnknown) {
		t.Errorf("unknown kind lock rank = %d, want last (%d)", got, EntityCategoryUnknown)
	}
	if got := GetEntityGroup(setRemoteCapableBit(unknown)); got != int(EntityCategoryRemote) {
		t.Errorf("unknown remote-bit id lock rank = %d, want remote (%d)", got, EntityCategoryRemote)
	}
}

func TestBusinessPoolDefinitionIsImmutableAndValidated(t *testing.T) {
	base := &entityKindEntry{category: 1, businessPool: BusinessPoolLong}
	if _, err := resolveEntityKindDefinition(base, EntityKindDef{Kind: 1, Category: 1, BusinessPool: BusinessPoolShort}); err == nil {
		t.Fatal("changed registered business pool")
	}
	if _, err := resolveEntityKindDefinition(nil, EntityKindDef{Kind: 1, Category: 1, BusinessPool: 99}); err == nil {
		t.Fatal("invalid pool accepted")
	}
	next, err := resolveEntityKindDefinition(base, EntityKindDef{Kind: 1, Category: 1})
	if err != nil || next.businessPool != BusinessPoolLong {
		t.Fatal("partial category registration lost pool")
	}
}
