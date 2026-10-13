package skill

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
)

func seedCheckpointMaps(runtime *Runtime, withRestorableOnly bool) {
	for index := 1; index <= 8; index++ {
		caster := EntityID(100 - index)
		skill := fmt.Sprintf("skill.%02d", index)
		runtime.procLedger[procLedgerKey{Root: EventID(50 - index), Caster: caster, Digest: skill}] = struct{}{}
		id := EventID(90 - index)
		runtime.rootEventCounts[id] = index
		runtime.rootEventOrder = append(runtime.rootEventOrder, id)
		if withRestorableOnly {
			continue
		}
		runtime.activePolicies[skillStateKey{Caster: caster, Skill: skill}] = CastID(index)
		runtime.abilityByProgram[skillStateKey{Caster: caster, Skill: skill}] = AbilityHandle(index)
	}
	// 直接写进 map 的条目不经 state mutation 流，按当前状态重定基线，Checkpoint 才肯写出。
	runtime.stateMutationBaseline = runtime.stateSnapshotLocked()
}

func TestRuntimeCheckpointBytesAreDeterministic(t *testing.T) {
	_, environment := compileRuntimeFixture(t, "simple_damage.json")
	runtime := NewRuntime(runtimeTestHost(environment), RuntimeOptions{})
	seedCheckpointMaps(runtime, false)
	first, err := runtime.Checkpoint()
	if err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < 20; attempt++ {
		again, err := runtime.Checkpoint()
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(again.Payload, first.Payload) || again.Checksum != first.Checksum {
			t.Fatalf("checkpoint %d of the same state differs:\n got %s\nwant %s", attempt+2, again.Payload, first.Payload)
		}
	}
	var payload runtimeCheckpointPayload
	if err := json.Unmarshal(first.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if !slices.IsSortedFunc(payload.ActivePolicies, func(a, b checkpointActivePolicy) int {
		return compareSkillStateKey(a.Caster, a.Skill, b.Caster, b.Skill)
	}) ||
		!slices.IsSortedFunc(payload.AbilityByProgram, func(a, b checkpointAbilityLookup) int {
			return compareSkillStateKey(a.Caster, a.Skill, b.Caster, b.Skill)
		}) ||
		!slices.IsSortedFunc(payload.RootEventCounts, func(a, b checkpointRootEvent) int { return compareOrdered(a.ID, b.ID) }) ||
		!slices.IsSortedFunc(payload.ProcLedger, func(a, b checkpointProcLedger) int {
			if c := compareOrdered(a.Root, b.Root); c != 0 {
				return c
			}
			return compareSkillStateKey(a.Caster, a.Digest, b.Caster, b.Digest)
		}) {
		t.Fatalf("checkpoint lists are not sorted: %s", first.Payload)
	}
}

func TestRuntimeRestoresUnsortedLegacyCheckpointLists(t *testing.T) {
	program, environment := compileRuntimeFixture(t, "simple_damage.json")
	host := runtimeTestHost(environment)
	runtime := NewRuntime(host, RuntimeOptions{})
	seedCheckpointMaps(runtime, true)
	sorted, err := runtime.Checkpoint()
	if err != nil {
		t.Fatal(err)
	}
	// 旧版本的样子：列表按任意顺序写出。这里把两个可恢复的列表倒过来。
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(sorted.Payload, &fields); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"proc_ledger", "root_event_counts"} {
		var items []json.RawMessage
		if err := json.Unmarshal(fields[key], &items); err != nil || len(items) != 8 {
			t.Fatalf("%s = %s (%v)", key, fields[key], err)
		}
		slices.Reverse(items)
		reversed, err := json.Marshal(items)
		if err != nil {
			t.Fatal(err)
		}
		fields[key] = reversed
	}
	legacy := sorted
	if legacy.Payload, err = json.Marshal(fields); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(legacy.Payload)
	legacy.Checksum = hex.EncodeToString(digest[:])
	restored, err := RestoreRuntime(host, RuntimeOptions{}, legacy, ProgramResolverFunc(func(string, string) (*Program, error) { return program, nil }))
	if err != nil {
		t.Fatalf("restore of an unsorted checkpoint: %v", err)
	}
	again, err := restored.Checkpoint()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(again.Payload, sorted.Payload) {
		t.Fatalf("checkpoint after restoring the unsorted lists differs:\n got %s\nwant %s", again.Payload, sorted.Payload)
	}
}

