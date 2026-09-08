package skill

import (
	"errors"
	"reflect"
	"testing"
)

func TestInputSchemaVisibilityAndValidation(t *testing.T) {
	tests := []struct {
		name   string
		schema string
		slots  []string
	}{
		{"none", `{"type":"none"}`, []string{}},
		{"direction", `{"type":"direction"}`, []string{"$input.direction"}},
		{"position", `{"type":"position","maximum_range":10,"clamp_policy":"reject"}`, []string{"$input.position"}},
		{"entity", `{"type":"entity","maximum_range":10}`, []string{"$input.target"}},
		{"direction position", `{"type":"direction_position","maximum_range":10,"clamp_policy":"reject"}`, []string{"$input.direction", "$input.position"}},
		{"entity position", `{"type":"entity_position","maximum_range":10,"clamp_policy":"reject"}`, []string{"$input.position", "$input.target"}},
		{"two point", `{"type":"two_point","maximum_range":20,"minimum_length":2,"maximum_length":10,"clamp_policy":"clamp_end"}`, []string{"$input.end_position", "$input.start_position"}},
		{"drag", `{"type":"drag","maximum_range":20,"minimum_length":2,"maximum_length":10,"clamp_policy":"clamp_end"}`, []string{"$input.drag_direction", "$input.drag_length", "$input.end_position", "$input.start_position"}},
		{"path", `{"type":"path","maximum_points":4,"maximum_total_length":20,"minimum_segment_length":2,"simplification_policy":"drop_short_segments","clamp_policy":"reject"}`, []string{"$input.end_position", "$input.path", "$input.start_position"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			program := compileInputSkill(t, "schema-"+tickString(Tick(len(test.schema))), test.schema, `{"enter":{"flow":"finish"}}`, DefaultCompileEnvironment())
			view := InspectInputLayout(program)
			got := make([]string, len(view.Slots))
			for index, slot := range view.Slots {
				got[index] = slot.Name
			}
			if !reflect.DeepEqual(got, test.slots) {
				t.Fatalf("slots=%v want=%v", got, test.slots)
			}
		})
	}

	t.Run("none does not infer target", func(t *testing.T) {
		json := inputSkillJSON("none-reference", `{"type":"none"}`, `{"enter":{"flow":"effect","effect":{"type":"damage","target":"$input.target","amount":1,"damage_type":"physical"}}}`)
		_, diagnostics := Compile(mustParseJSON(t, json), DefaultCompileEnvironment())
		if !diagnosticsHaveErrors(diagnostics) {
			t.Fatal("expected unavailable input diagnostic")
		}
	})

	t.Run("none does not require a position read", func(t *testing.T) {
		program := compileInputSkill(t, "none-no-read", `{"type":"none"}`, `{"enter":{"flow":"finish"}}`, DefaultCompileEnvironment())
		memory := NewMemoryHost(program.AuthorityIdentity())
		memory.ConfigureGameplayCatalog(DefaultCompileEnvironment().Gameplay)
		host := &rejectInputReadHost{MemoryHost: memory}
		if _, err := NewRuntime(host, RuntimeOptions{}).Activate(program, CastInput{Caster: 1}); err != nil {
			t.Fatalf("none input unexpectedly read world position: %v", err)
		}
	})

	invalidSchemas := []string{
		`{"type":"position","maximum_range":-1,"clamp_policy":"reject"}`,
		`{"type":"position","maximum_range":10,"clamp_policy":"clamp_each_segment"}`,
		`{"type":"two_point","maximum_range":10,"minimum_length":8,"maximum_length":4,"clamp_policy":"reject"}`,
		`{"type":"path","maximum_points":1,"maximum_total_length":20,"minimum_segment_length":2,"simplification_policy":"drop_short_segments","clamp_policy":"reject"}`,
		`{"type":"path","maximum_points":4,"maximum_total_length":2,"minimum_segment_length":3,"simplification_policy":"reject","clamp_policy":"reject"}`,
		`{"type":"path","maximum_points":4,"maximum_total_length":20,"minimum_segment_length":2,"simplification_policy":"unknown","clamp_policy":"reject"}`,
	}
	for index, schema := range invalidSchemas {
		t.Run("invalid declaration "+tickString(Tick(index)), func(t *testing.T) {
			json := inputSkillJSON("invalid-"+tickString(Tick(index)), schema, `{"enter":{"flow":"finish"}}`)
			_, diagnostics := Compile(mustParseJSON(t, json), DefaultCompileEnvironment())
			if !diagnosticsHaveErrors(diagnostics) {
				t.Fatalf("expected input schema diagnostic for %s", schema)
			}
		})
	}

	t.Run("phase input ports require visible fields", func(t *testing.T) {
		json := inputSkillJSON("invalid-port", `{"type":"position"}`, `{"enter":{"flow":"finish"},"target_changed":{"flow":"finish"}}`)
		_, diagnostics := Compile(mustParseJSON(t, json), DefaultCompileEnvironment())
		if !diagnosticsHaveErrors(diagnostics) {
			t.Fatal("expected target_changed input compatibility diagnostic")
		}
		for _, diagnostic := range diagnostics {
			if diagnostic.Code == DiagnosticInputUnavailable {
				if diagnostic.Path != "$.input_schema" {
					t.Fatalf("diagnostic path=%q want=$.input_schema", diagnostic.Path)
				}
				return
			}
		}
		t.Fatal("missing INPUT_UNAVAILABLE diagnostic")
	})

	t.Run("inspector exposes complete advanced plans", func(t *testing.T) {
		path := compileInputSkill(t, "inspect-path", `{"type":"path","maximum_points":4,"maximum_total_length":20,"minimum_segment_length":2,"simplification_policy":"drop_short_segments","clamp_policy":"clamp_each_segment"}`, `{"enter":{"flow":"finish"}}`, DefaultCompileEnvironment())
		view := InspectInputLayout(path)
		if view.Kind != "path" || view.MaximumPathPoints != 4 || view.MaximumPathLength != 20 || view.MinimumSegmentLength != 2 || view.ClampPolicy != "clamp_each_segment" || view.SimplificationPolicy != "drop_short_segments" {
			t.Fatalf("path input view=%#v", view)
		}

		direction := compileInputSkill(t, "inspect-direction", `{"type":"direction"}`, `{"enter":{"flow":"finish"},"direction_changed":{"flow":"finish"}}`, DefaultCompileEnvironment())
		view = InspectInputLayout(direction)
		if !reflect.DeepEqual(view.UpdatePorts, []InputPort{InputPortDirectionChanged}) {
			t.Fatalf("update ports=%v", view.UpdatePorts)
		}
		view.UpdatePorts[0] = InputPortTargetChanged
		if got := InspectInputLayout(direction).UpdatePorts; !reflect.DeepEqual(got, []InputPort{InputPortDirectionChanged}) {
			t.Fatalf("inspector leaked mutable update ports: %v", got)
		}
	})

	t.Run("advanced input plan changes gameplay digest", func(t *testing.T) {
		first := compileInputSkill(t, "digest-input", `{"type":"path","maximum_points":4,"maximum_total_length":20,"minimum_segment_length":2,"simplification_policy":"reject","clamp_policy":"reject"}`, `{"enter":{"flow":"finish"}}`, DefaultCompileEnvironment())
		second := compileInputSkill(t, "digest-input", `{"type":"path","maximum_points":4,"maximum_total_length":21,"minimum_segment_length":2,"simplification_policy":"reject","clamp_policy":"reject"}`, `{"enter":{"flow":"finish"}}`, DefaultCompileEnvironment())
		if InspectIdentity(first).GameplayDigest == InspectIdentity(second).GameplayDigest {
			t.Fatal("maximum_total_length did not change gameplay digest")
		}
	})

	t.Run("path length consumes compile budget", func(t *testing.T) {
		environment := DefaultCompileEnvironment()
		environment.Limits.MaxInputPathLength = 9
		environment.Digest = authorityDigest(environment)
		json := inputSkillJSON("path-budget", `{"type":"path","maximum_points":4,"maximum_total_length":10,"minimum_segment_length":1,"simplification_policy":"reject","clamp_policy":"reject"}`, `{"enter":{"flow":"finish"}}`)
		_, diagnostics := Compile(mustParseJSON(t, json), environment)
		if !diagnosticsHaveErrors(diagnostics) {
			t.Fatal("expected input path length budget diagnostic")
		}
	})
}

