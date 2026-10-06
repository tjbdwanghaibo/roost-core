package skill

import (
	"fmt"
	"testing"
)

// 本文件钉住 N09 第五批（RR-20261005-NC-220 / 221 / 222）的同一条承诺：编译器接受的
// 定义，Runtime 能按作者写的意思执行。三处旧实现都是“编译通过，Runtime 在别的上下文
// 里执行或干脆不执行”：
//   - NC-220：缓存型快照点（cast_start / phase_start / spawn_start）的整个读取在
//     采样点求值，编译期却按读取所在位置检查；
//   - NC-221：被动技能的输入只来自触发事件的 target、proc 深度按 `ProcDepth >= max_depth`
//     压制，编译期不查这两个前提；
//   - NC-222：只有召唤效果（summon）会启动衍生物，其他效果上的 `spawn` / `on` 编译后被丢弃。

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
