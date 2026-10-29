package saga

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	corenest "github.com/tjbdwanghaibo/roost-core/framework/nest"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
)

// 原生步骤的“最多生效一次”以操作实例（saga + 步骤 + 方向）为单位，不以尝试（CommandID）为单位。
//
// 协调器是 simEngine（单元分区，手动推进时钟）；参与方是一个带回执存储的实体（step_receipt_test.go 的 receiptEntity），每次
// 尝试是一笔真实的 Nest 事务（生成包装的形状：Begin → 业务 → Complete），截止判定用同一个时钟。业务执行次数记在 execByOp，
// 生效次数（成功提交）记在 effectiveByOp。结果在事务提交之后交给“消费者”，由用例决定什么时候送达协调器（模拟发布延迟、丢失）。
//
//   - (a) 尝试 k 已提交、协调器已发出 k+1；k 的结果在 k+1 等待期间到达并被接收，随后 k+1 投递：k+1 不再执行，只回放 k 的回执。
//   - (b) k 的结果在 k+1 投递前没有送达协调器（参与方在发布前崩溃）：k+1 投递时回放 k 的回执，协调器接收并前进，
//     而不是让 k+1 再扣一次；(b') 退避中送达的成功被接收。
//   - (c) 尝试 k 没有提交（进程在 Nest 事务前崩溃）、协调器放弃之后它才被重投：命令截止已过，Begin 判定过期，不执行、不留回执。
//     （回执放进实体之后，“已提交、截止后才送达”的那一种由 (c') 的迟到成功补偿处理：设计 §5.1，决定 2A。）
//   - (d) k 已提交、结果还没送达时协调器发出 k+1：k+1 回放 k 的回执，这一步只生效一次。
//
// 历史：U-0280（演练 drill6 的交错）；第 4 批把参与方从原生收件箱（租约 + DataEngine 回执 + lease fence）换成实体里的回执。
func TestNativeStepTakesEffectAtMostOncePerOperation(t *testing.T) {
	t.Run("a: late result accepted while k+1 waits, k+1 replays it", func(t *testing.T) {
		w := newNativeWorld(t, 15, time.Now().UTC())
		w.tick(0)
		k := w.pendingCommand()
		resultK := w.commitAttempt(k) // 尝试 k 提交后，进程在发布结果之前被 kill -9
		w.tick(5 * time.Second)       // k 超时 → 退避
		w.tick(10 * time.Second)      // 发出 k+1
		k1 := w.pendingCommand()
		w.deliver(resultK) // k 的结果在协调器等 k+1 时到达（重启后的重投回放）→ 接收，saga 前进
		if got := w.record().Step; got != 1 {
			t.Fatalf("late result of attempt %d was not accepted while attempt %d waited: step=%d", k.Attempt, k1.Attempt, got)
		}
		w.deliverCommand(k1)
		w.assertEffective(k.IdempotencyKey, 1)
	})

	t.Run("b: late result not delivered before k+1, k+1 replays it", func(t *testing.T) {
		w := newNativeWorld(t, 15, time.Now().UTC())
		w.tick(0)
		k := w.pendingCommand()
		w.commitAttempt(k)      // 提交了，结果没有发布（不投递）
		w.tick(5 * time.Second) // k 超时 → 退避（pending）
		w.tick(10 * time.Second)
		k1 := w.pendingCommand()
		w.deliverCommand(k1)
		if record := w.record(); record.Step != 1 || record.CompletedSteps != 1 {
			t.Fatalf("attempt %d did not carry the dropped result of attempt %d to the coordinator: %+v", k1.Attempt, k.Attempt, record)
		}
		w.assertEffective(k.IdempotencyKey, 1)
	})

	// RR-20261006-42：k 的结果在退避期间送达 → 接收（操作还开着），saga 前进；k+1 不会再派发。
	t.Run("b': late result delivered during backoff is accepted", func(t *testing.T) {
		w := newNativeWorld(t, 15, time.Now().UTC())
		w.tick(0)
		k := w.pendingCommand()
		resultK := w.commitAttempt(k)
		w.tick(5 * time.Second) // k 超时 → 退避（pending）
		if err := w.complete(resultK); err != nil {
			t.Fatalf("completion of attempt %d during backoff = %v, want accepted", k.Attempt, err)
		}
		if record := w.record(); record.Step != 1 || record.CompletedSteps != 1 {
			t.Fatalf("success of attempt %d during backoff was not taken as the step's result: %+v", k.Attempt, record)
		}
		w.deliver(resultK) // 重投：按回执去重
		if record := w.record(); record.Step != 1 || record.CompletedSteps != 1 {
			t.Fatalf("redelivery moved the record: %+v", record)
		}
		w.assertEffective(k.IdempotencyKey, 1)
	})

	t.Run("c: a redelivery after the deadline that finds no receipt does not run", func(t *testing.T) {
		w := newNativeWorld(t, 1, time.Now().UTC())
		w.tick(0)
		k := w.pendingCommand() // 进程在 Nest 事务之前崩溃：没有提交
		w.tick(5 * time.Second) // 唯一一次尝试超时 → 没有已完成步骤 → Failed
		if record := w.record(); record.Status != StatusFailed {
			t.Fatalf("coordinator did not give up: %+v", record)
		}
		if outcome := w.runAttempt(k); outcome.Kind != StepExpired {
			t.Fatalf("redelivery of %s after its deadline = %+v, want expired (not run)", k.ID, outcome)
		}
		w.assertEffective(k.IdempotencyKey, 0)
		if got := w.executions(k.IdempotencyKey); got != 0 {
			t.Fatalf("the expired redelivery ran the business %d times", got)
		}
	})

	// saga 方向 ④（2026-10-07）：之前是“只告警、不重开”，现在重开去补偿这一步；告警计数不变。
	t.Run("c': success committed before the deadline but delivered after abandonment is alarmed and compensated", func(t *testing.T) {
		w := newNativeWorld(t, 1, time.Now().UTC())
		w.tick(0)
		k := w.pendingCommand()
		resultK := w.commitAttempt(k)
		w.tick(5 * time.Second) // 结果还在路上，协调器按超时放弃 → Failed
		before := counterValue("saga.completion.late_after_abandon_total")
		duplicatesBefore := w.sim.e.Stats().Duplicates
		w.deliver(resultK)
		record := w.record()
		if !compensatingStep(record, 0) || record.CompletedSteps != 0 {
			t.Fatalf("a late success did not bring the abandoned saga back to compensate only that step: %+v", record)
		}
		if grown := counterValue("saga.completion.late_after_abandon_total") - before; grown != 1 {
			t.Errorf("success of %s arrived after the coordinator abandoned the step; saga.completion.late_after_abandon_total grew by %d, want 1 (duplicates grew by %d): an effective but uncompensated step must be visible",
				k.ID, grown, w.sim.e.Stats().Duplicates-duplicatesBefore)
		}
		w.assertEffective(k.IdempotencyKey, 1)
	})

	t.Run("d: k committed, its result undelivered, k+1 replays it", func(t *testing.T) {
		w := newNativeWorld(t, 15, time.Now().UTC())
		w.tick(0)
		k := w.pendingCommand()
		w.commitAttempt(k)
		w.tick(5 * time.Second)
		w.tick(10 * time.Second)
		k1 := w.pendingCommand()
		w.deliverCommand(k1) // 同一实体：k+1 读到 k 的回执，回放而不执行
		if record := w.record(); record.Step != 1 {
			t.Fatalf("saga did not advance on attempt %s: %+v", k1.ID, record)
		}
		w.assertEffective(k.IdempotencyKey, 1)
		if got := w.executions(k.IdempotencyKey); got != 1 {
			t.Fatalf("handler ran %d times, want 1", got)
		}
	})
}

