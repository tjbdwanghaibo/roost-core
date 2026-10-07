package skill

// 衍生物“施放 → 移交 → 停止”端到端用例（维护者 2026-10-07：先补端到端用例，再在这套用例上修
// F08-R / F08-H+，不再逐条打补丁；docs/framework/impl/08-skill.md §11）。
//
// 在 MemoryHost 上跑真实的 Activate / Advance / Cancel / Interrupt / Shutdown，覆盖 area、projectile、beam、
// orbit 四种衍生物，每种走一遍：召唤后立即 finish、召唤后 wait 再 finish（移交前后都在推进）、同上但施放中
// Checkpoint / RestoreRuntime、wait 超过衍生物寿命（施放中到期）、施放中 Cancel / Interrupt、施放中与移交后 Shutdown。
// 每个 tick 断言：
//
//   - 效果按 tick 结算：衍生物每一步的 tick 回调在它该跑的每个 tick 恰好跑一次，不丢、不重复，与什么时候移交无关；
//     停止之后不再结算；到期跑一次 end 回调，Cancel / Interrupt / Shutdown（施放中与移交后）跑一次 cancel 回调
//     （施放中 Shutdown 这一格是 RR-20261006-55 后续补的断言，修前 0 次）；
//   - 事件链：衍生物回调里的效果继承施法的 RootEventID / ProcDepth，每次结算的 EventID 互不相同，父事件存在且
//     不是自己；proc 施放的第 0 号效果不与施法事件撞号；max_depth 与 once_per_root 按“衍生物属于施法的因果链”生效；
//   - Host 侧状态：MemoryHost 里仍 active 的衍生物，恰好是 Runtime 仍负责的衍生物（running / stop_pending），没有残留。
//
// 另有：施放中 area 回调在之后的 tick finish 时结束施法；推进入口的源码守卫（只有 advanceOwnedSpawns 与启动步）。
//
// 修前红（基线 47abe511）：RR-20261006-51（施放中不推进，移交前的 tick 丢失）、RR-20261006-52（运动衍生物启动步
// Frame 之后被拒，Host 侧残留）、RR-20261006-53（事件链不继承、EventID 每 tick 相同）、RR-20261006-54（proc 第 0 号
// 效果撞号）。失败文本抄在各自的 docs/bug 记录里。

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// spawnLifecycleHost 记录每一次伤害命令（tick、目标、事件上下文），可让某一类运动步骤被拒一次。
type spawnLifecycleHost struct {
	*MemoryHost
	damages  []recordedSpawnDamage
	failStep func(SpawnStepCommand) bool
}

type recordedSpawnDamage struct {
	tick   Tick
	target EntityID
	event  EventContext
}

var errSpawnStepRefused = errors.New("test host: spawn step refused")

func (host *spawnLifecycleHost) Apply(command EffectCommand) (EffectResult, error) {
	if damage, ok := command.Payload.(DamageCommand); ok {
		host.damages = append(host.damages, recordedSpawnDamage{tick: host.tick, target: damage.Target, event: cloneEventContext(damage.Event)})
	}
	return host.MemoryHost.Apply(command)
}

func (host *spawnLifecycleHost) StepSpawn(command SpawnStepCommand, state SpawnHostState) (SpawnStepResult, error) {
	if host.failStep != nil && host.failStep(command) {
		host.failStep = nil
		return SpawnStepResult{}, errSpawnStepRefused
	}
	return host.MemoryHost.StepSpawn(command, state)
}

// damagesAt 返回某个 tick 由某个技能的衍生物回调造成的伤害（SpawnID 非零的事件来自衍生物回调）。
func (host *spawnLifecycleHost) spawnDamagesAt(tick Tick) []recordedSpawnDamage {
	result := make([]recordedSpawnDamage, 0)
	for _, damage := range host.damages {
		if damage.tick == tick && damage.event.SpawnID != 0 {
			result = append(result, damage)
		}
	}
	return result
}

