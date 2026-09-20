package entity

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// U-0160 · C2 · RR-20260909-04：同一个包里放两个 //roost:entity 实体，生成器此前给每个文件都
// 声明包级 registerEntityOnce / RegisterEntity，生成成功、消费者编译报 redeclared。现在每个实体
// 的注册函数按实体名命名，包里只有一个聚合的 RegisterEntity（带 //roost:register 标记）调用它们。
// 这里不编译消费者（要拉 core），而是解析生成物的包级声明：重名即编译失败的直接原因。
func TestGenerateGivesEachEntityInAPackageDistinctRegistrationSymbols(t *testing.T) {
	dir := t.TempDir()
	player, err := os.ReadFile(filepath.Join("testdata", "player.go"))
	if err != nil {
		t.Fatal(err)
	}
	// A second, unrelated entity in the same package: derived from the fixture
	// with its own type name, kind, category and DAO collections.
	npc := strings.NewReplacer(
		"Player", "Npc", "player", "npc",
		"EntityCategory = 1", "EntityCategory = 2",
		"EntityKind     = 1", "EntityKind     = 2",
		"CompTypeBag    entity.ComponentType = 1", "CompTypeBag    entity.ComponentType = 11",
		"CompTypeBattle entity.ComponentType = 2", "CompTypeBattle entity.ComponentType = 12",
		"MailDao", "NpcMailDao", `"mails"`, `"npc_mails"`,
	).Replace(string(player))
	// The fixture's component type constants and registerEntityKinds helper are
	// package-level too; the second file must not redeclare what it shares.
	npc = strings.Replace(npc, "var _ = registerEntityKinds()", "var _ = registerNpcEntityKinds()", 1)
	npc = strings.Replace(npc, "func registerEntityKinds() struct{} {", "func registerNpcEntityKinds() struct{} {", 1)
	for _, f := range []struct{ name, body string }{{"player.go", string(player)}, {"npc.go", npc}} {
		if err := os.WriteFile(filepath.Join(dir, f.name), []byte(f.body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	if err := Run([]string{"-dir", dir}, io.Discard); err != nil {
		t.Fatalf("generate two entities in one package: %v", err)
	}
	generated, err := filepath.Glob(filepath.Join(dir, "*_gen_wire.go"))
	if err != nil || len(generated) != 2 {
		t.Fatalf("generated files = %v (%v), want two wiring files", generated, err)
	}

	fset := token.NewFileSet()
	declaredIn := map[string]string{}
	registerEntityBodies := 0
	var aggregateCalls []string
	for _, path := range generated {
		file, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if err != nil {
			t.Fatalf("generated %s does not parse: %v", filepath.Base(path), err)
		}
		for _, decl := range file.Decls {
			for _, name := range topLevelNames(decl) {
				if prev, dup := declaredIn[name]; dup {
					t.Errorf("%s redeclared in %s (other declaration in %s) — the consumer would fail to compile", name, filepath.Base(path), prev)
				}
				declaredIn[name] = filepath.Base(path)
			}
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || fn.Name.Name != "RegisterEntity" {
				continue
			}
			registerEntityBodies++
			hasMarker := false
			if fn.Doc != nil {
				for _, c := range fn.Doc.List {
					hasMarker = hasMarker || strings.Contains(c.Text, "//roost:register phase=entity")
				}
			}
			if !hasMarker {
				t.Errorf("RegisterEntity in %s lost its //roost:register marker", filepath.Base(path))
			}
			for _, stmt := range fn.Body.List {
				if call, ok := stmt.(*ast.ExprStmt); ok {
					if c, ok := call.X.(*ast.CallExpr); ok {
						if id, ok := c.Fun.(*ast.Ident); ok {
							aggregateCalls = append(aggregateCalls, id.Name)
						}
					}
				}
			}
		}
	}
	if registerEntityBodies != 1 {
		t.Fatalf("package declares RegisterEntity %d times, want exactly one aggregate entry point", registerEntityBodies)
	}
	for _, want := range []string{"registerNpcEntity", "registerPlayerEntity"} {
		if _, ok := declaredIn[want]; !ok {
			t.Errorf("per-entity registration %s not generated; declared: %v", want, keys(declaredIn))
		}
		found := false
		for _, call := range aggregateCalls {
			found = found || call == want
		}
		if !found {
			t.Errorf("RegisterEntity does not call %s; calls: %v", want, aggregateCalls)
		}
	}
}

func topLevelNames(decl ast.Decl) []string {
	switch d := decl.(type) {
	case *ast.FuncDecl:
		if d.Recv == nil {
			return []string{d.Name.Name}
		}
	case *ast.GenDecl:
		var names []string
		for _, spec := range d.Specs {
			switch s := spec.(type) {
			case *ast.ValueSpec:
				for _, n := range s.Names {
					if n.Name != "_" {
						names = append(names, n.Name)
					}
				}
			case *ast.TypeSpec:
				names = append(names, s.Name.Name)
			}
		}
		return names
	}
	return nil
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
