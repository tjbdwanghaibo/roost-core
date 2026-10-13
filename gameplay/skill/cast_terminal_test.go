package skill

import (
	"errors"
	"testing"
)

// 施法的每条终止路径（启动失败、Cancel / Interrupt / Release 中途出错、排程任务失败）都把 cast 收敛到同一个终态：
// 本 cast 的排程任务与帧全部撤掉、衍生物停止、policy 槽位释放、不再占施法者窗口。
// 历史：RR-20261005-NC-110～112。

const terminalChargeEnter = `{"flow":"parallel","branches":[{"flow":"wait","ticks":3,"then":{"flow":"effect","effect":{"type":"damage","target":"$input.target","amount":40,"damage_type":"true"}}},{"flow":"effect","effect":{"type":"resource","target":"$caster","resource":"mana","operation":"spend","amount":50}}]}`

func compileTerminalCharge(t *testing.T) (*Program, CompileEnvironment) {
	t.Helper()
	json := `{"schema":"roost.skill/v2","id":"skill.test.terminal.charge","name":"Terminal","description":"Charge whose enter schedules a wait, then fails.","activation":{"type":"active","policy":{"mode":"charge","max_charge_ticks":10,"min_charge_bp":0,"auto_release":false}},"input_schema":{"type":"entity"},"cooldown_ticks":0,"costs":[],"memory":{},"initial_phase":"cast","phases":[{"id":"cast","timeout_ticks":0,"on":{"enter":` + terminalChargeEnter + `,"release":{"flow":"finish"}}}]}`
	return compileRuntimeJSON(t, json)
}

func setTerminalMana(host *MemoryHost, mana int64) {
	host.UpsertEntity(MemoryEntity{ID: 1, Alive: true, Health: 100, MaxHealth: 100, Resources: map[string]int64{"mana": mana}})
}

func scheduledTasksFor(runtime *Runtime, id CastID) int {
	count := 0
	for _, task := range runtime.scheduler.tasks {
		if castID, _ := scheduledTaskIdentity(task.Payload); castID == id {
			count++
		}
	}
	return count
}

// 启动失败、未提交的 cast 被删除且 ID 复用：它已排程的等待不能落到下一个拿到同一 ID 的 cast 上。
func TestFailedStartLeavesNoScheduledWorkForTheReusedCastID(t *testing.T) {
	program, environment := compileTerminalCharge(t)
	host := runtimeTestHost(environment)
	setTerminalMana(host, 10)
	runtime := NewRuntime(host, RuntimeOptions{})
	if id, err := runtime.Start(program, CastInput{Caster: 1, Target: 2}); !errors.Is(err, ErrInsufficientResource) || id != 0 {
		t.Fatalf("first start = %d, %v; want 0, ErrInsufficientResource", id, err)
	}
	if left := scheduledTasksFor(runtime, 1); left != 0 || len(runtime.frames) != 0 {
		t.Errorf("failed start left %d scheduled tasks and %d frames for cast 1", left, len(runtime.frames))
	}
	// 失败启动的 cast 被删除、ID 复用：完成队列里也不能残留指向这个 ID 的条目。
	if len(runtime.completedCastOrder) != 0 {
		t.Errorf("completed queue after a deleted failed start = %v", runtime.completedCastOrder)
	}
	if err := runtime.Advance(1); err != nil {
		t.Fatal(err)
	}
	setTerminalMana(host, 100)
	id, err := runtime.Start(program, CastInput{Caster: 1, Target: 2})
	if err != nil || id != 1 {
		t.Fatalf("second start = %d, %v; want the reused id 1", id, err)
	}
	if err := runtime.Advance(3); err != nil {
		t.Fatalf("advance to 3: %v", err)
	}
	if got := host.HealthForTest(2); got != 100 {
		t.Fatalf("health at tick 3 = %d: the rejected cast's wait landed on the reused id (second cast is due at tick 4)", got)
	}
	if err := runtime.Advance(4); err != nil {
		t.Fatalf("advance to 4: %v", err)
	}
	if got := host.HealthForTest(2); got != 60 {
		t.Fatalf("health at tick 4 = %d, want 60", got)
	}
}

