package skill

import (
	"reflect"
	"testing"
)

func TestTemporalProfileCompileRules(t *testing.T) {
	environment := DefaultCompileEnvironment()
	capture := `{"flow":"effect","effect":{"type":"capture_snapshot","target":"$caster","profile":"temporal.position_health"},"result":{"as":"snapshot","success":{"flow":"finish"},"failure":{"flow":"finish"}}}`
	program, diagnostics := Compile(mustParseJSON(t, inputSkillJSON("temporal-capture", `{"type":"none"}`, `{"enter":`+capture+`}`)), environment)
	requireNoErrors(t, diagnostics)
	view := Inspect(program)
	if len(view.Operations) == 0 || view.Operations[0].Kind != "capture_snapshot" {
		t.Fatalf("operations=%#v", view.Operations)
	}

	unknown := `{"flow":"effect","effect":{"type":"capture_snapshot","target":"$caster","profile":"temporal.unknown"}}`
	_, diagnostics = Compile(mustParseJSON(t, inputSkillJSON("temporal-unknown", `{"type":"none"}`, `{"enter":`+unknown+`}`)), environment)
	requireDiagnostic(t, diagnostics, DiagnosticCapabilityUnknown)

	restoreWrongType := `{"flow":"effect","effect":{"type":"restore_snapshot","target":"$caster","snapshot":"$caster"}}`
	_, diagnostics = Compile(mustParseJSON(t, inputSkillJSON("temporal-wrong-token", `{"type":"none"}`, `{"enter":`+restoreWrongType+`}`)), environment)
	requireDiagnostic(t, diagnostics, DiagnosticTypeMismatch)

	restoreWrongPolicy := `{"flow":"effect","effect":{"type":"restore_snapshot","target":"$caster","snapshot":{"read_state":{"state":"token","owner":"$caster","snapshot":"current"}},"on_blocked":"warp"}}`
	json := `{"schema":"roost.skill/v2","id":"skill.test.temporal-policy","name":"Temporal","description":"Temporal profile.","gameplay_tags":["spell"],"activation":{"type":"active","policy":{"mode":"tap"}},"input_schema":{"type":"none"},"cooldown_ticks":0,"costs":[],"memory":{},"persistent_state":{"token":{"type":"snapshot_token","scope":"owner","default":null,"lifetime":{"duration_ticks":1,"maximum_duration_ticks":1,"on_write":"refresh","clear_on":[]}}},"initial_phase":"cast","phases":[{"id":"cast","timeout_ticks":0,"on":{"enter":` + restoreWrongPolicy + `}}]}`
	_, diagnostics = Compile(mustParseJSON(t, json), environment)
	requireDiagnostic(t, diagnostics, DiagnosticShapeInvalid)
}

func TestTemporalCatalogRejectsDuplicateProfileKey(t *testing.T) {
	environment := DefaultCompileEnvironment()
	environment.Gameplay.Temporal.Entries = append(environment.Gameplay.Temporal.Entries, TemporalSnapshotProfile{Handle: 2, Key: "temporal.position_health", Fields: []string{"health"}, MaximumAgeTicks: 10, MaximumPerOwner: 1, RestorePolicy: "authorized_fields", EventPolicy: "temporal_only", BlockedPositionPolicy: "fail"})
	environment.Digest = authorityDigest(environment)
	requireDiagnostic(t, validateCompileEnvironment(environment), DiagnosticCatalogTemporalPolicy)
}

