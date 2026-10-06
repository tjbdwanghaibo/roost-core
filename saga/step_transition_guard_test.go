package saga

// saga 方向 ①（维护者第六轮决定）：协调器写记录全部经过 Engine.stepTransition，由它决定关闭哪个操作、是否开新一生，
// 并由它把请求交给 Store。“放弃关闭”在截止、人工 Compensate、定义缺失三个出口各漏写过一次（U-0280、
// RR-20261005-NC-250），都是出口自己拼 ApplyRequest 时少写了 CloseOperation。
//
// RR-20261006-14：原守卫按语法检查 Engine 方法，看不到包级 helper 改写参数里的请求再写入（helper 收到
// stepTransition 的结果，把 CloseOperation 清空后自己调 Store.Apply，守卫照样通过）。现在 stepTransition 自己调
// Store.Apply、只返回写入的记录，出口手里没有请求；这个守卫用 go/types 检查包内全部非测试代码（不只 Engine 方法），
// 三条规则一起保证请求只能是 stepTransition 的决定：
//
//  1. ApplyRequest（或其指针）类型的值只能在 stepTransition 里产生；别处只能读收到的参数（Store 实现读请求落库）。
//     复合字面量、new、零值变量、类型转换、函数返回、取下标、解引用等任何产生请求值的表达式都会报出。
//  2. stepTransition 之外不能改写请求：对请求或其字段 / 元素的赋值、自增、取地址都会报出（包括收到的参数）。
//  3. Store 的 Apply（参数是 ApplyRequest 的 Apply 方法，调用或取方法值）只在 stepTransition 里出现。
//
// 代际不在守卫里：stepTransition 按 before 与原因重写 after.Incarnation，出口写的值不起作用（见
// TestStepTransitionAloneDecidesTheIncarnation）。反射与 unsafe 不在检查范围内，包内没有对 ApplyRequest 用它们。

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestEveryCoordinatorWriteGoesThroughStepTransition(t *testing.T) {
	for _, violation := range stepTransitionGuardViolations(t) {
		t.Error(violation)
	}
}

// 代际只由 stepTransition 决定：出口在 after 上写的 Incarnation 被忽略，Resume 与补偿方向的人工 Compensate 才加一。
func TestStepTransitionAloneDecidesTheIncarnation(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	cases := []struct {
		name  string
		cause transitionCause
		phase Phase
		want  uint32
	}{
		{"timeout keeps the life", causeTimeout, PhaseForward, 3},
		{"dispatch keeps the life", causeDispatch, PhaseForward, 3},
		{"forward manual compensate keeps the life", causeManualCompensate, PhaseForward, 3},
		{"compensating manual compensate opens a life", causeManualCompensate, PhaseCompensate, 4},
		{"resume opens a life", causeResume, PhaseForward, 4},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newMemoryStore()
			before := Record{ID: "inc-1", Type: "gift", DefinitionVersion: 1, Status: StatusPending, Phase: tc.phase, Version: 1, Incarnation: 3, Data: []byte("{}"), CreatedAt: now, UpdatedAt: now, NextRunAt: now}
			if err := store.Create(ctx, before); err != nil {
				t.Fatal(err)
			}
			after := before.Clone()
			after.Version++
			after.Incarnation = 99
			written, _, err := (&Engine{store: store}).stepTransition(ctx, before, after, transition{cause: tc.cause})
			if err != nil {
				t.Fatal(err)
			}
			stored, err := store.Get(ctx, before.ID)
			if err != nil {
				t.Fatal(err)
			}
			if written.Incarnation != tc.want || stored.Incarnation != tc.want {
				t.Fatalf("cause %d from incarnation 3 with after.Incarnation=99: returned %d, stored %d, want %d", tc.cause, written.Incarnation, stored.Incarnation, tc.want)
			}
		})
	}
}

