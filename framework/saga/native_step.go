package saga

import (
	"context"
	"errors"
	"fmt"
	"time"

	coredata "github.com/tjbdwanghaibo/roost-core/framework/dataengine"
)

// 原生步骤的业务侧形状（SAGA.md「原生步骤执行契约」）。业务只写步骤体：拿到 NativeStep，把它作为参数交给自己的
// Nest handler，在 handler 的事务里改实体并调用 step.Complete。收件箱预留、回执、completion 与等待投影都由框架做。
//
//	body := func(ctx context.Context, step saga.NativeStep) error {
//		state, err := decode(step.Command().Payload)
//		if err != nil {
//			return err
//		}
//		_, err = syncsender.NewDebitSender(nest).Sync_Debit(ctx, state.EntityID, state.Amount, step)
//		return err
//	}
//
//	//roost:nest rollback=undo durability=strict
//	func handlerDebit(target player.IBagEntity, amount int32, step saga.NativeStep) (int32, error) {
//		left, err := target.BagComp().Remove(amount)
//		if err != nil {
//			return 0, step.Complete(false, "insufficient")  // 业务拒绝也提交：只有回执与失败的 completion
//		}
//		return left, step.Complete(true, "")
//	}
//
// 契约：
//   - 幂等键是 Command.IdempotencyKey（一个操作实例：saga + 步骤 + 方向 + 代际）。同一操作实例的全部尝试里至多一次
//     生效，由收件箱的租约与投影事务里的 lease fence 保证，步骤体不需要自己的幂等。
//   - 生效点是 Nest 事务的投影：业务修改、命令回执与 completion 是同一条 WAL 记录，要么一起生效，要么都不生效。
//     重投读到回执就回放那份 completion，不再执行步骤体。
//   - 步骤体返回错误 = 结果未知（Nest 提交可能已经发生）：消费者保留这次尝试的租约并 nak，重投时等它的回执到命令
//     截止，之后由协调器按步骤超时发下一次尝试。只有 dataengine.ErrFencedEntityPending 交还租约、立刻重投。
//   - 补偿就是补偿方向的命令（Command.Phase = PhaseCompensate），同样走这里，同样至多生效一次；补偿被拒
//     （Complete(false, …)）时协调器把 saga 标为 manual_required。
//   - 步骤体只能用于业务本身就是一笔 Nest 事务的步骤；调用另一个服务的步骤用 Mongo 步骤并按 IdempotencyKey 幂等。

// NativeStep 是原生步骤体在 Nest 事务里提交结果所需的全部：命令、本次投递取得的执行权（租约 fence）与收件箱。
//
// 只由框架构造（DataEngineStepInbox.StepHandler、RunHandedOver），按值作为参数交给 Nest handler；零值的 Complete
// 返回错误。它只对这一次投递有效：不要保存，不要复制进脱离的 goroutine——两次投递共用一份执行权会让其中一次的
// fence 名不副实。
type NativeStep struct {
	inbox       *DataEngineStepInbox
	command     Command
	reservation Reservation
}

// NativeStepBody 是业务写的原生步骤体：在 Nest handler 里执行步骤并调用 step.Complete。返回 nil 表示 Nest 调用已
// 返回成功（结果随投影送达协调器）；返回错误见 NativeStep 的契约（结果未知）。
type NativeStepBody func(ctx context.Context, step NativeStep) error

// ErrNoNativeStep 表示原生步骤体不在原生步骤消费者里运行（ctx 上没有本次投递的执行权）。
var ErrNoNativeStep = errors.New("saga: no native step reservation on the context; run the body under SubscribeDataEngineStep or RunHandedOver")

// Command 是这一步的命令：载荷、IdempotencyKey、方向（Phase）与截止时间。
func (step NativeStep) Command() Command { return step.command }

