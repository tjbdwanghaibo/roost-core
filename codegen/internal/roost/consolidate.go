package roost

import (
	"bytes"
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
	// SingleModule is the second stage: three repositories to one. kit and
	// codegen stop being modules, so it needs no per-package table.
	SingleModule struct {
		Boundary       struct{ Core string }       `yaml:"boundary"`
		RemovedModules []string                    `yaml:"removed_modules"`
		Prefix         []struct{ From, To string } `yaml:"prefix"`
	} `yaml:"single_module"`
	// Layout is the third stage: packages moving inside core (the sync block
	// gathering under sync/, ARCH-12). Applied to the result of the two
	// stages above; `package` is set where the package name changes.
	Layout struct {
		Boundary struct{ Core string } `yaml:"boundary"`
		Move     []struct {
			From    string `yaml:"from"`
			To      string `yaml:"to"`
			Package string `yaml:"package"`
		} `yaml:"move"`
		// Removed lists, per final import path, exported symbols a v1.16.x
		// project may use that no longer exist there and cannot be rewritten
		// mechanically; each group carries the migration guide printed with
		// every use (RR-20260926-24 复核残留).
		Removed []struct {
			Package string   `yaml:"package"`
			Guide   string   `yaml:"guide"`
			Symbols []string `yaml:"symbols"`
		} `yaml:"removed"`
	} `yaml:"layout"`
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

// singleModulePath applies the three-to-one prefixes to an import path and
// reports whether it moved.
//
// It runs on the RESULT of the first stage, never on the raw import: stage one
// moved most of kit's implementation into core proper, and prefixing first
// would send those packages to roost-core/kit/<pkg>, where they do not live.
func singleModulePath(p string, m consolidationMap) (string, bool) {
	for _, rule := range m.SingleModule.Prefix {
		if p == rule.From {
			return rule.To, true
		}
		if strings.HasPrefix(p, rule.From+"/") {
			return rule.To + strings.TrimPrefix(p, rule.From), true
		}
	}
	return p, false
}

// layoutPath applies the third stage (moves inside core) to an import path
// that the first two stages have already resolved. It returns the new path,
// the new package name when it differs from the old base, and whether it
// moved. A subpackage of a moved package moves with it.
func layoutPath(p string, m consolidationMap) (string, string, bool) {
	for _, rule := range m.Layout.Move {
		if p == rule.From {
			return rule.To, rule.Package, true
		}
		if strings.HasPrefix(p, rule.From+"/") {
			return rule.To + strings.TrimPrefix(p, rule.From), "", true
		}
	}
	return p, "", false
}

// ConsolidateResult summarises what the rewrite touched.
type ConsolidateResult struct {
	Files      []string // Go files whose imports were rewritten
	GoMod      bool     // go.mod lost the removed modules / gained the boundary versions
	Manifest   bool     // roost.yaml lost versions.skill / versions.service
	Unresolved []string // "file: import" pairs on removed modules the map does not know
	Removed    []string // "file:line: qualifier.Symbol" uses of symbols that no longer exist
}

// fileRemovedUse is a removedUse with the project-relative file it is in.
type fileRemovedUse struct {
	file string
	removedUse
}

// removedSymbolsError lists every use as file:line with a reference to its
// guide, then each guide once.
func removedSymbolsError(uses []fileRemovedUse, m consolidationMap) error {
	sort.Slice(uses, func(i, j int) bool {
		if uses[i].file != uses[j].file {
			return uses[i].file < uses[j].file
		}
		return uses[i].line < uses[j].line
	})
	var b strings.Builder
	fmt.Fprintf(&b, "consolidate: %d use(s) of framework symbols that no longer exist and cannot be rewritten automatically; change them as the guides say, then run `roost project upgrade --consolidate` again:\n", len(uses))
	var groups []int
	seen := map[int]bool{}
	for _, use := range uses {
		fmt.Fprintf(&b, "  %s:%d: %s [%d]\n", use.file, use.line, use.ref, use.group+1)
		if !seen[use.group] {
			seen[use.group] = true
			groups = append(groups, use.group)
		}
	}
	sort.Ints(groups)
	b.WriteString("guides:")
	for _, group := range groups {
		fmt.Fprintf(&b, "\n  [%d] %s: %s", group+1, m.Layout.Removed[group].Package, m.Layout.Removed[group].Guide)
	}
	return errors.New(b.String())
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
	var removedUses []fileRemovedUse
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
		changed, unresolved, removed, err := consolidateFile(p, table, m, dryRun)
		if err != nil {
			return fmt.Errorf("%s: %w", p, err)
		}
		rel := relativeSlash(root, p)
		for _, u := range unresolved {
			result.Unresolved = append(result.Unresolved, rel+": "+u)
		}
		for _, use := range removed {
			removedUses = append(removedUses, fileRemovedUse{file: rel, removedUse: use})
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
	if len(removedUses) > 0 {
		// The imports are already rewritten (unless dry-run); what is left is
		// manual. Say what was done, then fail with the list — succeeding here
		// would leave the user to discover the same list as `undefined` errors
		// with no pointer to the replacement API.
		if stdout != nil && len(result.Files) > 0 {
			verb := "rewrote"
			if dryRun {
				verb = "would rewrite"
			}
			fmt.Fprintf(stdout, "consolidate: %s %d Go file(s)\n", verb, len(result.Files))
			for _, f := range result.Files {
				fmt.Fprintf(stdout, "  %s\n", f)
			}
		}
		for _, use := range removedUses {
			result.Removed = append(result.Removed, fmt.Sprintf("%s:%d: %s", use.file, use.line, use.ref))
		}
		return result, removedSymbolsError(removedUses, m)
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

// edit replaces src[start:end] with text; consolidateFile applies them back to
// front so earlier offsets stay valid.
type edit struct {
	start, end int
	text       string
}

// keptImportEdits repoints an import whose symbols stay in kit to where kit
// moved it. When the package name changes on the way (roost-kit/room →
// kit/syncbus, package room → syncbus) an unaliased import gets the name the
// file already uses, the same rule the everything-moves branch applies;
// without it `room.NewSyncBusMod` compiles to `undefined: room`
// (RR-20260926-24 复核残留).
func keptImportEdits(fset *token.FileSet, imp *ast.ImportSpec, keptPath, keptBase, alias string) []edit {
	start, end := fset.Position(imp.Path.Pos()).Offset, fset.Position(imp.Path.End()).Offset
	edits := []edit{{start, end, strconv.Quote(keptPath)}}
	if imp.Name == nil && keptBase != alias {
		edits = append(edits, edit{start, start, alias + " "})
	}
	return edits
}

// consolidateFile rewrites one Go file. Split packages are decided per
// selector: symbols the map lists as staying in kit keep the old import, every
// other symbol moves to the core path; a file using both ends up with both
// imports.
func consolidateFile(p string, table map[string]relocation, m consolidationMap, dryRun bool) (bool, []string, []removedUse, error) {
	src, err := os.ReadFile(p)
	if err != nil {
		return false, nil, nil, err
	}
	content, changed, unresolved, err := rewriteImports(p, src, table, m)
	if err != nil {
		return false, unresolved, nil, err
	}
	// Scanned on what the file will contain, so the reported lines are the
	// lines the user opens — and a rerun after the imports were rewritten
	// still finds what is left.
	removed, err := findRemovedUses(content, m)
	if err != nil {
		return false, unresolved, nil, err
	}
	if !changed || dryRun {
		return changed, unresolved, removed, nil
	}
	return true, unresolved, removed, os.WriteFile(p, content, 0o644)
}

// rewriteImports computes the relocated source of one Go file without
// writing it. It reports the new content (src itself when nothing moved),
// whether anything changed, and imports on removed modules the map does not
// know.
func rewriteImports(p string, src []byte, table map[string]relocation, m consolidationMap) ([]byte, bool, []string, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, p, src, parser.ParseComments)
	if err != nil {
		return nil, false, nil, err
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
			return nil, false, nil, err
		}
		rel, mapped := table[oldPath]
		switch {
		case mapped:
			// Stage one knows this package. Stage two then applies to where it
			// LANDED: a package that stayed in kit moves again, one that went
			// to core proper is already home.
			if moved, ok := singleModulePath(rel.to, m); ok {
				rel.to = moved
			}
		default:
			// Not in stage one's table: either untouched by the first
			// consolidation, or under a module the second stage relocates
			// wholesale.
			if moved, ok := singleModulePath(oldPath, m); ok {
				rel = relocation{to: moved}
				mapped = true
			}
		}
		// Stage three moves packages inside core, wherever the path came from.
		if moved, pkgName, ok := layoutPath(rel.to, m); mapped && ok {
			rel.to = moved
			if pkgName != "" {
				rel.pkgName = pkgName
			}
		} else if !mapped {
			if moved, pkgName, ok := layoutPath(oldPath, m); ok {
				rel = relocation{to: moved, pkgName: pkgName}
				mapped = true
			}
		}
		if !mapped {
			for _, mod := range append(append([]string(nil), m.RemovedModules...), m.SingleModule.RemovedModules...) {
				if oldPath == mod || strings.HasPrefix(oldPath, mod+"/") {
					unresolved = append(unresolved, oldPath)
				}
			}
			continue
		}
		// Where a split package's kept symbols live after stage two. For an
		// unsplit package this equals the old path and nothing uses it.
		keptPath, keptMoved := singleModulePath(oldPath, m)
		keptBase := path.Base(keptPath)
		if moved, pkgName, ok := layoutPath(keptPath, m); ok {
			keptPath, keptMoved, keptBase = moved, true, path.Base(moved)
			if pkgName != "" {
				keptBase = pkgName
			}
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
			// Room 模块的正式改名只作用于已确认的 kit import，不触碰同名业务符号。
			if rel.to == "github.com/tjbdwanghaibo/roost-core/kit/syncbus" || (rel.kitKeeps[sel.Sel.Name] && keptPath == "github.com/tjbdwanghaibo/roost-core/kit/syncbus") {
				renamed := map[string]string{"RoomMod": "SyncBusMod", "NewRoomMod": "NewSyncBusMod"}[sel.Sel.Name]
				if renamed != "" {
					edits = append(edits, edit{fset.Position(sel.Sel.Pos()).Offset, fset.Position(sel.Sel.End()).Offset, renamed})
				}
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
			// Only Mod glue used: the kit import stays — but stage two moves
			// the place it stayed in.
			if keptMoved {
				edits = append(edits, keptImportEdits(fset, imp, keptPath, keptBase, alias)...)
			}
		default:
			// Mixed: kit import stays (at its stage-two location), moved
			// symbols get a second import.
			if keptMoved {
				edits = append(edits, keptImportEdits(fset, imp, keptPath, keptBase, alias)...)
			}
			coreAlias := fresh("core" + path.Base(oldPath))
			extraImports = append(extraImports, coreAlias+" "+strconv.Quote(rel.to))
			for _, sel := range coreUses {
				edits = append(edits, edit{fset.Position(sel.X.Pos()).Offset, fset.Position(sel.X.End()).Offset, coreAlias})
			}
		}
	}
	if len(edits) == 0 && len(extraImports) == 0 {
		return src, false, unresolved, nil
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
		return nil, false, unresolved, fmt.Errorf("rewritten source does not format: %w", err)
	}
	return formatted, true, unresolved, nil
}

// removedUse is one reference, in a file's final content, to a symbol the
// layout stage lists as removed at the path the file imports.
type removedUse struct {
	line  int
	ref   string // qualifier.Symbol as the file writes it
	group int    // index into consolidationMap.Layout.Removed
}

// findRemovedUses lists every qualified reference to a removed symbol. The
// qualifier is resolved through the file's own imports — an explicit alias or
// the path's last element, which is the package name for every path the table
// names — so a local identifier that happens to share a package's name is
// not a use.
func findRemovedUses(src []byte, m consolidationMap) ([]removedUse, error) {
	if len(m.Layout.Removed) == 0 {
		return nil, nil
	}
	removedAt := map[string]map[string]int{} // import path → symbol → group
	for i, group := range m.Layout.Removed {
		if removedAt[group.Package] == nil {
			removedAt[group.Package] = map[string]int{}
		}
		for _, symbol := range group.Symbols {
			removedAt[group.Package][symbol] = i
		}
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "", src, 0)
	if err != nil {
		return nil, err
	}
	byQualifier := map[string]map[string]int{}
	for _, imp := range file.Imports {
		importPath, err := strconv.Unquote(imp.Path.Value)
		if err != nil || removedAt[importPath] == nil {
			continue
		}
		name := path.Base(importPath)
		if imp.Name != nil {
			name = imp.Name.Name
		}
		if name == "_" || name == "." {
			continue
		}
		byQualifier[name] = removedAt[importPath]
	}
	if len(byQualifier) == 0 {
		return nil, nil
	}
	var uses []removedUse
	ast.Inspect(file, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		id, ok := sel.X.(*ast.Ident)
		if !ok || id.Obj != nil {
			return true
		}
		if group, ok := byQualifier[id.Name][sel.Sel.Name]; ok {
			uses = append(uses, removedUse{line: fset.Position(sel.Pos()).Line, ref: id.Name + "." + sel.Sel.Name, group: group})
		}
		return true
	})
	return uses, nil
}

var consolidateRequireLine = regexp.MustCompile(`(?m)^\s*github\.com/tjbdwanghaibo/(roost-skill|roost-service|roost-kit|roost-codegen)\s+\S+[^\n]*\n`)

// consolidateGoMod drops the folded-in modules from go.mod — roost-skill and
// roost-service from the first stage, roost-kit and roost-codegen from the
// second. After the rewrite no import names them, so the requires are dead.
//
// It deliberately leaves the core version alone: the caller's dependency resolution
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
	text = regexp.MustCompile(`(?m)^require github\.com/tjbdwanghaibo/(roost-skill|roost-service|roost-kit|roost-codegen)\s+\S+[^\n]*\n`).ReplaceAllString(text, "")
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

// needsConsolidation reports whether a project still predates one of the
// relocations: its go.mod requires a folded-in module (stages one and two),
// or one of its Go files imports a pre-layout core path (stage three).
func needsConsolidation(root string) bool {
	raw, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		return false
	}
	for _, module := range []string{"roost-skill", "roost-service", "roost-kit", "roost-codegen"} {
		if strings.Contains(string(raw), "github.com/tjbdwanghaibo/"+module) {
			return true
		}
	}
	_, m, err := loadConsolidationMap()
	if err != nil || len(m.Layout.Move) == 0 {
		return false
	}
	found := false
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil || found {
			return filepath.SkipAll
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
		src, err := os.ReadFile(p)
		if err != nil {
			return nil
		}
		for _, rule := range m.Layout.Move {
			if bytes.Contains(src, []byte(strconv.Quote(rule.From))) || bytes.Contains(src, []byte(`"`+rule.From+`/`)) {
				found = true
				return filepath.SkipAll
			}
		}
		return nil
	})
	return found
}
