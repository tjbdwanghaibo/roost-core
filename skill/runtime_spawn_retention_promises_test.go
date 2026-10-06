package skill

// RR-20261006-23：CompletedCastLimit 承诺有界保留终态 cast，“仍被引用”的 cast 例外。castEvictableLocked 把 cast 名下
// 任何衍生物记录都当作引用，而衍生物停止后记录从不删除：起过 entity 衍生物（minion / area）的 cast，衍生物早已结束也永不回收，
// live Runtime 的 cast 与衍生物记录无界增长，完成队列超过 CompletedCastLimit 后 checkpoint 恢复判 corrupt。
// 承诺：只有运行中的衍生物（包括移交后仍在运行的）钉住 cast；已停的记录随 cast 一起回收。

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

func TestCastsWhoseSpawnsEndedStayWithinTheCompletedCastLimit(t *testing.T) {
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
	if owned := runtime.OwnedSpawns(1); len(owned) != 1 || owned[0].SourceCastID != pinned {
		t.Fatalf("owned spawns = %+v, want cast %d's minion spawn handed off and running", owned, pinned)
	}
	for range 8 {
		start(short)
		advance(6) // cast 在 tick 2 结束、衍生物移交，tick 4 衍生物到期
	}
	if stats := runtime.RetentionStats(); stats.Casts > options.CompletedCastLimit+1 {
		t.Errorf("retained casts = %d (completed queue %d) after 8 casts whose minion spawns ended; CompletedCastLimit is %d plus the pinned one", stats.Casts, stats.CompletedCasts, options.CompletedCastLimit)
	}
	if runtime.spawns.count() > options.CompletedCastLimit+1 {
		t.Errorf("retained spawn records = %d: records of evicted casts stay behind", runtime.spawns.count())
	}
	if _, found := runtime.InspectCast(pinned); !found {
		t.Errorf("cast %d was evicted while its handed-off minion spawn still runs", pinned)
	}
	checkpoint, err := runtime.Checkpoint()
	if err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	if _, err := RestoreRuntime(host, RuntimeOptions{}, checkpoint, resolver); err != nil {
		t.Fatalf("restore after 9 summon casts with CompletedCastLimit 3: %v", err)
	}

	advance(100) // 长衍生物到期，pinned 不再被引用
	start(short)
	advance(6)
	if _, found := runtime.InspectCast(pinned); found {
		t.Errorf("cast %d is still retained after its minion spawn ended and newer casts completed", pinned)
	}
	if stats := runtime.RetentionStats(); stats.Casts > options.CompletedCastLimit {
		t.Errorf("retained casts = %d after the long minion spawn ended, want at most %d", stats.Casts, options.CompletedCastLimit)
	}
}

// RR-20261006-30：被钉住的终态 cast 可以多于 CompletedCastLimit（pruneCompletedCastsLocked 跳过仍被引用的 cast），
// 恢复却要求完成队列不超过上限，live Runtime 合法持有的状态写出的 checkpoint 恢复不了。这里上限 1、两个移交后
// 仍在运行的召唤各钉住一个 cast。承诺：恢复按 prune 的不变量核对——超出上限的部分必须都仍被引用。
func TestHandedOffSpawnsBeyondTheCompletedLimitStillRestore(t *testing.T) {
	long, environment := summonSkill(t, "skill.test.retention.long", "100")
	host := runtimeTestHost(environment)
	runtime := NewRuntime(host, RuntimeOptions{CompletedCastLimit: 1})
	for range 2 {
		if _, err := runtime.Start(long, CastInput{Caster: 1, Target: 2}); err != nil {
			t.Fatal(err)
		}
		if err := runtime.Advance(runtime.currentTick + 3); err != nil {
			t.Fatal(err)
		}
	}
	if owned := runtime.OwnedSpawns(1); len(owned) != 2 {
		t.Fatalf("owned spawns = %+v, want both minion spawns handed off and running", owned)
	}
	if stats := runtime.RetentionStats(); stats.CompletedCasts != 2 {
		t.Fatalf("completed queue = %d, want both pinned casts kept beyond CompletedCastLimit 1", stats.CompletedCasts)
	}
	checkpoint, err := runtime.Checkpoint()
	if err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	resolver := ProgramResolverFunc(func(string, string) (*Program, error) { return long, nil })
	if _, err := RestoreRuntime(host, RuntimeOptions{}, checkpoint, resolver); err != nil {
		t.Fatalf("restore with two pinned casts and CompletedCastLimit 1: %v", err)
	}
}
