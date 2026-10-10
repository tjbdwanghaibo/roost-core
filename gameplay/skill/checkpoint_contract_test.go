package skill

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
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
