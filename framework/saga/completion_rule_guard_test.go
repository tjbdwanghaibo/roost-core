package saga

// v1.23.1 起 saga 的完成判定是一条统一规则（docs/feature/SAGA-COMPLETION-RULE-UNIFIED-2026-10-07.md）：成功是操作的结论，
// 只要这个操作开过、还没有带结果关闭就接收；拒绝是本生这个操作的结论、可重试失败是一次尝试的结论，只在协调器正等着时接收。
// 之前 Complete 按记录状态逐条列举（U-0280、B1、方向 ③④、RR-20261006-42），两天补了五次。两个守卫：
//
//  1. 判定表：judgeCompletion 在（结论种类 × 代际 × 记录位置）的全部组合上等于方案第 1 节的表；
//  2. 结构：Complete 只经 judgeCompletion 判定，自己不读记录状态、不调用 positionedAt / openOperation——再按记录状态加一条
//     接收分支就会被报出，要改规则只能改 judgeCompletion 与方案。

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

func TestJudgeCompletionIsTheUnifiedRule(t *testing.T) {
	const current uint32 = 2
	operation := operationKey("g", PhaseForward, 1)
	other := operationKey("g", PhaseForward, 0)
	waitingFor := commandID(operation, current, 3)
	positions := map[string]Record{
		"waiting":     {ID: "g", Phase: PhaseForward, Step: 1, Status: StatusWaiting, OperationKey: operation, CommandID: waitingFor, Attempt: 3},
		"backoff":     {ID: "g", Phase: PhaseForward, Step: 1, Status: StatusPending, Attempt: 3},
		"to dispatch": {ID: "g", Phase: PhaseForward, Step: 1, Status: StatusPending},
		"elsewhere":   {ID: "g", Phase: PhaseForward, Step: 0, Status: StatusWaiting, OperationKey: other, CommandID: commandID(other, current, 1), Attempt: 1},
	}
	incarnations := map[string]uint32{"older": current - 1, "same": current, "newer": current + 1}
	kinds := []string{"success", "refusal", "retryable of the waited attempt", "retryable of an earlier attempt"}

	// want 按方案第 1 节的表逐行写，不复用 judgeCompletion 的分支。
	want := func(kind, life, position string) completionVerdict {
		if life == "newer" {
			return verdictStaleIncarnation // 0
		}
		if kind == "success" {
			opened := (life == "same" && (position == "waiting" || position == "backoff")) || (life == "older" && position != "elsewhere")
			if opened {
				return verdictAccept // S
			}
			return verdictHistory // S'
		}
		if life == "older" {
			return verdictStaleIncarnation // R/F-old
		}
		if position != "waiting" {
			return verdictHistory // R/F-gone
		}
		if kind == "retryable of an earlier attempt" {
			return verdictStaleAttempt // F-stale
		}
		return verdictAccept // R / F
	}

	for position, base := range positions {
		for life, incarnation := range incarnations {
			for _, kind := range kinds {
				record := base
				record.Incarnation = current
				completion := Completion{SagaID: "g", IdempotencyKey: operation, CommandID: commandID(operation, incarnation, 3)}
				switch kind {
				case "success":
					completion.Success = true
				case "retryable of the waited attempt":
					completion.Retryable = true
					if life == "same" {
						completion.CommandID = waitingFor
					}
				case "retryable of an earlier attempt":
					completion.Retryable = true
					completion.CommandID = commandID(operation, incarnation, 2)
				}
				got := judgeCompletion(record, completion, commandIDIncarnation(operation, completion.CommandID))
				if expected := want(kind, life, position); got != expected {
					t.Errorf("%s from the %s incarnation, record %s: verdict %d, want %d", kind, life, position, got, expected)
				}
			}
		}
	}
}

func TestCompleteJudgesOnlyThroughJudgeCompletion(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "engine.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	forbiddenFields := map[string]bool{"Status": true, "OperationKey": true, "CommandID": true, "Incarnation": true, "Retryable": true, "Attempt": true}
	forbiddenCalls := map[string]bool{"positionedAt": true, "openOperation": true}
	judges := 0
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		inComplete := fn.Name.Name == "Complete" && fn.Recv != nil
		ast.Inspect(fn.Body, func(node ast.Node) bool {
			switch n := node.(type) {
			case *ast.CallExpr:
				ident, ok := n.Fun.(*ast.Ident)
				if !ok {
					return true
				}
				if inComplete && ident.Name == "judgeCompletion" {
					judges++
				}
				if inComplete && forbiddenCalls[ident.Name] {
					t.Errorf("Complete calls %s: the acceptance rule lives only in judgeCompletion", ident.Name)
				}
				if ident.Name == "positionedAt" && fn.Name.Name != "judgeCompletion" {
					t.Errorf("%s calls positionedAt: only judgeCompletion decides acceptance from the record position", fn.Name.Name)
				}
			case *ast.SelectorExpr:
				if x, ok := n.X.(*ast.Ident); ok && inComplete && x.Name == "record" && forbiddenFields[n.Sel.Name] {
					t.Errorf("Complete reads record.%s: the acceptance rule lives only in judgeCompletion", n.Sel.Name)
				}
			}
			return true
		})
	}
	if judges != 1 {
		t.Fatalf("Complete calls judgeCompletion %d times, want exactly one decision entry", judges)
	}
}