func TestTemporalCaptureFreezesProfileAuthorization(t *testing.T) {
	environment := DefaultCompileEnvironment()
	environment.Gameplay.Temporal.Entries = []TemporalSnapshotProfile{{Handle: 1, Key: "temporal.frozen", Fields: []string{"health"}, MaximumAgeTicks: 20, MaximumPerOwner: 4, RestorePolicy: "authorized_fields", EventPolicy: "temporal_only", BlockedPositionPolicy: "fail"}}
	environment.Digest = authorityDigest(environment)
	host := runtimeTestHost(environment)
	host.UpsertEntity(MemoryEntity{ID: 1, Alive: true, Position: Position{X: 1}, Health: 80, MaxHealth: 100})
	captured, err := host.Apply(EffectCommand{Meta: CommandMeta{RequiredRevision: host.CurrentRevision()}, Payload: TemporalCaptureCommand{Owner: 1, Target: 1, ProgramID: "skill.temporal", GameplayDigest: environment.Digest, Profile: 1}})
	if err != nil {
		t.Fatal(err)
	}

	broadened := environment.Gameplay
	broadened.Temporal.Entries[0].Fields = []string{"position", "health"}
	host.ConfigureGameplayCatalog(broadened)
	host.UpsertEntity(MemoryEntity{ID: 1, Alive: true, Position: Position{X: 9}, Health: 10, MaxHealth: 100})
	result, err := host.Apply(EffectCommand{Meta: CommandMeta{RequiredRevision: host.CurrentRevision()}, Payload: TemporalRestoreCommand{Owner: 1, Target: 1, ProgramID: "skill.temporal", GameplayDigest: environment.Digest, Token: captured.Payload.(SnapshotCaptureEffectResult).Token}})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Payload.(SnapshotRestoreEffectResult).Succeeded || host.entities[1].Position != (Position{X: 9}) || host.entities[1].Health != 80 {
		t.Fatalf("restore=%#v entity=%#v", result, host.entities[1])
	}
}

func TestTemporalResultLayoutsAcceptHostExpectedFailures(t *testing.T) {
	cases := []struct {
		name    string
		layout  resultLayoutProgram
		payload EffectResultPayload
		failure ExpectedFailureReason
	}{
		{name: "capture invalid target", layout: resultLayoutByType(resultTypeSnapshotCapture, valueType{}), payload: SnapshotCaptureEffectResult{ResultOutcome: failedResultOutcome(ExpectedFailureInvalidTarget)}, failure: ExpectedFailureInvalidTarget},
		{name: "restore invalid target", layout: resultLayoutByType(resultTypeSnapshotRestore, valueType{}), payload: SnapshotRestoreEffectResult{ResultOutcome: failedResultOutcome(ExpectedFailureInvalidTarget)}, failure: ExpectedFailureInvalidTarget},
		{name: "restore blocked destination", layout: resultLayoutByType(resultTypeSnapshotRestore, valueType{}), payload: SnapshotRestoreEffectResult{ResultOutcome: failedResultOutcome(ExpectedFailureDestinationBlocked)}, failure: ExpectedFailureDestinationBlocked},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			_, outcome, err := runtimeEffectResultFromHost(test.layout, test.payload)
			if err != nil || outcome.FailureReason != test.failure {
				t.Fatalf("outcome=%#v error=%v", outcome, err)
			}
		})
	}
}

func TestTemporalRestoreResultProjectsFieldLists(t *testing.T) {
	layout := resultLayoutByType(resultTypeSnapshotRestore, valueType{})
	value, _, err := runtimeEffectResultFromHost(layout, SnapshotRestoreEffectResult{ResultOutcome: successfulResultOutcome(), Applied: true, AppliedFields: []string{"health"}, SkippedFields: []string{"position"}})
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string][]string{"applied_fields": {"health"}, "skipped_fields": {"position"}} {
		field, found := layout.field(name, resultOutcomeSuccess)
		if !found {
			t.Fatalf("missing result field %q", name)
		}
		projected, found := value.effectResultField(field.handle)
		got, strings := projected.Strings()
		if !found || !strings || !reflect.DeepEqual(got, want) {
			t.Fatalf("field %q was not projected: %#v", name, projected)
		}
	}
}

