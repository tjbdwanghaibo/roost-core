package roost

import (
	_ "embed"
	"errors"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// consolidationMap is the package relocation map of the five-repositories-to-
// three consolidation (core v1.14.0 / kit v1.13.0). It is the single source of
// truth shared with the migration batches; `roost project upgrade
// --consolidate` applies it to a business project.
//
//go:embed migration/consolidation_imports.yaml
var consolidationMapYAML []byte

type consolidationMap struct {
	Schema         int                                 `yaml:"schema"`
	Boundary       struct{ Core, Kit, Codegen string } `yaml:"boundary"`
	RemovedModules []string                            `yaml:"removed_modules"`
	Split          []struct {
		Kit      string   `yaml:"kit"`
		Core     string   `yaml:"core"`
		Package  string   `yaml:"package"`
		KitKeeps []string `yaml:"kit_keeps"`
	} `yaml:"split"`
	Move []struct {
		From     string   `yaml:"from"`
		To       string   `yaml:"to"`
		KitKeeps []string `yaml:"kit_keeps"`
	} `yaml:"move"`
	Keep    []string                    `yaml:"keep"`
	Skill   []struct{ From, To string } `yaml:"skill"`
	Service []struct{ From, To string } `yaml:"service"`
}

// relocation is what the map says about one old import path.
type relocation struct {
	to       string          // new import path for symbols that moved
	pkgName  string          // package name at the new path when it differs from the base name
	kitKeeps map[string]bool // symbols that stay at the old path (split packages)
}

func loadConsolidationMap() (map[string]relocation, consolidationMap, error) {
	var m consolidationMap
	if err := yaml.Unmarshal(consolidationMapYAML, &m); err != nil {
		return nil, m, fmt.Errorf("consolidation map: %w", err)
	}
	table := make(map[string]relocation)
	add := func(from, to, pkgName string, keeps []string) {
		r := relocation{to: to, pkgName: pkgName, kitKeeps: make(map[string]bool)}
		for _, k := range keeps {
			r.kitKeeps[k] = true
		}
		table[from] = r
	}
	for _, s := range m.Split {
		add(s.Kit, s.Core, s.Package, s.KitKeeps)
	}
	for _, mv := range m.Move {
		add(mv.From, mv.To, "", mv.KitKeeps)
	}
	for _, s := range m.Skill {
		add(s.From, s.To, "", nil)
	}
	for _, s := range m.Service {
		add(s.From, s.To, "", nil)
	}
	return table, m, nil
}

// ConsolidateResult summarises what the rewrite touched.
type ConsolidateResult struct {
	Files      []string // Go files whose imports were rewritten
	GoMod      bool     // go.mod lost the removed modules / gained the boundary versions
	Manifest   bool     // roost.yaml lost versions.skill / versions.service
	Unresolved []string // "file: import" pairs on removed modules the map does not know
}

// ConsolidateProject rewrites a business project from the pre-consolidation
// layout (roost-kit implementations, roost-skill, roost-service) to the
// consolidated one. It is idempotent: a project already on the new layout is
// reported as untouched. With dryRun it only reports.
func ConsolidateProject(root string, dryRun bool, stdout io.Writer) (ConsolidateResult, error) {
	table, m, err := loadConsolidationMap()
	if err != nil {
		return ConsolidateResult{}, err
	}
	var result ConsolidateResult
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			name := d.Name()
			if p != root && (name == ".git" || name == "vendor" || name == "node_modules" || strings.HasPrefix(name, ".")) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") {
			return nil
		}
		changed, unresolved, err := consolidateFile(p, table, m.RemovedModules, dryRun)
		if err != nil {
			return fmt.Errorf("%s: %w", p, err)
		}
		rel := relativeSlash(root, p)
		for _, u := range unresolved {
			result.Unresolved = append(result.Unresolved, rel+": "+u)
		}
		if changed {
			result.Files = append(result.Files, rel)
		}
		return nil
	})
	if err != nil {
		return result, err
	}
	sort.Strings(result.Files)
	if len(result.Unresolved) > 0 {
		return result, fmt.Errorf("consolidate: %d import(s) point at removed modules but are not in the relocation map:\n  %s", len(result.Unresolved), strings.Join(result.Unresolved, "\n  "))
	}
	result.GoMod, err = consolidateGoMod(filepath.Join(root, "go.mod"), m, dryRun)
	if err != nil {
		return result, err
	}
	result.Manifest, err = consolidateManifest(root, dryRun)
	if err != nil {
		return result, err
	}
	if stdout != nil {
		verb := "rewrote"
		if dryRun {
			verb = "would rewrite"
		}
		fmt.Fprintf(stdout, "consolidate: %s %d Go file(s)", verb, len(result.Files))
		if result.GoMod {
			fmt.Fprintf(stdout, ", go.mod")
		}
		if result.Manifest {
			fmt.Fprintf(stdout, ", %s", ManifestName)
		}
		fmt.Fprintln(stdout)
		for _, f := range result.Files {
			fmt.Fprintf(stdout, "  %s\n", f)
		}
	}
	return result, nil
}