// 协调器决定（截止、补偿、Resume）与晚到回执、同一操作的多个结果或旧尝试、终态保留期之后送达的交错。
func TestNativeStepOperationInterleavingsWithCoordinatorDecisions(t *testing.T) {
	// saga 方向 ④（2026-10-07）：迟到成功由协调器自己把 saga 带回补偿、只补偿这一步，不需要 Resume；这一步仍然只执行过一次。
	t.Run("a late effective step on a failed saga is compensated, not run again", func(t *testing.T) {
		w := newNativeWorld(t, 1, time.Now().UTC())
		w.tick(0)
		k := w.pendingCommand()
		resultK := w.commitAttempt(k)
		w.tick(5 * time.Second)
		w.deliver(resultK) // 放弃之后才到：补偿这一步
		w.tick(6 * time.Second)
		compensation := w.pendingCommand()
		if compensation.Phase != PhaseCompensate || compensation.Step != 0 {
			t.Fatalf("the late effective step was not compensated: %+v", compensation)
		}
		w.completeCompensation(compensation, "")
		if record := w.record(); record.Status != StatusCompensated {
			t.Fatalf("want Compensated after compensating the late step: %+v", record)
		}
		w.assertEffective(k.IdempotencyKey, 1)
		if got := w.executions(k.IdempotencyKey); got != 1 {
			t.Fatalf("debit handler ran %d times, want 1", got)
		}
	})

	t.Run("saga deadline abandons a step whose success arrives later: alarm and compensate it", func(t *testing.T) {
		w := newNativeWorldWithDeadline(t, 15, time.Now().UTC(), 7*time.Second)
		w.tick(0)
		k := w.pendingCommand()
		resultK := w.commitAttempt(k)
		w.tick(5 * time.Second) // 超时 → 退避
		w.tick(8 * time.Second) // saga 截止 → 没有已完成步骤 → Failed，操作被放弃
		before := counterValue("saga.completion.late_after_abandon_total")
		w.deliver(resultK)
		if record := w.record(); !compensatingStep(record, 0) || record.LateStep != 1 {
			t.Fatalf("late success did not bring a saga closed by its deadline back to compensate the step: %+v", record)
		}
		if grown := counterValue("saga.completion.late_after_abandon_total") - before; grown != 1 {
			t.Fatalf("late success after the saga deadline grew the alarm by %d, want 1", grown)
		}
	})

	t.Run("manual Compensate during a step's backoff abandons it: a late success is alarmed", func(t *testing.T) {
		w := newNativeWorld(t, 15, time.Now().UTC())
		w.tick(0)
		w.deliverCommand(w.pendingCommand()) // debit 正常完成
		w.tick(time.Second)
		deliver := w.pendingCommand() // deliver 的第一次尝试超时，进入退避
		w.tick(7 * time.Second)
		if record := w.record(); record.Status != StatusPending || record.Step != 1 || record.Attempt != 1 {
			t.Fatalf("deliver is not in backoff: %+v", record)
		}
		if _, err := w.compensate(w.ctx, w.sagaID, "operator", w.at(7*time.Second)); err != nil {
			t.Fatal(err)
		}
		before := counterValue("saga.completion.late_after_abandon_total")
		if err := w.complete(Completion{CommandID: deliver.ID, IdempotencyKey: deliver.IdempotencyKey, SagaID: deliver.SagaID, Success: true}); err != nil {
			t.Fatalf("late success after manual compensation = %v, want acknowledged", err)
		}
		if grown := counterValue("saga.completion.late_after_abandon_total") - before; grown != 1 {
			t.Fatalf("late success of a step abandoned by Compensate grew the alarm by %d, want 1", grown)
		}
		if record := w.record(); record.Phase != PhaseCompensate || record.Status.Terminal() || record.CompletedSteps != 1 {
			t.Fatalf("late success changed the compensating saga: %+v", record)
		}
	})

	// 原生步骤没有可重试结论（Nest 出错 = 结果未知、nak）；可重试失败来自自定义参与方（Engine.Complete）。它只是一次尝试的结论，
	// 不挡同一操作的下一次尝试执行。
	t.Run("a retryable failure does not stop a later attempt from running", func(t *testing.T) {
		w := newNativeWorld(t, 15, time.Now().UTC())
		w.tick(0)
		k := w.pendingCommand()
		if err := w.complete(Completion{CommandID: k.ID, IdempotencyKey: k.IdempotencyKey, SagaID: k.SagaID, Success: false, Retryable: true, Error: "busy"}); err != nil {
			t.Fatal(err)
		}
		w.tick(time.Second)
		k1 := w.pendingCommand()
		if k1.Attempt != 2 {
			t.Fatalf("retryable failure did not schedule attempt 2: %+v", k1)
		}
		w.deliverCommand(k1)
		if record := w.record(); record.Step != 1 {
			t.Fatalf("attempt after a retryable failure did not run: %+v", record)
		}
		if got := w.executions(k.IdempotencyKey); got != 1 {
			t.Fatalf("handler ran %d times, want 1 (the retry; the retryable failure came from a custom participant)", got)
		}
		// 旧尝试的可重试失败晚到：操作已带结果关闭，按重复确认，不告警。
		before := counterValue("saga.completion.late_after_abandon_total")
		if err := w.complete(Completion{CommandID: k.ID, IdempotencyKey: k.IdempotencyKey, SagaID: k.SagaID, Success: false, Retryable: true, Error: "busy"}); err != nil {
			t.Fatalf("late result of an older attempt = %v, want acknowledged", err)
		}
		if counterValue("saga.completion.late_after_abandon_total") != before {
			t.Fatal("a late result of an operation closed with a result was alarmed as abandoned")
		}
	})

	t.Run("a refusal dropped during backoff is replayed in the same life, not in a resumed one", func(t *testing.T) {
		w := newNativeWorld(t, 15, time.Now().UTC())
		w.outcome = func(command Command) (bool, string) {
			if strings.Contains(command.ID, ":r1:") {
				return true, ""
			}
			return false, "sender no longer has the items"
		}
		w.tick(0)
		k := w.pendingCommand()
		resultK := w.commitAttempt(k)
		w.tick(5 * time.Second)
		if err := w.complete(resultK); !errors.Is(err, ErrNotWaiting) {
			t.Fatalf("refusal during backoff = %v, want ErrNotWaiting", err)
		}
		w.tick(10 * time.Second)
		w.deliverCommand(w.pendingCommand())
		record := w.record()
		if record.Status != StatusFailed {
			t.Fatalf("the replayed refusal did not fail the saga: %+v", record)
		}
		if got := w.executions(k.IdempotencyKey); got != 1 {
			t.Fatalf("a refused step ran %d times in one life, want 1", got)
		}
		// 运维修复原因后 Resume：新的一生必须真的重新执行，旧的一生的拒绝不再回放。
		if _, err := w.resume(w.ctx, ResumeRequest{ID: w.sagaID, Now: w.at(11 * time.Second)}); err != nil {
			t.Fatal(err)
		}
		w.deliverCommand(w.pendingCommand())
		if record := w.record(); record.Step != 1 {
			t.Fatalf("Resume replayed the refusal of the previous life instead of running the step: %+v", record)
		}
		if got := w.executions(k.IdempotencyKey); got != 2 {
			t.Fatalf("handler ran %d times, want 2 (one per life)", got)
		}
	})

	// 终态过了保留期被剪掉之后才到的成功：协调器找不到记录，不接收、不改任何状态，只计 saga.completion.unknown_saga_total
	// （设计 §2.3；Redis 协调器时这一条是“回执与 tombstone 过 TTL 之后按 ErrNotWaiting 丢弃”）。
	t.Run("after terminal retention a late success is not recorded and not applied", func(t *testing.T) {
		w := newNativeWorld(t, 1, time.Now().UTC())
		w.tick(0)
		k := w.pendingCommand()
		resultK := w.commitAttempt(k)
		w.tick(5 * time.Second)
		if record := w.record(); record.Status != StatusFailed {
			t.Fatalf("precondition: want Failed, got %+v", record)
		}
		w.tick(5*time.Second + w.sim.e.opts.TerminalRetention + time.Second)
		w.sim.p.prune(w.sim.clock.Now()) // 剪枝在分区循环里每轮提交之后做
		if _, ok := w.sim.p.sagas[w.sagaID]; ok {
			t.Fatal("precondition: the failed saga was not pruned after terminal retention")
		}
		lateBefore, unknownBefore := counterValue("saga.completion.late_after_abandon_total"), counterValue("saga.completion.unknown_saga_total")
		if err := w.complete(resultK); !errors.Is(err, ErrNotFound) {
			t.Fatalf("late success after terminal retention = %v, want ErrNotFound (the saga is gone)", err)
		}
		if counterValue("saga.completion.late_after_abandon_total") != lateBefore || counterValue("saga.completion.unknown_saga_total") != unknownBefore+1 {
			t.Fatal("a success for a pruned saga must count unknown_saga, not late_after_abandon")
		}
		if _, ok := w.sim.p.sagas[w.sagaID]; ok {
			t.Fatal("a late success recreated a pruned saga")
		}
	})
}