func compareSkillStateKey(leftCaster EntityID, leftSkill string, rightCaster EntityID, rightSkill string) int {
	if c := compareOrdered(leftCaster, rightCaster); c != 0 {
		return c
	}
	return compareOrdered(leftSkill, rightSkill)
}

func compareOrdered[T ~uint64 | ~string](left, right T) int {
	switch {
	case left < right:
		return -1
	case left > right:
		return 1
	}
	return 0
}

func TestCheckpointRestoreRejectsPhaseTimeoutTask(t *testing.T) {
	program, environment := compileCastWindowSkill(t, true)
	host := runtimeTestHost(environment)
	source := NewRuntime(host, RuntimeOptions{MatchSeed: fixedTestSeed(20)})
	if _, err := source.Activate(program, CastInput{Caster: 1, Target: 2}); err != nil {
		t.Fatal(err)
	}
	if err := source.Advance(2); err != nil {
		t.Fatal(err)
	}
	checkpoint, err := source.Checkpoint()
	if err != nil {
		t.Fatal(err)
	}
	resolver := ProgramResolverFunc(func(id, digest string) (*Program, error) {
		if id == program.id && digest == program.identity.gameplayDigest {
			return program, nil
		}
		return nil, ErrCheckpointProgram
	})
	// 对照：未改动的 checkpoint 能恢复，下面唯一的差别是任务种类。
	if _, err := RestoreRuntime(host, RuntimeOptions{}, checkpoint, resolver); err != nil {
		t.Fatalf("unmodified checkpoint must restore: %v", err)
	}

	// UseNumber：保留 uint64 字段（随机键、种子）的精度，重编码后只有 kind 不同。
	var payload map[string]any
	decoder := json.NewDecoder(bytes.NewReader(checkpoint.Payload))
	decoder.UseNumber()
	if err := decoder.Decode(&payload); err != nil {
		t.Fatal(err)
	}
	tasks, _ := payload["tasks"].([]any)
	rewritten := false
	for _, raw := range tasks {
		task := raw.(map[string]any)
		if castID, _ := task["cast_id"].(json.Number); castID != "" && castID != "0" {
			task["kind"] = "phase_timeout"
			rewritten = true
			break
		}
	}
	if !rewritten {
		t.Fatalf("checkpoint has no cast task to rewrite: %s", checkpoint.Payload)
	}
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(data)
	forged := RuntimeCheckpoint{Version: checkpoint.Version, Payload: data, Checksum: hex.EncodeToString(digest[:])}
	if _, err := RestoreRuntime(host, RuntimeOptions{}, forged, resolver); !errors.Is(err, ErrCheckpointCorrupt) {
		t.Fatalf("checkpoint with a phase_timeout task restored with err=%v; phase timeouts are rejected at compile time, so the task must be refused as corrupt", err)
	}
}

func TestRuntimeCheckpointRestoresActiveTimelineDeterministically(t *testing.T) {
	program, environment := compileCastWindowSkill(t, true)
	seed := fixedTestSeed(88)

	baselineHost := runtimeTestHost(environment)
	baseline := NewRuntime(baselineHost, RuntimeOptions{MatchSeed: seed})
	baselineID, err := baseline.Activate(program, CastInput{Caster: 1, Target: 2})
	if err != nil {
		t.Fatal(err)
	}
	if err := baseline.Advance(2); err != nil {
		t.Fatal(err)
	}

	recoveredHost := runtimeTestHost(environment)
	source := NewRuntime(recoveredHost, RuntimeOptions{MatchSeed: seed})
	recoveredID, err := source.Activate(program, CastInput{Caster: 1, Target: 2})
	if err != nil {
		t.Fatal(err)
	}
	if err := source.Advance(2); err != nil {
		t.Fatal(err)
	}
	checkpoint, err := source.Checkpoint()
	if err != nil {
		t.Fatal(err)
	}
	resolver := ProgramResolverFunc(func(id, digest string) (*Program, error) {
		if id == program.id && digest == program.identity.gameplayDigest {
			return program, nil
		}
		return nil, ErrCheckpointProgram
	})
	recovered, err := RestoreRuntime(recoveredHost, RuntimeOptions{}, checkpoint, resolver)
	if err != nil {
		t.Fatal(err)
	}
	if recoveredID != baselineID {
		t.Fatalf("cast id=%d, want %d", recoveredID, baselineID)
	}
	for _, tick := range []Tick{3, 5, 9} {
		if err := baseline.Advance(tick); err != nil {
			t.Fatal(err)
		}
		if err := recovered.Advance(tick); err != nil {
			t.Fatal(err)
		}
		left, _ := baseline.InspectCast(baselineID)
		right, _ := recovered.InspectCast(recoveredID)
		if left.Status != right.Status || left.WindowStage != right.WindowStage || left.PulseIndex != right.PulseIndex {
			t.Fatalf("tick %d baseline=%#v recovered=%#v", tick, left, right)
		}
		if baselineHost.HealthForTest(2) != recoveredHost.HealthForTest(2) || baselineHost.ResourceForTest(1, "mana") != recoveredHost.ResourceForTest(1, "mana") {
			t.Fatalf("tick %d host state diverged", tick)
		}
	}
}

