// Package registry scans a project for //roost:register markers and generates
// the single aggregate that performs every static registration.
//
// Static registration is everything that must complete before any app.Mod
// starts: entity kinds and builders, component factories, config tables,
// protocol handlers, routes. None of it needs an app.Registry and none of it
// does I/O, so it runs once in bootstrap.New before app.New — which is why
// there is no mod for it. An entity builder registered inside a mod's Start is
// already too late for a mod that resolves entities in Provide; running before
// app.New removes that ordering question by construction.
package registry

import (
	"fmt"
	"github.com/tjbdwanghaibo/roost-codegen/internal/marker"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Phases run in this order. The order is semantic, not stylistic:
//   - kind must precede entity, because registering a builder resolves the
//     entity kind to its category;
//   - config must precede anything that reads a config table while registering;
//   - pre and post exist so business registrations that must run first or last
//     have a place, which is why there is no separate custom hook file.
var Phases = []string{"pre", "kind", "config", "component", "entity", "protocol", "nest", "route", "post"}

var markerRe = marker.Regexp("register", `(\s+(.*))?`)

// Registration is one marked function.
type Registration struct {
	ImportPath   string
	Func         string
	Phase        string
	Order        int
	ReturnsError bool
	// Position is only used for error messages.
	Position string
}

// Scan walks root and returns every //roost:register function, sorted into
// execution order: by phase, then by the explicit order attribute, then by
// import path and function name so the generated output never depends on
// filesystem or map iteration order.
func Scan(root string, modulePath string) ([]Registration, error) {
	if strings.TrimSpace(modulePath) == "" {
		return nil, fmt.Errorf("registry: module path is required to build import paths")
	}
	var found []Registration
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if path != root && skipDir(entry.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			return nil
		}
		registrations, err := scanFile(root, modulePath, path)
		if err != nil {
			return err
		}
		found = append(found, registrations...)
		return nil
	})
	if err != nil {
		return nil, err
	}
	if err := validate(found); err != nil {
		return nil, err
	}
	sortRegistrations(found)
	return found, nil
}

func skipDir(name string) bool {
	switch name {
	case ".git", "vendor", "testdata", "node_modules":
		return true
	}
	return strings.HasPrefix(name, ".")
}

func scanFile(root, modulePath, path string) ([]Registration, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
	if err != nil {
		// A project can legitimately contain a file this generator cannot
		// parse (work in progress). Skipping it silently would drop
		// registrations, so report it.
		return nil, fmt.Errorf("registry: parse %s: %w", path, err)
	}
	importPath, err := packageImportPath(root, modulePath, path)
	if err != nil {
		return nil, err
	}
	var out []Registration
	for _, decl := range file.Decls {
		fnDecl, ok := decl.(*ast.FuncDecl)
		if !ok || fnDecl.Doc == nil {
			continue
		}
		options, marked := markerOptions(fnDecl.Doc)
		if !marked {
			continue
		}
		position := fmt.Sprintf("%s:%d", path, fset.Position(fnDecl.Pos()).Line)
		registration, err := build(fnDecl, options, importPath, position)
		if err != nil {
			return nil, err
		}
		out = append(out, registration)
	}
	return out, nil
}

func markerOptions(doc *ast.CommentGroup) (map[string]string, bool) {
	for _, comment := range doc.List {
		match := markerRe.FindStringSubmatch(strings.TrimSpace(comment.Text))
		if match == nil {
			continue
		}
		options := map[string]string{}
		for _, field := range strings.Fields(match[2]) {
			key, value, found := strings.Cut(field, "=")
			if !found {
				options[key] = ""
				continue
			}
			options[key] = value
		}
		return options, true
	}
	return nil, false
}

