package skill

// RR-20261006-21 后续（维护者 2026-10-06：“如果是技能本身的问题是不是技能自己处理比较好”）：施放失败走
// failCastLocked 时，Runtime 让宿主停掉这次施放启动的进程；宿主 StopProcess 失败时，之前 Runtime 只把 cast
// 留作 failed、把错误还给调用方，此后不再处理——宿主不自己清理，召唤物 / 区域 / 飞行物就一直留在场景里。
//
// 承诺：Runtime 对自己启动、应该停掉却没停掉的进程负责到底。
//   - 停止失败的进程记录标成 stop_pending（复用 RR-21 / RR-23 的记录生命周期：记录钉住 cast，cast ID 不复用）；
//   - 之后的 tick 按退避重试 StopProcess，不每个 tick 打宿主；成功后记录按 RR-23 的规则随 cast 回收；
//   - 重试有次数上限，到上限发 skill.process.stop_retry_exhausted.total 与一条日志，记录保留；
//   - 待停止条目数受 MaxStopPendingProcesses 约束；
//   - 待停止状态进 checkpoint，恢复后继续重试。

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/metrics"
)

var errHostStopUnavailable = errors.New("test host: stop unavailable")

// stopRetryHost 让前 failStops 次 StopProcess 失败（负数表示一直失败），并记下每次 StopProcess 时宿主的 tick。
// 成功的停止交给 MemoryHost，它对重复停止幂等（TestMemoryHostStopProcessIsIdempotent）。
type stopRetryHost struct {
	*MemoryHost
	failStops int
	tick      Tick
	stopTicks []Tick
}

func (host *stopRetryHost) Advance(tick Tick) (WorldRevision, error) {
	host.tick = tick
	return host.MemoryHost.Advance(tick)
}

func (host *stopRetryHost) StopProcess(command ProcessStopCommand, state ProcessHostState) (CommitReceipt, error) {
	host.stopTicks = append(host.stopTicks, host.tick)
	if host.failStops != 0 {
		if host.failStops > 0 {
			host.failStops--
		}
		return CommitReceipt{}, errHostStopUnavailable
	}
	return host.MemoryHost.StopProcess(command, state)
}

// chargeSummonThatCannotPay 的 enter 先召出一个寿命 100 tick 的陷阱（summon 进程），随后付 50 mana；测试把
// mana 设成 10，提交前失败，走 failCastLocked 停进程。陷阱寿命要长：lifecycle 实体到期后，修前的
// reapUnhandedEntityProcesses 会顺手再停一次，掩盖“没有重试”。
func chargeSummonThatCannotPay(t *testing.T) (*Program, CompileEnvironment) {
	t.Helper()
	summon := strings.Replace(summonWithCountingCancel, `"duration_ticks":10`, `"duration_ticks":100`, 1)
	enter := `{"flow":"sequence","steps":[` + summon + `,{"flow":"effect","effect":{"type":"resource","target":"$caster","resource":"mana","operation":"spend","amount":50}}]}`
	json := `{"schema":"roost.skill/v2","id":"skill.test.stopretry.charge","name":"StopRetry","description":"Charge whose enter summons, then cannot pay.","activation":{"type":"active","policy":{"mode":"charge","max_charge_ticks":10,"min_charge_bp":0,"auto_release":false}},"input_schema":{"type":"entity"},"cooldown_ticks":0,"costs":[],"memory":{},"initial_phase":"cast","phases":[{"id":"cast","timeout_ticks":0,"on":{"enter":` + enter + `,"release":{"flow":"finish"}}}]}`
	return compileRuntimeJSON(t, json)
}

func stopRetryTap(t *testing.T) *Program {
	t.Helper()
	program, _ := compileRuntimeJSON(t, asyncSkillJSON("stopretry.tap", `{"type":"entity"}`, `{"flow":"finish"}`))
	return program
}

func onlyProcessOfCast(t *testing.T, runtime *Runtime, id CastID) *ProcessInstance {
	t.Helper()
	var found *ProcessInstance
	for _, process := range runtime.processes {
		if process.CastID != id {
			continue
		}
		if found != nil {
			t.Fatalf("cast %d owns more than one process record", id)
		}
		found = process
	}
	if found == nil {
		t.Fatalf("cast %d owns no process record", id)
	}
	return found
}

