package hotcode

import "testing"

func adderFactory(delta int) func(int) int { return func(v int) int { return v + delta } }

// RR-20261005-NC-246（N10 观察 O-H2）：hotcode.list 的 Patched 必须反映“有补丁在生效”。之前
// 用函数代码指针与原函数比较：同一个函数字面量 / 工厂产生、只是捕获值不同的闭包代码指针
// 相同，补丁在生效（行为已变、Meta 已写）却被报成 Patched=false。现在由 Replace / Revert
// 自己记下是否打了补丁，与 Meta、代数一起整体发布。
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

// RR-20261005-NC-247（N10 观察 O-H3）：Resolve[T] 在注册的动态类型不是 T 时静默回落到
// fallback，补丁永远不生效也不报错——Replace 照样成功、hotcode.list 照样显示已打补丁。
// 签名相同、只差命名（func(int) int 与 type namedAdder func(int) int）时现在按 T 转换后
// 照常生效；签名确实不同、无法转换时仍回落 fallback，但计入 PointInfo.ResolveMismatches，
// 让运维在 hotcode.list 里看到“这个点的调用方类型对不上”。
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
