package saga

import (
	"context"
	"strings"
	"testing"
	"time"
)

// 协调器接收 completion 时核对代际（Resume 开启新一生）：成功在任何一生里都不重做，拒绝只在本生回放。
// 跑在 nativeWorld 上（simEngine 协调器 + 回执在实体里的参与方，真实 Nest 事务），手动推进时钟：
//
//   - E1：Resume 之后，上一生晚到的拒绝不被新一生接收（只计数），新一生照常执行并前进；
//   - E2：放弃后迟到的成功按（操作，代际）只告警一次，不随每次重投重复计数；
//   - E3：Resume 之后、新一生派发之前到达的上一生成功，直接接收为这个操作的结果，不告警、不再派发这一步；
//   - E4：补偿方向 ManualRequired 上的人工 Compensate 进入新一生，补偿真正重新执行，而不是复用上一轮的 CommandID。
//
// 历史：维护者决定 B1（2026-10-05）。
func TestCoordinatorChecksTheIncarnationOfACompletion(t *testing.T) {

	t.Run("E1: a refusal from the previous life is not accepted by the resumed life", func(t *testing.T) {
		w := newNativeWorld(t, 1, time.Now().UTC())
		w.outcome = func(command Command) (bool, string) {
			if strings.Contains(command.ID, ":r1:") {
				return true, ""
			}
			return false, "sender no longer has the items"
		}
		w.tick(0)
		k := w.pendingCommand()
		resultK := w.commitAttempt(k)
		w.tick(5 * time.Second) // 唯一一次尝试超时 → Failed
		if _, err := w.resume(w.ctx, ResumeRequest{ID: w.sagaID, Now: w.at(6 * time.Second)}); err != nil {
			t.Fatal(err)
		}
		w.tick(6 * time.Second)
		resumed := w.pendingCommand()
		staleBefore := counterValue("saga.completion.stale_incarnation_total")
		w.deliver(resultK) // 上一生的拒绝在新一生等待时到达
		if record := w.record(); record.Status != StatusWaiting || record.CommandID != resumed.ID {
			t.Errorf("the refusal of %s from the previous life was accepted by the resumed life waiting on %s: the saga is now %s (step %d, completed %d); the inbox would run %s anyway, so its debit ends outside CompletedSteps",
				k.ID, resumed.ID, record.Status, record.Step, record.CompletedSteps, resumed.ID)
		}
		if grown := counterValue("saga.completion.stale_incarnation_total") - staleBefore; grown != 1 {
			t.Errorf("a stale refusal from the previous life grew saga.completion.stale_incarnation_total by %d, want 1", grown)
		}
		w.deliverCommand(resumed)
		w.deliverEffects()
		if record := w.record(); record.Step != 1 || record.CompletedSteps != 1 {
			t.Errorf("the resumed life did not advance on its own success: %+v", record)
		}
		w.assertEffective(k.IdempotencyKey, 1)
	})

	t.Run("E2: a late success after abandonment is alarmed once however often it arrives", func(t *testing.T) {
		w := newNativeWorld(t, 2, time.Now().UTC())
		w.tick(0)
		k := w.pendingCommand()
		resultK := w.commitAttempt(k)
		w.tick(5 * time.Second)  // k 超时 → 退避；k 的 completion effect 还没送达（退避中送达会被接收）
		w.tick(10 * time.Second) // 最后一次尝试 k+1
		expired := w.pendingCommand()
		w.tick(16 * time.Second) // k+1 超时 → 重试用尽 → Failed（放弃关闭）；之后的投递晚于它的截止：不执行，回放 k 的成功
		before := counterValue("saga.completion.late_after_abandon_total")
		w.deliverCommand(expired)
		w.deliverCommand(expired) // JetStream 重投（上一次 ack 丢失）
		w.deliver(resultK)        // k 的结果重投
		if grown := counterValue("saga.completion.late_after_abandon_total") - before; grown != 1 {
			t.Errorf("one effective success of %s arrived three times after the coordinator abandoned the step; saga.completion.late_after_abandon_total grew by %d, want 1: the alarm must count the uncompensated step, not its deliveries",
				k.ID, grown)
		}
		w.assertEffective(k.IdempotencyKey, 1)
		// saga 方向 ④（2026-10-07）：迟到的成功不再只告警，saga 被带回补偿、只补偿这一步（之前断言“Failed 不变”）。
		if record := w.record(); !compensatingStep(record, 0) || record.LateStep != 1 || record.CompletedSteps != 0 {
			t.Fatalf("late success did not bring the failed saga back to compensate that step: %+v", record)
		}
	})

	t.Run("E3: a previous life's success arriving after Resume, before dispatch, is taken as the result", func(t *testing.T) {
		w := newNativeWorld(t, 1, time.Now().UTC())
		w.tick(0)
		k := w.pendingCommand()
		resultK := w.commitAttempt(k)
		w.tick(5 * time.Second) // completion 还在路上，协调器按超时放弃 → Failed
		if _, err := w.resume(w.ctx, ResumeRequest{ID: w.sagaID, Now: w.at(6 * time.Second)}); err != nil {
			t.Fatal(err)
		}
		before := counterValue("saga.completion.late_after_abandon_total")
		w.deliver(resultK) // 新一生还没派发这一步
		grown := counterValue("saga.completion.late_after_abandon_total") - before
		if record := w.record(); grown != 0 || record.Step != 1 || record.CompletedSteps != 1 {
			t.Errorf("success of %s arrived after Resume, before the new life dispatched the step: late_after_abandon grew by %d (want 0) and the saga is %s at step %d with %d completed (want step 1, 1 completed): it is alarmed as uncompensated now and counted later when the inbox replays it",
				k.ID, grown, record.Status, record.Step, record.CompletedSteps)
		}
		w.tick(6 * time.Second)
		if next := w.pendingCommand(); next.Step != 1 {
			w.deliverCommand(next) // 不应发生：新一生派发 debit，收件箱回放 k 的成功
		}
		w.assertEffective(k.IdempotencyKey, 1)
		if got := w.executions(k.IdempotencyKey); got != 1 {
			t.Fatalf("debit handler ran %d times, want 1", got)
		}
	})

	t.Run("E4: manual Compensate on a compensation ManualRequired re-runs the compensation", func(t *testing.T) {
		w := newNativeWorld(t, 1, time.Now().UTC())
		refundFixed := false
		w.outcome = func(command Command) (bool, string) {
			if command.Phase == PhaseCompensate && !refundFixed {
				return false, "refund target is locked"
			}
			return true, ""
		}
		w.tick(0)
		w.deliverCommand(w.pendingCommand()) // debit 成功
		w.deliverEffects()
		w.tick(time.Second)
		deliver := w.pendingCommand() // deliver（Mongo 步骤）拒绝 → 补偿 debit
		if err := w.complete(Completion{CommandID: deliver.ID, IdempotencyKey: deliver.IdempotencyKey, SagaID: deliver.SagaID, Success: false, Error: "recipient missing"}); err != nil {
			t.Fatal(err)
		}
		w.tick(2 * time.Second)
		first := w.pendingCommand()
		if first.Phase != PhaseCompensate {
			t.Fatalf("expected the refund of debit, got %+v", first)
		}
		w.deliverCommand(first) // 退款被拒 → ManualRequired
		w.deliverEffects()
		if record := w.record(); record.Status != StatusManualRequired || record.Phase != PhaseCompensate {
			t.Fatalf("refused refund did not stop at ManualRequired: %+v", record)
		}
		refundFixed = true
		if _, err := w.compensate(w.ctx, w.sagaID, "operator unlocked the refund target", w.at(3*time.Second)); err != nil {
			t.Fatal(err)
		}
		w.tick(3 * time.Second)
		again := w.pendingCommand()
		if again.ID == first.ID {
			t.Errorf("manual Compensate re-dispatched the refund as %s, the CommandID of the refused attempt: the native inbox sees a different digest under the same ID (identity conflict, nak forever) and a Mongo inbox replays the old refusal; the refund never re-runs", again.ID)
		}
		w.deliverCommand(again)
		w.deliverEffects()
		if record := w.record(); record.Status != StatusCompensated {
			t.Errorf("refund after manual Compensate did not finish the compensation: %+v", record)
		}
		if got := w.executions(first.IdempotencyKey); got != 2 {
			t.Errorf("refund handler ran %d times, want 2 (the refused one and the re-run after Compensate)", got)
		}
	})

	// 守卫正确做法：补偿方向 ManualRequired 修复原因后用 Resume，进入新一生、补偿重新执行。
	t.Run("Resume on a compensation ManualRequired re-runs the compensation", func(t *testing.T) {
		w := newNativeWorld(t, 1, time.Now().UTC())
		refundFixed := false
		w.outcome = func(command Command) (bool, string) {
			if command.Phase == PhaseCompensate && !refundFixed {
				return false, "refund target is locked"
			}
			return true, ""
		}
		w.tick(0)
		w.deliverCommand(w.pendingCommand())
		w.deliverEffects()
		w.tick(time.Second)
		deliver := w.pendingCommand()
		if err := w.complete(Completion{CommandID: deliver.ID, IdempotencyKey: deliver.IdempotencyKey, SagaID: deliver.SagaID, Success: false, Error: "recipient missing"}); err != nil {
			t.Fatal(err)
		}
		w.tick(2 * time.Second)
		first := w.pendingCommand()
		w.deliverCommand(first)
		w.deliverEffects()
		refundFixed = true
		if _, err := w.resume(w.ctx, ResumeRequest{ID: w.sagaID, Now: w.at(3 * time.Second)}); err != nil {
			t.Fatal(err)
		}
		w.tick(3 * time.Second)
		again := w.pendingCommand()
		w.deliverCommand(again)
		w.deliverEffects()
		if record := w.record(); record.Status != StatusCompensated || w.executions(first.IdempotencyKey) != 2 {
			t.Fatalf("Resume did not re-run the refused refund: %+v executions=%d", record, w.executions(first.IdempotencyKey))
		}
	})
}