func TestTemporalCaptureTokenOwnershipExpiryAndCapacity(t *testing.T) {
	environment := DefaultCompileEnvironment()
	host := runtimeTestHost(environment)
	profile := TemporalProfileHandle(1)
	var tokens []SnapshotToken
	for index := 0; index < 4; index++ {
		result, err := host.Apply(EffectCommand{Meta: CommandMeta{RequiredRevision: host.CurrentRevision()}, Payload: TemporalCaptureCommand{Owner: 1, Target: 1, ProgramID: "skill.temporal", GameplayDigest: environment.Digest, Profile: profile}})
		if err != nil {
			t.Fatal(err)
		}
		capture, ok := result.Payload.(SnapshotCaptureEffectResult)
		if !ok || !capture.Succeeded || capture.Token.OpaqueID() == 0 {
			t.Fatalf("capture=%#v", result)
		}
		for _, previous := range tokens {
			if previous == capture.Token {
				t.Fatalf("duplicate token %#v", capture.Token)
			}
		}
		tokens = append(tokens, capture.Token)
	}
	result, err := host.Apply(EffectCommand{Meta: CommandMeta{RequiredRevision: host.CurrentRevision()}, Payload: TemporalCaptureCommand{Owner: 1, Target: 1, ProgramID: "skill.temporal", GameplayDigest: environment.Digest, Profile: profile}})
	if err != nil || result.Payload.(SnapshotCaptureEffectResult).FailureReason != ExpectedFailureCapacityReached {
		t.Fatalf("capacity result=%#v error=%v", result, err)
	}

	result, err = host.Apply(EffectCommand{Meta: CommandMeta{RequiredRevision: host.CurrentRevision()}, Payload: TemporalRestoreCommand{Owner: 2, Target: 1, ProgramID: "skill.temporal", GameplayDigest: environment.Digest, Token: tokens[0]}})
	if err != nil || result.Payload.(SnapshotRestoreEffectResult).FailureReason != ExpectedFailurePermissionDenied {
		t.Fatalf("cross owner result=%#v error=%v", result, err)
	}
	result, err = host.Apply(EffectCommand{Meta: CommandMeta{RequiredRevision: host.CurrentRevision()}, Payload: TemporalRestoreCommand{Owner: 1, Target: 1, ProgramID: "skill.other", GameplayDigest: environment.Digest, Token: tokens[0]}})
	if err != nil || result.Payload.(SnapshotRestoreEffectResult).FailureReason != ExpectedFailurePermissionDenied {
		t.Fatalf("cross program result=%#v error=%v", result, err)
	}
	if _, err := host.Advance(601); err != nil {
		t.Fatal(err)
	}
	result, err = host.Apply(EffectCommand{Meta: CommandMeta{RequiredRevision: host.CurrentRevision()}, Payload: TemporalRestoreCommand{Owner: 1, Target: 1, ProgramID: "skill.temporal", GameplayDigest: environment.Digest, Token: tokens[0]}})
	if err != nil || result.Payload.(SnapshotRestoreEffectResult).FailureReason != ExpectedFailureReferenceExpired {
		t.Fatalf("expired result=%#v error=%v", result, err)
	}
}

