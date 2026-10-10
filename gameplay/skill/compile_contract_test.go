package skill

import (
	"fmt"
	"strings"
	"testing"
)

func captureSpawn(tick string) string {
	return `{"flow":"effect","effect":{"type":"summon","template":"deployable.trap","position":"$caster.position","count":1,"duration_ticks":3},"spawn":{"kind":"area","duration_ticks":3,"interval_ticks":1,"area":{"from":"$caster","kind":"entity","shape":{"type":"circle","radius":4},"filters":[{"type":"targetable"}],"order":{"by":"stable_id","direction":"asc"},"limit":2}},"on":{"tick":` + tick + `}}`
}

func captureRead(entity, point string) string {
	return fmt.Sprintf(`{"read_attribute":{"entity":"%s","attribute":"ability_power","snapshot":"%s"}}`, entity, point)
}

func captureDamage(target, amount string) string {
	return `{"flow":"effect","effect":{"type":"damage","target":"` + target + `","amount":` + amount + `,"damage_type":"physical"}}`
}

const captureHoldPosition = `{"flow":"effect","effect":{"type":"issue_entity_command","target":"$event.target","command":"hold_position"}}`

// RR-20261005-NC-220：缓存型快照点只能读采样点上可求值的实体。旧实现：
//   - spawn_start 写在 phase 流程里也能编译。施法里有召唤效果时，衍生物启动按
//     spawn_start 采样全部计划，在脱离施法的衍生物上下文里求 `$input.target` /
//     `$memory.x`，每次施法 ErrProgramInvariant；没有召唤效果时退化成“第一次读到的值”。
//   - 实体引用 `$local.*` 的缓存型读取在采样点没有局部变量，旧实现前面的 pass 不查，
//     落到 lower 报 LOWER_UNRESOLVED（定位在 `$`，看不出是哪一处读取）。
func TestCompileRejectsSnapshotsThatCannotBeCapturedWhereTheyAreRead(t *testing.T) {
	selectEach := func(from, do string) string {
		return `{"flow":"select","select":{"from":"` + from + `","kind":"entity","shape":{"type":"circle","radius":10},"filters":[],"order":{"by":"stable_id","direction":"asc"},"limit":2},"consume":{"mode":"each","as":"t","do":` + do + `}}`
	}
	enter := agreementEnterPath
	requireAgreementRejected(t, []agreementCase{
		{"spawn_start of an input in the phase flow with a summon",
			agreementSkillJSON("entity", "{}", "[]", agreementSteps(captureDamage("$input.target", captureRead("$input.target", "spawn_start")), captureSpawn(captureHoldPosition))),
			string(DiagnosticAttributeSnapshotInvalid), enter + ".effect.amount.read_attribute.snapshot"},
		{"spawn_start of a memory entity in the phase flow with a summon",
			agreementSkillJSON("none", `{"who":{"type":"entity","default":"$caster"}}`, "[]", agreementSteps(captureDamage("$caster", captureRead("$memory.who", "spawn_start")), captureSpawn(captureHoldPosition))),
			string(DiagnosticAttributeSnapshotInvalid), enter + ".effect.amount.read_attribute.snapshot"},
		{"spawn_start in the phase flow without any summon",
			agreementSkillJSON("entity", "{}", "[]", agreementSteps(captureDamage("$input.target", captureRead("$caster", "spawn_start")))),
			string(DiagnosticAttributeSnapshotInvalid), enter + ".effect.amount.read_attribute.snapshot"},
		{"cast_start of a select local",
			agreementSkillJSON("none", "{}", "[]", agreementSteps(selectEach("$caster", captureDamage("$local.t", captureRead("$local.t", "cast_start"))))),
			string(DiagnosticAttributeSnapshotInvalid), enter + ".consume.do.effect.amount.read_attribute.entity"},
		{"phase_start of a select local",
			agreementSkillJSON("none", "{}", "[]", agreementSteps(selectEach("$caster", captureDamage("$local.t", captureRead("$local.t", "phase_start"))))),
			string(DiagnosticAttributeSnapshotInvalid), enter + ".consume.do.effect.amount.read_attribute.entity"},
		{"spawn_start of a callback local",
			agreementSkillJSON("none", "{}", "[]", agreementSteps(captureSpawn(selectEach("$owner", captureDamage("$local.t", captureRead("$local.t", "spawn_start")))))),
			string(DiagnosticAttributeSnapshotInvalid), enter + ".on.tick.consume.do.effect.amount.read_attribute.entity"},
	})
}