func TestInputClampRangeDragAndBlockedPosition(t *testing.T) {
	environment := DefaultCompileEnvironment()
	t.Run("two point clamps end", func(t *testing.T) {
		program := compileInputSkill(t, "two-point-clamp", `{"type":"two_point","maximum_range":20,"minimum_length":2,"maximum_length":5,"clamp_policy":"clamp_end"}`, `{"enter":{"flow":"finish"}}`, environment)
		host := runtimeTestHost(environment)
		runtime := NewRuntime(host, RuntimeOptions{})
		start, end := Position{X: 0, Y: 0}, Position{X: 10, Y: 0}
		castID, err := runtime.Activate(program, CastInput{Caster: 1, StartPosition: &start, EndPosition: &end})
		if err != nil {
			t.Fatal(err)
		}
		got, ok := inputValueForTest(t, runtime, castID, "$input.end_position").Position()
		if !ok || got != (Position{X: 5, Y: 0}) {
			t.Fatalf("end=%#v present=%v", got, ok)
		}
	})

	t.Run("diagonal clamp corrects rounded point inward", func(t *testing.T) {
		program := compileInputSkill(t, "position-diagonal-clamp", `{"type":"position","maximum_range":5,"clamp_policy":"clamp_end"}`, `{"enter":{"flow":"finish"}}`, environment)
		host := runtimeTestHost(environment)
		runtime := NewRuntime(host, RuntimeOptions{})
		position := Position{X: -100, Y: -100}
		castID, err := runtime.Activate(program, CastInput{Caster: 1, Position: &position})
		if err != nil {
			t.Fatal(err)
		}
		got, ok := inputValueForTest(t, runtime, castID, "$input.position").Position()
		if !ok || got != (Position{X: -3, Y: -4}) {
			t.Fatalf("position=%#v present=%v", got, ok)
		}
	})

	t.Run("two point rejects below minimum", func(t *testing.T) {
		program := compileInputSkill(t, "two-point-min", `{"type":"two_point","maximum_range":20,"minimum_length":2,"maximum_length":5,"clamp_policy":"reject"}`, `{"enter":{"flow":"finish"}}`, environment)
		host := runtimeTestHost(environment)
		runtime := NewRuntime(host, RuntimeOptions{})
		start, end := Position{}, Position{X: 1}
		if _, err := runtime.Activate(program, CastInput{Caster: 1, StartPosition: &start, EndPosition: &end}); !errors.Is(err, ErrCastInputInvalid) {
			t.Fatalf("error=%v", err)
		}
	})

	t.Run("drag derives normalized direction and length", func(t *testing.T) {
		program := compileInputSkill(t, "drag-derived", `{"type":"drag","maximum_range":20,"minimum_length":1,"maximum_length":10,"clamp_policy":"reject"}`, `{"enter":{"flow":"finish"}}`, environment)
		host := runtimeTestHost(environment)
		runtime := NewRuntime(host, RuntimeOptions{})
		start, end := Position{}, Position{X: 3, Y: 4}
		castID, err := runtime.Activate(program, CastInput{Caster: 1, StartPosition: &start, EndPosition: &end})
		if err != nil {
			t.Fatal(err)
		}
		direction, directionOK := inputValueForTest(t, runtime, castID, "$input.drag_direction").Direction()
		length, lengthOK := inputValueForTest(t, runtime, castID, "$input.drag_length").Int()
		if !directionOK || direction != (Direction{X: 6000, Y: 8000}) || !lengthOK || length != 5 {
			t.Fatalf("direction=%#v/%v length=%d/%v", direction, directionOK, length, lengthOK)
		}
	})

	t.Run("position range reject and clamp", func(t *testing.T) {
		requested := Position{X: 10}
		reject := compileInputSkill(t, "position-reject", `{"type":"position","maximum_range":5,"clamp_policy":"reject"}`, `{"enter":{"flow":"finish"}}`, environment)
		host := runtimeTestHost(environment)
		if _, err := NewRuntime(host, RuntimeOptions{}).Activate(reject, CastInput{Caster: 1, Position: &requested}); !errors.Is(err, ErrCastInputInvalid) {
			t.Fatalf("reject error=%v", err)
		}
		clamp := compileInputSkill(t, "position-clamp", `{"type":"position","maximum_range":5,"clamp_policy":"clamp_end"}`, `{"enter":{"flow":"finish"}}`, environment)
		runtime := NewRuntime(host, RuntimeOptions{})
		castID, err := runtime.Activate(clamp, CastInput{Caster: 1, Position: &requested})
		if err != nil {
			t.Fatal(err)
		}
		got, _ := inputValueForTest(t, runtime, castID, "$input.position").Position()
		if got != (Position{X: 5}) {
			t.Fatalf("clamped position=%#v", got)
		}
	})

	t.Run("nearest valid delegates blocked world fact to host", func(t *testing.T) {
		program := compileInputSkill(t, "nearest-valid", `{"type":"position","maximum_range":10,"clamp_policy":"nearest_valid"}`, `{"enter":{"flow":"finish"}}`, environment)
		memory := runtimeTestHost(environment)
		host := &resolvingInputHost{MemoryHost: memory, blocked: map[Position]Position{{X: 5}: {X: 4}}}
		runtime := NewRuntime(host, RuntimeOptions{})
		requested := Position{X: 5}
		castID, err := runtime.Activate(program, CastInput{Caster: 1, Position: &requested})
		if err != nil {
			t.Fatal(err)
		}
		got, _ := inputValueForTest(t, runtime, castID, "$input.position").Position()
		if got != (Position{X: 4}) {
			t.Fatalf("resolved position=%#v", got)
		}
	})

	t.Run("position without range does not read caster position", func(t *testing.T) {
		program := compileInputSkill(t, "position-no-range", `{"type":"position","clamp_policy":"reject"}`, `{"enter":{"flow":"finish"}}`, environment)
		memory := NewMemoryHost(program.AuthorityIdentity())
		memory.ConfigureGameplayCatalog(environment.Gameplay)
		runtime := NewRuntime(&rejectInputReadHost{MemoryHost: memory}, RuntimeOptions{})
		position := Position{X: 5}
		if _, err := runtime.Activate(program, CastInput{Caster: 1, Position: &position}); err != nil {
			t.Fatalf("unranged position unexpectedly read caster: %v", err)
		}
	})

	t.Run("two point revalidates host-adjusted clamped end", func(t *testing.T) {
		program := compileInputSkill(t, "two-point-resolved", `{"type":"two_point","maximum_range":20,"minimum_length":2,"maximum_length":5,"clamp_policy":"nearest_valid"}`, `{"enter":{"flow":"finish"}}`, environment)
		start, end := Position{}, Position{X: 10}
		for name, resolved := range map[string]Position{"above maximum": {X: 8}, "below minimum": {X: 1}} {
			t.Run(name, func(t *testing.T) {
				memory := runtimeTestHost(environment)
				host := &resolvingInputHost{MemoryHost: memory, blocked: map[Position]Position{{X: 5}: resolved}}
				if _, err := NewRuntime(host, RuntimeOptions{}).Activate(program, CastInput{Caster: 1, StartPosition: &start, EndPosition: &end}); !errors.Is(err, ErrCastInputInvalid) {
					t.Fatalf("error=%v", err)
				}
			})
		}
	})

	t.Run("two point rejects host-adjusted end outside caster range", func(t *testing.T) {
		program := compileInputSkill(t, "two-point-resolved-range", `{"type":"two_point","maximum_range":10,"minimum_length":2,"maximum_length":5,"clamp_policy":"nearest_valid"}`, `{"enter":{"flow":"finish"}}`, environment)
		start, end := Position{X: 10}, Position{X: -10}
		memory := runtimeTestHost(environment)
		host := &resolvingInputHost{MemoryHost: memory, blocked: map[Position]Position{{X: 5}: {X: 15}}}
		if _, err := NewRuntime(host, RuntimeOptions{}).Activate(program, CastInput{Caster: 1, StartPosition: &start, EndPosition: &end}); !errors.Is(err, ErrCastInputInvalid) {
			t.Fatalf("error=%v", err)
		}
	})

	t.Run("extreme coordinates clamp with exact distance", func(t *testing.T) {
		maximum := int64(^uint64(0) >> 1)
		schema := `{"type":"two_point","maximum_range":` + tickString(Tick(maximum)) + `,"minimum_length":1,"maximum_length":` + tickString(Tick(maximum)) + `,"clamp_policy":"clamp_end"}`
		program := compileInputSkill(t, "two-point-extreme", schema, `{"enter":{"flow":"finish"}}`, environment)
		host := runtimeTestHost(environment)
		runtime := NewRuntime(host, RuntimeOptions{})
		start, end := Position{X: -maximum - 1}, Position{X: maximum}
		castID, err := runtime.Activate(program, CastInput{Caster: 1, StartPosition: &start, EndPosition: &end})
		if err != nil {
			t.Fatal(err)
		}
		gotStart, _ := inputValueForTest(t, runtime, castID, "$input.start_position").Position()
		gotEnd, _ := inputValueForTest(t, runtime, castID, "$input.end_position").Position()
		if gotStart != (Position{X: -maximum}) || gotEnd != (Position{}) {
			t.Fatalf("start=%#v end=%#v", gotStart, gotEnd)
		}
	})
}

