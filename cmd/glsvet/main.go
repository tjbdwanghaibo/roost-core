// glsvet enforces the Nest handler concurrency boundary and also flags calls
// to goroutine-bound framework APIs from general `go` statements. A handler
// may hand explicit business DTOs to Nest effects or roost-core/worker, but it
// must never create or wrap its own goroutine.
//
// Usage:
//
//	go run github.com/tjbdwanghaibo/roost-core/cmd/glsvet ./...
//
// The check follows same-file named function calls from a handler, catches raw
// go statements and common async wrapper .Go calls, and recognizes direct
// roost-core/worker Pool variables as the allowed .Go implementation. Test
// files are skipped by default; pass -tests to include them. Stop/close
// functions with a ctx parameter that receive from a channel outside a select
// on that ctx get a review hint (-stophints, on by default), and business
// packages (a directory named game under the module root, -businessdirs) that
// read time.Now / Since / Until directly get a business-clock hint
// (-clockhints, D-L3); hints are printed but never counted as findings. Exit
// status is 1
// when any finding is reported and 2 when an argument could not be vetted (a
// missing directory or a file that does not parse).
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

// stopHints 控制停止入口的复审提示（stophints.go，维护者决定 A3）。提示只打印、不计入违例、
// 不改变退出码；默认打开，-stophints=false 关闭。
var stopHints = flag.Bool("stophints", true, "print review hints for stop/close functions that receive from a channel without their ctx (hints never fail the run)")

// hintCount 是本次运行打印的提示条数，只用于结尾的汇总。
var hintCount int

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
	// RR-20261005-NC-204：没能检查的输入（目录不存在、`<dir>/...` 的根不存在、文件解析失败）
	// 单独计数并以 2 退出。之前它们被当成“不是 Go 目录”按 0 个违例放行，CI 里路径拼错或
	// 包改名后门禁永远是绿的。
	failures := 0
	for _, argument := range flag.Args() {
		directories, err := expandArgument(argument)
		if err != nil {
			fmt.Fprintf(os.Stderr, "glsvet: %v\n", err)
			failures++
			continue
		}
		for _, directory := range directories {
			count, err := vetDirectory(fileSet, directory)
			findings += count
			if err != nil {
				fmt.Fprintf(os.Stderr, "glsvet: %s: %v\n", directory, err)
				failures++
			}
		}
	}
	if hintCount > 0 {
		fmt.Fprintf(os.Stderr, "glsvet: %d hint(s) for review; hints do not fail the run\n", hintCount)
	}
	if findings > 0 {
		fmt.Fprintf(os.Stderr, "glsvet: %d finding(s)\n", findings)
	}
	if failures > 0 {
		fmt.Fprintf(os.Stderr, "glsvet: %d input(s) could not be vetted\n", failures)
		os.Exit(2)
	}
	if findings > 0 {
		os.Exit(1)
	}
}

// expandArgument turns one command-line argument into the directories to vet.
// A plain argument must be an existing directory; `<root>/...` walks root,
// which must exist. Directories without Go files are returned too — they are
// not errors, vetDirectory finds nothing in them.
func expandArgument(argument string) ([]string, error) {
	if !strings.HasSuffix(argument, "/...") {
		info, err := os.Stat(argument)
		if err != nil {
			return nil, err
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("%s is not a directory", argument)
		}
		return []string{argument}, nil
	}
	root := strings.TrimSuffix(argument, "/...")
	if root == "" {
		root = "."
	}
	var directories []string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() {
			return nil
		}
		name := entry.Name()
		if strings.HasPrefix(name, ".") && path != root || name == "testdata" || name == "vendor" {
			return filepath.SkipDir
		}
		directories = append(directories, path)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return directories, nil
}

