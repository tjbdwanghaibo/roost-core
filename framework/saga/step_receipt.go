package saga

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/nest"
)

// step_receipt.go 是参与方新路径的原生步骤（设计 §5.1，决定 2A，第 3 批并存、未接线）：一个操作实例
// （Command.IdempotencyKey）的结论作为回执放在被改的实体里，读写都在实体的 Nest 锁内、随业务修改同一笔事务提交 / 回滚。
//
// 一次运行的顺序（RunNativeStep → 步骤体 → 生成的 Nest 包装 → 业务 handler）：
//
//  1. 生成的包装在业务函数之前调用 step.Begin(第一个锁目标)：有成功回执（任何代际）或本代际的拒绝回执 → 回放，不执行业务；
//     否则命令已过截止 → 过期未执行；否则返回 false，业务执行。
//  2. 业务调用 step.Complete(success, reason)：在同一事务里写回执（覆盖旧一生的拒绝），顺手剪掉至多 16 条过了保留期的旧回执。
//  3. 结论经 nest.AfterCommit 交给这次运行：只在事务持久之后可见（strict：锁内 fsync 之后；pipelined：本事务的 WAL 票据落盘
//     之后）。Nest 请求返回之后消费者才读它，发结果到协调器分区、PubAck 后才 ack 命令。
//
// 至多一次靠两件事：同一实体的事务由实体锁串行，回执与业务修改在同一条 WAL 记录里。重投（结果未知、ack 丢失）读到回执就回放。
// 不需要租约、操作状态文档或投影里的 fence。

// StepReceipt 是一个操作实例的结论：成功，或某一代际的业务拒绝。可重试失败与过期不留回执。
type StepReceipt struct {
	// CommandID 是生效的那次尝试；回放时原样作为结果的 CommandID 发给协调器（协调器按它去重、核对代际）。
	CommandID string `json:"command_id"`
	// Incarnation 由 CommandID 解析（commandIDIncarnation）：拒绝只对同一代际回放，补偿方向重开的新一生要重新执行。
	Incarnation uint32 `json:"incarnation"`
	Success     bool   `json:"success"`
	Error       string `json:"error,omitempty"`
	Data        []byte `json:"data,omitempty"`
	// At 是写回执的时刻（unix 毫秒），保留期按它剪。
	At int64 `json:"at"`
}

// stepReceiptVersion 是回执编码的版本；不认识的版本按坏回执处理（不当作“没有回执”，那会让操作再执行一次）。
const stepReceiptVersion uint16 = 1

type stepReceiptWire struct {
	V uint16 `json:"v"`
	StepReceipt
}

func encodeStepReceipt(receipt StepReceipt) []byte {
	raw, _ := json.Marshal(stepReceiptWire{V: stepReceiptVersion, StepReceipt: receipt}) // 字符串、整数、布尔与字节切片，不会失败
	return raw
}

func decodeStepReceipt(raw []byte) (StepReceipt, error) {
	var wire stepReceiptWire
	if err := json.Unmarshal(raw, &wire); err != nil {
		return StepReceipt{}, fmt.Errorf("%w: step receipt: %v", ErrInvalidRecord, err)
	}
	if wire.V != stepReceiptVersion || wire.CommandID == "" || (!wire.Success && wire.Error == "") {
		return StepReceipt{}, fmt.Errorf("%w: step receipt version %d command %q", ErrInvalidRecord, wire.V, wire.CommandID)
	}
	return wire.StepReceipt, nil
}

// StepReceipts 是实体 DAO 上的回执存储（键是操作实例 Command.IdempotencyKey，值是框架编码的 StepReceipt）。只在实体的 Nest
// 锁内、事务里调用；写入经 DAO 生成的 mutator，随事务提交 / 回滚（A1“回滚统一走 DAO”）。
//
// 值是不透明字节：编码与校验归框架（坏回执报错而不是当作没有），生成的适配只把 DAO 的 map[string][]byte 字段原样转过来。
// 返回的切片不得修改。
type StepReceipts interface {
	StepReceipt(operation string) ([]byte, bool)
	PutStepReceipt(operation string, encoded []byte)
	DeleteStepReceipt(operation string)
	RangeStepReceipts(func(operation string, encoded []byte) bool)
}

// StepReceiptHost 是带回执存储的实体（roost add saga -native 给实体生成 SagaStepReceipts）。
type StepReceiptHost interface {
	SagaStepReceipts() StepReceipts
}

