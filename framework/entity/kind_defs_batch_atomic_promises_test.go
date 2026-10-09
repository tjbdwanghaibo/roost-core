package entity

import "testing"

// RR-20260927-10（OPEN-ITEMS C17）：RegisterEntityKindDefs / RegisterEntityKindCategories 按条写入注册表，中途某条被拒时
// 直接返回，前面已写入的定义（新 kind、策略升级、派生锁档）留在注册表里。调用方拿到错误后无法得知哪些已生效，重试同一批
// 还可能因为“部分已登记”得到不同结果。承诺：先对整批校验（含批内同一 kind 的多条定义），全部通过才写入；出错时注册表与
// 派生锁档保持调用前的样子。Must* 版本 panic 同样不留半批。

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