func stopRetryResolver(programs ...*Program) ProgramResolver {
	return ProgramResolverFunc(func(id, _ string) (*Program, error) {
		for _, program := range programs {
			if program.id == id {
				return program, nil
			}
		}
		return nil, ErrCheckpointProgram
	})
}

func advanceEachTick(t *testing.T, runtime *Runtime, through Tick) {
	t.Helper()
	for runtime.currentTick < through {
		if err := runtime.Advance(runtime.currentTick + 1); err != nil {
			t.Fatalf("advance to %d: %v", runtime.currentTick+1, err)
		}
	}
}

func counterValue(name string) int64 {
	var total int64
	for _, metric := range metrics.Snapshot() {
		if metric.Name == name {
			total += metric.Value
		}
	}
	return total
}

func equalTicks(left, right []Tick) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

// 宿主第一次停失败、第二次成功：Runtime 在退避之后重试并停掉进程，之前不打宿主；停掉之后记录随 cast 按
// RR-23 的规则回收，cast ID 不复用。修前进程一直留在宿主侧。
func TestFailedStartRetriesTheStopItCouldNotFinish(t *testing.T) {
	program, environment := chargeSummonThatCannotPay(t)
	tap := stopRetryTap(t)
	base := runtimeTestHost(environment)
	setTerminalMana(base, 10)
	host := &stopRetryHost{MemoryHost: base, failStops: 1}
	runtime := NewRuntime(host, RuntimeOptions{CompletedCastLimit: 1})
	id, err := runtime.Start(program, CastInput{Caster: 1, Target: 2})
	if !errors.Is(err, ErrInsufficientResource) || id != 1 {
		t.Fatalf("failed start = %d, %v; want cast 1 kept with ErrInsufficientResource (RR-21)", id, err)
	}
	if active := activeHostProcesses(base); active != 1 {
		t.Fatalf("host processes after the refused stop = %d, want 1", active)
	}
	if status := onlyProcessOfCast(t, runtime, 1).Status; status != "stop_pending" {
		t.Errorf("process status after the refused stop = %q, want stop_pending", status)
	}

	advanceEachTick(t, runtime, 20)
	if active := activeHostProcesses(base); active != 0 {
		t.Fatalf("host still runs the summon at tick %d; StopProcess called at ticks %v, the runtime never retried", runtime.currentTick, host.stopTicks)
	}
	// 默认退避 4 tick：tick 0 失败，tick 4 重试成功；1～3 不打宿主。
	if want := []Tick{0, 4}; !equalTicks(host.stopTicks, want) {
		t.Errorf("StopProcess called at ticks %v, want %v", host.stopTicks, want)
	}
	if status := onlyProcessOfCast(t, runtime, 1).Status; status != ProcessCancelled {
		t.Errorf("process status after the retry = %q, want cancelled", status)
	}
	if callbacks := runtimeEventCount(runtime, "owned_process_callback_cancel"); callbacks != 1 {
		t.Errorf("cancel callbacks = %d, want exactly one: a retry only re-issues the host stop", callbacks)
	}

	// 记录与 cast 按 RR-23 回收：再完成一个施法，完成队列超过上限 1，已不再被引用的 cast 1 连同记录一起回收。
	setTerminalMana(base, 100)
	next, err := runtime.Start(tap, CastInput{Caster: 1, Target: 2})
	if err != nil || next != 2 {
		t.Fatalf("next start = %d, %v; want a fresh id 2 (the failed cast's id is never reused)", next, err)
	}
	if _, found := runtime.InspectCast(1); found {
		t.Errorf("cast 1 still retained after its process stopped and the completed queue exceeded its limit")
	}
	if left := runtimeProcessesOfCast(runtime, 1); left != 0 {
		t.Errorf("%d process records of the reclaimed cast 1 stay behind", left)
	}
	checkpoint, err := runtime.Checkpoint()
	if err != nil {
		t.Fatalf("checkpoint after the retried stop: %v", err)
	}
	if _, err := RestoreRuntime(host, RuntimeOptions{}, checkpoint, stopRetryResolver(program, tap)); err != nil {
		t.Fatalf("restore after the retried stop: %v", err)
	}
}

