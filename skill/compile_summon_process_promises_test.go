package skill

// O22（维护者第十二轮决定，2026-10-06）：summon 进程的寿命是 spawn 效果的 duration_ticks
// （编译期要求为正），进程自己的 duration_ticks 从来不被读取（process_owned.go 只在
// 带 motion / area 的进程上用模板时长），以前却连负数都照样编译通过。现在 summon 上写
// duration_ticks 直接编译期拒绝；area 成员字段同理（summon 不做成员检测，以前同样被
// 提前返回跳过检查，写了 area 运行期反而拿 0 时长报 ErrProgramInvariant）。
//
// 承诺：summon 上写 duration_ticks（正、零以外的任何值）或 area / interval_ticks /
// emit_leave_on_stop 是 MOTION_INVALID；不写时照常编译，寿命取 spawn 的时长。

import (
	"testing"
)

func TestSummonProcessRejectsFieldsItNeverReads(t *testing.T) {
	area := `"area":{"from":"$caster","kind":"entity","shape":{"type":"circle","radius":4},"filters":[{"type":"targetable"}],"order":{"by":"stable_id","direction":"asc"},"limit":2}`
	for name, process := range map[string]string{
		"positive duration":  `"kind":"summon","duration_ticks":5`,
		"negative duration":  `"kind":"summon","duration_ticks":-3`,
		"area":               `"kind":"summon",` + area,
		"interval":           `"kind":"summon","interval_ticks":1`,
		"emit leave on stop": `"kind":"summon","emit_leave_on_stop":true`,
	} {
		t.Run(name, func(t *testing.T) {
			_, diagnostics := compileToArtifacts(mustParseJSON(t, motionSkillJSON(process)), DefaultCompileEnvironment())
			requireDiagnostic(t, diagnostics, DiagnosticMotionInvalid)
		})
	}
}

func TestSummonProcessWithoutDurationCompilesAndLivesForTheSpawnDuration(t *testing.T) {
	flow := `{"flow":"sequence","steps":[{"flow":"effect","effect":{"type":"spawn","template":"deployable.trap","position":"$caster.position","count":1,"duration_ticks":10},"process":{"kind":"summon"}},{"flow":"finish"}]}`
	program, environment := compileOwnedSkill(t, "summon-spawn-duration", flow)
	runtime := NewRuntime(runtimeTestHost(environment), RuntimeOptions{})
	if _, err := runtime.Activate(program, CastInput{Caster: 1}); err != nil {
		t.Fatal(err)
	}
	if len(runtime.ownedProcesses) != 1 {
		t.Fatalf("owned processes = %d, want the one summon", len(runtime.ownedProcesses))
	}
	for _, process := range runtime.ownedProcesses {
		if process.EndTick-process.StartTick != 10 {
			t.Fatalf("summon lives %d ticks, want the spawn duration 10", process.EndTick-process.StartTick)
		}
	}
}
