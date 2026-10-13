package attribute

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// profile 解析器的每条规则都只有这里守：每个用例在合法 profile 上只破坏一条规则，按点名的原因拒绝。
func TestParseDirRejectsEachProfileViolation(t *testing.T) {
	const head = "package attribute\n\n"
	cases := []struct{ label, source, want string }{
		{"more fields than max", head + "//roost:attribute index=1 max=1\ntype P struct {\n\tHP int64\n\tMP int64\n\tdirtyMask uint64\n}\n", "more than max=1"},
		{"duplicate field", head + "//roost:attribute index=1 max=4\ntype P struct {\n\tHP int64\n\tHP int64\n\tdirtyMask uint64\n}\n", "duplicate field HP"},
		{"duplicate formula", head + "//roost:attribute index=1 max=4\ntype P struct {\n\tHP int64\n\tTotal int64\n\tdirtyMask uint64\n}\n\nfunc (p P) _Total(HP int64) int64 { return HP }\nfunc (p P) _Total(HP int64) int64 { return HP }\n", "formula duplicated"},
		{"two results", head + "//roost:attribute index=1 max=4\ntype P struct {\n\tHP int64\n\tTotal int64\n\tdirtyMask uint64\n}\n\nfunc (p P) _Total(HP int64) (int64, int64) { return HP, HP }\n", "must return exactly one value"},
		{"result type mismatch", head + "//roost:attribute index=1 max=4\ntype P struct {\n\tHP int64\n\tTotal int64\n\tdirtyMask uint64\n}\n\nfunc (p P) _Total(HP int64) int32 { return 0 }\n", "returns int32, want int64"},
		{"unknown input field", head + "//roost:attribute index=1 max=4\ntype P struct {\n\tHP int64\n\tTotal int64\n\tdirtyMask uint64\n}\n\nfunc (p P) _Total(Nope int64) int64 { return Nope }\n", "references unknown field Nope"},
		{"input type mismatch", head + "//roost:attribute index=1 max=4\ntype P struct {\n\tHP int64\n\tTotal int64\n\tdirtyMask uint64\n}\n\nfunc (p P) _Total(HP int32) int64 { return int64(HP) }\n", "param HP has type int32, want int64"},
		{"no inputs", head + "//roost:attribute index=1 max=4\ntype P struct {\n\tHP int64\n\tTotal int64\n\tdirtyMask uint64\n}\n\nfunc (p P) _Total() int64 { return 1 }\n", "must reference at least one attribute field"},
		// The one field a declaration must carry itself (RR-20260917-06).
		{"no dirty mask", head + "//roost:attribute index=1 max=4\ntype P struct {\n\tHP int64\n}\n", "must declare an unexported `dirtyMask uint64` field"},
		{"formula cycle", head + "//roost:attribute index=1 max=4\ntype P struct {\n\tA int64\n\tB int64\n\tdirtyMask uint64\n}\n\nfunc (p P) _A(B int64) int64 { return B }\nfunc (p P) _B(A int64) int64 { return A }\n", "formula cycle"},
	}
	for _, testCase := range cases {
		t.Run(testCase.label, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "profile.go"), []byte(testCase.source), 0o644); err != nil {
				t.Fatal(err)
			}
			_, err := parseDir(dir)
			if err == nil {
				t.Fatalf("accepted a profile that breaks %q", testCase.label)
			}
			if !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("error %q does not contain %q", err, testCase.want)
			}
		})
	}
	// And the valid profile with a formula still parses, so the cases above
	// fail for the reason they name rather than for the fixture being broken.
	dir := t.TempDir()
	valid := head + "//roost:attribute index=1 max=4\ntype P struct {\n\tHP int64\n\tTotal int64\n\tdirtyMask uint64\n}\n\nfunc (p P) _Total(HP int64) int64 { return HP * 2 }\n"
	if err := os.WriteFile(filepath.Join(dir, "profile.go"), []byte(valid), 0o644); err != nil {
		t.Fatal(err)
	}
	profiles, err := parseDir(dir)
	if err != nil || len(profiles) != 1 {
		t.Fatalf("valid profile: profiles=%d err=%v", len(profiles), err)
	}
}

// 每个属性都以 AttrValue（int64）进出框架，脏位是 uint64 里的一位，AttrID 是 uint16：float 字段（会被截断进稀疏表）、
// 超过 64 个字段（生成 `1 << 64` 常量、编译失败）等无法这样表示的声明在生成期拒绝并说明原因；合法的整数字段（含窄类型）
// 照常生成。历史：RR-20261005-NC-62。
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

// 非数字 / 非正数拒绝，空值取回退值。
func TestParsePositiveIntRefusesNonPositiveValues(t *testing.T) {
	for _, raw := range []string{"abc", "0", "-3"} {
		if _, err := parsePositiveInt(raw, 4); err == nil || !strings.Contains(err.Error(), "invalid positive int") {
			t.Fatalf("parsePositiveInt(%q) = %v", raw, err)
		}
	}
	if v, err := parsePositiveInt("", 4); err != nil || v != 4 {
		t.Fatalf("parsePositiveInt(\"\") = (%d, %v), want the fallback", v, err)
	}
	if v, err := parsePositiveInt("7", 4); err != nil || v != 7 {
		t.Fatalf("parsePositiveInt(\"7\") = (%d, %v)", v, err)
	}
}