// nativeWorld 是一个协调器（simEngine）+ 一个参与方实体（回执在实体里）的确定性模型。
type nativeWorld struct {
	t       *testing.T
	ctx     context.Context
	sim     *simEngine
	t0      time.Time
	sagaID  string
	outcome func(Command) (success bool, reason string)

	entity        *receiptEntity
	committer     *receiptCommitter
	execByOp      map[string]int
	effectiveByOp map[string]int
	queued        []Command
	// inflight 是已提交、还没送达协调器的结果（deliverEffects 一次送出）。
	inflight []Completion
}

func newNativeWorld(t *testing.T, attempts uint32, t0 time.Time) *nativeWorld {
	return newNativeWorldWithDeadline(t, attempts, t0, 0)
}

func newNativeWorldWithDeadline(t *testing.T, attempts uint32, t0 time.Time, sagaDeadline time.Duration) *nativeWorld {
	t.Helper()
	return newNativeWorldWithSteps(t, attempts, t0, sagaDeadline)
}

// newNativeWorldWithSteps 与 newNativeWorldWithDeadline 相同，extra 追加在 debit / deliver 之后（需要第 1 步完成后
// saga 仍未结束的用例用它）。
func newNativeWorldWithSteps(t *testing.T, attempts uint32, t0 time.Time, sagaDeadline time.Duration, extra ...Step) *nativeWorld {
	t.Helper()
	w := &nativeWorld{t: t, ctx: context.Background(), sim: newSimEngine(t, nil), t0: t0, entity: newReceiptEntity(), committer: &receiptCommitter{},
		execByOp: map[string]int{}, effectiveByOp: map[string]int{}}
	w.outcome = func(Command) (bool, string) { return true, "" }
	if err := w.sim.e.Register(Definition{Type: "gift", Version: 1, Steps: append([]Step{
		{Name: "debit", ForwardTopic: "gift.debit", Timeout: 5 * time.Second, MaxAttempts: attempts, BackoffMin: 100 * time.Millisecond, BackoffMax: time.Second},
		{Name: "deliver", ForwardTopic: "gift.deliver", Timeout: 5 * time.Second, MaxAttempts: 5, BackoffMin: 100 * time.Millisecond, BackoffMax: time.Second},
	}, extra...)}); err != nil {
		t.Fatal(err)
	}
	bk := fmt.Sprintf("gift-%d", nativeWorldSeq.Add(1))
	w.sagaID = IDOf("gift", bk)
	start := StartRequest{Type: "gift", DefinitionVersion: 1, BusinessKey: bk, Data: []byte(`{"gift":"x"}`), Now: t0}
	if sagaDeadline > 0 {
		start.DeadlineAt = t0.Add(sagaDeadline)
	}
	if _, err := w.sim.StartSaga(w.ctx, start); err != nil {
		t.Fatal(err)
	}
	w.drain()
	return w
}

