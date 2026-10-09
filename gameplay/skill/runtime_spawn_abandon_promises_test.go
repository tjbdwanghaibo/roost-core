package skill

// 待停止上限改为“放弃”（维护者第十三轮“待停止上限”选 B，2026-10-07；docs/feature/REFACTOR-2026-10-07-skill-spawn-partition.md §11）。
//
// 之前待停止条目超过 MaxStopPendingSpawns 时，makeRoomForStopPendingLocked 在别的循环进行中删掉最早的记录：
// Shutdown / RemoveProgram 的“取 ID 列表 → 逐个处理”循环随后取回 nil，只靠 RR-20261006-34 加的判空才不崩；
// 宿主那边仍在运行这个衍生物，Runtime 却不再记得它，客户端还收到 spawn_remove。
//
// 承诺：
//   - 到上限时最早的待停止记录挪进“已放弃”分区（status abandoned），不删：同一轮循环后面取回的是这条已放弃的记录，
//     停止请求对它是空操作——不依赖判空也安全；
//   - 放弃之后不再重试、不再打宿主；计 skill.spawn.abandoned.total，写一条点名 spawn id、cast、宿主的 Error 日志；
//     不占待停止名额、不钉住 cast（cast 照常按 CompletedCastLimit 回收，放弃的记录留下）；
//   - 客户端看到一次 spawn_update(abandoned)（presentation）与 spawn_upsert(abandoned)（state mutation），不发 spawn_remove；
//     presentation reset 不再带它（Runtime 不再推进它的表现）；
//   - “已放弃”分区有自己的上限 MaxAbandonedSpawns，超限只在 Advance 末尾清理最早的（计 skill.spawn.abandoned_pruned.total、
//     写日志），这时 state mutation 才发 spawn_remove；Advance 之外的入口不删记录，checkpoint 恢复不因暂时超限拒绝。

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// spawnMutationTrail 返回 state mutation 里关于这个衍生物的“kind:status”序列。
func spawnMutationTrail(runtime *Runtime, id SpawnID) string {
	var trail []string
	for _, mutation := range runtime.StateDeltas(0, 0).Mutations {
		if mutation.SpawnID != id {
			continue
		}
		entry := string(mutation.Kind)
		if mutation.Spawn != nil {
			entry += ":" + string(mutation.Spawn.Status)
		}
		trail = append(trail, entry)
	}
	return strings.Join(trail, " ")
}

func countSpawnStops(host *stopRetryHost, id SpawnID) int {
	count := 0
	for _, stopped := range host.stopIDs {
		if stopped == id {
			count++
		}
	}
	return count
}

// Shutdown / RemoveProgram 循环中途触发待停止上限：后面那条记录被放弃而不是删除，循环取回它、停止请求是空操作。
func TestStopSweepAbandonsAtTheStopPendingLimit(t *testing.T) {
	for _, entry := range []struct {
		name string
		stop func(runtime *Runtime, program *Program) error
	}{
		{name: "Shutdown", stop: func(runtime *Runtime, _ *Program) error { return runtime.Shutdown() }},
		{name: "RemoveProgram", stop: func(runtime *Runtime, program *Program) error { return runtime.RemoveProgram(program.id) }},
	} {
		t.Run(entry.name, func(t *testing.T) {
			program, environment := compileRuntimeJSON(t, asyncSkillJSON("abandon."+strings.ToLower(entry.name), `{"type":"entity"}`, `{"flow":"sequence","steps":[`+longSummonWithCountingCancel+`,{"flow":"finish"}]}`))
			host := newStopEntryHost(environment)
			runtime := NewRuntime(host, RuntimeOptions{MaxStopPendingSpawns: 1})
			logs := captureDefaultLog(t)
			abandonedBefore := counterValue(MetricSpawnAbandoned)
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

			// 入口按 ID 先停前一个：被拒、转入待停止，上限把后一个挪进已放弃；循环接着走到后一个。
			host.failStops, host.failRemovals = 1, true
			stopsBefore := countSpawnStops(host.stopRetryHost, secondSpawn)
			stopErr := callWithoutPanic(func() error { return entry.stop(runtime, program) })
			if !errors.Is(stopErr, errHostStopUnavailable) {
				t.Fatalf("%s = %v, want the refused stop reported", entry.name, stopErr)
			}
			if status := spawnStatusInSnapshot(runtime, firstSpawn); status != SpawnStopPending {
				t.Errorf("spawn %d status %q after the refused stop, want stop_pending", firstSpawn, status)
			}
			if status := spawnStatusInSnapshot(runtime, secondSpawn); status != SpawnAbandoned {
				t.Errorf("spawn %d status %q at MaxStopPendingSpawns, want abandoned (kept, not dropped)", secondSpawn, status)
			}
			if stats := runtime.RetentionStats(); stats.StopPendingSpawns != 1 || stats.AbandonedSpawns != 1 {
				t.Errorf("retention stats = %+v, want one stop-pending and one abandoned spawn", stats)
			}
			if got := counterValue(MetricSpawnAbandoned) - abandonedBefore; got != 1 {
				t.Errorf("%s grew by %d, want 1", MetricSpawnAbandoned, got)
			}
			text := logs.String()
			if !strings.Contains(text, "level=ERROR") || !strings.Contains(text, "abandoned") || !strings.Contains(text, fmt.Sprintf("spawn_id=%d", secondSpawn)) || !strings.Contains(text, fmt.Sprintf("cast_id=%d", second)) || !strings.Contains(text, "owner=1") {
				t.Errorf("abandon log = %q, want an error naming spawn %d, cast %d and owner 1", text, secondSpawn, second)
			}
			if trail := spawnMutationTrail(runtime, secondSpawn); strings.Contains(trail, string(StateMutationSpawnRemove)) || !strings.HasSuffix(trail, "spawn_upsert:abandoned") {
				t.Errorf("state mutations for spawn %d = %q, want it to end in spawn_upsert:abandoned without spawn_remove", secondSpawn, trail)
			}

			// 之后的 tick：前一个按退避重试停掉；被放弃的不再打宿主、记录留着，也不再钉住 cast。
			advanceEachTick(t, runtime, runtime.currentTick+8)
			if status := spawnStatusInSnapshot(runtime, firstSpawn); status != SpawnCancelled {
				t.Errorf("spawn %d status %q after the retry, want cancelled", firstSpawn, status)
			}
			if status := spawnStatusInSnapshot(runtime, secondSpawn); status != SpawnAbandoned {
				t.Errorf("spawn %d status %q after the retries, want still abandoned", secondSpawn, status)
			}
			if stops := countSpawnStops(host.stopRetryHost, secondSpawn) - stopsBefore; stops != 0 {
				t.Errorf("host got %d StopSpawn calls for abandoned spawn %d, want none", stops, secondSpawn)
			}
			for _, owned := range runtime.OwnedSpawns(1) {
				if owned.ID == secondSpawn {
					t.Errorf("abandoned spawn %d still listed in OwnedSpawns", secondSpawn)
				}
			}
			runtime.mutex.Lock()
			pinned := runtime.castHasRunningSpawnLocked(second)
			runtime.mutex.Unlock()
			if pinned {
				t.Errorf("cast %d still pinned by its abandoned spawn", second)
			}
		})
	}
}

