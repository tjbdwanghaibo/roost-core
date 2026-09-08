package skill

import (
	"reflect"
	"testing"
)

func TestSchedulerOrdersByDueTickThenSequence(t *testing.T) {
	scheduler := newScheduler()
	scheduler.Push(scheduledTask{DueTick: 5, Sequence: 2})
	scheduler.Push(scheduledTask{DueTick: 3, Sequence: 3})
	scheduler.Push(scheduledTask{DueTick: 5, Sequence: 1})
	got := []uint64{scheduler.Pop().Sequence, scheduler.Pop().Sequence, scheduler.Pop().Sequence}
	want := []uint64{3, 1, 2}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestRuntimeWaitExecutesExactlyAtDueTick(t *testing.T) {
	flow := `{"flow":"wait","ticks":5,"then":{"flow":"sequence","steps":[{"flow":"effect","effect":{"type":"damage","target":"$input.target","amount":3,"damage_type":"physical"}},{"flow":"finish"}]}}`
	program, environment := compileRuntimeJSON(t, asyncSkillJSON("wait", `{"type":"entity"}`, flow))
	host := runtimeTestHost(environment)
	runtime := NewRuntime(host, RuntimeOptions{})
	castID, err := runtime.Activate(program, CastInput{Caster: 1, Target: 2})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot, _ := runtime.InspectCast(castID); snapshot.Status != CastSuspended {
		t.Fatalf("cast = %#v", snapshot)
	}
	if err := runtime.Advance(4); err != nil || host.HealthForTest(2) != 100 {
		t.Fatalf("wait fired early: %v", err)
	}
	if err := runtime.Advance(5); err != nil || host.HealthForTest(2) != 97 {
		t.Fatalf("wait did not fire at due tick: %v", err)
	}
	if snapshot, _ := runtime.InspectCast(castID); snapshot.Status != CastFinished {
		t.Fatalf("cast = %#v", snapshot)
	}
}

func TestRuntimeRepeatIntervalExecutesExactCount(t *testing.T) {
	flow := `{"flow":"sequence","steps":[{"flow":"repeat","times":3,"interval_ticks":2,"index_as":"i","do":{"flow":"effect","effect":{"type":"resource","target":"$caster","resource":"mana","operation":"add","amount":1}}},{"flow":"finish"}]}`
	program, environment := compileRuntimeJSON(t, asyncSkillJSON("repeat", `{"type":"none"}`, flow))
	host := runtimeTestHost(environment)
	runtime := NewRuntime(host, RuntimeOptions{})
	castID, err := runtime.Activate(program, CastInput{Caster: 1})
	if err != nil || host.ResourceForTest(1, "mana") != 101 {
		t.Fatalf("initial repeat = %v mana=%d", err, host.ResourceForTest(1, "mana"))
	}
	if err := runtime.Advance(2); err != nil || host.ResourceForTest(1, "mana") != 102 {
		t.Fatalf("second repeat = %v mana=%d", err, host.ResourceForTest(1, "mana"))
	}
	if err := runtime.Advance(4); err != nil || host.ResourceForTest(1, "mana") != 103 {
		t.Fatalf("third repeat = %v mana=%d", err, host.ResourceForTest(1, "mana"))
	}
	if snapshot, _ := runtime.InspectCast(castID); snapshot.Status != CastFinished {
		t.Fatalf("cast = %#v", snapshot)
	}
}

func TestRuntimeSelectEachSuspensionRetainsIndependentLocalFrames(t *testing.T) {
	flow := `{"flow":"sequence","steps":[{"flow":"select","select":{"from":"$caster","kind":"entity","shape":{"type":"circle","radius":100},"filters":[{"type":"alive"},{"type":"not_caster"}],"order":{"by":"distance","direction":"asc"},"limit":2},"consume":{"mode":"each","as":"enemy","do":{"flow":"wait","ticks":2,"then":{"flow":"effect","effect":{"type":"damage","target":"$local.enemy","amount":1,"damage_type":"physical"}}}}},{"flow":"finish"}]}`
	program, environment := compileRuntimeJSON(t, asyncSkillJSON("select-frames", `{"type":"none"}`, flow))
	host := runtimeTestHost(environment)
	host.UpsertEntity(MemoryEntity{ID: 3, Alive: true, Health: 100, MaxHealth: 100, Position: Position{X: 1}})
	runtime := NewRuntime(host, RuntimeOptions{})
	castID, err := runtime.Activate(program, CastInput{Caster: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(runtime.frames) != 2 {
		t.Fatalf("retained frames = %d", len(runtime.frames))
	}
	if err := runtime.Advance(2); err != nil {
		t.Fatal(err)
	}
	if host.HealthForTest(2) != 99 || host.HealthForTest(3) != 99 {
		t.Fatalf("local frames aliased: h2=%d h3=%d", host.HealthForTest(2), host.HealthForTest(3))
	}
	if len(runtime.frames) != 0 {
		t.Fatalf("completed frames retained = %d", len(runtime.frames))
	}
	if snapshot, _ := runtime.InspectCast(castID); snapshot.Status != CastFinished {
		t.Fatalf("cast = %#v", snapshot)
	}
}

func TestRuntimeStalePhaseTaskReleasesFrameOnce(t *testing.T) {
	flow := `{"flow":"wait","ticks":3,"then":{"flow":"sequence","steps":[{"flow":"effect","effect":{"type":"damage","target":"$input.target","amount":1,"damage_type":"physical"}},{"flow":"finish"}]}}`
	program, environment := compileRuntimeJSON(t, asyncSkillJSON("stale", `{"type":"entity"}`, flow))
	host := runtimeTestHost(environment)
	runtime := NewRuntime(host, RuntimeOptions{})
	castID, err := runtime.Activate(program, CastInput{Caster: 1, Target: 2})
	if err != nil {
		t.Fatal(err)
	}
	cast := runtime.casts[castID]
	cast.phaseToken++
	cast.logicalFinished = true
	if err := runtime.Advance(3); err != nil {
		t.Fatal(err)
	}
	if host.HealthForTest(2) != 100 || len(runtime.frames) != 0 || cast.pendingTasks != 0 || cast.status != CastFinished {
		t.Fatalf("stale task was not cleanly discarded: health=%d frames=%d cast=%#v", host.HealthForTest(2), len(runtime.frames), cast)
	}
}

func compileRuntimeJSON(t *testing.T, input string) (*Program, CompileEnvironment) {
	t.Helper()
	environment := DefaultCompileEnvironment()
	program, diagnostics := Compile(mustParseJSON(t, input), environment)
	requireNoErrors(t, diagnostics)
	return program, environment
}

func asyncSkillJSON(id, input, flow string) string {
	return `{"schema":"roost.skill/v2","id":"skill.test.runtime.` + id + `","name":"Async","description":"Async runtime.","activation":{"type":"active","policy":{"mode":"tap"}},"input_schema":` + input + `,"cooldown_ticks":0,"costs":[],"memory":{},"initial_phase":"cast","phases":[{"id":"cast","timeout_ticks":0,"on":{"enter":` + flow + `}}]}`
}