var nativeWorldSeq atomic.Int64

func (w *nativeWorld) at(offset time.Duration) time.Time { return w.t0.Add(offset) }

// drain 把协调器派发的命令收进队列。
func (w *nativeWorld) drain() {
	for {
		select {
		case command := <-w.sim.commands:
			w.queued = append(w.queued, command)
		default:
			return
		}
	}
}

// tick 把协调器与参与方的时钟推到 t0+offset，并处理到期。
func (w *nativeWorld) tick(offset time.Duration) {
	w.t.Helper()
	w.sim.clock.Set(w.at(offset))
	w.sim.step()
	w.drain()
}

func (w *nativeWorld) record() Record {
	w.t.Helper()
	record, err := w.sim.Get(w.ctx, w.sagaID)
	if err != nil {
		w.t.Fatal(err)
	}
	return record
}

// pendingCommand 取最早派发、还没被取走的命令。
func (w *nativeWorld) pendingCommand() Command {
	w.t.Helper()
	w.drain()
	if len(w.queued) == 0 {
		w.t.Fatal("no command queued")
	}
	command := w.queued[0]
	w.queued = w.queued[1:]
	return command
}

// runAttempt 是参与方处理一次投递：一笔 Nest 事务（生成包装的形状：Begin → 业务 → Complete），截止按参与方时钟（与协调器同一个）。
// 返回事务提交后的结论；业务执行与生效分别计数。
func (w *nativeWorld) runAttempt(command Command) StepOutcome {
	w.t.Helper()
	run := newNativeRun(command, 0)
	run.now = w.sim.clock.Now
	step := NativeStep{run: run, command: command}
	_, err := corenest.RunIsolatedTransaction(w.ctx, w.committer, "gift-step", func() (any, error) {
		if replayed, err := step.Begin(w.entity); replayed || err != nil {
			return nil, err
		}
		w.execByOp[command.IdempotencyKey]++
		success, reason := w.outcome(command)
		return nil, step.Complete(success, reason)
	})
	if err != nil {
		w.t.Fatalf("native step %s: %v", command.ID, err)
	}
	outcome := run.result()
	if outcome.Kind == StepExecuted && outcome.Completion.Success {
		w.effectiveByOp[command.IdempotencyKey]++
	}
	return outcome
}