// 已放弃分区超过 MaxAbandonedSpawns：Advance 之外不删；Advance 末尾删最早的、计指标、写日志，这时才发 spawn_remove。
// 暂时超限的 checkpoint 能恢复，恢复出的 Runtime 在下一次 Advance 末尾做同样的清理。
func TestAbandonedSpawnsArePrunedOnlyAtTheEndOfAdvance(t *testing.T) {
	program, environment := compileRuntimeJSON(t, asyncSkillJSON("abandon.prune", `{"type":"entity"}`, `{"flow":"sequence","steps":[`+longSummonWithCountingCancel+`,{"flow":"finish"}]}`))
	host := newStopEntryHost(environment)
	host.failStops, host.failRemovals = -1, true
	runtime := NewRuntime(host, RuntimeOptions{MaxStopPendingSpawns: 1, MaxAbandonedSpawns: 1})
	logs := captureDefaultLog(t)
	prunedBefore := counterValue(MetricSpawnAbandonedPruned)
	var spawns []SpawnID
	for range 3 {
		castID, err := runtime.Start(program, CastInput{Caster: 1, Target: 2})
		if err != nil {
			t.Fatal(err)
		}
		spawns = append(spawns, onlySpawnOfCast(t, runtime, castID).ID)
		if err := runtime.Shutdown(); !errors.Is(err, errHostStopUnavailable) {
			t.Fatalf("Shutdown = %v, want the refused stop reported", err)
		}
	}
	// 三轮之后：最新的待停止，前两个已放弃——超过 MaxAbandonedSpawns = 1，但 Advance 之前不删。
	for index, want := range []SpawnStatus{SpawnAbandoned, SpawnAbandoned, SpawnStopPending} {
		if status := spawnStatusInSnapshot(runtime, spawns[index]); status != want {
			t.Errorf("before Advance: spawn %d status %q, want %q", spawns[index], status, want)
		}
	}
	if got := counterValue(MetricSpawnAbandonedPruned) - prunedBefore; got != 0 {
		t.Errorf("%s grew by %d outside Advance, want 0", MetricSpawnAbandonedPruned, got)
	}
	checkpoint, err := runtime.Checkpoint()
	if err != nil {
		t.Fatal(err)
	}
	restored, err := RestoreRuntime(host, RuntimeOptions{}, checkpoint, resolveAnyOf(program))
	if err != nil {
		t.Fatalf("restore with the abandoned partition over its limit: %v", err)
	}
	// checkpoint 版本 7：版本 6 及更早（没有 abandoned 与 max_abandoned_spawns）拒绝。
	previous := checkpoint
	previous.Version = 6
	if _, err := RestoreRuntime(host, RuntimeOptions{}, previous, resolveAnyOf(program)); !errors.Is(err, ErrCheckpointUnsupported) {
		t.Errorf("restore of a version 6 checkpoint = %v, want ErrCheckpointUnsupported", err)
	}

	if err := runtime.Advance(runtime.currentTick + 1); err != nil {
		t.Fatal(err)
	}
	if status := spawnStatusInSnapshot(runtime, spawns[0]); status != "" {
		t.Errorf("after Advance: oldest abandoned spawn %d status %q, want it pruned", spawns[0], status)
	}
	if status := spawnStatusInSnapshot(runtime, spawns[1]); status != SpawnAbandoned {
		t.Errorf("after Advance: spawn %d status %q, want it kept as abandoned", spawns[1], status)
	}
	if got := counterValue(MetricSpawnAbandonedPruned) - prunedBefore; got != 1 {
		t.Errorf("%s grew by %d, want 1", MetricSpawnAbandonedPruned, got)
	}
	if text := logs.String(); !strings.Contains(text, "abandoned spawn pruned") || !strings.Contains(text, fmt.Sprintf("spawn_id=%d", spawns[0])) {
		t.Errorf("prune log = %q, want one naming spawn %d", text, spawns[0])
	}
	if trail := spawnMutationTrail(runtime, spawns[0]); trail != "spawn_upsert:running spawn_upsert:stop_pending spawn_upsert:abandoned spawn_remove" {
		t.Errorf("state mutations for spawn %d = %q, want spawn_remove only after the Advance that pruned it", spawns[0], trail)
	}

	if err := restored.Advance(restored.currentTick + 1); err != nil {
		t.Fatal(err)
	}
	restored.mutex.Lock()
	restoredLayout := spawnPartitionLayout(restored)
	restored.mutex.Unlock()
	runtime.mutex.Lock()
	liveLayout := spawnPartitionLayout(runtime)
	runtime.mutex.Unlock()
	if restoredLayout != liveLayout {
		t.Errorf("restored runtime after Advance: %s, live %s", restoredLayout, liveLayout)
	}
}