func build(fnDecl *ast.FuncDecl, options map[string]string, importPath, position string) (Registration, error) {
	name := fnDecl.Name.Name
	if fnDecl.Recv != nil {
		return Registration{}, fmt.Errorf("registry: %s: //roost:register on method %s; the marker only applies to package-level functions", position, name)
	}
	if !ast.IsExported(name) {
		return Registration{}, fmt.Errorf("registry: %s: //roost:register on unexported function %s; the aggregate can only call exported functions", position, name)
	}
	if fnDecl.Type.Params != nil && len(fnDecl.Type.Params.List) > 0 {
		return Registration{}, fmt.Errorf("registry: %s: %s takes parameters; a registration function must take none", position, name)
	}
	returnsError, err := classifyResults(fnDecl, position, name)
	if err != nil {
		return Registration{}, err
	}

	phase, ok := options["phase"]
	if !ok || phase == "" {
		return Registration{}, fmt.Errorf("registry: %s: %s is missing phase=; valid phases in order: %s",
			position, name, strings.Join(Phases, ", "))
	}
	if !validPhase(phase) {
		return Registration{}, fmt.Errorf("registry: %s: %s has unknown phase %q; valid phases in order: %s",
			position, name, phase, strings.Join(Phases, ", "))
	}

	order := 0
	if raw, ok := options["order"]; ok {
		parsed, convErr := strconv.Atoi(raw)
		if convErr != nil {
			return Registration{}, fmt.Errorf("registry: %s: %s has non-integer order %q", position, name, raw)
		}
		order = parsed
	}

	for key := range options {
		switch key {
		case "phase", "order":
		default:
			return Registration{}, fmt.Errorf("registry: %s: %s has unknown marker option %q; supported: phase, order", position, name, key)
		}
	}

	return Registration{
		ImportPath:   importPath,
		Func:         name,
		Phase:        phase,
		Order:        order,
		ReturnsError: returnsError,
		Position:     position,
	}, nil
}

func classifyResults(fnDecl *ast.FuncDecl, position, name string) (bool, error) {
	if fnDecl.Type.Results == nil || len(fnDecl.Type.Results.List) == 0 {
		return false, nil
	}
	if len(fnDecl.Type.Results.List) > 1 {
		return false, fmt.Errorf("registry: %s: %s returns %d values; a registration function returns nothing or a single error",
			position, name, len(fnDecl.Type.Results.List))
	}
	identifier, ok := fnDecl.Type.Results.List[0].Type.(*ast.Ident)
	if !ok || identifier.Name != "error" {
		return false, fmt.Errorf("registry: %s: %s returns a non-error value; a registration function returns nothing or a single error",
			position, name)
	}
	return true, nil
}

func validPhase(phase string) bool {
	for _, candidate := range Phases {
		if candidate == phase {
			return true
		}
	}
	return false
}

func validate(registrations []Registration) error {
	seen := make(map[string]string, len(registrations))
	for _, registration := range registrations {
		key := registration.ImportPath + "." + registration.Func
		if previous, exists := seen[key]; exists {
			return fmt.Errorf("registry: %s is marked twice (%s and %s)", key, previous, registration.Position)
		}
		seen[key] = registration.Position
	}
	return nil
}

func sortRegistrations(registrations []Registration) {
	phaseIndex := make(map[string]int, len(Phases))
	for index, phase := range Phases {
		phaseIndex[phase] = index
	}
	sort.SliceStable(registrations, func(i, j int) bool {
		left, right := registrations[i], registrations[j]
		if phaseIndex[left.Phase] != phaseIndex[right.Phase] {
			return phaseIndex[left.Phase] < phaseIndex[right.Phase]
		}
		if left.Order != right.Order {
			return left.Order < right.Order
		}
		if left.ImportPath != right.ImportPath {
			return left.ImportPath < right.ImportPath
		}
		return left.Func < right.Func
	})
}

func packageImportPath(root, modulePath, filePath string) (string, error) {
	relative, err := filepath.Rel(root, filepath.Dir(filePath))
	if err != nil {
		return "", err
	}
	relative = filepath.ToSlash(relative)
	if relative == "." {
		return modulePath, nil
	}
	return modulePath + "/" + relative, nil
}