// vetDirectory reports the findings in one directory. parser.ParseDir returns
// no error for a directory without Go files; an error means the directory
// could not be read or a file did not parse, and the caller must not count
// that as clean. Packages that did parse are still vetted.
func vetDirectory(fileSet *token.FileSet, directory string) (int, error) {
	packages, parseErr := parser.ParseDir(fileSet, directory, func(info os.FileInfo) bool {
		return *includeTests || !strings.HasSuffix(info.Name(), "_test.go")
	}, 0)
	findings := 0
	hintCount += vetClockHints(fileSet, directory) // D-L3：只提示，不计入 findings
	for _, pkg := range packages {
		// Hints are advice, not findings: they are printed and do not change
		// the exit status (A1, like the A3 review prompts).
		for _, hint := range componentUndoHints(fileSet, pkg) {
			fmt.Println(hint)
		}
		voidAdmissionMethods := collectVoidAdmissionMethods(pkg)
		returningAdmissionMethods := collectReturningAdmissionMethods(pkg)
		var functions map[string][]*ast.FuncDecl
		if *stopHints {
			files := make([]*ast.File, 0, len(pkg.Files))
			for _, file := range pkg.Files {
				files = append(files, file)
			}
			functions = collectFunctions(files)
		}
		for _, file := range pkg.Files {
			findings += reportHandlerConcurrency(fileSet, file)
			findings += reportIgnoredAdmission(fileSet, file, voidAdmissionMethods, returningAdmissionMethods)
			if *stopHints {
				hintCount += reportStopHints(fileSet, file, functions) // 只提示，不计入 findings
			}
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
	return findings, parseErr
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
		// kit 合仓之后在 roost-core/kit/ 下，已经被上面那条前缀覆盖，所以这里
		// 不再单列一条——留着会是一行永远不触发的判断（三仓合一仓 P2）。
		// roost-skill 是更早一轮合进来之前的旧路径，留作漏改 import 的信号。
		if !strings.HasPrefix(path, "github.com/tjbdwanghaibo/roost-core/") &&
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
			fmt.Printf("%s: raw goroutine reachable from Nest handler %s; use nest.Emit(Effect) or roost-core/worker with explicit business parameters\n", position, handler)
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
			fmt.Printf("%s: async .%s wrapper reachable from Nest handler %s; only roost-core/worker Pool.Go/TryGo is allowed\n", position, selector.Sel.Name, handler)
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
		if strings.Trim(spec.Path.Value, "\"") != "github.com/tjbdwanghaibo/roost-core/worker" {
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

// componentUndoCalls are the ways to put an inverse operation into a Nest
// transaction by hand.
var componentUndoCalls = map[string]bool{
	"RecordUndo":      true,
	"RecordUndoToken": true,
	"DeferRollback":   true,
}

// componentUndoHints reports a component method that registers its own undo.
// The rule (A1, maintainer 2026-10-05,
// docs/feature/REFACTOR-2026-10-05-dao-unified-rollback.md): state a
// transaction changes lives in a DAO — a `nopersist` field when it must not be
// stored — so the DAO's rollback is the only rollback, and a component keeps
// no state of its own that needs one. A component is a type in this package
// that embeds ComponentBase or whose name ends in "Component". A DAO's own
// methods may record undo; that is how a generated setter works.
//
// 组件方法调用同包里直接登记 undo 的包级 helper 函数也提示，跟进一层（RR-20261006-13，与停止类函数的
// 提示同样做法，见 packageUndoHelpers）。
//
// skill.Runtime 是 A1 的明确例外（维护者决定 B4，2026-10-06）：它的冷却、ammo、cast、proc
// 账本与 revision 不进事务，handler 回滚后不回退，业务按此设计（docs/skill/
// skill-casting-and-combat.md）。Runtime 不是组件、也不登记 undo，这条提示本来就不会命中它，
// 所以不需要豁免；TestSkillPackagesGetNoComponentUndoHint 钉住 skill 各包零提示。
func componentUndoHints(fileSet *token.FileSet, pkg *ast.Package) []string {
	components := make(map[string]bool)
	for _, file := range pkg.Files {
		ast.Inspect(file, func(node ast.Node) bool {
			spec, ok := node.(*ast.TypeSpec)
			if !ok {
				return true
			}
			if strings.HasSuffix(spec.Name.Name, "Component") {
				components[spec.Name.Name] = true
				return true
			}
			if structType, ok := spec.Type.(*ast.StructType); ok {
				for _, field := range structType.Fields.List {
					if len(field.Names) == 0 && embedsComponentBase(field.Type) {
						components[spec.Name.Name] = true
					}
				}
			}
			return true
		})
	}
	undoHelpers := packageUndoHelpers(pkg)
	var hints []string
	for _, file := range pkg.Files {
		for _, declaration := range file.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok || function.Recv == nil || len(function.Recv.List) == 0 || function.Body == nil {
				continue
			}
			receiver := receiverTypeName(function.Recv.List[0].Type)
			if !components[receiver] {
				continue
			}
			ast.Inspect(function.Body, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				name := undoCallName(call)
				if componentUndoCalls[name] {
					hints = append(hints, fmt.Sprintf("%s: hint: component %s.%s registers its own undo (%s); keep transaction state in the DAO (a nopersist field if it must not be stored) so the DAO's rollback covers it",
						fileSet.Position(call.Pos()), receiver, function.Name.Name, name))
					return true
				}
				if ident, ok := call.Fun.(*ast.Ident); ok {
					if undo := undoHelpers[ident.Name]; undo != "" {
						hints = append(hints, fmt.Sprintf("%s: hint: component %s.%s registers undo through %s (%s); keep transaction state in the DAO (a nopersist field if it must not be stored) so the DAO's rollback covers it",
							fileSet.Position(call.Pos()), receiver, function.Name.Name, ident.Name, undo))
					}
				}
				return true
			})
		}
	}
	return hints
}

// undoCallName 返回调用的函数名（f(...) 或 x.f(...) 的 f），用来与 componentUndoCalls 比较。
func undoCallName(call *ast.CallExpr) string {
	switch fun := call.Fun.(type) {
	case *ast.SelectorExpr:
		return fun.Sel.Name
	case *ast.Ident:
		return fun.Name
	}
	return ""
}

// packageUndoHelpers 返回同包里直接登记 undo 的包级函数：函数名 → 它调用的第一个 undo 入口（RR-20261006-13）。
// 组件方法调用这样的 helper 与直接登记是同一个违例；之前只看组件方法体里的直接调用，把 RecordUndo 挪进 helper 就看不见。
// 与停止类函数的提示一样只跟一层。只看包级函数、不看方法：没有类型信息时 x.f() 按名字匹配会把组件调 DAO setter
// （DAO 方法自己登记 undo，A1 要求的正确写法）误报成违例；组件自己的方法已经各自被直接检查。
func packageUndoHelpers(pkg *ast.Package) map[string]string {
	helpers := make(map[string]string)
	for _, file := range pkg.Files {
		for _, declaration := range file.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok || function.Recv != nil || function.Body == nil || componentUndoCalls[function.Name.Name] {
				continue
			}
			ast.Inspect(function.Body, func(node ast.Node) bool {
				if helpers[function.Name.Name] != "" {
					return false
				}
				if call, ok := node.(*ast.CallExpr); ok && componentUndoCalls[undoCallName(call)] {
					helpers[function.Name.Name] = undoCallName(call)
					return false
				}
				return true
			})
		}
	}
	return helpers
}

func embedsComponentBase(expression ast.Expr) bool {
	switch typed := expression.(type) {
	case *ast.StarExpr:
		return embedsComponentBase(typed.X)
	case *ast.SelectorExpr:
		return typed.Sel.Name == "ComponentBase"
	case *ast.Ident:
		return typed.Name == "ComponentBase"
	}
	return false
}

func receiverTypeName(expression ast.Expr) string {
	switch typed := expression.(type) {
	case *ast.StarExpr:
		return receiverTypeName(typed.X)
	case *ast.Ident:
		return typed.Name
	case *ast.IndexExpr:
		return receiverTypeName(typed.X)
	case *ast.IndexListExpr:
		return receiverTypeName(typed.X)
	}
	return ""
}
