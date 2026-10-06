package skill

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"testing"
)

// RR-20261006-03（O20）：phase 计时保持方向 B（B3④），timeout_ticks > 0 在编译期就被拒绝，live 代码从不调度
// phase_timeout 任务；恢复出来的这类任务执行时只会 ErrProgramInvariant。承诺：checkpoint 里出现
// phase_timeout 任务按 ErrCheckpointCorrupt 拒绝恢复（只拒绝不迁移）。修前 restoreCheckpointTask 仍把它
// 恢复成 phaseTimeoutTask，RestoreRuntime 返回成功。
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