// StepOutcomeKind 是一次步骤运行的结论种类。
type StepOutcomeKind uint8

const (
	// StepExecuted：本次执行了业务并提交了回执，Completion 是本次的结论。
	StepExecuted StepOutcomeKind = iota + 1
	// StepReplayed：已有结论（本命令或同一操作的另一次尝试），没有执行业务；Completion 的 CommandID 是生效的那次尝试。
	StepReplayed
	// StepExpired：命令过了截止且没有可回放的结论，没有执行；不发结果，直接 ack（协调器按超时处理）。
	StepExpired
)

// StepOutcome 是一次步骤运行（原生或 Mongo）交给消费者的结论。
type StepOutcome struct {
	Kind       StepOutcomeKind
	Completion Completion
}

// maxReceiptPrunePerCommit 是一次 Complete 顺手剪掉的过期回执上限：剪枝跟着业务事务走，不能让一次步骤的 WAL 记录无界变大。
const maxReceiptPrunePerCommit = 16

// DefaultStepReceiptRetention 是回执保留期缺省值（saga.receipt_retention，选项 RR-A）：必须长于步骤命令可能重投的最长时间。
const DefaultStepReceiptRetention = 168 * time.Hour

// nativeRun 是一次原生步骤运行（一次投递或一次转交）的状态：Begin / Complete 在 Nest 事务里（业务池 goroutine）写它，
// 结论经 AfterCommit 写入，消费者在 Nest 请求返回之后读。Nest 的回复晚于 AfterCommit（strict 的 Guard 释放回调、
// pipelined 的完成泵都在回复之前跑），mu 只是让“请求出错返回、事务仍在别处提交”时的并发读写也安全。
type nativeRun struct {
	command     Command
	retention   time.Duration
	now         func() time.Time
	incarnation uint32

	mu sync.Mutex
	// executing 是 Begin 判定“执行”的那笔事务与它的回执存储；Complete 只在同一事务里有效，写完清空。
	executing *nest.RollbackTx
	receipts  StepReceipts
	outcome   StepOutcome
}

func newNativeRun(command Command, retention time.Duration) *nativeRun {
	if retention <= 0 {
		retention = DefaultStepReceiptRetention
	}
	return &nativeRun{command: command, retention: retention, now: time.Now, incarnation: commandIDIncarnation(command.IdempotencyKey, command.ID)}
}

// begin 是 NativeStep.Begin 的新路径，在事务里、实体锁内调用。
func (run *nativeRun) begin(tx *nest.RollbackTx, receipts StepReceipts) (bool, error) {
	command := run.command
	if raw, found := receipts.StepReceipt(command.IdempotencyKey); found {
		receipt, err := decodeStepReceipt(raw)
		if err != nil {
			return false, fmt.Errorf("saga: native step %s: %w", command.ID, err)
		}
		if receipt.Success || receipt.Incarnation == run.incarnation {
			// 回放。把同一份回执原样再写一次，让本事务成为一条 WAL 记录：pipelined 下回执可能是另一笔还没落盘的事务写的
			// （级联读在进程内允许），空事务没有 WAL 票据、AfterCommit 会立即执行，结论就会先于它的持久化外发；有了这条记录，
			// AfterCommit 在它落盘之后才跑，而 WAL 按序落盘，写回执的那笔一定已经落盘。strict 不需要（锁内 fsync），多一次
			// 写只在重投时发生。
			receipts.PutStepReceipt(command.IdempotencyKey, raw)
			completion := Completion{
				CommandID: receipt.CommandID, IdempotencyKey: command.IdempotencyKey, SagaID: command.SagaID,
				Success: receipt.Success, Error: receipt.Error, Data: receipt.Data,
			}
			tx.AfterCommit(func() { run.settle(StepOutcome{Kind: StepReplayed, Completion: completion}) })
			return true, nil
		}
		// 只有旧一生的拒绝：补偿方向重开后的新一生要重新执行，回执由本次覆盖。
	}
	if !run.now().Before(command.DeadlineAt) {
		// 截止判定在事务开始时（与协调器同一口径：系统时钟，截止由协调器按它写）。之后才落盘的执行只能由迟到成功补偿处理。
		tx.AfterCommit(func() { run.settle(StepOutcome{Kind: StepExpired}) })
		return true, nil
	}
	run.mu.Lock()
	run.executing, run.receipts = tx, receipts
	run.mu.Unlock()
	return false, nil
}

