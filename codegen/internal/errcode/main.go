package errcode

import (
	"bytes"
	"encoding/csv"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

type Definition struct {
	Code    int32
	Name    string
	Message string
	File    string
}

// DefaultOutFile is where the error-code table is written inside a business
// project; `roost generate` refers to it (U-0118).
const DefaultOutFile = "docs/generated/errcode.csv"

func Run(args []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("errcode", flag.ContinueOnError)
	flags.SetOutput(stdout)
	root := flags.String("root", ".", "repository root")
	out := flags.String("out", DefaultOutFile, "output csv path")
	if err := flags.Parse(args); err != nil {
		return err
	}

	defs, err := extractDefinitions(*root)
	if err != nil {
		return err
	}
	if err := writeCSV(*out, defs); err != nil {
		return err
	}
	return nil
}

func extractDefinitions(root string) ([]Definition, error) {
	root = filepath.Clean(root)
	var defs []Definition
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "bin", "disk", "vendor":
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) != ".go" || strings.HasSuffix(filepath.Base(path), "_test.go") || filepath.Base(path) == "main.go" && filepath.Dir(path) == filepath.Join(root, "tool", "errcode") {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if !bytes.Contains(raw, []byte(errcodeImportPath)) && !bytes.Contains(raw, []byte("errcode.Define")) {
			return nil
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, raw, 0)
		if err != nil {
			return fmt.Errorf("parse %s: %w", path, err)
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			rel = path
		}
		rel = filepath.ToSlash(rel)
		local, ok := errcodeLocalName(file)
		if !ok {
			return nil
		}
		var visitErr error
		ast.Inspect(file, func(node ast.Node) bool {
			if visitErr != nil {
				return false
			}
			call, ok := node.(*ast.CallExpr)
			if !ok || !isErrcodeDefine(call.Fun, local) {
				return true
			}
			// Every Define has to be readable here: the exported table is what
			// clients map codes from, and the duplicate checks below are only
			// as complete as this scan. A Define whose code, name or message
			// is not a literal used to be skipped silently — missing from the
			// table and invisible to the duplicate check (RR-20261005-NC-63).
			def, ok := literalDefinition(call)
			if !ok {
				visitErr = fmt.Errorf("%s:%d: errcode.Define must be called with integer and string literals (code, name, message) so the error table can be exported and checked", rel, fset.Position(call.Pos()).Line)
				return false
			}
			def.File = rel
			defs = append(defs, def)
			return true
		})
		if visitErr != nil {
			return visitErr
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.SliceStable(defs, func(i, j int) bool {
		if defs[i].Code == defs[j].Code {
			return defs[i].Name < defs[j].Name
		}
		return defs[i].Code < defs[j].Code
	})
	for i := 1; i < len(defs); i++ {
		if defs[i].Code == defs[i-1].Code {
			return nil, fmt.Errorf("duplicate errcode %d: %s and %s", defs[i].Code, defs[i-1].File, defs[i].File)
		}
	}
	// A name is a client-facing identifier too (and the reason ClientError
	// falls back to when a message is empty): one name for two codes is the
	// same conflict seen from the other side.
	byName := make(map[string]Definition, len(defs))
	for _, def := range defs {
		if first, exists := byName[def.Name]; exists {
			return nil, fmt.Errorf("duplicate errcode name %q: %d in %s and %d in %s", def.Name, first.Code, first.File, def.Code, def.File)
		}
		byName[def.Name] = def
	}
	return defs, nil
}

const errcodeImportPath = "github.com/tjbdwanghaibo/roost-core/errcode"

// errcodeLocalName reports the name roost-core's errcode package goes by in
// this file ("errcode", an alias, or "." for a dot import). A file that does
// not import it is read as `errcode.` — what the scan has always matched —
// and a blank import defines nothing.
func errcodeLocalName(file *ast.File) (string, bool) {
	if len(file.Imports) == 0 {
		return "errcode", true
	}
	for _, spec := range file.Imports {
		importPath, err := strconv.Unquote(spec.Path.Value)
		if err != nil || importPath != errcodeImportPath {
			continue
		}
		if spec.Name == nil {
			return "errcode", true
		}
		if spec.Name.Name == "_" {
			return "", false
		}
		return spec.Name.Name, true
	}
	return "errcode", true
}

func isErrcodeDefine(fun ast.Expr, local string) bool {
	switch fn := fun.(type) {
	case *ast.SelectorExpr:
		ident, ok := fn.X.(*ast.Ident)
		return ok && local != "." && ident.Name == local && fn.Sel.Name == "Define"
	case *ast.Ident:
		return local == "." && fn.Name == "Define"
	}
	return false
}

func literalDefinition(call *ast.CallExpr) (Definition, bool) {
	if len(call.Args) != 3 {
		return Definition{}, false
	}
	code, ok := call.Args[0].(*ast.BasicLit)
	if !ok || code.Kind != token.INT {
		return Definition{}, false
	}
	code64, err := strconv.ParseInt(code.Value, 10, 32)
	if err != nil {
		return Definition{}, false
	}
	var texts [2]string
	for i, arg := range call.Args[1:] {
		lit, ok := arg.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return Definition{}, false
		}
		text, err := strconv.Unquote(lit.Value)
		if err != nil {
			return Definition{}, false
		}
		texts[i] = text
	}
	return Definition{Code: int32(code64), Name: texts[0], Message: texts[1]}, true
}

func writeCSV(path string, defs []Definition) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	w := csv.NewWriter(f)
	if err := w.Write([]string{"Code", "Name", "Message", "File"}); err != nil {
		return err
	}
	for _, def := range defs {
		if err := w.Write([]string{
			strconv.FormatInt(int64(def.Code), 10),
			def.Name,
			def.Message,
			def.File,
		}); err != nil {
			return err
		}
	}
	w.Flush()
	return w.Error()
}
