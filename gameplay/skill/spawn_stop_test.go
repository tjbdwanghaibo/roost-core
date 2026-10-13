package skill

import (
	"bytes"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
)

// 停止入口统一（维护者 2026-10-07）。
//
// 衍生物的停止入口：施法失败（failCastLocked）与施法里的 goto / Cancel / Interrupt / 收尾、衍生物启动失败的清理、
// 移交时 lifecycle 实体已失效、tick 驱动的到期 / 失效回收、RemoveProgram、Shutdown。
//
// 承诺：任何入口遇到宿主拒绝，衍生物都进入同一个待停止状态——记录 stop_pending、不在 OwnedSpawns 里、
// StateSnapshot 可见，同一次请求里只打一次宿主；之后 Runtime 在 SpawnStopRetryBackoff 个 tick 后重试并停掉它，
// 回调只跑一次。
//
// 守卫：spawnStopEntries 枚举全部停止入口。TestSpawnStopEntriesAreRegistered 读包源码，列出调用
// requestSpawnStop 的函数，必须与表里登记的 callers 一致；terminateSpawn / stopSpawn 只能由 requestSpawnStop
// 调用，宿主 StopSpawn 只能由 stopSpawn 调用。新增停止入口必须在表里登记一行并给出触发场景。

// stopEntryHost 在 stopRetryHost 之上可让宿主的实体清理（RemoveOwnedEntitiesForMatchEnd / ByProgram）一起失败：
// 宿主故障时停不下衍生物，通常也删不掉实体，lifecycle 实体留在世界里，Runtime 不会因为 lifecycle 消失而“顺手”再停一次。
type stopEntryHost struct {
	*stopRetryHost
	failRemovals bool
	failCommit   bool
}

var errHostRemovalUnavailable = errors.New("test host: entity removal unavailable")
var errHostCommitUnavailable = errors.New("test host: owned summon commit unavailable")

func (host *stopEntryHost) RemoveOwnedEntitiesForMatchEnd() error {
	if host.failRemovals {
		return errHostRemovalUnavailable
	}
	return host.MemoryHost.RemoveOwnedEntitiesForMatchEnd()
}

func (host *stopEntryHost) RemoveOwnedEntitiesByProgram(programID string) error {
	if host.failRemovals {
		return errHostRemovalUnavailable
	}
	return host.MemoryHost.RemoveOwnedEntitiesByProgram(programID)
}

func (host *stopEntryHost) CommitOwnedSummon(transactionID OwnedSummonTransactionID) error {
	if host.failCommit {
		host.failCommit = false
		return errHostCommitUnavailable
	}
	return host.MemoryHost.CommitOwnedSummon(transactionID)
}

// stopEntryRun 是一个入口被触发之后的现场：被拒绝停止的衍生物、入口返回的错误。
type stopEntryRun struct {
	runtime *Runtime
	host    *stopEntryHost
	spawnID SpawnID
	err     error
}

type spawnStopEntry struct {
	name string
	// callers 是这个入口里直接调用 requestSpawnStop 的函数名（守卫用它核对源码）。
	callers []string
	// wantErr 是入口返回给调用方的错误（errors.Is）；拒绝本身照常报告这一次。
	wantErr []error
	// callback 是该入口停止时应跑的回调事件，wantCallbacks 是它在整个过程里应跑的次数。
	callback      string
	wantCallbacks int
	// trigger 布好场景、让宿主拒绝下一次 StopSpawn（failStops = 1）后触发入口。
	trigger func(t *testing.T) stopEntryRun
}

// 寿命 100 tick 的 minion 衍生物，cancel 回调计数；寿命要长，避免到期回收在观察窗口里替入口再停一次。
var longSummonWithCountingCancel = strings.Replace(summonWithCountingCancel, `"duration_ticks":10`, `"duration_ticks":100`, 1)