func TestRuntimeCheckpointRestoresActiveSpawns(t *testing.T) {
	tests := []struct {
		fixture string
		input   CastInput
		ticks   []Tick
	}{
		{fixture: "area_heal.json", input: CastInput{Caster: 1}, ticks: []Tick{1, 3}},
		{fixture: "dynamic_numeric.json", input: CastInput{Caster: 1}, ticks: []Tick{1, 3}},
		{fixture: "tracking_boomerang.json", input: CastInput{Caster: 1}, ticks: []Tick{1, 3, 10}},
	}
	for _, test := range tests {
		t.Run(test.fixture, func(t *testing.T) {
			program, environment := compileRuntimeFixture(t, test.fixture)
			baselineHost := runtimeTestHost(environment)
			baseline := NewRuntime(baselineHost, RuntimeOptions{MatchSeed: fixedTestSeed(91)})
			if _, err := baseline.Activate(program, test.input); err != nil {
				t.Fatal(err)
			}
			recoveredHost := runtimeTestHost(environment)
			source := NewRuntime(recoveredHost, RuntimeOptions{MatchSeed: fixedTestSeed(91)})
			if _, err := source.Activate(program, test.input); err != nil {
				t.Fatal(err)
			}
			checkpoint, err := source.Checkpoint()
			if err != nil {
				t.Fatal(err)
			}
			recovered, err := RestoreRuntime(recoveredHost, RuntimeOptions{}, checkpoint, ProgramResolverFunc(func(id, digest string) (*Program, error) {
				if id == program.id && digest == program.identity.gameplayDigest {
					return program, nil
				}
				return nil, ErrCheckpointProgram
			}))
			if err != nil {
				t.Fatal(err)
			}
			for _, tick := range test.ticks {
				if err := baseline.Advance(tick); err != nil {
					t.Fatal(err)
				}
				if err := recovered.Advance(tick); err != nil {
					t.Fatal(err)
				}
				want, _ := json.Marshal(baseline.StateSnapshot())
				got, _ := json.Marshal(recovered.StateSnapshot())
				if !bytes.Equal(got, want) || recoveredHost.CurrentRevision() != baselineHost.CurrentRevision() {
					t.Fatalf("tick %d diverged\nwant=%s\ngot=%s", tick, want, got)
				}
			}
		})
	}
}

func TestRuntimeCheckpointFailsClosed(t *testing.T) {
	program, environment := compileRuntimeFixture(t, "simple_damage.json")
	host := runtimeTestHost(environment)
	runtime := NewRuntime(host, RuntimeOptions{})
	checkpoint, err := runtime.Checkpoint()
	if err != nil {
		t.Fatal(err)
	}
	resolver := ProgramResolverFunc(func(string, string) (*Program, error) { return program, nil })

	corrupt := checkpoint
	corrupt.Payload = append([]byte(nil), checkpoint.Payload...)
	corrupt.Payload[len(corrupt.Payload)-1] ^= 1
	if _, err := RestoreRuntime(host, RuntimeOptions{}, corrupt, resolver); !errors.Is(err, ErrCheckpointCorrupt) {
		t.Fatalf("corrupt restore=%v", err)
	}
	wrongHost := runtimeTestHost(environment)
	wrongHost.revision++
	if _, err := RestoreRuntime(wrongHost, RuntimeOptions{}, checkpoint, resolver); !errors.Is(err, ErrCheckpointHostMismatch) {
		t.Fatalf("host mismatch=%v", err)
	}
	unsupported := checkpoint
	unsupported.Version++
	if _, err := RestoreRuntime(host, RuntimeOptions{}, unsupported, resolver); !errors.Is(err, ErrCheckpointUnsupported) {
		t.Fatalf("version mismatch=%v", err)
	}
}

