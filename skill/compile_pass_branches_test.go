package skill

import (
	"reflect"
	"testing"
)

// N09 第五批：random / temporal / graph / effect_result / proc / quantity 几个 pass 的直接
// 分支用例（snapshot 见 compile_capture_context_promises_test.go）。每个接受分支在
// MemoryHost 上施法，核对 Runtime 的执行点与编译期的产物一致；拒绝分支钉住诊断码与位置。
// 第四批的变异性质测试只间接覆盖这些分支。

func branchHost(t *testing.T, environment CompileEnvironment, entities ...MemoryEntity) *MemoryHost {
	t.Helper()
	host := runtimeTestHost(environment)
	for _, entity := range entities {
		host.UpsertEntity(entity)
	}
	return host
}

func branchRun(t *testing.T, program *Program, host *MemoryHost, input CastInput, ticks int) *Runtime {
	t.Helper()
	runtime := NewRuntime(host, RuntimeOptions{MatchSeed: fixedTestSeed(7)})
	if _, err := runtime.Activate(program, input); err != nil {
		t.Fatalf("activate: %v", err)
	}
	for tick := 0; tick < ticks; tick++ {
		if err := runtime.Advance(1); err != nil {
			t.Fatalf("advance %d: %v", tick, err)
		}
	}
	return runtime
}

func branchCompile(t *testing.T, input string) *Program {
	t.Helper()
	program, diagnostics := Compile(mustParseJSON(t, input), DefaultCompileEnvironment())
	requireNoErrors(t, diagnostics)
	return program
}

const branchRandomSelect = `{"flow":"select","select":{"from":"$caster","kind":"entity","shape":{"type":"circle","radius":10},"filters":[{"type":"targetable"}],"order":{"by":"random","direction":"asc"},"limit":LIMIT},"consume":{"mode":"each","as":"t","do":{"flow":"effect","effect":{"type":"damage","target":"$local.t","amount":AMOUNT,"damage_type":"physical"}}}}`

func branchRandom(limit, amount string) string {
	return stringsReplaceAllPairs(branchRandomSelect, "LIMIT", limit, "AMOUNT", amount)
}

func stringsReplaceAllPairs(text string, pairs ...string) string {
	for index := 0; index+1 < len(pairs); index += 2 {
		for {
			position := -1
			for offset := 0; offset+len(pairs[index]) <= len(text); offset++ {
				if text[offset:offset+len(pairs[index])] == pairs[index] {
					position = offset
					break
				}
			}
			if position < 0 {
				break
			}
			text = text[:position] + pairs[index+1] + text[position+len(pairs[index]):]
		}
	}
	return text
}