func TestInputPathBoundsSimplificationAndSnapshot(t *testing.T) {
	environment := DefaultCompileEnvironment()
	compilePath := func(t *testing.T, id, simplification, clamp string, points int, length int64) *Program {
		t.Helper()
		schema := `{"type":"path","maximum_points":` + tickString(Tick(points)) + `,"maximum_total_length":` + tickString(Tick(length)) + `,"minimum_segment_length":2,"simplification_policy":"` + simplification + `","clamp_policy":"` + clamp + `"}`
		return compileInputSkill(t, id, schema, `{"enter":{"flow":"finish"}}`, environment)
	}

	t.Run("drops short segments and freezes source slice", func(t *testing.T) {
		program := compilePath(t, "path-simplify", "drop_short_segments", "reject", 4, 20)
		host := runtimeTestHost(environment)
		runtime := NewRuntime(host, RuntimeOptions{})
		path := []Position{{}, {X: 1}, {X: 4}}
		castID, err := runtime.Activate(program, CastInput{Caster: 1, Path: path})
		if err != nil {
			t.Fatal(err)
		}
		path[0].X = 99
		got, ok := inputValueForTest(t, runtime, castID, "$input.path").Path()
		want := []Position{{}, {X: 4}}
		if !ok || !reflect.DeepEqual(got, want) {
			t.Fatalf("path=%#v present=%v want=%#v", got, ok, want)
		}
		start, _ := inputValueForTest(t, runtime, castID, "$input.start_position").Position()
		end, _ := inputValueForTest(t, runtime, castID, "$input.end_position").Position()
		if start != want[0] || end != want[1] {
			t.Fatalf("start=%#v end=%#v", start, end)
		}
	})

	t.Run("rejects short segment under reject policy", func(t *testing.T) {
		program := compilePath(t, "path-short-reject", "reject", "reject", 4, 20)
		host := runtimeTestHost(environment)
		path := []Position{{}, {X: 1}, {X: 4}}
		if _, err := NewRuntime(host, RuntimeOptions{}).Activate(program, CastInput{Caster: 1, Path: path}); !errors.Is(err, ErrCastInputInvalid) {
			t.Fatalf("error=%v", err)
		}
	})

	t.Run("rejects raw point count before simplification", func(t *testing.T) {
		program := compilePath(t, "path-points", "drop_short_segments", "reject", 3, 20)
		host := runtimeTestHost(environment)
		path := []Position{{}, {X: 1}, {X: 2}, {X: 5}}
		if _, err := NewRuntime(host, RuntimeOptions{}).Activate(program, CastInput{Caster: 1, Path: path}); !errors.Is(err, ErrCastInputInvalid) {
			t.Fatalf("error=%v", err)
		}
	})

	t.Run("rejects or clamps total length deterministically", func(t *testing.T) {
		path := []Position{{}, {X: 6}, {X: 12}}
		reject := compilePath(t, "path-total-reject", "reject", "reject", 4, 10)
		host := runtimeTestHost(environment)
		if _, err := NewRuntime(host, RuntimeOptions{}).Activate(reject, CastInput{Caster: 1, Path: path}); !errors.Is(err, ErrCastInputInvalid) {
			t.Fatalf("reject error=%v", err)
		}
		clamp := compilePath(t, "path-total-clamp", "reject", "clamp_end", 4, 10)
		runtime := NewRuntime(host, RuntimeOptions{})
		castID, err := runtime.Activate(clamp, CastInput{Caster: 1, Path: path})
		if err != nil {
			t.Fatal(err)
		}
		got, _ := inputValueForTest(t, runtime, castID, "$input.path").Path()
		want := []Position{{}, {X: 6}, {X: 10}}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("path=%#v want=%#v", got, want)
		}
	})

	t.Run("uses stable integer segment length", func(t *testing.T) {
		program := compilePath(t, "path-integer-length", "reject", "reject", 4, 10)
		host := runtimeTestHost(environment)
		path := []Position{{}, {X: 3, Y: 4}, {X: 6, Y: 8}}
		if _, err := NewRuntime(host, RuntimeOptions{}).Activate(program, CastInput{Caster: 1, Path: path}); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("terminal resolver cannot create a short segment", func(t *testing.T) {
		program := compilePath(t, "path-terminal-short", "reject", "nearest_valid", 4, 10)
		memory := runtimeTestHost(environment)
		host := &resolvingInputHost{MemoryHost: memory, blocked: map[Position]Position{{X: 10}: {X: 7}}}
		path := []Position{{}, {X: 6}, {X: 12}}
		if _, err := NewRuntime(host, RuntimeOptions{}).Activate(program, CastInput{Caster: 1, Path: path}); !errors.Is(err, ErrCastInputInvalid) {
			t.Fatalf("error=%v", err)
		}
	})

	t.Run("clamp each segment resolves each path point", func(t *testing.T) {
		program := compilePath(t, "path-clamp-each", "reject", "clamp_each_segment", 4, 20)
		memory := runtimeTestHost(environment)
		host := &resolvingInputHost{MemoryHost: memory, blocked: map[Position]Position{{X: 6}: {X: 5}, {X: 12}: {X: 10}}}
		runtime := NewRuntime(host, RuntimeOptions{})
		castID, err := runtime.Activate(program, CastInput{Caster: 1, Path: []Position{{}, {X: 6}, {X: 12}}})
		if err != nil {
			t.Fatal(err)
		}
		got, _ := inputValueForTest(t, runtime, castID, "$input.path").Path()
		want := []Position{{}, {X: 5}, {X: 10}}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("path=%#v want=%#v", got, want)
		}
	})
}

