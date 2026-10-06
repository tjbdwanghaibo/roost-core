package skill

// RR-20261006-23：CompletedCastLimit 承诺有界保留终态 cast，“仍被引用”的 cast 例外。castEvictableLocked 把 cast 名下
// 任何进程记录都当作引用，而进程停止后记录从不删除：起过 entity 进程（summon / area）的 cast，进程早已结束也永不回收，
// live Runtime 的 cast 与进程记录无界增长，完成队列超过 CompletedCastLimit 后 checkpoint 恢复判 corrupt。
// 承诺：只有运行中的进程（包括移交后仍在运行的）钉住 cast；已停的记录随 cast 一起回收。

import (
	"strings"
	"testing"
)

func summonSkill(t *testing.T, id string, duration string) (*Program, CompileEnvironment) {
	t.Helper()
	summon := strings.Replace(summonWithCountingCancel, `"duration_ticks":10`, `"duration_ticks":`+duration, 1)
	json := `{"schema":"roost.skill/v2","id":"` + id + `","name":"Summon","description":"Summons, then finishes.","activation":{"type":"active","policy":{"mode":"tap"}},"input_schema":{"type":"entity"},"cooldown_ticks":0,"costs":[],"memory":{},"initial_phase":"cast","phases":[{"id":"cast","timeout_ticks":0,"on":{"enter":{"flow":"sequence","steps":[` + summon + `,{"flow":"wait","ticks":2,"then":{"flow":"finish"}}]}}}]}`
	return compileRuntimeJSON(t, json)
}

func TestCastsWhoseProcessesEndedStayWithinTheCompletedCastLimit(t *testing.T) {
	short, environment := summonSkill(t, "skill.test.retention.short", "4")
	long, _ := summonSkill(t, "skill.test.retention.long", "100")
	host := runtimeTestHost(environment)
	options := RuntimeOptions{CompletedCastLimit: 3}
	runtime := NewRuntime(host, options)
	advance := func(ticks Tick) {
		t.Helper()
		if err := runtime.Advance(runtime.currentTick + ticks); err != nil {
			t.Fatal(err)
		}
	}
	start := func(program *Program) CastID {
		t.Helper()
		id, err := runtime.Start(program, CastInput{Caster: 1, Target: 2})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	resolver := ProgramResolverFunc(func(id, _ string) (*Program, error) {
		if id == long.id {
			return long, nil
		}
		return short, nil
	})

	pinned := start(long)
	advance(3)
	if owned := runtime.OwnedProcesses(1); len(owned) != 1 || owned[0].SourceCastID != pinned {
		t.Fatalf("owned processes = %+v, want cast %d's summon handed off and running", owned, pinned)
	}
	for range 8 {
		start(short)
		advance(6) // cast 在 tick 2 结束、进程移交，tick 4 进程到期
	}
	if stats := runtime.RetentionStats(); stats.Casts > options.CompletedCastLimit+1 {
		t.Errorf("retained casts = %d (completed queue %d) after 8 casts whose summons ended; CompletedCastLimit is %d plus the pinned one", stats.Casts, stats.CompletedCasts, options.CompletedCastLimit)
	}
	if len(runtime.processes) > options.CompletedCastLimit+1 {
		t.Errorf("retained process records = %d: records of evicted casts stay behind", len(runtime.processes))
	}
	if _, found := runtime.InspectCast(pinned); !found {
		t.Errorf("cast %d was evicted while its handed-off summon still runs", pinned)
	}
	checkpoint, err := runtime.Checkpoint()
	if err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	if _, err := RestoreRuntime(host, RuntimeOptions{}, checkpoint, resolver); err != nil {
		t.Fatalf("restore after 9 summon casts with CompletedCastLimit 3: %v", err)
	}

	advance(100) // 长进程到期，pinned 不再被引用
	start(short)
	advance(6)
	if _, found := runtime.InspectCast(pinned); found {
		t.Errorf("cast %d is still retained after its summon ended and newer casts completed", pinned)
	}
	if stats := runtime.RetentionStats(); stats.Casts > options.CompletedCastLimit {
		t.Errorf("retained casts = %d after the long summon ended, want at most %d", stats.Casts, options.CompletedCastLimit)
	}
}
