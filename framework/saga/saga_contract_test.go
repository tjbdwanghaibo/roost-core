package saga

import (
	"bytes"
	"context"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestNativeStepStaleAttemptRetryableFailureDoesNotAbandonTheLastAttempt(t *testing.T) {
	w := newNativeWorld(t, 2, time.Now().UTC())
	w.outcome = func(command Command) Completion {
		if command.Attempt == 1 {
			return Completion{Success: false, Retryable: true, Error: "busy"}
		}
		return Completion{Success: true}
	}
	w.tick(0)
	k := w.pendingCommand()
	recordK := w.commitAttempt(w.inboxA, k)
	if !w.project(recordK) { // k 在截止前投影：一次可重试失败，completion effect 还在路上
		t.Fatal("attempt k projected within its deadline was skipped")
	}
	w.tick(5 * time.Second)  // k 超时 → 退避
	w.tick(10 * time.Second) // 发出最后一次尝试 k+1
	k1 := w.pendingCommand()
	staleBefore := counterValue("saga.completion.stale_attempt_total")
	lateBefore := counterValue("saga.completion.late_after_abandon_total")
	w.deliverEffect(recordK) // k 的可重试失败在协调器等 k+1 时到达
	if record := w.record(); record.Status != StatusWaiting || record.CommandID != k1.ID {
		t.Fatalf("a retryable failure of earlier attempt %s moved the record while the coordinator waits for %s: status=%s step=%d command=%q last_error=%q; want still waiting for %s",
			k.ID, k1.ID, record.Status, record.Step, record.CommandID, record.LastError, k1.ID)
	}
	if grown := counterValue("saga.completion.stale_attempt_total") - staleBefore; grown != 1 {
		t.Errorf("stale_attempt_total grew by %d, want 1 for the ignored failure of %s", grown, k.ID)
	}
	w.deliverCommand(w.inboxB, k1) // k+1 看到 k 只是可重试失败 → 执行并生效
	w.deliverEffects()
	w.assertEffective(k.IdempotencyKey, 1)
	record := w.record()
	if record.Phase != PhaseForward || record.CompletedSteps != 1 || record.Step != 1 {
		t.Fatalf("the last attempt %s took effect but the saga did not count it: status=%s phase=%s step=%d completed=%d", k1.ID, record.Status, record.Phase, record.Step, record.CompletedSteps)
	}
	if grown := counterValue("saga.completion.late_after_abandon_total") - lateBefore; grown != 0 {
		t.Errorf("late_after_abandon_total grew by %d, want 0: the step was never abandoned", grown)
	}
}

// ③ 的边界（守卫）：同一生较早尝试的拒绝是操作的结论，下一次尝试回放它（CommandID 是被拒的那次），协调器必须接收，
// 而不是当作“不是正在等的尝试”丢掉、等到重试用尽。
func TestNativeStepReplayedRefusalOfAnEarlierAttemptIsAccepted(t *testing.T) {
	w := newNativeWorld(t, 3, time.Now().UTC())
	w.outcome = func(Command) Completion { return Completion{Success: false, Error: "out of stock"} }
	w.tick(0)
	k := w.pendingCommand()
	if recordK := w.commitAttempt(w.inboxA, k); !w.project(recordK) { // 拒绝已结算，effect 还没送达
		t.Fatal("attempt k projected within its deadline was skipped")
	}
	w.tick(5 * time.Second)  // k 超时 → 退避
	w.tick(10 * time.Second) // 第二次尝试
	k1 := w.pendingCommand()
	w.deliverCommand(w.inboxB, k1) // 收件箱看到本生的拒绝 → 不执行，回放 k 的 completion
	w.deliverReplays()
	if got := w.executions(k.IdempotencyKey); got != 1 {
		t.Fatalf("handler ran %d times, want 1 (the refusal must be replayed, not re-executed)", got)
	}
	if record := w.record(); record.Status != StatusFailed || record.LastError != "out of stock" {
		t.Fatalf("the replayed refusal of %s was not accepted while waiting for %s: status=%s last_error=%q", k.ID, k1.ID, record.Status, record.LastError)
	}
}

// ④：第 0 步最后一次尝试在截止前生效，completion 在 saga Failed 之后才到 → 重开，只补偿第 0 步，载荷是那次成功的 Data。
func TestNativeStepLateSuccessReopensAFailedSagaToCompensateTheStep(t *testing.T) {
	w := newNativeWorld(t, 1, time.Now().UTC())
	w.outcome = func(Command) Completion { return Completion{Success: true, Data: []byte("debit-receipt")} }
	w.tick(0)
	k := w.pendingCommand()
	recordK := w.commitAttempt(w.inboxA, k)
	if !w.project(recordK) {
		t.Fatal("attempt k projected within its deadline was skipped")
	}
	w.tick(5 * time.Second) // 唯一一次尝试超时 → 用尽 → Failed
	if record := w.record(); record.Status != StatusFailed {
		t.Fatalf("want Failed after the only attempt timed out: %+v", record)
	}
	lateBefore := counterValue("saga.completion.late_after_abandon_total")
	duplicatesBefore := w.engine.Stats().Duplicates
	w.deliverEffect(recordK) // 已生效的扣款，completion 晚于放弃到达
	if record := w.record(); record.Status != StatusCompensating || record.Phase != PhaseCompensate || record.Step != 0 {
		t.Fatalf("step %s took effect after the saga failed, but the saga was not brought back to compensate it: status=%s phase=%s step=%d completed=%d",
			k.IdempotencyKey, record.Status, record.Phase, record.Step, record.CompletedSteps)
	}
	w.tick(5 * time.Second)
	compensation := w.pendingCommand()
	if compensation.Phase != PhaseCompensate || compensation.Step != 0 || compensation.IdempotencyKey != w.sagaID+":2:0" || !bytes.Equal(compensation.Payload, []byte("debit-receipt")) {
		t.Fatalf("compensation command = %+v, want step 0 compensate with the late success's data as payload", compensation)
	}
	if err := w.complete(Completion{CommandID: compensation.ID, IdempotencyKey: compensation.IdempotencyKey, SagaID: w.sagaID, Success: true}); err != nil {
		t.Fatal(err)
	}
	if record := w.record(); record.Status != StatusCompensated || record.CompletedSteps != 0 {
		t.Fatalf("after compensating the late step: status=%s completed=%d, want Compensated", record.Status, record.CompletedSteps)
	}
	w.deliverEffect(recordK) // 同一个成功再次送达：重复
	if record := w.record(); record.Status != StatusCompensated {
		t.Fatalf("a redelivery of the late success reopened the saga again: %+v", record)
	}
	if grown := counterValue("saga.completion.late_after_abandon_total") - lateBefore; grown != 1 {
		t.Errorf("late_after_abandon_total grew by %d, want 1 (counted once per operation)", grown)
	}
	if grown := w.engine.Stats().Duplicates - duplicatesBefore; grown != 1 {
		t.Errorf("Duplicates grew by %d, want 1 for the redelivery", grown)
	}
}

// lateSecondStepWorld：第 0 步成功，第 1 步的一次尝试在截止前生效、completion 还在路上，saga 截止让协调器放弃第 1 步、开始补偿第 0 步。
func lateSecondStepWorld(t *testing.T) (*nativeWorld, Command) {
	t.Helper()
	w := newNativeWorldWithDeadline(t, 2, time.Now().UTC(), 3*time.Second)
	w.outcome = func(command Command) Completion {
		return Completion{Success: true, Data: []byte("data-of-" + command.StepName)}
	}
	w.tick(0)
	first := w.pendingCommand()
	recordFirst := w.commitAttempt(w.inboxA, first)
	if !w.project(recordFirst) {
		t.Fatal("step 0 projected within its deadline was skipped")
	}
	w.deliverEffect(recordFirst)
	w.tick(time.Second) // 派发第 1 步，命令截止 t0+6s
	second := w.pendingCommand()
	if second.Step != 1 {
		t.Fatalf("want step 1 dispatched, got %+v", second)
	}
	recordSecond := w.commitAttempt(w.inboxA, second)
	if !w.project(recordSecond) {
		t.Fatal("step 1 projected within its deadline was skipped")
	}
	w.tick(6 * time.Second) // 第 1 步截止已过且 saga 截止已过 → 放弃第 1 步，补偿第 0 步
	if record := w.record(); record.Status != StatusCompensating || record.Step != 0 || record.CompletedSteps != 1 {
		t.Fatalf("want compensating step 0 after the saga deadline: %+v", record)
	}
	w.mu.Lock()
	w.effects = append(w.effects, recordSecond) // 第 1 步的 completion effect 稍后由测试送达
	w.mu.Unlock()
	return w, second
}

func (w *nativeWorld) completeCompensation(command Command, data string) {
	w.t.Helper()
	completion := Completion{CommandID: command.ID, IdempotencyKey: command.IdempotencyKey, SagaID: w.sagaID, Success: true}
	if data != "" {
		completion.Data = []byte(data)
	}
	if err := w.complete(completion); err != nil {
		w.t.Fatalf("complete compensation %s: %v", command.ID, err)
	}
}

// ④：补偿已结束（Compensated）之后第 1 步的成功才到 → 只补偿第 1 步，第 0 步不再补偿第二次。
func TestNativeStepLateSuccessAfterCompensatedCompensatesOnlyThatStep(t *testing.T) {
	w, second := lateSecondStepWorld(t)
	w.tick(6 * time.Second)
	compensateFirst := w.pendingCommand()
	w.completeCompensation(compensateFirst, "")
	if record := w.record(); record.Status != StatusCompensated {
		t.Fatalf("want Compensated before the late success arrives: %+v", record)
	}
	w.deliverEffects() // 第 1 步的成功晚到
	if record := w.record(); record.Status != StatusCompensating || record.Step != 1 {
		t.Fatalf("step %s took effect after it was abandoned, but the compensated saga was not reopened to compensate it: status=%s step=%d", second.IdempotencyKey, record.Status, record.Step)
	}
	w.tick(7 * time.Second)
	compensateSecond := w.pendingCommand()
	if compensateSecond.IdempotencyKey != w.sagaID+":2:1" || !bytes.Equal(compensateSecond.Payload, []byte("data-of-deliver")) {
		t.Fatalf("want only step 1 compensated with its own data, got %+v", compensateSecond)
	}
	w.completeCompensation(compensateSecond, "")
	if record := w.record(); record.Status != StatusCompensated {
		t.Fatalf("after compensating the late step: %+v", record)
	}
	w.tick(8 * time.Second)
	w.store.mu.Lock()
	queued := len(w.store.outbox)
	w.store.mu.Unlock()
	if queued != 0 {
		t.Fatalf("%d more commands queued after the late step was compensated; step 0 must not be compensated twice", queued)
	}
}

// ④：迟到成功到达时第 0 步的补偿正在等结果 → 不打断它；它结束后补偿第 1 步，第 1 步补偿的结果不进入 Data 链。
func TestNativeStepLateSuccessDuringAnInFlightCompensationIsCompensatedNext(t *testing.T) {
	w, second := lateSecondStepWorld(t)
	w.tick(6 * time.Second)
	compensateFirst := w.pendingCommand() // 第 0 步的补偿在途
	w.deliverEffects()                    // 第 1 步的成功晚到
	if record := w.record(); record.Status != StatusWaiting || record.CommandID != compensateFirst.ID {
		t.Fatalf("the late success interrupted the in-flight compensation %s: %+v", compensateFirst.ID, record)
	}
	w.completeCompensation(compensateFirst, "refund-0")
	if record := w.record(); record.Status != StatusCompensating || record.Step != 1 {
		t.Fatalf("after the in-flight compensation the late step %s must be compensated next: status=%s step=%d completed=%d",
			second.IdempotencyKey, record.Status, record.Step, record.CompletedSteps)
	}
	w.tick(7 * time.Second)
	compensateSecond := w.pendingCommand()
	if compensateSecond.Step != 1 || !bytes.Equal(compensateSecond.Payload, []byte("data-of-deliver")) {
		t.Fatalf("compensation of the late step = %+v, want step 1 with its own forward data", compensateSecond)
	}
	w.completeCompensation(compensateSecond, "undo-1")
	record := w.record()
	if record.Status != StatusCompensated || string(record.Data) != "refund-0" {
		t.Fatalf("after both compensations: status=%s data=%q, want Compensated with the regular chain's data refund-0", record.Status, record.Data)
	}
}

// ④：补偿被拒进入 ManualRequired 之后迟到的成功只记下，运维 Resume 时先补偿迟到的那一步，再补偿前缀。
func TestNativeStepLateSuccessOnManualRequiredIsCompensatedOnResume(t *testing.T) {
	w, second := lateSecondStepWorld(t)
	w.tick(6 * time.Second)
	compensateFirst := w.pendingCommand()
	if err := w.complete(Completion{CommandID: compensateFirst.ID, IdempotencyKey: compensateFirst.IdempotencyKey, SagaID: w.sagaID, Success: false, Error: "refund rejected"}); err != nil {
		t.Fatal(err)
	}
	if record := w.record(); record.Status != StatusManualRequired {
		t.Fatalf("want ManualRequired after the compensation was refused: %+v", record)
	}
	w.deliverEffects() // 第 1 步的成功晚到
	if record := w.record(); record.Status != StatusManualRequired {
		t.Fatalf("a late success must not run compensations on a record waiting for an operator: %+v", record)
	}
	if _, err := w.engine.Resume(w.ctx, ResumeRequest{ID: w.sagaID, Now: w.at(7 * time.Second)}); err != nil {
		t.Fatal(err)
	}
	w.tick(7 * time.Second)
	compensateSecond := w.pendingCommand()
	if compensateSecond.Step != 1 || compensateSecond.Phase != PhaseCompensate {
		t.Fatalf("after Resume the late step %s must be compensated first, got %+v", second.IdempotencyKey, compensateSecond)
	}
	w.completeCompensation(compensateSecond, "")
	w.tick(8 * time.Second)
	compensateFirstAgain := w.pendingCommand()
	if compensateFirstAgain.Step != 0 {
		t.Fatalf("after the late step the prefix is compensated, got %+v", compensateFirstAgain)
	}
	w.completeCompensation(compensateFirstAgain, "")
	if record := w.record(); record.Status != StatusCompensated {
		t.Fatalf("want Compensated: %+v", record)
	}
}

// ④ 只管正向：补偿方向放弃之后迟到的成功仍只告警（Resume 后新一生回放它）。
func TestNativeStepLateCompensationSuccessIsOnlyAlarmed(t *testing.T) {
	w, _ := lateSecondStepWorld(t) // 第 1 步的迟到成功留在 effects 里不送达，只看第 0 步的补偿
	w.tick(6 * time.Second)
	w.pendingCommand()       // 补偿第 0 步，第一次尝试（截止 t0+11s）
	w.tick(11 * time.Second) // 超时 → 退避
	w.tick(12 * time.Second)
	retry := w.pendingCommand() // 第二次尝试（截止 t0+17s）
	w.tick(17 * time.Second)    // 也超时 → 用尽 → ManualRequired
	if record := w.record(); record.Status != StatusManualRequired {
		t.Fatalf("want ManualRequired after the compensation timed out twice: %+v", record)
	}
	lateBefore := counterValue("saga.completion.late_after_abandon_total")
	w.completeCompensation(retry, "") // 补偿其实生效了，completion 晚到
	if record := w.record(); record.Status != StatusManualRequired || record.Phase != PhaseCompensate || record.Step != 0 {
		t.Fatalf("a late compensation success changed the record: %+v", record)
	}
	if grown := counterValue("saga.completion.late_after_abandon_total") - lateBefore; grown != 1 {
		t.Errorf("late_after_abandon_total grew by %d, want 1 (compensate-direction late success is alarmed for the operator)", grown)
	}
}

// ③ 在 MongoStore 上：同一生较早尝试的可重试失败不写记录、计 StaleAttempt；正在等的尝试的可重试失败照常接收。
func TestMongoStoreIgnoresARetryableFailureOfAnEarlierAttempt(t *testing.T) {
	engine, store, record := waitingOnMongo(t)
	ctx := context.Background()
	current := record.Clone()
	current.Version++
	current.Attempt = 2
	current.CommandID = commandID(record.OperationKey, 0, 2)
	if _, err := store.Apply(ctx, ApplyRequest{ExpectedVersion: record.Version, After: current}); err != nil {
		t.Fatal(err)
	}
	earlier := Completion{CommandID: record.CommandID, IdempotencyKey: record.OperationKey, SagaID: record.ID, Success: false, Retryable: true, Error: "busy"}
	if _, err := engine.Complete(ctx, earlier); err != nil {
		t.Fatalf("stale retryable failure = %v, want acknowledged", err)
	}
	if stored, _ := store.Get(ctx, record.ID); stored.Version != current.Version || stored.Status != StatusWaiting {
		t.Fatalf("a stale retryable failure wrote the record: %+v", stored)
	}
	if got := engine.Stats().StaleAttempt; got != 1 {
		t.Fatalf("StaleAttempt = %d, want 1", got)
	}
	own := earlier
	own.CommandID = current.CommandID
	after, err := engine.Complete(ctx, own)
	if err != nil || after.Status != StatusCompensating {
		t.Fatalf("the current attempt's retryable failure on the last attempt = %+v, %v; want accepted (exhausted → compensating)", after, err)
	}
}

// ④ 在 MongoStore 上：迟到的步骤与载荷随记录持久化（late_step / late_data），回执与带结果关闭在同一事务里，补偿命令带它的载荷。
func TestMongoStoreLateStepCompensationRoundTrip(t *testing.T) {
	engine, store, record := waitingOnMongo(t)
	ctx := context.Background()
	t0 := time.Now().UTC()
	claimAndProcess := func(at time.Time) {
		t.Helper()
		claimed, err := store.ClaimDue(ctx, ClaimRequest{Owner: "coordinator", Now: at, LeaseDuration: time.Second, Limit: 10})
		if err != nil || len(claimed) != 1 {
			t.Fatalf("claim at %s: %d records, %v", at, len(claimed), err)
		}
		if err := engine.processClaimed(ctx, claimed[0], at); err != nil {
			t.Fatal(err)
		}
	}
	claimAndProcess(t0.Add(2 * time.Minute)) // 第 1 步第 1 次尝试超时 → 退避
	claimAndProcess(t0.Add(3 * time.Minute)) // 第 2 次尝试
	claimAndProcess(t0.Add(4 * time.Minute)) // 超时 → 用尽 → 补偿第 0 步（第 1 步放弃关闭）
	late := Completion{CommandID: record.CommandID, IdempotencyKey: record.OperationKey, SagaID: record.ID, Success: true, Data: []byte("march-data")}
	if _, err := engine.Complete(ctx, late); err != nil {
		t.Fatal(err)
	}
	stored, err := store.Get(ctx, record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != StatusCompensating || stored.Step != 1 || stored.LateStep != 2 || string(stored.LateData) != "march-data" || stored.CompletedSteps != 1 {
		t.Fatalf("stored record after the late success: %+v", stored)
	}
	if history, err := store.CompletionHistory(ctx, late); err != nil || !history.Receipt {
		t.Fatalf("late success history = %+v, %v; want its receipt recorded with the transition", history, err)
	}
	claimAndProcess(time.Now().UTC().Add(time.Second))
	outbox, err := store.ClaimOutbox(ctx, ClaimRequest{Owner: "publisher", Now: time.Now().UTC().Add(time.Second), LeaseDuration: time.Second, Limit: 10})
	if err != nil || len(outbox) != 1 {
		t.Fatalf("outbox: %d, %v", len(outbox), err)
	}
	compensation := outbox[0].Command
	if compensation.IdempotencyKey != operationKey(record.ID, PhaseCompensate, 1) || string(compensation.Payload) != "march-data" {
		t.Fatalf("compensation command = %+v", compensation)
	}
	after, err := engine.Complete(ctx, Completion{CommandID: compensation.ID, IdempotencyKey: compensation.IdempotencyKey, SagaID: record.ID, Success: true})
	if err != nil {
		t.Fatal(err)
	}
	if after.Status != StatusCompensating || after.Step != 0 || after.LateStep != 0 || after.LateData != nil || after.CompletedSteps != 1 {
		t.Fatalf("after compensating the late step: %+v; want compensating step 0 with the late step cleared", after)
	}
}

func reopenedCount(from, reason string) int64 {
	var total int64
	for _, metric := range metrics.Snapshot() {
		if metric.Name == "saga.reopened_total" && metric.Labels["saga_type"] == "gift" &&
			metric.Labels["from_status"] == from && metric.Labels["reason"] == reason {
			total += metric.Value
		}
	}
	return total
}

// lockedBuffer 让 slog 的写与测试的读不竞争。
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// captureWarnLogs 把默认 slog 换成写进缓冲的 WARN 级处理器，测试结束换回。
func captureWarnLogs(t *testing.T) *lockedBuffer {
	t.Helper()
	logs := &lockedBuffer{}
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return logs
}

// reopenLogLine 返回日志里那一条“saga 重开”的行；没有返回空串。
func reopenLogLine(logs *lockedBuffer, sagaID string) string {
	for _, line := range strings.Split(logs.String(), "\n") {
		if strings.Contains(line, "saga: reopened") && strings.Contains(line, "saga_id="+sagaID) {
			return line
		}
	}
	return ""
}

func assertReopenLog(t *testing.T, logs *lockedBuffer, sagaID string, fields ...string) {
	t.Helper()
	line := reopenLogLine(logs, sagaID)
	if line == "" {
		t.Fatalf("no WARN \"saga: reopened\" line for %s; logs:\n%s", sagaID, logs.String())
	}
	for _, field := range append([]string{"level=WARN", "saga_type=gift"}, fields...) {
		if !strings.Contains(line, field) {
			t.Errorf("reopen log line lacks %q: %s", field, line)
		}
	}
}

// Failed 被迟到的第 0 步成功重开：计一次 {from_status=failed, reason=late_success}，WARN 写明原终态与迟到步骤；
// 同一成功再次送达不再计。
func TestReopeningAFailedSagaIsCountedAndLogged(t *testing.T) {
	logs := captureWarnLogs(t)
	w := newNativeWorld(t, 1, time.Now().UTC())
	w.outcome = func(Command) Completion { return Completion{Success: true, Data: []byte("debit-receipt")} }
	w.tick(0)
	k := w.pendingCommand()
	recordK := w.commitAttempt(w.inboxA, k)
	if !w.project(recordK) {
		t.Fatal("attempt k projected within its deadline was skipped")
	}
	w.tick(5 * time.Second) // 唯一一次尝试超时 → Failed
	if record := w.record(); record.Status != StatusFailed {
		t.Fatalf("want Failed after the only attempt timed out: %+v", record)
	}
	before := reopenedCount("failed", "late_success")
	w.deliverEffect(recordK) // 迟到的成功把 Failed 重开
	if record := w.record(); record.Status != StatusCompensating {
		t.Fatalf("precondition (direction 4): the late success must reopen the saga: %+v", record)
	}
	if grown := reopenedCount("failed", "late_success") - before; grown != 1 {
		t.Fatalf("a Failed saga was reopened by a late success of %s, but saga.reopened_total{from_status=failed,reason=late_success} grew by %d, want 1",
			k.IdempotencyKey, grown)
	}
	assertReopenLog(t, logs, w.sagaID, "from_status=failed", "reason=late_success", "late_step=0")
	if got := w.engine.Stats().Reopened; got != 1 {
		t.Errorf("Stats().Reopened = %d, want 1", got)
	}

	w.tick(5 * time.Second)
	compensation := w.pendingCommand()
	w.completeCompensation(compensation, "")
	if record := w.record(); record.Status != StatusCompensated {
		t.Fatalf("after compensating the late step: %+v", record)
	}
	w.deliverEffect(recordK) // 重复送达：不是又一次重开
	if grown := reopenedCount("failed", "late_success") - before; grown != 1 {
		t.Errorf("saga.reopened_total grew by %d after a redelivery, want still 1", grown)
	}
}

// Compensated 被迟到的第 1 步成功重开：计一次 {from_status=compensated, reason=late_success}。
func TestReopeningACompensatedSagaIsCountedAndLogged(t *testing.T) {
	logs := captureWarnLogs(t)
	w, second := lateSecondStepWorld(t)
	w.tick(6 * time.Second)
	w.completeCompensation(w.pendingCommand(), "")
	if record := w.record(); record.Status != StatusCompensated {
		t.Fatalf("want Compensated before the late success arrives: %+v", record)
	}
	before := reopenedCount("compensated", "late_success")
	w.deliverEffects()
	if record := w.record(); record.Status != StatusCompensating || record.Step != 1 {
		t.Fatalf("precondition (direction 4): the compensated saga must be reopened: %+v", record)
	}
	if grown := reopenedCount("compensated", "late_success") - before; grown != 1 {
		t.Fatalf("a Compensated saga was reopened by a late success of %s, but saga.reopened_total{from_status=compensated,reason=late_success} grew by %d, want 1",
			second.IdempotencyKey, grown)
	}
	assertReopenLog(t, logs, w.sagaID, "from_status=compensated", "reason=late_success", "late_step=1")
}

// 迟到成功到达时补偿还在进行（记录不是终态）：不是重开，不计数。
func TestLateSuccessDuringCompensationIsNotAReopen(t *testing.T) {
	logs := captureWarnLogs(t)
	w, _ := lateSecondStepWorld(t)
	w.tick(6 * time.Second)
	w.pendingCommand() // 第 0 步的补偿在途
	beforeFailed, beforeCompensated := reopenedCount("failed", "late_success"), reopenedCount("compensated", "late_success")
	w.deliverEffects()
	if grown := reopenedCount("failed", "late_success") + reopenedCount("compensated", "late_success") - beforeFailed - beforeCompensated; grown != 0 {
		t.Fatalf("saga.reopened_total grew by %d for a saga that had not ended, want 0", grown)
	}
	if line := reopenLogLine(logs, w.sagaID); line != "" {
		t.Fatalf("a saga that had not ended was logged as reopened: %s", line)
	}
}

// ManualRequired 期间记下的迟到步骤：记下时不计（记录仍等运维），Resume 先补它时计一次 {from_status=manual_required, reason=resume}。
func TestResumeCompensatingALateStepIsCountedAndLogged(t *testing.T) {
	logs := captureWarnLogs(t)
	w, _ := lateSecondStepWorld(t)
	w.tick(6 * time.Second)
	compensateFirst := w.pendingCommand()
	if err := w.complete(Completion{CommandID: compensateFirst.ID, IdempotencyKey: compensateFirst.IdempotencyKey, SagaID: w.sagaID, Success: false, Error: "refund rejected"}); err != nil {
		t.Fatal(err)
	}
	before := reopenedCount("manual_required", "resume")
	w.deliverEffects() // 第 1 步的成功晚到，只记下 LateStep
	if record := w.record(); record.Status != StatusManualRequired || record.LateStep != 2 {
		t.Fatalf("precondition (direction 4): the late step must be recorded on ManualRequired: %+v", record)
	}
	if grown := reopenedCount("manual_required", "resume") - before; grown != 0 {
		t.Fatalf("saga.reopened_total grew by %d when the late step was only recorded, want 0 (the record still waits for the operator)", grown)
	}
	if _, err := w.engine.Resume(w.ctx, ResumeRequest{ID: w.sagaID, Now: w.at(7 * time.Second)}); err != nil {
		t.Fatal(err)
	}
	if grown := reopenedCount("manual_required", "resume") - before; grown != 1 {
		t.Fatalf("Resume compensates the late step 1 recorded during ManualRequired, but saga.reopened_total{from_status=manual_required,reason=resume} grew by %d, want 1", grown)
	}
	assertReopenLog(t, logs, w.sagaID, "from_status=manual_required", "reason=resume", "late_step=1")
}

// ManualRequired 上没有迟到步骤的普通 Resume 不是重开。
func TestPlainResumeIsNotAReopen(t *testing.T) {
	logs := captureWarnLogs(t)
	w, _ := lateSecondStepWorld(t)
	w.tick(6 * time.Second)
	compensateFirst := w.pendingCommand()
	if err := w.complete(Completion{CommandID: compensateFirst.ID, IdempotencyKey: compensateFirst.IdempotencyKey, SagaID: w.sagaID, Success: false, Error: "refund rejected"}); err != nil {
		t.Fatal(err)
	}
	before := reopenedCount("manual_required", "resume")
	if _, err := w.engine.Resume(w.ctx, ResumeRequest{ID: w.sagaID, Now: w.at(7 * time.Second)}); err != nil {
		t.Fatal(err)
	}
	if grown := reopenedCount("manual_required", "resume") - before; grown != 0 {
		t.Fatalf("a Resume without a late step grew saga.reopened_total by %d, want 0", grown)
	}
	if line := reopenLogLine(logs, w.sagaID); line != "" {
		t.Fatalf("a plain Resume was logged as a reopen: %s", line)
	}
}

// 人工 Compensate 与 Resume 等价（ManualRequired 上换代、先补迟到步骤）：计一次 {from_status=manual_required, reason=compensate}。
func TestManualCompensateOfALateStepIsCountedAndLogged(t *testing.T) {
	logs := captureWarnLogs(t)
	w, _ := lateSecondStepWorld(t)
	w.tick(6 * time.Second)
	compensateFirst := w.pendingCommand()
	if err := w.complete(Completion{CommandID: compensateFirst.ID, IdempotencyKey: compensateFirst.IdempotencyKey, SagaID: w.sagaID, Success: false, Error: "refund rejected"}); err != nil {
		t.Fatal(err)
	}
	w.deliverEffects() // 第 1 步的成功晚到，只记下 LateStep
	before := reopenedCount("manual_required", "compensate")
	record, err := w.engine.Compensate(w.ctx, w.sagaID, "operator", w.at(7*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if record.Status != StatusCompensating || record.Step != 1 {
		t.Fatalf("Compensate must compensate the late step first: %+v", record)
	}
	if grown := reopenedCount("manual_required", "compensate") - before; grown != 1 {
		t.Fatalf("saga.reopened_total{from_status=manual_required,reason=compensate} grew by %d, want 1", grown)
	}
	assertReopenLog(t, logs, w.sagaID, "from_status=manual_required", "reason=compensate", "late_step=1")
	if got := w.engine.Stats().Reopened; got != 1 {
		t.Errorf("Stats().Reopened = %d, want 1", got)
	}
}
