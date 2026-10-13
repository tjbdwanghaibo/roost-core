package roost

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// `roost id next errcode`、`roost add errcode`（NextID）、`roost id check` / doctor（CheckIDs）与 errcode 生成器看到同一组
// 错误码定义：认别名 / 点导入，不认注释和字符串里的文字。否则 add 发出重复编号、id check 报 valid，或误报重复。

func TestIDToolsSeeErrcodeDefinitionsTheWayTheGeneratorDoes(t *testing.T) {
	t.Parallel()
	root := copyOfNewProject(t, "configdata")
	m, err := LoadManifest(root)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "internal", "errorsprobe")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	first, err := NextID(root, m, "errcode", "")
	if err != nil {
		t.Fatal(err)
	}
	code := strconv.FormatInt(first, 10)
	write("aliased.go", "package errorsprobe\n\nimport ec \"github.com/tjbdwanghaibo/roost-core/infra/base/errcode\"\n\nvar ErrAliased = ec.Define("+code+", \"aliased\", \"aliased import\")\n")
	next, err := NextID(root, m, "errcode", "")
	if err != nil {
		t.Fatal(err)
	}
	if next == first {
		t.Errorf("roost id next errcode handed out %d again although ec.Define(%d, …) (aliased import) already uses it", next, first)
	}

	// A second definition of the same code under the plain import: the
	// generator reports it, and so must the ID check.
	write("plain.go", "package errorsprobe\n\nimport \"github.com/tjbdwanghaibo/roost-core/infra/base/errcode\"\n\nvar ErrPlain = errcode.Define("+code+", \"plain\", \"plain import\")\n")
	if err := CheckIDs(root, m); err == nil || !strings.Contains(err.Error(), "duplicate errcode id "+code) {
		t.Errorf("roost id check with %d defined twice (aliased and plain import) = %v, want a duplicate", first, err)
	}
	if err := os.Remove(filepath.Join(dir, "plain.go")); err != nil {
		t.Fatal(err)
	}

	// Text that only looks like a definition is not one.
	write("lookalike.go", "package errorsprobe\n\n// Retired: errcode.Define("+code+", \"aliased_old\", \"x\")\nconst note = `errcode.Define("+code+", \"in_a_string\", \"x\")`\n")
	if err := CheckIDs(root, m); err != nil {
		t.Errorf("roost id check counted a comment / string as a definition: %v", err)
	}
}