// 失败启动之后立刻 checkpoint 必须能恢复（不留指向不存在 cast 的任务）。
func TestCheckpointAfterFailedStartRestores(t *testing.T) {
	program, environment := compileTerminalCharge(t)
	host := runtimeTestHost(environment)
	setTerminalMana(host, 10)
	runtime := NewRuntime(host, RuntimeOptions{})
	if _, err := runtime.Start(program, CastInput{Caster: 1, Target: 2}); !errors.Is(err, ErrInsufficientResource) {
		t.Fatalf("start = %v", err)
	}
	checkpoint, err := runtime.Checkpoint()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := RestoreRuntime(host, RuntimeOptions{}, checkpoint, ProgramResolverFunc(func(string, string) (*Program, error) { return program, nil })); err != nil {
		t.Fatalf("restore after a failed start: %v", err)
	}
}

// cancel 回调失败：Cancel 返回错误，但 cast 必须终止——施法者可以再次施法，再次 Cancel 被拒绝而不是重跑回调。
func TestCancelCallbackFailureStillEndsTheCast(t *testing.T) {
	window := `"cast_window":{"windup_ticks":5,"commit_tick":3,"recovery_ticks":1,"movement":"locked","turning":"allowed","interrupt_tags":[],"refund_before_commit":true}`
	json := `{"schema":"roost.skill/v2","id":"skill.test.terminal.cancel","name":"Terminal","description":"Cancel callback that cannot pay.","activation":{"type":"active","policy":{"mode":"tap"},` + window + `},"input_schema":{"type":"entity"},"cooldown_ticks":0,"costs":[],"memory":{},"initial_phase":"cast","phases":[{"id":"cast","timeout_ticks":0,"on":{"enter":{"flow":"finish"},"cancel":{"flow":"effect","effect":{"type":"resource","target":"$caster","resource":"mana","operation":"spend","amount":500}}}}]}`
	program, environment := compileRuntimeJSON(t, json)
	host := runtimeTestHost(environment)
	runtime := NewRuntime(host, RuntimeOptions{})
	castID, err := runtime.Activate(program, CastInput{Caster: 1, Target: 2})
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.Cancel(castID); !errors.Is(err, ErrInsufficientResource) {
		t.Fatalf("cancel = %v, want the callback's ErrInsufficientResource", err)
	}
	snapshot, _ := runtime.InspectCast(castID)
	if snapshot.Status != CastFailed {
		t.Fatalf("cast after failed cancel: status=%s stage=%s, want failed", snapshot.Status, snapshot.WindowStage)
	}
	if err := runtime.Cancel(castID); !errors.Is(err, ErrCastInputRejected) {
		t.Fatalf("second cancel = %v, want ErrCastInputRejected (the callback must not run again)", err)
	}
	if runtime.ActiveCastCount() != 0 {
		t.Fatalf("active casts = %d after the cast ended", runtime.ActiveCastCount())
	}
	if _, err := runtime.Activate(program, CastInput{Caster: 1, Target: 2}); err != nil {
		t.Fatalf("caster still locked after the failed cancel: %v", err)
	}
}

// 手动 Release 在重新进入施法窗口后失败（付费不足）：cast 必须终止，不能停在 preparing 永久占住施法者。
func TestChargeReleaseFailureStillEndsTheCast(t *testing.T) {
	policy := `{"mode":"charge","max_charge_ticks":10,"min_charge_bp":0,"auto_release":true}`
	program, environment := compilePolicySkill(t, "charge", policy, 0)
	host := runtimeTestHost(environment)
	runtime := NewRuntime(host, RuntimeOptions{})
	castID, err := runtime.Activate(program, CastInput{Caster: 1, Target: 2})
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.Advance(5); err != nil {
		t.Fatal(err)
	}
	setTerminalMana(host, 1)
	if err := runtime.Release(castID); !errors.Is(err, ErrInsufficientResource) {
		t.Fatalf("release = %v, want ErrInsufficientResource", err)
	}
	snapshot, _ := runtime.InspectCast(castID)
	if snapshot.Status != CastFailed {
		t.Fatalf("cast after failed release: status=%s stage=%s, want failed", snapshot.Status, snapshot.WindowStage)
	}
	setTerminalMana(host, 100)
	if _, err := runtime.Activate(program, CastInput{Caster: 1, Target: 2}); err != nil {
		t.Fatalf("caster still locked after the failed release: %v", err)
	}
}

