package skill

// RR-20261006-21 后续（维护者 2026-10-06：“如果是技能本身的问题是不是技能自己处理比较好”）：施放失败走
// failCastLocked 时，Runtime 让宿主停掉这次施放启动的衍生物；宿主 StopSpawn 失败时，之前 Runtime 只把 cast
// 留作 failed、把错误还给调用方，此后不再处理——宿主不自己清理，召唤物 / 区域 / 飞行物就一直留在场景里。
//
// 承诺：Runtime 对自己启动、应该停掉却没停掉的衍生物负责到底。
//   - 停止失败的衍生物记录标成 stop_pending（复用 RR-21 / RR-23 的记录生命周期：记录钉住 cast，cast ID 不复用）；
//   - 之后的 tick 按退避重试 StopSpawn，不每个 tick 打宿主；成功后记录按 RR-23 的规则随 cast 回收；
//   - 重试有次数上限，到上限发 skill.spawn.stop_retry_exhausted.total 与一条日志，记录保留；
//   - 待停止条目数受 MaxStopPendingSpawns 约束（超限的最早条目挪进已放弃分区，维护者第十三轮“待停止上限”选 B）；
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

// stopRetryHost 让前 failStops 次 StopSpawn 失败（负数表示一直失败），并记下每次 StopSpawn 时宿主的 tick。
// 成功的停止交给 MemoryHost，它对重复停止幂等（TestMemoryHostStopSpawnIsIdempotent）。
type stopRetryHost struct {
	*MemoryHost
	failStops int
	tick      Tick
	stopTicks []Tick
	stopIDs   []SpawnID
}

func (host *stopRetryHost) Advance(tick Tick) (WorldRevision, error) {
	host.tick = tick
	return host.MemoryHost.Advance(tick)
}

func (host *stopRetryHost) StopSpawn(command SpawnStopCommand, state SpawnHostState) (CommitReceipt, error) {
	host.stopTicks = append(host.stopTicks, host.tick)
	host.stopIDs = append(host.stopIDs, command.Meta.SpawnID)
	if host.failStops != 0 {
		if host.failStops > 0 {
			host.failStops--
		}
		return CommitReceipt{}, errHostStopUnavailable
	}
	return host.MemoryHost.StopSpawn(command, state)
}

// chargeSummonThatCannotPay 的 enter 先召出一个寿命 100 tick 的陷阱（带 minion 衍生物），随后付 50 mana；测试把
// mana 设成 10，提交前失败，走 failCastLocked 停衍生物。陷阱寿命要长：lifecycle 实体到期后，修前的
// reapUnhandedEntitySpawns 会顺手再停一次，掩盖“没有重试”。
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

