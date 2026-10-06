package skill

// 衍生物分区存放（维护者第十三轮“skill 衍生物两张表”，docs/feature/REFACTOR-2026-10-07-skill-spawn-partition.md）。
//
// 承诺：每条衍生物记录恰好在一个分区里，分区由记录字段（Status、handedOff）决定——施放中 / 已移交 / 待停止 /
// 已停止 / 已放弃；不再有指向同一批记录的第二张表。施放、移交、停止、待停止、重试、到上限、到待停止上限挪进已放弃、
// 已放弃分区超限清理、随 cast 回收、checkpoint 恢复之后都成立，恢复出的分区与恢复前一致。
//
// 守卫：
//   - TestSpawnPartitionsFollowRecordFields 在上面这些操作序列的每一步之后核对分区；
//   - TestSpawnPartitionWritesStayInSpawnTable 用 go/types 读包源码：分区 map 只能在 spawn_table.go 里引用，
//     写 map[SpawnID]*SpawnInstance（下标赋值、delete）只能在 spawnTable 的 setState / add / drop 与
//     newSpawnTable 里，给 SpawnInstance 的 Status、handedOff 赋值只能在 setState 里，Runtime.spawns 整体不能
//     被重新赋值。另起一张衍生物索引表、绕过 setState 改字段都会红；
//   - 同一守卫核对删记录的调用点（维护者第十三轮“待停止上限”选 B）：spawnTable.drop 只能在登记表 spawnDropSites 里的
//     函数调用，已放弃分区的清理 pruneAbandonedSpawnsLocked 只能由 Advance 调用。在 Shutdown 等循环里直接 drop 会红。