// toggle 的 pulse 失败后 cast 终止并释放 policy 槽位：下一次激活开始一个新 cast，
// 不能变成对失败 cast 的 toggle-off（执行它的 release 效果、把 failed 改写成 finished、起冷却）。
func TestFailedToggleReleasesItsPolicySlot(t *testing.T) {
	policy := `{"mode":"toggle","pulse_interval_ticks":2,"max_duration_ticks":20,"sustain_costs":[]}`
	json := `{"schema":"roost.skill/v2","id":"skill.test.terminal.toggle","name":"Terminal","description":"Toggle whose pulse cannot pay.","activation":{"type":"active","policy":` + policy + `},"input_schema":{"type":"entity"},"cooldown_ticks":10,"costs":[],"memory":{"entered":{"type":"bool","default":false}},"initial_phase":"cast","phases":[{"id":"cast","timeout_ticks":0,"on":{"enter":{"flow":"effect","effect":{"type":"set_memory","name":"entered","value":true}},"pulse":{"flow":"effect","effect":{"type":"resource","target":"$caster","resource":"mana","operation":"spend","amount":500}},"release":{"flow":"sequence","steps":[{"flow":"effect","effect":{"type":"damage","target":"$input.target","amount":2,"damage_type":"physical"}},{"flow":"finish"}]}}}]}`
	program, environment := compileRuntimeJSON(t, json)
	host := runtimeTestHost(environment)
	runtime := NewRuntime(host, RuntimeOptions{})
	castID, err := runtime.Activate(program, CastInput{Caster: 1, Target: 2})
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.Advance(2); !errors.Is(err, ErrInsufficientResource) {
		t.Fatalf("pulse advance = %v, want ErrInsufficientResource", err)
	}
	next, err := runtime.Activate(program, CastInput{Caster: 1, Target: 2})
	if err != nil {
		t.Fatal(err)
	}
	failed, _ := runtime.InspectCast(castID)
	if next == castID || failed.Status != CastFailed || host.HealthForTest(2) != 100 || runtime.CooldownUntil(program, 1) != 0 {
		t.Fatalf("activation after a failed toggle: id=%d (failed=%d) failedStatus=%s health=%d cooldown=%d; want a new cast, the failed one untouched",
			next, castID, failed.Status, host.HealthForTest(2), runtime.CooldownUntil(program, 1))
	}
	if err := runtime.Cancel(castID); !errors.Is(err, ErrCastInputRejected) {
		t.Fatalf("cancel of a failed cast = %v, want ErrCastInputRejected", err)
	}
}

// 出错的终止只走 failCastLocked 一个终态入口，另外三条路径：
//
//   - Interrupt 停衍生物出错：原错误返回给调用方，cast 进 failed、保留第一次原因，再次 Interrupt / Cancel 被拒、
//     不重跑回调，施法者不被占住，宿主侧衍生物已停；
//   - toggle 的 release 回调出错（经 Release 与经再次激活的 toggle-off 两个入口）：同上，且 policy 槽位释放，
//     下一次激活开始新 cast；
//   - charge 的 enter 在提交前起了 owned 衍生物后失败：Start 返回 (0, 原错误)，宿主侧衍生物停止，Runtime 不在被复用的
//     cast ID 名下留下衍生物记录，失败启动之后的 checkpoint 能恢复；宿主停不下衍生物时 cast 保留（见 spawn_stop_test.go）。
//
// 历史：RR-20261006-21。

// summonWithFailingCancel 召出一个带 minion 衍生物的陷阱：取消时回调先给拥有者回 1 点血（用来数回调跑了几次），再向拥有者
// 扣 500 mana，测试宿主上必然余额不足。
const summonWithFailingCancel = `{"flow":"effect","effect":{"type":"summon","template":"deployable.trap","position":"$caster.position","count":1,"duration_ticks":10},"spawn":{"kind":"minion"},"on":{"cancel":{"flow":"sequence","steps":[{"flow":"effect","effect":{"type":"heal","target":"$owner","amount":1}},{"flow":"effect","effect":{"type":"resource","target":"$owner","resource":"mana","operation":"spend","amount":500}}]}}}`

