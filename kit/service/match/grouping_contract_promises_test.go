package match

import (
	"reflect"
	"testing"
)

// U-0217 · C2 · RR-20260916-05:`NewMod(grouping, reporter)` 与 `Config.Grouping` 承诺"哪些候选成组是整个匹配
// 策略,由这里注入",store 里却没有任何路径读取它——Enqueue 只入队、Commit 提交指定票、Sweep 只清过期票,成组
// 完全由调用方 Candidates → Grouping → Commit 驱动。一个项目实现了自己的 Grouping 交给 NewMod,会以为匹配按它的
// 规则进行,实际静默失效。承诺(采用 WANTED / RR 的 B 方向):Mod 与 Store Config 不再接受一个不会被执行的策略;
// `Grouping` 接口及 FirstCome / ScoreWindow 保留为调用方(应用 matchmaker)的工具。测试用反射断言签名,
// 所以修前修后都能编译:修前 Config 有 Grouping 字段、NewMod 收两个参数,红。
func TestGroupingIsNotAnInjectionPointOfTheStore(t *testing.T) {
	if _, has := reflect.TypeOf(Config{}).FieldByName("Grouping"); has {
		t.Fatal("Config.Grouping still exists: the store never executes it, so accepting it is a promise the store cannot keep")
	}
	if in := reflect.TypeOf(NewMod).NumIn(); in != 1 {
		t.Fatalf("NewMod takes %d parameters; the Mod has no executor for a Grouping and must not accept one", in)
	}
	// The policy types stay: they are the matchmaker's tools (see the
	// codegen game-demo matchmaker), just not the store's configuration.
	var _ Grouping = FirstComeGrouping{}
	var _ Grouping = ScoreWindowGrouping{}
}