func stepTransitionGuardViolations(t *testing.T) []string {
	t.Helper()
	fset, files, info, pkg := typeCheckSagaSources(t)
	requestObject := pkg.Scope().Lookup("ApplyRequest")
	if requestObject == nil {
		t.Fatal("saga.ApplyRequest not found: the guard no longer sees the request type")
	}
	requestType := requestObject.Type()
	isRequest := func(typ types.Type) bool {
		if typ == nil {
			return false
		}
		if pointer, ok := typ.(*types.Pointer); ok {
			typ = pointer.Elem()
		}
		return types.Identical(typ, requestType)
	}

	var violations []string
	var transitionBody *ast.BlockStmt
	applyInTransition, parameterReads := 0, 0
	for _, file := range files {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			inTransition := fn.Name.Name == "stepTransition" && fn.Recv != nil && receiverName(fn.Recv) == "Engine"
			if inTransition {
				transitionBody = fn.Body
			}
			report := func(node ast.Node, what string) {
				violations = append(violations, fset.Position(node.Pos()).String()+": "+what+" in "+fn.Name.Name)
			}
			// 规则 2 的目标：沿 a.b[i].c / *p / (x) 向下，链上任何一段是请求就算改写请求。
			checkWrite := func(target ast.Expr, what string) {
				if inTransition {
					return // stepTransition 自己逐字段构造请求
				}
				for expr := target; expr != nil; {
					if isRequest(info.TypeOf(expr)) {
						report(target, what)
						return
					}
					switch x := expr.(type) {
					case *ast.ParenExpr:
						expr = x.X
					case *ast.SelectorExpr:
						expr = x.X
					case *ast.IndexExpr:
						expr = x.X
					case *ast.StarExpr:
						expr = x.X
					default:
						expr = nil
					}
				}
			}
			ast.Inspect(fn.Body, func(node ast.Node) bool {
				switch n := node.(type) {
				case *ast.AssignStmt:
					for _, lhs := range n.Lhs {
						if ident, ok := lhs.(*ast.Ident); ok && n.Tok == token.DEFINE && info.Defs[ident] != nil {
							continue // 新声明的变量由规则 1 检查它的值
						}
						checkWrite(lhs, "ApplyRequest modified")
					}
				case *ast.IncDecStmt:
					checkWrite(n.X, "ApplyRequest modified")
				case *ast.RangeStmt:
					if n.Tok == token.ASSIGN {
						for _, target := range []ast.Expr{n.Key, n.Value} {
							if target != nil {
								checkWrite(target, "ApplyRequest modified")
							}
						}
					}
				case *ast.UnaryExpr:
					if n.Op == token.AND {
						checkWrite(n.X, "address of an ApplyRequest taken")
					}
				case *ast.SelectorExpr:
					// 规则 3：Store 的 Apply，调用或方法值。
					if selection := info.Selections[n]; selection != nil && (selection.Kind() == types.MethodVal || selection.Kind() == types.MethodExpr) && n.Sel.Name == "Apply" && takesRequest(selection.Type(), isRequest) {
						if inTransition {
							applyInTransition++
						} else {
							report(n, "Store.Apply used outside stepTransition")
						}
					}
				case *ast.ValueSpec:
					for _, name := range n.Names {
						if object := info.Defs[name]; object != nil && isRequest(object.Type()) && !inTransition {
							report(name, "ApplyRequest variable declared outside stepTransition")
						}
					}
				}
				// 规则 1：产生请求值的表达式。
				expr, ok := node.(ast.Expr)
				if !ok || inTransition {
					return true
				}
				tv, ok := info.Types[expr]
				if !ok || tv.IsType() || !isRequest(tv.Type) {
					return true
				}
				switch x := expr.(type) {
				case *ast.ParenExpr:
					return true
				case *ast.Ident:
					if variable, ok := info.Uses[x].(*types.Var); ok && (variable.Kind() == types.ParamVar || variable.Kind() == types.RecvVar) {
						parameterReads++
						return true
					}
				}
				report(expr, "ApplyRequest value produced outside stepTransition")
				return true
			})
		}
	}
	// 守卫没有失明：看得到 stepTransition 里的 Store.Apply，也看得到 Store 实现读请求参数。
	if transitionBody == nil || applyInTransition == 0 {
		t.Fatalf("found no Store.Apply in Engine.stepTransition (found method: %v): the guard no longer sees the coordinator's write", transitionBody != nil)
	}
	if parameterReads == 0 {
		t.Fatal("found no read of an ApplyRequest parameter (MongoStore.Apply): the guard no longer resolves the request type")
	}
	return violations
}

func takesRequest(typ types.Type, isRequest func(types.Type) bool) bool {
	signature, ok := typ.(*types.Signature)
	if !ok {
		return false
	}
	for i := 0; i < signature.Params().Len(); i++ {
		if isRequest(signature.Params().At(i).Type()) {
			return true
		}
	}
	return false
}

func receiverName(fields *ast.FieldList) string {
	if fields == nil || len(fields.List) == 0 {
		return ""
	}
	expr := fields.List[0].Type
	if star, ok := expr.(*ast.StarExpr); ok {
		expr = star.X
	}
	if ident, ok := expr.(*ast.Ident); ok {
		return ident.Name
	}
	return ""
}

// typeCheckSagaSources 按编译器看到的文件集（go list 的 GoFiles，不含测试文件）给 saga 包做类型检查，依赖用
// go list -export 给出的导出数据。go 命令取运行本测试的工具链。
func typeCheckSagaSources(t *testing.T) (*token.FileSet, []*ast.File, *types.Info, *types.Package) {
	t.Helper()
	goTool := filepath.Join(runtime.GOROOT(), "bin", "go")
	if _, err := os.Stat(goTool); err != nil {
		goTool = "go"
	}
	command := exec.Command(goTool, "list", "-export", "-deps", "-json=ImportPath,Export,Dir,GoFiles,DepOnly", ".")
	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, err := command.Output()
	if err != nil {
		t.Fatalf("go list -export: %v\n%s", err, stderr.String())
	}
	type listed struct {
		ImportPath, Export, Dir string
		GoFiles                 []string
		DepOnly                 bool
	}
	exports := map[string]string{}
	var self listed
	decoder := json.NewDecoder(bytes.NewReader(output))
	for {
		var entry listed
		if err := decoder.Decode(&entry); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		exports[entry.ImportPath] = entry.Export
		if !entry.DepOnly {
			self = entry
		}
	}
	if len(self.GoFiles) == 0 {
		t.Fatalf("go list found no source files for the saga package")
	}
	fset := token.NewFileSet()
	files := make([]*ast.File, 0, len(self.GoFiles))
	for _, name := range self.GoFiles {
		file, err := parser.ParseFile(fset, filepath.Join(self.Dir, name), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, file)
	}
	lookup := func(path string) (io.ReadCloser, error) {
		export, ok := exports[path]
		if !ok || export == "" {
			return nil, errors.New("no export data for " + path)
		}
		return os.Open(export)
	}
	info := &types.Info{
		Types:      map[ast.Expr]types.TypeAndValue{},
		Defs:       map[*ast.Ident]types.Object{},
		Uses:       map[*ast.Ident]types.Object{},
		Selections: map[*ast.SelectorExpr]*types.Selection{},
	}
	config := types.Config{Importer: importer.ForCompiler(fset, "gc", lookup)}
	pkg, err := config.Check(self.ImportPath, fset, files, info)
	if err != nil {
		t.Fatalf("type-check saga: %v", err)
	}
	return fset, files, info, pkg
}
