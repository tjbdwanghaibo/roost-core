package skill

import (
	"bytes"
	"encoding/json"
	"os"
	"sort"
	"testing"
)

type fixtureCase struct {
	name          string
	input         CastInput
	advanceTicks  []Tick
	release       bool
	passive       bool
	passiveResult string
	expectedCast  CastStatus
}

func TestAllFixturesParseCompileInspectAndRun(t *testing.T) {
	cases := discoveredFixtureCases(t)
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			environment := DefaultCompileEnvironment()
			program, diagnostics := Compile(mustParseFixture(t, test.name), environment)
			requireNoErrors(t, diagnostics)
			if view := Inspect(program); view.ID == "" || len(view.Operations) == 0 {
				t.Fatalf("invalid program inspection: %#v", view)
			}
			host := runtimeTestHost(environment)
			runtime := NewRuntime(host, RuntimeOptions{MatchSeed: fixedTestSeed(23)})
			if test.passive {
				if _, err := runtime.ActivatePassive(program, EventContext{EventID: 1, RootEventID: 1, Source: 2, Owner: 1, Target: 1, Result: test.passiveResult}); err != nil {
					t.Fatal(err)
				}
				if err := runtime.Advance(0); err != nil {
					t.Fatal(err)
				}
				assertRuntimeCheckpointRoundTrip(t, runtime, host, program)
				return
			}
			castID, err := runtime.Activate(program, test.input)
			if err != nil {
				t.Fatal(err)
			}
			for _, tick := range test.advanceTicks {
				if err := runtime.Advance(tick); err != nil {
					t.Fatal(err)
				}
			}
			if test.release {
				if err := runtime.Release(castID); err != nil {
					t.Fatal(err)
				}
			}
			cast, found := runtime.InspectCast(castID)
			if !found || cast.Status != test.expectedCast {
				t.Fatalf("cast = %#v, found=%v, want %s", cast, found, test.expectedCast)
			}
			assertRuntimeCheckpointRoundTrip(t, runtime, host, program)
		})
	}
}

func assertRuntimeCheckpointRoundTrip(t *testing.T, runtime *Runtime, host Host, program *Program) {
	t.Helper()
	checkpoint, err := runtime.Checkpoint()
	if err != nil {
		runtime.mutex.Lock()
		processes, owned := len(runtime.processes), len(runtime.ownedProcesses)
		processPrograms := make(map[ProcessID]bool, len(runtime.processes))
		for id, process := range runtime.processes {
			processPrograms[id] = process != nil && process.Program != nil
		}
		tasks := append(taskHeap(nil), runtime.scheduler.tasks...)
		baseline, _ := json.Marshal(runtime.stateMutationBaseline)
		current, _ := json.Marshal(runtime.stateSnapshotLocked())
		runtime.mutex.Unlock()
		t.Fatalf("%v; processes=%d owned=%d programs=%v tasks=%+v\nbaseline=%s\ncurrent=%s", err, processes, owned, processPrograms, tasks, baseline, current)
	}
	restored, err := RestoreRuntime(host, RuntimeOptions{}, checkpoint, ProgramResolverFunc(func(id, digest string) (*Program, error) {
		if id == program.id && digest == program.identity.gameplayDigest {
			return program, nil
		}
		return nil, ErrCheckpointProgram
	}))
	if err != nil {
		var payload runtimeCheckpointPayload
		_ = json.Unmarshal(checkpoint.Payload, &payload)
		t.Fatalf("%v; tasks=%+v", err, payload.Tasks)
	}
	want, err := json.Marshal(runtime.StateSnapshot())
	if err != nil {
		t.Fatal(err)
	}
	got, err := json.Marshal(restored.StateSnapshot())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("checkpoint snapshot mismatch\nwant=%s\ngot=%s", want, got)
	}
}