func newStopEntryHost(environment CompileEnvironment) *stopEntryHost {
	return &stopEntryHost{stopRetryHost: &stopRetryHost{MemoryHost: runtimeTestHost(environment)}}
}

// handedOffSummon 起一个 tap 施法：召出带长寿命 minion 衍生物的陷阱后立即 finish，衍生物移交给 Runtime 推进。
func handedOffSummon(t *testing.T, id string) (*Runtime, *stopEntryHost, *Program, SpawnID) {
	t.Helper()
	program, environment := compileRuntimeJSON(t, asyncSkillJSON(id, `{"type":"entity"}`, `{"flow":"sequence","steps":[`+longSummonWithCountingCancel+`,{"flow":"finish"}]}`))
	host := newStopEntryHost(environment)
	runtime := NewRuntime(host, RuntimeOptions{})
	castID, err := runtime.Start(program, CastInput{Caster: 1, Target: 2})
	if err != nil {
		t.Fatal(err)
	}
	spawn := onlySpawnOfCast(t, runtime, castID)
	if !spawn.handedOff {
		t.Fatalf("minion spawn not handed off after the cast finished")
	}
	return runtime, host, program, spawn.ID
}

// waitingSummon 起一个 tap 施法：召出带长寿命 minion 衍生物的陷阱后等 50 tick 再 finish，衍生物在施法期间未移交。
func waitingSummon(t *testing.T, id, window string) (*Runtime, *stopEntryHost, *Program, CastID, SpawnID) {
	t.Helper()
	activation := `{"type":"active","policy":{"mode":"tap"}` + window + `}`
	json := `{"schema":"roost.skill/v2","id":"skill.test.stopentry.` + id + `","name":"StopEntry","description":"Summons, then waits.","activation":` + activation + `,"input_schema":{"type":"entity"},"cooldown_ticks":0,"costs":[],"memory":{},"initial_phase":"cast","phases":[{"id":"cast","timeout_ticks":0,"on":{"enter":{"flow":"sequence","steps":[` + longSummonWithCountingCancel + `,{"flow":"wait","ticks":50,"then":{"flow":"finish"}}]}}}]}`
	program, environment := compileRuntimeJSON(t, json)
	host := newStopEntryHost(environment)
	runtime := NewRuntime(host, RuntimeOptions{})
	castID, err := runtime.Start(program, CastInput{Caster: 1, Target: 2})
	if err != nil {
		t.Fatal(err)
	}
	return runtime, host, program, castID, onlySpawnOfCast(t, runtime, castID).ID
}

func dismissLifecycle(t *testing.T, host *stopEntryHost, program *Program, spawn *SpawnInstance) {
	t.Helper()
	if _, err := host.MemoryHost.Apply(EffectCommand{Payload: OwnedEntityCommand{Owner: 1, GameplayDigest: program.identity.gameplayDigest, Target: spawn.LifecycleEntity, Command: "dismiss"}}); err != nil {
		t.Fatal(err)
	}
}

