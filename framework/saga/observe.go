package saga

import (
	"log/slog"
	"strconv"
	"strings"

	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
)

// observe.go 是协调器判定之外的观察：重开、迟到成功、旧尝试 / 旧一生的结果。分区循环在判定之后调用，计数由调用方加。

// saga.reopened_total 的 reason 标签：重开从哪里来。取值固定三个，保持低基数。
const (
	// reopenLateSuccess：已结束的 Failed / Compensated 收到放弃后迟到的正向成功，协调器立即把它带回补偿。
	reopenLateSuccess = "late_success"
	// reopenResume / reopenCompensate：ManualRequired 期间记下的迟到步骤（只记 LateStep、不自动跑），运维 Resume / Compensate 时先补它。
	reopenResume     = "resume"
	reopenCompensate = "compensate"
)

// reportReopen 记一次 saga 重开（saga 方向 ④ 的可观测性）：before 是重开前的记录（它的状态就是原终态），after 是写入的记录
// （正在补偿迟到那一步）。按终态做业务的一方识别方法见 SAGA.md「运维观察」：记下读到终态时的 Version，之后 Version 变大就是
// 记录被改过（重开或 Resume）。标签只有 saga 类型、原状态与原因，saga id 与步骤号只进日志。
func reportReopen(before, after Record, reason string) {
	from := before.Status.String()
	metrics.IncCounter("saga.reopened_total", metrics.Labels{"saga_type": before.Type, "from_status": from, "reason": reason}, 1)
	slog.Warn("saga: reopened to compensate a step that took effect after the coordinator abandoned it",
		"saga_id", before.ID, "saga_type", before.Type, "from_status", from, "reason", reason,
		"late_step", after.LateStep-1, "status", after.Status.String(), "version", after.Version)
}

// reportLateCompensation 记一次由协调器自动补偿的迟到成功（saga 方向 ④）：仍计 late_after_abandon（phase=forward），
// 但已经有了归宿，只记 WARN。
func reportLateCompensation(record Record, completion Completion) {
	phase, step := operationPosition(completion.IdempotencyKey)
	metrics.IncCounter("saga.completion.late_after_abandon_total", metrics.Labels{"saga_type": record.Type, "phase": phase}, 1)
	slog.Warn("saga: step succeeded after the coordinator abandoned it; compensating it",
		"saga_id", completion.SagaID, "saga_type", record.Type, "status", record.Status.String(), "step", step,
		"command_id", completion.CommandID, "operation", completion.IdempotencyKey)
}

// reportStaleAttempt 记一份被拒收的同一生较早尝试的可重试失败（saga 方向 ③）：协调器在等之后的那次尝试。
func reportStaleAttempt(record Record, completion Completion) {
	phase, step := operationPosition(completion.IdempotencyKey)
	metrics.IncCounter("saga.completion.stale_attempt_total", metrics.Labels{"saga_type": record.Type, "phase": phase}, 1)
	slog.Warn("saga: ignored a retryable failure from an earlier attempt; waiting for the current attempt",
		"saga_id", completion.SagaID, "saga_type", record.Type, "phase", phase, "step", step,
		"command_id", completion.CommandID, "waiting_for", record.CommandID, "error", completion.Error)
}

// reportStaleIncarnation 记一份被拒收的旧一生结果。这是 Resume 之后的正常现象，不是故障：只计数、记 WARN。
func reportStaleIncarnation(record Record, completion Completion, incarnation uint32) {
	phase, step := operationPosition(completion.IdempotencyKey)
	metrics.IncCounter("saga.completion.stale_incarnation_total", metrics.Labels{"saga_type": record.Type, "phase": phase}, 1)
	slog.Warn("saga: ignored a step result from an earlier incarnation of the saga",
		"saga_id", completion.SagaID, "saga_type", record.Type, "phase", phase, "step", step, "command_id", completion.CommandID,
		"result_incarnation", incarnation, "incarnation", record.Incarnation, "success", completion.Success, "error", completion.Error)
}

// reportLateAfterAbandon 告警一份协调器不能自己处理的“放弃之后才到的成功”：补偿方向的操作（正向的由协调器补偿，saga
// 方向 ④），以及 lateForwardStep 判为不应发生的情形。补偿方向停在 ManualRequired 时 Resume，新一生的同一补偿会回放这次成功
// 而不是再执行一次。标签只有 saga 类型与方向，保持低基数。
func reportLateAfterAbandon(record Record, completion Completion) {
	phase, step := operationPosition(completion.IdempotencyKey)
	metrics.IncCounter("saga.completion.late_after_abandon_total", metrics.Labels{"saga_type": record.Type, "phase": phase}, 1)
	slog.Error("saga: step succeeded after the coordinator abandoned it; the coordinator cannot account for it",
		"saga_id", completion.SagaID, "saga_type", record.Type, "status", record.Status.String(), "phase", phase, "step", step,
		"command_id", completion.CommandID, "operation", completion.IdempotencyKey)
}

// operationPosition 从 operationKey（sagaID:phase:step，见 operationKey）取出方向与步骤号，用于告警标签与日志。
func operationPosition(operation string) (phase, step string) {
	cut := strings.LastIndexByte(operation, ':')
	if cut <= 0 {
		return "unknown", ""
	}
	rest, step := operation[:cut], operation[cut+1:]
	switch rest[strings.LastIndexByte(rest, ':')+1:] {
	case strconv.Itoa(int(PhaseForward)):
		return PhaseForward.String(), step
	case strconv.Itoa(int(PhaseCompensate)):
		return PhaseCompensate.String(), step
	default:
		return "unknown", step
	}
}
