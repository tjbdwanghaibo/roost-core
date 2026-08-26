// glsvet flags calls to goroutine-bound framework APIs from inside `go`
// statements. The nest execution model binds transaction context, guard
// scope, and lock reentrancy to the calling goroutine (via goroutine-local
// storage), so calling these APIs from a spawned goroutine silently loses the
// transaction: RecordUndo returns false, CurrentRollbackTx returns nil, and
// the mutation escapes rollback.
//
// Usage:
//
//	go run github.com/tjbdwanghaibo/cube-core/cmd/glsvet ./...
//
// The check is purely syntactic: it inspects function literals launched by
// `go` statements (including nested literals) for selector calls whose method
// name is goroutine-bound. It cannot follow named functions passed to `go`
// (go doWork(...)), so keep transaction logic out of functions that are also
// launched as goroutines. Test files are skipped by default (tests
// legitimately spawn goroutines to exercise per-goroutine behavior); pass
// -tests to include them. Exit status is 1 when any finding is reported.
package main

import (
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
)

var includeTests = flag.Bool("tests", false, "also vet _test.go files")

// goroutineBoundCalls are framework entry points whose results are bound to
// the calling goroutine. The map value documents the failure mode shown to
// the user.
var goroutineBoundCalls = map[string]string{
	"RecordUndo":        "returns false in a spawned goroutine; the mutation escapes rollback",
	"RecordUndoToken":   "returns false in a spawned goroutine; the mutation escapes rollback",
	"CurrentRollbackTx": "returns nil in a spawned goroutine",
	"CurrentContext":    "returns nil in a spawned goroutine (fctx is goroutine-local)",
	"CurrentGuardScope": "returns nil in a spawned goroutine (guard scope is goroutine-local)",
	"GetEntityGuard":    "resolves no guard in a spawned goroutine",
}

func main() {
	flag.Parse()
	if flag.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "usage: glsvet [-tests] <packages or directories, ./... supported>")
		os.Exit(2)
	}
	fileSet := token.NewFileSet()
	findings := 0
	for _, argument := range flag.Args() {
		for _, directory := range expandArgument(argument) {
			findings += vetDirectory(fileSet, directory)
		}
	}
	if findings > 0 {
		fmt.Fprintf(os.Stderr, "glsvet: %d finding(s)\n", findings)
		os.Exit(1)
	}
}

func expandArgument(argument string) []string {
	if !strings.HasSuffix(argument, "/...") {
		return []string{argument}
	}
	root := strings.TrimSuffix(argument, "/...")
	if root == "" {
		root = "."
	}
	var directories []string
	_ = filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil || !entry.IsDir() {
			return nil
		}
		name := entry.Name()
		if strings.HasPrefix(name, ".") && path != root || name == "testdata" || name == "vendor" {
			return filepath.SkipDir
		}
		directories = append(directories, path)
		return nil
	})
	return directories
}

func vetDirectory(fileSet *token.FileSet, directory string) int {
	packages, err := parser.ParseDir(fileSet, directory, func(info os.FileInfo) bool {
		return *includeTests || !strings.HasSuffix(info.Name(), "_test.go")
	}, 0)
	if err != nil {
		return 0 // not a Go directory; nothing to vet
	}
	findings := 0
	for _, pkg := range packages {
		for _, file := range pkg.Files {
			ast.Inspect(file, func(node ast.Node) bool {
				goStatement, ok := node.(*ast.GoStmt)
				if !ok {
					return true
				}
				if literal, ok := goStatement.Call.Fun.(*ast.FuncLit); ok {
					findings += reportBoundCalls(fileSet, literal.Body)
				}
				return true
			})
		}
	}
	return findings
}

func reportBoundCalls(fileSet *token.FileSet, body *ast.BlockStmt) int {
	findings := 0
	ast.Inspect(body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if reason, bound := goroutineBoundCalls[selector.Sel.Name]; bound {
			position := fileSet.Position(call.Pos())
			fmt.Printf("%s: %s called inside a go statement — %s\n", position, selector.Sel.Name, reason)
			findings++
		}
		return true
	})
	return findings
}
