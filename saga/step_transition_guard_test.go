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
			ast.Inspect(fn.Body, func(node ast.Node) bool {
				switch n := node.(type) {
				case *ast.CompositeLit:
					if ident, ok := n.Type.(*ast.Ident); ok && ident.Name == "ApplyRequest" && !inTransition {
						violations = append(violations, fset.Position(n.Pos()).String()+": ApplyRequest built outside stepTransition in "+fn.Name.Name)
					}
				case *ast.AssignStmt:
					for _, lhs := range n.Lhs {
						if isIncarnationField(lhs) && !inTransition {
							violations = append(violations, fset.Position(n.Pos()).String()+": Incarnation assigned outside stepTransition in "+fn.Name.Name)
						}
					}
				case *ast.IncDecStmt:
					if isIncarnationField(n.X) && !inTransition {
						violations = append(violations, fset.Position(n.Pos()).String()+": Incarnation changed outside stepTransition in "+fn.Name.Name)
					}
				case *ast.CallExpr:
					if !engineMethod || !isStoreApply(n) {
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