// Complete 在当前 Nest 事务里绑定命令回执（带 lease fence）并发出 completion，必须在 Nest handler 内调用。
//
// 两样都要在事务里：回执让重投成为回放而不是第二次执行，completion 是协调器等的结果。业务拒绝同样调用它
// （success=false，不改实体，只提交回执与失败的 completion）：协调器听不到的拒绝只会等到步骤超时。拒绝不可重试，
// 协调器随即补偿已完成的步骤；reason 写进 completion 的 Error，空串时填一条通用说明。
func (step NativeStep) Complete(success bool, reason string) error {
	if step.inbox == nil {
		return ErrNoNativeStep
	}
	if err := step.inbox.Bind(step.command, step.reservation); err != nil {
		return err
	}
	completion := Completion{
		CommandID:      step.command.ID,
		IdempotencyKey: step.command.IdempotencyKey,
		SagaID:         step.command.SagaID,
		Success:        success,
	}
	if !success {
		completion.Error = reason
		if completion.Error == "" {
			completion.Error = "saga: native step " + step.command.StepName + " refused"
		}
	}
	return EmitCompletion(completion)
}

// StepHandler 把原生步骤体接到 SubscribeDataEngineStep：从 ctx 取本次投递的执行权构造 NativeStep 再调用 body。
// 返回的 Completion 恒为零值，消费者不使用它（原生步骤的结果是事务里 emit 的那一份）。
func (inbox *DataEngineStepInbox) StepHandler(body NativeStepBody) StepHandler {
	return func(ctx context.Context, command Command) (Completion, error) {
		if inbox == nil || body == nil {
			return Completion{}, ErrInvalidRecord
		}
		reservation, ok := ReservationFromContext(ctx)
		if !ok {
			return Completion{}, ErrNoNativeStep
		}
		return Completion{}, body(ctx, NativeStep{inbox: inbox, command: command, reservation: reservation})
	}
}

// RunHandedOver 在没有 JetStream 投递的路径上执行一条原生步骤命令：另一个进程判定命令“不归它”（StepConsumerConfig.Admit
// 拒绝）后把命令转交过来，本进程自己取执行权再运行 body。按 sid / 分片决定谁执行、怎么转交是业务的拓扑，框架只提供这一半。
//
// 与消费者取的是同一份执行权（同一操作实例的状态文档），所以两条路径互斥：谁先预留谁执行，另一条读到回执或在途租约。
// 返回 nil 的情形：命令已过截止（协调器按超时处理）、已有结果或另一次投递持有有效租约（Duplicate）、body 成功。
// 这里不等投影、不发布任何东西：原消息仍在转交方手里，它的重投在回执可见后 ack；转交丢失只多耗时间，不丢步骤。
// body 返回 dataengine.ErrFencedEntityPending 时交还租约（与消费者相同），其余错误保留租约到期，见 NativeStep 的契约。
func (inbox *DataEngineStepInbox) RunHandedOver(ctx context.Context, command Command, body NativeStepBody) error {
	if body == nil {
		return ErrInvalidRecord
	}
	if err := command.Validate(); err != nil {
		return err
	}
	// 截止判定在取执行权之前，与消费者同一口径（系统时钟，截止由协调器按它写）。
	if !time.Now().Before(command.DeadlineAt) {
		return nil
	}
	if inbox == nil || inbox.client == nil {
		return ErrInvalidRecord
	}
	if ctx == nil {
		ctx = context.Background()
	}
	processCtx, cancel := context.WithDeadline(ctx, command.DeadlineAt)
	defer cancel()
	reservation, err := inbox.Reserve(processCtx, command)
	switch {
	case errors.Is(err, ErrCommandExpired), errors.Is(err, errAttemptSuperseded), errors.Is(err, errOperationAttemptInFlight):
		// 不会在这里执行：截止已过、被较新尝试接替，或别人正持有执行权。转交方的重投负责收尾。
		return nil
	case err != nil:
		return fmt.Errorf("saga: reserve handed-over step: %w", err)
	case reservation.Duplicate:
		return nil
	}
	if err := body(processCtx, NativeStep{inbox: inbox, command: command, reservation: reservation}); err != nil {
		if errors.Is(err, coredata.ErrFencedEntityPending) {
			if releaseErr := inbox.releaseLease(context.WithoutCancel(ctx), reservation); releaseErr != nil {
				err = errors.Join(err, fmt.Errorf("saga: release unused step lease: %w", releaseErr))
			}
		}
		return err
	}
	return nil
}