// spawnStopEntries 是全部停止入口的登记表。新增调用 requestSpawnStop 的函数必须在这里登记一行。
var spawnStopEntries = []spawnStopEntry{
	{
		name:    "cast failure (failCastLocked)",
		callers: []string{"stopCastSpawns"},
		wantErr: []error{ErrInsufficientResource}, callback: "owned_spawn_callback_cancel", wantCallbacks: 1,
		trigger: func(t *testing.T) stopEntryRun {
			program, environment := chargeSummonThatCannotPay(t)
			host := newStopEntryHost(environment)
			setTerminalMana(host.MemoryHost, 10)
			host.failStops = 1
			runtime := NewRuntime(host, RuntimeOptions{})
			id, err := runtime.Start(program, CastInput{Caster: 1, Target: 2})
			return stopEntryRun{runtime: runtime, host: host, spawnID: onlySpawnOfCast(t, runtime, id).ID, err: err}
		},
	},
	{
		name:    "interrupt",
		callers: []string{"stopCastSpawns"},
		wantErr: []error{errHostStopUnavailable}, callback: "owned_spawn_callback_cancel", wantCallbacks: 1,
		trigger: func(t *testing.T) stopEntryRun {
			window := `,"cast_window":{"windup_ticks":0,"commit_tick":0,"recovery_ticks":1,"movement":"locked","turning":"allowed","interrupt_tags":["spell"],"refund_before_commit":true}`
			runtime, host, program, castID, spawnID := waitingSummon(t, "interrupt", window)
			host.failStops = 1
			err := runtime.Interrupt(castID, program.cast.interruptTags[0])
			return stopEntryRun{runtime: runtime, host: host, spawnID: spawnID, err: err}
		},
	},
	{
		// minion 衍生物的 enter 回调付不起 500 mana：startEntitySpawn 停掉刚起的衍生物（StopCauseFailure，不跑回调）。
		name:    "failed spawn start",
		callers: []string{"startEntitySpawn"},
		wantErr: []error{ErrInsufficientResource, errHostStopUnavailable}, callback: "owned_spawn_callback_cancel", wantCallbacks: 0,
		trigger: func(t *testing.T) stopEntryRun {
			summon := strings.Replace(longSummonWithCountingCancel, `"on":{"cancel":`, `"on":{"enter":{"flow":"effect","effect":{"type":"resource","target":"$owner","resource":"mana","operation":"spend","amount":500}},"cancel":`, 1)
			program, environment := compileRuntimeJSON(t, asyncSkillJSON("stopentry.startfail", `{"type":"entity"}`, `{"flow":"sequence","steps":[`+summon+`,{"flow":"finish"}]}`))
			host := newStopEntryHost(environment)
			host.failStops = 1
			runtime := NewRuntime(host, RuntimeOptions{})
			id, err := runtime.Start(program, CastInput{Caster: 1, Target: 2})
			if id == 0 {
				t.Fatalf("start = (0, %v): the cast whose minion spawn the host refused to stop must be kept (RR-21)", err)
			}
			return stopEntryRun{runtime: runtime, host: host, spawnID: onlySpawnOfCast(t, runtime, id).ID, err: err}
		},
	},
	{
		// 宿主提交 owned 实体事务失败：executeOwnedSummon 停掉本次已起的 minion 衍生物（StopCauseFailure，不跑回调）。
		name:    "failed owned summon commit",
		callers: []string{"executeOwnedSummon"},
		wantErr: []error{errHostCommitUnavailable, errHostStopUnavailable}, callback: "owned_spawn_callback_cancel", wantCallbacks: 0,
		trigger: func(t *testing.T) stopEntryRun {
			program, environment := compileRuntimeJSON(t, asyncSkillJSON("stopentry.commitfail", `{"type":"entity"}`, `{"flow":"sequence","steps":[`+longSummonWithCountingCancel+`,{"flow":"finish"}]}`))
			host := newStopEntryHost(environment)
			host.failStops, host.failCommit = 1, true
			runtime := NewRuntime(host, RuntimeOptions{})
			id, err := runtime.Start(program, CastInput{Caster: 1, Target: 2})
			if id == 0 {
				t.Fatalf("start = (0, %v): the cast whose minion spawn the host refused to stop must be kept (RR-21)", err)
			}
			return stopEntryRun{runtime: runtime, host: host, spawnID: onlySpawnOfCast(t, runtime, id).ID, err: err}
		},
	},
	{
		// 施法收尾移交 entity 衍生物时 lifecycle 实体已不在：handoffEntitySpawns 停掉它（跑 cancel 回调）。
		// 正常流程里 reapUnhandedEntitySpawns 先一步发现，这里直接调用移交，与 TestOwnedSpawnCancellationDetachesBeforeCallback 同法。
		name:    "handoff with a vanished lifecycle entity",
		callers: []string{"handoffEntitySpawns"},
		wantErr: []error{errHostStopUnavailable}, callback: "owned_spawn_callback_cancel", wantCallbacks: 1,
		trigger: func(t *testing.T) stopEntryRun {
			runtime, host, program, castID, spawnID := waitingSummon(t, "handoff", "")
			dismissLifecycle(t, host, program, runtime.spawns.get(spawnID))
			host.failStops = 1
			runtime.mutex.Lock()
			runtime.beginStateMutationLocked()
			err := runtime.handoffEntitySpawns(runtime.casts[castID])
			runtime.commitStateMutationsLocked()
			runtime.mutex.Unlock()
			return stopEntryRun{runtime: runtime, host: host, spawnID: spawnID, err: err}
		},
	},
	{
		// 施法期间 lifecycle 实体消失：下一次 Advance 的 reapUnhandedEntitySpawns 停掉它（跑 cancel 回调）。
		name:    "lifecycle entity vanished before handoff",
		callers: []string{"reapUnhandedEntitySpawns"},
		wantErr: []error{errHostStopUnavailable}, callback: "owned_spawn_callback_cancel", wantCallbacks: 1,
		trigger: func(t *testing.T) stopEntryRun {
			runtime, host, program, _, spawnID := waitingSummon(t, "reap", "")
			dismissLifecycle(t, host, program, runtime.spawns.get(spawnID))
			host.failStops = 1
			err := runtime.Advance(1)
			return stopEntryRun{runtime: runtime, host: host, spawnID: spawnID, err: err}
		},
	},
	{
		// 移交后的 minion 衍生物到期：tick 驱动的回收（terminateOwnedSpawn，跑 end 回调；RR-20261006-31）。
		name:    "handed-off spawn expiry",
		callers: []string{"terminateOwnedSpawn"},
		wantErr: []error{errHostStopUnavailable}, callback: "owned_spawn_callback_end", wantCallbacks: 1,
		trigger: func(t *testing.T) stopEntryRun {
			summon := strings.Replace(summonWithCountingCancel, `"duration_ticks":10`, `"duration_ticks":4`, 1)
			summon = strings.Replace(summon, `"on":{"cancel"`, `"on":{"end"`, 1)
			program, environment := compileRuntimeJSON(t, asyncSkillJSON("stopentry.expiry", `{"type":"entity"}`, `{"flow":"sequence","steps":[`+summon+`,{"flow":"finish"}]}`))
			host := newStopEntryHost(environment)
			runtime := NewRuntime(host, RuntimeOptions{})
			castID, err := runtime.Start(program, CastInput{Caster: 1, Target: 2})
			if err != nil {
				t.Fatal(err)
			}
			spawnID := onlySpawnOfCast(t, runtime, castID).ID
			advanceEachTick(t, runtime, 3)
			host.failStops = 1
			err = runtime.Advance(4)
			return stopEntryRun{runtime: runtime, host: host, spawnID: spawnID, err: err}
		},
	},
	{
		// 宿主故障：停不下 minion 衍生物，也删不掉程序的实体。
		name:    "RemoveProgram",
		callers: []string{"RemoveProgram"},
		wantErr: []error{errHostStopUnavailable}, callback: "owned_spawn_callback_cancel", wantCallbacks: 1,
		trigger: func(t *testing.T) stopEntryRun {
			runtime, host, program, spawnID := handedOffSummon(t, "stopentry.removeprogram")
			host.failStops, host.failRemovals = 1, true
			err := runtime.RemoveProgram(program.id)
			return stopEntryRun{runtime: runtime, host: host, spawnID: spawnID, err: err}
		},
	},
	{
		// 宿主故障：停不下 minion 衍生物，比赛结束的实体清理也失败。
		name:    "Shutdown",
		callers: []string{"Shutdown"},
		wantErr: []error{errHostStopUnavailable}, callback: "owned_spawn_callback_cancel", wantCallbacks: 1,
		trigger: func(t *testing.T) stopEntryRun {
			runtime, host, _, spawnID := handedOffSummon(t, "stopentry.shutdown")
			host.failStops, host.failRemovals = 1, true
			err := runtime.Shutdown()
			return stopEntryRun{runtime: runtime, host: host, spawnID: spawnID, err: err}
		},
	},
}