import (
	"fmt"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// fileSpawnForTest 把测试直接构造的记录放进分区（按记录字段）。
func fileSpawnForTest(runtime *Runtime, spawn *SpawnInstance) {
	runtime.spawns.drop(spawn.ID)
	runtime.spawns.add(spawn)
}

// allSpawnRecords 按 ID 升序返回全部分区的记录。
func allSpawnRecords(runtime *Runtime) []*SpawnInstance {
	return spawnRecordsIn(runtime)
}

// handedOffSpawnRecords 按 ID 升序返回已移交分区的记录（之前的 ownedSpawns）。
func handedOffSpawnRecords(runtime *Runtime) []*SpawnInstance {
	return spawnRecordsIn(runtime, spawnHandedOff)
}

func spawnRecordsIn(runtime *Runtime, partitions ...spawnPartition) []*SpawnInstance {
	ids := runtime.spawns.sortedIDs(nil, partitions...)
	result := make([]*SpawnInstance, 0, len(ids))
	for _, id := range ids {
		result = append(result, runtime.spawns.get(id))
	}
	return result
}

var spawnPartitionNames = [spawnPartitionCount]string{"casting", "handed_off", "stop_pending", "stopped", "abandoned"}

// spawnPartitionViolations 核对分区不变量：每条记录恰好在一个分区里，键就是记录 ID，所在分区与字段一致。
func spawnPartitionViolations(runtime *Runtime) []string {
	var violations []string
	seen := map[SpawnID]string{}
	for index, records := range runtime.spawns.partitions {
		partition := spawnPartition(index)
		if records == nil {
			violations = append(violations, fmt.Sprintf("partition %s is nil", spawnPartitionNames[partition]))
			continue
		}
		for id, spawn := range records {
			switch {
			case spawn == nil:
				violations = append(violations, fmt.Sprintf("partition %s holds a nil record under %d", spawnPartitionNames[partition], id))
				continue
			case spawn.ID != id:
				violations = append(violations, fmt.Sprintf("partition %s holds record %d under key %d", spawnPartitionNames[partition], spawn.ID, id))
			case spawn.partition() != partition:
				violations = append(violations, fmt.Sprintf("record %d (status %q, handed off %v) sits in %s, its fields say %s", id, spawn.Status, spawn.handedOff, spawnPartitionNames[partition], spawnPartitionNames[spawn.partition()]))
			}
			if previous, ok := seen[id]; ok {
				violations = append(violations, fmt.Sprintf("record %d sits in both %s and %s", id, previous, spawnPartitionNames[partition]))
			}
			seen[id] = spawnPartitionNames[partition]
		}
	}
	sort.Strings(violations)
	return violations
}

// spawnPartitionLayout 是分区 → 记录 ID 的快照，用来比较恢复前后。
func spawnPartitionLayout(runtime *Runtime) string {
	var parts []string
	for index := range runtime.spawns.partitions {
		parts = append(parts, fmt.Sprintf("%s=%v", spawnPartitionNames[index], runtime.spawns.sortedIDs(nil, spawnPartition(index))))
	}
	return strings.Join(parts, " ")
}

// assertSpawnPartitions 核对 live Runtime 的分区，再 checkpoint 恢复一份，核对恢复出的分区与 live 一致。
func assertSpawnPartitions(t *testing.T, step string, runtime *Runtime, host Host, resolver ProgramResolver) {
	t.Helper()
	runtime.mutex.Lock()
	violations := spawnPartitionViolations(runtime)
	layout := spawnPartitionLayout(runtime)
	runtime.mutex.Unlock()
	for _, violation := range violations {
		t.Errorf("%s: %s", step, violation)
	}
	checkpoint, err := runtime.Checkpoint()
	if err != nil {
		t.Fatalf("%s: checkpoint: %v", step, err)
	}
	restored, err := RestoreRuntime(host, RuntimeOptions{}, checkpoint, resolver)
	if err != nil {
		t.Fatalf("%s: restore: %v", step, err)
	}
	for _, violation := range spawnPartitionViolations(restored) {
		t.Errorf("%s: restored: %s", step, violation)
	}
	if got := spawnPartitionLayout(restored); got != layout {
		t.Errorf("%s: restored partitions %s, live %s", step, got, layout)
	}
}

func resolveAnyOf(programs ...*Program) ProgramResolver {
	return ProgramResolverFunc(func(id, digest string) (*Program, error) {
		for _, program := range programs {
			if program.id == id && program.identity.gameplayDigest == digest {
				return program, nil
			}
		}
		return nil, ErrCheckpointProgram
	})
}

func TestSpawnPartitionsFollowRecordFields(t *testing.T) {
	// 每个停止入口：施放 / 移交 → 宿主拒绝停止 → 待停止 → 退避重试 → 已停止，每个 tick 都核对，并经 checkpoint 恢复。
	for _, entry := range spawnStopEntries {
		t.Run("stop entry "+entry.name, func(t *testing.T) {
			run := entry.trigger(t)
			resolver := ProgramResolverFunc(func(id, digest string) (*Program, error) {
				for _, cast := range run.runtime.casts {
					if cast.program.id == id && cast.program.identity.gameplayDigest == digest {
						return cast.program, nil
					}
				}
				return nil, ErrCheckpointProgram
			})
			if spawn := run.runtime.spawns.get(run.spawnID, spawnStopPending); spawn == nil {
				t.Fatalf("spawn %d not in the stop-pending partition after the refused stop: %s", run.spawnID, spawnPartitionLayout(run.runtime))
			}
			assertSpawnPartitions(t, "after the refused stop", run.runtime, run.host, resolver)
			for through := run.runtime.currentTick + 12; run.runtime.currentTick < through; {
				if err := run.runtime.Advance(run.runtime.currentTick + 1); err != nil {
					t.Fatalf("advance to %d: %v", run.runtime.currentTick+1, err)
				}
				assertSpawnPartitions(t, fmt.Sprintf("tick %d", run.runtime.currentTick), run.runtime, run.host, resolver)
			}
			if spawn := run.runtime.spawns.get(run.spawnID); spawn != nil && spawn.partition() != spawnStopped {
				t.Errorf("spawn %d still in %s after the retry: %s", run.spawnID, spawnPartitionNames[spawn.partition()], spawnPartitionLayout(run.runtime))
			}
		})
	}

	// 施放中 → 已移交 → 到期已停止 → 随 cast 回收（CompletedCastLimit），中间夹着一个一直运行的召唤。
	t.Run("handoff, expiry and eviction", func(t *testing.T) {
		short, environment := summonSkill(t, "skill.test.partition.short", "4")
		long, _ := summonSkill(t, "skill.test.partition.long", "100")
		host := runtimeTestHost(environment)
		runtime := NewRuntime(host, RuntimeOptions{CompletedCastLimit: 1})
		resolver := resolveAnyOf(short, long)
		pinned, err := runtime.Start(long, CastInput{Caster: 1, Target: 2})
		if err != nil {
			t.Fatal(err)
		}
		if spawn := onlySpawnOfCast(t, runtime, pinned); spawn.partition() != spawnCasting {
			t.Fatalf("spawn of a running cast sits in %s, want casting", spawnPartitionNames[spawn.partition()])
		}
		assertSpawnPartitions(t, "started", runtime, host, resolver)
		for round := range 3 {
			if _, err := runtime.Start(short, CastInput{Caster: 1, Target: 2}); err != nil {
				t.Fatal(err)
			}
			for range 6 {
				if err := runtime.Advance(runtime.currentTick + 1); err != nil {
					t.Fatal(err)
				}
				assertSpawnPartitions(t, fmt.Sprintf("round %d tick %d", round, runtime.currentTick), runtime, host, resolver)
			}
		}
		if got := runtime.spawns.count(spawnHandedOff); got != 1 {
			t.Errorf("handed-off partition holds %d records, want the pinned summon only: %s", got, spawnPartitionLayout(runtime))
		}
		if got := runtime.spawns.count(spawnStopped); got > 1 {
			t.Errorf("stopped partition holds %d records with CompletedCastLimit 1: evicted casts left records behind (%s)", got, spawnPartitionLayout(runtime))
		}
	})

	// 一直拒绝停止：Shutdown → 待停止 → 重试到上限（exhausted）→ 新的待停止条目超过 MaxStopPendingSpawns，最早的挪进已放弃。
	// 2026-10-07（待停止上限选 B）：之前断言最早的那条被删掉，现在断言它在已放弃分区里。
	t.Run("retry exhaustion and the stop-pending limit", func(t *testing.T) {
		program, environment := compileRuntimeJSON(t, asyncSkillJSON("partition.limits", `{"type":"entity"}`, `{"flow":"sequence","steps":[`+longSummonWithCountingCancel+`,{"flow":"finish"}]}`))
		host := newStopEntryHost(environment)
		host.failStops = -1
		runtime := NewRuntime(host, RuntimeOptions{SpawnStopRetryLimit: 1, MaxStopPendingSpawns: 2})
		resolver := resolveAnyOf(program)
		var started []SpawnID
		for round := range 3 {
			castID, err := runtime.Start(program, CastInput{Caster: 1, Target: 2})
			if err != nil {
				t.Fatal(err)
			}
			started = append(started, onlySpawnOfCast(t, runtime, castID).ID)
			assertSpawnPartitions(t, fmt.Sprintf("round %d handed off", round), runtime, host, resolver)
			if err := runtime.Shutdown(); err == nil {
				t.Fatalf("round %d: Shutdown returned nil with every stop refused", round)
			}
			assertSpawnPartitions(t, fmt.Sprintf("round %d shutdown", round), runtime, host, resolver)
			for range 6 {
				if err := runtime.Advance(runtime.currentTick + 1); err != nil {
					t.Fatal(err)
				}
				assertSpawnPartitions(t, fmt.Sprintf("round %d tick %d", round, runtime.currentTick), runtime, host, resolver)
			}
		}
		if got := runtime.spawns.count(spawnStopPending); got != 2 {
			t.Errorf("stop-pending partition holds %d records, want MaxStopPendingSpawns 2: %s", got, spawnPartitionLayout(runtime))
		}
		if runtime.spawns.get(started[0], spawnAbandoned) == nil {
			t.Errorf("oldest stop-pending spawn %d not abandoned past MaxStopPendingSpawns: %s", started[0], spawnPartitionLayout(runtime))
		}
		if stats := runtime.RetentionStats(); stats.StopRetryExhaustedSpawns == 0 {
			t.Errorf("retention stats %+v: no spawn reached the retry limit", stats)
		}
		host.failStops = 0
		if err := runtime.Shutdown(); err != nil {
			t.Fatal(err)
		}
		assertSpawnPartitions(t, "final shutdown", runtime, host, resolver)
		if live := runtime.spawns.count(spawnLivePartitions...); live != 0 {
			t.Errorf("%d spawns still live after a successful shutdown: %s", live, spawnPartitionLayout(runtime))
		}
	})

	// 待停止上限 1、已放弃上限 2，宿主一直拒绝：每轮“施放 → 移交 → Shutdown”把上一条待停止挪进已放弃；
	// 偶数轮之后 Advance，已放弃分区只在 Advance 末尾清理到 2 条；每一步都经 checkpoint 恢复核对。
	// 最后从同一个 checkpoint 恢复一份，与 live 各推进一次，分区相同（恢复出的 Runtime 按同一上限清理）。
	t.Run("abandoned at the stop-pending limit, pruned at the end of Advance", func(t *testing.T) {
		program, environment := compileRuntimeJSON(t, asyncSkillJSON("partition.abandon", `{"type":"entity"}`, `{"flow":"sequence","steps":[`+longSummonWithCountingCancel+`,{"flow":"finish"}]}`))
		host := newStopEntryHost(environment)
		host.failStops, host.failRemovals = -1, true
		runtime := NewRuntime(host, RuntimeOptions{MaxStopPendingSpawns: 1, MaxAbandonedSpawns: 2})
		resolver := resolveAnyOf(program)
		var started []SpawnID
		wantAbandoned := 0
		for round := range 6 {
			castID, err := runtime.Start(program, CastInput{Caster: 1, Target: 2})
			if err != nil {
				t.Fatal(err)
			}
			started = append(started, onlySpawnOfCast(t, runtime, castID).ID)
			if err := runtime.Shutdown(); err == nil {
				t.Fatalf("round %d: Shutdown returned nil with every stop refused", round)
			}
			assertSpawnPartitions(t, fmt.Sprintf("round %d shutdown", round), runtime, host, resolver)
			// 第一轮之后每轮 Shutdown 都把上一条待停止挪进已放弃；Advance 之外不删。
			if round > 0 {
				wantAbandoned++
			}
			if got := runtime.spawns.count(spawnAbandoned); got != wantAbandoned {
				t.Errorf("round %d before Advance: abandoned partition holds %d records, want %d (nothing pruned outside Advance): %s", round, got, wantAbandoned, spawnPartitionLayout(runtime))
			}
			if round%2 == 1 {
				if err := runtime.Advance(runtime.currentTick + 1); err != nil {
					t.Fatal(err)
				}
				assertSpawnPartitions(t, fmt.Sprintf("round %d advance", round), runtime, host, resolver)
				wantAbandoned = min(wantAbandoned, 2)
				if got := runtime.spawns.count(spawnAbandoned); got != wantAbandoned {
					t.Errorf("round %d after Advance: abandoned partition holds %d records, want %d (MaxAbandonedSpawns 2): %s", round, got, wantAbandoned, spawnPartitionLayout(runtime))
				}
			}
		}
		if got := runtime.spawns.sortedIDs(nil, spawnAbandoned); len(got) != 2 || got[0] != started[3] || got[1] != started[4] {
			t.Errorf("abandoned partition %v, want the two newest abandoned %v: %s", got, started[3:5], spawnPartitionLayout(runtime))
		}
		// 再放弃一条（暂时超限）后 checkpoint；恢复出的 Runtime 与 live 各推进一次，清理结果相同。
		if _, err := runtime.Start(program, CastInput{Caster: 1, Target: 2}); err != nil {
			t.Fatal(err)
		}
		if err := runtime.Shutdown(); err == nil {
			t.Fatal("Shutdown returned nil with every stop refused")
		}
		checkpoint, err := runtime.Checkpoint()
		if err != nil {
			t.Fatal(err)
		}
		restored, err := RestoreRuntime(host, RuntimeOptions{}, checkpoint, resolver)
		if err != nil {
			t.Fatalf("restore with the abandoned partition over its limit: %v", err)
		}
		next := runtime.currentTick + 1
		if err := runtime.Advance(next); err != nil {
			t.Fatal(err)
		}
		if err := restored.Advance(next); err != nil {
			t.Fatal(err)
		}
		for _, violation := range spawnPartitionViolations(restored) {
			t.Errorf("restored after Advance: %s", violation)
		}
		if live, got := spawnPartitionLayout(runtime), spawnPartitionLayout(restored); live != got || runtime.spawns.count(spawnAbandoned) != 2 {
			t.Errorf("after Advance: restored %s, live %s (want 2 abandoned in both)", got, live)
		}
		// 宿主恢复：Shutdown 停掉待停止的，已放弃的不再停（Runtime 不再负责），记录留到被清理。
		host.failStops, host.failRemovals = 0, false
		if err := runtime.Shutdown(); err != nil {
			t.Fatal(err)
		}
		assertSpawnPartitions(t, "final shutdown", runtime, host, resolver)
		if live, abandoned := runtime.spawns.count(spawnLivePartitions...), runtime.spawns.count(spawnAbandoned); live != 0 || abandoned != 2 {
			t.Errorf("after a successful shutdown: %d live, %d abandoned, want 0 and 2: %s", live, abandoned, spawnPartitionLayout(runtime))
		}
	})
}

// spawnDropSites 是允许调用 spawnTable.drop 的函数登记表（维护者第十三轮“待停止上限”选 B）。删记录只发生在这三个安全点，
// 不在任何“取 ID 列表 → 逐个处理”的循环中途；新增调用点必须在这里登记并说明为什么不会让别的循环取回 nil。
var spawnDropSites = map[string]string{
	"Runtime.pruneAbandonedSpawnsLocked": "Advance 末尾清理超出 MaxAbandonedSpawns 的已放弃记录（只由 Advance 调用）",
	"Runtime.forgetCastSpawnsLocked":     "随 cast 回收已停止分区的记录（cast 已确认没有 live 衍生物）",
	"Runtime.startEntitySpawn":           "衍生物启动失败、停止成功之后回收这条刚启动的记录（调用方不持有它的 ID 列表）",
}

// 源码守卫：分区 map 与分区字段只在 spawn_table.go 的指定函数里写。
func TestSpawnPartitionWritesStayInSpawnTable(t *testing.T) {
	for _, violation := range spawnPartitionWriteViolations(t) {
		t.Error(violation)
	}
}

func spawnPartitionWriteViolations(t *testing.T) []string {
	t.Helper()
	fileSet := token.NewFileSet()
	names, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var files []*ast.File
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fileSet, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, file)
	}
	info := &types.Info{Types: map[ast.Expr]types.TypeAndValue{}, Selections: map[*ast.SelectorExpr]*types.Selection{}, Uses: map[*ast.Ident]types.Object{}, Defs: map[*ast.Ident]types.Object{}}
	config := types.Config{Importer: importer.ForCompiler(fileSet, "source", nil)}
	pkg, err := config.Check("github.com/tjbdwanghaibo/roost-core/skill", fileSet, files, info)
	if err != nil {
		t.Fatal(err)
	}
	field := func(typeName, name string) types.Object {
		named := pkg.Scope().Lookup(typeName).Type().Underlying().(*types.Struct)
		for index := range named.NumFields() {
			if named.Field(index).Name() == name {
				return named.Field(index)
			}
		}
		t.Fatalf("%s has no field %s", typeName, name)
		return nil
	}
	partitionsField := field("spawnTable", "partitions")
	statusField, handedOffField := field("SpawnInstance", "Status"), field("SpawnInstance", "handedOff")
	tableField := field("Runtime", "spawns")
	spawnMap := types.NewMap(pkg.Scope().Lookup("SpawnID").Type(), types.NewPointer(pkg.Scope().Lookup("SpawnInstance").Type()))

	mapWriters := map[string]bool{"spawnTable.setState": true, "spawnTable.add": true, "spawnTable.drop": true, "newSpawnTable": true}
	fieldWriters := map[string]bool{"spawnTable.setState": true}
	method := func(typeName, name string) types.Object {
		object, _, _ := types.LookupFieldOrMethod(types.NewPointer(pkg.Scope().Lookup(typeName).Type()), false, pkg, name)
		if object == nil {
			t.Fatalf("%s has no method %s", typeName, name)
		}
		return object
	}
	dropMethod, pruneMethod := method("spawnTable", "drop"), method("Runtime", "pruneAbandonedSpawnsLocked")

	selected := func(expr ast.Expr) types.Object {
		if selector, ok := ast.Unparen(expr).(*ast.SelectorExpr); ok {
			if selection := info.Selections[selector]; selection != nil {
				return selection.Obj()
			}
		}
		return nil
	}
	// writtenMap 返回被下标赋值写入的 map 表达式（m[k] = v 的 m），不是下标赋值时返回 nil。
	writtenMap := func(expr ast.Expr) ast.Expr {
		if index, ok := ast.Unparen(expr).(*ast.IndexExpr); ok {
			return index.X
		}
		return nil
	}
	isSpawnMap := func(expr ast.Expr) bool {
		return expr != nil && types.Identical(info.TypeOf(expr), spawnMap)
	}

	var violations []string
	for _, file := range files {
		fileName := filepath.Base(fileSet.Position(file.Pos()).Filename)
		for _, declaration := range file.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok || function.Body == nil {
				continue
			}
			name := function.Name.Name
			if function.Recv != nil && len(function.Recv.List) == 1 {
				receiver := function.Recv.List[0].Type
				if star, ok := receiver.(*ast.StarExpr); ok {
					receiver = star.X
				}
				if ident, ok := receiver.(*ast.Ident); ok {
					name = ident.Name + "." + name
				}
			}
			report := func(node ast.Node, format string, args ...any) {
				violations = append(violations, fmt.Sprintf("%s: %s: %s", fileSet.Position(node.Pos()), name, fmt.Sprintf(format, args...)))
			}
			ast.Inspect(function.Body, func(node ast.Node) bool {
				switch typed := node.(type) {
				case *ast.SelectorExpr:
					switch selected(typed) {
					case dropMethod:
						if _, ok := spawnDropSites[name]; !ok {
							report(typed, "references spawnTable.drop; records leave the table only at the sites registered in spawnDropSites (no deletion in the middle of another loop)")
						}
					case pruneMethod:
						if name != "Runtime.Advance" {
							report(typed, "references pruneAbandonedSpawnsLocked; the abandoned partition is pruned only at the end of Advance")
						}
					}
					if obj := selected(typed); obj == partitionsField && fileName != "spawn_table.go" {
						report(typed, "references spawnTable.partitions outside spawn_table.go; go through spawnTable's methods")
					}
				case *ast.AssignStmt:
					for _, lhs := range typed.Lhs {
						if target := writtenMap(lhs); isSpawnMap(target) && !mapWriters[name] {
							report(lhs, "writes a map[SpawnID]*SpawnInstance; only spawnTable.setState / add / drop and newSpawnTable may (no second spawn index)")
						}
						switch selected(lhs) {
						case statusField, handedOffField:
							if !fieldWriters[name] {
								report(lhs, "assigns a partition field (SpawnInstance.Status / handedOff); only spawnTable.setState may, so the record moves with it")
							}
						case tableField:
							report(lhs, "reassigns Runtime.spawns; records enter and leave through spawnTable.add / drop")
						case partitionsField:
							if !mapWriters[name] {
								report(lhs, "reassigns spawnTable.partitions")
							}
						}
						if star, ok := ast.Unparen(lhs).(*ast.StarExpr); ok && types.Identical(info.TypeOf(star), pkg.Scope().Lookup("SpawnInstance").Type()) {
							report(lhs, "overwrites a whole SpawnInstance; partition fields change only through spawnTable.setState")
						}
					}
				case *ast.UnaryExpr:
					if typed.Op == token.AND {
						switch selected(typed.X) {
						case statusField, handedOffField:
							report(typed, "takes the address of a partition field")
						}
					}
				case *ast.CallExpr:
					if ident, ok := typed.Fun.(*ast.Ident); ok && ident.Name == "delete" && len(typed.Args) > 0 && isSpawnMap(typed.Args[0]) && !mapWriters[name] {
						report(typed, "deletes from a map[SpawnID]*SpawnInstance; only spawnTable.setState / drop may")
					}
				}
				return true
			})
		}
	}
	sort.Strings(violations)
	return violations
}
