package skill

// O7（维护者第十二轮决定，2026-10-06）：checkpoint 的 ActivePolicies、ProcLedger、
// RootEventCounts、AbilityByProgram 四个列表以前按 map 迭代顺序写出，同一个状态两次
// Checkpoint 的字节（和 Checksum）不同，无法拿 checkpoint 摘要比对一致性。现在四个列表
// 按键排序写出。格式不变：恢复本来就与列表顺序无关（rootEventOrder 恢复时按 ID 排序），
// 旧版本写出的乱序 checkpoint 照常可读，恢复后再 Checkpoint 得到排序后的字节。
//
// 承诺：同一状态反复 Checkpoint，Payload 与 Checksum 逐字节相同、四个列表有序；
// 把列表打乱后的 checkpoint（旧版本的样子）照常恢复，恢复后的 Checkpoint 与原来逐字节相同。

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
