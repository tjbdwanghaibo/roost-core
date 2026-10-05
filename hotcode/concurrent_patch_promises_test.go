package hotcode

import (
	"sync"
	"testing"
)

// RR-20261005-NC-123：一个补丁点的“当前函数 / Meta / 代数”必须作为一个整体切换。之前
// Replace / Revert 分三次独立原子写（current、meta、gen），并发的 Replace 与 Revert
// （两个运维同时操作、或 load_plugin 与 revert 交错）可以永久留下“当前是原函数、Meta 却是
// 补丁版本”或“当前是补丁、Meta 为空”的组合，hotcode.list 报的 Patched 与 Meta 互相矛盾。
// 这里没有可注入的调度点，用有界轮数的并发对撞：修前每次运行都能撞出不一致；修后写者
// 串行、读者一次读出整份状态，任何交错下都一致。

func originalPatchTarget() int { return 1 }
func patchedPatchTarget() int  { return 2 }

func TestConcurrentReplaceAndRevertLeaveAConsistentPoint(t *testing.T) {
	const rounds = 100000
	for round := 0; round < rounds; round++ {
		registry := NewRegistry()
		if err := registry.Register("point", originalPatchTarget); err != nil {
			t.Fatal(err)
		}
		start := make(chan struct{})
		var wait sync.WaitGroup
		wait.Add(2)
		go func() {
			defer wait.Done()
			<-start
			_ = registry.Replace("point", patchedPatchTarget, Meta{Version: "v2"})
		}()
		go func() {
			defer wait.Done()
			<-start
			_ = registry.Revert("point")
		}()
		close(start)
		wait.Wait()

		info := registry.List()[0]
		resolved := registry.Resolve("point", nil).(func() int)()
		patched := resolved == 2
		if info.Patched != patched || info.Patched != (info.Meta.Version == "v2") || info.Generation != 2 {
			t.Fatalf("round %d: torn patch point: resolve=%d patched=%v meta=%+v generation=%d",
				round, resolved, info.Patched, info.Meta, info.Generation)
		}
	}
}
