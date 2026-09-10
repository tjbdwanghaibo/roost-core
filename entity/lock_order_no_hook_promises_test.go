package entity

import "testing"

// M-04 · 锁档就是 kind 的 category,不再经过任何应用层钩子。
//
// 这条测试**不声明 category**:声明与否不该改变锁档的来源。GetEntityGroupFunc 那个
// 包级可写变量删除后,唯一的来源是注册表里这个 kind 的 category;managed 恒定排在
// EntityCategoryRemote 档;本进程不认识的 kind 排最后,因为持有它之后什么都锁不了,
// 是最保守的答案。
func TestLockOrderIsTheCategoryWithNoApplicationHook(t *testing.T) {
	t.Cleanup(resetEntityCategoriesForTest)

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
	// the remote bit ranks with remote.
	unknown := int64(1) << UniqueIDShift
	unknown |= int64(uint64(200) << EntityKindShift)
	if got := GetEntityGroup(unknown); got != int(EntityCategoryUnknown) {
		t.Errorf("unknown kind lock rank = %d, want last (%d)", got, EntityCategoryUnknown)
	}
	if got := GetEntityGroup(setRemoteCapableBit(unknown)); got != int(EntityCategoryRemote) {
		t.Errorf("unknown remote-bit id lock rank = %d, want remote (%d)", got, EntityCategoryRemote)
	}
}
