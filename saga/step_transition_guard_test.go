package saga

// saga 方向 ①（维护者第六轮决定）：协调器写记录的出口全部经过 stepTransition，由它决定关闭哪个操作、是否开新一生。
// “放弃关闭”在截止、人工 Compensate、定义缺失三个出口各漏写过一次（U-0280、RR-20261005-NC-250），都是出口自己拼
// ApplyRequest 时少写了 CloseOperation。这个守卫在源码层面钉住：包内非测试代码只有 stepTransition 构造 ApplyRequest、
// 改 Incarnation，Engine 的每个 store.Apply 调用都直接传 stepTransition(...) 的结果。新出口手拼请求时这里报出位置。

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

func TestEveryCoordinatorWriteGoesThroughStepTransition(t *testing.T) {
	violations := stepTransitionGuardViolations(t, ".")
	for _, violation := range violations {
		t.Error(violation)
	}
}

// 守卫自己的负对照（发版前复审）：testdata/stepguard 里两个出口不手拼字面量、不改 Incarnation，却绕过了
// stepTransition 的决定——改 stepTransition 返回的请求、逐字段拼请求经 store 的别名写入。守卫必须逐个报出。
func TestStepTransitionGuardSeesBypassesWithoutALiteral(t *testing.T) {
	violations := stepTransitionGuardViolations(t, "testdata/stepguard")
	for _, exit := range []string{"mutatedTransitionExit", "aliasedStoreExit"} {
		found := false
		for _, violation := range violations {
			if strings.HasSuffix(violation, " in "+exit) || strings.Contains(violation, " in "+exit+" ") {
				found = true
			}
		}
		if !found {
			t.Errorf("guard reported nothing for %s (violations: %q); the exit bypasses stepTransition", exit, violations)
		}
	}
}

func stepTransitionGuardViolations(t *testing.T, dir string) []string {
	t.Helper()
	fset := token.NewFileSet()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var violations []string
	applyCalls := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, dir+"/"+name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			inTransition := fn.Recv == nil && fn.Name.Name == "stepTransition"
			engineMethod := fn.Recv != nil && receiverName(fn.Recv) == "Engine"
			transitionResults := transitionResultNames(fn.Body)
			report := func(pos token.Pos, what string) {
				violations = append(violations, fset.Position(pos).String()+": "+what+" in "+fn.Name.Name)
			}
			ast.Inspect(fn.Body, func(node ast.Node) bool {
				switch n := node.(type) {
				case *ast.CompositeLit:
					if isApplyRequestType(n.Type) && !inTransition {
						report(n.Pos(), "ApplyRequest built outside stepTransition")
					}
				case *ast.ValueSpec:
					// var request ApplyRequest 之后逐字段赋值，同样是手拼请求。
					if isApplyRequestType(n.Type) && !inTransition {
						report(n.Pos(), "ApplyRequest declared outside stepTransition")
					}
				case *ast.AssignStmt:
					for _, lhs := range n.Lhs {
						if isIncarnationField(lhs) && !inTransition {
							report(n.Pos(), "Incarnation assigned outside stepTransition")
						}
						// 改 stepTransition 的结果（关闭哪个操作、租约、outbox……）等于推翻它的决定。
						if base := selectorBase(lhs); base != nil && base.Obj != nil && transitionResults[base.Obj] && !inTransition {
							report(n.Pos(), "result of stepTransition modified")
						}
					}
				case *ast.IncDecStmt:
					if isIncarnationField(n.X) && !inTransition {
						report(n.Pos(), "Incarnation changed outside stepTransition")
					}
				case *ast.UnaryExpr:
					if ident, ok := n.X.(*ast.Ident); ok && n.Op == token.AND && ident.Obj != nil && transitionResults[ident.Obj] && !inTransition {
						report(n.Pos(), "address of a stepTransition result taken")
					}
				case *ast.CallExpr:
					if ident, ok := n.Fun.(*ast.Ident); ok && ident.Name == "new" && len(n.Args) == 1 && isApplyRequestType(n.Args[0]) && !inTransition {
						report(n.Pos(), "ApplyRequest allocated outside stepTransition")
					}
					if !engineMethod {
						return true
					}
					if !isStoreApply(n) {
						// Store 的别名（store := e.store; store.Apply(...)）让下面的参数检查看不到这次写入。
						if selector, ok := n.Fun.(*ast.SelectorExpr); ok && selector.Sel.Name == "Apply" && len(n.Args) == 2 {
							report(n.Pos(), "Apply called on something other than e.store")
						}
						return true
					}
					applyCalls++
					if len(n.Args) != 2 || !isStepTransitionCall(n.Args[1]) && !isRequestFromTransition(fn.Body, n.Args[1]) {
						violations = append(violations, fset.Position(n.Pos()).String()+": store.Apply in "+fn.Name.Name+" does not take stepTransition(...)")
					}
				}
				return true
			})
		}
	}
	if applyCalls == 0 {
		t.Fatal("found no store.Apply call in Engine methods: the guard no longer sees the coordinator")
	}
	return violations
}