// summonWithCountingCancel 的取消回调总能成功：给拥有者回 1 点血，用 owned_spawn_callback_cancel 事件计数。
const summonWithCountingCancel = `{"flow":"effect","effect":{"type":"summon","template":"deployable.trap","position":"$caster.position","count":1,"duration_ticks":10},"spawn":{"kind":"minion"},"on":{"cancel":{"flow":"effect","effect":{"type":"heal","target":"$owner","amount":1}}}}`

func runtimeEventCount(runtime *Runtime, kind string) int {
	count := 0
	for _, event := range runtime.RuntimeEvents() {
		if event.Kind == kind {
			count++
		}
	}
	return count
}

func activeHostSpawns(host *MemoryHost) int {
	host.mutex.Lock()
	defer host.mutex.Unlock()
	active := 0
	for _, spawn := range host.spawns {
		if spawn.active {
			active++
		}
	}
	return active
}

func runtimeSpawnsOfCast(runtime *Runtime, id CastID) int {
	count := 0
	for _, spawn := range allSpawnRecords(runtime) {
		if spawn.CastID == id {
			count++
		}
	}
	return count
}

// Interrupt 停衍生物时 entity 衍生物的 cancel 回调失败：错误原样返回，cast 进入失败终态，回调只跑一次，
// 宿主侧衍生物已停，施法者可以再次施法。
func TestInterruptSpawnStopFailureStillEndsTheCast(t *testing.T) {
	window := `"cast_window":{"windup_ticks":0,"commit_tick":0,"recovery_ticks":1,"movement":"locked","turning":"allowed","interrupt_tags":["spell"],"refund_before_commit":true}`
	enter := `{"flow":"sequence","steps":[` + summonWithFailingCancel + `,{"flow":"wait","ticks":5,"then":{"flow":"finish"}}]}`
	json := `{"schema":"roost.skill/v2","id":"skill.test.terminal.interrupt","name":"Terminal","description":"Interrupt whose spawn cancel callback cannot pay.","activation":{"type":"active","policy":{"mode":"tap"},` + window + `},"input_schema":{"type":"entity"},"cooldown_ticks":0,"costs":[],"memory":{},"initial_phase":"cast","phases":[{"id":"cast","timeout_ticks":0,"on":{"enter":` + enter + `}}]}`
	program, environment := compileRuntimeJSON(t, json)
	host := runtimeTestHost(environment)
	host.UpsertEntity(MemoryEntity{ID: 1, Alive: true, Health: 50, MaxHealth: 100, Resources: map[string]int64{"mana": 100}})
	runtime := NewRuntime(host, RuntimeOptions{})
	castID, err := runtime.Activate(program, CastInput{Caster: 1, Target: 2})
	if err != nil {
		t.Fatal(err)
	}
	if active := activeHostSpawns(host); active != 1 {
		t.Fatalf("host spawns before interrupt = %d, want the running minion spawn", active)
	}
	tag := program.cast.interruptTags[0]
	if err := runtime.Interrupt(castID, tag); !errors.Is(err, ErrInsufficientResource) {
		t.Fatalf("interrupt = %v, want the cancel callback's ErrInsufficientResource", err)
	}
	snapshot, _ := runtime.InspectCast(castID)
	if snapshot.Status != CastFailed || snapshot.Failure != ErrInsufficientResource.Error() {
		t.Fatalf("cast after failed interrupt: status=%s stage=%s failure=%q, want failed with the callback's error", snapshot.Status, snapshot.WindowStage, snapshot.Failure)
	}
	if callbacks := host.HealthForTest(1) - 50; callbacks != 1 {
		t.Fatalf("cancel callbacks = %d, want exactly one (the failure path must not run the callback again)", callbacks)
	}
	if active := activeHostSpawns(host); active != 0 {
		t.Fatalf("host spawns after failed interrupt = %d, want the minion spawn stopped", active)
	}
	if left := scheduledTasksFor(runtime, castID); left != 0 {
		t.Fatalf("failed interrupt left %d scheduled tasks for the cast", left)
	}
	if err := runtime.Interrupt(castID, tag); !errors.Is(err, ErrCastInputRejected) {
		t.Fatalf("second interrupt = %v, want ErrCastInputRejected", err)
	}
	if err := runtime.Cancel(castID); !errors.Is(err, ErrCastInputRejected) {
		t.Fatalf("cancel after failed interrupt = %v, want ErrCastInputRejected", err)
	}
	if runtime.ActiveCastCount() != 0 {
		t.Fatalf("active casts = %d after the cast failed", runtime.ActiveCastCount())
	}
	if err := runtime.Advance(6); err != nil {
		t.Fatalf("advance past the cancelled wait: %v", err)
	}
	if snapshot, _ := runtime.InspectCast(castID); snapshot.Status != CastFailed || snapshot.Failure != ErrInsufficientResource.Error() {
		t.Fatalf("cast after advance: status=%s failure=%q, want the first failure kept", snapshot.Status, snapshot.Failure)
	}
	host.UpsertEntity(MemoryEntity{ID: 1, Alive: true, Health: 100, MaxHealth: 100, Resources: map[string]int64{"mana": 1000}})
	if _, err := runtime.Activate(program, CastInput{Caster: 1, Target: 2}); err != nil {
		t.Fatalf("caster still locked after the failed interrupt: %v", err)
	}
}