// assertHostSpawnsMatchRuntime：Host 侧仍 active 的衍生物 = Runtime 仍负责的衍生物（运行中 / 待停止）。
func assertHostSpawnsMatchRuntime(t *testing.T, label string, host *MemoryHost, runtime *Runtime) {
	t.Helper()
	host.mutex.Lock()
	active := make([]SpawnID, 0)
	for id, spawn := range host.spawns {
		if spawn.active {
			active = append(active, id)
		}
	}
	host.mutex.Unlock()
	runtime.mutex.Lock()
	live := runtime.spawns.sortedIDs(nil, spawnLivePartitions...)
	runtime.mutex.Unlock()
	sort.Slice(active, func(i, j int) bool { return active[i] < active[j] })
	if fmt.Sprint(active) != fmt.Sprint(live) {
		t.Errorf("%s: host active spawns = %v, runtime live spawns = %v; the host must not keep a spawn the runtime no longer stops", label, active, live)
	}
}

func spawnLifecycleSkill(id, activation, flow string) string {
	return `{"schema":"roost.skill/v2","id":"skill.test.e2e.` + id + `","name":"E2E","description":"Spawn lifecycle.","gameplay_tags":["spell"],"activation":` + activation + `,"input_schema":{"type":"none"},"cooldown_ticks":0,"costs":[],"memory":{},"initial_phase":"cast","phases":[{"id":"cast","timeout_ticks":0,"on":{"enter":` + flow + `}}]}`
}

const spawnLifecycleActive = `{"type":"active","policy":{"mode":"tap"},"cast_window":{"windup_ticks":0,"commit_tick":0,"recovery_ticks":0,"movement":"locked","turning":"allowed","interrupt_tags":["spell"],"refund_before_commit":false}}`

const spawnLifecycleMark = `{"flow":"effect","effect":{"type":"issue_entity_command","target":"$lifecycle_entity","command":"hold_position"}}`

// spawnLifecycleKind 是一种衍生物：它的模板、每个 tick 回调造成几次伤害、自然寿命里最后一个有 tick 回调的 tick。
type spawnLifecycleKind struct {
	name     string
	spawn    string
	tick     string
	perTick  int
	lastTick Tick
}

func spawnLifecycleKinds() []spawnLifecycleKind {
	damageCaster := `{"flow":"effect","effect":{"type":"damage","target":"$owner","amount":1,"damage_type":"physical"}}`
	damageMember := `{"flow":"effect","effect":{"type":"damage","target":"$event.target","amount":1,"damage_type":"physical"}}`
	return []spawnLifecycleKind{
		// area：6 tick、间隔 1，成员是施法者 1 与实体 2，每个 tick 对每个成员结算一次；第 6 tick 到期。
		{name: "area", spawn: `{"kind":"area","duration_ticks":6,"interval_ticks":1,"area":{"from":"$caster","kind":"entity","shape":{"type":"circle","radius":10},"filters":[{"type":"targetable"}],"order":{"by":"stable_id","direction":"asc"},"limit":2}}`, tick: damageMember, perTick: 2, lastTick: 5},
		// 运动衍生物：4 tick，completion end 在第 4 tick 那一步完成，所以 0～4 每 tick 一次。
		{name: "projectile", spawn: `{"kind":"projectile","duration_ticks":4,"motion":{"frame":{"type":"world"},"trajectory":{"type":"linear","speed":1},"completion":{"type":"end"}}}`, tick: damageCaster, perTick: 1, lastTick: 4},
		{name: "beam", spawn: `{"kind":"beam","duration_ticks":4,"motion":{"frame":{"type":"world"},"trajectory":{"type":"stationary"},"completion":{"type":"end"}}}`, tick: damageCaster, perTick: 1, lastTick: 4},
		{name: "orbit", spawn: `{"kind":"orbit","duration_ticks":4,"motion":{"frame":{"type":"world"},"trajectory":{"type":"orbit","anchor":"$caster","radius":10,"angular_speed":1000},"completion":{"type":"end"}}}`, tick: damageCaster, perTick: 1, lastTick: 4},
	}
}

