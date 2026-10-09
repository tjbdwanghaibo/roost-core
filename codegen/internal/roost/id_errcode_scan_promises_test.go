package roost

// RR-20261005-NC-73（RR-20261005-NC-63 残余，N07 交给 N08）：`roost id next errcode`、
// `roost add errcode`（NextID）、`roost id check` / doctor（CheckIDs）用正则
// `errcode\.Define\(\s*([0-9]+)\s*,` 找已占用的错误码，而 errcode 生成器（`roost generate`）
// 自 NC-63 起按 AST 读取：认别名 / 点导入，不认注释和字符串里的文字。两边口径不同：
//   - 别名导入的 `ec.Define(100000, …)` 不进占用表，`roost add errcode` 再发 100000，
//     add 成功、id check 报 valid，直到 generate 才报 duplicate errcode；
//   - 注释或字符串里的 `errcode.Define(100003, …` 被当成占用，id check 误报重复。
// 承诺：ID 工具与生成器看到同一组错误码定义。

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

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