// checkpoint 发生在待停止期间：恢复出来的 Runtime 带着待停止状态，按原来的重试时刻继续重试。
func TestStopPendingSurvivesCheckpointAndKeepsRetrying(t *testing.T) {
	program, environment := chargeSummonThatCannotPay(t)
	base := runtimeTestHost(environment)
	setTerminalMana(base, 10)
	host := &stopRetryHost{MemoryHost: base, failStops: 1}
	runtime := NewRuntime(host, RuntimeOptions{})
	if id, err := runtime.Start(program, CastInput{Caster: 1, Target: 2}); id != 1 || !errors.Is(err, ErrInsufficientResource) {
		t.Fatalf("failed start = %d, %v", id, err)
	}
	advanceEachTick(t, runtime, 2)
	checkpoint, err := runtime.Checkpoint()
	if err != nil {
		t.Fatalf("checkpoint while the stop is pending: %v", err)
	}
	restored, err := RestoreRuntime(host, RuntimeOptions{}, checkpoint, stopRetryResolver(program))
	if err != nil {
		t.Fatalf("restore while the stop is pending: %v", err)
	}
	if status := onlyProcessOfCast(t, restored, 1).Status; status != "stop_pending" {
		t.Errorf("restored process status = %q, want stop_pending", status)
	}
	advanceEachTick(t, restored, 3)
	if len(host.stopTicks) != 1 {
		t.Errorf("restored runtime called StopProcess at ticks %v before the retry was due (tick 4)", host.stopTicks)
	}
	advanceEachTick(t, restored, 20)
	if active := activeHostProcesses(base); active != 0 {
		t.Fatalf("restored runtime never retried the stop: host still runs the summon, StopProcess at ticks %v", host.stopTicks)
	}
	if want := []Tick{0, 4}; !equalTicks(host.stopTicks, want) {
		t.Errorf("StopProcess called at ticks %v, want %v", host.stopTicks, want)
	}
}

// 待停止的 cast 被钉住，可以多于 CompletedCastLimit 个（这里两个 failed cast、上限 1）。checkpoint 恢复要接受这种
// 状态，之后照常重试并回收到上限以内。
func TestPinnedCastsBeyondTheCompletedLimitStillRestore(t *testing.T) {
	program, environment := chargeSummonThatCannotPay(t)
	base := runtimeTestHost(environment)
	setTerminalMana(base, 10)
	host := &stopRetryHost{MemoryHost: base, failStops: 2}
	runtime := NewRuntime(host, RuntimeOptions{CompletedCastLimit: 1})
	for want := CastID(1); want <= 2; want++ {
		if id, err := runtime.Start(program, CastInput{Caster: 1, Target: 2}); id != want || !errors.Is(err, ErrInsufficientResource) {
			t.Fatalf("failed start = %d, %v; want cast %d kept", id, err, want)
		}
	}
	checkpoint, err := runtime.Checkpoint()
	if err != nil {
		t.Fatalf("checkpoint with two pinned casts: %v", err)
	}
	restored, err := RestoreRuntime(host, RuntimeOptions{}, checkpoint, stopRetryResolver(program))
	if err != nil {
		t.Fatalf("restore with two pinned failed casts and CompletedCastLimit 1: %v", err)
	}
	advanceEachTick(t, restored, 20)
	if active := activeHostProcesses(base); active != 0 {
		t.Fatalf("host processes after the retries = %d, want 0 (StopProcess at ticks %v)", active, host.stopTicks)
	}
	if stats := restored.RetentionStats(); stats.Casts > 1 || stats.CompletedCasts > 1 {
		t.Errorf("retained casts = %d (completed queue %d) after both stops succeeded, want at most CompletedCastLimit 1", stats.Casts, stats.CompletedCasts)
	}
}

// captureDefaultLog 把 slog 默认 logger 换成写入缓冲区的 logger，用例结束时还原。
func captureDefaultLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buffer bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buffer, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return &buffer
}