func TestTemporalRestoreRespectsProfileAndBlockedPolicy(t *testing.T) {
	newEnvironment := func(fields []string, blocked string, allowRevive bool) CompileEnvironment {
		environment := DefaultCompileEnvironment()
		environment.Gameplay.Temporal.Entries = []TemporalSnapshotProfile{{Handle: 1, Key: "temporal.test", Fields: fields, MaximumAgeTicks: 20, MaximumPerOwner: 4, RestorePolicy: "authorized_fields", EventPolicy: "temporal_only", BlockedPositionPolicy: blocked, AllowRevive: allowRevive}}
		environment.Digest = authorityDigest(environment)
		return environment
	}
	capture := func(t *testing.T, host *MemoryHost, environment CompileEnvironment) SnapshotToken {
		t.Helper()
		result, err := host.Apply(EffectCommand{Meta: CommandMeta{RequiredRevision: host.CurrentRevision()}, Payload: TemporalCaptureCommand{Owner: 1, Target: 1, ProgramID: "skill.temporal", GameplayDigest: environment.Digest, Profile: 1}})
		if err != nil {
			t.Fatal(err)
		}
		return result.Payload.(SnapshotCaptureEffectResult).Token
	}
	restore := func(t *testing.T, host *MemoryHost, environment CompileEnvironment, token SnapshotToken, policy string) SnapshotRestoreEffectResult {
		t.Helper()
		result, err := host.Apply(EffectCommand{Meta: CommandMeta{RequiredRevision: host.CurrentRevision()}, Payload: TemporalRestoreCommand{Owner: 1, Target: 1, ProgramID: "skill.temporal", GameplayDigest: environment.Digest, Token: token, OnBlocked: policy}})
		if err != nil {
			t.Fatal(err)
		}
		return result.Payload.(SnapshotRestoreEffectResult)
	}

	t.Run("restores only authorized fields", func(t *testing.T) {
		environment := newEnvironment([]string{"position", "facing", "health", "resources", "statuses", "ability_state"}, "fail", false)
		host := runtimeTestHost(environment)
		host.UpsertEntity(MemoryEntity{ID: 1, Alive: true, Position: Position{X: 1}, Facing: Direction{X: 1}, Health: 80, MaxHealth: 100, Resources: map[string]int64{"mana": 80}, Statuses: map[StatusHandle]bool{1: true}, AbilityState: map[string]RuntimeValue{"stance": IntRuntimeValue(1, quantityCount)}, Cooldowns: map[string]Tick{"dash": 4}})
		token := capture(t, host, environment)
		host.UpsertEntity(MemoryEntity{ID: 1, Alive: true, Position: Position{X: 9}, Facing: Direction{Y: 1}, Health: 10, MaxHealth: 100, Resources: map[string]int64{"mana": 10}, Statuses: map[StatusHandle]bool{1: false}, AbilityState: map[string]RuntimeValue{"stance": IntRuntimeValue(2, quantityCount)}, Cooldowns: map[string]Tick{"dash": 99}})
		result := restore(t, host, environment, token, "fail")
		entity := host.entities[1]
		stance, _ := entity.AbilityState["stance"].Int()
		if !result.Succeeded || entity.Position != (Position{X: 1}) || entity.Facing != (Direction{X: 1}) || entity.Health != 80 || entity.Resources["mana"] != 80 || !entity.Statuses[1] || stance != 1 || entity.Cooldowns["dash"] != 99 {
			t.Fatalf("result=%#v entity=%#v", result, entity)
		}
	})

	t.Run("blocked policy controls position", func(t *testing.T) {
		for policy, want := range map[string]Position{"nearest": {X: 2}, "stay": {X: 9}} {
			t.Run(policy, func(t *testing.T) {
				environment := newEnvironment([]string{"position"}, policy, false)
				host := runtimeTestHost(environment)
				host.temporalBlocked = map[Position]Position{{X: 1}: {X: 2}}
				host.UpsertEntity(MemoryEntity{ID: 1, Alive: true, Position: Position{X: 1}})
				token := capture(t, host, environment)
				host.UpsertEntity(MemoryEntity{ID: 1, Alive: true, Position: Position{X: 9}})
				result := restore(t, host, environment, token, policy)
				if !result.Succeeded || host.entities[1].Position != want {
					t.Fatalf("result=%#v position=%#v want=%#v", result, host.entities[1].Position, want)
				}
				if policy == "stay" && (len(result.AppliedFields) != 0 || len(result.SkippedFields) != 1 || result.SkippedFields[0] != "position") {
					t.Fatalf("stay result=%#v", result)
				}
			})
		}
		environment := newEnvironment([]string{"position"}, "fail", false)
		host := runtimeTestHost(environment)
		host.temporalBlocked = map[Position]Position{{X: 1}: {X: 2}}
		host.UpsertEntity(MemoryEntity{ID: 1, Alive: true, Position: Position{X: 1}})
		token := capture(t, host, environment)
		host.UpsertEntity(MemoryEntity{ID: 1, Alive: true, Position: Position{X: 9}})
		result := restore(t, host, environment, token, "fail")
		if result.FailureReason != ExpectedFailureDestinationBlocked || host.entities[1].Position != (Position{X: 9}) {
			t.Fatalf("result=%#v position=%#v", result, host.entities[1].Position)
		}
	})

	t.Run("revive requires profile permission", func(t *testing.T) {
		for name, allowRevive := range map[string]bool{"default": false, "allowed": true} {
			t.Run(name, func(t *testing.T) {
				environment := newEnvironment([]string{"health"}, "fail", allowRevive)
				host := runtimeTestHost(environment)
				host.UpsertEntity(MemoryEntity{ID: 1, Alive: true, Health: 80, MaxHealth: 100})
				token := capture(t, host, environment)
				host.UpsertEntity(MemoryEntity{ID: 1, Alive: false, Health: 0, MaxHealth: 100})
				result := restore(t, host, environment, token, "fail")
				if result.Succeeded != allowRevive || host.entities[1].Alive != allowRevive {
					t.Fatalf("allow=%v result=%#v entity=%#v", allowRevive, result, host.entities[1])
				}
			})
		}
	})
}