// RR-20261005-NC-220 的控制：采样点上可求值的读取照常编译，且值确实取自采样点。
// spawn_start 在 owned 衍生物回调里读 `$owner`：area 衍生物启动时（Activate 内）先跑一次
// tick 回调，之后改属性（5 → 40）再推进一次 tick 回调。spawn_start 两次都是 5，
// current 第二次读到 40。
func TestSnapshotsCapturedAtTheirPointStillRun(t *testing.T) {
	t.Run("cast_start in the phase flow", func(t *testing.T) {
		input := agreementSkillJSON("entity", "{}", "[]", agreementSteps(captureDamage("$input.target", captureRead("$caster", "cast_start"))))
		program, diagnostics := Compile(mustParseJSON(t, input), DefaultCompileEnvironment())
		requireNoErrors(t, diagnostics)
		host := runtimeTestHost(DefaultCompileEnvironment())
		host.UpsertEntity(MemoryEntity{ID: 1, Alive: true, Health: 100, MaxHealth: 100, Resources: map[string]int64{"mana": 100}, Attributes: map[AttributeHandle]int64{2: 7}})
		runtime := NewRuntime(host, RuntimeOptions{})
		if _, err := runtime.Activate(program, CastInput{Caster: 1, Target: 2}); err != nil {
			t.Fatal(err)
		}
		if health := host.HealthForTest(2); health != 93 {
			t.Fatalf("target health = %d, want 93", health)
		}
	})
	for _, point := range []struct {
		name string
		want int64
	}{{"spawn_start", 100 - 5 - 5}, {"current", 100 - 5 - 40}} {
		t.Run(point.name+" in an owned spawn callback", func(t *testing.T) {
			input := agreementSkillJSON("none", "{}", "[]", agreementSteps(captureSpawn(captureDamage("$event.target", captureRead("$owner", point.name)))))
			program, diagnostics := Compile(mustParseJSON(t, input), DefaultCompileEnvironment())
			requireNoErrors(t, diagnostics)
			host := runtimeTestHost(DefaultCompileEnvironment())
			host.UpsertEntity(MemoryEntity{ID: 1, Alive: true, Health: 100, MaxHealth: 100, Resources: map[string]int64{"mana": 100}, Attributes: map[AttributeHandle]int64{2: 5}, Untargetable: true})
			host.UpsertEntity(MemoryEntity{ID: 2, Alive: true, Health: 100, MaxHealth: 100})
			runtime := NewRuntime(host, RuntimeOptions{})
			if _, err := runtime.Activate(program, CastInput{Caster: 1}); err != nil {
				t.Fatal(err)
			}
			host.UpsertEntity(MemoryEntity{ID: 1, Alive: true, Health: 100, MaxHealth: 100, Resources: map[string]int64{"mana": 100}, Attributes: map[AttributeHandle]int64{2: 40}, Untargetable: true})
			for tick := 0; tick < 6; tick++ {
				if err := runtime.Advance(1); err != nil {
					t.Fatalf("advance %d: %v", tick, err)
				}
			}
			if health := host.HealthForTest(2); health != point.want {
				t.Fatalf("target health = %d, want %d", health, point.want)
			}
		})
	}
}

func capturePassiveJSON(input, maxDepth string) string {
	return `{"schema":"roost.skill/v2","id":"skill.test.passive_input","name":"Passive","description":"Runs on damage.","gameplay_tags":["spell"],"activation":{"type":"passive_on_damaged","cooldown_scope":"caster","event_filter":{"required_tags":[],"excluded_tags":[],"elements":[],"damage_types":[],"results":[]},"proc_policy":{"max_depth":` + maxDepth + `,"allow_self_trigger":false,"once_per_root_event":true}},"input_schema":` + input + `,"cooldown_ticks":0,"costs":[],"memory":{},"initial_phase":"cast","phases":[{"id":"cast","timeout_ticks":0,"on":{"enter":{"flow":"sequence","steps":[{"flow":"effect","effect":{"type":"heal","target":"$caster","amount":1}},{"flow":"finish"}]}}}]}`
}

// RR-20261005-NC-221：被动技能由 ActivatePassive 按触发事件启动，施法输入只有事件的
// target（`passiveCastInput`），深度按 `ProcDepth >= max_depth` 压制。旧实现编译期不查：
// 输入是 position / direction 等的被动每次触发都 `passive_suppressed: unavailable`，
// max_depth 0 的被动每次触发都 `passive_suppressed: max_depth`——编译通过、永远不生效。
func TestCompileRejectsPassivesThatCanNeverActivate(t *testing.T) {
	cases := []agreementCase{
		{"max_depth zero", capturePassiveJSON(`{"type":"none"}`, "0"), string(DiagnosticShapeInvalid), "$.activation.proc_policy.max_depth"},
	}
	for _, schema := range []string{
		`{"type":"position","maximum_range":10}`,
		`{"type":"direction"}`,
		`{"type":"direction_position","maximum_range":10}`,
		`{"type":"entity_position","maximum_range":10}`,
	} {
		cases = append(cases, agreementCase{"input " + schema, capturePassiveJSON(schema, "1"), string(DiagnosticInputUnavailable), "$.input_schema"})
	}
	requireAgreementRejected(t, cases)
}