// 宿主一直停不下：重试按退避进行、到 ProcessStopRetryLimit 告警（指标 + 一条日志）后不再打宿主，记录保留；
// 待停止条目数受 MaxStopPendingProcesses 约束，超限时丢最早的已告警条目并告警。
func TestStopRetriesAreBoundedAndAlertWhenExhausted(t *testing.T) {
	program, environment := chargeSummonThatCannotPay(t)
	base := runtimeTestHost(environment)
	setTerminalMana(base, 10)
	host := &stopRetryHost{MemoryHost: base, failStops: -1}
	logs := captureDefaultLog(t)
	exhaustedBefore, droppedBefore := counterValue(MetricProcessStopRetryExhausted), counterValue(MetricProcessStopPendingDropped)
	options := RuntimeOptions{ProcessStopRetryBackoff: 2, ProcessStopRetryLimit: 3, MaxStopPendingProcesses: 2, CompletedCastLimit: 1}
	runtime := NewRuntime(host, options)
	if id, err := runtime.Start(program, CastInput{Caster: 1, Target: 2}); id != 1 || !errors.Is(err, ErrInsufficientResource) {
		t.Fatalf("failed start = %d, %v", id, err)
	}

	advanceEachTick(t, runtime, 60)
	// tick 0 第一次停止失败；重试在 2、2+4=6、6+8=14，第三次失败到上限，之后不再打宿主。
	if want := []Tick{0, 2, 6, 14}; !equalTicks(host.stopTicks, want) {
		t.Errorf("StopProcess called at ticks %v, want %v", host.stopTicks, want)
	}
	if got := counterValue(MetricProcessStopRetryExhausted) - exhaustedBefore; got != 1 {
		t.Errorf("%s grew by %d, want 1", MetricProcessStopRetryExhausted, got)
	}
	if text := logs.String(); strings.Count(text, "process stop retries exhausted") != 1 || !strings.Contains(text, "process_id=1") {
		t.Errorf("exhaustion log = %q, want exactly one warning naming process 1", text)
	}
	process := onlyProcessOfCast(t, runtime, 1)
	if process.Status != ProcessStopPending || !process.stopRetryExhausted {
		t.Errorf("exhausted process = status %q exhausted %v, want the stop_pending record kept", process.Status, process.stopRetryExhausted)
	}
	if _, found := runtime.InspectCast(1); !found {
		t.Errorf("cast 1 reclaimed while its process still runs on the host")
	}
	if stats := runtime.RetentionStats(); stats.StopPendingProcesses != 1 || stats.StopRetryExhaustedProcesses != 1 {
		t.Errorf("retention stats = %+v, want one stop-pending process, exhausted", stats)
	}
	if active := activeHostProcesses(base); active != 1 {
		t.Errorf("host processes = %d, want the summon the host keeps refusing to stop", active)
	}
	checkpoint, err := runtime.Checkpoint()
	if err != nil {
		t.Fatalf("checkpoint with an exhausted stop: %v", err)
	}
	if _, err := RestoreRuntime(host, RuntimeOptions{}, checkpoint, stopRetryResolver(program)); err != nil {
		t.Fatalf("restore with an exhausted stop: %v", err)
	}

	// 内存有界：再失败两次启动，待停止条目最多 2 条；超限时丢最早的已告警条目（进程 1）。
	for want := CastID(2); want <= 3; want++ {
		if id, err := runtime.Start(program, CastInput{Caster: 1, Target: 2}); id != want || !errors.Is(err, ErrInsufficientResource) {
			t.Fatalf("failed start = %d, %v; want cast %d kept", id, err, want)
		}
	}
	if stats := runtime.RetentionStats(); stats.StopPendingProcesses != 2 {
		t.Errorf("stop-pending processes = %d, want MaxStopPendingProcesses 2", stats.StopPendingProcesses)
	}
	if got := counterValue(MetricProcessStopPendingDropped) - droppedBefore; got != 1 {
		t.Errorf("%s grew by %d, want 1", MetricProcessStopPendingDropped, got)
	}
	if left := runtimeProcessesOfCast(runtime, 1); left != 0 {
		t.Errorf("the dropped record of cast 1 is still there (%d records)", left)
	}
	if !strings.Contains(logs.String(), "stop-pending process dropped") {
		t.Errorf("no log for the dropped stop-pending record: %q", logs.String())
	}
	if stats := runtime.RetentionStats(); stats.Casts > options.CompletedCastLimit+options.MaxStopPendingProcesses {
		t.Errorf("retained casts = %d, want at most CompletedCastLimit + MaxStopPendingProcesses", stats.Casts)
	}
}

