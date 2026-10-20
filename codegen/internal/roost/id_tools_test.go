package roost

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
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

// D9（维护者 2026-10-16 决定 B）：DAO 文档的 _id 是编码后的 EntityID，运维按玩家 ID 查不到。
// `roost id entity` 双向换算：完整 EntityID → unique / kind / category 低两位 / remote，以及反过来。
// 示例来自登记：Player（kind 1，category Other=5，低两位存 5&3=1）的 9001 存成 9217029。
func TestRoostIDEntityConvertsBothWays(t *testing.T) {
	t.Parallel()
	target := filepath.Join(t.TempDir(), "planet")
	if _, _, err := NewProject(NewOptions{Name: "planet", Module: "example.com/planet", Out: target, Features: []string{"entity", "dao"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := Add(target, AddOptions{Kind: "entity", Name: "Player"}); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) string {
		t.Helper()
		var stdout, stderr bytes.Buffer
		if err := runID(append(args, "--root", target), &stdout, &stderr); err != nil {
			t.Fatalf("roost id %v: %v\n%s", args, err, stderr.String())
		}
		return stdout.String()
	}
	decoded := run("entity", "9217029")
	for _, want := range []string{"entity_id: 9217029", "unique_id: 9001", "kind: 1 (Player)", "category_bits: 1", "remote: false"} {
		if !strings.Contains(decoded, want) {
			t.Errorf("decode 9217029 missing %q:\n%s", want, decoded)
		}
	}
	for _, args := range [][]string{
		{"entity", "--unique", "9001", "--entity", "Player"},
		{"entity", "--unique", "9001", "--kind", "1", "--category", "5"},
	} {
		if got := run(args...); !strings.Contains(got, "entity_id: 9217029") {
			t.Errorf("encode %v:\n%s", args, got)
		}
	}
	// Remote 位在最高有效位之下（bit 62）：remote 托管的 kind 编码后带它，解码报告它。
	remote := run("entity", "--unique", "7", "--kind", "3", "--category", "1", "--remote")
	want := strconv.FormatInt(int64(1)<<62|7<<10|3<<2|1, 10)
	if !strings.Contains(remote, "entity_id: "+want) {
		t.Errorf("remote encode: want %s:\n%s", want, remote)
	}
	if got := run("entity", want); !strings.Contains(got, "unique_id: 7") || !strings.Contains(got, "remote: true") {
		t.Errorf("remote decode:\n%s", got)
	}
}

// 换算命令里的位布局是 framework/entity/idgen.go 的副本（生成器不 import 运行时）：直接读那份源码的常量比对，
// 布局一改这里就红。
func TestRoostIDEntityLayoutMatchesTheFramework(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), filepath.Join("..", "..", "..", "framework", "entity", "idgen.go"), nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	valuesByName := map[string]int64{}
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			for i, name := range spec.(*ast.ValueSpec).Names {
				if exprs := spec.(*ast.ValueSpec).Values; i < len(exprs) {
					if literal, ok := exprs[i].(*ast.BasicLit); ok && literal.Kind == token.INT {
						v, _ := strconv.ParseInt(literal.Value, 0, 64)
						valuesByName[name.Name] = v
					}
				}
			}
		}
	}
	for name, want := range map[string]int64{
		"EntityCategoryBits": entityIDCategoryBits, "EntityKindBits": entityIDKindBits,
		"EntityRemoteBits": entityIDRemoteBits, "UniqueIDBits": entityIDUniqueBits,
	} {
		got, ok := valuesByName[name]
		if !ok {
			t.Fatalf("framework/entity/idgen.go no longer declares %s as an integer literal", name)
		}
		if got != want {
			t.Errorf("%s: framework %d, roost id %d", name, got, want)
		}
	}
}

// DAO 骨架写固定的逻辑库名 db=main（D9 修订版 A），物理库名在生成配置的 dataengine.databases 里按工程名派生：
// 同一 Mongo 上的两个工程开箱不写同一个库，代码里也不出现部署相关的名字。
func TestDAOSkeletonDatabaseFollowsTheProjectName(t *testing.T) {
	t.Parallel()
	target := filepath.Join(t.TempDir(), "planet")
	if _, _, err := NewProject(NewOptions{Name: "planet", Module: "example.com/planet", Out: target, Features: []string{"entity", "dao"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := Add(target, AddOptions{Kind: "entity", Name: "Player"}); err != nil {
		t.Fatal(err)
	}
	if _, err := Add(target, AddOptions{Kind: "dao", Name: "Player", Entity: "Player"}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(target, "db", "def", "player.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "//roost:dao coll=player db=main\n") {
		t.Fatalf("DAO skeleton does not use the logical database main:\n%s", raw)
	}
	config, err := os.ReadFile(filepath.Join(target, "configs", "service", "config.game.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(config), "  databases: {main: planet}\n") {
		t.Fatalf("generated config does not map the logical database main to the project database:\n%s", config)
	}
	for project, want := range map[string]string{
		"planet":                       "planet",
		"star_wars":                    "star_wars",
		"admin":                        "admin_data", // Mongo 系统库
		"local":                        "local_data",
		"config":                       "config_data",
		strings.Repeat("a", 51) + "_b": strings.Repeat("a", 51), // 52 字节处截断后去掉末尾 "_"
		strings.Repeat("x", 60):        strings.Repeat("x", 52),
	} {
		if got := daoDatabaseName(project); got != want {
			t.Errorf("daoDatabaseName(%q) = %q, want %q", project, got, want)
		}
	}
}