// RR-20261005-NC-221 的控制：none / entity 输入、max_depth 1 的被动照常触发，entity 输入
// 拿到事件的 target。
func TestPassivesWithEventInputsStillActivate(t *testing.T) {
	for _, schema := range []string{`{"type":"none"}`, `{"type":"entity"}`} {
		t.Run(schema, func(t *testing.T) {
			program, environment := compileRuntimeJSON(t, capturePassiveJSON(schema, "1"))
			host := runtimeTestHost(environment)
			host.UpsertEntity(MemoryEntity{ID: 1, Alive: true, Health: 50, MaxHealth: 100})
			runtime := NewRuntime(host, RuntimeOptions{})
			if _, err := runtime.ActivatePassive(program, EventContext{EventID: 9, RootEventID: 9, Owner: 1, Source: 2, Target: 2}); err != nil {
				t.Fatal(err)
			}
			if err := runtime.Advance(1); err != nil {
				t.Fatal(err)
			}
			if health := host.HealthForTest(1); health != 51 {
				t.Fatalf("passive did not run: health = %d, events = %#v", health, runtime.RuntimeEvents())
			}
		})
	}
}

// RR-20261005-NC-222：只有召唤效果会启动实体衍生物（`executeOwnedSummon` →
// `startEntitySpawn`）。旧实现里其他效果上的 `spawn` / `on` 照样编译、lower 成衍生物
// 模板（presentation plan 里还会出现衍生物挂载点），但 Runtime 从不启动，回调从不执行。
func TestCompileRejectsSpawnsOnEffectsThatDoNotSummon(t *testing.T) {
	spawn := `"spawn":{"kind":"area","duration_ticks":2,"interval_ticks":1,"area":{"from":"$caster","kind":"entity","shape":{"type":"circle","radius":4},"filters":[{"type":"targetable"}],"order":{"by":"stable_id","direction":"asc"},"limit":2}}`
	callbacks := `"on":{"enter":{"flow":"effect","effect":{"type":"heal","target":"$event.target","amount":1}}}`
	damage := `{"type":"damage","target":"$input.target","amount":1,"damage_type":"physical"}`
	heal := `{"type":"heal","target":"$caster","amount":1}`
	requireAgreementRejected(t, []agreementCase{
		{"damage with a spawn", agreementSkillJSON("entity", "{}", "[]", agreementSteps(`{"flow":"effect","effect":`+damage+`,`+spawn+`}`)), string(DiagnosticShapeInvalid), agreementEnterPath + ".spawn"},
		{"damage with callbacks", agreementSkillJSON("entity", "{}", "[]", agreementSteps(`{"flow":"effect","effect":`+damage+`,`+callbacks+`}`)), string(DiagnosticShapeInvalid), agreementEnterPath + ".on"},
		{"heal with a spawn and callbacks", agreementSkillJSON("none", "{}", "[]", agreementSteps(`{"flow":"effect","effect":`+heal+`,`+spawn+`,`+callbacks+`}`)), string(DiagnosticShapeInvalid), agreementEnterPath + ".spawn"},
	})
}

// RR-20261005-NC-223：B3（`023eb276`，v1.20.2）让 lower 对查不到的 `$local.` 引用报
// LOWER_UNRESOLVED。`lowerSnapshots` 却用空作用域 lower 全部快照计划的实体——包括
// current / each_tick / on_hit / on_event 这些运行期从不采样、只在读取处求值的计划——于是
// “对每个选中目标读它自己的属性”这类合法定义从 v1.20.2 起编译失败（定位在 `$`）。
// v1.20.1 能编译并执行（计划实体退成同名 builtin，从未被求值）。
func TestReadsOfALocalEntityAtNonCachedPointsCompileAndRun(t *testing.T) {
	for _, point := range []string{"current", "each_tick", "on_hit", "on_event"} {
		t.Run(point, func(t *testing.T) {
			environment := DefaultCompileEnvironment()
			for index := range environment.Gameplay.Attributes.Entries {
				if environment.Gameplay.Attributes.Entries[index].Key == "ability_power" && !containsString(environment.Gameplay.Attributes.Entries[index].Snapshots, point) {
					environment.Gameplay.Attributes.Entries[index].Snapshots = append(environment.Gameplay.Attributes.Entries[index].Snapshots, point)
				}
			}
			environment.Digest = authorityDigest(environment)
			each := `{"flow":"select","select":{"from":"$caster","kind":"entity","shape":{"type":"circle","radius":10},"filters":[{"type":"targetable"}],"order":{"by":"stable_id","direction":"asc"},"limit":2},"consume":{"mode":"each","as":"t","do":` + captureDamage("$local.t", captureRead("$local.t", point)) + `}}`
			program, diagnostics := Compile(mustParseJSON(t, agreementSkillJSON("none", "{}", "[]", agreementSteps(each))), environment)
			requireNoErrors(t, diagnostics)
			host := runtimeTestHost(environment)
			host.UpsertEntity(MemoryEntity{ID: 1, Alive: true, Health: 100, MaxHealth: 100, Resources: map[string]int64{"mana": 100}, Untargetable: true})
			host.UpsertEntity(MemoryEntity{ID: 2, Alive: true, Health: 100, MaxHealth: 100, Attributes: map[AttributeHandle]int64{2: 7}})
			host.UpsertEntity(MemoryEntity{ID: 3, Alive: true, Health: 100, MaxHealth: 100, Attributes: map[AttributeHandle]int64{2: 11}})
			runtime := NewRuntime(host, RuntimeOptions{})
			if _, err := runtime.Activate(program, CastInput{Caster: 1}); err != nil {
				t.Fatal(err)
			}
			if got2, got3 := host.HealthForTest(2), host.HealthForTest(3); got2 != 93 || got3 != 89 {
				t.Fatalf("each target must take its own ability power: health 2=%d 3=%d, want 93 / 89", got2, got3)
			}
		})
	}
}