// toggle 的 release 回调失败（经 Release 与经再次激活的 toggle-off）：错误原样返回，cast 进入失败终态，
// policy 槽位释放；再次 Release 被拒而不是重跑回调，下一次激活开始新 cast。
func TestToggleReleaseCallbackFailureStillEndsTheCast(t *testing.T) {
	policy := `{"mode":"toggle","pulse_interval_ticks":2,"max_duration_ticks":20,"sustain_costs":[]}`
	release := `{"flow":"sequence","steps":[{"flow":"effect","effect":{"type":"resource","target":"$caster","resource":"mana","operation":"spend","amount":500}},{"flow":"finish"}]}`
	json := `{"schema":"roost.skill/v2","id":"skill.test.terminal.togglerelease","name":"Terminal","description":"Toggle whose release cannot pay.","activation":{"type":"active","policy":` + policy + `},"input_schema":{"type":"entity"},"cooldown_ticks":0,"costs":[],"memory":{},"initial_phase":"cast","phases":[{"id":"cast","timeout_ticks":0,"on":{"enter":{"flow":"effect","effect":{"type":"heal","target":"$caster","amount":1}},"pulse":{"flow":"effect","effect":{"type":"damage","target":"$input.target","amount":1,"damage_type":"physical"}},"release":` + release + `}}]}`
	program, environment := compileRuntimeJSON(t, json)
	for name, releaseIt := range map[string]func(*Runtime, CastID) error{
		"release": func(runtime *Runtime, id CastID) error { return runtime.Release(id) },
		"toggle off": func(runtime *Runtime, id CastID) error {
			again, err := runtime.Activate(program, CastInput{Caster: 1, Target: 2})
			if again != id {
				t.Errorf("toggle-off activation returned cast %d, want the active cast %d", again, id)
			}
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			host := runtimeTestHost(environment)
			runtime := NewRuntime(host, RuntimeOptions{})
			castID, err := runtime.Activate(program, CastInput{Caster: 1, Target: 2})
			if err != nil {
				t.Fatal(err)
			}
			if err := runtime.Advance(1); err != nil {
				t.Fatal(err)
			}
			if err := releaseIt(runtime, castID); !errors.Is(err, ErrInsufficientResource) {
				t.Fatalf("release = %v, want the release callback's ErrInsufficientResource", err)
			}
			snapshot, _ := runtime.InspectCast(castID)
			if snapshot.Status != CastFailed || snapshot.Failure != ErrInsufficientResource.Error() {
				t.Fatalf("cast after failed release: status=%s stage=%s failure=%q, want failed with the callback's error", snapshot.Status, snapshot.WindowStage, snapshot.Failure)
			}
			if left := scheduledTasksFor(runtime, castID); left != 0 {
				t.Fatalf("failed release left %d scheduled tasks (pulses) for the cast", left)
			}
			if err := runtime.Release(castID); !errors.Is(err, ErrCastInputRejected) {
				t.Fatalf("second release = %v, want ErrCastInputRejected (the release callback must not run again)", err)
			}
			health := host.HealthForTest(2)
			if err := runtime.Advance(4); err != nil {
				t.Fatalf("advance past the next pulse: %v", err)
			}
			if got := host.HealthForTest(2); got != health {
				t.Fatalf("target health %d -> %d: the failed toggle kept pulsing", health, got)
			}
			if runtime.ActiveCastCount() != 0 {
				t.Fatalf("active casts = %d after the toggle failed", runtime.ActiveCastCount())
			}
			next, err := runtime.Activate(program, CastInput{Caster: 1, Target: 2})
			if err != nil || next == castID {
				t.Fatalf("activation after the failed toggle = %d, %v; want a new cast (policy slot released), failed one was %d", next, err, castID)
			}
			if failed, _ := runtime.InspectCast(castID); failed.Status != CastFailed {
				t.Fatalf("failed toggle rewritten to %s by the next activation", failed.Status)
			}
		})
	}
}