func isApplyRequestType(expr ast.Expr) bool {
	if star, ok := expr.(*ast.StarExpr); ok {
		expr = star.X
	}
	ident, ok := expr.(*ast.Ident)
	return ok && ident.Name == "ApplyRequest"
}

// selectorBase 返回 a.b.c 的根标识符 a；不是选择器时返回 nil。
func selectorBase(expr ast.Expr) *ast.Ident {
	selector, ok := expr.(*ast.SelectorExpr)
	if !ok {
		return nil
	}
	for {
		switch x := selector.X.(type) {
		case *ast.Ident:
			return x
		case *ast.SelectorExpr:
			selector = x
		default:
			return nil
		}
	}
}

// transitionResultNames 是函数里被 stepTransition(...) 的结果赋值过的变量（按解析器解析出的对象区分作用域：
// Resume 的参数 request 与循环里 request := stepTransition(...) 是两个变量）。
func transitionResultNames(body *ast.BlockStmt) map[*ast.Object]bool {
	names := make(map[*ast.Object]bool)
	ast.Inspect(body, func(node ast.Node) bool {
		assign, ok := node.(*ast.AssignStmt)
		if !ok || len(assign.Lhs) != len(assign.Rhs) {
			return true
		}
		for i, lhs := range assign.Lhs {
			if ident, ok := lhs.(*ast.Ident); ok && ident.Obj != nil && isStepTransitionCall(assign.Rhs[i]) {
				names[ident.Obj] = true
			}
		}
		return true
	})
	return names
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

func isIncarnationField(expr ast.Expr) bool {
	selector, ok := expr.(*ast.SelectorExpr)
	return ok && selector.Sel.Name == "Incarnation"
}

// isStoreApply 匹配 e.store.Apply(...)。
func isStoreApply(call *ast.CallExpr) bool {
	selector, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || selector.Sel.Name != "Apply" {
		return false
	}
	inner, ok := selector.X.(*ast.SelectorExpr)
	return ok && inner.Sel.Name == "store"
}

func isStepTransitionCall(expr ast.Expr) bool {
	call, ok := expr.(*ast.CallExpr)
	if !ok {
		return false
	}
	ident, ok := call.Fun.(*ast.Ident)
	return ok && ident.Name == "stepTransition"
}

// isRequestFromTransition 允许 request := stepTransition(...) 之后 store.Apply(ctx, request)：变量在同一函数里
// 只被 stepTransition 的结果赋值过。
func isRequestFromTransition(body *ast.BlockStmt, arg ast.Expr) bool {
	ident, ok := arg.(*ast.Ident)
	if !ok {
		return false
	}
	assigned, fromTransition := 0, 0
	ast.Inspect(body, func(node ast.Node) bool {
		assign, ok := node.(*ast.AssignStmt)
		if !ok {
			return true
		}
		for i, lhs := range assign.Lhs {
			if name, ok := lhs.(*ast.Ident); ok && name.Name == ident.Name {
				assigned++
				if len(assign.Rhs) == len(assign.Lhs) && isStepTransitionCall(assign.Rhs[i]) {
					fromTransition++
				}
			}
		}
		return true
	})
	return assigned > 0 && assigned == fromTransition
}
