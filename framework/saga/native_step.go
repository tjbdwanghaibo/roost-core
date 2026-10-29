package saga

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"

	"github.com/tjbdwanghaibo/roost-core/framework/nest"
)

// 原生步骤的业务侧形状（SAGA.md「原生步骤执行契约」）。业务只写步骤体：拿到 NativeStep，把它作为参数交给自己的
// Nest handler，在 handler 的事务里改实体并调用 step.Complete。回执的读写、结果发布都由框架做。
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
//	//roost:nest rollback=undo durability=pipelined
//	func handlerDebit(target player.IBagEntity, amount int32, step saga.NativeStep) (int32, error) {
//		left, err := target.BagComp().Remove(amount)
//		if err != nil {
//			return 0, step.Complete(false, "insufficient")  // 业务拒绝也提交：只有回执
//		}
//		return left, step.Complete(true, "")
//	}
//
// 生成的 handler 包装在调用 handlerDebit 之前先 step.Begin(target)（选项 N-A，业务 handler 不写）。
//
// 契约（设计 §5.1）：
//   - 幂等键是 Command.IdempotencyKey（一个操作实例：saga + 步骤 + 方向）。同一操作实例的全部尝试里至多一次生效：回执放在
//     被改的实体里（roost add saga -native 生成回执字段），同一实体的事务由实体锁串行，回执与业务修改在同一条 WAL 记录里。
//     重投（结果未知、ack 丢失）在 Begin 读到回执就回放，不再执行业务。
//   - 生效点是 Nest 事务的 WAL 落盘（strict：锁内 fsync；pipelined：本事务的 WAL 票据落盘）；结论在落盘之后才交给消费者，
//     由它直接发回协调器分区，PubAck 之后 ack 命令。
//   - 步骤体返回错误 = 结果未知（Nest 提交可能已经发生）：消费者 nak 退避重投，重投时 Begin 读到回执就回放。
//   - 补偿就是补偿方向的命令（Command.Phase = PhaseCompensate），同样走这里，同样至多生效一次；补偿被拒
//     （Complete(false, …)）时协调器把 saga 标为 manual_required。
//   - 步骤体只能用于业务本身就是一笔 Nest 事务的步骤，且不能修改 Remote 实体（ErrNativeStepRemoteWrite）；调用另一个服务的
//     步骤用 Mongo 步骤并按 IdempotencyKey 幂等。

// NativeStep 是原生步骤体在 Nest 事务里提交结果所需的全部：命令与这一次运行（回执检查与结论）。
//
// 只由框架构造（RunNativeStep：步骤消费者与转交路径），按值作为参数交给 Nest handler；零值的 Begin / Complete 返回
// ErrNoNativeStep。它只对这一次运行有效：不要保存，不要复制进脱离的 goroutine。
type NativeStep struct {
	command Command
	run     *nativeRun
}

// ErrNoNativeStep 表示 NativeStep 不是框架为这一次运行构造的（零值，或步骤体不在 RunNativeStep 里运行）。
var ErrNoNativeStep = errors.New("saga: no native step run; run the body under the step consumer or Steps.RunHandedOver")

// Begin 是生成的 Nest 包装在业务函数之前调用的回执检查（选项 N-A），target 是 handler 的第一个锁目标，必须实现
// StepReceiptHost（roost add saga -native 给实体生成回执字段与 SagaStepReceipts）。返回 replayed=true 时包装不调用业务函数、
// 以零值成功返回：这个操作已有结论（回放），或命令已过截止（过期未执行）；结论在事务提交后交给这次运行。
//
// 参数是 any 而不是 StepReceiptHost：handler 的锁目标常是组件视图接口（I<Comp>Entity），接口里没有这个方法，具体实体类型才有；
// 所以在运行时检查，缺了返回 ErrNoStepReceipts（点名类型），事务回滚，消费者 nak 并告警。
func (step NativeStep) Begin(target any) (bool, error) {
	if step.run == nil {
		return false, ErrNoNativeStep
	}
	tx := nest.CurrentRollbackTx()
	if tx == nil {
		return false, nest.ErrTransactionClosed
	}
	host, ok := target.(StepReceiptHost)
	if !ok {
		return false, fmt.Errorf("%w: %T", ErrNoStepReceipts, target)
	}
	receipts := host.SagaStepReceipts()
	if receipts == nil {
		return false, fmt.Errorf("%w: %T returned nil", ErrNoStepReceipts, target)
	}
	return step.run.begin(tx, receipts)
}