// 客户端看到的：放弃时 presentation 一条 spawn_update(abandoned)、state mutation 一条 spawn_upsert(abandoned)，没有 spawn_remove；
// reset 不再带它。cast 照常按 CompletedCastLimit 回收，放弃的记录留下。
func TestAbandonedSpawnIsVisibleToSyncConsistently(t *testing.T) {
	program, environment := visualAreaThatCannotPay(t)
	base := runtimeTestHost(environment)
	setTerminalMana(base, 10)
	host := &stopRetryHost{MemoryHost: base, failStops: -1}
	runtime := NewRuntime(host, RuntimeOptions{MaxStopPendingSpawns: 1, CompletedCastLimit: 1})
	for want := CastID(1); want <= 2; want++ {
		if id, err := runtime.Start(program, CastInput{Caster: 1, Target: 2}); id != want || !errors.Is(err, ErrInsufficientResource) {
			t.Fatalf("failed start = %d, %v; want cast %d kept", id, err, want)
		}
	}
	first, second := onlySpawnOfCast(t, runtime, 1).ID, onlySpawnOfCast(t, runtime, 2).ID
	advanceEachTick(t, runtime, 40)

	if _, found := runtime.InspectCast(1); found {
		t.Errorf("cast 1 kept past CompletedCastLimit 1: its only spawn is abandoned and no longer pins it")
	}
	if status := spawnStatusInSnapshot(runtime, first); status != SpawnAbandoned {
		t.Errorf("spawn %d status %q, want abandoned, kept after its cast was reclaimed", first, status)
	}
	if trail := spawnMutationTrail(runtime, first); trail != "spawn_upsert:stop_pending spawn_upsert:abandoned" {
		t.Errorf("state mutations for spawn %d = %q, want spawn_upsert:stop_pending spawn_upsert:abandoned (no spawn_remove)", first, trail)
	}
	var presentation []string
	for _, event := range runtime.PresentationEvents(0) {
		if event.HasSpawn && event.SpawnID == first && event.Kind != PresentationSpawnSignal {
			presentation = append(presentation, string(event.Kind)+":"+string(event.SpawnStatus))
		}
	}
	if want := "spawn_start:running spawn_update:stop_pending spawn_update:abandoned"; strings.Join(presentation, " ") != want {
		t.Errorf("presentation for spawn %d = %v, want %s", first, presentation, want)
	}
	reset := map[SpawnID]SpawnStatus{}
	for _, entry := range runtime.PresentationSnapshot().Active {
		if entry.Kind == ActivePresentationSpawn {
			reset[entry.SpawnID] = entry.SpawnStatus
		}
	}
	if _, found := reset[first]; found {
		t.Errorf("presentation reset still carries abandoned spawn %d", first)
	}
	if status := reset[second]; status != SpawnStopPending {
		t.Errorf("presentation reset carries stop-pending spawn %d as %q, want stop_pending", second, status)
	}
}