// visualAreaThatCannotPay 起一个带视觉的 area 进程，随后付费失败；用来核对待停止期间客户端看到的状态与表现。
func visualAreaThatCannotPay(t *testing.T) (*Program, CompileEnvironment) {
	t.Helper()
	area := `{"flow":"effect","effect":{"type":"spawn","template":"deployable.trap","position":"$caster.position","count":1,"duration_ticks":100},"process":{"kind":"area","duration_ticks":100,"interval_ticks":1,"visual":{"category":"area","theme":"default","elements":["default"]},"area":{"from":"$caster","kind":"entity","shape":{"type":"circle","radius":10},"filters":[{"type":"targetable"}],"order":{"by":"stable_id","direction":"asc"},"limit":2}}}`
	enter := `{"flow":"sequence","steps":[` + area + `,{"flow":"effect","effect":{"type":"resource","target":"$caster","resource":"mana","operation":"spend","amount":50}}]}`
	json := `{"schema":"roost.skill/v2","id":"skill.test.stopretry.visual","name":"StopRetry","description":"Charge whose enter starts a visual area, then cannot pay.","presentation":{"icon_keywords":["flare","blade","spark"]},"activation":{"type":"active","policy":{"mode":"charge","max_charge_ticks":10,"min_charge_bp":0,"auto_release":false}},"input_schema":{"type":"entity"},"cooldown_ticks":0,"costs":[],"memory":{},"initial_phase":"cast","phases":[{"id":"cast","timeout_ticks":0,"on":{"enter":` + enter + `,"release":{"flow":"finish"}}}]}`
	return compileRuntimeJSON(t, json)
}

// 客户端（skillsync 转发 StateDeltas 与 PresentationEvents）看到的：state mutation 依次是 process_upsert（stop_pending）、
// process_upsert（cancelled）、cast 回收时的 process_remove；表现在进入待停止时多一条带 stop_pending 的 process_update，
// 停掉时 process_stop；待停止期间 PresentationSnapshot 的 reset 仍带着这个进程（宿主侧还在），停掉后不再带。
func TestStopPendingIsVisibleToSyncConsistently(t *testing.T) {
	program, environment := visualAreaThatCannotPay(t)
	tap := stopRetryTap(t)
	base := runtimeTestHost(environment)
	setTerminalMana(base, 10)
	host := &stopRetryHost{MemoryHost: base, failStops: 1}
	runtime := NewRuntime(host, RuntimeOptions{CompletedCastLimit: 1})
	if id, err := runtime.Start(program, CastInput{Caster: 1, Target: 2}); id != 1 || !errors.Is(err, ErrInsufficientResource) {
		t.Fatalf("failed start = %d, %v", id, err)
	}
	processID := onlyProcessOfCast(t, runtime, 1).ID
	resetHas := func() (ProcessStatus, bool) {
		for _, entry := range runtime.PresentationSnapshot().Active {
			if entry.Kind == ActivePresentationProcess && entry.ProcessID == processID {
				return entry.ProcessStatus, true
			}
		}
		return "", false
	}
	if status, found := resetHas(); !found || status != ProcessStopPending {
		t.Errorf("presentation reset during stop_pending: found=%v status=%q, want the process kept as stop_pending", found, status)
	}
	advanceEachTick(t, runtime, 10)
	if _, found := resetHas(); found {
		t.Errorf("presentation reset still carries the process after the host stopped it")
	}
	setTerminalMana(base, 100)
	if _, err := runtime.Start(tap, CastInput{Caster: 1, Target: 2}); err != nil {
		t.Fatal(err)
	}

	var mutations []string
	for _, mutation := range runtime.StateDeltas(0, 0).Mutations {
		if mutation.ProcessID != processID {
			continue
		}
		entry := string(mutation.Kind)
		if mutation.Process != nil {
			entry += ":" + string(mutation.Process.Status)
		}
		mutations = append(mutations, entry)
	}
	if want := "process_upsert:stop_pending process_upsert:cancelled process_remove"; strings.Join(mutations, " ") != want {
		t.Errorf("state mutations for the process = %v, want %s", mutations, want)
	}
	var presentation []string
	for _, event := range runtime.PresentationEvents(0) {
		if event.HasProcess && event.ProcessID == processID && event.Kind != PresentationProcessSignal {
			presentation = append(presentation, string(event.Kind)+":"+string(event.ProcessStatus))
		}
	}
	if want := "process_start:running process_update:stop_pending process_stop:cancelled"; strings.Join(presentation, " ") != want {
		t.Errorf("presentation for the process = %v, want %s", presentation, want)
	}
}

