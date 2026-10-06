package skill

// 停止入口统一（维护者 2026-10-07：“停止入口统一”，docs/feature/REFACTOR-2026-10-06-skill-spawn-stop-unified.md）。
//
// 衍生物的停止入口——施法失败（failCastLocked）与施法里的 goto / Cancel / Interrupt / 收尾、衍生物启动失败的清理、
// 移交时 lifecycle 实体已失效、tick 驱动的到期 / 失效回收、RemoveProgram、Shutdown——之前各自处理宿主拒绝
// StopSpawn：施法失败与 tick 回收标成 stop_pending 由 Runtime 退避重试（RR-20261006-21 后续、RR-20261006-31），
// Shutdown / RemoveProgram 把记录留成 running、错误交给调用方重试，其余路径靠错误一路传到 failCastLocked 再停一次
// （再跑一次 cancel 回调，RR-20261006-32）。
//
// 承诺：任何入口遇到宿主拒绝，衍生物都进入同一个待停止状态——记录 stop_pending、不在 OwnedSpawns 里、
// StateSnapshot 可见，同一次请求里只打一次宿主；之后 Runtime 在 SpawnStopRetryBackoff 个 tick 后重试并停掉它，
// 回调只跑一次。
//
// 守卫：spawnStopEntries 枚举全部停止入口。TestSpawnStopEntriesAreRegistered 读包源码，列出调用
// requestSpawnStop 的函数，必须与表里登记的 callers 一致；terminateSpawn / stopSpawn 只能由 requestSpawnStop
// 调用，宿主 StopSpawn 只能由 stopSpawn 调用。新增停止入口必须在表里登记一行并给出触发场景。

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

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
		callers: []string{"stopScopedSpawns"},
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
		callers: []string{"stopScopedSpawns"},
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
			dismissLifecycle(t, host, program, runtime.spawns[spawnID])
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
			dismissLifecycle(t, host, program, runtime.spawns[spawnID])
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
			spawn := runtime.spawns[run.spawnID]
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
			if spawn := runtime.spawns[run.spawnID]; spawn != nil && spawn.liveOnHost() {
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
	for callee, allowed := range map[string]string{"terminateSpawn": "requestSpawnStop", "stopSpawn": "terminateSpawn", "StopSpawn": "stopSpawn"} {
		for _, caller := range calls[callee] {
			if caller != allowed {
				t.Errorf("%s calls %s directly; only %s may (stop entries go through requestSpawnStop)", caller, callee, allowed)
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
