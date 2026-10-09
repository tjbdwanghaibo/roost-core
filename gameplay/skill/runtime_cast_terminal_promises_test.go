package skill

// RR-20261005-NC-110 / NC-111 / NC-112：施法的每条终止路径（启动失败、Cancel / Interrupt / Release 中途出错、
// 排程任务失败）都必须把 cast 收敛到同一个终态：本 cast 的排程任务与帧全部撤掉、衍生物停止、policy 槽位释放、
// 不再占施法者窗口。旧实现各条路径手写收尾、各漏一步：启动失败删掉 cast 并复用 ID 却留下排程任务（NC-110）；
// Cancel 回调失败时任务已撤、phase token 已推进，cast 停在 preparing 永久占住施法者（NC-111）；
// 排程失败的 toggle 不释放 policy 槽位，下次激活变成对失败 cast 的 toggle-off（NC-112）。

import (
	"errors"
	"testing"
)

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

// NC-110：启动失败、未提交的 cast 被删除且 ID 复用，它已排程的等待不能落到下一个拿到同一 ID 的 cast 上。
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

// NC-110：失败启动之后立刻 checkpoint，必须能恢复（旧实现留下指向不存在 cast 的任务，恢复判为 corrupt）。
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

// NC-111：cancel 回调失败，Cancel 返回错误，但 cast 必须终止：施法者可以再次施法，再次 Cancel 被拒绝而不是重跑回调。
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

// NC-111：手动 Release 在重新进入施法窗口后失败（付费不足），cast 必须终止，不能停在 preparing 永久占住施法者。
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

// NC-112：toggle 的 pulse 失败后 cast 终止并释放 policy 槽位：下一次激活开始一个新 cast，
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