// consolidateFile rewrites one Go file. Split packages are decided per
// selector: symbols the map lists as staying in kit keep the old import, every
// other symbol moves to the core path; a file using both ends up with both
// imports.
func consolidateFile(p string, table map[string]relocation, removed []string, dryRun bool) (bool, []string, error) {
	src, err := os.ReadFile(p)
	if err != nil {
		return false, nil, err
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, p, src, parser.ParseComments)
	if err != nil {
		return false, nil, err
	}
	type edit struct {
		start, end int
		text       string
	}
	var edits []edit
	var unresolved []string
	var extraImports []string     // fully rendered import specs to add
	declared := map[string]bool{} // identifiers declared or imported in the file, to pick fresh aliases
	for _, imp := range file.Imports {
		if imp.Name != nil {
			declared[imp.Name.Name] = true
		} else if pth, err := strconv.Unquote(imp.Path.Value); err == nil {
			declared[path.Base(pth)] = true
		}
	}
	for _, obj := range file.Scope.Objects {
		declared[obj.Name] = true
	}
	fresh := func(base string) string {
		name := base
		for i := 2; declared[name]; i++ {
			name = fmt.Sprintf("%s%d", base, i)
		}
		declared[name] = true
		return name
	}
	for _, imp := range file.Imports {
		oldPath, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			return false, nil, err
		}
		rel, mapped := table[oldPath]
		if !mapped {
			for _, mod := range removed {
				if oldPath == mod || strings.HasPrefix(oldPath, mod+"/") {
					unresolved = append(unresolved, oldPath)
				}
			}
			continue
		}
		// The name this file uses to refer to the package.
		alias := path.Base(oldPath)
		if imp.Name != nil {
			alias = imp.Name.Name
		}
		// Collect selector uses alias.Sym.
		var kitUses, coreUses []*ast.SelectorExpr
		ast.Inspect(file, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			id, ok := sel.X.(*ast.Ident)
			if !ok || id.Name != alias || id.Obj != nil {
				return true
			}
			if rel.kitKeeps[sel.Sel.Name] {
				kitUses = append(kitUses, sel)
			} else {
				coreUses = append(coreUses, sel)
			}
			return true
		})
		newBase := path.Base(rel.to)
		if rel.pkgName != "" {
			newBase = rel.pkgName
		}
		switch {
		case len(kitUses) == 0:
			// Everything moves: repoint the import in place.
			edits = append(edits, edit{fset.Position(imp.Path.Pos()).Offset, fset.Position(imp.Path.End()).Offset, strconv.Quote(rel.to)})
			// Keep the identifier this file uses: add an explicit alias when the
			// new package name differs from it.
			if imp.Name == nil && newBase != alias {
				edits = append(edits, edit{fset.Position(imp.Path.Pos()).Offset, fset.Position(imp.Path.Pos()).Offset, alias + " "})
			}
		case len(coreUses) == 0:
			// Only Mod glue used: the kit import stays as it is.
		default:
			// Mixed: kit import stays, moved symbols get a second import.
			coreAlias := fresh("core" + path.Base(oldPath))
			extraImports = append(extraImports, coreAlias+" "+strconv.Quote(rel.to))
			for _, sel := range coreUses {
				edits = append(edits, edit{fset.Position(sel.X.Pos()).Offset, fset.Position(sel.X.End()).Offset, coreAlias})
			}
		}
	}
	if len(edits) == 0 && len(extraImports) == 0 {
		return false, unresolved, nil
	}
	if len(extraImports) > 0 {
		first := file.Imports[0]
		var decl *ast.GenDecl
		for _, d := range file.Decls {
			gen, ok := d.(*ast.GenDecl)
			if !ok || gen.Tok != token.IMPORT {
				continue
			}
			for _, spec := range gen.Specs {
				if spec == first {
					decl = gen
				}
			}
		}
		if decl != nil && !decl.Lparen.IsValid() {
			// A single-line `import "x"` has no block to extend: add a second,
			// parenthesized import declaration right after it (RR-20260908-03 —
			// splicing a bare spec after the first one produced `coreredis "…"`
			// at top level, which does not parse).
			end := fset.Position(decl.End()).Offset
			edits = append(edits, edit{end, end, "\nimport (\n\t" + strings.Join(extraImports, "\n\t") + "\n)"})
		} else {
			// Insert after the first import spec's line, inside the import block.
			end := fset.Position(first.End()).Offset
			edits = append(edits, edit{end, end, "\n\t" + strings.Join(extraImports, "\n\t")})
		}
	}
	sort.Slice(edits, func(i, j int) bool {
		if edits[i].start != edits[j].start {
			return edits[i].start > edits[j].start
		}
		return edits[i].end > edits[j].end
	})
	out := []byte(string(src))
	for _, e := range edits {
		out = append(out[:e.start], append([]byte(e.text), out[e.end:]...)...)
	}
	formatted, err := format.Source(out)
	if err != nil {
		return false, unresolved, fmt.Errorf("rewritten source does not format: %w", err)
	}
	if dryRun {
		return true, unresolved, nil
	}
	return true, unresolved, os.WriteFile(p, formatted, 0o644)
}

