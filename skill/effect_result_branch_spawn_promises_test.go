package skill

// O29（维护者第十二轮决定，2026-10-06，只改文案）：effect result 分支（以及 status 实例
// 选择的消费流程）不能挂起、不能启动带 on 回调的衍生物——规则是 effectResultBranchMaySuspend：
// wait、带间隔的 repeat、带 on 的效果。spawn 加不带回调的衍生物照常编译、分支执行时照常
// 启动衍生物。以前诊断写 “cannot suspend or start a spawn”，与规则不符。
//
// 承诺：result 分支里 spawn + 无回调衍生物编译并启动衍生物；带 on 的衍生物被拒绝，诊断文案
// 说的是“带 on 回调的衍生物”，不再说不能启动衍生物。

import (
	"strings"
	"testing"
)

func TestEffectResultBranchMayStartASpawnWithoutCallbacks(t *testing.T) {
	spawn := `{"flow":"effect","effect":{"type":"spawn","template":"deployable.trap","position":"$caster.position","count":1,"duration_ticks":5},"spawn":{"kind":"summon"}}`
	flow := `{"flow":"sequence","steps":[{"flow":"effect","effect":{"type":"heal","target":"$caster","amount":1},"result":{"success":` + spawn + `,"failure":` + spawn + `}},{"flow":"finish"}]}`
	program, environment := compileOwnedSkill(t, "result-branch-spawn", flow)
	runtime := NewRuntime(runtimeTestHost(environment), RuntimeOptions{})
	if _, err := runtime.Activate(program, CastInput{Caster: 1}); err != nil {
		t.Fatal(err)
	}
	if len(runtime.ownedSpawns) != 1 {
		t.Fatalf("owned spawns = %d, want the summon started from the result branch", len(runtime.ownedSpawns))
	}
}

func TestEffectResultBranchDiagnosticNamesSpawnCallbacks(t *testing.T) {
	spawn := `{"flow":"effect","effect":{"type":"spawn","template":"deployable.trap","position":"$caster.position","count":1,"duration_ticks":5},"spawn":{"kind":"summon"},"on":{"end":{"flow":"effect","effect":{"type":"heal","target":"$owner","amount":1}}}}`
	flow := `{"flow":"sequence","steps":[{"flow":"effect","effect":{"type":"heal","target":"$caster","amount":1},"result":{"success":` + spawn + `}},{"flow":"finish"}]}`
	json := strings.Replace(minimalSkillJSON, `{"flow":"finish","reason":"done"}`, flow, 1)
	_, diagnostics := compileToArtifacts(mustParseJSON(t, json), DefaultCompileEnvironment())
	for _, diagnostic := range diagnostics {
		if !strings.HasPrefix(diagnostic.Message, "effect result branches") {
			continue
		}
		if !strings.Contains(diagnostic.Message, "with on callbacks") {
			t.Fatalf("result branch diagnostic %q must name spawn on callbacks", diagnostic.Message)
		}
		return
	}
	t.Fatalf("no effect result branch diagnostic: %#v", diagnostics)
}