func spawnLifecycleSummon(kind spawnLifecycleKind) string {
	return `{"flow":"effect","effect":{"type":"summon","template":"deployable.trap","position":"$caster.position","count":1,"duration_ticks":30},"spawn":` + kind.spawn + `,"on":{"tick":` + kind.tick + `,"end":` + spawnLifecycleMark + `,"cancel":` + spawnLifecycleMark + `}}`
}

// spawnLifecycleScenario 是施法从召唤到停止的一条路径。stopTick 之后（含停止发生在这个 tick 的 Advance 之后）
// 不再结算；-1 表示不提前停止。
type spawnLifecycleScenario struct {
	name       string
	tail       string
	actionTick Tick
	action     func(runtime *Runtime, castID CastID, program *Program) error
	stopTick   Tick
	wantEnd    int
	wantCancel int
	// restoreTick > 0：在这个 tick 的 Advance 之后 Checkpoint、在同一个 Host 上 RestoreRuntime，之后用恢复出的 Runtime
	// 继续（施放中衍生物的 next_tick 与回调事件 ID 计数随 checkpoint 保存，恢复后不丢步、ID 不重复）。
	restoreTick Tick
}

func spawnLifecycleScenarios() []spawnLifecycleScenario {
	cancel := func(runtime *Runtime, castID CastID, _ *Program) error { return runtime.Cancel(castID) }
	interrupt := func(runtime *Runtime, castID CastID, program *Program) error {
		return runtime.Interrupt(castID, program.cast.interruptTags[0])
	}
	shutdown := func(runtime *Runtime, _ CastID, _ *Program) error { return runtime.Shutdown() }
	removeProgram := func(runtime *Runtime, _ CastID, program *Program) error { return runtime.RemoveProgram(program.id) }
	wait := func(ticks int) string {
		return `{"flow":"wait","ticks":` + intString(ticks) + `,"then":{"flow":"finish"}}`
	}
	return []spawnLifecycleScenario{
		{name: "finish at once", tail: `{"flow":"finish"}`, stopTick: -1, wantEnd: 1},
		{name: "wait 3 then finish", tail: wait(3), stopTick: -1, wantEnd: 1},
		{name: "wait 3 with checkpoint restore while casting", tail: wait(3), stopTick: -1, wantEnd: 1, restoreTick: 1},
		{name: "wait past expiry", tail: wait(10), stopTick: -1, wantEnd: 1},
		{name: "cancel while casting", tail: wait(10), actionTick: 2, action: cancel, stopTick: 2, wantCancel: 1},
		{name: "interrupt while casting", tail: wait(10), actionTick: 2, action: interrupt, stopTick: 2, wantCancel: 1},
		{name: "shutdown while casting", tail: wait(10), actionTick: 2, action: shutdown, stopTick: 2, wantCancel: 1},
		{name: "shutdown after handoff", tail: `{"flow":"finish"}`, actionTick: 2, action: shutdown, stopTick: 2, wantCancel: 1},
		// RemoveProgram 停施放中的 entity 衍生物也跑一次 cancel，和 Shutdown / Cancel / Interrupt 一致（RR-20261006-55 后续二）。
		{name: "remove program while casting", tail: wait(10), actionTick: 2, action: removeProgram, stopTick: 2, wantCancel: 1},
		{name: "remove program after handoff", tail: `{"flow":"finish"}`, actionTick: 2, action: removeProgram, stopTick: 2, wantCancel: 1},
	}
}