// charge 的 enter 在提交前起了 minion 衍生物，随后付费失败：Start 返回 (0, 原错误)，等于“没有施法”。
// 宿主侧衍生物已停（cancel 回调跑一次），Runtime 不在被复用的 cast ID 名下留下衍生物记录，失败启动之后
// checkpoint 能恢复，下一个拿到同一 ID 的 cast 只看到自己的衍生物。
//
// owned 实体本身不随失败启动删除：Runtime 不在事务里（维护者决定 B4），与 Cancel 停掉未移交的 entity 衍生物
// 一样，实体按召唤效果的寿命由宿主回收。
func TestFailedChargeStartWithOwnedSpawnLeavesNoResidue(t *testing.T) {
	enter := `{"flow":"sequence","steps":[` + summonWithCountingCancel + `,{"flow":"effect","effect":{"type":"resource","target":"$caster","resource":"mana","operation":"spend","amount":50}}]}`
	json := `{"schema":"roost.skill/v2","id":"skill.test.terminal.chargesummon","name":"Terminal","description":"Charge whose enter summons, then cannot pay.","activation":{"type":"active","policy":{"mode":"charge","max_charge_ticks":10,"min_charge_bp":0,"auto_release":false}},"input_schema":{"type":"entity"},"cooldown_ticks":0,"costs":[],"memory":{},"initial_phase":"cast","phases":[{"id":"cast","timeout_ticks":0,"on":{"enter":` + enter + `,"release":{"flow":"finish"}}}]}`
	program, environment := compileRuntimeJSON(t, json)
	host := runtimeTestHost(environment)
	setTerminalMana(host, 10)
	runtime := NewRuntime(host, RuntimeOptions{})
	if id, err := runtime.Start(program, CastInput{Caster: 1, Target: 2}); !errors.Is(err, ErrInsufficientResource) || id != 0 {
		t.Fatalf("first start = %d, %v; want 0, ErrInsufficientResource", id, err)
	}
	if active := activeHostSpawns(host); active != 0 {
		t.Errorf("host spawns after the failed start = %d, want the minion spawn stopped", active)
	}
	if callbacks := runtimeEventCount(runtime, "owned_spawn_callback_cancel"); callbacks != 1 {
		t.Errorf("cancel callbacks = %d, want exactly one", callbacks)
	}
	if owned := runtime.OwnedSpawns(1); len(owned) != 0 {
		t.Errorf("owned spawns = %#v, want none: the failed cast hands nothing off", owned)
	}
	if left := runtimeSpawnsOfCast(runtime, 1); left != 0 {
		t.Errorf("failed start left %d spawn records under cast 1, the id the next cast reuses", left)
	}
	checkpoint, err := runtime.Checkpoint()
	if err != nil {
		t.Fatalf("checkpoint after a failed start with a minion spawn: %v", err)
	}
	if _, err := RestoreRuntime(host, RuntimeOptions{}, checkpoint, ProgramResolverFunc(func(string, string) (*Program, error) { return program, nil })); err != nil {
		t.Fatalf("restore after a failed start with a minion spawn: %v", err)
	}
	setTerminalMana(host, 100)
	id, err := runtime.Start(program, CastInput{Caster: 1, Target: 2})
	if err != nil || id != 1 {
		t.Fatalf("second start = %d, %v; want the reused id 1", id, err)
	}
	if spawns := runtimeSpawnsOfCast(runtime, id); spawns != 1 {
		t.Fatalf("cast %d owns %d spawn records, want only its own minion spawn", id, spawns)
	}
	if _, err := runtime.Checkpoint(); err != nil {
		t.Fatalf("checkpoint with the reused id: %v", err)
	}
}