// RR-20261005-NC-224：召唤效果上衍生物的字段（area 选择、motion、numeric track）在衍生物启动那一步
// 用施法求值，之后每一步用 detachedSpawnCast 求值——那里没有施法的输入、memory 与局部变量
// （AI 生成约束写明“Entity-scoped spawns cannot read expired Cast memory or input”，但
// 只对回调检查了）。旧实现按施法作用域检查这些字段：能编译，Activate 正常，下一 tick 起
// Advance 返回 ErrProgramInvariant（N09 第五批补的性质测试种子变异出 area.from=$input.target）。
func TestCompileRejectsCastValuesInOwnedSpawnFields(t *testing.T) {
	areaFrom := func(from string) string {
		return `{"flow":"effect","effect":{"type":"summon","template":"deployable.trap","position":"$caster.position","count":1,"duration_ticks":3},"spawn":{"kind":"area","duration_ticks":3,"interval_ticks":1,"area":{"from":"` + from + `","kind":"entity","shape":{"type":"circle","radius":4},"filters":[{"type":"targetable"}],"order":{"by":"stable_id","direction":"asc"},"limit":2}}}`
	}
	projectile := func(motion string) string {
		return `{"flow":"effect","effect":{"type":"summon","template":"deployable.trap","position":"$caster.position","count":1,"duration_ticks":5},"spawn":{"kind":"projectile","duration_ticks":5,"motion":` + motion + `}}`
	}
	eachTarget := func(do string) string {
		return `{"flow":"select","select":{"from":"$caster","kind":"entity","shape":{"type":"circle","radius":10},"filters":[],"order":{"by":"stable_id","direction":"asc"},"limit":1},"consume":{"mode":"each","as":"t","do":` + do + `}}`
	}
	enter := agreementEnterPath
	requireAgreementRejected(t, []agreementCase{
		{"area from an input", agreementSkillJSON("entity", "{}", "[]", agreementSteps(areaFrom("$input.target"))), string(DiagnosticInputUnavailable), enter + ".spawn.area.from"},
		{"area from a memory", agreementSkillJSON("none", `{"who":{"type":"entity","default":"$caster"}}`, "[]", agreementSteps(areaFrom("$memory.who"))), string(DiagnosticInputUnavailable), enter + ".spawn.area.from"},
		{"area from a flow local", agreementSkillJSON("none", "{}", "[]", agreementSteps(eachTarget(areaFrom("$local.t")))), string(DiagnosticInputUnavailable), enter + ".consume.do.spawn.area.from"},
		{"tracking an input", agreementSkillJSON("entity", "{}", "[]", agreementSteps(projectile(`{"frame":{"type":"world"},"steering":{"type":"tracking","target":"$input.target","duration_ticks":3},"trajectory":{"type":"linear","speed":1},"completion":{"type":"end"}}`))), string(DiagnosticInputUnavailable), enter + ".spawn.motion.steering.target"},
		{"following an input", agreementSkillJSON("entity", "{}", "[]", agreementSteps(projectile(`{"frame":{"type":"follow","target":"$input.target"},"trajectory":{"type":"linear","speed":1},"completion":{"type":"end"}}`))), string(DiagnosticInputUnavailable), enter + ".spawn.motion.frame.target"},
	})
}