func TestInputUpdateReplacesOnlyPortFieldAndPreservesCommitState(t *testing.T) {
	environment := DefaultCompileEnvironment()
	t.Run("target changed is atomic and does not repay cost", func(t *testing.T) {
		schema := `{"type":"entity_position","maximum_range":100,"clamp_policy":"reject"}`
		events := `{"enter":{"flow":"effect","effect":{"type":"damage","target":"$input.target","amount":1,"damage_type":"physical"}},"target_changed":{"flow":"effect","effect":{"type":"damage","target":"$input.target","amount":2,"damage_type":"physical"}},"pulse":{"flow":"effect","effect":{"type":"set_memory","name":"counter","value":0}},"release":{"flow":"finish"}}`
		program := compileInputUpdateSkill(t, "target", schema, events, 10, `[{"resource":"mana","amount":5}]`)
		host := runtimeTestHost(environment)
		host.UpsertEntity(MemoryEntity{ID: 3, Alive: true, Health: 100, MaxHealth: 100})
		runtime := NewRuntime(host, RuntimeOptions{})
		position := Position{X: 1}
		castID, err := runtime.Activate(program, CastInput{Caster: 1, Target: 2, Position: &position})
		if err != nil {
			t.Fatal(err)
		}
		if host.ResourceForTest(1, "mana") != 95 || host.HealthForTest(2) != 99 || runtime.CooldownUntil(program, 1) != 0 {
			t.Fatal("activation commit state mismatch")
		}
		extra := Position{X: 7}
		if err := runtime.Input(castID, InputPortTargetChanged, InputPayload{Target: 3, Position: &extra}); !errors.Is(err, ErrCastInputInvalid) {
			t.Fatalf("extraneous field error=%v", err)
		}
		if current, _ := inputValueForTest(t, runtime, castID, "$input.target").Entity(); current != 2 || host.HealthForTest(3) != 100 {
			t.Fatal("failed update changed snapshot or executed root")
		}
		if err := runtime.Input(castID, InputPortTargetChanged, InputPayload{Target: 3}); err != nil {
			t.Fatal(err)
		}
		current, _ := inputValueForTest(t, runtime, castID, "$input.target").Entity()
		fixedPosition, _ := inputValueForTest(t, runtime, castID, "$input.position").Position()
		if current != 3 || fixedPosition != position || host.HealthForTest(3) != 98 {
			t.Fatalf("target=%d position=%#v health3=%d", current, fixedPosition, host.HealthForTest(3))
		}
		if host.ResourceForTest(1, "mana") != 95 || runtime.CooldownUntil(program, 1) != 0 {
			t.Fatal("input update changed cost or cooldown")
		}
		if err := runtime.Release(castID); err != nil {
			t.Fatal(err)
		}
		if runtime.CooldownUntil(program, 1) != 10 {
			t.Fatalf("cooldown=%d", runtime.CooldownUntil(program, 1))
		}
		if err := runtime.Input(castID, InputPortTargetChanged, InputPayload{Target: 2}); !errors.Is(err, ErrCastInputRejected) {
			t.Fatalf("finished update error=%v", err)
		}
	})

	t.Run("direction changed preserves position and rejects undeclared port", func(t *testing.T) {
		schema := `{"type":"direction_position","maximum_range":100,"clamp_policy":"reject"}`
		events := `{"enter":{"flow":"effect","effect":{"type":"set_memory","name":"counter","value":0}},"direction_changed":{"flow":"effect","effect":{"type":"add_memory","name":"counter","value":1}},"pulse":{"flow":"effect","effect":{"type":"set_memory","name":"counter","value":0}},"release":{"flow":"finish"}}`
		program := compileInputUpdateSkill(t, "direction", schema, events, 0, `[]`)
		host := runtimeTestHost(environment)
		runtime := NewRuntime(host, RuntimeOptions{})
		position, direction := Position{X: 2}, Direction{X: 1}
		castID, err := runtime.Activate(program, CastInput{Caster: 1, Position: &position, Direction: &direction})
		if err != nil {
			t.Fatal(err)
		}
		updated := Direction{Y: 1}
		if err := runtime.Input(castID, InputPortDirectionChanged, InputPayload{Direction: &updated}); err != nil {
			t.Fatal(err)
		}
		gotDirection, _ := inputValueForTest(t, runtime, castID, "$input.direction").Direction()
		gotPosition, _ := inputValueForTest(t, runtime, castID, "$input.position").Position()
		counter, _ := runtime.casts[castID].memory[0].Int()
		if gotDirection != updated || gotPosition != position || counter != 1 {
			t.Fatalf("direction=%#v position=%#v counter=%d", gotDirection, gotPosition, counter)
		}
		if err := runtime.Input(castID, InputPortTargetChanged, InputPayload{Target: 2}); !errors.Is(err, ErrCastInputRejected) {
			t.Fatalf("undeclared port error=%v", err)
		}
	})

	t.Run("target update without range does not require world position", func(t *testing.T) {
		schema := `{"type":"entity"}`
		events := `{"enter":{"flow":"effect","effect":{"type":"set_memory","name":"counter","value":0}},"target_changed":{"flow":"effect","effect":{"type":"add_memory","name":"counter","value":1}},"pulse":{"flow":"effect","effect":{"type":"set_memory","name":"counter","value":0}},"release":{"flow":"finish"}}`
		program := compileInputUpdateSkill(t, "target-no-range", schema, events, 0, `[]`)
		memory := runtimeTestHost(environment)
		runtime := NewRuntime(&rejectInputReadHost{MemoryHost: memory}, RuntimeOptions{})
		castID, err := runtime.Activate(program, CastInput{Caster: 1, Target: 2})
		if err != nil {
			t.Fatal(err)
		}
		if err := runtime.Input(castID, InputPortTargetChanged, InputPayload{Target: 999}); err != nil {
			t.Fatalf("unranged target update read world position: %v", err)
		}
		current, _ := inputValueForTest(t, runtime, castID, "$input.target").Entity()
		counter, _ := runtime.casts[castID].memory[0].Int()
		if current != 999 || counter != 1 {
			t.Fatalf("target=%d counter=%d", current, counter)
		}
	})

	t.Run("failed cast rejects input", func(t *testing.T) {
		schema := `{"type":"direction"}`
		events := `{"enter":{"flow":"effect","effect":{"type":"set_memory","name":"counter","value":0}},"direction_changed":{"flow":"effect","effect":{"type":"add_memory","name":"counter","value":1}},"pulse":{"flow":"effect","effect":{"type":"damage","target":"$caster","amount":1,"damage_type":"physical"}},"release":{"flow":"finish"}}`
		program := compileInputUpdateSkill(t, "failed-cast", schema, events, 0, `[]`)
		memory := runtimeTestHost(environment)
		runtime := NewRuntime(&effectResultHost{MemoryHost: memory, err: ErrHostContractViolation}, RuntimeOptions{})
		direction := Direction{X: 1}
		castID, err := runtime.Activate(program, CastInput{Caster: 1, Direction: &direction})
		if err != nil {
			t.Fatal(err)
		}
		if err := runtime.Advance(50); !errors.Is(err, ErrHostContractViolation) {
			t.Fatalf("pulse error=%v", err)
		}
		updated := Direction{Y: 1}
		if err := runtime.Input(castID, InputPortDirectionChanged, InputPayload{Direction: &updated}); !errors.Is(err, ErrCastInputRejected) {
			t.Fatalf("failed cast input error=%v", err)
		}
	})
}