// 每个停止入口在宿主拒绝 StopSpawn 后都进入同一个待停止状态，并由 Runtime 在退避之后重试停掉。
func TestEveryStopEntryDefersARefusedStopTheSameWay(t *testing.T) {
	for _, entry := range spawnStopEntries {
		t.Run(entry.name, func(t *testing.T) {
			run := entry.trigger(t)
			runtime, host := run.runtime, run.host
			for _, want := range entry.wantErr {
				if !errors.Is(run.err, want) {
					t.Errorf("entry returned %v, want it to report %v", run.err, want)
				}
			}
			spawn := runtime.spawns.get(run.spawnID)
			if spawn == nil {
				t.Fatalf("spawn %d record gone after the refused stop", run.spawnID)
			}
			if spawn.Status != SpawnStopPending {
				t.Errorf("spawn status after the refused stop = %q, want stop_pending", spawn.Status)
			}
			if len(host.stopTicks) != 1 {
				t.Errorf("StopSpawn called at host ticks %v within the request, want exactly one refused call", host.stopTicks)
			}
			for _, owned := range runtime.OwnedSpawns(0) {
				if owned.ID == run.spawnID {
					t.Errorf("spawn %d still listed by OwnedSpawns (status %q): the runtime would keep stepping it", owned.ID, owned.Status)
				}
			}
			visible := SpawnStatus("")
			for _, view := range runtime.StateSnapshot().Spawns {
				if view.ID == run.spawnID {
					visible = view.Status
				}
			}
			if visible != SpawnStopPending {
				t.Errorf("StateSnapshot shows spawn status %q, want stop_pending", visible)
			}
			if len(host.stopTicks) == 0 {
				t.FailNow()
			}

			refused := host.stopTicks[0]
			advanceEachTick(t, runtime, refused+20)
			// 默认退避 4 tick：拒绝之后第 4 个 tick 重试一次并成功，期间不打宿主。
			if want := []Tick{refused, refused + 4}; !equalTicks(host.stopTicks, want) {
				t.Errorf("StopSpawn called at host ticks %v, want %v (the runtime retries after the backoff)", host.stopTicks, want)
			}
			if spawn := runtime.spawns.get(run.spawnID); spawn != nil && spawn.liveOnHost() {
				t.Errorf("spawn status at tick %d = %q, want stopped by the retry", runtime.currentTick, spawn.Status)
			}
			host.mutex.Lock()
			active := host.spawns[run.spawnID].active
			host.mutex.Unlock()
			if active {
				t.Errorf("host still runs spawn %d at tick %d", run.spawnID, runtime.currentTick)
			}
			if got := runtimeEventCount(runtime, entry.callback); got != entry.wantCallbacks {
				t.Errorf("%s ran %d times, want %d (a refused stop must not run the callback again)", entry.callback, got, entry.wantCallbacks)
			}
		})
	}
}

