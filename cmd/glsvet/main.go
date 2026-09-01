// glsvet enforces the Nest handler concurrency boundary and also flags calls
// to goroutine-bound framework APIs from general `go` statements. A handler
// may hand explicit business DTOs to Nest effects or cube-core/worker, but it
// must never create or wrap its own goroutine.
//
// Usage:
//
//	go run github.com/tjbdwanghaibo/cube-core/cmd/glsvet ./...
//
// The check follows same-file named function calls from a handler, catches raw
// go statements and common async wrapper .Go calls, and recognizes direct
// cube-core/worker Pool variables as the allowed .Go implementation. Test
// files are skipped by default; pass -tests to include them. Exit status is 1
// when any finding is reported.
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

var admissionCalls = map[string]bool{
	"Dispatch":       true,
	"TryDispatch":    true,
	"Publish":        true,
	"PublishRequest": true,
	"Submit":         true,
	"TrySubmit":      true,
	"TryGo":          true,
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
		voidAdmissionMethods := collectVoidAdmissionMethods(pkg)
		returningAdmissionMethods := collectReturningAdmissionMethods(pkg)
		for _, file := range pkg.Files {
			findings += reportHandlerConcurrency(fileSet, file)
			findings += reportIgnoredAdmission(fileSet, file, voidAdmissionMethods, returningAdmissionMethods)
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

func collectReturningAdmissionMethods(pkg *ast.Package) map[string]bool {
	returning := make(map[string]bool)
	for _, file := range pkg.Files {
		for _, declaration := range file.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok || function.Recv == nil || !admissionCalls[function.Name.Name] {
				continue
			}
			if function.Type.Results != nil && len(function.Type.Results.List) > 0 {
				returning[function.Name.Name] = true
			}
		}
	}
	return returning
}

func collectVoidAdmissionMethods(pkg *ast.Package) map[string]bool {
	seen := make(map[string]bool)
	allVoid := make(map[string]bool)
	for _, file := range pkg.Files {
		for _, declaration := range file.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok || function.Recv == nil || !admissionCalls[function.Name.Name] {
				continue
			}
			name := function.Name.Name
			if !seen[name] {
				seen[name] = true
				allVoid[name] = true
			}
			if function.Type.Results != nil && len(function.Type.Results.List) > 0 {
				allVoid[name] = false
			}
		}
	}
	return allVoid
}

func reportIgnoredAdmission(fileSet *token.FileSet, file *ast.File, voidMethods, returningMethods map[string]bool) int {
	frameworkAliases := frameworkImportAliases(file)
	frameworkReceivers := collectFrameworkReceivers(file, frameworkAliases)
	findings := 0
	ast.Inspect(file, func(node ast.Node) bool {
		switch statement := node.(type) {
		case *ast.ExprStmt:
			if name, receiver, ok := admissionCall(statement.X); ok && !voidMethods[name] &&
				(returningMethods[name] || isFrameworkReceiver(receiver, frameworkAliases, frameworkReceivers)) {
				position := fileSet.Position(statement.Pos())
				fmt.Printf("%s: %s admission result is ignored; handle success/failure explicitly\n", position, name)
				findings++
			}
		case *ast.AssignStmt:
			if !allBlankIdentifiers(statement.Lhs) {
				return true
			}
			for _, expression := range statement.Rhs {
				// A blank assignment only compiles when the call has results, so
				// no receiver inference is needed and third-party void methods are
				// not at risk of a name-only false positive here.
				if name, _, ok := admissionCall(expression); ok && !voidMethods[name] {
					position := fileSet.Position(statement.Pos())
					fmt.Printf("%s: %s admission result is assigned only to blank identifiers; handle it or use a documented completion helper\n", position, name)
					findings++
				}
			}
		}
		return true
	})
	return findings
}

func admissionCall(expression ast.Expr) (string, ast.Expr, bool) {
	call, ok := expression.(*ast.CallExpr)
	if !ok {
		return "", nil, false
	}
	selector, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || !admissionCalls[selector.Sel.Name] {
		return "", nil, false
	}
	return selector.Sel.Name, selector.X, true
}