func TestTemporalRuntimeCaptureAndRestore(t *testing.T) {
	environment := DefaultCompileEnvironment()
	restore := `{"flow":"effect","effect":{"type":"restore_snapshot","target":"$caster","snapshot":"$local.snapshot.token"},"result":{"as":"restored","success":{"flow":"finish"},"failure":{"flow":"finish"}}}`
	flow := `{"flow":"effect","effect":{"type":"capture_snapshot","target":"$caster","profile":"temporal.position_health"},"result":{"as":"snapshot","success":{"flow":"sequence","steps":[{"flow":"effect","effect":{"type":"damage","target":"$caster","amount":10,"damage_type":"physical"}},` + restore + `]},"failure":{"flow":"finish"}}}`
	program, diagnostics := Compile(mustParseJSON(t, inputSkillJSON("temporal-runtime", `{"type":"none"}`, `{"enter":`+flow+`}`)), environment)
	requireNoErrors(t, diagnostics)
	host := runtimeTestHost(environment)
	host.UpsertEntity(MemoryEntity{ID: 1, Alive: true, Position: Position{X: 1}, Health: 80, MaxHealth: 100, Resources: map[string]int64{"mana": 100}})
	if _, err := NewRuntime(host, RuntimeOptions{}).Activate(program, CastInput{Caster: 1}); err != nil {
		t.Fatal(err)
	}
	if host.entities[1].Position != (Position{X: 1}) || host.entities[1].Health != 80 {
		t.Fatalf("entity=%#v", host.entities[1])
	}
	seen := map[string]bool{}
	for _, event := range host.Events(0) {
		seen[event.Kind] = true
	}
	if !seen["temporal_snapshot_captured"] || !seen["temporal_restored"] {
		t.Fatalf("events=%#v", host.Events(0))
	}
}

func TestTemporalNoWorldRollback(t *testing.T) {
	environment := DefaultCompileEnvironment()
	environment.Gameplay.Temporal.Entries = []TemporalSnapshotProfile{{Handle: 1, Key: "temporal.world", Fields: []string{"position", "resources"}, MaximumAgeTicks: 20, MaximumPerOwner: 4, RestorePolicy: "authorized_fields", EventPolicy: "temporal_only", BlockedPositionPolicy: "fail"}}
	environment.Digest = authorityDigest(environment)
	host := runtimeTestHost(environment)
	host.UpsertEntity(MemoryEntity{ID: 1, Alive: true, Position: Position{X: 1}, Resources: map[string]int64{"mana": 80}})
	host.UpsertEntity(MemoryEntity{ID: 2, Alive: true, Position: Position{X: 2}, Resources: map[string]int64{"mana": 70}})
	capture, err := host.Apply(EffectCommand{Meta: CommandMeta{RequiredRevision: host.CurrentRevision()}, Payload: TemporalCaptureCommand{Owner: 1, Target: 1, ProgramID: "skill.temporal", GameplayDigest: environment.Digest, Profile: 1}})
	if err != nil {
		t.Fatal(err)
	}
	token := capture.Payload.(SnapshotCaptureEffectResult).Token
	host.UpsertEntity(MemoryEntity{ID: 1, Alive: true, Position: Position{X: 9}, Resources: map[string]int64{"mana": 10}})
	host.UpsertEntity(MemoryEntity{ID: 2, Alive: true, Position: Position{X: 99}, Resources: map[string]int64{"mana": 44}})
	host.ownedEntities[99] = OwnedEntityMetadata{Entity: 99, Owner: 2, DueTick: 20}
	if _, err := host.Advance(1); err != nil {
		t.Fatal(err)
	}
	if _, err := host.Apply(EffectCommand{Meta: CommandMeta{RequiredRevision: host.CurrentRevision()}, Payload: TemporalRestoreCommand{Owner: 1, Target: 1, ProgramID: "skill.temporal", GameplayDigest: environment.Digest, Token: token}}); err != nil {
		t.Fatal(err)
	}
	if entity := host.entities[1]; entity.Position != (Position{X: 1}) || entity.Resources["mana"] != 80 {
		t.Fatalf("captured entity=%#v", entity)
	}
	if entity := host.entities[2]; entity.Position != (Position{X: 99}) || entity.Resources["mana"] != 44 {
		t.Fatalf("other entity rolled back: %#v", entity)
	}
	if host.tick != 1 || host.ownedEntities[99].Owner != 2 {
		t.Fatalf("world state rolled back tick=%d owned=%#v", host.tick, host.ownedEntities[99])
	}
	for _, event := range host.Events(0) {
		if event.Kind == "damage" || event.Kind == "heal" {
			t.Fatalf("default temporal restore emitted combat event: %#v", event)
		}
	}
}