// NativeStepBody 是业务写的原生步骤体：在 Nest handler 里执行步骤并调用 step.Complete。返回 nil 表示 Nest 调用已
// 返回成功（结论在事务持久之后交给消费者）；返回错误见 NativeStep 的契约（结果未知）。
type NativeStepBody func(ctx context.Context, step NativeStep) error

// Command 是这一步的命令：载荷、IdempotencyKey、方向（Phase）与截止时间。
func (step NativeStep) Command() Command { return step.command }

// Complete 在当前 Nest 事务里写这个操作的回执，必须在 Begin 判定执行的同一事务里调用（生成的包装保证顺序）。
//
// 业务拒绝同样调用它（success=false，不改实体，只提交回执）：协调器听不到的拒绝只会等到步骤超时。拒绝不可重试，
// 协调器随即补偿已完成的步骤；reason 写进结果的 Error，空串时填一条通用说明。回执覆盖旧一生的拒绝，同一事务里顺手
// 剪掉至多 16 条过了保留期的旧回执；结论在事务持久之后交给消费者，由它直接发回协调器分区。
func (step NativeStep) Complete(success bool, reason string) error {
	if step.run == nil {
		return ErrNoNativeStep
	}
	return step.run.complete(success, reason)
}

// 原生步骤 handler 的登记（维护者 2026-10-29 决定：原生步骤推荐 durability=pipelined，配合 Nest 的 pipelined 异步完成）。
//
// 生成的注册函数对原生步骤 handler 调用 MustRegisterNativeStepHandler 而不是 nest.MustRegisterHandlerWithMeta：除了登记到
// Nest 的包级注册表，这里另记一份“哪些 handler 是原生步骤、声明了什么 durability”。装配层（wiring/dataengine）在 Init 时
// 读 PipelinedNativeStepHandlers，把它们并进 nest.pipelined 的白名单，并把它们（只有它们）交给 Nest 的异步完成名单——项目不用
// 为原生步骤手写配置；Nest 异步完成按 handler 生效，其他 handler（包括自己声明 pipelined 的）的提交方式不受影响。
var (
	nativeStepMu       sync.Mutex
	nativeStepHandlers = make(map[nest.HandlerName]nest.DurabilityPolicy)
)

// RegisterNativeStepHandler 把原生步骤 handler 注册进 Nest 的包级注册表并记为原生步骤。durability 必须是 strict 或 pipelined：
// 步骤结论在 Nest 返回后直接发回协调器，提交那时必须已经落盘（async / memory 不满足）。生成器在解析时已核对过同一条规则，
// 这里是运行期的同一份强制，手写注册也逃不过。Nest 拒绝注册（重名、声明非法）时不登记。
func RegisterNativeStepHandler(name nest.HandlerName, handler nest.BaseHandler, meta nest.HandlerMeta) error {
	if meta.Durability != nest.DurabilityStrict && meta.Durability != nest.DurabilityPipelined {
		return fmt.Errorf("saga: native step handler %q needs durability strict or pipelined (got %s): its result leaves the process right after Nest returns", name.String(), meta.Durability)
	}
	nativeStepMu.Lock()
	defer nativeStepMu.Unlock()
	if err := nest.RegisterHandlerWithMeta(name, handler, meta); err != nil {
		return err
	}
	nativeStepHandlers[name] = meta.Durability
	return nil
}

// MustRegisterNativeStepHandler 是 RegisterNativeStepHandler 的 panic 版本（生成的注册函数用它）。
func MustRegisterNativeStepHandler(name nest.HandlerName, handler nest.BaseHandler, meta nest.HandlerMeta) {
	if err := RegisterNativeStepHandler(name, handler, meta); err != nil {
		panic(err)
	}
}

// PipelinedNativeStepHandlers 返回已登记、声明 durability=pipelined 的原生步骤 handler 名（排序）。装配层据此给 Nest 的
// pipelined 白名单补名字，并以它们作为 Nest 异步完成的 handler 名单；声明 strict 的原生步骤不在其中（strict 是合法的显式选择）。
func PipelinedNativeStepHandlers() []string {
	nativeStepMu.Lock()
	defer nativeStepMu.Unlock()
	var names []string
	for name, durability := range nativeStepHandlers {
		if durability == nest.DurabilityPipelined {
			names = append(names, name.String())
		}
	}
	slices.Sort(names)
	return names
}

// ResetNativeStepHandlersForTest 清空原生步骤登记，只供测试使用（与 nest.ResetHandlersForTest 配对）。
func ResetNativeStepHandlersForTest() {
	nativeStepMu.Lock()
	defer nativeStepMu.Unlock()
	nativeStepHandlers = make(map[nest.HandlerName]nest.DurabilityPolicy)
}