// commitAttempt 提交一次尝试（执行或回放），返回它要发回协调器的结果（还没送达）。
func (w *nativeWorld) commitAttempt(command Command) Completion {
	w.t.Helper()
	outcome := w.runAttempt(command)
	if outcome.Kind == StepExpired {
		w.t.Fatalf("attempt %s expired before it ran", command.ID)
	}
	return outcome.Completion
}

// deliverCommand 是步骤消费者的完整处理：运行一次，执行或回放的结果立即发回协调器；过期未执行直接 ack。
func (w *nativeWorld) deliverCommand(command Command) {
	w.t.Helper()
	if outcome := w.runAttempt(command); outcome.Kind != StepExpired {
		w.deliver(outcome.Completion)
	}
	w.drain()
}

func (w *nativeWorld) complete(completion Completion) error {
	_, err := w.sim.Complete(w.ctx, completion)
	w.drain()
	return err
}

// deliver 把一份结果交给协调器；协调器不接收的（不在等、重复）只计数，不是错误——分区的输入不能 Term，跳过后水位照常前进。
func (w *nativeWorld) deliver(completion Completion) {
	w.t.Helper()
	if err := w.complete(completion); err != nil && !errors.Is(err, ErrNotWaiting) && !errors.Is(err, ErrIdentityConflict) && !errors.Is(err, ErrNotFound) {
		w.t.Fatalf("complete: %v", err)
	}
}

