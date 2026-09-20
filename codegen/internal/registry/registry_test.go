package registry

import (
	"go/parser"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeProject lays out a throwaway module so the scanner runs against real
// files rather than a fake filesystem.
func writeProject(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for rel, content := range files {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// The whole point of the aggregate is execution order, and it must come from
// the phase list rather than from the order files happen to be walked in.
func TestScanOrdersByPhaseThenOrderThenPath(t *testing.T) {
	root := writeProject(t, map[string]string{
		"game/z/z.go": "package z\n\n//roost:register phase=entity\nfunc RegisterEntity() {}\n",
		"game/a/a.go": "package a\n\n//roost:register phase=component\nfunc RegisterComponent() {}\n",
		"game/b/b.go": "package b\n\n//roost:register phase=component order=-10\nfunc RegisterScheduler() {}\n",
		"game/c/c.go": "package c\n\n//roost:register phase=kind\nfunc RegisterKinds() {}\n",
	})
	got, err := Scan(root, "example.com/planet")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"example.com/planet/game/c.RegisterKinds",     // phase kind
		"example.com/planet/game/b.RegisterScheduler", // phase component, order -10
		"example.com/planet/game/a.RegisterComponent", // phase component, order 0
		"example.com/planet/game/z.RegisterEntity",    // phase entity
	}
	if len(got) != len(want) {
		t.Fatalf("scanned %d registrations, want %d: %v", len(got), len(want), qualifiedNames(got))
	}
	for i, qualified := range want {
		if actual := got[i].ImportPath + "." + got[i].Func; actual != qualified {
			t.Fatalf("position %d: got %s want %s\nfull order: %v", i, actual, qualified, qualifiedNames(got))
		}
	}
}

func qualifiedNames(registrations []Registration) []string {
	out := make([]string, 0, len(registrations))
	for _, registration := range registrations {
		out = append(out, registration.ImportPath+"."+registration.Func)
	}
	return out
}

// Order must not depend on walk order, which varies by filesystem. Scanning
// the same project repeatedly has to produce the identical sequence.
func TestScanIsDeterministicAcrossRuns(t *testing.T) {
	files := map[string]string{}
	for _, name := range []string{"alpha", "beta", "gamma", "delta", "epsilon", "zeta"} {
		files["game/components/"+name+"/"+name+".go"] =
			"package " + name + "\n\n//roost:register phase=component\nfunc RegisterComponent() {}\n"
	}
	root := writeProject(t, files)

	var first []string
	for run := 0; run < 10; run++ {
		got, err := Scan(root, "example.com/planet")
		if err != nil {
			t.Fatal(err)
		}
		names := qualifiedNames(got)
		if run == 0 {
			first = names
			continue
		}
		if strings.Join(names, ",") != strings.Join(first, ",") {
			t.Fatalf("scan order varied between runs:\n run 0: %v\n run %d: %v", first, run, names)
		}
	}
}

// A marker the generator cannot honour must fail the build, not be skipped:
// a silently dropped registration is a nil dependency much later.
func TestScanRejectsUnusableMarkers(t *testing.T) {
	for _, testCase := range []struct {
		label  string
		source string
		want   string
	}{
		{"missing phase", "//roost:register\nfunc RegisterX() {}", "missing phase="},
		{"empty phase", "//roost:register phase=\nfunc RegisterX() {}", "missing phase="},
		{"unknown phase", "//roost:register phase=whenever\nfunc RegisterX() {}", "unknown phase"},
		{"unknown option", "//roost:register phase=entity mode=lazy\nfunc RegisterX() {}", "unknown marker option"},
		{"non-integer order", "//roost:register phase=entity order=soon\nfunc RegisterX() {}", "non-integer order"},
		{"takes parameters", "//roost:register phase=entity\nfunc RegisterX(n int) {}", "takes parameters"},
		{"unexported", "//roost:register phase=entity\nfunc registerX() {}", "unexported function"},
		{"two results", "//roost:register phase=entity\nfunc RegisterX() (int, error) { return 0, nil }", "returns 2 values"},
		{"non-error result", "//roost:register phase=entity\nfunc RegisterX() int { return 0 }", "non-error value"},
		{"on a method", "type T struct{}\n\n//roost:register phase=entity\nfunc (T) RegisterX() {}", "on method"},
	} {
		root := writeProject(t, map[string]string{
			"game/x/x.go": "package x\n\n" + testCase.source + "\n",
		})
		_, err := Scan(root, "example.com/planet")
		if err == nil {
			t.Fatalf("%s: accepted", testCase.label)
		}
		if !strings.Contains(err.Error(), testCase.want) {
			t.Fatalf("%s: error %q does not contain %q", testCase.label, err, testCase.want)
		}
	}
	// The valid phases must be listed, or the author cannot fix the mistake.
	root := writeProject(t, map[string]string{
		"game/x/x.go": "package x\n\n//roost:register phase=whenever\nfunc RegisterX() {}\n",
	})
	_, err := Scan(root, "example.com/planet")
	if err == nil || !strings.Contains(err.Error(), "component") {
		t.Fatalf("the error does not list the valid phases: %v", err)
	}
}

// Test files and vendored code must not contribute registrations: a fake
// registration in a _test.go file would be compiled into production startup.
func TestScanIgnoresTestdataVendorAndTestFiles(t *testing.T) {
	root := writeProject(t, map[string]string{
		"game/real/real.go":       "package real\n\n//roost:register phase=entity\nfunc RegisterEntity() {}\n",
		"game/real/real_test.go":  "package real\n\n//roost:register phase=entity\nfunc RegisterFromTest() {}\n",
		"game/real/testdata/x.go": "package testdata\n\n//roost:register phase=entity\nfunc RegisterFromTestdata() {}\n",
		"vendor/dep/dep.go":       "package dep\n\n//roost:register phase=entity\nfunc RegisterFromVendor() {}\n",
		".hidden/hidden.go":       "package hidden\n\n//roost:register phase=entity\nfunc RegisterFromHidden() {}\n",
	})
	got, err := Scan(root, "example.com/planet")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Func != "RegisterEntity" {
		t.Fatalf("scan picked up excluded files: %v", qualifiedNames(got))
	}
}

// The generated file must compile. This is the assertion that catches the
// class of bug where the template emits a call to fmt without importing it.
//
// fmt and the entity package are now unconditional: the aggregate always ends
// with an entity.ValidateEntityRegistry() call whose error it wraps, so there
// is no longer a shape of project that needs neither (M-05).
func TestGeneratedAggregateParsesAndImportsWhatItUses(t *testing.T) {
	for _, testCase := range []struct {
		label         string
		registrations []Registration
	}{
		{"empty project", nil},
		{
			"no error returns",
			[]Registration{{ImportPath: "example.com/p/game/a", Func: "RegisterComponent", Phase: "component"}},
		},
		{
			"one error return",
			[]Registration{{ImportPath: "example.com/p/configs/generated", Func: "RegisterTables", Phase: "config", ReturnsError: true}},
		},
	} {
		content, err := render("example.com/p", testCase.registrations)
		if err != nil {
			t.Fatalf("%s: %v", testCase.label, err)
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, "generated.go", content, parser.ParseComments)
		if err != nil {
			t.Fatalf("%s: generated source does not parse: %v\n%s", testCase.label, err, content)
		}
		imported := map[string]bool{}
		for _, spec := range file.Imports {
			imported[strings.Trim(spec.Path.Value, `"`)] = true
		}
		for _, always := range []string{"fmt", "sync", "github.com/tjbdwanghaibo/roost-core/entity"} {
			if !imported[always] {
				t.Fatalf("%s: %s is always used by RegisterAll but was not imported\n%s", testCase.label, always, content)
			}
		}
		for _, registration := range testCase.registrations {
			if !imported[registration.ImportPath] {
				t.Fatalf("%s: %s is called but not imported\n%s", testCase.label, registration.ImportPath, content)
			}
		}
	}
}

// Two packages with the same base name is normal in a game project
// (game/entities/player and game/components/player), and the aliases must not
// collide or the generated file will not compile.
func TestRenderDisambiguatesSameBaseNamePackages(t *testing.T) {
	content, err := render("example.com/p", []Registration{
		{ImportPath: "example.com/p/game/entities/player", Func: "RegisterEntity", Phase: "entity"},
		{ImportPath: "example.com/p/game/components/player", Func: "RegisterComponent", Phase: "component"},
		{ImportPath: "example.com/p/game/view/player", Func: "RegisterView", Phase: "pre"},
	})
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "generated.go", content, parser.ParseComments)
	if err != nil {
		t.Fatalf("generated source does not parse: %v\n%s", err, content)
	}
	aliases := map[string]bool{}
	for _, spec := range file.Imports {
		if spec.Name == nil {
			continue
		}
		if aliases[spec.Name.Name] {
			t.Fatalf("duplicate import alias %q\n%s", spec.Name.Name, content)
		}
		aliases[spec.Name.Name] = true
	}
	if len(aliases) != 4 { // three project packages plus sync/fmt have no alias
		t.Logf("aliases: %v", aliases)
	}
}

// A function marked twice would be called twice. The leaf functions carry
// their own sync.Once so it would be harmless today, but it is a mistake and
// the generator should say so rather than emit a duplicate call.
func TestScanRejectsDuplicateMarkersOnOneFunction(t *testing.T) {
	root := writeProject(t, map[string]string{
		"game/x/x.go": "package x\n\n//roost:register phase=entity\nfunc RegisterEntity() {}\n",
		"game/x/y.go": "package x\n\n//roost:register phase=component\nfunc RegisterEntity() {}\n",
	})
	_, err := Scan(root, "example.com/planet")
	if err == nil || !strings.Contains(err.Error(), "marked twice") {
		t.Fatalf("duplicate marker accepted or misreported: %v", err)
	}
}

func TestScanRequiresModulePath(t *testing.T) {
	root := writeProject(t, map[string]string{"x.go": "package main\n"})
	if _, err := Scan(root, "  "); err == nil {
		t.Fatal("Scan accepted an empty module path")
	}
}

// Generate must not rewrite an unchanged file: roost generate reports what it
// touched, and a spurious rewrite makes every run look like a change.
func TestGenerateSkipsUnchangedFile(t *testing.T) {
	root := t.TempDir()
	registrations := []Registration{{ImportPath: "example.com/p/game/a", Func: "RegisterComponent", Phase: "component"}}

	changed, err := Generate(root, "example.com/p", registrations)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("first Generate reported no change")
	}
	changed, err = Generate(root, "example.com/p", registrations)
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Fatal("second Generate rewrote an identical file")
	}

	registrations = append(registrations, Registration{ImportPath: "example.com/p/game/b", Func: "RegisterEntity", Phase: "entity"})
	changed, err = Generate(root, "example.com/p", registrations)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("Generate did not rewrite after the registration set changed")
	}
	if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(OutputPath))); err != nil {
		t.Fatalf("aggregate was not written to %s: %v", OutputPath, err)
	}
}

