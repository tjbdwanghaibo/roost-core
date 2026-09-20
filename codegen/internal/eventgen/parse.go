package eventgen

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"path/filepath"
	"strings"
)

// EventDef holds a parsed event struct definition.
type EventDef struct {
	Name    string // struct name, e.g. "EventPlayerOnLine"
	Fields  []EventFieldDef
	Imports []string
}

// EventFieldDef describes one field line in an event struct.
type EventFieldDef struct {
	Names []string
	Type  string
	Tag   string
}

// ConstName returns the EventType constant name, e.g. "EventTypePlayerOnLine".
func (e EventDef) ConstName() string {
	return "EventType" + strings.TrimPrefix(e.Name, "Event")
}

func (f EventFieldDef) NamesText() string {
	return strings.Join(f.Names, ", ")
}

// parseEventDir scans dir for struct types prefixed with "Event".
func parseEventDir(dir string) ([]EventDef, error) {
	fset := token.NewFileSet()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}

	var events []EventDef

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") {
			continue
		}
		// Skip generated files
		if strings.HasSuffix(entry.Name(), "_gen.go") {
			continue
		}
		// Skip test files
		if strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}

		filePath := filepath.Join(dir, entry.Name())
		content, err := os.ReadFile(filePath)
		if err != nil {
			return nil, err
		}

		f, err := parser.ParseFile(fset, filePath, content, 0)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", entry.Name(), err)
		}
		imports := parseImportLines(f.Imports)

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
				structType, ok := typeSpec.Type.(*ast.StructType)
				if !ok {
					continue
				}
				name := typeSpec.Name.Name
				if strings.HasPrefix(name, "Event") && name != "EventData" {
					fields, err := parseEventFields(fset, structType)
					if err != nil {
						return nil, fmt.Errorf("parse fields for %s: %w", name, err)
					}
					events = append(events, EventDef{Name: name, Fields: fields, Imports: imports})
				}
			}
		}
	}

	return events, nil
}

func parseEventFields(fset *token.FileSet, st *ast.StructType) ([]EventFieldDef, error) {
	if st == nil || st.Fields == nil {
		return nil, nil
	}
	fields := make([]EventFieldDef, 0, len(st.Fields.List))
	for _, field := range st.Fields.List {
		typeStr, err := exprString(fset, field.Type)
		if err != nil {
			return nil, err
		}
		names := make([]string, 0, len(field.Names))
		for _, name := range field.Names {
			names = append(names, name.Name)
		}
		tag := ""
		if field.Tag != nil {
			tag = field.Tag.Value
		}
		fields = append(fields, EventFieldDef{
			Names: names,
			Type:  typeStr,
			Tag:   tag,
		})
	}
	return fields, nil
}

func exprString(fset *token.FileSet, expr ast.Expr) (string, error) {
	var buf bytes.Buffer
	if err := printer.Fprint(&buf, fset, expr); err != nil {
		return "", err
	}
	return buf.String(), nil
}

func parseImportLines(imports []*ast.ImportSpec) []string {
	if len(imports) == 0 {
		return nil
	}
	out := make([]string, 0, len(imports))
	for _, item := range imports {
		if item == nil || item.Path == nil {
			continue
		}
		if item.Name != nil {
			out = append(out, item.Name.Name+" "+item.Path.Value)
			continue
		}
		out = append(out, item.Path.Value)
	}
	return out
}