func (w *nativeWorld) executions(operation string) int { return w.execByOp[operation] }

// deliverEffects 送出全部在路上的结果。
func (w *nativeWorld) deliverEffects() {
	w.t.Helper()
	inflight := w.inflight
	w.inflight = nil
	for _, completion := range inflight {
		w.deliver(completion)
	}
}

// resume / compensate 是运维操作（时钟取请求里的时刻），之后把派发的命令收进队列。
func (w *nativeWorld) resume(ctx context.Context, request ResumeRequest) (Record, error) {
	if !request.Now.IsZero() {
		w.sim.clock.Set(request.Now)
	}
	record, err := w.sim.Resume(ctx, request)
	w.drain()
	return record, err
}

func (w *nativeWorld) compensate(ctx context.Context, id, reason string, now time.Time) (Record, error) {
	if !now.IsZero() {
		w.sim.clock.Set(now)
	}
	record, err := w.sim.Compensate(ctx, id, reason, now)
	w.drain()
	return record, err
}

// assertEffective 断言这个操作实例真实生效的次数（成功提交），与实体里的回执一致。
func (w *nativeWorld) assertEffective(operation string, want int) {
	w.t.Helper()
	raw, found := w.entity.receipts[operation]
	succeeded := false
	if found {
		receipt, err := decodeStepReceipt(raw)
		if err != nil {
			w.t.Fatal(err)
		}
		succeeded = receipt.Success
	}
	if got := w.effectiveByOp[operation]; got != want || succeeded != (want > 0) {
		w.t.Errorf("operation %s took effect %d time(s) (success receipt in the entity: %v), want %d (handler ran %d times)",
			operation, got, succeeded, want, w.executions(operation))
	}
}

func counterValue(name string) int64 {
	var total int64
	for _, metric := range metrics.Snapshot() {
		if metric.Name == name {
			total += metric.Value
		}
	}
	return total
}

