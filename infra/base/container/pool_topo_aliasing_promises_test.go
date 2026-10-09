package container

import (
	"slices"
	"testing"
)

// RR-20261005-NC-267（N13 观察 O5）：同一个对象 Put 两次不能让它在空闲表里出现两份。
// 旧行为：Put 在 workList 里找不到对象（第二次 Put）时照样追加到 freeList，之后两次 Get 交出同一个
// 对象，两个使用者共享一份可变状态。
func TestObjectPoolIgnoresASecondPutOfTheSameObject(t *testing.T) {
	type item struct{ n int }
	pool := NewObjectPool(func() *item { return &item{} }, nil)
	a := pool.Get()
	pool.Put(a)
	pool.Put(a) // 重复归还

	x := pool.Get()
	y := pool.Get()
	if x == y {
		t.Fatalf("two Gets after a double Put returned the same object %p: both callers now share it", x)
	}
	// 邻近分支：从未经 Get 的外来对象仍可放入，且只放一份。
	foreign := &item{n: 7}
	pool.Put(foreign)
	pool.Put(foreign)
	if got := pool.Get(); got != foreign {
		t.Fatalf("Get after Put(foreign) = %p, want the foreign object %p", got, foreign)
	}
	if got := pool.Get(); got == foreign {
		t.Fatal("a foreign object put twice was handed out twice")
	}
}

// RR-20261005-NC-268（N13 观察 O4）：依赖表与调用方互不共享切片。
// 旧行为：RegisterCompDependency 保存调用方的变参切片（`deps...` 传的就是调用方数组），
// GetCompDependencies 返回内部切片；任一方改动都直接改了登记的依赖，且不使排序缓存失效。
func TestTopologicalSortCacheDoesNotShareSlicesWithCallers(t *testing.T) {
	cache := NewTopologicalSortCache[string]()
	deps := []string{"B", "C"}
	cache.RegisterCompDependency("A", deps...)
	deps[0] = "X" // 调用方复用自己的切片

	if got := cache.GetCompDependencies("A"); !slices.Equal(got, []string{"B", "C"}) {
		t.Errorf("dependencies of A after the caller reused its slice = %v, want [B C]", got)
	}

	got := cache.GetCompDependencies("A")
	got[1] = "Y" // 调用方改返回值
	if again := cache.GetCompDependencies("A"); !slices.Equal(again, []string{"B", "C"}) {
		t.Fatalf("dependencies of A after the caller edited the returned slice = %v, want [B C]", again)
	}
}