func compileInputUpdateSkill(t *testing.T, id, schema, events string, cooldown Tick, costs string) *Program {
	t.Helper()
	policy := `{"mode":"hold","pulse_interval_ticks":50,"max_duration_ticks":100,"sustain_costs":[]}`
	json := `{"schema":"roost.skill/v2","id":"skill.test.input.update.` + id + `","name":"Input Update","description":"Immutable input update.","gameplay_tags":["spell"],"activation":{"type":"active","policy":` + policy + `},"input_schema":` + schema + `,"cooldown_ticks":` + tickString(cooldown) + `,"costs":` + costs + `,"memory":{"counter":{"type":"int","default":0}},"initial_phase":"cast","phases":[{"id":"cast","timeout_ticks":0,"on":` + events + `}]}`
	program, diagnostics := Compile(mustParseJSON(t, json), DefaultCompileEnvironment())
	requireNoErrors(t, diagnostics)
	return program
}

type resolvingInputHost struct {
	*MemoryHost
	blocked map[Position]Position
}

type rejectInputReadHost struct{ *MemoryHost }

func (host *rejectInputReadHost) Read(ReadRequest) (ReadResult, error) {
	return ReadResult{}, ErrEntityNotFound
}

func (host *resolvingInputHost) ResolveInputPosition(request InputPositionRequest) (Position, bool) {
	resolved, blocked := host.blocked[request.Position]
	if !blocked {
		return request.Position, true
	}
	if request.Policy == "nearest_valid" || request.Policy == "clamp_each_segment" {
		return resolved, true
	}
	return Position{}, false
}

func inputValueForTest(t *testing.T, runtime *Runtime, castID CastID, name string) RuntimeValue {
	t.Helper()
	cast := runtime.casts[castID]
	for index, slot := range cast.program.input.slots {
		if slot.name == name {
			return cloneRuntimeValue(cast.inputs[index])
		}
	}
	t.Fatalf("input slot %s not found", name)
	return RuntimeValue{}
}

func compileInputSkill(t *testing.T, id, schema, events string, environment CompileEnvironment) *Program {
	t.Helper()
	program, diagnostics := Compile(mustParseJSON(t, inputSkillJSON(id, schema, events)), environment)
	requireNoErrors(t, diagnostics)
	return program
}

func inputSkillJSON(id, schema, events string) string {
	return `{"schema":"roost.skill/v2","id":"skill.test.input.` + id + `","name":"Input","description":"Typed cast input.","gameplay_tags":["spell"],"activation":{"type":"active","policy":{"mode":"tap"}},"input_schema":` + schema + `,"cooldown_ticks":0,"costs":[],"memory":{},"initial_phase":"cast","phases":[{"id":"cast","timeout_ticks":0,"on":` + events + `}]}`
}
