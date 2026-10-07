package saga

import (
	"bytes"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/metrics"
)

// saga 重开的可观测性（维护者第十三轮“saga 迟到成功重开终态”，选 A；方向 ④ 见
// docs/feature/SAGA-DIRECTION-3-4-2026-10-07.md）。
//
// 方向 ④ 让已结束的 Failed / Compensated 因为迟到的正向成功被重开、补偿那一步；ManualRequired 期间记下的迟到步骤在
// 运维 Resume / Compensate 时先补。承诺：每次重开计一次 saga.reopened_total{saga_type,from_status,reason}，并写一条 WARN
// （saga id、类型、原终态、迟到的步骤、原因），运维与按终态做业务的一方能看到“终态被改过”。
// 旧行为：重开只计 saga.completion.late_after_abandon_total{phase="forward"}（它按迟到成功计，不区分记录是否已终态），
// 没有重开计数，日志也不写原终态；ManualRequired 上 Resume 先补迟到步骤时没有任何专门的信号。

// reopenedCount 读 saga.reopened_total 中 saga_type=gift、给定原状态与原因的那条序列。
func reopenedCount(from, reason string) int64 {
	var total int64
	for _, metric := range metrics.Snapshot() {
		if metric.Name == "saga.reopened_total" && metric.Labels["saga_type"] == "gift" &&
			metric.Labels["from_status"] == from && metric.Labels["reason"] == reason {
			total += metric.Value
		}
	}
	return total
}

// lockedBuffer 让 slog 的写与测试的读不竞争。
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// captureWarnLogs 把默认 slog 换成写进缓冲的 WARN 级处理器，测试结束换回。
func captureWarnLogs(t *testing.T) *lockedBuffer {
	t.Helper()
	logs := &lockedBuffer{}
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return logs
}

// reopenLogLine 返回日志里那一条“saga 重开”的行；没有返回空串。
func reopenLogLine(logs *lockedBuffer, sagaID string) string {
	for _, line := range strings.Split(logs.String(), "\n") {
		if strings.Contains(line, "saga: reopened") && strings.Contains(line, "saga_id="+sagaID) {
			return line
		}
	}
	return ""
}

func assertReopenLog(t *testing.T, logs *lockedBuffer, sagaID string, fields ...string) {
	t.Helper()
	line := reopenLogLine(logs, sagaID)
	if line == "" {
		t.Fatalf("no WARN \"saga: reopened\" line for %s; logs:\n%s", sagaID, logs.String())
	}
	for _, field := range append([]string{"level=WARN", "saga_type=gift"}, fields...) {
		if !strings.Contains(line, field) {
			t.Errorf("reopen log line lacks %q: %s", field, line)
		}
	}
}

// Failed 被迟到的第 0 步成功重开：计一次 {from_status=failed, reason=late_success}，WARN 写明原终态与迟到步骤；
// 同一成功再次送达不再计。
func TestReopeningAFailedSagaIsCountedAndLogged(t *testing.T) {
	logs := captureWarnLogs(t)
	w := newNativeWorld(t, 1, time.Now().UTC())
	w.outcome = func(Command) Completion { return Completion{Success: true, Data: []byte("debit-receipt")} }
	w.tick(0)
	k := w.pendingCommand()
	recordK := w.commitAttempt(w.inboxA, k)
	if !w.project(recordK) {
		t.Fatal("attempt k projected within its deadline was skipped")
	}
	w.tick(5 * time.Second) // 唯一一次尝试超时 → Failed
	if record := w.record(); record.Status != StatusFailed {
		t.Fatalf("want Failed after the only attempt timed out: %+v", record)
	}
	before := reopenedCount("failed", "late_success")
	w.deliverEffect(recordK) // 迟到的成功把 Failed 重开
	if record := w.record(); record.Status != StatusCompensating {
		t.Fatalf("precondition (direction 4): the late success must reopen the saga: %+v", record)
	}
	if grown := reopenedCount("failed", "late_success") - before; grown != 1 {
		t.Fatalf("a Failed saga was reopened by a late success of %s, but saga.reopened_total{from_status=failed,reason=late_success} grew by %d, want 1",
			k.IdempotencyKey, grown)
	}
	assertReopenLog(t, logs, w.sagaID, "from_status=failed", "reason=late_success", "late_step=0")
	if got := w.engine.Stats().Reopened; got != 1 {
		t.Errorf("Stats().Reopened = %d, want 1", got)
	}

	w.tick(5 * time.Second)
	compensation := w.pendingCommand()
	w.completeCompensation(compensation, "")
	if record := w.record(); record.Status != StatusCompensated {
		t.Fatalf("after compensating the late step: %+v", record)
	}
	w.deliverEffect(recordK) // 重复送达：不是又一次重开
	if grown := reopenedCount("failed", "late_success") - before; grown != 1 {
		t.Errorf("saga.reopened_total grew by %d after a redelivery, want still 1", grown)
	}
}

// Compensated 被迟到的第 1 步成功重开：计一次 {from_status=compensated, reason=late_success}。
func TestReopeningACompensatedSagaIsCountedAndLogged(t *testing.T) {
	logs := captureWarnLogs(t)
	w, second := lateSecondStepWorld(t)
	w.tick(6 * time.Second)
	w.completeCompensation(w.pendingCommand(), "")
	if record := w.record(); record.Status != StatusCompensated {
		t.Fatalf("want Compensated before the late success arrives: %+v", record)
	}
	before := reopenedCount("compensated", "late_success")
	w.deliverEffects()
	if record := w.record(); record.Status != StatusCompensating || record.Step != 1 {
		t.Fatalf("precondition (direction 4): the compensated saga must be reopened: %+v", record)
	}
	if grown := reopenedCount("compensated", "late_success") - before; grown != 1 {
		t.Fatalf("a Compensated saga was reopened by a late success of %s, but saga.reopened_total{from_status=compensated,reason=late_success} grew by %d, want 1",
			second.IdempotencyKey, grown)
	}
	assertReopenLog(t, logs, w.sagaID, "from_status=compensated", "reason=late_success", "late_step=1")
}