// random：每个 order.by=random 的 select 是一个随机位点，InvocationBound 按外层 repeat 次数 ×
// select-each 的 limit 放大；area 的成员顺序不能随机。运行期同一种子得到同一顺序，limit 截断
// 发生在随机排序之后。
func TestRandomPassBranches(t *testing.T) {
	t.Run("site bounds follow repeat and each", func(t *testing.T) {
		inner := branchRandom("2", "1")
		repeat := `{"flow":"repeat","times":3,"interval_ticks":0,"index_as":"i","do":` + inner + `}`
		outer := `{"flow":"select","select":{"from":"$caster","kind":"entity","shape":{"type":"circle","radius":10},"filters":[],"order":{"by":"stable_id","direction":"asc"},"limit":2},"consume":{"mode":"each","as":"o","do":` + inner + `}}`
		program := branchCompile(t, agreementSkillJSON("none", "{}", "[]", agreementSteps(inner, repeat, outer)))
		sites := InspectRandomSites(program)
		bounds := make([]int, len(sites))
		for index, site := range sites {
			if site.Kind != "selection_order" || int(site.Index) != index {
				t.Fatalf("site %d = %#v", index, site)
			}
			bounds[index] = site.InvocationBound
		}
		if !reflect.DeepEqual(bounds, []int{1, 3, 2}) {
			t.Fatalf("invocation bounds = %v, want [1 3 2]", bounds)
		}
	})
	t.Run("area order cannot be random", func(t *testing.T) {
		area := `{"flow":"effect","effect":{"type":"spawn","template":"deployable.trap","position":"$caster.position","count":1,"duration_ticks":2},"spawn":{"kind":"area","duration_ticks":2,"interval_ticks":1,"area":{"from":"$caster","kind":"entity","shape":{"type":"circle","radius":4},"filters":[],"order":{"by":"random","direction":"asc"},"limit":2}}}`
		requireAgreementRejected(t, []agreementCase{{"random area", agreementSkillJSON("none", "{}", "[]", agreementSteps(area)), string(DiagnosticShapeInvalid), agreementEnterPath + ".spawn.area.order"}})
	})
	t.Run("runtime order is seeded and limit applies after shuffling", func(t *testing.T) {
		program := branchCompile(t, agreementSkillJSON("none", "{}", "[]", agreementSteps(branchRandom("1", "10"))))
		victims := func() EntityID {
			host := branchHost(t, DefaultCompileEnvironment(),
				MemoryEntity{ID: 1, Alive: true, Health: 100, MaxHealth: 100, Untargetable: true},
				MemoryEntity{ID: 3, Alive: true, Health: 100, MaxHealth: 100},
				MemoryEntity{ID: 4, Alive: true, Health: 100, MaxHealth: 100},
			)
			branchRun(t, program, host, CastInput{Caster: 1}, 0)
			hit := EntityID(0)
			for _, id := range []EntityID{2, 3, 4} {
				if host.HealthForTest(id) == 90 {
					if hit != 0 {
						t.Fatalf("limit 1 damaged both %d and %d", hit, id)
					}
					hit = id
				}
			}
			if hit == 0 {
				t.Fatal("random select consumed nothing")
			}
			return hit
		}
		if first, second := victims(), victims(); first != second {
			t.Fatalf("same seed chose %d then %d", first, second)
		}
	})
}

// temporal：profile 必须在 catalog 里、on_blocked 必须是合法策略；合法但与 profile 不同的
// on_blocked 能编译，MemoryHost 以 policy_rejected 预期失败返回，走 failure 分支（O27）。
func TestTemporalPassBranches(t *testing.T) {
	capture := func(profile, onBlocked string) string {
		return `{"flow":"effect","effect":{"type":"capture_snapshot","target":"$caster","profile":"` + profile + `"},"result":{"as":"snap","success":{"flow":"effect","effect":{"type":"restore_snapshot","target":"$caster","snapshot":"$local.snap.token","on_blocked":"` + onBlocked + `"},"result":{"as":"r","success":{"flow":"effect","effect":{"type":"damage","target":"$caster","amount":5,"damage_type":"physical"}},"failure":{"flow":"effect","effect":{"type":"heal","target":"$caster","amount":5}}}},"failure":{"flow":"finish"}}}`
	}
	requireAgreementRejected(t, []agreementCase{
		{"unknown profile", agreementSkillJSON("none", "{}", "[]", agreementSteps(capture("temporal.unknown", "fail"))), string(DiagnosticCapabilityUnknown), agreementEnterPath + ".effect.profile"},
		{"invalid blocked policy", agreementSkillJSON("none", "{}", "[]", agreementSteps(capture("temporal.position_health", "warp"))), string(DiagnosticShapeInvalid), agreementEnterPath + ".result.success.effect.on_blocked"},
	})
	for _, test := range []struct {
		onBlocked string
		want      int64
	}{{"fail", 45}, {"", 45}, {"stay", 55}} {
		t.Run("restore on_blocked="+test.onBlocked, func(t *testing.T) {
			program := branchCompile(t, agreementSkillJSON("none", "{}", "[]", agreementSteps(capture("temporal.position_health", test.onBlocked))))
			host := branchHost(t, DefaultCompileEnvironment(), MemoryEntity{ID: 1, Alive: true, Health: 50, MaxHealth: 100})
			branchRun(t, program, host, CastInput{Caster: 1}, 0)
			if health := host.HealthForTest(1); health != test.want {
				t.Fatalf("caster health = %d, want %d", health, test.want)
			}
		})
	}
}

