package saga

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/mongo/mongotest"
	fnats "github.com/tjbdwanghaibo/roost-core/nats"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// B1（维护者决定 2026-10-05）：协调器接收 completion 时核对代际（Resume 一生）。U-0280 的契约说“成功在任何一生里
// 都不重做、拒绝只在本生回放”，收件箱照此执行，协调器却按 IdempotencyKey 接收同一操作任一生、任一次尝试的结果。
// 独立审查列出的四个边角各一条用例，跑在 nativeWorld 上（真实 SubscribeDataEngineStep 消费者、mongotest 上真实的
// DataEngineStepInbox、真实 dataengine MongoStore.Project 投影器），手动推进时钟：
//
//   - E1：Resume 之后，上一生晚到的拒绝在新一生等待时到达，不能被新一生接收（只计数），新一生照常执行并前进；
//   - E2：放弃后迟到的成功按（操作，代际）只告警一次，不随每次重投重复计数；
//   - E3：Resume 之后、新一生派发之前到达的上一生成功，直接接收为这个操作的结果，不告警、不再派发这一步；
//   - E4：补偿方向 ManualRequired 上的人工 Compensate 进入新一生，补偿真正重新执行，而不是复用上一轮的 CommandID。
func TestCoordinatorChecksTheIncarnationOfACompletion(t *testing.T) {
	refusal := Completion{Success: false, Error: "sender no longer has the items"}

	t.Run("E1: a refusal from the previous life is not accepted by the resumed life", func(t *testing.T) {
		w := newNativeWorld(t, 1, time.Now().UTC())
		w.outcome = func(command Command) Completion {
			if strings.Contains(command.ID, ":r1:") {
				return Completion{Success: true}
			}
			return refusal
		}
		w.tick(0)
		k := w.pendingCommand()
		recordK := w.commitAttempt(w.inboxA, k)
		if !w.project(recordK) { // 拒绝在截止前投影，completion effect 还在路上
			t.Fatal("refusal projected within its deadline was skipped")
		}
		w.tick(5 * time.Second) // 唯一一次尝试超时 → Failed
		if _, err := w.engine.Resume(w.ctx, ResumeRequest{ID: w.sagaID, Now: w.at(6 * time.Second)}); err != nil {
			t.Fatal(err)
		}
		w.tick(6 * time.Second)
		resumed := w.pendingCommand()
		staleBefore := counterValue("saga.completion.stale_incarnation_total")
		w.deliverEffect(recordK) // 上一生的拒绝在新一生等待时到达
		if record := w.record(); record.Status != StatusWaiting || record.CommandID != resumed.ID {
			t.Errorf("the refusal of %s from the previous life was accepted by the resumed life waiting on %s: the saga is now %s (step %d, completed %d); the inbox would run %s anyway, so its debit ends outside CompletedSteps",
				k.ID, resumed.ID, record.Status, record.Step, record.CompletedSteps, resumed.ID)
		}
		if grown := counterValue("saga.completion.stale_incarnation_total") - staleBefore; grown != 1 {
			t.Errorf("a stale refusal from the previous life grew saga.completion.stale_incarnation_total by %d, want 1", grown)
		}
		w.deliverCommand(w.inboxB, resumed)
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
		recordK := w.commitAttempt(w.inboxA, k)
		if !w.project(recordK) {
			t.Fatal("attempt k projected within its deadline was skipped")
		}
		w.tick(5 * time.Second) // k 超时 → 退避
		if err := w.complete(w.effectCompletion(recordK)); !errors.Is(err, ErrNotWaiting) {
			t.Fatalf("completion of attempt %d during backoff = %v, want ErrNotWaiting", k.Attempt, err)
		}
		w.tick(10 * time.Second) // 最后一次尝试 k+1
		expired := w.pendingCommand()
		expired.DeadlineAt = time.Now().UTC().Add(-time.Second) // 投递晚于截止：不执行，回放 k 的成功
		w.tick(16 * time.Second)                                // k+1 超时 → 重试用尽 → Failed（放弃关闭）
		before := counterValue("saga.completion.late_after_abandon_total")
		w.deliverCommand(w.inboxB, expired)
		w.deliverReplays()
		w.deliverCommand(w.inboxB, expired) // JetStream 重投（上一次 ack 丢失）
		w.deliverReplays()
		w.deliverEffect(recordK) // k 的 completion effect 重投
		if grown := counterValue("saga.completion.late_after_abandon_total") - before; grown != 1 {
			t.Errorf("one effective success of %s arrived three times after the coordinator abandoned the step; saga.completion.late_after_abandon_total grew by %d, want 1: the alarm must count the uncompensated step, not its deliveries",
				k.ID, grown)
		}
		w.assertEffective(k.IdempotencyKey, 1)
		// saga 方向 ④（2026-10-07）：迟到的成功不再只告警，saga 被带回补偿、只补偿这一步（之前断言“Failed 不变”）。
		if record := w.record(); record.Status != StatusCompensating || record.Step != 0 || record.LateStep != 1 || record.CompletedSteps != 0 {
			t.Fatalf("late success did not bring the failed saga back to compensate that step: %+v", record)
		}
	})

	t.Run("E3: a previous life's success arriving after Resume, before dispatch, is taken as the result", func(t *testing.T) {
		w := newNativeWorld(t, 1, time.Now().UTC())
		w.tick(0)
		k := w.pendingCommand()
		recordK := w.commitAttempt(w.inboxA, k)
		if !w.project(recordK) {
			t.Fatal("attempt projected within its deadline was skipped")
		}
		w.tick(5 * time.Second) // completion 还在路上，协调器按超时放弃 → Failed
		if _, err := w.engine.Resume(w.ctx, ResumeRequest{ID: w.sagaID, Now: w.at(6 * time.Second)}); err != nil {
			t.Fatal(err)
		}
		before := counterValue("saga.completion.late_after_abandon_total")
		w.deliverEffect(recordK) // 新一生还没派发这一步
		grown := counterValue("saga.completion.late_after_abandon_total") - before
		if record := w.record(); grown != 0 || record.Step != 1 || record.CompletedSteps != 1 {
			t.Errorf("success of %s arrived after Resume, before the new life dispatched the step: late_after_abandon grew by %d (want 0) and the saga is %s at step %d with %d completed (want step 1, 1 completed): it is alarmed as uncompensated now and counted later when the inbox replays it",
				k.ID, grown, record.Status, record.Step, record.CompletedSteps)
		}
		w.tick(6 * time.Second)
		if next := w.pendingCommand(); next.Step != 1 {
			w.deliverCommand(w.inboxB, next) // 修前：新一生派发 debit，收件箱回放 k 的成功
			w.deliverReplays()
		}
		w.assertEffective(k.IdempotencyKey, 1)
		if got := w.executions(k.IdempotencyKey); got != 1 {
			t.Fatalf("debit handler ran %d times, want 1", got)
		}
	})

	t.Run("E4: manual Compensate on a compensation ManualRequired re-runs the compensation", func(t *testing.T) {
		w := newNativeWorld(t, 1, time.Now().UTC())
		refundFixed := false
		w.outcome = func(command Command) Completion {
			if command.Phase == PhaseCompensate && !refundFixed {
				return Completion{Success: false, Error: "refund target is locked"}
			}
			return Completion{Success: true}
		}
		w.tick(0)
		w.deliverCommand(w.inboxA, w.pendingCommand()) // debit 成功
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
		w.deliverCommand(w.inboxA, first) // 退款被拒 → ManualRequired
		w.deliverEffects()
		if record := w.record(); record.Status != StatusManualRequired || record.Phase != PhaseCompensate {
			t.Fatalf("refused refund did not stop at ManualRequired: %+v", record)
		}
		refundFixed = true
		if _, err := w.engine.Compensate(w.ctx, w.sagaID, "operator unlocked the refund target", w.at(3*time.Second)); err != nil {
			t.Fatal(err)
		}
		w.tick(3 * time.Second)
		again := w.pendingCommand()
		if again.ID == first.ID {
			t.Errorf("manual Compensate re-dispatched the refund as %s, the CommandID of the refused attempt: the native inbox sees a different digest under the same ID (identity conflict, nak forever) and a Mongo inbox replays the old refusal; the refund never re-runs", again.ID)
		}
		if err := w.deliverCommandErr(w.inboxB, again); err != nil {
			t.Fatalf("delivery of the re-dispatched refund %s: %v", again.ID, err)
		}
		w.deliverEffects()
		if record := w.record(); record.Status != StatusCompensated {
			t.Errorf("refund after manual Compensate did not finish the compensation: %+v", record)
		}
		if got := w.executions(first.IdempotencyKey); got != 2 {
			t.Errorf("refund handler ran %d times, want 2 (the refused one and the re-run after Compensate)", got)
		}
	})

	// 正确做法的守卫（修前即绿）：补偿方向 ManualRequired 修复原因后用 Resume，进入新一生、补偿重新执行。
	t.Run("Resume on a compensation ManualRequired re-runs the compensation", func(t *testing.T) {
		w := newNativeWorld(t, 1, time.Now().UTC())
		refundFixed := false
		w.outcome = func(command Command) Completion {
			if command.Phase == PhaseCompensate && !refundFixed {
				return Completion{Success: false, Error: "refund target is locked"}
			}
			return Completion{Success: true}
		}
		w.tick(0)
		w.deliverCommand(w.inboxA, w.pendingCommand())
		w.deliverEffects()
		w.tick(time.Second)
		deliver := w.pendingCommand()
		if err := w.complete(Completion{CommandID: deliver.ID, IdempotencyKey: deliver.IdempotencyKey, SagaID: deliver.SagaID, Success: false, Error: "recipient missing"}); err != nil {
			t.Fatal(err)
		}
		w.tick(2 * time.Second)
		first := w.pendingCommand()
		w.deliverCommand(w.inboxA, first)
		w.deliverEffects()
		refundFixed = true
		if _, err := w.engine.Resume(w.ctx, ResumeRequest{ID: w.sagaID, Now: w.at(3 * time.Second)}); err != nil {
			t.Fatal(err)
		}
		w.tick(3 * time.Second)
		again := w.pendingCommand()
		if err := w.deliverCommandErr(w.inboxB, again); err != nil {
			t.Fatalf("delivery of the resumed refund %s: %v", again.ID, err)
		}
		w.deliverEffects()
		if record := w.record(); record.Status != StatusCompensated || w.executions(first.IdempotencyKey) != 2 {
			t.Fatalf("Resume did not re-run the refused refund: %+v executions=%d", record, w.executions(first.IdempotencyKey))
		}
	})
}