func frameworkImportAliases(file *ast.File) map[string]bool {
	aliases := make(map[string]bool)
	for _, spec := range file.Imports {
		path := strings.Trim(spec.Path.Value, "\"")
		if !strings.HasPrefix(path, "github.com/tjbdwanghaibo/cube-core/") &&
			!strings.HasPrefix(path, "github.com/tjbdwanghaibo/cube-kit/") &&
			!strings.HasPrefix(path, "github.com/tjbdwanghaibo/roost-skill/") {
			continue
		}
		name := filepath.Base(path)
		if spec.Name != nil {
			name = spec.Name.Name
		}
		if name != "_" && name != "." {
			aliases[name] = true
		}
	}
	return aliases
}

func collectFrameworkReceivers(file *ast.File, aliases map[string]bool) map[string]bool {
	receivers := make(map[string]bool)
	ast.Inspect(file, func(node ast.Node) bool {
		switch typed := node.(type) {
		case *ast.Field:
			if isFrameworkType(typed.Type, aliases) {
				for _, name := range typed.Names {
					receivers[name.Name] = true
				}
			}
		case *ast.ValueSpec:
			if isFrameworkType(typed.Type, aliases) {
				for _, name := range typed.Names {
					receivers[name.Name] = true
				}
			}
		case *ast.AssignStmt:
			for index, rhs := range typed.Rhs {
				call, ok := rhs.(*ast.CallExpr)
				if !ok || index >= len(typed.Lhs) || !isFrameworkReceiver(call.Fun, aliases, receivers) {
					continue
				}
				if name, ok := typed.Lhs[index].(*ast.Ident); ok {
					receivers[name.Name] = true
				}
			}
		}
		return true
	})
	return receivers
}

func isFrameworkType(expression ast.Expr, aliases map[string]bool) bool {
	switch typed := expression.(type) {
	case *ast.SelectorExpr:
		identifier, ok := typed.X.(*ast.Ident)
		return ok && aliases[identifier.Name]
	case *ast.StarExpr:
		return isFrameworkType(typed.X, aliases)
	case *ast.ArrayType:
		return isFrameworkType(typed.Elt, aliases)
	case *ast.IndexExpr:
		return isFrameworkType(typed.X, aliases)
	case *ast.IndexListExpr:
		return isFrameworkType(typed.X, aliases)
	}
	return false
}

func isFrameworkReceiver(expression ast.Expr, aliases, receivers map[string]bool) bool {
	switch typed := expression.(type) {
	case *ast.Ident:
		return aliases[typed.Name] || receivers[typed.Name]
	case *ast.SelectorExpr:
		return receivers[typed.Sel.Name] || isFrameworkReceiver(typed.X, aliases, receivers)
	case *ast.IndexExpr:
		return isFrameworkReceiver(typed.X, aliases, receivers)
	case *ast.IndexListExpr:
		return isFrameworkReceiver(typed.X, aliases, receivers)
	}
	return false
}

func allBlankIdentifiers(expressions []ast.Expr) bool {
	if len(expressions) == 0 {
		return false
	}
	for _, expression := range expressions {
		identifier, ok := expression.(*ast.Ident)
		if !ok || identifier.Name != "_" {
			return false
		}
	}
	return true
}

func reportHandlerConcurrency(fileSet *token.FileSet, file *ast.File) int {
	functions := make(map[string]*ast.FuncDecl)
	for _, declaration := range file.Decls {
		if function, ok := declaration.(*ast.FuncDecl); ok && function.Recv == nil {
			functions[function.Name.Name] = function
		}
	}
	workerAliases := workerImportAliases(file)
	workerPools := collectWorkerPools(file, workerAliases)
	findings := 0
	for _, function := range functions {
		if !isNestHandler(function) {
			continue
		}
		findings += inspectHandlerFunction(fileSet, function, function.Name.Name, functions, workerPools, make(map[string]bool))
	}
	return findings
}

