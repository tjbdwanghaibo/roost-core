package skill

// RR-20261005-NC-117：CompletedCastLimit 承诺“有界保留终态 cast”，checkpoint 恢复也按“所有 finished / failed”
// 重建完成队列并以 CompletedCastLimit 校验。旧实现的 trackCompletedCastLocked 只把 finished 与“已提交的 failed”
// 放进完成队列：提交前失败（施法窗口里 commit 付费不足、NC-111 修复后 Cancel / Release 回调失败）的 cast 永远
// 不进队列、永远不回收，live Runtime 无界增长；累计超过 CompletedCastLimit 之后，checkpoint 恢复时重建的
// 完成队列超限，整份 checkpoint 被判 corrupt。

import (
	"encoding/json"
	"errors"
	"testing"
)

func failBeforeCommit(t *testing.T, runtime *Runtime, program *Program, tick *Tick) CastID {
	t.Helper()
	id, err := runtime.Start(program, CastInput{Caster: 1, Target: 2})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	*tick += 3
	if err := runtime.Advance(*tick); !errors.Is(err, ErrInsufficientResource) {
		t.Fatalf("commit advance = %v, want ErrInsufficientResource", err)
	}
	if snapshot, _ := runtime.InspectCast(id); snapshot.Status != CastFailed || runtime.casts[id].committed {
		t.Fatalf("cast %d: status=%s committed=%v, want an uncommitted failure", id, snapshot.Status, runtime.casts[id].committed)
	}
	*tick += 10
	if err := runtime.Advance(*tick); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestUncommittedFailedCastsStayWithinTheCompletedCastLimit(t *testing.T) {
	program, environment := compileRuntimeFixture(t, "cast_window_interrupt.json")
	host := runtimeTestHost(environment)
	host.UpsertEntity(MemoryEntity{ID: 1, Alive: true, Health: 100, MaxHealth: 100, Resources: map[string]int64{"mana": 0}})
	options := RuntimeOptions{CompletedCastLimit: 2}
	runtime := NewRuntime(host, options)
	tick := Tick(0)
	for range 5 {
		failBeforeCommit(t, runtime, program, &tick)
	}
	if stats := runtime.RetentionStats(); stats.Casts > options.CompletedCastLimit {
		t.Errorf("retained casts = %d (completed queue %d) after 5 pre-commit failures; CompletedCastLimit is %d", stats.Casts, stats.CompletedCasts, options.CompletedCastLimit)
	}
	checkpoint, err := runtime.Checkpoint()
	if err != nil {
		t.Fatal(err)
	}
	restored, err := RestoreRuntime(host, options, checkpoint, ProgramResolverFunc(func(string, string) (*Program, error) { return program, nil }))
	if err != nil {
		t.Fatalf("restore after pre-commit failures: %v", err)
	}
	again, err := restored.Checkpoint()
	if err != nil {
		t.Fatal(err)
	}
	var before, after struct {
		CompletedCastOrder []CastID `json:"completed_cast_order"`
	}
	_ = json.Unmarshal(checkpoint.Payload, &before)
	_ = json.Unmarshal(again.Payload, &after)
	if len(before.CompletedCastOrder) != len(after.CompletedCastOrder) {
		t.Fatalf("completed queue live=%v restored=%v: restore rebuilt a different retention set", before.CompletedCastOrder, after.CompletedCastOrder)
	}
}

// NC-117 修复后失败启动会先进完成队列再被删除复用 ID：队列里不能残留指向被复用 ID 的条目。
func TestFailedStartLeavesNoCompletedQueueEntry(t *testing.T) {
	program, environment := compileTerminalCharge(t)
	host := runtimeTestHost(environment)
	setTerminalMana(host, 10)
	runtime := NewRuntime(host, RuntimeOptions{})
	if _, err := runtime.Start(program, CastInput{Caster: 1, Target: 2}); !errors.Is(err, ErrInsufficientResource) {
		t.Fatalf("start = %v", err)
	}
	if len(runtime.completedCastOrder) != 0 {
		t.Fatalf("completed queue after a deleted failed start = %v", runtime.completedCastOrder)
	}
}
