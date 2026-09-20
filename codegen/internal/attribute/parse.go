package attribute

import (
	"fmt"
	"github.com/tjbdwanghaibo/roost-codegen/internal/marker"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

var attributeMarkerRe = marker.Regexp("attribute", `\s+(.+)`)

type rawProfile struct {
	def      ProfileDef
	fieldMap map[string]int
	methods  []*ast.FuncDecl
}

func parseDir(dir string) ([]ProfileDef, error) {
	fset := token.NewFileSet()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}

	var raws []rawProfile
	var pkg string
	var methods []*ast.FuncDecl

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") ||
			strings.HasPrefix(entry.Name(), "gen_") ||
			strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		f, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
		if pkg == "" {
			pkg = f.Name.Name
		}
		for _, decl := range f.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv != nil {
				methods = append(methods, fn)
			}
		}
		extracted, err := extractProfiles(fset, f, pkg)
		if err != nil {
			return nil, err
		}
		raws = append(raws, extracted...)
	}

	for i := range raws {
		raws[i].methods = methods
		if err := attachFormulas(&raws[i]); err != nil {
			return nil, err
		}
	}
	out := make([]ProfileDef, 0, len(raws))
	for _, raw := range raws {
		out = append(out, raw.def)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func extractProfiles(fset *token.FileSet, f *ast.File, pkg string) ([]rawProfile, error) {
	type markerInfo struct {
		line   int
		params map[string]string
	}
	var markers []markerInfo
	for _, cg := range f.Comments {
		for _, c := range cg.List {
			matches := attributeMarkerRe.FindStringSubmatch(c.Text)
			if matches == nil {
				continue
			}
			markers = append(markers, markerInfo{
				line:   fset.Position(c.Pos()).Line,
				params: parseKV(matches[1]),
			})
		}
	}
	if len(markers) == 0 {
		return nil, nil
	}

	var profiles []rawProfile
	for _, decl := range f.Decls {
		genDecl, ok := decl.(*ast.GenDecl)
		if !ok || genDecl.Tok != token.TYPE {
			continue
		}
		for _, spec := range genDecl.Specs {
			typeSpec, ok := spec.(*ast.TypeSpec)
			if !ok {
				continue
			}
			st, ok := typeSpec.Type.(*ast.StructType)
			if !ok {
				continue
			}
			line := fset.Position(typeSpec.Pos()).Line
			for _, marker := range markers {
				if marker.line != line-1 && marker.line != line-2 {
					continue
				}
				profile, err := buildProfile(typeSpec.Name.Name, pkg, st, marker.params)
				if err != nil {
					return nil, err
				}
				profiles = append(profiles, profile)
				break
			}
		}
	}
	return profiles, nil
}

func buildProfile(name, pkg string, st *ast.StructType, params map[string]string) (rawProfile, error) {
	indexBegin, err := parsePositiveInt(params["index"], 1)
	if err != nil {
		return rawProfile{}, err
	}
	maxCount, err := parsePositiveInt(params["max"], 64)
	if err != nil {
		return rawProfile{}, err
	}
	raw := rawProfile{
		def: ProfileDef{
			Name:       name,
			Package:    pkg,
			IndexBegin: indexBegin,
			MaxCount:   maxCount,
		},
		fieldMap: make(map[string]int),
	}
	// The one field a declaration must carry itself: every generated setter
	// ORs its attribute's bit into it. Checked after the per-field rules, so
	// a declaration with a real field problem still hears about that first
	// (RR-20260917-06).
	hasDirtyMask := false
	for _, field := range st.Fields.List {
		if exprString(field.Type) != "uint64" {
			continue
		}
		for _, fieldName := range field.Names {
			if fieldName.Name == "dirtyMask" {
				hasDirtyMask = true
			}
		}
	}
	for _, field := range st.Fields.List {
		if len(field.Names) == 0 {
			continue
		}
		fieldType := exprString(field.Type)
		for _, fieldName := range field.Names {
			if !fieldName.IsExported() {
				continue
			}
			attrName, skip := attrTag(field.Tag)
			if skip {
				continue
			}
			if attrName == "" {
				attrName = toSnake(fieldName.Name)
			}
			if len(raw.def.Fields) >= maxCount {
				return rawProfile{}, fmt.Errorf("%s has more than max=%d attribute fields", name, maxCount)
			}
			if _, exists := raw.fieldMap[fieldName.Name]; exists {
				return rawProfile{}, fmt.Errorf("%s has duplicate field %s", name, fieldName.Name)
			}
			index := len(raw.def.Fields)
			f := AttributeField{
				Name:      fieldName.Name,
				Type:      fieldType,
				AttrName:  attrName,
				ID:        indexBegin + index,
				Bit:       index,
				ConstName: "Attr" + fieldName.Name,
				MaskName:  "AttrMask" + fieldName.Name,
			}
			raw.fieldMap[fieldName.Name] = index
			raw.def.Fields = append(raw.def.Fields, f)
		}
	}
	if !hasDirtyMask {
		return rawProfile{}, fmt.Errorf("%s must declare an unexported `dirtyMask uint64` field: the generated setters write through it", name)
	}
	return raw, nil
}

func attachFormulas(raw *rawProfile) error {
	receiverName := raw.def.Name
	formulasByOutput := make(map[int]FormulaDef)
	for _, fn := range raw.methods {
		if !isMethodOf(fn, receiverName) || !strings.HasPrefix(fn.Name.Name, "_") {
			continue
		}
		outputName := strings.TrimPrefix(fn.Name.Name, "_")
		output, ok := raw.fieldMap[outputName]
		if !ok {
			continue
		}
		if _, exists := formulasByOutput[output]; exists {
			return fmt.Errorf("%s.%s formula duplicated", receiverName, outputName)
		}
		if fn.Type.Results == nil || len(fn.Type.Results.List) != 1 {
			return fmt.Errorf("%s.%s must return exactly one value", receiverName, fn.Name.Name)
		}
		outputType := raw.def.Fields[output].Type
		if got := exprString(fn.Type.Results.List[0].Type); got != outputType {
			return fmt.Errorf("%s.%s returns %s, want %s", receiverName, fn.Name.Name, got, outputType)
		}
		formula := FormulaDef{Method: fn.Name.Name, Output: output}
		if fn.Type.Params != nil {
			for _, p := range fn.Type.Params.List {
				paramType := exprString(p.Type)
				for _, name := range p.Names {
					input, ok := raw.fieldMap[name.Name]
					if !ok {
						return fmt.Errorf("%s.%s references unknown field %s", receiverName, fn.Name.Name, name.Name)
					}
					if raw.def.Fields[input].Type != paramType {
						return fmt.Errorf("%s.%s param %s has type %s, want %s", receiverName, fn.Name.Name, name.Name, paramType, raw.def.Fields[input].Type)
					}
					formula.Inputs = append(formula.Inputs, input)
				}
			}
		}
		if len(formula.Inputs) == 0 {
			return fmt.Errorf("%s.%s must reference at least one attribute field", receiverName, fn.Name.Name)
		}
		raw.def.Fields[output].Derived = true
		formulasByOutput[output] = formula
	}
	formulas, err := topoFormulas(raw.def.Fields, formulasByOutput)
	if err != nil {
		return err
	}
	raw.def.Formulas = formulas
	return nil
}

func isMethodOf(fn *ast.FuncDecl, receiver string) bool {
	if fn == nil || fn.Recv == nil || len(fn.Recv.List) != 1 {
		return false
	}
	switch t := fn.Recv.List[0].Type.(type) {
	case *ast.Ident:
		return t.Name == receiver
	case *ast.StarExpr:
		if id, ok := t.X.(*ast.Ident); ok {
			return id.Name == receiver
		}
	}
	return false
}

func topoFormulas(fields []AttributeField, formulasByOutput map[int]FormulaDef) ([]FormulaDef, error) {
	var out []FormulaDef
	state := make(map[int]int, len(formulasByOutput))
	var visit func(int) error
	visit = func(field int) error {
		if state[field] == 1 {
			return fmt.Errorf("attribute formula cycle at %s", fields[field].Name)
		}
		if state[field] == 2 {
			return nil
		}
		formula, ok := formulasByOutput[field]
		if !ok {
			return nil
		}
		state[field] = 1
		for _, input := range formula.Inputs {
			if err := visit(input); err != nil {
				return err
			}
		}
		state[field] = 2
		out = append(out, formula)
		return nil
	}
	keys := make([]int, 0, len(formulasByOutput))
	for output := range formulasByOutput {
		keys = append(keys, output)
	}
	sort.Ints(keys)
	for _, output := range keys {
		if err := visit(output); err != nil {
			return nil, err
		}
	}
	return out, nil
}
