// 补丁注册表：替换 / 解析 / 回退、签名不符、注册与入口守卫、并发切换一致、Patched 与类型转换的可见性、Bundle 回滚。

package hotcode

import (
	"errors"
	"strings"
	"sync"
	"testing"
)

func TestRegistryReplaceResolveAndRevert(t *testing.T) {
	reg := NewRegistry()
	name := "test.fn"
	orig := func(v int) int { return v + 1 }
	patch := func(v int) int { return v + 10 }

	if err := reg.Register(name, orig); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if got := reg.Resolve(name, orig).(func(int) int)(1); got != 2 {
		t.Fatalf("orig = %d", got)
	}
	if err := reg.Replace(name, patch, Meta{Version: "v1"}); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	if got := reg.Resolve(name, orig).(func(int) int)(1); got != 11 {
		t.Fatalf("patched = %d", got)
	}
	if err := reg.Revert(name); err != nil {
		t.Fatalf("Revert: %v", err)
	}
	if got := reg.Resolve(name, orig).(func(int) int)(1); got != 2 {
		t.Fatalf("reverted = %d", got)
	}
}

func TestRegistryRejectsSignatureMismatch(t *testing.T) {
	reg := NewRegistry()
	if err := reg.Register("test.fn", func(int) int { return 1 }); err != nil {
		t.Fatalf("Register: %v", err)
	}
	err := reg.Replace("test.fn", func(string) int { return 1 }, Meta{})
	if !errors.Is(err, ErrTypeMismatch) {
		t.Fatalf("expected ErrTypeMismatch, got %v", err)
	}
}

// A patch point is addressed by name; an empty name, a non-function, or a
// second registration under the same name would make Replace ambiguous.
func TestRegistryRefusesNamelessNonFunctionAndDuplicatePoints(t *testing.T) {
	r := NewRegistry()
	if err := r.Register("", func() {}); !errors.Is(err, ErrNameRequired) {
		t.Fatalf("empty name = %v", err)
	}
	if err := r.Register("point", 42); !errors.Is(err, ErrFuncRequired) {
		t.Fatalf("non-function = %v", err)
	}
	if err := r.Register("point", nil); !errors.Is(err, ErrFuncRequired) {
		t.Fatalf("nil function = %v", err)
	}
	if err := r.Register("point", func() {}); err != nil {
		t.Fatal(err)
	}
	if err := r.Register("point", func() {}); !errors.Is(err, ErrDuplicate) || !strings.Contains(err.Error(), "point") {
		t.Fatalf("duplicate = %v", err)
	}
	if err := r.Replace("", func() {}, Meta{}); !errors.Is(err, ErrNameRequired) {
		t.Fatalf("replace without name = %v", err)
	}
	if err := r.Replace("point", "not a func", Meta{}); !errors.Is(err, ErrFuncRequired) {
		t.Fatalf("replace with non-function = %v", err)
	}
}

// 管理命令注册缺注册表拒绝；未知补丁点报 ErrNotFound。插件路径校验按平台分文件（plugin_guards_test.go /
// plugin_stub_guards_test.go）；真实 .so 的加载在 hotcode/plugintest 里现场构建验证。
func TestHotcodeEntryPointsRefuseMissingRegistryPathsAndPoints(t *testing.T) {
	if err := RegisterAdminCommands(nil); err == nil || !strings.Contains(err.Error(), "admin registry is required") {
		t.Fatalf("RegisterAdminCommands(nil) = %v", err)
	}
	if err := NewRegistry().Revert("missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Revert of an unknown point = %v", err)
	}
}

// 一个补丁点的“当前函数 / Meta / 代数”作为一个整体切换：并发的 Replace 与 Revert 在任何交错下，list 读到的 Patched 与 Meta 都一致。
// 没有可注入的调度点，用有界轮数的并发对撞。
// 历史：RR-20261005-NC-123。

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

func adderFactory(delta int) func(int) int { return func(v int) int { return v + delta } }