// B1 在协调器上：放弃后迟到的成功按（操作，代际）只告警一次（tombstone 的 LateAlarms 记已告警的代际）；同一代际之后的送达
// 计重复；另一代际照常告警。旧一生的拒绝不接收、计 StaleIncarnation；旧一生的成功在记录正等这个操作时接收为结果。
// 历史：TestRedisStoreMarksALateSuccessAlarmOncePerLife（标记在 RedisStore 的 tombstone 上，这里在分区状态的 tombstone 上）。
func TestCoordinatorMarksALateSuccessAlarmOncePerLife(t *testing.T) {
	ctx := context.Background()
	waiting := func(t *testing.T) (*simEngine, Record) {
		t.Helper()
		engine := newSimEngine(t, nil)
		now := engine.clock.Now()
		id := IDOf("rally", "gift-1")
		record := Record{
			ID: id, Type: "rally", DefinitionVersion: 1, BusinessKey: "gift-1",
			Status: StatusWaiting, Phase: PhaseForward, Step: 1, CompletedSteps: 1, Attempt: 2, Version: 3,
			OperationKey: operationKey(id, PhaseForward, 1), CommandID: commandID(operationKey(id, PhaseForward, 1), 0, 2),
			NextRunAt: now.Add(time.Second), CreatedAt: now, UpdatedAt: now,
		}
		engine.seed(record)
		return engine, record
	}
	t.Run("engine alarms the first delivery and counts the rest as duplicates", func(t *testing.T) {
		engine, record := waiting(t)
		engine.advance(2 * time.Second) // march 的 MaxAttempts 2：第 2 次尝试超时即用尽，放弃关闭
		late := Completion{CommandID: record.CommandID, IdempotencyKey: record.OperationKey, SagaID: record.ID, Success: true}
		before := engine.e.Stats()
		for i := 0; i < 3; i++ {
			if _, err := engine.Complete(ctx, late); err != nil {
				t.Fatal(err)
			}
		}
		if after := engine.e.Stats(); after.LateAfterAbandon != before.LateAfterAbandon+1 || after.Duplicates != before.Duplicates+2 {
			t.Fatalf("three deliveries of one late success: stats %+v -> %+v, want one alarm and two duplicates", before, after)
		}
	})
	t.Run("a compensate-direction late success is alarmed once per life", func(t *testing.T) {
		engine := newSimEngine(t, nil)
		record, err := engine.StartSaga(ctx, StartRequest{Type: "rally", DefinitionVersion: 1, BusinessKey: "alarm-lives"})
		if err != nil {
			t.Fatal(err)
		}
		first := engine.waitCommand()
		engine.Complete(ctx, Completion{CommandID: first.ID, IdempotencyKey: first.IdempotencyKey, SagaID: record.ID, Success: true})
		second := engine.waitCommand()
		engine.Complete(ctx, Completion{CommandID: second.ID, IdempotencyKey: second.IdempotencyKey, SagaID: record.ID, Error: "refused"})
		compensation := engine.waitCommand() // 补偿第 0 步，第 0 代
		engine.advance(time.Second + time.Millisecond)
		engine.advance(backoffPassed)
		engine.waitCommand()
		engine.advance(time.Second + time.Millisecond) // 补偿用尽 → ManualRequired（放弃关闭）
		late := Completion{CommandID: compensation.ID, IdempotencyKey: compensation.IdempotencyKey, SagaID: record.ID, Success: true}
		before := engine.e.Stats().LateAfterAbandon
		for i := 0; i < 2; i++ {
			if _, err := engine.Complete(ctx, late); err != nil {
				t.Fatal(err)
			}
		}
		if got := engine.e.Stats().LateAfterAbandon - before; got != 1 {
			t.Fatalf("incarnation 0: %d alarms for two deliveries, want 1", got)
		}
		if alarms := engine.p.sagas[record.ID].Ops[compensation.IdempotencyKey].LateAlarms; len(alarms) != 1 || alarms[0] != 0 {
			t.Fatalf("tombstone alarms = %v, want [0]", alarms)
		}
	})
	t.Run("a stale result is ignored and counted", func(t *testing.T) {
		engine, record := waiting(t)
		if _, err := engine.Resume(ctx, ResumeRequest{ID: record.ID}); err == nil {
			t.Fatal("precondition: a waiting record cannot resume")
		}
		// 模拟 Resume 之后新一生在等同一操作：记录换代，等新一生的第 1 次尝试。
		resumed := record.Clone()
		resumed.Version++
		resumed.Incarnation = 1
		resumed.Attempt = 1
		resumed.CommandID = commandID(record.OperationKey, 1, 1)
		engine.seed(resumed)
		before := engine.e.Stats()
		got, err := engine.Complete(ctx, Completion{CommandID: record.CommandID, IdempotencyKey: record.OperationKey, SagaID: record.ID, Success: false, Error: "refused in the previous life"})
		if err != nil || got.Status != StatusWaiting || got.Version != resumed.Version {
			t.Fatalf("stale refusal: record %+v err %v, want the waiting record unchanged", got, err)
		}
		if after := engine.e.Stats(); after.StaleIncarnation != before.StaleIncarnation+1 {
			t.Fatalf("stale refusal: stats %+v -> %+v, want StaleIncarnation+1", before, after)
		}
		// 旧一生的成功：记录正等这个操作，接收为结果。
		got, err = engine.Complete(ctx, Completion{CommandID: record.CommandID, IdempotencyKey: record.OperationKey, SagaID: record.ID, Success: true})
		if err != nil || got.CompletedSteps != record.CompletedSteps+1 {
			t.Fatalf("previous life's success while the new life waits: record %+v err %v, want it taken as the step result", got, err)
		}
	})
}

func TestCommandIDIncarnationInvertsCommandID(t *testing.T) {
	for _, incarnation := range []uint32{0, 1, 7, 4294967295} {
		id := commandID("gift-1:1:0", incarnation, 3)
		if got := commandIDIncarnation("gift-1:1:0", id); got != incarnation {
			t.Fatalf("commandIDIncarnation(%q) = %d, want %d", id, got, incarnation)
		}
	}
	for _, id := range []string{"manual-1", "gift-1:1:0:rx:1", "gift-1:1:0:r1", "gift-2:1:0:r1:1"} {
		if got := commandIDIncarnation("gift-1:1:0", id); got != 0 {
			t.Fatalf("commandIDIncarnation(%q) = %d, want 0 for a non-coordinator id", id, got)
		}
	}
}
