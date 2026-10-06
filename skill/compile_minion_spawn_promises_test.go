package skill

// O22（维护者第十二轮决定，2026-10-06）：minion 衍生物的寿命是召唤效果的 duration_ticks
// （编译期要求为正），衍生物自己的 duration_ticks 从来不被读取（spawn_owned.go 只在
// 带 motion / area 的衍生物上用模板时长），以前却连负数都照样编译通过。现在 minion 上写
// duration_ticks 直接编译期拒绝；area 成员字段同理（minion 不做成员检测，以前同样被
// 提前返回跳过检查，写了 area 运行期反而拿 0 时长报 ErrProgramInvariant）。
//
// 承诺：minion 上写 duration_ticks（正、零以外的任何值）或 area / interval_ticks /
// emit_leave_on_stop 是 MOTION_INVALID；不写时照常编译，寿命取召唤效果的时长。

import (
	"testing"
)

func TestMinionSpawnRejectsFieldsItNeverReads(t *testing.T) {
	area := `"area":{"from":"$caster","kind":"entity","shape":{"type":"circle","radius":4},"filters":[{"type":"targetable"}],"order":{"by":"stable_id","direction":"asc"},"limit":2}`
	for name, spawn := range map[string]string{
		"positive duration":  `"kind":"minion","duration_ticks":5`,
		"negative duration":  `"kind":"minion","duration_ticks":-3`,
		"area":               `"kind":"minion",` + area,
		"interval":           `"kind":"minion","interval_ticks":1`,
		"emit leave on stop": `"kind":"minion","emit_leave_on_stop":true`,
	} {
		t.Run(name, func(t *testing.T) {
			_, diagnostics := compileToArtifacts(mustParseJSON(t, motionSkillJSON(spawn)), DefaultCompileEnvironment())
			requireDiagnostic(t, diagnostics, DiagnosticMotionInvalid)
		})
	}
}

func TestMinionSpawnWithoutDurationCompilesAndLivesForTheSummonDuration(t *testing.T) {
	flow := `{"flow":"sequence","steps":[{"flow":"effect","effect":{"type":"summon","template":"deployable.trap","position":"$caster.position","count":1,"duration_ticks":10},"spawn":{"kind":"minion"}},{"flow":"finish"}]}`
	program, environment := compileOwnedSkill(t, "minion-spawn-duration", flow)
	runtime := NewRuntime(runtimeTestHost(environment), RuntimeOptions{})
	if _, err := runtime.Activate(program, CastInput{Caster: 1}); err != nil {
		t.Fatal(err)
	}
	if len(runtime.ownedSpawns) != 1 {
		t.Fatalf("owned spawns = %d, want the one minion spawn", len(runtime.ownedSpawns))
	}
	for _, spawn := range runtime.ownedSpawns {
		if spawn.EndTick-spawn.StartTick != 10 {
			t.Fatalf("minion spawn lives %d ticks, want the summon duration 10", spawn.EndTick-spawn.StartTick)
		}
	}
}