func TestTemporalRestoreFailureCannotCarryAppliedFields(t *testing.T) {
	layout := resultLayoutByType(resultTypeSnapshotRestore, valueType{})
	_, _, err := runtimeEffectResultFromHost(layout, SnapshotRestoreEffectResult{ResultOutcome: failedResultOutcome(ExpectedFailurePolicyRejected), AppliedFields: []string{"position"}})
	if err == nil {
		t.Fatal("failed restore accepted applied fields")
	}
}

func TestTemporalRestoreRejectedEmitsTemporalEvent(t *testing.T) {
	environment := DefaultCompileEnvironment()
	environment.Gameplay.Temporal.Entries = []TemporalSnapshotProfile{{Handle: 1, Key: "temporal.blocked", Fields: []string{"position"}, MaximumAgeTicks: 20, MaximumPerOwner: 4, RestorePolicy: "authorized_fields", EventPolicy: "temporal_only", BlockedPositionPolicy: "fail"}}
	environment.Digest = authorityDigest(environment)
	host := runtimeTestHost(environment)
	host.temporalBlocked = map[Position]Position{{X: 1}: {X: 2}}
	host.UpsertEntity(MemoryEntity{ID: 1, Alive: true, Position: Position{X: 1}})
	captured, err := host.Apply(EffectCommand{Meta: CommandMeta{RequiredRevision: host.CurrentRevision()}, Payload: TemporalCaptureCommand{Owner: 1, Target: 1, ProgramID: "skill.temporal", GameplayDigest: environment.Digest, Profile: 1}})
	if err != nil {
		t.Fatal(err)
	}
	host.UpsertEntity(MemoryEntity{ID: 1, Alive: true, Position: Position{X: 9}})
	result, err := host.Apply(EffectCommand{Meta: CommandMeta{RequiredRevision: host.CurrentRevision()}, Payload: TemporalRestoreCommand{Owner: 1, Target: 1, ProgramID: "skill.temporal", GameplayDigest: environment.Digest, Token: captured.Payload.(SnapshotCaptureEffectResult).Token}})
	if err != nil || result.Payload.(SnapshotRestoreEffectResult).FailureReason != ExpectedFailureDestinationBlocked {
		t.Fatalf("result=%#v error=%v", result, err)
	}
	seen := false
	for _, event := range host.Events(0) {
		seen = seen || event.Kind == "temporal_restore_rejected"
	}
	if !seen {
		t.Fatalf("events=%#v", host.Events(0))
	}
}

func TestTemporalDerivedCombatEventsRequireProfilePolicy(t *testing.T) {
	for name, eventPolicy := range map[string]struct {
		policy string
		want   bool
	}{"default": {policy: "temporal_only", want: false}, "authorized": {policy: "derived_combat", want: true}} {
		t.Run(name, func(t *testing.T) {
			environment := DefaultCompileEnvironment()
			environment.Gameplay.Temporal.Entries = []TemporalSnapshotProfile{{Handle: 1, Key: "temporal.health", Fields: []string{"health"}, MaximumAgeTicks: 20, MaximumPerOwner: 4, RestorePolicy: "authorized_fields", EventPolicy: eventPolicy.policy, BlockedPositionPolicy: "fail"}}
			environment.Digest = authorityDigest(environment)
			host := runtimeTestHost(environment)
			host.UpsertEntity(MemoryEntity{ID: 1, Alive: true, Health: 80, MaxHealth: 100})
			captured, err := host.Apply(EffectCommand{Meta: CommandMeta{RequiredRevision: host.CurrentRevision()}, Payload: TemporalCaptureCommand{Owner: 1, Target: 1, ProgramID: "skill.temporal", GameplayDigest: environment.Digest, Profile: 1}})
			if err != nil {
				t.Fatal(err)
			}
			host.UpsertEntity(MemoryEntity{ID: 1, Alive: true, Health: 10, MaxHealth: 100})
			if _, err := host.Apply(EffectCommand{Meta: CommandMeta{RequiredRevision: host.CurrentRevision()}, Payload: TemporalRestoreCommand{Owner: 1, Target: 1, ProgramID: "skill.temporal", GameplayDigest: environment.Digest, Token: captured.Payload.(SnapshotCaptureEffectResult).Token}}); err != nil {
				t.Fatal(err)
			}
			seenHeal := false
			for _, event := range host.Events(0) {
				seenHeal = seenHeal || event.Kind == "heal_resolved"
			}
			if seenHeal != eventPolicy.want {
				t.Fatalf("policy=%s events=%#v", eventPolicy.policy, host.Events(0))
			}
		})
	}
}