// 迟到成功到达时补偿还在进行（记录不是终态）：不是重开，不计数。
func TestLateSuccessDuringCompensationIsNotAReopen(t *testing.T) {
	logs := captureWarnLogs(t)
	w, _ := lateSecondStepWorld(t)
	w.tick(6 * time.Second)
	w.pendingCommand() // 第 0 步的补偿在途
	beforeFailed, beforeCompensated := reopenedCount("failed", "late_success"), reopenedCount("compensated", "late_success")
	w.deliverEffects()
	if grown := reopenedCount("failed", "late_success") + reopenedCount("compensated", "late_success") - beforeFailed - beforeCompensated; grown != 0 {
		t.Fatalf("saga.reopened_total grew by %d for a saga that had not ended, want 0", grown)
	}
	if line := reopenLogLine(logs, w.sagaID); line != "" {
		t.Fatalf("a saga that had not ended was logged as reopened: %s", line)
	}
}

// ManualRequired 期间记下的迟到步骤：记下时不计（记录仍等运维），Resume 先补它时计一次 {from_status=manual_required, reason=resume}。
func TestResumeCompensatingALateStepIsCountedAndLogged(t *testing.T) {
	logs := captureWarnLogs(t)
	w, _ := lateSecondStepWorld(t)
	w.tick(6 * time.Second)
	compensateFirst := w.pendingCommand()
	if err := w.complete(Completion{CommandID: compensateFirst.ID, IdempotencyKey: compensateFirst.IdempotencyKey, SagaID: w.sagaID, Success: false, Error: "refund rejected"}); err != nil {
		t.Fatal(err)
	}
	before := reopenedCount("manual_required", "resume")
	w.deliverEffects() // 第 1 步的成功晚到，只记下 LateStep
	if record := w.record(); record.Status != StatusManualRequired || record.LateStep != 2 {
		t.Fatalf("precondition (direction 4): the late step must be recorded on ManualRequired: %+v", record)
	}
	if grown := reopenedCount("manual_required", "resume") - before; grown != 0 {
		t.Fatalf("saga.reopened_total grew by %d when the late step was only recorded, want 0 (the record still waits for the operator)", grown)
	}
	if _, err := w.engine.Resume(w.ctx, ResumeRequest{ID: w.sagaID, Now: w.at(7 * time.Second)}); err != nil {
		t.Fatal(err)
	}
	if grown := reopenedCount("manual_required", "resume") - before; grown != 1 {
		t.Fatalf("Resume compensates the late step 1 recorded during ManualRequired, but saga.reopened_total{from_status=manual_required,reason=resume} grew by %d, want 1", grown)
	}
	assertReopenLog(t, logs, w.sagaID, "from_status=manual_required", "reason=resume", "late_step=1")
}

// ManualRequired 上没有迟到步骤的普通 Resume 不是重开。
func TestPlainResumeIsNotAReopen(t *testing.T) {
	logs := captureWarnLogs(t)
	w, _ := lateSecondStepWorld(t)
	w.tick(6 * time.Second)
	compensateFirst := w.pendingCommand()
	if err := w.complete(Completion{CommandID: compensateFirst.ID, IdempotencyKey: compensateFirst.IdempotencyKey, SagaID: w.sagaID, Success: false, Error: "refund rejected"}); err != nil {
		t.Fatal(err)
	}
	before := reopenedCount("manual_required", "resume")
	if _, err := w.engine.Resume(w.ctx, ResumeRequest{ID: w.sagaID, Now: w.at(7 * time.Second)}); err != nil {
		t.Fatal(err)
	}
	if grown := reopenedCount("manual_required", "resume") - before; grown != 0 {
		t.Fatalf("a Resume without a late step grew saga.reopened_total by %d, want 0", grown)
	}
	if line := reopenLogLine(logs, w.sagaID); line != "" {
		t.Fatalf("a plain Resume was logged as a reopen: %s", line)
	}
}

// 人工 Compensate 与 Resume 等价（ManualRequired 上换代、先补迟到步骤）：计一次 {from_status=manual_required, reason=compensate}。
func TestManualCompensateOfALateStepIsCountedAndLogged(t *testing.T) {
	logs := captureWarnLogs(t)
	w, _ := lateSecondStepWorld(t)
	w.tick(6 * time.Second)
	compensateFirst := w.pendingCommand()
	if err := w.complete(Completion{CommandID: compensateFirst.ID, IdempotencyKey: compensateFirst.IdempotencyKey, SagaID: w.sagaID, Success: false, Error: "refund rejected"}); err != nil {
		t.Fatal(err)
	}
	w.deliverEffects() // 第 1 步的成功晚到，只记下 LateStep
	before := reopenedCount("manual_required", "compensate")
	record, err := w.engine.Compensate(w.ctx, w.sagaID, "operator", w.at(7*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if record.Status != StatusCompensating || record.Step != 1 {
		t.Fatalf("Compensate must compensate the late step first: %+v", record)
	}
	if grown := reopenedCount("manual_required", "compensate") - before; grown != 1 {
		t.Fatalf("saga.reopened_total{from_status=manual_required,reason=compensate} grew by %d, want 1", grown)
	}
	assertReopenLog(t, logs, w.sagaID, "from_status=manual_required", "reason=compensate", "late_step=1")
	if got := w.engine.Stats().Reopened; got != 1 {
		t.Errorf("Stats().Reopened = %d, want 1", got)
	}
}