// 协调器的 tombstone 记录关闭方式：放弃关闭后到达的成功告警，带结果关闭后的旧结果按重复处理；Resume 后的新一生带结果关闭时
// 改记为“带结果”。历史：TestRedisStoreTombstoneTellsAbandonedFromResolved（第 4 批改到分区状态）。
func TestCoordinatorTombstoneTellsAbandonedFromResolved(t *testing.T) {
	ctx := context.Background()
	waiting := func(t *testing.T) (*simEngine, Record) {
		t.Helper()
		engine := newSimEngine(t, nil)
		record, err := engine.StartSaga(ctx, StartRequest{Type: "rally", DefinitionVersion: 1, BusinessKey: "tomb"})
		if err != nil {
			t.Fatal(err)
		}
		first := engine.waitCommand()
		if _, err := engine.Complete(ctx, Completion{CommandID: first.ID, IdempotencyKey: first.IdempotencyKey, SagaID: record.ID, Success: true}); err != nil {
			t.Fatal(err)
		}
		engine.waitCommand()
		return engine, engine.record(record.ID) // 在等 march（第 1 步）的第 1 次尝试
	}
	t.Run("abandoned by timeout, then a late success", func(t *testing.T) {
		engine, record := waiting(t)
		engine.advance(time.Second + time.Millisecond) // 尝试 1 超时 → 退避
		engine.advance(backoffPassed)                  // 尝试 2
		engine.advance(time.Second + time.Millisecond) // 尝试 2 超时 → 用尽 → 补偿 reserve，放弃 march
		if tomb := engine.p.sagas[record.ID].Ops[record.OperationKey]; tomb.Closure != OperationAbandoned {
			t.Fatalf("tombstone after timeout exhaustion = %+v, want abandoned", tomb)
		}
		late := Completion{CommandID: record.CommandID, IdempotencyKey: record.OperationKey, SagaID: record.ID, Success: true}
		before := engine.e.Stats().LateAfterAbandon
		if _, err := engine.Complete(ctx, late); err != nil {
			t.Fatal(err)
		}
		if engine.e.Stats().LateAfterAbandon != before+1 {
			t.Fatal("late success after abandonment was not counted")
		}
	})
	t.Run("closed with a result, then an older attempt's result", func(t *testing.T) {
		engine, record := waiting(t)
		if _, err := engine.Complete(ctx, Completion{CommandID: record.CommandID, IdempotencyKey: record.OperationKey, SagaID: record.ID, Success: true}); err != nil {
			t.Fatal(err)
		}
		if tomb := engine.p.sagas[record.ID].Ops[record.OperationKey]; tomb.Closure != OperationClosedWithResult {
			t.Fatalf("tombstone = %+v, want a result closure", tomb)
		}
		older := Completion{CommandID: record.OperationKey + ":0", IdempotencyKey: record.OperationKey, SagaID: record.ID, Success: true}
		before := engine.e.Stats()
		if _, err := engine.Complete(ctx, older); err != nil {
			t.Fatal(err)
		}
		if after := engine.e.Stats(); after.LateAfterAbandon != before.LateAfterAbandon || after.Duplicates != before.Duplicates+1 {
			t.Fatalf("older result after a resolved operation: stats %+v -> %+v, want a duplicate", before, after)
		}
	})
	t.Run("a late success resolving an abandoned operation upgrades the tombstone", func(t *testing.T) {
		engine, record := waiting(t)
		engine.advance(time.Second + time.Millisecond)
		engine.advance(backoffPassed)
		engine.waitCommand()
		engine.advance(time.Second + time.Millisecond) // 放弃 march → 补偿 reserve
		engine.advance(0)                              // 派发补偿
		engine.waitCommand()
		// 补偿被拒 → ManualRequired，再 Resume：新一生的 march 接收成功时 tombstone 改记“带结果”。
		current := engine.record(record.ID)
		if _, err := engine.Complete(ctx, Completion{CommandID: current.CommandID, IdempotencyKey: current.OperationKey, SagaID: record.ID, Error: "refused"}); err != nil {
			t.Fatal(err)
		}
		if engine.record(record.ID).Status != StatusManualRequired {
			t.Fatalf("precondition: want ManualRequired, got %+v", engine.record(record.ID))
		}
		late := Completion{CommandID: record.CommandID, IdempotencyKey: record.OperationKey, SagaID: record.ID, Success: true}
		if _, err := engine.Complete(ctx, late); err != nil {
			t.Fatal(err)
		}
		if tomb := engine.p.sagas[record.ID].Ops[record.OperationKey]; tomb.Closure != OperationClosedWithResult {
			t.Fatalf("tombstone after a late success was compensated = %+v, want upgraded to a result closure", tomb)
		}
	})
}

