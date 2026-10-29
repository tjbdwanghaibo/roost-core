package saga

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"strconv"
	"strings"
	"time"
)

// transition.go 是 saga 的纯状态机：输入一份记录（与需要时的定义、结果、时刻），算出目标记录与这次转移的原因、
// 要关闭的操作、要派发的命令。这里不读写日志、不发布、不计数、不记日志——那些留给调用方（分区循环，partition.go）。
// “完成判定”“代际”“迟到成功补偿”等规则只有这一份（从原 Redis 协调器原样抽出，设计见
// docs/maintenance/DESIGN-2026-10-28-saga-jetstream-authority.md §0 第 2 条）。
//
// 协调器写记录的唯一转移（saga 方向 ①，见 docs/framework/SAGA.md）：截止、人工 Compensate、定义缺失、超时、接收结果等
// 出口都只算出目标状态 after，交给 planTransition 统一决定关闭哪个操作、是否开新一生。“离开当前步骤时关闭哪个操作”若在
// 各出口各写一遍，“放弃关闭”这条子规则就会在某个出口漏掉。行为回归见 completion_test.go 的
// TestStepTransitionAloneDecidesTheIncarnation 与各出口的关闭 / 代际用例。

// transitionCause 是协调器写一次记录的原因。
type transitionCause uint8

const (
	// causeDispatch：派发当前步骤的下一次尝试（不离开步骤，事件带新命令）。
	causeDispatch transitionCause = iota + 1
	// causeResult：接收了一次尝试的结果（成功、拒绝或可重试失败）。
	causeResult
	// causeTimeout：等结果超时（重试或用尽）。
	causeTimeout
	// causeDeadline：saga 截止，正向中止进入补偿或 Failed。
	causeDeadline
	// causeDefinitionMissing：定义版本缺失，fence 到 ManualRequired。
	causeDefinitionMissing
	// causeInvalidStep：步骤下标超出定义，fence 到 ManualRequired。
	causeInvalidStep
	// causeManualCompensate：人工 Compensate。
	causeManualCompensate
	// causeResume：人工 / 自动 Resume。
	causeResume
	// causeLateSuccess：放弃之后才到的正向成功，记下回执并补偿那一步（saga 方向 ④）。
	causeLateSuccess
)

// transition 是一次写记录除目标状态以外的输入。
type transition struct {
	cause transitionCause
	// receipt：接收的结果，与状态推进在同一条事件里记回执。分区按 receipt.Success 决定关闭方式：
	// 只有接收成功才是“带结果关闭”，其余关闭都是“放弃关闭”（OperationAbandoned）。
	receipt *Completion
}

// planTransition 把 before → after 定成一次记录转移，返回实际写入的 after 与要关闭的操作（空串表示不关闭）：
//
//  1. 关闭：before 开着的操作（openOperation）在 after 里不再开着，就关闭它；接收了成功结果时关闭结果所属的操作
//     （Resume 之后还没派发、记录停在该操作上时 before 没有开着的操作，B1；放弃后迟到的正向成功，它的操作早已放弃关闭，
//     这里改为带结果关闭，saga 方向 ④）。
//  2. 代际：after.Incarnation 只由 before 与原因决定，调用方在 after 上写的值不起作用。Resume 总是开新一生；
//     人工 Compensate 只在补偿方向停下的记录上开（B1：要重新执行的补偿步骤在这一生里已经派发过，不换代会复用
//     上一轮的 CommandID，收件箱只会回放旧的拒绝或报身份冲突）。
//
// 分区写记录都先经过它，再把结果写进事件。
func planTransition(before, after Record, t transition) (Record, string) {
	after.Incarnation = before.Incarnation
	if t.cause == causeResume || (t.cause == causeManualCompensate && before.Phase == PhaseCompensate) {
		after.Incarnation++
	}
	closeOperation := ""
	if open := openOperation(before); open != "" && open != openOperation(after) {
		closeOperation = open
	}
	if t.receipt != nil && t.receipt.Success {
		closeOperation = t.receipt.IdempotencyKey
	}
	return after, closeOperation
}

