package skill

import (
	"strings"
	"testing"
)

// RR-20261005-NC-211：编译器允许 spawn 出的 area 衍生物在 enter / tick / leave 等回调里
// `finish`（compile_owned_entity.go 的 allowAreaFinish），含义是“结束拥有它的施法并
// 停止本 area 余下的信号”。但施法先结束时，entity 作用域衍生物被移交（handedOff），
// 之后回调里的 finish 落到 runOwnedSpawnCallback 的
// `template.area == nil || spawn.handedOff → ErrProgramInvariant`：Advance 在这一
// tick 中途返回错误，同一 tick 余下的 owned 衍生物和排程任务都没跑。编译通过的定义
// 不能在运行期触发 ErrProgramInvariant；移交后的 finish 没有施法可结束，只结束本
// area 衍生物（与施法存活时“finish 停止本 area 余下信号”的规则一致）。
func areaHandoffFinishSkill(callbacks string) string {
	area := `{"flow":"effect","effect":{"type":"spawn","template":"deployable.trap","position":"$caster.position","count":1,"duration_ticks":6},"spawn":{"kind":"area","duration_ticks":6,"interval_ticks":1,"emit_leave_on_stop":true,"area":{"from":"$caster","kind":"entity","shape":{"type":"circle","radius":10},"filters":[{"type":"targetable"}],"order":{"by":"stable_id","direction":"asc"},"limit":2}},"on":` + callbacks + `}`
	return strings.Replace(minimalSkillJSON, `{"flow":"finish","reason":"done"}`, `{"flow":"sequence","steps":[`+area+`,{"flow":"finish"}]}`, 1)
}

func TestHandedOffAreaFinishEndsOnlyTheAreaSpawn(t *testing.T) {
	heal := `{"flow":"effect","effect":{"type":"heal","target":"$event.target","amount":1}}`
	cases := map[string]struct {
		callbacks, finishing string
		members              [][]EntityID
	}{
		// 成员 2 在第 2 次查询时离开：leave 回调在移交之后执行 finish。
		"leave after handoff": {`{"enter":` + heal + `,"leave":{"flow":"finish"}}`, "owned_spawn_callback_leave", [][]EntityID{{2}, {2}, {}}},
		// 成员 2 在移交之后才进入：enter 回调执行 finish。
		"enter after handoff": {`{"enter":{"flow":"finish"}}`, "owned_spawn_callback_enter", [][]EntityID{{}, {2}, {2}}},
	}
	for name, test := range cases {
		t.Run(name, func(t *testing.T) {
			callbacks := test.callbacks
			environment := DefaultCompileEnvironment()
			program, diagnostics := Compile(mustParseJSON(t, areaHandoffFinishSkill(callbacks)), environment)
			requireNoErrors(t, diagnostics)
			host := &areaSnapshotHost{MemoryHost: runtimeTestHost(environment), snapshots: test.members}
			runtime := NewRuntime(host, RuntimeOptions{})
			castID, err := runtime.Activate(program, CastInput{Caster: 1})
			if err != nil {
				t.Fatal(err)
			}
			if cast, _ := runtime.InspectCast(castID); cast.Status != CastFinished {
				t.Fatalf("cast = %#v, want finished before the area callback runs", cast)
			}
			if owned := runtime.OwnedSpawns(1); len(owned) != 1 {
				t.Fatalf("owned spawns = %#v, want the handed-off area", owned)
			}
			for tick := Tick(1); tick <= 8; tick++ {
				if err := runtime.Advance(tick); err != nil {
					t.Fatalf("Advance(%d) = %v; a compiled area finish must not fail the runtime", tick, err)
				}
			}
			if owned := runtime.OwnedSpawns(1); len(owned) != 0 {
				t.Fatalf("owned spawns after finish = %#v, want none", owned)
			}
			finishes := 0
			for _, event := range runtime.RuntimeEvents() {
				if event.Kind == test.finishing {
					finishes++
				}
			}
			if finishes != 1 {
				t.Fatalf("finishing callbacks = %d, want exactly one: finish stops the remaining signals", finishes)
			}
		})
	}
}

// 控制：施法存活时 area 回调的 finish 仍然结束施法（既有语义不变）。
func TestLiveAreaFinishStillFinishesTheCast(t *testing.T) {
	finish := `{"flow":"finish","reason":"area_complete"}`
	flow := `{"flow":"sequence","steps":[{"flow":"effect","effect":{"type":"spawn","template":"deployable.trap","position":"$caster.position","count":1,"duration_ticks":4},"spawn":{"kind":"area","duration_ticks":4,"interval_ticks":1,"area":{"from":"$caster","kind":"entity","shape":{"type":"circle","radius":10},"filters":[{"type":"targetable"}],"order":{"by":"stable_id","direction":"asc"},"limit":2}},"on":{"enter":` + finish + `}},{"flow":"wait","ticks":5,"then":{"flow":"finish"}}]}`
	environment := DefaultCompileEnvironment()
	program, diagnostics := Compile(mustParseJSON(t, strings.Replace(minimalSkillJSON, `{"flow":"finish","reason":"done"}`, flow, 1)), environment)
	requireNoErrors(t, diagnostics)
	host := &areaSnapshotHost{MemoryHost: runtimeTestHost(environment), snapshots: [][]EntityID{{2}}}
	runtime := NewRuntime(host, RuntimeOptions{})
	castID, err := runtime.Activate(program, CastInput{Caster: 1})
	if err != nil {
		t.Fatal(err)
	}
	if cast := runtime.casts[castID]; cast == nil || !cast.areaCallbackFinish || cast.status != CastFinished {
		t.Fatalf("cast = %#v, want the live area finish to finish it", cast)
	}
}