func TestSpawnLifecycleSettlesEveryTickFromCastToHandoffToStop(t *testing.T) {
	for _, kind := range spawnLifecycleKinds() {
		for _, scenario := range spawnLifecycleScenarios() {
			t.Run(kind.name+"/"+scenario.name, func(t *testing.T) {
				flow := `{"flow":"sequence","steps":[` + spawnLifecycleSummon(kind) + `,` + scenario.tail + `]}`
				program, environment := compileRuntimeJSON(t, spawnLifecycleSkill(kind.name, spawnLifecycleActive, flow))
				host := &spawnLifecycleHost{MemoryHost: runtimeTestHost(environment)}
				runtime := NewRuntime(host, RuntimeOptions{})
				castID, err := runtime.Activate(program, CastInput{Caster: 1})
				if err != nil {
					t.Fatal(err)
				}
				lastTick := kind.lastTick
				if scenario.stopTick >= 0 && scenario.stopTick < lastTick {
					lastTick = scenario.stopTick
				}
				for tick := Tick(0); tick <= 14; tick++ {
					if tick > 0 {
						if err := runtime.Advance(tick); err != nil {
							t.Fatalf("Advance(%d) = %v", tick, err)
						}
					}
					if scenario.restoreTick > 0 && tick == scenario.restoreTick {
						checkpoint, err := runtime.Checkpoint()
						if err != nil {
							t.Fatalf("Checkpoint at tick %d: %v", tick, err)
						}
						resolver := ProgramResolverFunc(func(id, digest string) (*Program, error) {
							if id == program.id {
								return program, nil
							}
							return nil, ErrCheckpointProgram
						})
						if runtime, err = RestoreRuntime(host, RuntimeOptions{}, checkpoint, resolver); err != nil {
							t.Fatalf("RestoreRuntime at tick %d: %v", tick, err)
						}
					}
					if scenario.action != nil && tick == scenario.actionTick {
						if err := scenario.action(runtime, castID, program); err != nil {
							t.Fatalf("tick %d action = %v", tick, err)
						}
					}
					want := 0
					if tick <= lastTick {
						want = kind.perTick
					}
					if got := host.spawnDamagesAt(tick); len(got) != want {
						t.Errorf("tick %d: spawn tick damages = %d, want %d (every tick the spawn runs settles exactly once, before and after handoff)", tick, len(got), want)
					}
					assertHostSpawnsMatchRuntime(t, fmt.Sprintf("tick %d", tick), host.MemoryHost, runtime)
				}
				events := runtime.RuntimeEvents()
				if got := countRuntimeEvents(events, "owned_spawn_callback_end"); got != scenario.wantEnd {
					t.Errorf("end callbacks = %d, want %d", got, scenario.wantEnd)
				}
				if got := countRuntimeEvents(events, "owned_spawn_callback_cancel"); scenario.wantCancel >= 0 && got != scenario.wantCancel || got > 1 {
					t.Errorf("cancel callbacks = %d, want %d", got, scenario.wantCancel)
				}
				assertSpawnDamageChain(t, host.damages, EventID(castID), 0)
			})
		}
	}
}

// assertSpawnDamageChain：衍生物回调里的伤害继承施法的根事件与深度，每次结算一个新的 EventID，父事件存在且不是自己。
// 每类违例只报第一条和总数。
func assertSpawnDamageChain(t *testing.T, damages []recordedSpawnDamage, root EventID, depth int) {
	t.Helper()
	seen := make(map[EventID]recordedSpawnDamage)
	violations := make(map[string][]string)
	order := make([]string, 0)
	report := func(kind, label string) {
		if _, ok := violations[kind]; !ok {
			order = append(order, kind)
		}
		violations[kind] = append(violations[kind], label)
	}
	for _, damage := range damages {
		event := damage.event
		label := fmt.Sprintf("tick %d target %d effect %d spawn %d (id=%d root=%d parent=%d depth=%d)", damage.tick, damage.target, event.EffectIndex, event.SpawnID, event.EventID, event.RootEventID, event.ParentEventID, event.ProcDepth)
		if event.RootEventID != root || event.ProcDepth != depth {
			report(fmt.Sprintf("want root %d depth %d inherited from the cast", root, depth), label)
		}
		if event.ParentEventID == 0 || event.ParentEventID == event.EventID {
			report("the parent event must exist and differ from the event", label)
		}
		if _, duplicate := seen[event.EventID]; duplicate {
			report("EventID reused; every settlement is a new event", label)
		}
		seen[event.EventID] = damage
	}
	for _, kind := range order {
		t.Errorf("%s: %d of %d damages, first %s", kind, len(violations[kind]), len(damages), violations[kind][0])
	}
}