func TestRuntimeRestoreRequiresFullRecoveryForOmittedDeliveryBuffers(t *testing.T) {
	program, environment := compileRuntimeFixture(t, "visual_projectile.json")
	host := runtimeTestHost(environment)
	runtime := NewRuntime(host, RuntimeOptions{})
	if _, err := runtime.Activate(program, CastInput{Caster: 1, Target: 2}); err != nil {
		t.Fatal(err)
	}
	checkpoint, err := runtime.Checkpoint()
	if err != nil {
		t.Fatal(err)
	}
	restored, err := RestoreRuntime(host, RuntimeOptions{}, checkpoint, ProgramResolverFunc(func(string, string) (*Program, error) { return program, nil }))
	if err != nil {
		t.Fatal(err)
	}
	if batch := restored.StateDeltas(0, 10); batch.LatestSequence == 0 || !batch.CursorExpired || len(batch.Mutations) != 0 {
		t.Fatalf("state recovery=%#v", batch)
	}
	if batch := restored.StateEvents(0, 10); batch.LatestSequence == 0 || !batch.CursorExpired || len(batch.Events) != 0 {
		t.Fatalf("event recovery=%#v", batch)
	}
	if batch := restored.PollPresentation(0, 10); batch.LatestSequence == 0 || !batch.CursorExpired || len(batch.Events) != 0 {
		t.Fatalf("presentation recovery=%#v", batch)
	}
}

func TestRuntimeCheckpointRestoresPendingPassiveActivation(t *testing.T) {
	program, environment := compileRuntimeFixture(t, "passive_counter.json")
	event := EventContext{EventID: 10, RootEventID: 10, Source: 2, Owner: 1, Target: 1, Result: "hit"}.WithGameplayTags([]GameplayTagHandle{2, 1})
	baselineHost := runtimeTestHost(environment)
	baseline := NewRuntime(baselineHost, RuntimeOptions{MatchSeed: fixedTestSeed(92)})
	if _, err := baseline.ActivatePassive(program, event); err != nil {
		t.Fatal(err)
	}
	recoveredHost := runtimeTestHost(environment)
	source := NewRuntime(recoveredHost, RuntimeOptions{MatchSeed: fixedTestSeed(92)})
	if _, err := source.ActivatePassive(program, event); err != nil {
		t.Fatal(err)
	}
	checkpoint, err := source.Checkpoint()
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := RestoreRuntime(recoveredHost, RuntimeOptions{}, checkpoint, ProgramResolverFunc(func(string, string) (*Program, error) { return program, nil }))
	if err != nil {
		t.Fatal(err)
	}
	if err := baseline.Advance(0); err != nil {
		t.Fatal(err)
	}
	if err := recovered.Advance(0); err != nil {
		t.Fatal(err)
	}
	want, _ := json.Marshal(baseline.StateSnapshot())
	got, _ := json.Marshal(recovered.StateSnapshot())
	if !bytes.Equal(got, want) || recoveredHost.CurrentRevision() != baselineHost.CurrentRevision() {
		t.Fatalf("passive continuation diverged\nwant=%s\ngot=%s", want, got)
	}
}

// RestoreRuntime 不走 NewRuntime 的快进路径：快进会压缩到当前前沿为止的 Host 事件，删掉 checkpoint 之后、
// 崩溃之前发出的事件——恰好是恢复后的 Runtime 要重放的那些。用会压缩事件的 Host 断言事件仍在、游标停在 checkpoint。
func TestRestoreRuntimeKeepsTheEventsEmittedAfterTheCheckpoint(t *testing.T) {
	host := NewMemoryHostWithOptions(AuthorityIdentity{Revision: "test", Digest: "test"}, MemoryHostOptions{CompactEvents: true})
	runtime := NewRuntime(host, RuntimeOptions{})
	if err := runtime.Advance(0); err != nil {
		t.Fatal(err)
	}
	checkpoint, err := runtime.Checkpoint()
	if err != nil {
		t.Fatal(err)
	}
	// Three events land after the checkpoint and before the "crash".
	host.mutex.Lock()
	for i := 0; i < 3; i++ {
		host.appendEventLocked("after-checkpoint", 0, 0)
	}
	host.mutex.Unlock()
	before := host.Events(0)
	if len(before) != 3 {
		t.Fatalf("host holds %d events before restore, want 3", len(before))
	}

	restored, err := RestoreRuntime(host, RuntimeOptions{}, checkpoint, ProgramResolverFunc(func(string, string) (*Program, error) { return nil, ErrCheckpointProgram }))
	if err != nil {
		t.Fatal(err)
	}
	after := host.Events(0)
	if len(after) != 3 {
		t.Fatalf("restore compacted the host's events: %d left of 3; a restored runtime can no longer replay what happened after its checkpoint", len(after))
	}
	if restored.eventCursor != 0 {
		t.Fatalf("restored cursor = %d, want the checkpoint's 0 so the three events are replayed", restored.eventCursor)
	}
}

