package main

// 停止入口复审提示（维护者决定 A3 ③，2026-10-05）：只提示、不计入违例、不影响退出码。
//
// “三步停机”第②步要求在调用方 ctx 内等待排空。带 ctx 参数的停止类函数里，不受 ctx 约束的等待会让
// 停机越过预算（RR-20261005-NC-173 的裸 <-done、NC-83 的会话关闭派发器）。这里按语法找一类形状：
// 不在 select 里的通道接收 `<-ch`，或所在 select 既没有 `<-ctx.Done()` 分支也没有 default。
//
// 另外跟进一层同包调用：停止函数调用 f() / receiver.f() 而没有把 ctx 传下去，f 不带 ctx 参数且 f 里有
// 这样的接收时，在调用处提示（NC-173 修前 Deregister(ctx) → waitLoopDone() → 裸 <-done 的形状）。
// release* 视为归还信号量令牌（<-sem），不跟进。跳过函数字面量（`go func(){ wg.Wait(); close(done) }()` 是
// 把无界等待转成可 select 的通道的标准写法）和 `if ctx == nil { ... }` 分支（nil ctx 显式表示不限时）。
// 请确认这个通道一定会在预算内关闭，或改用 select ctx.Done() / internal/operation.Lifetime.Wait(ctx)。
//
// 没有加 Mutex.Lock 提示：2026-10-05 在全仓（非测试文件）实测，带 ctx 的停止类函数里无参 Lock / RLock
// 共 33 处，全部是读写几个字段的短临界区，没有一处是会被在途工作长期持有的锁；没有类型与
// 持有时长信息的语法检查分不出两者，误报率约 100%，只会让提示被忽略。锁被长期持有的风险由停机契约
// 测试骨架（internal/stopcontract）在行为上覆盖。

import (
	"fmt"
	"go/ast"
	"go/token"
	"strings"
)

// stopFunctionPrefixes 是停止类函数名的前缀（导出与未导出都算）。
var stopFunctionPrefixes = []string{"stop", "close", "shutdown", "deregister", "drain"}

func isStopFunctionName(name string) bool {
	lower := strings.ToLower(name)
	for _, prefix := range stopFunctionPrefixes {
		if strings.HasPrefix(lower, prefix) {
			return true
		}
	}
	return false
}

// contextParam 返回函数的 context.Context 参数名；没有时返回 ""。
func contextParam(function *ast.FuncDecl) string {
	if function.Type.Params == nil {
		return ""
	}
	for _, field := range function.Type.Params.List {
		selector, ok := field.Type.(*ast.SelectorExpr)
		if !ok || selector.Sel.Name != "Context" {
			continue
		}
		if pkg, ok := selector.X.(*ast.Ident); !ok || pkg.Name != "context" {
			continue
		}
		for _, name := range field.Names {
			if name.Name != "_" {
				return name.Name
			}
		}
	}
	return ""
}

// collectFunctions 按名字收集包内的函数与方法声明（方法只按名字，不区分接收者：这是提示，宁可多看一处）。
func collectFunctions(files []*ast.File) map[string][]*ast.FuncDecl {
	functions := make(map[string][]*ast.FuncDecl)
	for _, file := range files {
		for _, declaration := range file.Decls {
			if function, ok := declaration.(*ast.FuncDecl); ok && function.Body != nil {
				functions[function.Name.Name] = append(functions[function.Name.Name], function)
			}
		}
	}
	return functions
}

// reportStopHints 打印 file 里停止类函数的复审提示，返回提示条数（不计入违例）。functions 是同包的
// 函数表：停止函数调用同包里不带 ctx 的函数、而那个函数做裸通道接收时（NC-173 修前 Deregister 调用
// waitLoopDone 的形状），在调用处提示。只跟一层。
func reportStopHints(fileSet *token.FileSet, file *ast.File, functions map[string][]*ast.FuncDecl) int {
	hints := 0
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok || function.Body == nil || !isStopFunctionName(function.Name.Name) {
			continue
		}
		ctxName := contextParam(function)
		if ctxName == "" {
			continue
		}
		for _, node := range unboundedWaitsIn(function.Body, ctxName) {
			position := fileSet.Position(node.Pos())
			fmt.Printf("%s: hint: %s(%s) receives from a channel without its ctx; confirm it is closed within the stop budget, or select on %s.Done() / use operation.Lifetime.Wait (A3 stop contract)\n",
				position, function.Name.Name, ctxName, ctxName)
			hints++
		}
		for _, call := range callsWithoutContext(function.Body, ctxName) {
			name := calledName(call, receiverName(function))
			if name == "" || strings.HasPrefix(name, "release") {
				// release* 是归还信号量令牌的惯用写法（<-sem 归还已占用的槽，不会阻塞），不提示。
				continue
			}
			for _, callee := range functions[name] {
				if callee == function || contextParam(callee) != "" || len(unboundedWaitsIn(callee.Body, "")) == 0 {
					continue
				}
				position := fileSet.Position(call.Pos())
				fmt.Printf("%s: hint: %s(%s) calls %s, which receives from a channel and cannot see the ctx; pass the ctx and select on it, or use operation.Lifetime.Wait (A3 stop contract)\n",
					position, function.Name.Name, ctxName, name)
				hints++
				break
			}
		}
	}
	return hints
}

