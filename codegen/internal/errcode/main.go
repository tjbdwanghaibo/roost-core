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
		if !bytes.Contains(raw, []byte("errcode.Define")) {
			return nil
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, raw, 0)
		if err != nil {
			return fmt.Errorf("parse %s: %w", path, err)
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			rel = path
		}
		var visitErr error
		ast.Inspect(file, func(node ast.Node) bool {
			if visitErr != nil {
				return false
			}
			call, ok := node.(*ast.CallExpr)
			if !ok || len(call.Args) != 3 {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Define" {
				return true
			}
			ident, ok := sel.X.(*ast.Ident)
			if !ok || ident.Name != "errcode" {
				return true
			}
			code, ok := call.Args[0].(*ast.BasicLit)
			if !ok || code.Kind != token.INT || strings.Trim(code.Value, "0123456789") != "" {
				return true
			}
			nameLit, ok := call.Args[1].(*ast.BasicLit)
			if !ok || nameLit.Kind != token.STRING {
				return true
			}
			messageLit, ok := call.Args[2].(*ast.BasicLit)
			if !ok || messageLit.Kind != token.STRING {
				return true
			}
			code64, err := strconv.ParseInt(code.Value, 10, 32)
			if err != nil {
				visitErr = fmt.Errorf("%s: parse errcode %q: %w", rel, code.Value, err)
				return false
			}
			name, err := strconv.Unquote(nameLit.Value)
			if err != nil {
				visitErr = fmt.Errorf("%s: parse errcode name: %w", rel, err)
				return false
			}
			message, err := strconv.Unquote(messageLit.Value)
			if err != nil {
				visitErr = fmt.Errorf("%s: parse errcode message: %w", rel, err)
				return false
			}
			defs = append(defs, Definition{Code: int32(code64), Name: name, Message: message, File: rel})
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
	return defs, nil
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
