package entity

import (
	"testing"
	"time"
)

// M-01 · 按 kind 的注册表读取不得被注册写入阻塞。
//
// 锁序热路径会读这张表:排序比较器 cmpGuidFunc 每次比较调两次 GetEntityGroup,
// maxLockedGroup 对每个已持有的锁调一次,广播分桶每个 id 调一次。而
// GetEntityGroup 走 IsRemoteCapableEntityID 再走 GetEntityKindRemotePolicy,
// 旧实现在这里取注册表的读写锁,于是"持有实体互斥"与"取注册表锁"之间形成一条
// 获取边;注册期的写锁会把正在做锁序判断的 goroutine 全部挡住。
//
// 新实现按 kind 用定长数组加原子指针,读取只是一次载入,与写入完全不相干。
func TestKindRegistryReadsDoNotBlockOnRegistrationWrites(t *testing.T) {
	t.Cleanup(ResetEntityRegistryForTest)
	ResetEntityRegistryForTest()

	const kind EntityKind = 7
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
		if r.group != EntityGroupRemote {
			t.Fatalf("GetEntityGroup = %d, want remote group %d", r.group, EntityGroupRemote)
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
	t.Cleanup(ResetEntityRegistryForTest)
	ResetEntityRegistryForTest()

	const (
		plain    EntityKind = 11
		upgraded EntityKind = 12
		built    EntityKind = 13
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
	if err := RegisterEntityKindDefs(EntityKindDef{Kind: 14, Category: EntityCategoryNone}); err == nil {
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
	all := GetAllEntityBuilders()
	if len(all) != 1 || all[0] != param {
		t.Fatalf("GetAllEntityBuilders = %v, want exactly the one registered builder", all)
	}

	// Reset clears every slot.
	ResetEntityRegistryForTest()
	if got, ok := EntityCategoryOfKind(built); ok {
		t.Fatalf("reset left kind %d registered with category %d", built, got)
	}
	if got := GetEntityBuilderParam(built); got != nil {
		t.Fatalf("reset left a builder for kind %d", built)
	}
	if got := GetAllEntityBuilders(); len(got) != 0 {
		t.Fatalf("reset left %d builders", len(got))
	}
}

// lockRegistryForTest holds the registry the way a registration does, so the
// test above can prove readers are not blocked by it.
func lockRegistryForTest()   { registryMu.Lock() }
func unlockRegistryForTest() { registryMu.Unlock() }