var consolidateRequireLine = regexp.MustCompile(`(?m)^\s*github\.com/tjbdwanghaibo/(roost-skill|roost-service)\s+\S+[^\n]*\n`)

// consolidateGoMod drops the folded-in modules from go.mod. It deliberately
// leaves the core / kit versions alone: the caller's dependency resolution
// (`go get` with the manifest's policy) moves them, and writing a boundary
// release that is not published yet would make that very `go get` fail
// ("unknown revision"). A project whose imports were rewritten but whose
// versions stay below the boundary simply fails to build until it resolves.
func consolidateGoMod(goMod string, _ consolidationMap, dryRun bool) (bool, error) {
	raw, err := os.ReadFile(goMod)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	text := consolidateRequireLine.ReplaceAllString(string(raw), "")
	text = regexp.MustCompile(`(?m)^require github\.com/tjbdwanghaibo/(roost-skill|roost-service)\s+\S+[^\n]*\n`).ReplaceAllString(text, "")
	if text == string(raw) {
		return false, nil
	}
	if dryRun {
		return true, nil
	}
	return true, os.WriteFile(goMod, []byte(text), 0o644)
}

// consolidateManifest drops versions.skill / versions.service from roost.yaml.
func consolidateManifest(root string, dryRun bool) (bool, error) {
	m, err := loadManifestForUpgrade(root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	if !m.Versions.legacyModulePolicies() {
		return false, nil
	}
	m.Versions.Skill, m.Versions.Service = "", ""
	if dryRun {
		return true, nil
	}
	raw, err := m.Marshal()
	if err != nil {
		return true, fmt.Errorf("rewrite %s: %w", ManifestName, err)
	}
	return true, os.WriteFile(filepath.Join(root, ManifestName), raw, 0o644)
}

// needsConsolidation reports whether a project's go.mod still requires the
// folded-in modules, i.e. whether its imports predate the consolidation.
func needsConsolidation(root string) bool {
	raw, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		return false
	}
	return strings.Contains(string(raw), "github.com/tjbdwanghaibo/roost-skill") || strings.Contains(string(raw), "github.com/tjbdwanghaibo/roost-service")
}