// RR-20261005-NC-224 的控制：`$caster` 系列在启动与移交后都是施法者，照常编译并跑满衍生物时长。
func TestOwnedSpawnFieldsReadingTheCasterStillRun(t *testing.T) {
	area := `{"flow":"effect","effect":{"type":"summon","template":"deployable.trap","position":"$caster.position","count":1,"duration_ticks":3},"spawn":{"kind":"area","duration_ticks":3,"interval_ticks":1,"area":{"from":"$caster","kind":"entity","shape":{"type":"circle","radius":4},"filters":[{"type":"targetable"}],"order":{"by":"stable_id","direction":"asc"},"limit":2}},"on":{"tick":` + captureDamage("$event.target", "1") + `}}`
	tracking := `{"flow":"effect","effect":{"type":"summon","template":"deployable.trap","position":"$caster.position","count":1,"duration_ticks":5},"spawn":{"kind":"projectile","duration_ticks":5,"motion":{"frame":{"type":"world"},"steering":{"type":"tracking","target":"$caster","duration_ticks":3},"trajectory":{"type":"linear","speed":1},"completion":{"type":"end"}}}}`
	for name, steps := range map[string]string{"area": area, "tracking": tracking} {
		t.Run(name, func(t *testing.T) {
			program, diagnostics := Compile(mustParseJSON(t, agreementSkillJSON("none", "{}", "[]", agreementSteps(steps))), DefaultCompileEnvironment())
			requireNoErrors(t, diagnostics)
			host := runtimeTestHost(DefaultCompileEnvironment())
			runtime := NewRuntime(host, RuntimeOptions{})
			if _, err := runtime.Activate(program, CastInput{Caster: 1}); err != nil {
				t.Fatal(err)
			}
			for tick := 0; tick < 6; tick++ {
				if err := runtime.Advance(1); err != nil {
					t.Fatalf("advance %d: %v", tick, err)
				}
			}
		})
	}
}

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
	if runtime.spawns.count(spawnHandedOff) != 1 {
		t.Fatalf("owned spawns = %d, want the one minion spawn", runtime.spawns.count(spawnHandedOff))
	}
	for _, spawn := range handedOffSpawnRecords(runtime) {
		if spawn.EndTick-spawn.StartTick != 10 {
			t.Fatalf("minion spawn lives %d ticks, want the summon duration 10", spawn.EndTick-spawn.StartTick)
		}
	}
}

func agreementSkillJSON(input, memory, costs, enter string) string {
	text := strings.Replace(minimalSkillJSON, `{"flow":"finish","reason":"done"}`, enter, 1)
	text = strings.Replace(text, `"input_schema":{"type":"none"}`, `"input_schema":{"type":"`+input+`"}`, 1)
	text = strings.Replace(text, `"memory":{}`, `"memory":`+memory, 1)
	return strings.Replace(text, `"costs":[]`, `"costs":`+costs, 1)
}

func agreementSteps(steps ...string) string {
	return `{"flow":"sequence","steps":[` + strings.Join(append(steps, `{"flow":"finish"}`), ",") + `]}`
}

func agreementEffect(effect string) string { return `{"flow":"effect","effect":` + effect + `}` }

func agreementSelect(filter string) string {
	return `{"flow":"select","select":{"from":"$caster","kind":"entity","shape":{"type":"circle","radius":10},"filters":[` + filter + `],"order":{"by":"stable_id","direction":"asc"},"limit":2},"consume":{"mode":"each","as":"t","do":{"flow":"effect","effect":{"type":"heal","target":"$local.t","amount":1}}}}`
}

type agreementCase struct {
	name, input, code, path string
}

func requireAgreementRejected(t *testing.T, cases []agreementCase) {
	t.Helper()
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			program, diagnostics := Compile(mustParseJSON(t, test.input), DefaultCompileEnvironment())
			if program != nil {
				t.Fatalf("%s compiled; diagnostics=%#v", test.name, diagnostics)
			}
			requireDiagnosticAt(t, diagnostics, DiagnosticCode(test.code), test.path)
		})
	}
}

const agreementEnterPath = "$.phases[0].on.enter.steps[0]"

// RR-20261005-NC-210：set_memory / add_memory / clear_memory 的 name 必须是声明过的
// memory，add_memory 只能用在 int memory 上。旧实现不查名字，lower 用
// `c.memory[name]` 的零值 0：有别的 memory 时静默写进槽位 0（类型也不检查），
// 没有 memory 时 executeMemory 返回 ErrProgramInvariant；bool memory 上的
// add_memory 每次施法 ErrRuntimeTypeMismatch。
func TestCompileRejectsMemoryEffectsOnUndeclaredOrNonIntMemory(t *testing.T) {
	declared := `{"count":{"type":"int","default":0},"flag":{"type":"bool","default":false}}`
	requireAgreementRejected(t, []agreementCase{
		{"set undeclared", agreementSkillJSON("none", declared, "[]", agreementSteps(agreementEffect(`{"type":"set_memory","name":"missing","value":true}`))), string(DiagnosticReferenceUnknown), agreementEnterPath + ".effect.name"},
		{"add undeclared", agreementSkillJSON("none", declared, "[]", agreementSteps(agreementEffect(`{"type":"add_memory","name":"missing","value":1}`))), string(DiagnosticReferenceUnknown), agreementEnterPath + ".effect.name"},
		{"clear undeclared", agreementSkillJSON("none", declared, "[]", agreementSteps(agreementEffect(`{"type":"clear_memory","name":"missing"}`))), string(DiagnosticReferenceUnknown), agreementEnterPath + ".effect.name"},
		{"set without any memory", agreementSkillJSON("none", "{}", "[]", agreementSteps(agreementEffect(`{"type":"set_memory","name":"charged","value":true}`))), string(DiagnosticReferenceUnknown), agreementEnterPath + ".effect.name"},
		{"add on bool memory", agreementSkillJSON("none", declared, "[]", agreementSteps(agreementEffect(`{"type":"add_memory","name":"flag","value":true}`))), string(DiagnosticTypeMismatch), agreementEnterPath + ".effect.name"},
	})
}