// 同一个失败启动，但宿主停不下 minion 衍生物：衍生物仍在宿主侧运行、Runtime 里还有它的运行中记录。这时不能把 cast
// 删掉、把 ID 还回去——记录会挂到下一个 cast 名下（下一个 cast 停衍生物时会连它一起停、checkpoint 引用不存在的
// cast）。失败的 cast 保留为 failed 并返回它的 ID，衍生物记录仍归它，下一次启动拿新 ID，checkpoint 能恢复。
func TestFailedChargeStartWhoseSpawnCannotStopKeepsItsCast(t *testing.T) {
	enter := `{"flow":"sequence","steps":[` + summonWithCountingCancel + `,{"flow":"effect","effect":{"type":"resource","target":"$caster","resource":"mana","operation":"spend","amount":50}}]}`
	json := `{"schema":"roost.skill/v2","id":"skill.test.terminal.chargesummonstuck","name":"Terminal","description":"Charge whose enter summons a spawn the host cannot stop, then cannot pay.","activation":{"type":"active","policy":{"mode":"charge","max_charge_ticks":10,"min_charge_bp":0,"auto_release":false}},"input_schema":{"type":"entity"},"cooldown_ticks":0,"costs":[],"memory":{},"initial_phase":"cast","phases":[{"id":"cast","timeout_ticks":0,"on":{"enter":` + enter + `,"release":{"flow":"finish"}}}]}`
	program, environment := compileRuntimeJSON(t, json)
	base := runtimeTestHost(environment)
	setTerminalMana(base, 10)
	host := &ownedSpawnTestHost{MemoryHost: base, failNextStop: true}
	runtime := NewRuntime(host, RuntimeOptions{})
	id, err := runtime.Start(program, CastInput{Caster: 1, Target: 2})
	if !errors.Is(err, ErrInsufficientResource) {
		t.Fatalf("first start = %d, %v; want the enter's ErrInsufficientResource", id, err)
	}
	if active := activeHostSpawns(base); active != 1 {
		t.Fatalf("host spawns = %d, want the minion spawn the host refused to stop", active)
	}
	if id != 1 {
		t.Errorf("failed start returned cast %d; with a spawn still running it must keep (and report) cast 1", id)
	}
	if snapshot, found := runtime.InspectCast(1); !found || snapshot.Status != CastFailed {
		t.Errorf("cast 1 after the failed start: found=%v status=%s, want it kept as failed", found, snapshot.Status)
	}
	setTerminalMana(base, 100)
	next, err := runtime.Start(program, CastInput{Caster: 1, Target: 2})
	if err != nil || next != 2 {
		t.Fatalf("second start = %d, %v; want a fresh id 2, cast 1 still owns a running spawn", next, err)
	}
	if spawns := runtimeSpawnsOfCast(runtime, next); spawns != 1 {
		t.Errorf("cast %d owns %d spawn records, want only its own minion spawn", next, spawns)
	}
	checkpoint, err := runtime.Checkpoint()
	if err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	if _, err := RestoreRuntime(host, RuntimeOptions{}, checkpoint, ProgramResolverFunc(func(string, string) (*Program, error) { return program, nil })); err != nil {
		t.Fatalf("restore with the failed cast's stuck spawn: %v", err)
	}
}