func inspectHandlerFunction(fileSet *token.FileSet, function *ast.FuncDecl, handler string, functions map[string]*ast.FuncDecl, workerPools map[string]bool, visiting map[string]bool) int {
	if function == nil || function.Body == nil || visiting[function.Name.Name] {
		return 0
	}
	visiting[function.Name.Name] = true
	defer delete(visiting, function.Name.Name)
	findings := 0
	outerNames := declaredNames(function)
	ast.Inspect(function.Body, func(node ast.Node) bool {
		switch typed := node.(type) {
		case *ast.GoStmt:
			position := fileSet.Position(typed.Go)
			fmt.Printf("%s: raw goroutine reachable from Nest handler %s; use nest.Emit(Effect) or cube-core/worker with explicit business parameters\n", position, handler)
			findings++
			return false
		case *ast.CallExpr:
			if identifier, ok := typed.Fun.(*ast.Ident); ok {
				if called := functions[identifier.Name]; called != nil && called != function {
					findings += inspectHandlerFunction(fileSet, called, handler, functions, workerPools, visiting)
				}
				return true
			}
			selector, ok := typed.Fun.(*ast.SelectorExpr)
			if !ok || (selector.Sel.Name != "Go" && selector.Sel.Name != "TryGo") {
				return true
			}
			if allowedWorkerPoolReceiver(selector.X, workerPools) {
				findings += reportWorkerClosureCaptures(fileSet, typed, handler, outerNames)
				return true
			}
			position := fileSet.Position(typed.Pos())
			fmt.Printf("%s: async .%s wrapper reachable from Nest handler %s; only cube-core/worker Pool.Go/TryGo is allowed\n", position, selector.Sel.Name, handler)
			findings++
		}
		return true
	})
	return findings
}

func declaredNames(function *ast.FuncDecl) map[string]bool {
	names := make(map[string]bool)
	addFields := func(fields *ast.FieldList) {
		if fields == nil {
			return
		}
		for _, field := range fields.List {
			for _, name := range field.Names {
				names[name.Name] = true
			}
		}
	}
	addFields(function.Type.Params)
	addFields(function.Type.Results)
	ast.Inspect(function.Body, func(node ast.Node) bool {
		if _, nested := node.(*ast.FuncLit); nested {
			return false
		}
		switch typed := node.(type) {
		case *ast.AssignStmt:
			if typed.Tok == token.DEFINE {
				for _, expression := range typed.Lhs {
					if name, ok := expression.(*ast.Ident); ok {
						names[name.Name] = true
					}
				}
			}
		case *ast.ValueSpec:
			for _, name := range typed.Names {
				names[name.Name] = true
			}
		case *ast.RangeStmt:
			if typed.Tok == token.DEFINE {
				for _, expression := range []ast.Expr{typed.Key, typed.Value} {
					if name, ok := expression.(*ast.Ident); ok {
						names[name.Name] = true
					}
				}
			}
		}
		return true
	})
	delete(names, "_")
	return names
}

func reportWorkerClosureCaptures(fileSet *token.FileSet, call *ast.CallExpr, handler string, outerNames map[string]bool) int {
	findings := 0
	for _, argument := range call.Args {
		literal, ok := argument.(*ast.FuncLit)
		if !ok {
			continue
		}
		inner := make(map[string]bool)
		addFields := func(fields *ast.FieldList) {
			if fields == nil {
				return
			}
			for _, field := range fields.List {
				for _, name := range field.Names {
					inner[name.Name] = true
				}
			}
		}
		addFields(literal.Type.Params)
		addFields(literal.Type.Results)
		ast.Inspect(literal.Body, func(node ast.Node) bool {
			if nested, ok := node.(*ast.FuncLit); ok && nested != literal {
				return false
			}
			switch typed := node.(type) {
			case *ast.AssignStmt:
				if typed.Tok == token.DEFINE {
					for _, expression := range typed.Lhs {
						if name, ok := expression.(*ast.Ident); ok {
							inner[name.Name] = true
						}
					}
				}
			case *ast.ValueSpec:
				for _, name := range typed.Names {
					inner[name.Name] = true
				}
			}
			return true
		})
		reported := make(map[string]bool)
		ast.Inspect(literal.Body, func(node ast.Node) bool {
			if nested, ok := node.(*ast.FuncLit); ok && nested != literal {
				return false
			}
			identifier, ok := node.(*ast.Ident)
			if !ok || !outerNames[identifier.Name] || inner[identifier.Name] || reported[identifier.Name] {
				return true
			}
			reported[identifier.Name] = true
			position := fileSet.Position(identifier.Pos())
			fmt.Printf("%s: worker closure in Nest handler %s captures %s; copy business data into the Task and use the callback parameter\n", position, handler, identifier.Name)
			findings++
			return true
		})
	}
	return findings
}