// openOperation 是记录当前开着、还可能有尝试生效的操作：在等某次尝试（Waiting），或已派发过、正在重试退避
// （Pending / Compensating 且 Attempt > 0）。还没派发的步骤、终态以及 Failed / ManualRequired（进入它们的那次
// 转移已经关闭了操作）返回空串。
func openOperation(record Record) string {
	switch record.Status {
	case StatusWaiting:
		return record.OperationKey
	case StatusPending, StatusCompensating:
		if record.Attempt > 0 {
			return operationKey(record.ID, record.Phase, record.Step)
		}
	}
	return ""
}

// ---- 启动 ----

// prepareStart 规范化启动请求并做与存储无关的校验：业务键去空白、长度、Data 上限，截止时间规范到 UTC 毫秒。
// 定义是否注册由调用方先查（两种协调器查的地方不同）。
func prepareStart(request StartRequest, maxPayload int) (StartRequest, error) {
	request.BusinessKey = strings.TrimSpace(request.BusinessKey)
	request.DeadlineAt = canonicalDeadline(request.DeadlineAt)
	if request.BusinessKey == "" || len(request.BusinessKey) > 512 || len(request.Data) > maxPayload {
		return StartRequest{}, ErrInvalidRecord
	}
	return request, nil
}

// newStartRecord 是一笔新 saga 的第一版记录：Pending、正向第 0 步、版本 1，立即到期；启动摘要独立保存原始意图。
// request 已经过 prepareStart。
func newStartRecord(request StartRequest, id string, now time.Time) (Record, error) {
	record := Record{ID: id, Type: request.Type, DefinitionVersion: request.DefinitionVersion, BusinessKey: request.BusinessKey, Status: StatusPending, Phase: PhaseForward, Step: 0, Version: 1, Data: append([]byte(nil), request.Data...), NextRunAt: now, DeadlineAt: request.DeadlineAt, CreatedAt: now, UpdatedAt: now}
	// Data / DeadlineAt 是运行状态，原始启动身份必须独立持久保存。
	intentDigest, err := startIntentDigest(request)
	if err != nil {
		return Record{}, err
	}
	record.StartDigest = intentDigest
	if err := record.Validate(); err != nil {
		return Record{}, err
	}
	return record, nil
}