// callsWithoutContext 返回 root 里（不含函数字面量与 nil ctx 分支）没有把 ctx 作为实参传入的调用。
func callsWithoutContext(root ast.Node, ctxName string) []*ast.CallExpr {
	var calls []*ast.CallExpr
	ast.Inspect(root, func(node ast.Node) bool {
		switch statement := node.(type) {
		case *ast.FuncLit:
			return false
		case *ast.IfStmt:
			if isNilCheck(statement.Cond, ctxName) {
				if statement.Else != nil {
					calls = append(calls, callsWithoutContext(statement.Else, ctxName)...)
				}
				return false
			}
		case *ast.CallExpr:
			passesContext := false
			for _, argument := range statement.Args {
				if ident, ok := argument.(*ast.Ident); ok && ident.Name == ctxName {
					passesContext = true
				}
			}
			if !passesContext {
				calls = append(calls, statement)
			}
		}
		return true
	})
	return calls
}

// calledName 返回同包调用 f(...) 或 receiver.f(...) 的 f；其他调用（a.b.f()、pkg.f()）返回 ""——没有
// 类型信息时按名字匹配它们误报太多（例如 a.Client.Close 撞上同包另一个类型的 Close）。
func calledName(call *ast.CallExpr, receiver string) string {
	switch fun := call.Fun.(type) {
	case *ast.Ident:
		return fun.Name
	case *ast.SelectorExpr:
		if ident, ok := fun.X.(*ast.Ident); ok && receiver != "" && ident.Name == receiver {
			return fun.Sel.Name
		}
	}
	return ""
}

// receiverName 返回方法接收者的变量名；函数或匿名接收者返回 ""。
func receiverName(function *ast.FuncDecl) string {
	if function.Recv == nil || len(function.Recv.List) == 0 || len(function.Recv.List[0].Names) == 0 {
		return ""
	}
	return function.Recv.List[0].Names[0].Name
}

// unboundedWaitsIn 返回 root 里不受 ctx 约束的通道接收。
func unboundedWaitsIn(root ast.Node, ctxName string) []ast.Node {
	var found []ast.Node
	bounded := map[ast.Node]bool{} // 受 ctx / default 约束的 select 里的接收
	ast.Inspect(root, func(node ast.Node) bool {
		switch statement := node.(type) {
		case *ast.FuncLit:
			return false
		case *ast.SelectStmt:
			if selectIsBounded(statement, ctxName) {
				for _, clause := range statement.Body.List {
					if comm, ok := clause.(*ast.CommClause); ok && comm.Comm != nil {
						ast.Inspect(comm.Comm, func(inner ast.Node) bool {
							if unary, ok := inner.(*ast.UnaryExpr); ok && unary.Op == token.ARROW {
								bounded[unary] = true
							}
							return true
						})
					}
				}
			}
		case *ast.UnaryExpr:
			if statement.Op == token.ARROW && !bounded[statement] && !isContextDone(statement.X, ctxName) {
				found = append(found, statement)
			}
		case *ast.IfStmt:
			if isNilCheck(statement.Cond, ctxName) {
				// nil ctx 的分支显式不限时；只检查 else 与之后的代码。
				if statement.Else != nil {
					found = append(found, unboundedWaitsIn(statement.Else, ctxName)...)
				}
				return false
			}
		}
		return true
	})
	return found
}

// selectIsBounded 报告 select 是否有 `<-ctx.Done()` 分支或 default 分支。
func selectIsBounded(statement *ast.SelectStmt, ctxName string) bool {
	for _, clause := range statement.Body.List {
		comm, ok := clause.(*ast.CommClause)
		if !ok {
			continue
		}
		if comm.Comm == nil {
			return true // default
		}
		var receive ast.Expr
		switch c := comm.Comm.(type) {
		case *ast.ExprStmt:
			receive = c.X
		case *ast.AssignStmt:
			if len(c.Rhs) == 1 {
				receive = c.Rhs[0]
			}
		}
		if unary, ok := receive.(*ast.UnaryExpr); ok && unary.Op == token.ARROW && isContextDone(unary.X, ctxName) {
			return true
		}
	}
	return false
}

// isContextDone 判断 expression 是否为 ctxName.Done()。ctxName 为空（被跟进的 helper 看不到调用方的
// ctx）时，任何 x.Done() 都算：helper 自己受某个 ctx 约束，由它的所有者负责。
func isContextDone(expression ast.Expr, ctxName string) bool {
	call, ok := expression.(*ast.CallExpr)
	if !ok || len(call.Args) != 0 {
		return false
	}
	selector, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || selector.Sel.Name != "Done" {
		return false
	}
	if ctxName == "" {
		return true
	}
	ident, ok := selector.X.(*ast.Ident)
	return ok && ident.Name == ctxName
}

// isNilCheck 判断 cond 是否为 `ctxName == nil`。
func isNilCheck(cond ast.Expr, ctxName string) bool {
	binary, ok := cond.(*ast.BinaryExpr)
	if !ok || binary.Op != token.EQL {
		return false
	}
	ident, ok := binary.X.(*ast.Ident)
	null, okNil := binary.Y.(*ast.Ident)
	return ok && okNil && ident.Name == ctxName && null.Name == "nil"
}
