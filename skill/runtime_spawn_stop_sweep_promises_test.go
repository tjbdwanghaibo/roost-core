package skill

// RR-20261006-34：Shutdown / RemoveProgram 先取一份衍生物 ID 列表，再逐个请求停止。前面一个被宿主拒绝、转入待停止时，
// 待停止条目到了 MaxStopPendingSpawns，makeRoomForStopPendingLocked 删掉最早的待停止记录——它可能正是列表里后面
// 那一个。之前循环按 ID 取回记录不判空，直接读 spawn.handedOff（Shutdown）/ spawn.CastID（RemoveProgram），
// 空指针 panic，持着 Runtime 的锁把进程带崩。
//
// 承诺：同一轮停止里被上限删掉的记录跳过（Runtime 已告警、不再负责它，与 retrySpawnStopsLocked 一致），
// 入口照常返回宿主的错误，其余衍生物照常转入待停止并在之后的 tick 重试停掉。

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

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
			if status := spawnStatusInSnapshot(runtime, secondSpawn); status != "" {
				t.Errorf("spawn %d status %q, want it dropped at MaxStopPendingSpawns", secondSpawn, status)
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