// 守卫：包里调用 requestSpawnStop 的函数必须都登记在 spawnStopEntries 里（retrySpawnStopsLocked 是状态机自己的
// 重试，不算入口）；停止的底层步骤只能经 requestSpawnStop 进入。
func TestSpawnStopEntriesAreRegistered(t *testing.T) {
	calls := spawnStopCallGraph(t)
	registered := map[string]bool{}
	for _, entry := range spawnStopEntries {
		for _, caller := range entry.callers {
			registered[caller] = true
		}
	}
	entries := map[string]bool{}
	for _, caller := range calls["requestSpawnStop"] {
		if caller != "retrySpawnStopsLocked" {
			entries[caller] = true
		}
	}
	if len(entries) == 0 {
		t.Fatalf("no function calls requestSpawnStop: the stop entries do not share the unified stop")
	}
	for caller := range entries {
		if !registered[caller] {
			t.Errorf("%s calls requestSpawnStop but is not registered in spawnStopEntries; add a row that triggers it with a refused StopSpawn", caller)
		}
	}
	for caller := range registered {
		if !entries[caller] {
			t.Errorf("spawnStopEntries registers %s, which no longer calls requestSpawnStop", caller)
		}
	}
	// Runtime 业务停止仍只有唯一入口。能力自检拥有独立探针，必须清理自己创建的 Host 资源。
	for callee, allowed := range map[string]map[string]bool{"terminateSpawn": {"requestSpawnStop": true}, "stopSpawn": {"terminateSpawn": true}, "StopSpawn": {"stopSpawn": true, "cleanupSpawnProbe": true}} {
		for _, caller := range calls[callee] {
			if !allowed[caller] {
				t.Errorf("%s calls %s directly; allowed owners: %v", caller, callee, allowed)
			}
		}
	}
}

