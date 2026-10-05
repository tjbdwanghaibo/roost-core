package attribute

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// RR-20261005-NC-62：每个属性都以 AttrValue（int64）进出框架，脏位是 uint64 里的一位，AttrID 是
// uint16。生成器以前不检查声明能否这样表示：float 字段照样生成，GetAttr / ExportValues 把 0.15
// 截成 0 写进稀疏表（导出、持久化、上线回放全丢值，且违反“零值不进表”）；max 超过 64 时第 65 个
// 字段生成 `1 << 64` 常量，生成成功、编译失败。承诺：这些声明在生成期就被拒绝并说明原因；
// 合法的整数字段（含窄类型）照常生成。
func TestParseDirRejectsAttributesTheWireCannotCarry(t *testing.T) {
	const head = "package attribute\n\n"
	cases := []struct{ label, source, want string }{
		{"float64 field", head + "//roost:attribute index=1 max=4\ntype P struct {\n\tRate float64\n\tdirtyMask uint64\n}\n", "Rate has type float64"},
		{"float32 field", head + "//roost:attribute index=1 max=4\ntype P struct {\n\tRate float32\n\tdirtyMask uint64\n}\n", "Rate has type float32"},
		{"bool field", head + "//roost:attribute index=1 max=4\ntype P struct {\n\tAlive bool\n\tdirtyMask uint64\n}\n", "Alive has type bool"},
		{"string field", head + "//roost:attribute index=1 max=4\ntype P struct {\n\tTitle string\n\tdirtyMask uint64\n}\n", "Title has type string"},
		{"max beyond the dirty mask", head + "//roost:attribute index=1 max=65\ntype P struct {\n\tHP int64\n\tdirtyMask uint64\n}\n", "max=65 exceeds the 64 bits"},
		{"ids beyond AttrID", head + "//roost:attribute index=65530 max=8\ntype P struct {\n\tHP int64\n\tdirtyMask uint64\n}\n", "beyond the AttrID range"},
	}
	for _, testCase := range cases {
		t.Run(testCase.label, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "profile.go"), []byte(testCase.source), 0o644); err != nil {
				t.Fatal(err)
			}
			_, err := parseDir(dir)
			if err == nil {
				t.Fatalf("accepted a profile the framework cannot represent (%s)", testCase.label)
			}
			if !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("error %q does not contain %q", err, testCase.want)
			}
		})
	}

	// Integer fields of every width still generate: narrowing is the
	// documented conversion, not a refusal. So does the full 64-bit mask.
	dir := t.TempDir()
	valid := head + "//roost:attribute index=65471 max=64\ntype P struct {\n\tA int8\n\tB int16\n\tC int32\n\tD int64\n\tE int\n\tF uint8\n\tG uint16\n\tH uint32\n\tI uint64\n\tJ uint\n\tdirtyMask uint64\n}\n"
	if err := os.WriteFile(filepath.Join(dir, "profile.go"), []byte(valid), 0o644); err != nil {
		t.Fatal(err)
	}
	profiles, err := parseDir(dir)
	if err != nil || len(profiles) != 1 || len(profiles[0].Fields) != 10 {
		t.Fatalf("integer profile: profiles=%d err=%v", len(profiles), err)
	}
}