func onlySpawnOfCast(t *testing.T, runtime *Runtime, id CastID) *SpawnInstance {
	t.Helper()
	var found *SpawnInstance
	for _, spawn := range allSpawnRecords(runtime) {
		if spawn.CastID != id {
			continue
		}
		if found != nil {
			t.Fatalf("cast %d owns more than one spawn record", id)
		}
		found = spawn
	}
	if found == nil {
		t.Fatalf("cast %d owns no spawn record", id)
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

// 宿主第一次停失败、第二次成功：Runtime 在退避之后重试并停掉衍生物，之前不打宿主；停掉之后记录随 cast 按
// RR-23 的规则回收，cast ID 不复用。修前衍生物一直留在宿主侧。
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
	if active := activeHostSpawns(base); active != 1 {
		t.Fatalf("host spawns after the refused stop = %d, want 1", active)
	}
	if status := onlySpawnOfCast(t, runtime, 1).Status; status != "stop_pending" {
		t.Errorf("spawn status after the refused stop = %q, want stop_pending", status)
	}

	advanceEachTick(t, runtime, 20)
	if active := activeHostSpawns(base); active != 0 {
		t.Fatalf("host still runs the minion spawn at tick %d; StopSpawn called at ticks %v, the runtime never retried", runtime.currentTick, host.stopTicks)
	}
	// 默认退避 4 tick：tick 0 失败，tick 4 重试成功；1～3 不打宿主。
	if want := []Tick{0, 4}; !equalTicks(host.stopTicks, want) {
		t.Errorf("StopSpawn called at ticks %v, want %v", host.stopTicks, want)
	}
	if status := onlySpawnOfCast(t, runtime, 1).Status; status != SpawnCancelled {
		t.Errorf("spawn status after the retry = %q, want cancelled", status)
	}
	if callbacks := runtimeEventCount(runtime, "owned_spawn_callback_cancel"); callbacks != 1 {
		t.Errorf("cancel callbacks = %d, want exactly one: a retry only re-issues the host stop", callbacks)
	}

	// 记录与 cast 按 RR-23 回收：再完成一个施法，完成队列超过上限 1，已不再被引用的 cast 1 连同记录一起回收。
	setTerminalMana(base, 100)
	next, err := runtime.Start(tap, CastInput{Caster: 1, Target: 2})
	if err != nil || next != 2 {
		t.Fatalf("next start = %d, %v; want a fresh id 2 (the failed cast's id is never reused)", next, err)
	}
	if _, found := runtime.InspectCast(1); found {
		t.Errorf("cast 1 still retained after its spawn stopped and the completed queue exceeded its limit")
	}
	if left := runtimeSpawnsOfCast(runtime, 1); left != 0 {
		t.Errorf("%d spawn records of the reclaimed cast 1 stay behind", left)
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
	if status := onlySpawnOfCast(t, restored, 1).Status; status != "stop_pending" {
		t.Errorf("restored spawn status = %q, want stop_pending", status)
	}
	advanceEachTick(t, restored, 3)
	if len(host.stopTicks) != 1 {
		t.Errorf("restored runtime called StopSpawn at ticks %v before the retry was due (tick 4)", host.stopTicks)
	}
	advanceEachTick(t, restored, 20)
	if active := activeHostSpawns(base); active != 0 {
		t.Fatalf("restored runtime never retried the stop: host still runs the minion spawn, StopSpawn at ticks %v", host.stopTicks)
	}
	if want := []Tick{0, 4}; !equalTicks(host.stopTicks, want) {
		t.Errorf("StopSpawn called at ticks %v, want %v", host.stopTicks, want)
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
	if active := activeHostSpawns(base); active != 0 {
		t.Fatalf("host spawns after the retries = %d, want 0 (StopSpawn at ticks %v)", active, host.stopTicks)
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

// 宿主一直停不下：重试按退避进行、到 SpawnStopRetryLimit 告警（指标 + 一条日志）后不再打宿主，记录保留；
// 待停止条目数受 MaxStopPendingSpawns 约束，超限时最早的已告警条目挪进已放弃分区并告警。
// 2026-10-07 按维护者第十三轮“待停止上限”选 B 改断言：之前要求超限的记录被删掉（skill.spawn.stop_pending_dropped.total、
// cast 1 名下不剩记录、日志 “stop-pending spawn dropped”）；现在记录留下、status abandoned，计 skill.spawn.abandoned.total，
// 日志 “stop-pending spawn abandoned”。retained casts 的上界不变（已放弃的记录不钉住 cast）。
func TestStopRetriesAreBoundedAndAlertWhenExhausted(t *testing.T) {
	program, environment := chargeSummonThatCannotPay(t)
	base := runtimeTestHost(environment)
	setTerminalMana(base, 10)
	host := &stopRetryHost{MemoryHost: base, failStops: -1}
	logs := captureDefaultLog(t)
	exhaustedBefore, abandonedBefore := counterValue(MetricSpawnStopRetryExhausted), counterValue(MetricSpawnAbandoned)
	options := RuntimeOptions{SpawnStopRetryBackoff: 2, SpawnStopRetryLimit: 3, MaxStopPendingSpawns: 2, CompletedCastLimit: 1}
	runtime := NewRuntime(host, options)
	if id, err := runtime.Start(program, CastInput{Caster: 1, Target: 2}); id != 1 || !errors.Is(err, ErrInsufficientResource) {
		t.Fatalf("failed start = %d, %v", id, err)
	}

	advanceEachTick(t, runtime, 60)
	// tick 0 第一次停止失败；重试在 2、2+4=6、6+8=14，第三次失败到上限，之后不再打宿主。
	if want := []Tick{0, 2, 6, 14}; !equalTicks(host.stopTicks, want) {
		t.Errorf("StopSpawn called at ticks %v, want %v", host.stopTicks, want)
	}
	if got := counterValue(MetricSpawnStopRetryExhausted) - exhaustedBefore; got != 1 {
		t.Errorf("%s grew by %d, want 1", MetricSpawnStopRetryExhausted, got)
	}
	if text := logs.String(); strings.Count(text, "spawn stop retries exhausted") != 1 || !strings.Contains(text, "spawn_id=1") {
		t.Errorf("exhaustion log = %q, want exactly one warning naming spawn 1", text)
	}
	spawn := onlySpawnOfCast(t, runtime, 1)
	if spawn.Status != SpawnStopPending || !spawn.stopRetryExhausted {
		t.Errorf("exhausted spawn = status %q exhausted %v, want the stop_pending record kept", spawn.Status, spawn.stopRetryExhausted)
	}
	if _, found := runtime.InspectCast(1); !found {
		t.Errorf("cast 1 reclaimed while its spawn still runs on the host")
	}
	if stats := runtime.RetentionStats(); stats.StopPendingSpawns != 1 || stats.StopRetryExhaustedSpawns != 1 {
		t.Errorf("retention stats = %+v, want one stop-pending spawn, exhausted", stats)
	}
	if active := activeHostSpawns(base); active != 1 {
		t.Errorf("host spawns = %d, want the minion spawn the host keeps refusing to stop", active)
	}
	checkpoint, err := runtime.Checkpoint()
	if err != nil {
		t.Fatalf("checkpoint with an exhausted stop: %v", err)
	}
	if _, err := RestoreRuntime(host, RuntimeOptions{}, checkpoint, stopRetryResolver(program)); err != nil {
		t.Fatalf("restore with an exhausted stop: %v", err)
	}

	// 内存有界：再失败两次启动，待停止条目最多 2 条；超限时最早的已告警条目（衍生物 1）挪进已放弃分区。
	for want := CastID(2); want <= 3; want++ {
		if id, err := runtime.Start(program, CastInput{Caster: 1, Target: 2}); id != want || !errors.Is(err, ErrInsufficientResource) {
			t.Fatalf("failed start = %d, %v; want cast %d kept", id, err, want)
		}
	}
	if stats := runtime.RetentionStats(); stats.StopPendingSpawns != 2 {
		t.Errorf("stop-pending spawns = %d, want MaxStopPendingSpawns 2", stats.StopPendingSpawns)
	}
	if got := counterValue(MetricSpawnAbandoned) - abandonedBefore; got != 1 {
		t.Errorf("%s grew by %d, want 1", MetricSpawnAbandoned, got)
	}
	if spawn := onlySpawnOfCast(t, runtime, 1); spawn.Status != SpawnAbandoned {
		t.Errorf("the record of cast 1 past the limit has status %q, want abandoned (kept)", spawn.Status)
	}
	if !strings.Contains(logs.String(), "stop-pending spawn abandoned") {
		t.Errorf("no log for the abandoned stop-pending record: %q", logs.String())
	}
	if stats := runtime.RetentionStats(); stats.Casts > options.CompletedCastLimit+options.MaxStopPendingSpawns {
		t.Errorf("retained casts = %d, want at most CompletedCastLimit + MaxStopPendingSpawns", stats.Casts)
	}
}

// visualAreaThatCannotPay 起一个带视觉的 area 衍生物，随后付费失败；用来核对待停止期间客户端看到的状态与表现。
func visualAreaThatCannotPay(t *testing.T) (*Program, CompileEnvironment) {
	t.Helper()
	area := `{"flow":"effect","effect":{"type":"summon","template":"deployable.trap","position":"$caster.position","count":1,"duration_ticks":100},"spawn":{"kind":"area","duration_ticks":100,"interval_ticks":1,"visual":{"category":"area","theme":"default","elements":["default"]},"area":{"from":"$caster","kind":"entity","shape":{"type":"circle","radius":10},"filters":[{"type":"targetable"}],"order":{"by":"stable_id","direction":"asc"},"limit":2}}}`
	enter := `{"flow":"sequence","steps":[` + area + `,{"flow":"effect","effect":{"type":"resource","target":"$caster","resource":"mana","operation":"spend","amount":50}}]}`
	json := `{"schema":"roost.skill/v2","id":"skill.test.stopretry.visual","name":"StopRetry","description":"Charge whose enter starts a visual area, then cannot pay.","presentation":{"icon_keywords":["flare","blade","spark"]},"activation":{"type":"active","policy":{"mode":"charge","max_charge_ticks":10,"min_charge_bp":0,"auto_release":false}},"input_schema":{"type":"entity"},"cooldown_ticks":0,"costs":[],"memory":{},"initial_phase":"cast","phases":[{"id":"cast","timeout_ticks":0,"on":{"enter":` + enter + `,"release":{"flow":"finish"}}}]}`
	return compileRuntimeJSON(t, json)
}

// 客户端（skillsync 转发 StateDeltas 与 PresentationEvents）看到的：state mutation 依次是 spawn_upsert（stop_pending）、
// spawn_upsert（cancelled）、cast 回收时的 spawn_remove；表现在进入待停止时多一条带 stop_pending 的 spawn_update，
// 停掉时 spawn_stop；待停止期间 PresentationSnapshot 的 reset 仍带着这个衍生物（宿主侧还在），停掉后不再带。
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
	spawnID := onlySpawnOfCast(t, runtime, 1).ID
	resetHas := func() (SpawnStatus, bool) {
		for _, entry := range runtime.PresentationSnapshot().Active {
			if entry.Kind == ActivePresentationSpawn && entry.SpawnID == spawnID {
				return entry.SpawnStatus, true
			}
		}
		return "", false
	}
	if status, found := resetHas(); !found || status != SpawnStopPending {
		t.Errorf("presentation reset during stop_pending: found=%v status=%q, want the spawn kept as stop_pending", found, status)
	}
	advanceEachTick(t, runtime, 10)
	if _, found := resetHas(); found {
		t.Errorf("presentation reset still carries the spawn after the host stopped it")
	}
	setTerminalMana(base, 100)
	if _, err := runtime.Start(tap, CastInput{Caster: 1, Target: 2}); err != nil {
		t.Fatal(err)
	}

	var mutations []string
	for _, mutation := range runtime.StateDeltas(0, 0).Mutations {
		if mutation.SpawnID != spawnID {
			continue
		}
		entry := string(mutation.Kind)
		if mutation.Spawn != nil {
			entry += ":" + string(mutation.Spawn.Status)
		}
		mutations = append(mutations, entry)
	}
	if want := "spawn_upsert:stop_pending spawn_upsert:cancelled spawn_remove"; strings.Join(mutations, " ") != want {
		t.Errorf("state mutations for the spawn = %v, want %s", mutations, want)
	}
	var presentation []string
	for _, event := range runtime.PresentationEvents(0) {
		if event.HasSpawn && event.SpawnID == spawnID && event.Kind != PresentationSpawnSignal {
			presentation = append(presentation, string(event.Kind)+":"+string(event.SpawnStatus))
		}
	}
	if want := "spawn_start:running spawn_update:stop_pending spawn_stop:cancelled"; strings.Join(presentation, " ") != want {
		t.Errorf("presentation for the spawn = %v, want %s", presentation, want)
	}
}

// RR-20261006-31：移交后的 minion 衍生物到期，宿主拒绝 StopSpawn。之前错误返回 Advance、衍生物仍到期，下一次 Advance
// 先重做它、再失败、再返回，Runtime 的 tick 停在原地（这个 Runtime 上所有施法一起冻住），每次 Advance 都打一次宿主。
// 承诺：错误照常返回这一次，衍生物转为待停止、按退避重试；Runtime 继续前进，end 回调只跑一次。
func TestHandedOffSpawnStopFailureDoesNotFreezeTheRuntime(t *testing.T) {
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
		t.Fatalf("runtime tick = %d after Advance(20): a spawn the host cannot stop froze the runtime (StopSpawn at host ticks %v)", runtime.currentTick, host.stopTicks)
	}
	if want := []Tick{4}; !equalTicks(failures, want) {
		t.Errorf("Advance failed at ticks %v, want only %v (the tick the stop was first refused)", failures, want)
	}
	// tick 4 到期停止失败；重试在 8（失败）、8+8=16（成功）。
	if want := []Tick{4, 8, 16}; !equalTicks(host.stopTicks, want) {
		t.Errorf("StopSpawn called at host ticks %v, want %v", host.stopTicks, want)
	}
	if active := activeHostSpawns(base); active != 0 {
		t.Errorf("host spawns = %d, want the minion spawn stopped by the retry", active)
	}
	if callbacks := runtimeEventCount(runtime, "owned_spawn_callback_end"); callbacks != 1 {
		t.Errorf("end callbacks = %d, want exactly one", callbacks)
	}
	if owned := runtime.OwnedSpawns(1); len(owned) != 0 {
		t.Errorf("owned spawns = %+v, want none after the stop", owned)
	}
}
