package main

import (
	"fmt"
	"go/ast"
	"go/token"
)

// 识别框架同步入口及常见显式等待。Await 的 work 是 I/O 段，resume 仍是业务段。
// 这是语法检查，不能证明任意第三方函数或间接调用不阻塞；运行时入口检查仍是主防线。
func inspectBusinessWaits(fset *token.FileSet, body ast.Node, handler string) int {
	findings := 0
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		fun := call.Fun
		switch f := fun.(type) {
		case *ast.IndexExpr:
			fun = f.X
		case *ast.IndexListExpr:
			fun = f.X
		}
		sel, ok := fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if sel.Sel.Name == "Await" {
			// work 可以等待 I/O；只检查匿名恢复段，动态目标选择器也不得等待。
			if len(call.Args) < 2 {
				return false
			}
			for _, arg := range call.Args[1:] {
				findings += inspectBusinessWaits(fset, arg, handler)
			}
			return false
		}
		switch sel.Sel.Name {
		case "Request", "RequestMulti", "RequestMultiGroup", "RunLocal", "Wait", "Sleep":
			fmt.Printf("%s: synchronous .%s reachable from business handler %s; return nest.Await(work, resume) instead\n", fset.Position(call.Pos()), sel.Sel.Name, handler)
			findings++
		}
		return true
	})
	return findings
}
