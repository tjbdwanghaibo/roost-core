package cfggen

// 维护者第十二轮决定（2026-10-06，B 类“cfggen globals 规则”）：globals 支持 required / min /
// enum，与 tables 用同一份规则（configdata/rules.Rule）。以前 cfggen 直接拒绝 globals 上的
// 规则，单例配置里漏写或写错的值（宽度 0、模式拼错）照样被加载。现在生成器把规则写成
// ObjectDef.Rules 字面量，configdata 在每次 Load / Reload 用 rules.CheckObject 检查；生成期
// 用同一个 rules.Rule.Validate 检查规则声明本身。unique / ref / index 对单个对象没有意义，
// 仍然拒绝。真实加载与 reload 的拒绝由 scripts/cfggen-golden-runtime.sh 跑的 roundtrip 覆盖。

import (
	"strings"
	"testing"
)

const globalRulesMeta = `globals:
  - name: world
    fields:
      - { name: width, type: int32, required: true, min: 1 }
      - { name: mode,  type: string, enum: [pve, pvp] }
      - { name: title, type: string }
`

func TestCfggenGlobalRulesBecomeObjectDefRules(t *testing.T) {
	source, err := runCfggen(t, globalRulesMeta)
	if err != nil {
		t.Fatalf("globals with required / min / enum rejected: %v", err)
	}
	for _, want := range []string{
		`configdata.ObjectDef[WorldCfg]{Name: "world", File: "world.json", Rules: []configdata.FieldRule{`,
		`{Field: "width", Required: true, Min: "1"}`,
		`{Field: "mode", Enum: []string{"pve", "pvp"}}`,
	} {
		if !strings.Contains(source, want) {
			t.Fatalf("generated source lacks %q:\n%s", want, source)
		}
	}
	// 对象注册路径不解析 cfg 标签：规则只在 Rules 字面量里，标签里不再写一份不生效的。
	if strings.Contains(source, `cfg:"`) {
		t.Fatalf("global fields carry cfg tags nothing evaluates:\n%s", source)
	}
}

func TestCfggenGlobalWithoutRulesKeepsItsRegistration(t *testing.T) {
	source, err := runCfggen(t, "globals:\n  - name: world\n    fields:\n      - { name: width, type: int32 }\n")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(source, `configdata.ObjectDef[WorldCfg]{Name: "world", File: "world.json"}`) {
		t.Fatalf("a global without rules changed its registration:\n%s", source)
	}
}

func TestCfggenGlobalRulesStillRejectWhatAnObjectCannotMean(t *testing.T) {
	for name, tc := range map[string]struct{ meta, want string }{
		"unique":         {"globals:\n  - name: g\n    fields:\n      - { name: x, type: int32, unique: true }\n", "unique"},
		"min on string":  {"globals:\n  - name: g\n    fields:\n      - { name: x, type: string, min: 1 }\n", "min needs a numeric field"},
		"enum repeats":   {"globals:\n  - name: g\n    fields:\n      - { name: x, type: string, enum: [a, a] }\n", "enum repeats"},
		"enum on a bean": {"beans:\n  - name: B\n    fields:\n      - { name: y, type: int32 }\nglobals:\n  - name: g\n    fields:\n      - { name: x, type: B, enum: [a] }\n", "enum needs"},
	} {
		if _, err := runCfggen(t, tc.meta); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: err = %v, want %q", name, err, tc.want)
		}
	}
}
