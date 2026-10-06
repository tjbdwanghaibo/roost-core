package saga

import "context"

// 协调器写记录的唯一转移（saga 方向 ①，维护者第六轮决定，方案
// docs/feature/SAGA-DIRECTION-STEP-TRANSITION-AND-MONGO-INBOX-2026-10-06.md）。
//
// “离开当前步骤时关闭哪个操作”曾经在截止、人工 Compensate、定义缺失、超时、接收结果几个出口各写一遍，
// “放弃关闭”这条子规则因此在三个出口各漏过一次（U-0280 补截止与人工 Compensate，RR-20261005-NC-250 补定义缺失）。
// 现在每个出口只算出目标状态 after，交给 stepTransition 统一决定：关闭哪个操作（tombstone 与排队命令的清理随之
// 在同一个 Store 事务里完成）、是否开新一生、带不带协调器租约，并由它把请求交给 Store。step_transition_guard_test.go
// 按类型检查全包，保证没有别的代码构造、改写请求或调用 Store.Apply（RR-20261006-14）。

// transitionCause 是协调器写一次记录的原因。
type transitionCause uint8

const (
	// causeDispatch：派发当前步骤的下一次尝试（不离开步骤，带新命令的 outbox）。
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
)

// transition 是一次写记录除目标状态以外的输入。
type transition struct {
	cause transitionCause
	// fenced：协调循环持有记录租约，写入同时核对它（ClaimDue 之后的出口）。Complete / Compensate / Resume
	// 不持租约，只按版本 fence。
	fenced bool
	// receipt：接收的结果，与状态推进在同一事务里记回执。Store 按 receipt.Success 决定关闭方式：
	// 只有接收成功才是“带结果关闭”，其余关闭都是“放弃关闭”（见 ApplyRequest）。
	receipt *Completion
	// outbox：派发的新命令；它替换同一操作仍排队的旧命令。
	outbox *OutboxRecord
}

// stepTransition 把 before → after 写成一次记录转移，是协调器写记录（Store.Apply）的唯一入口：ApplyRequest 只在
// 这里构造、只在这里交给 Store，调用方拿到的是写入的记录，拿不到可改写的请求。
//
//  1. 关闭：before 开着的操作（openOperation）在 after 里不再开着，就关闭它；接收了成功结果时关闭结果所属的操作
//     （Resume 之后还没派发、记录停在该操作上时 before 没有开着的操作，B1）。
//  2. 代际：after.Incarnation 只由 before 与原因决定，调用方在 after 上写的值不起作用。Resume 总是开新一生；
//     人工 Compensate 只在补偿方向停下的记录上开（B1：要重新执行的补偿步骤在这一生里已经派发过，不换代会复用
//     上一轮的 CommandID，收件箱只会回放旧的拒绝或报身份冲突）。
//  3. 租约：fenced 时带 before 的租约。
//
// 为什么做成 Engine 方法并在里面调 Store.Apply（RR-20261006-14）：原来它只返回 ApplyRequest，由出口自己调
// e.store.Apply，守卫按语法检查出口，看不到包级 helper 改写参数里的请求再写入（例如把 CloseOperation 清空）。
// 现在出口手里没有请求可改；step_transition_guard_test.go 按类型检查全包非测试代码：ApplyRequest 的值只能在这里
// 产生、任何地方都不能改写、Store.Apply 只在这里调用。
func (e *Engine) stepTransition(ctx context.Context, before, after Record, t transition) (Record, ApplyOutcome, error) {
	after.Incarnation = before.Incarnation
	if t.cause == causeResume || (t.cause == causeManualCompensate && before.Phase == PhaseCompensate) {
		after.Incarnation++
	}
	request := ApplyRequest{ExpectedVersion: before.Version, After: after, Outbox: t.outbox, Receipt: t.receipt}
	if t.fenced {
		request.ExpectedLease = before.Lease
	}
	if open := openOperation(before); open != "" && open != openOperation(after) {
		request.CloseOperation = open
	}
	if t.receipt != nil && t.receipt.Success {
		request.CloseOperation = t.receipt.IdempotencyKey
	}
	outcome, err := e.store.Apply(ctx, request)
	return request.After, outcome, err
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