// Generating alongside a hand-written aggregator would leave two lists and no
// way for the next person to know which to edit. The guard must fire on
// content, not merely on the path existing, and must name what to remove.
func TestRunRefusesWhenAHandWrittenAggregatorIsStillPresent(t *testing.T) {
	for _, testCase := range []struct {
		label string
		path  string
		body  string
		fires bool
	}{
		{
			label: "project-wide aggregator",
			path:  "game/bootstrap/register.go",
			body:  "package bootstrap\n\nfunc RegisterAll() {}\n",
			fires: true,
		},
		{
			label: "entity aggregator",
			path:  "game/entities/register/register.go",
			body:  "package register\n\nfunc RegisterEntities() {}\n",
			fires: true,
		},
		{
			label: "component aggregator",
			path:  "game/components/register/register.go",
			body:  "package register\n\nfunc RegisterComponents() {}\n",
			fires: true,
		},
		{
			label: "old nest aggregate location",
			path:  "game/bootstrap/nest.go",
			body:  "package bootstrap\n\nfunc RegisterNestHandlers() {}\n",
			fires: true,
		},
		{
			// The same path may hold unrelated code after the migration; only
			// the aggregator content itself is the signal.
			label: "same path without the aggregator function",
			path:  "game/bootstrap/register.go",
			body:  "package bootstrap\n\nfunc Helper() {}\n",
			fires: false,
		},
	} {
		root := writeProject(t, map[string]string{testCase.path: testCase.body})
		err := Run(root, "example.com/planet", io.Discard)
		if testCase.fires {
			if err == nil {
				t.Fatalf("%s: Run proceeded alongside a hand-written aggregator", testCase.label)
			}
			for _, want := range []string{testCase.path, OutputPath, "//roost:register", "roost generate"} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("%s: guard message is missing %q:\n%s", testCase.label, want, err)
				}
			}
			continue
		}
		if err != nil {
			t.Fatalf("%s: Run refused for the wrong reason: %v", testCase.label, err)
		}
	}
}