// R2：运动衍生物启动步里 Frame 之后的步骤被 Host 拒绝。Activate 报错、召唤回滚，Host 侧不能留着一个
// Runtime 不记得、谁都不会停的衍生物。
func TestRefusedMotionStartStepLeavesNoHostSpawn(t *testing.T) {
	steps := map[string]func(SpawnStepCommand) bool{
		"trajectory": func(command SpawnStepCommand) bool { _, ok := command.Motion.(TrajectoryMotionStep); return ok },
		"completion": func(command SpawnStepCommand) bool { _, ok := command.Motion.(CompletionMotionStep); return ok },
	}
	for _, kind := range spawnLifecycleKinds()[1:] {
		for stepName, fail := range steps {
			t.Run(kind.name+"/"+stepName, func(t *testing.T) {
				flow := `{"flow":"sequence","steps":[` + spawnLifecycleSummon(kind) + `,{"flow":"finish"}]}`
				program, environment := compileRuntimeJSON(t, spawnLifecycleSkill(kind.name, spawnLifecycleActive, flow))
				host := &spawnLifecycleHost{MemoryHost: runtimeTestHost(environment), failStep: fail}
				runtime := NewRuntime(host, RuntimeOptions{})
				if _, err := runtime.Activate(program, CastInput{Caster: 1}); !errors.Is(err, errSpawnStepRefused) {
					t.Fatalf("Activate = %v, want the refused step", err)
				}
				assertHostSpawnsMatchRuntime(t, "after the refused start", host.MemoryHost, runtime)
				for tick := Tick(1); tick <= 3; tick++ {
					if err := runtime.Advance(tick); err != nil {
						t.Fatalf("Advance(%d) = %v", tick, err)
					}
				}
				if err := runtime.Shutdown(); err != nil {
					t.Fatal(err)
				}
				assertHostSpawnsMatchRuntime(t, "after Shutdown", host.MemoryHost, runtime)
			})
		}
	}
}

// spawnReactorRouter 把召唤技能造成的伤害事件路由给一个 reactor 被动。
type spawnReactorRouter struct {
	source  string
	reactor *Program
}

func (router spawnReactorRouter) Candidates(event EventContext) []PassiveCandidate {
	if event.SkillID != router.source || event.EventID == 0 {
		return nil
	}
	return []PassiveCandidate{{Program: router.reactor, Owner: 2}}
}

func spawnReactorProgram(t *testing.T, maxDepth int, once bool) *Program {
	t.Helper()
	activation := `{"type":"passive_on_damaged","cooldown_scope":"caster","event_filter":{"required_tags":[],"excluded_tags":[],"elements":[],"damage_types":[],"results":[]},"proc_policy":{"max_depth":` + intString(maxDepth) + `,"allow_self_trigger":false,"once_per_root_event":` + fmt.Sprint(once) + `}}`
	program, _ := compileRuntimeJSON(t, spawnLifecycleSkill("reactor", activation, `{"flow":"finish","reason":"done"}`))
	return program
}

func reactorOutcomes(runtime *Runtime, reactor string) (activated int, suppressed map[string]int) {
	suppressed = make(map[string]int)
	for _, event := range runtime.RuntimeEvents() {
		if event.Context.SkillID != reactor {
			continue
		}
		switch event.Kind {
		case "passive_activated":
			activated++
		case "passive_suppressed":
			suppressed[event.Context.Result]++
		}
	}
	return activated, suppressed
}