// complete 是 NativeStep.Complete 的新路径：在 Begin 判定执行的同一事务里写回执、剪旧回执，结论在提交后交给这次运行。
func (run *nativeRun) complete(success bool, reason string) error {
	tx := nest.CurrentRollbackTx()
	run.mu.Lock()
	receipts := run.receipts
	admitted := tx != nil && run.executing == tx
	if admitted {
		run.executing, run.receipts = nil, nil
	}
	run.mu.Unlock()
	if !admitted {
		return fmt.Errorf("%w: command %s", ErrNativeStepNotBegun, run.command.ID)
	}
	if nest.CurrentDispatchHasRemoteWriteBatch() {
		return fmt.Errorf("%w: command %s", ErrNativeStepRemoteWrite, run.command.ID)
	}
	command := run.command
	now := run.now()
	receipt := StepReceipt{CommandID: command.ID, Incarnation: run.incarnation, Success: success, At: now.UnixMilli()}
	if !success {
		receipt.Error = reason
		if receipt.Error == "" {
			receipt.Error = "saga: native step " + command.StepName + " refused"
		}
	}
	receipts.PutStepReceipt(command.IdempotencyKey, encodeStepReceipt(receipt))
	pruneStepReceipts(receipts, command.IdempotencyKey, now.Add(-run.retention).UnixMilli())
	completion := Completion{CommandID: command.ID, IdempotencyKey: command.IdempotencyKey, SagaID: command.SagaID, Success: success, Error: receipt.Error}
	tx.AfterCommit(func() { run.settle(StepOutcome{Kind: StepExecuted, Completion: completion}) })
	return nil
}

// pruneStepReceipts 在同一事务里删掉至多 maxReceiptPrunePerCommit 条 At 早于 cutoff 的回执（不删刚写的那条）。解不开的旧回执
// 留着不删：它可能是更新格式写的，删掉会让那个操作再执行一次；坏回执在被读到时报错。
func pruneStepReceipts(receipts StepReceipts, keep string, cutoff int64) {
	var expired []string
	receipts.RangeStepReceipts(func(operation string, encoded []byte) bool {
		if operation == keep {
			return true
		}
		if receipt, err := decodeStepReceipt(encoded); err == nil && receipt.At < cutoff {
			expired = append(expired, operation)
		}
		return len(expired) < maxReceiptPrunePerCommit
	})
	for _, operation := range expired {
		receipts.DeleteStepReceipt(operation)
	}
}

func (run *nativeRun) settle(outcome StepOutcome) {
	run.mu.Lock()
	run.outcome = outcome
	run.mu.Unlock()
}

func (run *nativeRun) result() StepOutcome {
	run.mu.Lock()
	defer run.mu.Unlock()
	return run.outcome
}

// RunNativeStep 运行一次原生步骤（新路径）：构造这次运行，调用步骤体，返回 Nest 事务提交后的结论。
//
//   - 步骤体返回错误 = 结果未知（Nest 提交可能已经发生）：调用方 nak / 稍后重试，重投时 Begin 读到回执就回放。
//   - 步骤体返回 nil 但没有结论（没调 Nest handler，或 handler 没调 Complete 就成功返回）→ ErrNativeStepNoOutcome。
//
// 它不发布、不 ack：SubscribeNativeStep 据结论发结果再 ack；转交路径（命令由另一个进程转来）只执行，原消息持有者的重投收尾。
// 与消费者的互斥由实体锁与回执保证。receiptRetention ≤ 0 取 DefaultStepReceiptRetention。
func RunNativeStep(ctx context.Context, command Command, receiptRetention time.Duration, body NativeStepBody) (StepOutcome, error) {
	if body == nil {
		return StepOutcome{}, ErrInvalidRecord
	}
	if err := command.Validate(); err != nil {
		return StepOutcome{}, err
	}
	run := newNativeRun(command, receiptRetention)
	if err := body(ctx, NativeStep{run: run, command: command}); err != nil {
		return StepOutcome{}, err
	}
	outcome := run.result()
	if outcome.Kind == 0 {
		return StepOutcome{}, fmt.Errorf("%w: command %s", ErrNativeStepNoOutcome, command.ID)
	}
	return outcome, nil
}