// hotcode.list 的 Patched 反映“有补丁在生效”：同一个函数字面量 / 工厂产生、只是捕获值不同的闭包代码指针相同，也要报 Patched=true。
// Replace / Revert 自己记下是否打了补丁，与 Meta、代数一起整体发布。
// 历史：RR-20261005-NC-246。
func TestListReportsAClosurePatchFromTheSameFactoryAsPatched(t *testing.T) {
	r := NewRegistry()
	if err := r.Register("closure", adderFactory(1)); err != nil {
		t.Fatal(err)
	}
	if err := r.Replace("closure", adderFactory(100), Meta{Version: "v2"}); err != nil {
		t.Fatal(err)
	}
	if got := r.Resolve("closure", adderFactory(1)).(func(int) int)(1); got != 101 {
		t.Fatalf("resolved patch returns %d, want 101", got)
	}
	info := r.List()[0]
	if !info.Patched || info.Meta.Version != "v2" {
		t.Fatalf("List = %+v, want Patched=true with the patch's Meta: the patch is in effect", info)
	}
	if err := r.Revert("closure"); err != nil {
		t.Fatal(err)
	}
	if info := r.List()[0]; info.Patched {
		t.Fatalf("List after Revert = %+v, want Patched=false", info)
	}
}

type namedAdder func(int) int

// Resolve[T] 在签名相同、只差命名（func(int) int 与 type namedAdder func(int) int）时按 T 转换后照常生效；签名确实不同、无法转换时
// 回落 fallback，但计入 PointInfo.ResolveMismatches，让运维在 hotcode.list 里看到“这个点的调用方类型对不上”。
// 历史：RR-20261005-NC-247。
func TestResolveConvertsAnIdenticalSignatureAndCountsRealMismatches(t *testing.T) {
	ResetForTest()
	t.Cleanup(ResetForTest)
	original := func(v int) int { return v + 1 }
	if err := Register("resolve.typed", original); err != nil {
		t.Fatal(err)
	}
	if err := Replace("resolve.typed", func(v int) int { return v + 100 }, Meta{Version: "v2"}); err != nil {
		t.Fatal(err)
	}
	if got := Resolve[namedAdder]("resolve.typed", namedAdder(original))(1); got != 101 {
		t.Fatalf("Resolve[namedAdder] returned the fallback (%d), want the patch (101): a registered patch never takes effect for this caller", got)
	}
	if got := Resolve[func(string) string]("resolve.typed", func(s string) string { return s })("x"); got != "x" {
		t.Fatalf("Resolve with an unrelated signature = %q, want the fallback", got)
	}
	if info := List()[0]; info.ResolveMismatches != 1 {
		t.Fatalf("ResolveMismatches = %d, want 1 (the unrelated-signature call)", info.ResolveMismatches)
	}
}

type panickingBundle struct{}

func (panickingBundle) Meta() Meta { return Meta{Version: "boom"} }
func (panickingBundle) Apply(r *Registry) error {
	if err := r.Replace("panic.target", func(v int) int { return v + 100 }, Meta{Version: "boom"}); err != nil {
		return err
	}
	panic("bundle exploded halfway")
}
func (panickingBundle) Revert(*Registry) error { return nil }

// NC-245 的进程内补充（不需要 .so）：Apply 中途 panic 同样恢复成应用前那一代，并返回错误。
func TestApplyBundleRollsBackWhenApplyPanics(t *testing.T) {
	r := NewRegistry()
	if err := r.Register("panic.target", func(v int) int { return v + 1 }); err != nil {
		t.Fatal(err)
	}
	restored, err := r.ApplyBundle(panickingBundle{})
	if err == nil || restored != 1 {
		t.Fatalf("ApplyBundle = (%d, %v), want 1 restored point and the panic as an error", restored, err)
	}
	if got := resolveTyped(r, "panic.target", func(v int) int { return v })(1); got != 2 {
		t.Fatalf("after the panicking bundle target(1) = %d, want 2", got)
	}
	if info := r.List()[0]; info.Patched || info.Meta.Version != "" {
		t.Fatalf("after rollback %+v, want unpatched", info)
	}
}