// once_per_root：施法自己的伤害与它召唤的 area 每个 tick 的伤害同属一个根事件（施法），reactor 只触发一次。
func TestSpawnCallbacksShareTheCastRootForOncePerRoot(t *testing.T) {
	kind := spawnLifecycleKinds()[0]
	flow := `{"flow":"sequence","steps":[{"flow":"effect","effect":{"type":"damage","target":"$caster","amount":1,"damage_type":"physical"}},` + spawnLifecycleSummon(kind) + `,{"flow":"finish"}]}`
	program, environment := compileRuntimeJSON(t, spawnLifecycleSkill("once", spawnLifecycleActive, flow))
	reactor := spawnReactorProgram(t, 5, true)
	host := &spawnLifecycleHost{MemoryHost: runtimeTestHost(environment)}
	runtime := NewRuntime(host, RuntimeOptions{PassiveRouter: spawnReactorRouter{source: program.id, reactor: reactor}})
	castID, err := runtime.Activate(program, CastInput{Caster: 1})
	if err != nil {
		t.Fatal(err)
	}
	for tick := Tick(1); tick <= 8; tick++ {
		if err := runtime.Advance(tick); err != nil {
			t.Fatalf("Advance(%d) = %v", tick, err)
		}
	}
	activated, suppressed := reactorOutcomes(runtime, reactor.id)
	if activated != 1 || suppressed["once_per_root"] == 0 {
		t.Fatalf("reactor activated %d times, suppressed %v; want once for the whole cast root (the cast's damage and every area tick)", activated, suppressed)
	}
	assertSpawnDamageChain(t, host.damages, EventID(castID), 0)
}

// max_depth 与 proc 施放的事件链：召唤技能本身由一个深度 3 的事件触发（施法深度 4、根 77）。衍生物回调的伤害
// 继承深度 4，max_depth 4 的 reactor 一律被挡；第 0 号效果（伤害）的事件 ID 不与施法事件撞号。
func TestProcCastSpawnCallbacksKeepDepthAndRoot(t *testing.T) {
	kind := spawnLifecycleKinds()[0]
	activation := `{"type":"passive_on_damaged","cooldown_scope":"caster","event_filter":{"required_tags":[],"excluded_tags":[],"elements":[],"damage_types":[],"results":[]},"proc_policy":{"max_depth":8,"allow_self_trigger":false,"once_per_root_event":false}}`
	flow := `{"flow":"sequence","steps":[{"flow":"effect","effect":{"type":"damage","target":"$caster","amount":1,"damage_type":"physical"}},` + spawnLifecycleSummon(kind) + `,{"flow":"finish"}]}`
	program, environment := compileRuntimeJSON(t, spawnLifecycleSkill("proc", activation, flow))
	reactor := spawnReactorProgram(t, 4, false)
	host := &spawnLifecycleHost{MemoryHost: runtimeTestHost(environment)}
	runtime := NewRuntime(host, RuntimeOptions{PassiveRouter: spawnReactorRouter{source: program.id, reactor: reactor}})
	if _, err := runtime.ActivatePassive(program, EventContext{EventID: 900, RootEventID: 77, ProcDepth: 3, Owner: 1, Source: 2}); err != nil {
		t.Fatal(err)
	}
	for tick := Tick(0); tick <= 8; tick++ {
		if err := runtime.Advance(tick); err != nil {
			t.Fatalf("Advance(%d) = %v", tick, err)
		}
	}
	if len(host.damages) == 0 {
		t.Fatal("the proc cast settled no damage")
	}
	first := host.damages[0].event
	if first.SpawnID != 0 || first.EffectIndex != 0 {
		t.Fatalf("first damage = %+v, want the cast's effect 0", first)
	}
	if first.EventID == first.ParentEventID {
		t.Errorf("effect 0 event id %d equals its parent (the proc cast's own event); the cast and its effect 0 must not share an id", first.EventID)
	}
	assertSpawnDamageChain(t, host.damages, 77, 4)
	activated, suppressed := reactorOutcomes(runtime, reactor.id)
	if activated != 0 || suppressed["max_depth"] == 0 {
		t.Fatalf("reactor activated %d times, suppressed %v; want every depth-4 event stopped by max_depth 4", activated, suppressed)
	}
	if strings.Contains(fmt.Sprint(suppressed), "unavailable") {
		t.Fatalf("suppressed = %v", suppressed)
	}
}