// spawnStopCallGraph 返回包内（非测试文件）每个被调名对应的调用方函数名。只看 x.name(...) 形式的调用；
// 宿主实现自己的 StopSpawn 方法（MemoryHost、RecordingHost 转发）不算调用方。
func spawnStopCallGraph(t *testing.T) map[string][]string {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	watched := map[string]bool{"requestSpawnStop": true, "terminateSpawn": true, "stopSpawn": true, "StopSpawn": true}
	calls := map[string][]string{}
	fileSet := token.NewFileSet()
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
			if !ok || function.Body == nil || function.Name.Name == "StopSpawn" {
				continue
			}
			seen := map[string]bool{}
			ast.Inspect(function.Body, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				if selector, ok := call.Fun.(*ast.SelectorExpr); ok && watched[selector.Sel.Name] && !seen[selector.Sel.Name] {
					seen[selector.Sel.Name] = true
					calls[selector.Sel.Name] = append(calls[selector.Sel.Name], function.Name.Name)
				}
				return true
			})
		}
	}
	for _, callers := range calls {
		sort.Strings(callers)
	}
	return calls
}

// Runtime 对自己启动、应该停掉却没停掉的衍生物负责到底（施放失败走 failCastLocked、宿主 StopSpawn 失败时）：
//   - 停止失败的衍生物记录标成 stop_pending（记录钉住 cast，cast ID 不复用）；
//   - 之后的 tick 按退避重试 StopSpawn，不每个 tick 打宿主；成功后记录随 cast 按完成上限回收；
//   - 重试有次数上限，到上限发 skill.spawn.stop_retry_exhausted.total 与一条日志，记录保留；
//   - 待停止条目数受 MaxStopPendingSpawns 约束（超限的最早条目挪进已放弃分区，见 spawn_retention_test.go）；
//   - 待停止状态进 checkpoint，恢复后继续重试。

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
// mana 设成 10，提交前失败，走 failCastLocked 停衍生物。陷阱寿命要长：lifecycle 实体到期后的回收会顺手再停一次，
// 掩盖“没有重试”。
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

// 宿主第一次停失败、第二次成功：Runtime 在退避之后重试并停掉衍生物，之前不打宿主；停掉之后记录随 cast 按完成上限
// 回收，cast ID 不复用。
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

// 移交后的 minion 衍生物到期、宿主拒绝 StopSpawn：错误照常返回这一次，衍生物转为待停止、按退避重试；Runtime 继续前进
// （不停在同一个 tick 上反复重做、冻住这个 Runtime 上所有施法），end 回调只跑一次。
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