// Run must produce a compiling aggregate for a project that has no markers at
// all, because bootstrap.New calls RegisterAll unconditionally.
func TestRunWritesAValidAggregateForAProjectWithNoMarkers(t *testing.T) {
	root := writeProject(t, map[string]string{"main.go": "package main\n\nfunc main() {}\n"})
	if err := Run(root, "example.com/planet", io.Discard); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(OutputPath)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parser.ParseFile(token.NewFileSet(), "generated.go", raw, parser.ParseComments); err != nil {
		t.Fatalf("the empty aggregate does not parse: %v\n%s", err, raw)
	}
	if !strings.Contains(string(raw), "func RegisterAll() error") {
		t.Fatalf("the empty aggregate does not expose RegisterAll:\n%s", raw)
	}
}

// The generated nest aggregate lives in the registry package itself. A package
// cannot import itself, so a same-package registration is called unqualified
// and must not appear in the import block.
func TestRenderCallsSamePackageRegistrationsUnqualified(t *testing.T) {
	content, err := render("example.com/p", []Registration{
		{ImportPath: "example.com/p/internal/registry", Func: "RegisterNestHandlers", Phase: "nest"},
		{ImportPath: "example.com/p/game/a", Func: "RegisterComponent", Phase: "component"},
	})
	if err != nil {
		t.Fatal(err)
	}
	text := string(content)
	if strings.Contains(text, `"example.com/p/internal/registry"`) {
		t.Fatalf("aggregate imports its own package:\n%s", text)
	}
	if !strings.Contains(text, "\tRegisterNestHandlers()\n") {
		t.Fatalf("same-package registration is not called unqualified:\n%s", text)
	}
	if _, err := parser.ParseFile(token.NewFileSet(), "generated.go", content, parser.ParseComments); err != nil {
		t.Fatalf("generated source does not parse: %v\n%s", err, content)
	}
}
