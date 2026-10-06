package skill

// O29（维护者第十二轮决定，2026-10-06，只改文案）：effect result 分支（以及 status 实例
// 选择的消费流程）不能挂起、不能启动带 on 回调的进程——规则是 effectResultBranchMaySuspend：
// wait、带间隔的 repeat、带 on 的效果。spawn 加不带回调的进程照常编译、分支执行时照常
// 启动进程。以前诊断写 “cannot suspend or start a process”，与规则不符。
//
// 承诺：result 分支里 spawn + 无回调进程编译并启动进程；带 on 的进程被拒绝，诊断文案
// 说的是“带 on 回调的进程”，不再说不能启动进程。

import (
	"strings"
	"testing"
)

func TestEffectResultBranchMayStartAProcessWithoutCallbacks(t *testing.T) {
	spawn := `{"flow":"effect","effect":{"type":"spawn","template":"deployable.trap","position":"$caster.position","count":1,"duration_ticks":5},"process":{"kind":"summon"}}`
	flow := `{"flow":"sequence","steps":[{"flow":"effect","effect":{"type":"heal","target":"$caster","amount":1},"result":{"success":` + spawn + `,"failure":` + spawn + `}},{"flow":"finish"}]}`
	program, environment := compileOwnedSkill(t, "result-branch-process", flow)
	runtime := NewRuntime(runtimeTestHost(environment), RuntimeOptions{})
	if _, err := runtime.Activate(program, CastInput{Caster: 1}); err != nil {
		t.Fatal(err)
	}
	if len(runtime.ownedProcesses) != 1 {
		t.Fatalf("owned processes = %d, want the summon started from the result branch", len(runtime.ownedProcesses))
	}
}

func TestEffectResultBranchDiagnosticNamesProcessCallbacks(t *testing.T) {
	spawn := `{"flow":"effect","effect":{"type":"spawn","template":"deployable.trap","position":"$caster.position","count":1,"duration_ticks":5},"process":{"kind":"summon"},"on":{"end":{"flow":"effect","effect":{"type":"heal","target":"$owner","amount":1}}}}`
	flow := `{"flow":"sequence","steps":[{"flow":"effect","effect":{"type":"heal","target":"$caster","amount":1},"result":{"success":` + spawn + `}},{"flow":"finish"}]}`
	json := strings.Replace(minimalSkillJSON, `{"flow":"finish","reason":"done"}`, flow, 1)
	_, diagnostics := compileToArtifacts(mustParseJSON(t, json), DefaultCompileEnvironment())
	for _, diagnostic := range diagnostics {
		if !strings.HasPrefix(diagnostic.Message, "effect result branches") {
			continue
		}
		if !strings.Contains(diagnostic.Message, "with on callbacks") {
			t.Fatalf("result branch diagnostic %q must name process on callbacks", diagnostic.Message)
		}
		return
	}
	t.Fatalf("no effect result branch diagnostic: %#v", diagnostics)
}