// Shutdown / RemoveProgram 先取一份衍生物 ID 列表，再逐个请求停止。前面一个被宿主拒绝、转入待停止、达到
// MaxStopPendingSpawns 时，最早的待停止记录挪进已放弃分区——它可能正是列表里后面那一个：循环取回的是已放弃的记录、
// 停止请求是空操作，不 panic；入口照常返回宿主的错误，其余衍生物照常转入待停止并在之后的 tick 重试停掉。

func TestStopSweepSkipsSpawnsDroppedAtTheStopPendingLimit(t *testing.T) {
	for _, entry := range []struct {
		name string
		stop func(runtime *Runtime, program *Program) error
	}{
		{name: "Shutdown", stop: func(runtime *Runtime, _ *Program) error { return runtime.Shutdown() }},
		{name: "RemoveProgram", stop: func(runtime *Runtime, program *Program) error { return runtime.RemoveProgram(program.id) }},
	} {
		t.Run(entry.name, func(t *testing.T) {
			program, environment := compileRuntimeJSON(t, asyncSkillJSON("stopsweep."+strings.ToLower(entry.name), `{"type":"entity"}`, `{"flow":"sequence","steps":[`+longSummonWithCountingCancel+`,{"flow":"finish"}]}`))
			host := newStopEntryHost(environment)
			runtime := NewRuntime(host, RuntimeOptions{MaxStopPendingSpawns: 1})
			first, err := runtime.Start(program, CastInput{Caster: 1, Target: 2})
			if err != nil {
				t.Fatal(err)
			}
			second, err := runtime.Start(program, CastInput{Caster: 1, Target: 2})
			if err != nil {
				t.Fatal(err)
			}
			firstSpawn, secondSpawn := onlySpawnOfCast(t, runtime, first).ID, onlySpawnOfCast(t, runtime, second).ID
			// 后一个衍生物的 lifecycle 实体消失、tick 回收被拒：它先进待停止，占满 MaxStopPendingSpawns = 1。
			dismissLifecycle(t, host, program, onlySpawnOfCast(t, runtime, second))
			host.failStops = 1
			if err := runtime.Advance(1); !errors.Is(err, errHostStopUnavailable) {
				t.Fatalf("advance = %v, want the refused reap reported", err)
			}
			if status := spawnStatusInSnapshot(runtime, secondSpawn); status != SpawnStopPending {
				t.Fatalf("spawn %d status %q after the refused reap, want stop_pending", secondSpawn, status)
			}

			// 入口按 ID 先停前一个：被拒、转入待停止，上限把后一个删掉；循环接着走到后一个。
			host.failStops, host.failRemovals = 1, true
			stopErr := callWithoutPanic(func() error { return entry.stop(runtime, program) })
			if !errors.Is(stopErr, errHostStopUnavailable) {
				t.Fatalf("%s = %v, want the refused stop reported", entry.name, stopErr)
			}
			if status := spawnStatusInSnapshot(runtime, firstSpawn); status != SpawnStopPending {
				t.Errorf("spawn %d status %q after the refused stop, want stop_pending", firstSpawn, status)
			}
			if status := spawnStatusInSnapshot(runtime, secondSpawn); status != SpawnAbandoned {
				t.Errorf("spawn %d status %q, want it abandoned at MaxStopPendingSpawns", secondSpawn, status)
			}
			advanceEachTick(t, runtime, runtime.currentTick+8)
			if status := spawnStatusInSnapshot(runtime, firstSpawn); status != SpawnCancelled {
				t.Errorf("spawn %d status %q after the retry, want cancelled", firstSpawn, status)
			}
		})
	}
}

func spawnStatusInSnapshot(runtime *Runtime, id SpawnID) SpawnStatus {
	for _, view := range runtime.StateSnapshot().Spawns {
		if view.ID == id {
			return view.Status
		}
	}
	return ""
}

func callWithoutPanic(call func() error) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("panicked: %v", recovered)
		}
	}()
	return call()
}