// 载荷与校验和不符的 checkpoint 是损坏的，必须拒绝恢复（ErrCheckpointCorrupt），不能从没人写过的状态续跑。
func TestRestoreRefusesACheckpointWhosePayloadWasTampered(t *testing.T) {
	host := NewMemoryHost(AuthorityIdentity{Revision: "test", Digest: "test"})
	runtime := NewRuntime(host, RuntimeOptions{})
	if err := runtime.Advance(0); err != nil {
		t.Fatal(err)
	}
	checkpoint, err := runtime.Checkpoint()
	if err != nil {
		t.Fatal(err)
	}
	resolver := ProgramResolverFunc(func(string, string) (*Program, error) { return nil, ErrCheckpointProgram })
	if _, err := RestoreRuntime(host, RuntimeOptions{}, checkpoint, resolver); err != nil {
		t.Fatalf("an intact checkpoint was refused: %v", err)
	}
	tampered := checkpoint
	tampered.Payload = append([]byte(nil), checkpoint.Payload...)
	// Flip a byte inside the JSON payload without breaking its length.
	for i := len(tampered.Payload) - 1; i > 0; i-- {
		if tampered.Payload[i] == '0' {
			tampered.Payload[i] = '1'
			break
		} else if tampered.Payload[i] == '1' {
			tampered.Payload[i] = '2'
			break
		}
	}
	if _, err := RestoreRuntime(host, RuntimeOptions{}, tampered, resolver); !errors.Is(err, ErrCheckpointCorrupt) {
		t.Fatalf("a tampered payload restored with err=%v, want ErrCheckpointCorrupt", err)
	}
	wrongSum := checkpoint
	wrongSum.Checksum = strings.Repeat("0", len(checkpoint.Checksum))
	if _, err := RestoreRuntime(host, RuntimeOptions{}, wrongSum, resolver); !errors.Is(err, ErrCheckpointCorrupt) {
		t.Fatalf("a wrong checksum restored with err=%v, want ErrCheckpointCorrupt", err)
	}
}

// 恢复的构造过程不碰 Host 的事件队列：即使恢复因 Host 版本不符被拒（ErrCheckpointHostMismatch），
// checkpoint 之后的事件也必须原样保留，供正确恢复的一方重放。
func TestRestoreRuntimePreservesPostCheckpointEvents(t *testing.T) {
	environment := abilityTestEnvironment()
	program := compileAbilityTestSkill(t, environment, "restore-events", `{"mode":"tap"}`, 0, `{"flow":"finish"}`)
	host := runtimeTestHost(environment)
	runtime := NewRuntime(host, RuntimeOptions{})
	if _, err := runtime.Activate(program, CastInput{Caster: 1}); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Advance(1); err != nil {
		t.Fatal(err)
	}
	checkpoint, err := runtime.Checkpoint()
	if err != nil {
		t.Fatal(err)
	}
	cursorAt := runtime.eventCursor
	// World events arrive after the checkpoint, before the "crash".
	if _, err := host.Apply(EffectCommand{Payload: HealCommand{Target: 2, Amount: 1}}); err != nil {
		t.Fatal(err)
	}
	postCheckpoint := host.Events(cursorAt)
	if len(postCheckpoint) == 0 {
		t.Fatal("fixture produced no post-checkpoint events")
	}
	resolver := ProgramResolverFunc(func(id, digest string) (*Program, error) { return program, nil })
	// The heal advanced the host revision past the checkpoint, so this
	// restore is correctly rejected — but rejection must leave the host
	// untouched: before the fix, construction had already compacted the
	// post-checkpoint events away.
	if _, err := RestoreRuntime(host, RuntimeOptions{}, checkpoint, resolver); !errors.Is(err, ErrCheckpointHostMismatch) {
		t.Fatalf("expected host mismatch, got %v", err)
	}
	if remaining := host.Events(cursorAt); len(remaining) != len(postCheckpoint) {
		t.Fatalf("rejected restore compacted %d post-checkpoint events away", len(postCheckpoint)-len(remaining))
	}
}