// 以失败关闭的操作（可重试失败用尽、拒绝）记为放弃关闭，只有成功记为带结果关闭。
// 历史：TestRedisStoreTombstoneOfAFailureCloseIsAbandoned。
func TestCoordinatorTombstoneOfAFailureCloseIsAbandoned(t *testing.T) {
	ctx := context.Background()
	for name, outcome := range map[string]Completion{
		"retryable failure exhausts the step": {Success: false, Retryable: true, Error: "busy"},
		"refusal":                             {Success: false, Error: "refused"},
	} {
		t.Run(name, func(t *testing.T) {
			engine := newSimEngine(t, nil)
			record, err := engine.StartSaga(ctx, StartRequest{Type: "rally", DefinitionVersion: 1, BusinessKey: "fail-close"})
			if err != nil {
				t.Fatal(err)
			}
			first := engine.waitCommand()
			if _, err := engine.Complete(ctx, Completion{CommandID: first.ID, IdempotencyKey: first.IdempotencyKey, SagaID: record.ID, Success: true}); err != nil {
				t.Fatal(err)
			}
			waiting := engine.record(record.ID)
			engine.waitCommand()
			if outcome.Retryable {
				// MaxAttempts 2：第 2 次尝试的可重试失败用尽。
				if _, err := engine.Complete(ctx, Completion{CommandID: waiting.CommandID, IdempotencyKey: waiting.OperationKey, SagaID: record.ID, Retryable: true, Error: "busy"}); err != nil {
					t.Fatal(err)
				}
				engine.advance(backoffPassed)
				waiting = engine.record(record.ID)
				engine.waitCommand()
			}
			outcome.CommandID, outcome.IdempotencyKey, outcome.SagaID = waiting.CommandID, waiting.OperationKey, record.ID
			if _, err := engine.Complete(ctx, outcome); err != nil {
				t.Fatal(err)
			}
			if tomb := engine.p.sagas[record.ID].Ops[waiting.OperationKey]; tomb.Closure != OperationAbandoned {
				t.Fatalf("tombstone after the operation closed on a %s = %+v, want abandoned (the coordinator accounted for no effect)", name, tomb)
			}
		})
	}
}

// 较早尝试 k 已生效但结果没有送达协调器，最后一次尝试 k+1 的投递晚于它自己的截止：Begin 读到 k 的成功回执仍回放它（不执行），
// 消费者把它发回协调器——协调器还在等就接收，已放弃就告警并补偿；否则扣款既不在 CompletedSteps 里、不被补偿，也没有告警。
func TestNativeStepExpiredDeliveryStillReplaysTheOperationsSuccess(t *testing.T) {
	w := newNativeWorld(t, 2, time.Now().UTC())
	w.tick(0)
	k := w.pendingCommand()
	w.commitAttempt(k)       // k 生效，结果没有送达
	w.tick(5 * time.Second)  // k 超时 → 退避
	w.tick(10 * time.Second) // 最后一次尝试 k+1
	k1 := w.pendingCommand()
	w.sim.clock.Set(k1.DeadlineAt.Add(time.Second)) // k+1 的投递晚于它的截止
	w.deliverCommand(k1)
	w.tick(16 * time.Second)
	w.assertEffective(k.IdempotencyKey, 1)
	if got := w.executions(k.IdempotencyKey); got != 1 {
		t.Fatalf("handler ran %d times, want 1 (the expired delivery must not run)", got)
	}
	if record := w.record(); record.CompletedSteps == 0 && record.LateStep == 0 {
		t.Errorf("attempt %s took effect and the expired delivery of %s found its receipt, yet the saga ended %s with CompletedSteps=0 and no late step: an uncompensated debit nobody is told about",
			k.ID, k1.ID, record.Status)
	}
}