// RR-20261005-NC-210 原触发的运行期后果：声明了 count 的技能里 set_memory 一个未声明
// 的名字，旧实现把 bool 写进 count 的槽位（槽位 0）。修复后它不再能编译，这里同时
// 钉住控制：声明过的名字照常写自己的槽位。
func TestDeclaredMemoryEffectsWriteTheirOwnSlot(t *testing.T) {
	declared := `{"count":{"type":"int","default":0},"flag":{"type":"bool","default":false}}`
	input := agreementSkillJSON("none", declared, "[]", agreementSteps(agreementEffect(`{"type":"add_memory","name":"count","value":2}`), agreementEffect(`{"type":"set_memory","name":"flag","value":true}`)))
	program, diagnostics := Compile(mustParseJSON(t, input), DefaultCompileEnvironment())
	requireNoErrors(t, diagnostics)
	host := runtimeTestHost(DefaultCompileEnvironment())
	runtime := NewRuntime(host, RuntimeOptions{})
	if _, err := runtime.Activate(program, CastInput{Caster: 1}); err != nil {
		t.Fatal(err)
	}
}

// RR-20261005-NC-212：catalog 里同一类条目的 key 必须唯一。旧实现只查 handle 唯一，
// 两个 unit template 用同一个 key 时，authority 表取后一个的 handle，typecheck 的
// unitTemplateEntry 取前一个的参数策略：bool 绑定到后一个 template 的 int 参数也能
// 编译，单个条目时同一定义被 TYPE_MISMATCH 拒绝。
func TestCompileEnvironmentRejectsDuplicateCatalogKeys(t *testing.T) {
	environment := DefaultCompileEnvironment()
	second := environment.Gameplay.UnitTemplates.Entries[0]
	second.Handle = 2
	second.Parameters = []UnitTemplateParameterPolicy{{Name: "power", ValueType: valueKindInt, Quantity: quantityCount, Minimum: 0, Maximum: 10}}
	environment.Gameplay.UnitTemplates.Entries = append(environment.Gameplay.UnitTemplates.Entries, second)
	environment.Digest = authorityDigest(environment)
	requireDiagnosticAt(t, validateCompileEnvironment(environment), DiagnosticCatalogDuplicateHandle, "$.gameplay.unit_templates[1].key")

	input := strings.Replace(string(mustReadFixture(t, "owned_trap.json")), `"duration_ticks":10}`, `"duration_ticks":10,"parameter_bindings":{"power":true}}`, 1)
	program, diagnostics := Compile(mustParseJSON(t, input), environment)
	if program != nil {
		t.Fatalf("duplicate unit template key compiled a bool binding for an int parameter; diagnostics=%#v", diagnostics)
	}

	for name, mutate := range map[string]func(*CompileEnvironment){
		"attributes": func(e *CompileEnvironment) {
			entry := e.Gameplay.Attributes.Entries[0]
			entry.Handle = 9
			e.Gameplay.Attributes.Entries = append(e.Gameplay.Attributes.Entries, entry)
		},
		"statuses": func(e *CompileEnvironment) {
			entry := e.Gameplay.Statuses.Entries[0]
			entry.Handle = 9
			e.Gameplay.Statuses.Entries = append(e.Gameplay.Statuses.Entries, entry)
		},
		"resources": func(e *CompileEnvironment) {
			e.Gameplay.Resources.Entries = append(e.Gameplay.Resources.Entries, ResourceCatalogEntry{Handle: 9, Key: "mana", Maximum: 1})
		},
		"tags": func(e *CompileEnvironment) {
			e.Gameplay.Tags.Entries = append(e.Gameplay.Tags.Entries, GameplayTagCatalogEntry{Handle: 9, Key: "spell", Classes: GameplayTagDeclarable})
		},
	} {
		t.Run(name, func(t *testing.T) {
			environment := DefaultCompileEnvironment()
			mutate(&environment)
			environment.Digest = authorityDigest(environment)
			if !diagnosticsHaveErrors(validateCompileEnvironment(environment)) {
				t.Fatalf("duplicate %s key accepted", name)
			}
		})
	}
}

