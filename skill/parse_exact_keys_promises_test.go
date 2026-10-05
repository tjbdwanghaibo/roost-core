package skill

import (
	"strings"
	"testing"
)

// RR-20261005-NC-150：Parse 承诺“未知字段、重复键都会被拒绝”。encoding/json 按
// 大小写不敏感匹配结构体字段，而重复键扫描按字节比较，于是 {"id":…,"ID":…}
// 两个键都被接受、后者生效，"Cooldown_Ticks" 被当作 cooldown_ticks。旧实现
// 下这些输入全部 Parse 成功。名字作键的 map（memory 等）不受大小写规则约束。
func TestParseRejectsCaseVariantKeys(t *testing.T) {
	replace := func(old, new string) string {
		if !strings.Contains(minimalSkillJSON, old) {
			t.Fatalf("fixture does not contain %q", old)
		}
		return strings.Replace(minimalSkillJSON, old, new, 1)
	}
	cases := map[string]string{
		"top-level duplicate by case":    replace(`"id":"skill.test.minimal",`, `"id":"skill.test.minimal","ID":"skill.test.other",`),
		"top-level non-canonical key":    replace(`"cooldown_ticks":0`, `"Cooldown_Ticks":0`),
		"nested duplicate by case":       replace(`{"flow":"finish","reason":"done"}`, `{"flow":"finish","reason":"done","Reason":"other"}`),
		"nested header by case":          replace(`{"flow":"finish","reason":"done"}`, `{"FLOW":"finish","reason":"done"}`),
		"phase field by case":            replace(`"timeout_ticks":0`, `"Timeout_Ticks":0`),
		"activation policy by case":      replace(`"policy":{"mode":"tap"}`, `"policy":{"Mode":"tap"}`),
		"value expression by case":       replace(`"costs":[]`, `"costs":[{"resource":"mana","amount":{"op":"add","Args":[1,2]}}]`),
		"memory declaration field case":  replace(`"memory":{}`, `"memory":{"counter":{"type":"int","Default":0}}`),
		"presentation field by case":     replace(`"memory":{}`, `"memory":{},"presentation":{"Icon_Keywords":["fire"]}`),
		"generated rejection field case": `{"schema":"roost.skill/v2","error":{"code":"UNSUPPORTED_CAPABILITY","Message":"x","message":"y"}}`,
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			if strings.HasPrefix(name, "generated") {
				if result, err := ParseGenerated([]byte(input)); err == nil {
					t.Fatalf("ParseGenerated accepted a case-variant key: %#v", result.Rejection)
				}
				return
			}
			if definition, err := Parse([]byte(input)); err == nil {
				t.Fatalf("Parse accepted a case-variant or unknown key; id=%q cooldown=%d", definition.ID, definition.CooldownTicks)
			}
		})
	}
}

// 控制：以名字为键的 map 照常区分大小写，规范键的定义照常通过。
func TestParseKeepsNameKeyedMapsAndCanonicalKeys(t *testing.T) {
	if _, err := Parse([]byte(minimalSkillJSON)); err != nil {
		t.Fatalf("canonical definition rejected: %v", err)
	}
	input := strings.Replace(minimalSkillJSON, `"memory":{}`, `"memory":{"counter":{"type":"int","default":0},"Counter":{"type":"int","default":1}}`, 1)
	definition, err := Parse([]byte(input))
	if err != nil {
		t.Fatalf("distinct memory names rejected at Parse: %v", err)
	}
	if len(definition.Memory) != 2 {
		t.Fatalf("memory = %#v, want two distinct names", definition.Memory)
	}
}