// deliverCommandErr 与 deliverCommand 相同，但把消费者对这次投递的结论（nil = ack，非 nil = nak 重投）交回调用方。
func (w *nativeWorld) deliverCommandErr(inbox *DataEngineStepInbox, command Command) error {
	w.t.Helper()
	client := &replayJetStream{world: w}
	transport, _ := NewJetStreamPublisher(client, "roost.saga")
	handler := func(ctx context.Context, command Command) (Completion, error) {
		reservation, ok := ReservationFromContext(ctx)
		if !ok {
			return Completion{}, errors.New("no reservation")
		}
		record := w.runHandler(inbox, command, reservation)
		if w.project(record) {
			w.mu.Lock()
			w.effects = append(w.effects, record)
			w.mu.Unlock()
		}
		return w.effectCompletion(record), nil
	}
	config := StepConsumerConfig{Stream: "ROOST_SAGA", Durable: "game-gift-step", Topic: "gift.step", AckWait: 30 * time.Second}
	if _, err := SubscribeDataEngineStep(w.ctx, client, transport, inbox, config, handler); err != nil {
		w.t.Fatal(err)
	}
	raw, err := json.Marshal(commandEnvelope{Version: WireVersion, Command: command})
	if err != nil {
		w.t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(w.ctx, 2*time.Second)
	defer cancel()
	return client.handler(ctx, &fnats.JetStreamMsg{Data: raw})
}

// B1 在 MongoStore 上：放弃后迟到的成功按（操作，代际）只告警一次；标记写在 tombstone 的 late_alarms 上，
// 旧 tombstone 没有这个字段时第一次照常告警；没有 tombstone 不标记。
func TestMongoStoreMarksALateSuccessAlarmOncePerLife(t *testing.T) {
	ctx := context.Background()
	t.Run("engine alarms the first delivery and counts the rest as duplicates", func(t *testing.T) {
		engine, _, record := waitingOnMongo(t)
		due := record.Clone()
		due.Attempt = 2 // march 的 MaxAttempts 2：超时即用尽，放弃关闭
		if err := engine.processClaimed(ctx, due, record.NextRunAt.Add(time.Second)); err != nil {
			t.Fatal(err)
		}
		late := Completion{CommandID: record.CommandID, IdempotencyKey: record.OperationKey, SagaID: record.ID, Success: true}
		before := engine.Stats()
		for i := 0; i < 3; i++ {
			if _, err := engine.Complete(ctx, late); err != nil {
				t.Fatal(err)
			}
		}
		if after := engine.Stats(); after.LateAfterAbandon != before.LateAfterAbandon+1 || after.Duplicates != before.Duplicates+2 {
			t.Fatalf("three deliveries of one late success: stats %+v -> %+v, want one alarm and two duplicates", before, after)
		}
	})
	t.Run("store marks each life once, legacy tombstones included", func(t *testing.T) {
		_, store, record := waitingOnMongo(t)
		late := Completion{CommandID: record.CommandID, IdempotencyKey: record.OperationKey, SagaID: record.ID, Success: true}
		if first, err := store.MarkLateSuccessAlarm(ctx, late, 0); err != nil || first {
			t.Fatalf("mark without a tombstone = %t, %v; want false", first, err)
		}
		// U-0280 之前形状的 tombstone：没有 closure、没有 late_alarms。
		if err := store.client.(*mongotest.Client).Collection("saga", defaultOperationCollection).Seed(bson.M{"_id": record.OperationKey, "saga_id": record.ID, "closure": operationClosureAbandoned, "created_at": record.CreatedAt}); err != nil {
			t.Fatal(err)
		}
		for _, step := range []struct {
			incarnation uint32
			want        bool
		}{{0, true}, {0, false}, {1, true}, {1, false}, {0, false}} {
			if first, err := store.MarkLateSuccessAlarm(ctx, late, step.incarnation); err != nil || first != step.want {
				t.Fatalf("mark incarnation %d = %t, %v; want %t", step.incarnation, first, err, step.want)
			}
		}
		other := late
		other.SagaID = "gift-2"
		if first, err := store.MarkLateSuccessAlarm(ctx, other, 2); err != nil || first {
			t.Fatalf("mark through another saga's id = %t, %v; want false", first, err)
		}
	})
	t.Run("a stale result is ignored and counted on MongoStore", func(t *testing.T) {
		engine, store, record := waitingOnMongo(t)
		resumed := record.Clone()
		resumed.Version++
		resumed.Incarnation = 1
		resumed.CommandID = commandID(record.OperationKey, 1, 1)
		if _, err := store.Apply(ctx, ApplyRequest{ExpectedVersion: record.Version, After: resumed}); err != nil {
			t.Fatal(err)
		}
		before := engine.Stats()
		got, err := engine.Complete(ctx, Completion{CommandID: record.CommandID, IdempotencyKey: record.OperationKey, SagaID: record.ID, Success: false, Error: "refused in the previous life"})
		if err != nil || got.Status != StatusWaiting || got.Version != resumed.Version {
			t.Fatalf("stale refusal: record %+v err %v, want the waiting record unchanged", got, err)
		}
		if after := engine.Stats(); after.StaleIncarnation != before.StaleIncarnation+1 {
			t.Fatalf("stale refusal: stats %+v -> %+v, want StaleIncarnation+1", before, after)
		}
		// 旧一生的成功：记录正等这个操作，接收为结果。
		got, err = engine.Complete(ctx, Completion{CommandID: record.CommandID, IdempotencyKey: record.OperationKey, SagaID: record.ID, Success: true})
		if err != nil || got.Status == StatusWaiting || got.CompletedSteps != record.CompletedSteps+1 {
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