// RR-20261005-NC-213：Runtime / Host 不执行的字段不能编译通过。chain 的
// allow_repeat 与 hop_interval_ticks、attribute_modifier 的 stack_policy 与
// max_stacks 都被 lower 进 Program、算进摘要，但 ChainSelectShape /
// AttributeModifierCommand 没有对应字段，作者写的语义被静默丢掉。
func TestCompileRejectsFieldsTheRuntimeDoesNotExecute(t *testing.T) {
	chain := func(shape string) string {
		return agreementSkillJSON("entity", "{}", "[]", agreementSteps(`{"flow":"select","select":{"from":"$input.target","kind":"entity","shape":{"type":"chain","hop_range":1,"max_targets":2,`+shape+`},"filters":[],"order":{"by":"stable_id","direction":"asc"},"limit":2},"consume":{"mode":"each","as":"t","do":{"flow":"effect","effect":{"type":"damage","target":"$local.t","amount":1,"damage_type":"physical"}}}}`))
	}
	modifier := func(extra string) string {
		return agreementSkillJSON("entity", "{}", "[]", agreementSteps(agreementEffect(`{"type":"attribute_modifier","target":"$input.target","attribute":"move_speed","operation":"add","value":5,"duration_ticks":10`+extra+`}`)))
	}
	requireAgreementRejected(t, []agreementCase{
		{"chain allow_repeat", chain(`"allow_repeat":true,"hop_interval_ticks":0`), string(DiagnosticShapeInvalid), agreementEnterPath + ".select.shape.allow_repeat"},
		{"chain hop_interval_ticks", chain(`"allow_repeat":false,"hop_interval_ticks":3`), string(DiagnosticShapeInvalid), agreementEnterPath + ".select.shape.hop_interval_ticks"},
		{"modifier stack_policy", modifier(`,"stack_policy":"stack"`), string(DiagnosticShapeInvalid), agreementEnterPath + ".effect.stack_policy"},
		{"modifier max_stacks", modifier(`,"max_stacks":3`), string(DiagnosticShapeInvalid), agreementEnterPath + ".effect.max_stacks"},
	})
	for name, input := range map[string]string{
		"chain defaults":    chain(`"allow_repeat":false,"hop_interval_ticks":0`),
		"modifier defaults": modifier(""),
	} {
		t.Run("control "+name, func(t *testing.T) {
			_, diagnostics := Compile(mustParseJSON(t, input), DefaultCompileEnvironment())
			requireNoErrors(t, diagnostics)
		})
	}
}

// RR-20261005-NC-214：effect、filter、cost 里引用的 status / attribute / resource
// 名字必须在 catalog 里。旧实现只有 damage_type、element、gameplay tag、collision、
// unit template 等查了；这几处由 lookup*Handle 的 map 零值兜底成 handle 0：
// add_status / resource / cost 每次施法被 Host 拒绝，remove_status、has_status、
// attribute_modifier、attribute_compare 静默作用在 handle 0 上。
func TestCompileRejectsUnknownCatalogNames(t *testing.T) {
	requireAgreementRejected(t, []agreementCase{
		{"add_status", agreementSkillJSON("entity", "{}", "[]", agreementSteps(agreementEffect(`{"type":"add_status","target":"$input.target","status":"missing","duration_ticks":5,"stacks":1}`))), string(DiagnosticCapabilityUnknown), agreementEnterPath + ".effect.status"},
		{"remove_status", agreementSkillJSON("entity", "{}", "[]", agreementSteps(agreementEffect(`{"type":"remove_status","target":"$input.target","status":"missing"}`))), string(DiagnosticCapabilityUnknown), agreementEnterPath + ".effect.status"},
		{"attribute_modifier", agreementSkillJSON("entity", "{}", "[]", agreementSteps(agreementEffect(`{"type":"attribute_modifier","target":"$input.target","attribute":"missing","operation":"add","value":5,"duration_ticks":10}`))), string(DiagnosticCapabilityUnknown), agreementEnterPath + ".effect.attribute"},
		{"resource", agreementSkillJSON("entity", "{}", "[]", agreementSteps(agreementEffect(`{"type":"resource","target":"$caster","resource":"missing","operation":"add","amount":1}`))), string(DiagnosticCapabilityUnknown), agreementEnterPath + ".effect.resource"},
		{"cost", agreementSkillJSON("none", "{}", `[{"resource":"missing","amount":1}]`, agreementSteps(agreementEffect(`{"type":"heal","target":"$caster","amount":1}`))), string(DiagnosticCapabilityUnknown), "$.costs[0].resource"},
		{"has_status filter", agreementSkillJSON("none", "{}", "[]", agreementSteps(agreementSelect(`{"type":"has_status","status":"missing"}`))), string(DiagnosticCapabilityUnknown), agreementEnterPath + ".select.filters[0].status"},
		{"attribute_compare filter", agreementSkillJSON("none", "{}", "[]", agreementSteps(agreementSelect(`{"type":"attribute_compare","attribute":"missing","op":"gt","value":0}`))), string(DiagnosticCapabilityUnknown), agreementEnterPath + ".select.filters[0].attribute"},
	})
	sustain := strings.Replace(agreementSkillJSON("none", "{}", "[]", `{"flow":"effect","effect":{"type":"heal","target":"$caster","amount":1}}`), `"policy":{"mode":"tap"}`, `"policy":{"mode":"toggle","pulse_interval_ticks":2,"max_duration_ticks":10,"sustain_costs":[{"resource":"missing","amount":1}]}`, 1)
	sustain = strings.Replace(sustain, `"on":{"enter":`, `"on":{"release":{"flow":"finish"},"enter":`, 1)
	requireAgreementRejected(t, []agreementCase{{"sustain cost", sustain, string(DiagnosticCapabilityUnknown), "$.activation.policy.sustain_costs[0].resource"}})
}