// graph：goto 跨多个 phase（包括从 effect result 分支里 goto）按图执行每个 phase 的 enter。
func TestGraphPassBranchesRunEveryReachablePhase(t *testing.T) {
	damage := `{"flow":"effect","effect":{"type":"damage","target":"$input.target","amount":1,"damage_type":"physical"}`
	phases := `[{"id":"a","timeout_ticks":0,"on":{"enter":{"flow":"sequence","steps":[` + damage + `},{"flow":"goto","phase":"b"}]}}},` +
		`{"id":"b","timeout_ticks":0,"on":{"enter":` + damage + `,"result":{"as":"r","success":{"flow":"goto","phase":"c"},"failure":{"flow":"finish"}}}}},` +
		`{"id":"c","timeout_ticks":0,"on":{"enter":{"flow":"sequence","steps":[` + damage + `},{"flow":"finish"}]}}}]`
	input := stringsReplaceAllPairs(phaseSkillJSON("a", phases), `"input_schema":{"type":"none"}`, `"input_schema":{"type":"entity"}`)
	program := branchCompile(t, input)
	host := branchHost(t, DefaultCompileEnvironment())
	runtime := branchRun(t, program, host, CastInput{Caster: 1, Target: 2}, 0)
	if health := host.HealthForTest(2); health != 97 {
		t.Fatalf("target health = %d, want 97 (one damage per phase)", health)
	}
	if snapshot, ok := runtime.InspectCast(1); !ok || snapshot.Status != CastFinished {
		t.Fatalf("cast = %#v", snapshot)
	}
}

// effect_result：没有即时结果的效果、带回调的衍生物效果、会挂起的分支都不能声明 result；
// 结果槽位受预算限制。spawn + 无回调衍生物写在 result 分支里能编译并执行（O28）。
func TestEffectResultPassBranches(t *testing.T) {
	wait := `{"flow":"wait","ticks":1,"then":{"flow":"finish"}}`
	repeat := `{"flow":"repeat","times":2,"interval_ticks":1,"index_as":"i","do":{"flow":"finish"}}`
	resource := `{"flow":"effect","effect":{"type":"resource","target":"$caster","resource":"mana","operation":"add","amount":1},"result":{"as":"r","success":{"flow":"finish"}}}`
	damageThen := func(success string) string {
		return `{"flow":"effect","effect":{"type":"damage","target":"$input.target","amount":1,"damage_type":"physical"},"result":{"as":"r","success":` + success + `,"failure":{"flow":"finish"}}}`
	}
	spawnWithCallbacks := `{"flow":"effect","effect":{"type":"spawn","template":"deployable.trap","position":"$caster.position","count":1,"duration_ticks":2},"on":{"enter":{"flow":"effect","effect":{"type":"heal","target":"$event.target","amount":1}}},"result":{"as":"r","success":{"flow":"finish"}}}`
	requireAgreementRejected(t, []agreementCase{
		{"resource has no result", agreementSkillJSON("none", "{}", "[]", agreementSteps(resource)), string(DiagnosticShapeInvalid), agreementEnterPath + ".result"},
		{"wait in a result branch", agreementSkillJSON("entity", "{}", "[]", agreementSteps(damageThen(wait))), string(DiagnosticShapeInvalid), agreementEnterPath + ".result.success"},
		{"timed repeat in a result branch", agreementSkillJSON("entity", "{}", "[]", agreementSteps(damageThen(repeat))), string(DiagnosticShapeInvalid), agreementEnterPath + ".result.success"},
		{"spawn effect with a result", agreementSkillJSON("none", "{}", "[]", agreementSteps(spawnWithCallbacks)), string(DiagnosticShapeInvalid), agreementEnterPath + ".result"},
	})
	t.Run("result slot budget", func(t *testing.T) {
		environment := DefaultCompileEnvironment()
		environment.Limits.MaxEffectResultSlots = 1
		environment.Digest = authorityDigest(environment)
		input := agreementSkillJSON("entity", "{}", "[]", agreementSteps(damageThen(`{"flow":"finish"}`), damageThen(`{"flow":"finish"}`)))
		_, diagnostics := Compile(mustParseJSON(t, input), environment)
		requireDiagnosticAt(t, diagnostics, DiagnosticBudgetExceeded, "$")
	})
	t.Run("spawn with a spawn inside a result branch runs", func(t *testing.T) {
		spawn := `{"flow":"effect","effect":{"type":"spawn","template":"deployable.trap","position":"$caster.position","count":1,"duration_ticks":2},"spawn":{"kind":"area","duration_ticks":2,"interval_ticks":1,"area":{"from":"$caster","kind":"entity","shape":{"type":"circle","radius":4},"filters":[{"type":"targetable"}],"order":{"by":"stable_id","direction":"asc"},"limit":2}}}`
		program := branchCompile(t, agreementSkillJSON("entity", "{}", "[]", agreementSteps(damageThen(spawn))))
		runtime := branchRun(t, program, branchHost(t, DefaultCompileEnvironment()), CastInput{Caster: 1, Target: 2}, 0)
		if started := runtime.OwnedSpawns(1); len(started) != 1 || started[0].Status != SpawnRunning {
			t.Fatalf("the result branch did not start its spawn: %#v", started)
		}
		for tick := 0; tick < 4; tick++ {
			if err := runtime.Advance(1); err != nil {
				t.Fatalf("advance %d: %v", tick, err)
			}
		}
	})
}