// RR-20261006-31：移交后的 summon 到期，宿主拒绝 StopProcess。之前错误返回 Advance、进程仍到期，下一次 Advance
// 先重做它、再失败、再返回，Runtime 的 tick 停在原地（这个 Runtime 上所有施法一起冻住），每次 Advance 都打一次宿主。
// 承诺：错误照常返回这一次，进程转为待停止、按退避重试；Runtime 继续前进，end 回调只跑一次。
func TestHandedOffProcessStopFailureDoesNotFreezeTheRuntime(t *testing.T) {
	summon := strings.Replace(summonWithCountingCancel, `"duration_ticks":10`, `"duration_ticks":4`, 1)
	summon = strings.Replace(summon, `"on":{"cancel"`, `"on":{"end"`, 1)
	json := `{"schema":"roost.skill/v2","id":"skill.test.stopretry.handoff","name":"Summon","description":"Summons, then finishes.","activation":{"type":"active","policy":{"mode":"tap"}},"input_schema":{"type":"entity"},"cooldown_ticks":0,"costs":[],"memory":{},"initial_phase":"cast","phases":[{"id":"cast","timeout_ticks":0,"on":{"enter":{"flow":"sequence","steps":[` + summon + `,{"flow":"finish"}]}}}]}`
	program, environment := compileRuntimeJSON(t, json)
	base := runtimeTestHost(environment)
	host := &stopRetryHost{MemoryHost: base, failStops: 2}
	runtime := NewRuntime(host, RuntimeOptions{})
	if _, err := runtime.Start(program, CastInput{Caster: 1, Target: 2}); err != nil {
		t.Fatal(err)
	}
	var failures []Tick
	for tick := Tick(1); tick <= 20; tick++ {
		if err := runtime.Advance(tick); err != nil {
			if !errors.Is(err, errHostStopUnavailable) {
				t.Fatalf("Advance(%d) = %v", tick, err)
			}
			failures = append(failures, tick)
		}
	}
	if runtime.currentTick != 20 {
		t.Fatalf("runtime tick = %d after Advance(20): a process the host cannot stop froze the runtime (StopProcess at host ticks %v)", runtime.currentTick, host.stopTicks)
	}
	if want := []Tick{4}; !equalTicks(failures, want) {
		t.Errorf("Advance failed at ticks %v, want only %v (the tick the stop was first refused)", failures, want)
	}
	// tick 4 到期停止失败；重试在 8（失败）、8+8=16（成功）。
	if want := []Tick{4, 8, 16}; !equalTicks(host.stopTicks, want) {
		t.Errorf("StopProcess called at host ticks %v, want %v", host.stopTicks, want)
	}
	if active := activeHostProcesses(base); active != 0 {
		t.Errorf("host processes = %d, want the summon stopped by the retry", active)
	}
	if callbacks := runtimeEventCount(runtime, "owned_process_callback_end"); callbacks != 1 {
		t.Errorf("end callbacks = %d, want exactly one", callbacks)
	}
	if owned := runtime.OwnedProcesses(1); len(owned) != 0 {
		t.Errorf("owned processes = %+v, want none after the stop", owned)
	}
}