// RR-20261005-NC-215：两个参考 Host（MemoryHost、combatcomponent）与 Runtime 自己都
// 拒绝的取值，编译期就拒绝：attribute_modifier 的 operation 不在属性的
// ModifierOperations 里、duration_ticks <= 0，add_status duration_ticks 为 0，
// resource 的 operation 不是 set / add / spend / sub，cost 的字面量为负，
// attribute_compare 的 op 不是比较运算。旧实现这些都编译通过，施法时每次失败
// （attribute_compare 是静默全部不匹配）。
func TestCompileRejectsValuesEveryHostRejects(t *testing.T) {
	modifier := func(operation, duration string) string {
		return agreementSkillJSON("entity", "{}", "[]", agreementSteps(agreementEffect(`{"type":"attribute_modifier","target":"$input.target","attribute":"move_speed","operation":"`+operation+`","value":5,"duration_ticks":`+duration+`}`)))
	}
	requireAgreementRejected(t, []agreementCase{
		{"modifier operation", modifier("set", "10"), string(DiagnosticShapeInvalid), agreementEnterPath + ".effect.operation"},
		{"modifier zero duration", modifier("add", "0"), string(DiagnosticShapeInvalid), agreementEnterPath + ".effect.duration_ticks"},
		{"modifier negative duration", modifier("add", "-4"), string(DiagnosticShapeInvalid), agreementEnterPath + ".effect.duration_ticks"},
		{"add_status zero duration", agreementSkillJSON("entity", "{}", "[]", agreementSteps(agreementEffect(`{"type":"add_status","target":"$input.target","status":"slow","duration_ticks":0,"stacks":1}`))), string(DiagnosticShapeInvalid), agreementEnterPath + ".effect.duration_ticks"},
		{"resource operation", agreementSkillJSON("entity", "{}", "[]", agreementSteps(agreementEffect(`{"type":"resource","target":"$caster","resource":"mana","operation":"mul_bp","amount":1}`))), string(DiagnosticShapeInvalid), agreementEnterPath + ".effect.operation"},
		{"negative cost", agreementSkillJSON("none", "{}", `[{"resource":"mana","amount":-1}]`, agreementSteps(agreementEffect(`{"type":"heal","target":"$caster","amount":1}`))), string(DiagnosticShapeInvalid), "$.costs[0].amount"},
		{"attribute_compare op", agreementSkillJSON("none", "{}", "[]", agreementSteps(agreementSelect(`{"type":"attribute_compare","attribute":"health","op":"zz","value":0}`))), string(DiagnosticShapeInvalid), agreementEnterPath + ".select.filters[0].op"},
	})
}

// 控制：合法取值照常编译并在 MemoryHost 上施法成功；名字、取值与 catalog 一致时
// 本批新增的检查不能误报。
func TestCatalogConsistentEffectsStillCompileAndRun(t *testing.T) {
	cases := map[string]string{
		"modifier":   agreementSkillJSON("entity", "{}", "[]", agreementSteps(agreementEffect(`{"type":"attribute_modifier","target":"$input.target","attribute":"move_speed","operation":"mul_bp","value":9000,"duration_ticks":10}`))),
		"status":     agreementSkillJSON("entity", "{}", "[]", agreementSteps(agreementEffect(`{"type":"add_status","target":"$input.target","status":"slow","duration_ticks":5,"stacks":1}`), agreementEffect(`{"type":"remove_status","target":"$input.target","status":"slow"}`))),
		"resource":   agreementSkillJSON("entity", "{}", `[{"resource":"mana","amount":0}]`, agreementSteps(agreementEffect(`{"type":"resource","target":"$caster","resource":"mana","operation":"spend","amount":1}`))),
		"filters":    agreementSkillJSON("none", "{}", "[]", agreementSteps(agreementSelect(`{"type":"missing_status","status":"slow"},{"type":"attribute_compare","attribute":"health","op":"gte","value":0}`))),
		"chain":      string(mustReadFixture(t, "chain_lightning.json")),
		"memory set": agreementSkillJSON("none", `{"flag":{"type":"bool","default":false}}`, "[]", agreementSteps(agreementEffect(`{"type":"set_memory","name":"flag","value":true}`), agreementEffect(`{"type":"clear_memory","name":"flag"}`))),
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			environment := DefaultCompileEnvironment()
			program, diagnostics := Compile(mustParseJSON(t, input), environment)
			requireNoErrors(t, diagnostics)
			castInput := CastInput{Caster: 1}
			if strings.Contains(input, `"input_schema":{"type":"entity"}`) || name == "chain" {
				castInput.Target = 2
			}
			runtime := NewRuntime(runtimeTestHost(environment), RuntimeOptions{})
			castID, err := runtime.Activate(program, castInput)
			if err != nil {
				t.Fatal(err)
			}
			if cast, _ := runtime.InspectCast(castID); cast.Status != CastFinished {
				t.Fatalf("cast = %#v, want finished", cast)
			}
		})
	}
}

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
