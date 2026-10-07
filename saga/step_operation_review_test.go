package saga

import (
	"context"
	"testing"
	"time"
)

// U-0280 复核的“较早尝试晚到的可重试失败让协调器在最后一次尝试执行中放弃”（O-S5-7）已由 saga 方向 ③ 去掉（2026-10-07）：
// 协调器不再接收那份失败，原用例 TestNativeStepSuccessAfterAFailureClosedOperationIsAlarmed 改写为
// saga_direction_3_4_promises_test.go 的 TestNativeStepStaleAttemptRetryableFailureDoesNotAbandonTheLastAttempt。

// 同一承诺在 MongoStore 上：以失败 receipt 关闭的 operation 记为放弃关闭，只有成功 receipt 记为带结果关闭。
func TestMongoStoreTombstoneOfAFailureCloseIsAbandoned(t *testing.T) {
	ctx := context.Background()
	for name, receipt := range map[string]Completion{
		"retryable failure exhausts the step": {Success: false, Retryable: true, Error: "busy"},
		"refusal":                             {Success: false, Error: "refused"},
	} {
		t.Run(name, func(t *testing.T) {
			_, store, record := waitingOnMongo(t)
			after := record.Clone()
			after.Version++
			after.Status, after.NextRunAt, after.OperationKey, after.CommandID = StatusCompensating, record.NextRunAt, "", ""
			after.Phase, after.Step = PhaseCompensate, 0
			receipt.CommandID, receipt.IdempotencyKey, receipt.SagaID, receipt.CompletedAt = record.OperationKey+":0", record.OperationKey, record.ID, time.Now().UTC()
			if _, err := store.Apply(ctx, ApplyRequest{ExpectedVersion: record.Version, After: after, Receipt: &receipt, CloseOperation: record.OperationKey}); err != nil {
				t.Fatal(err)
			}
			live := Completion{CommandID: record.CommandID, IdempotencyKey: record.OperationKey, SagaID: record.ID, Success: true}
			history, err := store.CompletionHistory(ctx, live)
			if err != nil || !history.Recorded || history.Receipt || history.Closure != OperationAbandoned {
				t.Fatalf("history of a success after the operation closed on a %s = %+v err=%v, want an abandoned tombstone (the coordinator accounted for no effect)", name, history, err)
			}
		})
	}
}

// U-0280 复核：较早尝试 k 已生效，它的 completion 没有送达协调器（原来是“退避期间到达、被以 ErrNotWaiting 丢弃”，
// RR-20261006-42 起退避中送达的成功被接收，这里改为 effect 积压 / 丢失）；
// 最后一次尝试 k+1 的投递晚于它自己的截止（消费者积压、进程重启），按 U-0281 不执行直接 ack。修前 ack 时不看
// 同一操作的其他尝试：k 的成功再也没人送达，协调器超时用尽、放弃，扣款不在 CompletedSteps 里、不会被补偿，
// 也没有告警。过期投递同样要把同一操作已生效的成功经 saga 结果流重发：协调器还在等就接收，已放弃就告警。
func TestNativeStepExpiredDeliveryStillReplaysTheOperationsSuccess(t *testing.T) {
	for name, expire := range map[string]func(*nativeWorld, Command) Command{
		// 消费者在 Reserve 之前就看到截止已过（U-0281 的过期分支）。
		"expired before the consumer looks": func(_ *nativeWorld, command Command) Command {
			command.DeadlineAt = time.Now().UTC().Add(-time.Second)
			return command
		},
		// 消费者检查时还没过截止，Reserve 时已过（ErrCommandExpired）。
		"expired by the time it is reserved": func(w *nativeWorld, command Command) Command {
			w.clock = command.DeadlineAt.Add(time.Second)
			return command
		},
	} {
		t.Run(name, func(t *testing.T) {
			w := newNativeWorld(t, 2, time.Now().UTC())
			w.tick(0)
			k := w.pendingCommand()
			recordK := w.commitAttempt(w.inboxA, k)
			if !w.project(recordK) {
				t.Fatal("attempt k projected within its deadline was skipped")
			}
			w.tick(5 * time.Second)  // k 超时 → 退避；k 的 completion effect 没有送达
			w.tick(10 * time.Second) // 最后一次尝试 k+1
			k1 := w.pendingCommand()
			w.deliverCommand(w.inboxB, expire(w, k1)) // 过期投递：不执行，ack
			w.tick(16 * time.Second)                  // k+1 超时 → 重试用尽 → Failed
			before := counterValue("saga.completion.late_after_abandon_total")
			w.deliverReplays()
			w.assertEffective(k.IdempotencyKey, 1)
			if got := w.executions(k.IdempotencyKey); got != 1 {
				t.Fatalf("handler ran %d times, want 1 (the expired delivery must not run)", got)
			}
			record := w.record()
			if record.CompletedSteps == 0 && counterValue("saga.completion.late_after_abandon_total")-before != 1 {
				t.Errorf("attempt %s took effect, its completion was dropped during backoff, and the expired delivery of %s was acknowledged without replaying it: the saga ended %s with CompletedSteps=0 and no late_after_abandon alarm — an uncompensated debit nobody is told about",
					k.ID, k1.ID, record.Status)
			}
		})
	}
}