// proc：results 过滤按事件的 Result 字符串匹配；不匹配时压制为 filter，匹配时施法。
func TestProcPassResultFilterBranches(t *testing.T) {
	input := stringsReplaceAllPairs(passiveSkillJSON(1, `[]`, `[]`), `"results":[]`, `"results":["kill"]`, `{"flow":"finish","reason":"done"}`, `{"flow":"sequence","steps":[{"flow":"effect","effect":{"type":"heal","target":"$caster","amount":1}},{"flow":"finish"}]}`)
	program, environment := compileRuntimeJSON(t, input)
	for _, test := range []struct {
		result string
		want   int64
	}{{"kill", 51}, {"hit", 50}} {
		t.Run(test.result, func(t *testing.T) {
			host := branchHost(t, environment, MemoryEntity{ID: 1, Alive: true, Health: 50, MaxHealth: 100})
			runtime := NewRuntime(host, RuntimeOptions{})
			if _, err := runtime.ActivatePassive(program, EventContext{EventID: 5, RootEventID: 5, Owner: 1, Source: 2, Result: test.result}); err != nil {
				t.Fatal(err)
			}
			if err := runtime.Advance(1); err != nil {
				t.Fatal(err)
			}
			if health := host.HealthForTest(1); health != test.want {
				t.Fatalf("health = %d, want %d", health, test.want)
			}
			if test.want == 50 {
				assertSuppressionReason(t, runtime.RuntimeEvents(), "filter")
			}
		})
	}
}

// quantity：add 的两边量纲必须一致（Runtime 的 CheckedAddRuntimeValues 也逐值比较）；字面量
// 取期望量纲。Inspect 的量纲“证明”是全 int64 区间、proved=true（O29）。
func TestQuantityPassBranches(t *testing.T) {
	ticksPlusCount := `{"flow":"effect","effect":{"type":"damage","target":"$input.target","amount":{"op":"add","args":["$cast.elapsed_ticks","$cast.pulse_index"]},"damage_type":"physical"}}`
	requireAgreementRejected(t, []agreementCase{
		{"ticks plus count", agreementSkillJSON("entity", "{}", "[]", agreementSteps(ticksPlusCount)), string(DiagnosticQuantityMismatch), agreementEnterPath + ".effect.amount"},
	})
	literal := `{"flow":"effect","effect":{"type":"damage","target":"$input.target","amount":{"op":"add","args":[{"read_attribute":{"entity":"$caster","attribute":"ability_power","snapshot":"current"}},3]},"damage_type":"physical"}}`
	program := branchCompile(t, agreementSkillJSON("entity", "{}", "[]", agreementSteps(literal)))
	host := branchHost(t, DefaultCompileEnvironment(), MemoryEntity{ID: 1, Alive: true, Health: 100, MaxHealth: 100, Resources: map[string]int64{"mana": 100}, Attributes: map[AttributeHandle]int64{2: 4}})
	branchRun(t, program, host, CastInput{Caster: 1, Target: 2}, 0)
	if health := host.HealthForTest(2); health != 93 {
		t.Fatalf("target health = %d, want 93", health)
	}
	for _, quantity := range InspectQuantities(program) {
		if !quantity.Proved || quantity.Minimum != -int64(^uint64(0)>>1)-1 || quantity.Maximum != int64(^uint64(0)>>1) {
			t.Fatalf("quantity %#v: the proof is the full int64 range", quantity)
		}
	}
}