func isNestHandler(function *ast.FuncDecl) bool {
	if function == nil || function.Recv != nil {
		return false
	}
	if strings.HasPrefix(function.Name.Name, "handler") {
		return true
	}
	if function.Doc != nil {
		for _, comment := range function.Doc.List {
			if strings.Contains(comment.Text, "roost:nest") {
				return true
			}
		}
	}
	return false
}

func workerImportAliases(file *ast.File) map[string]bool {
	aliases := make(map[string]bool)
	for _, spec := range file.Imports {
		if strings.Trim(spec.Path.Value, "\"") != "github.com/tjbdwanghaibo/cube-core/worker" {
			continue
		}
		name := "worker"
		if spec.Name != nil {
			name = spec.Name.Name
		}
		aliases[name] = true
	}
	return aliases
}

func collectWorkerPools(file *ast.File, aliases map[string]bool) map[string]bool {
	pools := make(map[string]bool)
	ast.Inspect(file, func(node ast.Node) bool {
		switch typed := node.(type) {
		case *ast.Field:
			if !isWorkerPoolType(typed.Type, aliases) {
				return true
			}
			for _, name := range typed.Names {
				pools[name.Name] = true
			}
		case *ast.ValueSpec:
			if !isWorkerPoolType(typed.Type, aliases) {
				return true
			}
			for _, name := range typed.Names {
				pools[name.Name] = true
			}
		case *ast.AssignStmt:
			for index, rhs := range typed.Rhs {
				if !isWorkerPoolConstructor(rhs, aliases) || index >= len(typed.Lhs) {
					continue
				}
				if name, ok := typed.Lhs[index].(*ast.Ident); ok {
					pools[name.Name] = true
				}
			}
		}
		return true
	})
	return pools
}

func isWorkerPoolType(expression ast.Expr, aliases map[string]bool) bool {
	switch typed := expression.(type) {
	case *ast.StarExpr:
		return isWorkerPoolType(typed.X, aliases)
	case *ast.IndexExpr:
		return isWorkerPoolType(typed.X, aliases)
	case *ast.IndexListExpr:
		return isWorkerPoolType(typed.X, aliases)
	case *ast.SelectorExpr:
		pkg, ok := typed.X.(*ast.Ident)
		return ok && aliases[pkg.Name] && typed.Sel.Name == "Pool"
	default:
		return false
	}
}

func isWorkerPoolConstructor(expression ast.Expr, aliases map[string]bool) bool {
	call, ok := expression.(*ast.CallExpr)
	if !ok {
		return false
	}
	fun := call.Fun
	if indexed, ok := fun.(*ast.IndexExpr); ok {
		fun = indexed.X
	}
	selector, ok := fun.(*ast.SelectorExpr)
	if !ok || selector.Sel.Name != "NewPool" {
		return false
	}
	pkg, ok := selector.X.(*ast.Ident)
	return ok && aliases[pkg.Name]
}

func allowedWorkerPoolReceiver(expression ast.Expr, pools map[string]bool) bool {
	switch typed := expression.(type) {
	case *ast.Ident:
		return pools[typed.Name]
	case *ast.SelectorExpr:
		return pools[typed.Sel.Name]
	default:
		return false
	}
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