func discoveredFixtureCases(t *testing.T) []fixtureCase {
	t.Helper()
	config := map[string]fixtureCase{
		"simple_damage.json":              {input: CastInput{Caster: 1, Target: 2}, expectedCast: CastFinished},
		"area_heal.json":                  {input: CastInput{Caster: 1}, advanceTicks: []Tick{3}, expectedCast: CastFinished},
		"chain_lightning.json":            {input: CastInput{Caster: 1, Target: 2}, expectedCast: CastFinished},
		"dynamic_numeric.json":            {input: CastInput{Caster: 1}, advanceTicks: []Tick{3}, expectedCast: CastFinished},
		"toggle_aura.json":                {input: CastInput{Caster: 1, Target: 2}, advanceTicks: []Tick{2}, release: true, expectedCast: CastFinished},
		"hold_beam.json":                  {input: CastInput{Caster: 1, Target: 2}, advanceTicks: []Tick{2}, release: true, expectedCast: CastFinished},
		"charge_projectile.json":          {input: CastInput{Caster: 1, Target: 2}, advanceTicks: []Tick{10}, expectedCast: CastFinished},
		"ammo_burst.json":                 {input: CastInput{Caster: 1, Target: 2}, expectedCast: CastFinished},
		"cast_window_interrupt.json":      {input: CastInput{Caster: 1, Target: 2}, advanceTicks: []Tick{9}, expectedCast: CastFinished},
		"visual_projectile.json":          {input: CastInput{Caster: 1, Target: 2}, expectedCast: CastFinished},
		"elemental_area_damage.json":      {input: CastInput{Caster: 1, Target: 2}, expectedCast: CastFinished},
		"persistent_mark.json":            {input: CastInput{Caster: 1, Target: 2}, expectedCast: CastFinished},
		"owned_trap.json":                 {input: CastInput{Caster: 1}, expectedCast: CastFinished},
		"temporal_rewind.json":            {input: CastInput{Caster: 1}, expectedCast: CastFinished},
		"carry_dash.json":                 {input: CastInput{Caster: 1}, expectedCast: CastFinished},
		"tracking_boomerang.json":         {input: CastInput{Caster: 1}, expectedCast: CastFinished},
		"path_projectile.json":            {input: CastInput{Caster: 1, Path: []Position{{}, {X: 4}, {X: 8}}}, expectedCast: CastFinished},
		"recast_combo.json":               {input: CastInput{Caster: 1, Target: 2}, expectedCast: CastFinished},
		"beam.json":                       {input: CastInput{Caster: 1, Target: 2}, expectedCast: CastFinished},
		"projectile_area.json":            {input: CastInput{Caster: 1}, expectedCast: CastFinished},
		"heroic_swing.json":               {input: CastInput{Caster: 1, Target: 2}, expectedCast: CastFinished},
		"passive_counter.json":            {passive: true},
		"attribute_scaling_snapshot.json": {input: CastInput{Caster: 1, Target: 2}, expectedCast: CastFinished},
		"passive_proc_guard.json":         {passive: true},
		"area_membership.json":            {input: CastInput{Caster: 1}, advanceTicks: []Tick{2}, expectedCast: CastFinished},
		"status_modifier.json":            {input: CastInput{Caster: 1, Target: 2}, expectedCast: CastFinished},
		"shared_state_combo.json":         {input: CastInput{Caster: 1}, expectedCast: CastFinished},
		"cooldown_refund.json":            {input: CastInput{Caster: 1}, expectedCast: CastFinished},
		"ammo_on_kill.json":               {passive: true, passiveResult: "kill"},
		"ability_disable.json":            {input: CastInput{Caster: 1}, expectedCast: CastFinished},
		"owned_pet_command.json":          {input: CastInput{Caster: 1}, expectedCast: CastFinished},
		"entity_scoped_aura.json":         {input: CastInput{Caster: 1}, expectedCast: CastFinished},
		"status_cleanse.json":             {input: CastInput{Caster: 1, Target: 2}, expectedCast: CastFinished},
		"status_steal.json":               {input: CastInput{Caster: 1, Target: 2}, expectedCast: CastFinished},
		"effect_result_kill_branch.json":  {input: CastInput{Caster: 1, Target: 2}, expectedCast: CastFinished},
		"two_point_wall.json":             {input: CastInput{Caster: 1, StartPosition: positionPointer(Position{}), EndPosition: positionPointer(Position{X: 4})}, expectedCast: CastFinished},
		"portal_pair.json":                {input: CastInput{Caster: 1, StartPosition: positionPointer(Position{}), EndPosition: positionPointer(Position{X: 4})}, expectedCast: CastFinished},
	}
	entries, err := os.ReadDir("testdata")
	if err != nil {
		t.Fatal(err)
	}
	result := make([]fixtureCase, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || entry.Name()[len(entry.Name())-5:] != ".json" {
			continue
		}
		fixture, found := config[entry.Name()]
		if !found {
			t.Fatalf("fixture %s needs an explicit runtime acceptance configuration", entry.Name())
		}
		fixture.name = entry.Name()
		result = append(result, fixture)
	}
	sort.Slice(result, func(left, right int) bool { return result[left].name < result[right].name })
	return result
}

func positionPointer(position Position) *Position { return &position }
