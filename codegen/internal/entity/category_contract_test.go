package entity

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGeneratedWiringImportsEveryPackageItQualifies(t *testing.T) {
	dir := t.TempDir()
	source, err := os.ReadFile(filepath.Join("testdata", "player.go"))
	if err != nil {
		t.Fatal(err)
	}
	// A category constant that lives in a business package, imported by the
	// source file and used only inside the category expression.
	body := strings.Replace(string(source),
		"//roost:entity entityKind=EntityKindPlayer",
		"//roost:entity entityKind=EntityKindPlayer category=view.EntityCategoryPlayer", 1)
	if body == string(source) {
		t.Fatal("fixture marker not found")
	}
	body = strings.Replace(body,
		`"github.com/tjbdwanghaibo/roost-core/framework/entity"`,
		"\"github.com/tjbdwanghaibo/roost-core/framework/entity\"\n\tview \"github.com/tjbdwanghaibo/cube/game/view\"", 1)
	body += "\nvar _ = view.EntityCategoryPlayer\n"
	if err := os.WriteFile(filepath.Join(dir, "player.go"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	entities, pkg, err := parseDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "player_gen_wire.go")
	if _, err := generate(entities[0], pkg, out, true); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, out, raw, parser.ParseComments)
	if err != nil {
		t.Fatalf("generated source does not parse: %v\n%s", err, raw)
	}
	imported := map[string]bool{}
	for _, spec := range file.Imports {
		name := ""
		if spec.Name != nil {
			name = spec.Name.Name
		} else {
			path := strings.Trim(spec.Path.Value, `"`)
			name = path[strings.LastIndex(path, "/")+1:]
		}
		imported[name] = true
	}
	// Locals shadow nothing here: the generated file declares no package-level
	// identifier that could be mistaken for a qualifier.
	declared := map[string]bool{}
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv == nil {
			declared[fn.Name.Name] = true
		}
	}
	missing := map[string]bool{}
	ast.Inspect(file, func(node ast.Node) bool {
		sel, ok := node.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		ident, ok := sel.X.(*ast.Ident)
		if !ok || ident.Obj != nil || declared[ident.Name] {
			return true
		}
		if !imported[ident.Name] {
			missing[ident.Name] = true
		}
		return true
	})
	if len(missing) != 0 {
		names := make([]string, 0, len(missing))
		for name := range missing {
			names = append(names, name)
		}
		t.Fatalf("generated wiring qualifies %v without importing it:\n%s", names, raw)
	}
	if !imported["view"] {
		t.Fatalf("the category's package was not imported:\n%s", raw)
	}
}

func TestEntityMarkerCategoryIsGeneratedDirectly(t *testing.T) {
	dir := t.TempDir()
	source, err := os.ReadFile(filepath.Join("testdata", "player.go"))
	if err != nil {
		t.Fatal(err)
	}
	body := strings.Replace(string(source),
		"//roost:entity entityKind=EntityKindPlayer",
		"//roost:entity entityKind=EntityKindPlayer category=entity.EntityCategoryPlayer", 1)
	if body == string(source) {
		t.Fatal("fixture marker not found; the replacement above did nothing")
	}
	if err := os.WriteFile(filepath.Join(dir, "player.go"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	entities, pkg, err := parseDir(dir)
	if err != nil {
		t.Fatalf("parseDir with category=: %v", err)
	}
	if len(entities) != 1 {
		t.Fatalf("entities = %d", len(entities))
	}
	if entities[0].Category != "entity.EntityCategoryPlayer" {
		t.Fatalf("parsed category = %q, want the marker's value", entities[0].Category)
	}

	out := filepath.Join(dir, "player_gen_wire.go")
	if _, err := generate(entities[0], pkg, out, true); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	generated := string(raw)
	if !strings.Contains(generated, "Category: entity.EntityCategoryPlayer") {
		t.Errorf("generated wiring does not carry the declared category:\n%s", generated)
	}
	if strings.Contains(generated, "MustEntityCategoryOfKind") {
		t.Errorf("generated wiring still resolves the category at run time:\n%s", generated)
	}
}

// Without category= the generated wiring keeps the run-time lookup, so a
// project that has not moved its markers over is unaffected.
func TestEntityMarkerWithoutCategoryKeepsTheRuntimeLookup(t *testing.T) {
	entities, pkg, err := parseDir(filepath.Join("testdata"))
	if err != nil {
		t.Fatal(err)
	}
	if entities[0].Category != "" {
		t.Fatalf("fixture without category= parsed category %q", entities[0].Category)
	}
	out := filepath.Join(t.TempDir(), "player_gen_wire.go")
	if _, err := generate(entities[0], pkg, out, true); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "MustEntityCategoryOfKind") {
		t.Error("a marker without category= must keep resolving at run time")
	}
}

// A category= value has to look like a category constant expression, so a typo
// fails the generator instead of the consumer's compiler.
func TestEntityMarkerRefusesAMalformedCategory(t *testing.T) {
	for _, bad := range []string{"1", "\"player\"", "entity.", "Player Category"} {
		if err := validateMarkerValues(map[string]string{"category": bad}); err == nil {
			t.Errorf("category=%q was accepted", bad)
		}
	}
	for _, good := range []string{"entity.EntityCategoryOther", "view.EntityCategoryPlayer", "EntityCategoryWorld"} {
		if err := validateMarkerValues(map[string]string{"category": good}); err != nil {
			t.Errorf("category=%q must be accepted: %v", good, err)
		}
	}
}