// 施放中 area 衍生物的回调在之后某个 tick 执行 finish（施法停在 wait 上）：结束拥有它的施法——撤掉余下的流程（wait 之后的
// 伤害不再执行）、走正常收尾，area 停止、不再结算（RR-20261006-51 之后施放中的衍生物才会在之后的 tick 上跑回调）。
func TestCastingAreaFinishAtALaterTickFinishesTheCast(t *testing.T) {
	flow := `{"flow":"sequence","steps":[{"flow":"effect","effect":{"type":"summon","template":"deployable.trap","position":"$caster.position","count":1,"duration_ticks":30},"spawn":{"kind":"area","duration_ticks":6,"interval_ticks":1,"area":{"from":"$caster","kind":"entity","shape":{"type":"circle","radius":10},"filters":[{"type":"targetable"}],"order":{"by":"stable_id","direction":"asc"},"limit":2}},"on":{"enter":{"flow":"finish"}}},{"flow":"wait","ticks":10,"then":{"flow":"sequence","steps":[{"flow":"effect","effect":{"type":"damage","target":"$caster","amount":1,"damage_type":"physical"}},{"flow":"finish"}]}}]}`
	program, environment := compileRuntimeJSON(t, spawnLifecycleSkill("area-finish", spawnLifecycleActive, flow))
	host := &areaSnapshotHost{MemoryHost: runtimeTestHost(environment), snapshots: [][]EntityID{{}, {}, {2}}}
	runtime := NewRuntime(host, RuntimeOptions{})
	castID, err := runtime.Activate(program, CastInput{Caster: 1})
	if err != nil {
		t.Fatal(err)
	}
	for tick := Tick(1); tick <= 14; tick++ {
		if err := runtime.Advance(tick); err != nil {
			t.Fatalf("Advance(%d) = %v", tick, err)
		}
		if tick == 2 {
			if cast, _ := runtime.InspectCast(castID); cast.Status != CastFinished && cast.WindowStage != CastWindowRecovering && cast.WindowStage != CastWindowComplete {
				t.Fatalf("cast at tick 2: status %s stage %s, want finished by the area callback", cast.Status, cast.WindowStage)
			}
		}
		assertHostSpawnsMatchRuntime(t, fmt.Sprintf("tick %d", tick), host.MemoryHost, runtime)
	}
	if got := countRuntimeEvents(runtime.RuntimeEvents(), "owned_spawn_callback_enter"); got != 1 {
		t.Fatalf("enter callbacks = %d, want 1", got)
	}
	if health := host.MemoryHost.entities[1].Health; health != 100 {
		t.Fatalf("caster health = %d, want 100: the flow after the wait must not run once the cast finished", health)
	}
	if live := runtime.spawns.count(spawnLivePartitions...); live != 0 {
		t.Fatalf("live spawns = %d, want the finishing area stopped", live)
	}
}

// 推进入口守卫（RR-20261006-51）：衍生物的一步（stepSpawnMotion / stepAreaMembership）只在两处发生——启动那一步
// （startEntitySpawn）与逐 tick 推进（advanceOwnedSpawns）。与“分区只由 spawnTable 写”“停止只经 requestSpawnStop”
// 两条守卫一起，衍生物的推进、存放、停止各只有一个入口；新增一条推进路径（例如之前只在 checkpoint 恢复里存在的
// spawnStepTask）会在这里变红。
func TestSpawnStepsHaveOneTickEntry(t *testing.T) {
	allowed := map[string]bool{"startEntitySpawn": true, "advanceOwnedSpawns": true}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fileSet := token.NewFileSet()
	callers := map[string]bool{}
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		source, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		file, err := parser.ParseFile(fileSet, name, source, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, declaration := range file.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok || function.Body == nil {
				continue
			}
			ast.Inspect(function.Body, func(node ast.Node) bool {
				if call, ok := node.(*ast.CallExpr); ok {
					if selector, ok := call.Fun.(*ast.SelectorExpr); ok && (selector.Sel.Name == "stepSpawnMotion" || selector.Sel.Name == "stepAreaMembership") {
						callers[function.Name.Name] = true
					}
				}
				return true
			})
		}
	}
	for caller := range callers {
		if !allowed[caller] {
			t.Errorf("%s steps a spawn; spawns advance only in advanceOwnedSpawns (and their start step in startEntitySpawn)", caller)
		}
	}
	for caller := range allowed {
		if !callers[caller] {
			t.Errorf("%s no longer steps spawns; update the guard together with the advancing entry", caller)
		}
	}
}
