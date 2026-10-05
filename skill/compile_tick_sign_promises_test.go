package skill

import (
	"strings"
	"testing"
)

// RR-20261005-NC-152：作者写的 tick 一律非负，且负值在编译期按字段定位拒绝。
// 旧实现只检查 global_cooldown / cast window / policy 等少数字段：负 cooldown
// 让冷却立即结束，负 repeat 间隔在第一次迭代提交后 ErrProgramInvariant，负
// add_status 时长每次被 Host 拒绝，负 chain 间隔被当作 0；负 wait 虽被拒，
// 诊断却是 `$` 上的“lifetime_ticks budget 9223372036854775807”。
func TestCompileRejectsNegativeTicksAtTheirField(t *testing.T) {
	finish := `{"flow":"finish","reason":"done"}`
	damage := `{"flow":"effect","effect":{"type":"damage","target":"$input.target","amount":1,"damage_type":"physical"}}`
	withEnter := func(enter string) string {
		return strings.Replace(strings.Replace(minimalSkillJSON, finish, enter, 1), `"input_schema":{"type":"none"}`, `"input_schema":{"type":"entity"}`, 1)
	}
	cases := []struct {
		name, input, path string
	}{
		{"cooldown_ticks", strings.Replace(minimalSkillJSON, `"cooldown_ticks":0`, `"cooldown_ticks":-5`, 1), "$.cooldown_ticks"},
		{"phase timeout_ticks", strings.Replace(minimalSkillJSON, `"timeout_ticks":0`, `"timeout_ticks":-3`, 1), "$.phases[0].timeout_ticks"},
		{"wait ticks", withEnter(`{"flow":"wait","ticks":-1,"then":` + finish + `}`), "$.phases[0].on.enter.ticks"},
		{"repeat interval_ticks", withEnter(`{"flow":"sequence","steps":[{"flow":"repeat","times":3,"interval_ticks":-2,"index_as":"i","do":` + damage + `},` + finish + `]}`), "$.phases[0].on.enter.steps[0].interval_ticks"},
		{"add_status duration_ticks", withEnter(`{"flow":"sequence","steps":[{"flow":"effect","effect":{"type":"add_status","target":"$input.target","status":"slow","duration_ticks":-1,"stacks":1,"max_stacks":1}},` + finish + `]}`), "$.phases[0].on.enter.steps[0].effect.duration_ticks"},
		{"chain hop_interval_ticks", withEnter(`{"flow":"sequence","steps":[{"flow":"select","select":{"from":"$input.target","kind":"entity","shape":{"type":"chain","hop_range":1,"max_targets":1,"allow_repeat":false,"hop_interval_ticks":-1},"filters":[],"order":{"by":"stable_id","direction":"asc"},"limit":1},"consume":{"mode":"one","as":"target","then":` + damage + `},"on_empty":` + finish + `},` + finish + `]}`), "$.phases[0].on.enter.steps[0].select.shape.hop_interval_ticks"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			program, diagnostics := Compile(mustParseJSON(t, test.input), DefaultCompileEnvironment())
			if program != nil {
				t.Fatalf("negative %s compiled; diagnostics=%#v", test.name, diagnostics)
			}
			requireDiagnosticAt(t, diagnostics, DiagnosticShapeInvalid, test.path)
		})
	}
}

// 控制：同样形状、tick 为 0 时照常编译。add_status 的时长要求为正
// （RR-20261005-NC-215：两个参考 Host 都拒绝 0），这里用 1，0 的拒绝由
// TestCompileRejectsValuesEveryHostRejects 钉住。
func TestCompileAcceptsZeroTicks(t *testing.T) {
	finish := `{"flow":"finish","reason":"done"}`
	input := strings.Replace(strings.Replace(minimalSkillJSON, finish, `{"flow":"sequence","steps":[{"flow":"wait","ticks":0,"then":{"flow":"effect","effect":{"type":"add_status","target":"$input.target","status":"slow","duration_ticks":1,"stacks":1,"max_stacks":1}}},{"flow":"repeat","times":2,"interval_ticks":0,"index_as":"i","do":{"flow":"effect","effect":{"type":"damage","target":"$input.target","amount":1,"damage_type":"physical"}}},`+finish+`]}`, 1), `"input_schema":{"type":"none"}`, `"input_schema":{"type":"entity"}`, 1)
	if _, diagnostics := Compile(mustParseJSON(t, input), DefaultCompileEnvironment()); diagnosticsHaveErrors(diagnostics) {
		t.Fatalf("zero ticks rejected: %#v", diagnostics)
	}
}