func TestTemporalDerivedCombatEventCarriesRestoreContext(t *testing.T) {
	environment := DefaultCompileEnvironment()
	environment.Gameplay.Temporal.Entries = []TemporalSnapshotProfile{{Handle: 1, Key: "temporal.context", Fields: []string{"health"}, MaximumAgeTicks: 20, MaximumPerOwner: 4, RestorePolicy: "authorized_fields", EventPolicy: "derived_combat", BlockedPositionPolicy: "fail"}}
	environment.Digest = authorityDigest(environment)
	host := runtimeTestHost(environment)
	host.UpsertEntity(MemoryEntity{ID: 1, Alive: true, Health: 80, MaxHealth: 100})
	captured, err := host.Apply(EffectCommand{Meta: CommandMeta{RequiredRevision: host.CurrentRevision()}, Payload: TemporalCaptureCommand{Owner: 1, Target: 1, ProgramID: "skill.temporal", GameplayDigest: environment.Digest, Profile: 1}})
	if err != nil {
		t.Fatal(err)
	}
	host.UpsertEntity(MemoryEntity{ID: 1, Alive: true, Health: 10, MaxHealth: 100})
	context := EventContext{Source: 7, Owner: 8, Target: 99, Result: "stale"}
	if _, err := host.Apply(EffectCommand{Meta: CommandMeta{RequiredRevision: host.CurrentRevision()}, Payload: TemporalRestoreCommand{Owner: 1, Target: 1, ProgramID: "skill.temporal", GameplayDigest: environment.Digest, Token: captured.Payload.(SnapshotCaptureEffectResult).Token, Context: context}}); err != nil {
		t.Fatal(err)
	}
	for _, event := range host.Events(0) {
		if event.Kind == "heal_resolved" {
			if event.Context.Source != 7 || event.Context.Owner != 8 || event.Context.Target != 1 || event.Context.Result != "healed" {
				t.Fatalf("context=%#v", event.Context)
			}
			return
		}
	}
	t.Fatal("missing derived heal event")
}

func TestTemporalEventsNormalizeSnapshotTarget(t *testing.T) {
	environment := DefaultCompileEnvironment()
	host := runtimeTestHost(environment)
	host.UpsertEntity(MemoryEntity{ID: 1, Alive: true, Position: Position{X: 1}, Health: 80, MaxHealth: 100})
	capture := func(t *testing.T) SnapshotToken {
		t.Helper()
		result, err := host.Apply(EffectCommand{Meta: CommandMeta{RequiredRevision: host.CurrentRevision()}, Payload: TemporalCaptureCommand{Owner: 1, Target: 1, ProgramID: "skill.temporal", GameplayDigest: environment.Digest, Profile: 1, Context: EventContext{Target: 99}}})
		if err != nil {
			t.Fatal(err)
		}
		return result.Payload.(SnapshotCaptureEffectResult).Token
	}
	restore := func(token SnapshotToken) {
		_, err := host.Apply(EffectCommand{Meta: CommandMeta{RequiredRevision: host.CurrentRevision()}, Payload: TemporalRestoreCommand{Owner: 1, Target: 1, ProgramID: "skill.temporal", GameplayDigest: environment.Digest, Token: token, Context: EventContext{Target: 99}}})
		if err != nil {
			t.Fatal(err)
		}
	}

	first := capture(t)
	host.UpsertEntity(MemoryEntity{ID: 1, Alive: true, Position: Position{X: 9}, Health: 10, MaxHealth: 100})
	restore(first)
	second := capture(t)
	host.temporalBlocked = map[Position]Position{{X: 1}: {X: 2}}
	host.UpsertEntity(MemoryEntity{ID: 1, Alive: true, Position: Position{X: 9}, Health: 80, MaxHealth: 100})
	restore(second)

	want := map[string]bool{"temporal_snapshot_captured": false, "temporal_restored": false, "temporal_restore_rejected": false}
	for _, event := range host.Events(0) {
		if _, tracked := want[event.Kind]; !tracked {
			continue
		}
		if event.Context.Target != 1 {
			t.Fatalf("event %s context target=%d", event.Kind, event.Context.Target)
		}
		want[event.Kind] = true
	}
	for kind, seen := range want {
		if !seen {
			t.Fatalf("missing event %s", kind)
		}
	}
}