// startIntentDigest 使用规范化后的启动请求；ID 单独校验，Now 只用于首次调度。
// JSON 的 omitempty 保持既有 nil/空 Data 等价；UTC 毫秒截止时间与 Mongo 精度一致。
func startIntentDigest(request StartRequest) (string, error) {
	// 固定摘要格式，不因以后 StartRequest 增加选项而改变既存记录的身份。
	intent := struct {
		Type              string    `json:"type"`
		BusinessKey       string    `json:"business_key"`
		DefinitionVersion uint32    `json:"definition_version"`
		Data              []byte    `json:"data,omitempty"`
		DeadlineAt        time.Time `json:"deadline_at,omitempty"`
	}{request.Type, request.BusinessKey, request.DefinitionVersion, request.Data, request.DeadlineAt}
	raw, err := json.Marshal(intent)
	if err != nil {
		return "", fmt.Errorf("%w: start intent digest: %v", ErrInvalidRecord, err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

// ---- 到期 ----

// duePlan 是一条到期记录的处理结论：转移原因、目标记录，派发时还有要发出的命令。
type duePlan struct {
	cause   transitionCause
	after   Record
	command *Command
}

// planDue 处理一条到期（NextRunAt 已到）的非终态记录，按顺序检查全部出口：定义缺失 → ManualRequired；正向越过 saga 截止 →
// 补偿；在等结果（Waiting）→ 超时重试或补偿；步骤越界 → ManualRequired；否则派发当前步骤的下一次尝试。defined 表示
// 记录的定义版本已注册（definition 只在 defined 时有效）。after 还没经过 planTransition（代际与关闭由它定）。
func planDue(record Record, definition Definition, defined bool, now time.Time) duePlan {
	if !defined {
		after := record.Clone()
		after.Status = StatusManualRequired
		after.LastError = fmt.Sprintf("definition not registered: %s/%d", record.Type, record.DefinitionVersion)
		after.Version++
		after.UpdatedAt = now
		after.NextRunAt = time.Time{}
		after.CommandID = ""
		after.OperationKey = ""
		// 在等结果或正在重试退避的操作都被放弃（planTransition 关闭它）。
		return duePlan{cause: causeDefinitionMissing, after: after}
	}
	if !record.DeadlineAt.IsZero() && !now.Before(record.DeadlineAt) && record.Phase == PhaseForward {
		return duePlan{cause: causeDeadline, after: beginCompensation(record, "saga deadline exceeded", now)}
	}
	if record.Status == StatusWaiting {
		return duePlan{cause: causeTimeout, after: retryOrCompensate(record, definition, "step result timeout", now)}
	}
	step, ok := stepFor(record, definition)
	if !ok {
		after := record.Clone()
		after.Status = StatusManualRequired
		after.LastError = "invalid saga step"
		after.Version++
		after.UpdatedAt = now
		return duePlan{cause: causeInvalidStep, after: after}
	}
	after := record.Clone()
	after.Status = StatusWaiting
	after.Attempt++
	after.Version++
	after.UpdatedAt = now
	after.NextRunAt = now.Add(step.Timeout)
	after.OperationKey = operationKey(record.ID, record.Phase, record.Step)
	after.CommandID = commandID(after.OperationKey, after.Incarnation, after.Attempt)
	topic := step.ForwardTopic
	if record.Phase == PhaseCompensate {
		topic = step.CompensateTopic
		if topic == "" {
			topic = step.ForwardTopic + ".compensate"
		}
	}
	payload := record.Data
	if compensatingLateStep(record) {
		// 补偿迟到生效的那一步，载荷是它自己的正向成功 Data（saga 方向 ④）。
		payload = record.LateData
	}
	command := Command{ID: after.CommandID, IdempotencyKey: after.OperationKey, SagaID: record.ID, SagaType: record.Type, DefinitionVersion: record.DefinitionVersion, BusinessKey: record.BusinessKey, Step: record.Step, StepName: step.Name, Phase: record.Phase, Attempt: after.Attempt, Topic: topic, Payload: append([]byte(nil), payload...), DeadlineAt: after.NextRunAt, CreatedAt: now}
	return duePlan{cause: causeDispatch, after: after, command: &command}
}

// ---- 运维：Resume / Compensate ----

// prepareResume 规范化 Resume 请求：ID 去空白、Now 规范到 UTC（零值取 clock），截止时间规范到毫秒；拒绝非法 ID、
// “同时设截止又清截止”与不在未来的新截止。返回规范化后的请求（Now 已填）与新截止。
func prepareResume(request ResumeRequest, clock time.Time) (ResumeRequest, time.Time, error) {
	request.ID = strings.TrimSpace(request.ID)
	if !validSubjectToken(request.ID, 128) || (request.ClearDeadline && !request.DeadlineAt.IsZero()) {
		return ResumeRequest{}, time.Time{}, ErrInvalidRecord
	}
	if request.Now.IsZero() {
		request.Now = clock.UTC()
	} else {
		request.Now = request.Now.UTC()
	}
	deadline := canonicalDeadline(request.DeadlineAt)
	if !deadline.IsZero() && !deadline.After(request.Now) {
		return ResumeRequest{}, time.Time{}, ErrDeadlineExpired
	}
	return request, deadline, nil
}

// planResume 是 Resume 的目标记录：只有 Failed / ManualRequired 能 Resume；定义必须已注册；补偿方向停下、或有已完成
// 前缀 / 迟到步骤的记录继续补偿（先补迟到那一步，saga 方向 ④），否则回到正向重试当前步骤。新一生由 planTransition 开
// （causeResume），之后的 CommandID 与 Resume 之前的所有回执不相交。request 已经过 prepareResume。
func planResume(record Record, defined bool, request ResumeRequest, deadline time.Time) (Record, error) {
	if record.Status != StatusFailed && record.Status != StatusManualRequired {
		return Record{}, fmt.Errorf("saga: status %d cannot resume", record.Status)
	}
	if !defined {
		return Record{}, fmt.Errorf("%w: %s/%d", ErrDefinitionMissing, record.Type, record.DefinitionVersion)
	}
	now := request.Now
	after := record.Clone()
	after.Version++
	after.UpdatedAt = now
	after.Attempt = 0
	after.LastError = ""
	after.CommandID = ""
	after.OperationKey = ""
	after.NextRunAt = now
	if request.ClearDeadline {
		after.DeadlineAt = time.Time{}
	} else if !deadline.IsZero() {
		after.DeadlineAt = deadline
	} else if after.Phase == PhaseForward && !after.DeadlineAt.IsZero() && !now.Before(after.DeadlineAt) {
		return Record{}, ErrDeadlineExpired
	}
	if record.Phase == PhaseCompensate || record.CompletedSteps > 0 || record.LateStep > 0 {
		// 补偿：先补放弃后才生效的那一步（saga 方向 ④），再按倒序补已完成的前缀。
		after = nextCompensation(after, now)
	} else {
		after.Phase = PhaseForward
		after.Status = StatusPending
	}
	return after, nil
}

// planCompensate 是人工 Compensate 的判定：write=true 时 after 是要写入的补偿状态（当前步骤正在重试退避时放弃它；
// 补偿方向停下的记录进入新一生，B1——两者都由 planTransition 决定）；write=false 时按 (after, err) 原样回答调用方：
// 已在补偿或已补偿返回记录本身，Completed 返回记录与 ErrSagaCompleted（维护者 2026-10-07 选 A：撤销已完成的业务走业务
// 自己的冲正流程），在等结果或没有可补偿的步骤返回错误。
func planCompensate(record Record, reason string, now time.Time) (Record, bool, error) {
	if record.Status == StatusCompensated || record.Status == StatusCompensating {
		return record, false, nil
	}
	if record.Status == StatusWaiting {
		return Record{}, false, fmt.Errorf("saga: cannot force compensation while a step result is in flight")
	}
	if record.Status == StatusCompleted {
		return record, false, ErrSagaCompleted
	}
	if record.CompletedSteps == 0 && record.LateStep == 0 {
		return Record{}, false, fmt.Errorf("saga: no completed steps to compensate")
	}
	return beginCompensation(record, reason, now), true, nil
}

// ---- 结果 ----

// completionVerdict 是 judgeCompletion 的结论，协调器按它走四个去向之一。
type completionVerdict int

const (
	// verdictAccept：接收为记录所停操作的结果（applyCompletion + 转移，成功时带结果关闭这个操作）。
	verdictAccept completionVerdict = iota
	// verdictHistory：记录不在这个操作上（或没在等这个结论），按回执与 tombstone 判断（judgeHistory）：
	// 已带结果关闭 → 重复；放弃关闭后迟到的成功 → 补偿这一步（方向 ④）或告警；从没开过 → ErrNotWaiting。
	verdictHistory
	// verdictStaleIncarnation：上一生的拒绝 / 失败（B1），或协调器不可能产生的更新代际；只计数。
	verdictStaleIncarnation
	// verdictStaleAttempt：较早一次尝试的可重试失败（方向 ③）；只计数。
	verdictStaleAttempt
)

// judgeCompletion 是 saga 完成判定的唯一规则（不再按记录状态逐条列举分支）：
//
//   - 成功是操作的结论，不分代际、不分尝试：只要这个操作开过、还没有带结果关闭就接收。记录停在这个操作上（positionedAt：
//     在等它、它在重试退避、或下一步就派发它）且 completion 所属的那一生开过它（本生：记录开着它；旧一生：CommandID 的代际
//     更旧）时，操作必定没带结果关闭（离开后回到同一操作键只有开新一生的 Resume / 人工 Compensate，且都不会回到已带结果
//     关闭的步骤），直接接收为结果；其余由回执与 tombstone 区分放弃关闭（仍接收，落点是方向 ④ 补偿这一步）、带结果关闭
//     （重复）与从没开过（ErrNotWaiting）。
//   - 拒绝是本生这个操作的结论：本生、记录在等这个操作时接收，从哪次尝试来都一样（收件箱让之后的尝试只回放它）。
//   - 可重试失败是一次尝试的结论：只接收正在等的那次尝试的（方向 ③）。
//   - 比记录还新的代际协调器不可能产生，一律不接收。
//
// 收件箱的每操作状态文档保证同一操作至多一次生效，所以“未带结果关闭的成功”就是还没计过的那一次。
func judgeCompletion(record Record, completion Completion, incarnation uint32) completionVerdict {
	operation := completion.IdempotencyKey
	switch {
	case incarnation > record.Incarnation:
		return verdictStaleIncarnation
	case completion.Success:
		// 协调器开过这个操作（本生开着它，或 completion 所属的旧一生派发过它）且记录还停在它上面：接收为结果。
		opened := openOperation(record) == operation || incarnation < record.Incarnation
		if opened && positionedAt(record, operation) {
			return verdictAccept
		}
		return verdictHistory
	case incarnation != record.Incarnation:
		return verdictStaleIncarnation
	case record.Status != StatusWaiting || record.OperationKey != operation:
		return verdictHistory
	case completion.Retryable && completion.CommandID != record.CommandID:
		return verdictStaleAttempt
	default:
		return verdictAccept
	}
}

// positionedAt 判断记录是否正停在这个操作上：在等它的某次尝试，或者下一步就要派发它（重试退避中、Resume 之后还没派发）。
func positionedAt(record Record, operation string) bool {
	switch record.Status {
	case StatusWaiting:
		return record.OperationKey == operation
	case StatusPending, StatusCompensating:
		return operationKey(record.ID, record.Phase, record.Step) == operation
	default:
		return false
	}
}

// completionHistory 是协调器对一份“不在等待中”的 completion 的了解（分区状态里的回执与操作 tombstone，stateHistory 给出）：
// 用来区分“重复的结果”和“放弃之后才到的成功”——后者说明那一步已经生效、却没有被纳入补偿。
type completionHistory struct {
	// Recorded：这份 completion 已被接收，或它的 operation 已关闭。
	Recorded bool
	// Receipt：这个 CommandID 的 completion 本身已被接收（不只是 operation 已关闭）。
	Receipt bool
	// Closure 是 operation tombstone 记录的关闭方式。
	Closure OperationClosure
}

// historyVerdict 是 judgeHistory 的结论：协调器不接收为当前结果的 completion 按回执与 tombstone 走哪个去向。
type historyVerdict uint8

const (
	// historyNotWaiting：这个操作从没开过、这次尝试也没有回执——协调器既不在等它、也没有它的记录（ErrNotWaiting）。
	historyNotWaiting historyVerdict = iota + 1
	// historyDuplicate：已接收过的回执，或操作已关闭（带结果关闭之后的重投、放弃关闭后重复送达的失败等）；只计重复。
	historyDuplicate
	// historyLateStep：放弃关闭之后才到的正向成功，协调器补偿那一步（saga 方向 ④，lateStepState）。
	historyLateStep
	// historyLateAlarm：放弃关闭之后才到、协调器不能补偿的成功（补偿方向的操作，或 lateForwardStep 判为不应发生）：
	// 按（操作，代际）只告警一次，之后的送达按重复处理。
	historyLateAlarm
)

// judgeHistory 判定 judgeCompletion 给出 verdictHistory 的 completion。history 是存储对它的了解（回执、tombstone 与
// 关闭方式）；historyLateStep 时第二个返回值是要补偿的正向步骤号。
func judgeHistory(record Record, completion Completion, history completionHistory) (historyVerdict, int) {
	if !history.Recorded {
		return historyNotWaiting, 0
	}
	if completion.Success && !history.Receipt && history.Closure == OperationAbandoned {
		if step, ok := lateForwardStep(record, completion.IdempotencyKey); ok {
			return historyLateStep, step
		}
		return historyLateAlarm, 0
	}
	return historyDuplicate, 0
}

// lateForwardStep 判断一份放弃后迟到的成功能否由协调器补偿（saga 方向 ④），返回那一步的步骤号。只处理正向操作，
// 且记录已离开正向（补偿方向、Failed，或等运维的 ManualRequired）。下面几种按 4.3 的论证不会发生，出现时退回只告警：
// 记录还在正向非终态、已记着另一个迟到步骤、或这一步在已完成前缀之内。
func lateForwardStep(record Record, operation string) (int, bool) {
	phase, step, ok := parseOperationKey(record.ID, operation)
	if !ok || phase != PhaseForward || step < record.CompletedSteps || (record.LateStep != 0 && record.LateStep != step+1) {
		return 0, false
	}
	return step, record.Phase == PhaseCompensate || record.Status == StatusFailed || record.Status == StatusManualRequired
}

// lateStepState 是 saga 方向 ④ 的目标记录：第 step 步在协调器放弃之后生效了，把它记成迟到步骤（LateStep / LateData）：
//
//   - 记录在两个补偿之间（Compensating 且还没派发）或已经终态（Failed / Compensated）：立即转去补偿这一步，终态被重开；
//   - 某个补偿在等结果或在重试退避：不打断它（它可能生效），它接收结果后 nextCompensation 先补这一步；
//   - ManualRequired：只记下，运维 Resume / Compensate 时先补这一步（等运维的记录不自动跑）。
//
// 这次转移带这份 completion 的回执（causeLateSuccess），planTransition 把 tombstone 改为带结果关闭，之后的送达按回执去重。
func lateStepState(record Record, completion Completion, step int) Record {
	now := completion.CompletedAt
	after := record.Clone()
	after.Version++
	after.UpdatedAt = now
	after.LateStep = step + 1
	after.LateData = append([]byte(nil), completion.Data...)
	if openOperation(record) == "" && record.Status != StatusManualRequired {
		after.LastError = fmt.Sprintf("step %d took effect after the coordinator abandoned it", step)
		after = nextCompensation(after, now)
	}
	return after
}

// nextCompensation 选下一个要补偿的步骤：先补放弃后才生效的那一步（LateStep，saga 方向 ④），再按 CompletedSteps 倒序；
// 都补完是 Compensated。开始补偿（compensationState）、补偿成功（applyCompletion）、Resume、迟到成功到达都经这里，
// 选择规则只有这一处。after 的版本、时间、错误由调用方设置。
func nextCompensation(after Record, now time.Time) Record {
	after.Phase = PhaseCompensate
	after.Attempt = 0
	after.CommandID = ""
	after.OperationKey = ""
	switch {
	case after.LateStep > 0:
		after.Status = StatusCompensating
		after.Step = after.LateStep - 1
		after.NextRunAt = now
	case after.CompletedSteps > 0:
		after.Status = StatusCompensating
		after.Step = after.CompletedSteps - 1
		after.NextRunAt = now
	default:
		after.Status = StatusCompensated
		after.NextRunAt = time.Time{}
	}
	return after
}

// compensatingLateStep 表示记录当前的补偿操作是迟到的那一步（不是已完成前缀里的一步）。
func compensatingLateStep(record Record) bool {
	return record.Phase == PhaseCompensate && record.LateStep > 0 && record.Step == record.LateStep-1
}

// applyCompletion 是接收一份结果（judgeCompletion 判为 verdictAccept）之后的目标记录：成功推进到下一步 / 完成 / 下一个
// 补偿；拒绝在正向转补偿、在补偿方向转 ManualRequired；可重试失败按步骤预算重试或转补偿。result.CompletedAt 是协调器时间。
func applyCompletion(record Record, definition Definition, result Completion) Record {
	now := result.CompletedAt
	after := record.Clone()
	after.Version++
	after.UpdatedAt = now
	after.CommandID = ""
	after.OperationKey = ""
	if result.Success {
		late := compensatingLateStep(record)
		if result.Data != nil && !late {
			after.Data = append(after.Data[:0], result.Data...)
		}
		after.Attempt = 0
		after.LastError = ""
		if record.Phase == PhaseForward {
			after.CompletedSteps = record.Step + 1
			after.Step++
			if !record.DeadlineAt.IsZero() && !now.Before(record.DeadlineAt) {
				return compensationState(after, "saga deadline exceeded after step completion", now)
			}
			if after.Step >= len(definition.Steps) {
				after.Status = StatusCompleted
				after.NextRunAt = time.Time{}
			} else {
				after.Status = StatusPending
				after.NextRunAt = now
			}
		} else {
			if late {
				// 迟到的那一步补偿完了（saga 方向 ④）：它不在已完成前缀里，CompletedSteps 不变；它的结果不进入 Data 链。
				after.LateStep, after.LateData = 0, nil
			} else {
				after.CompletedSteps--
			}
			after = nextCompensation(after, now)
		}
		return after
	}
	if !result.Retryable {
		if after.Phase == PhaseCompensate {
			after.Status = StatusManualRequired
			after.NextRunAt = time.Time{}
			return after
		}
		return compensationState(after, result.Error, now)
	}
	return retryState(after, definition, result.Error, now)
}

// retryOrCompensate 是从已存记录出发的转移（到期循环的超时路径）：推进一次版本，再决定重试还是补偿。applyCompletion
// 已经推进过版本，直接调用 retryState（见 beginCompensation）。
func retryOrCompensate(record Record, definition Definition, reason string, now time.Time) Record {
	after := record.Clone()
	after.Version = record.Version + 1
	return retryState(after, definition, reason, now)
}

// retryState 在尝试次数未用尽时安排当前步骤的下一次尝试，否则转入补偿（失败的步骤本身是补偿时转 ManualRequired）。
// after 的版本已由调用方推进。
func retryState(after Record, definition Definition, reason string, now time.Time) Record {
	record := after
	after.UpdatedAt = now
	after.LastError = reason
	after.CommandID = ""
	after.OperationKey = ""
	step, ok := stepFor(record, definition)
	if !ok {
		after.Status = StatusManualRequired
		after.NextRunAt = time.Time{}
		return after
	}
	if after.Attempt < step.MaxAttempts {
		if after.Phase == PhaseForward {
			after.Status = StatusPending
		} else {
			after.Status = StatusCompensating
		}
		after.NextRunAt = now.Add(backoff(record.ID, after.Attempt, step.BackoffMin, step.BackoffMax))
		return after
	}
	if after.Phase == PhaseCompensate {
		after.Status = StatusManualRequired
		after.NextRunAt = time.Time{}
		return after
	}
	return compensationState(after, reason, now)
}

// beginCompensation 是从已存记录出发的转移：推进一次版本，再把记录转成补偿状态。已经推进过版本的调用方
// （applyCompletion、retryOrCompensate）直接用 compensationState——对已推进的副本再调用本函数会把版本推进两次。
func beginCompensation(record Record, reason string, now time.Time) Record {
	after := record.Clone()
	after.Version = record.Version + 1
	return compensationState(after, reason, now)
}

// compensationState 在调用方已推进版本的记录上填入补偿（或失败终态）状态。
func compensationState(after Record, reason string, now time.Time) Record {
	after.UpdatedAt = now
	after.LastError = reason
	after.Attempt = 0
	after.CommandID = ""
	after.OperationKey = ""
	if after.CompletedSteps == 0 && after.LateStep == 0 {
		after.Status = StatusFailed
		after.NextRunAt = time.Time{}
		return after
	}
	return nextCompensation(after, now)
}

// ---- 身份与小工具 ----

func stepFor(record Record, definition Definition) (Step, bool) {
	if record.Step < 0 || record.Step >= len(definition.Steps) {
		return Step{}, false
	}
	return definition.Steps[record.Step], true
}

func operationKey(id string, phase Phase, step int) string {
	return fmt.Sprintf("%s:%d:%d", id, phase, step)
}

// parseOperationKey 是 operationKey 的逆：取出这个 saga 的操作的方向与步骤号；不是这个 saga 的操作返回 false。
func parseOperationKey(id, operation string) (Phase, int, bool) {
	rest, ok := strings.CutPrefix(operation, id+":")
	if !ok {
		return 0, 0, false
	}
	phaseText, stepText, ok := strings.Cut(rest, ":")
	if !ok {
		return 0, 0, false
	}
	phase, err := strconv.Atoi(phaseText)
	if err != nil || (Phase(phase) != PhaseForward && Phase(phase) != PhaseCompensate) {
		return 0, 0, false
	}
	step, err := strconv.Atoi(stepText)
	if err != nil || step < 0 {
		return 0, 0, false
	}
	return Phase(phase), step, true
}

// commandID 命名一次派发尝试。第 0 代使用 "operationKey:attempt" 格式；Resume 过的记录代际非零，铸出的标识与之前每一生
// 都不相交。
func commandID(operationKey string, incarnation, attempt uint32) string {
	if incarnation == 0 {
		return fmt.Sprintf("%s:%d", operationKey, attempt)
	}
	return fmt.Sprintf("%s:r%d:%d", operationKey, incarnation, attempt)
}

// commandIDIncarnation 是 commandID 的逆：从 operationKey:attempt / operationKey:rN:attempt 取出代际 N。
// 不是这个格式的 CommandID（测试或手工命令）按第 0 代处理。协调器核对 completion 的代际（B1）与原生收件箱
// 判断拒绝是否同一生都用它，两边对同一个 ID 必须得出同一个代际。
func commandIDIncarnation(operationKey, commandID string) uint32 {
	rest, ok := strings.CutPrefix(commandID, operationKey+":r")
	if !ok {
		return 0
	}
	digits, _, ok := strings.Cut(rest, ":")
	if !ok {
		return 0
	}
	value, err := strconv.ParseUint(digits, 10, 32)
	if err != nil {
		return 0
	}
	return uint32(value)
}

func canonicalDeadline(value time.Time) time.Time {
	if value.IsZero() {
		return time.Time{}
	}
	// BSON datetime 与许多 SQL 后端只保存到毫秒；在身份比较之前规范化，精确重投的启动意图才保持幂等。
	return value.UTC().Truncate(time.Millisecond)
}

func backoff(key string, attempt uint32, minimum, maximum time.Duration) time.Duration {
	if minimum <= 0 {
		minimum = time.Millisecond
	}
	if maximum < minimum {
		maximum = minimum
	}
	d := minimum
	for i := uint32(1); i < attempt && d < maximum; i++ {
		if d > maximum/2 {
			d = maximum
			break
		}
		d *= 2
	}
	if d > maximum {
		d = maximum
	}
	if d <= 1 {
		return d
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(key))
	var raw [4]byte
	binaryPutUint32(raw[:], attempt)
	_, _ = h.Write(raw[:])
	half := d / 2
	return half + time.Duration(uint64(h.Sum32())%uint64(d-half+1))
}

func binaryPutUint32(dst []byte, value uint32) {
	dst[0] = byte(value >> 24)
	dst[1] = byte(value >> 16)
	dst[2] = byte(value >> 8)
	dst[3] = byte(value)
}
