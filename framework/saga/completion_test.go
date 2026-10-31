package saga

import (
	"context"
	"errors"
	"testing"
	"time"
)

// 完成判定与代际变更按实际结果验证，不约束内部函数必须如何组织。
func TestJudgeCompletionIsTheUnifiedRule(t *testing.T) {
	const current uint32 = 2
	operation := operationKey("g", testLifecycle, PhaseForward, 1)
	other := operationKey("g", testLifecycle, PhaseForward, 0)
	waitingFor := commandID(operation, current, 3)
	positions := map[string]Record{
		"waiting":     {Lifecycle: testLifecycle, ID: "g", Phase: PhaseForward, Step: 1, Status: StatusWaiting, OperationKey: operation, CommandID: waitingFor, Attempt: 3},
		"backoff":     {Lifecycle: testLifecycle, ID: "g", Phase: PhaseForward, Step: 1, Status: StatusPending, Attempt: 3},
		"to dispatch": {Lifecycle: testLifecycle, ID: "g", Phase: PhaseForward, Step: 1, Status: StatusPending},
		"elsewhere":   {Lifecycle: testLifecycle, ID: "g", Phase: PhaseForward, Step: 0, Status: StatusWaiting, OperationKey: other, CommandID: commandID(other, current, 1), Attempt: 1},
	}
	incarnations := map[string]uint32{"older": current - 1, "same": current, "newer": current + 1}
	kinds := []string{"success", "refusal", "retryable of the waited attempt", "retryable of an earlier attempt"}

	// want 按方案第 1 节的表逐行写，不复用 judgeCompletion 的分支。
	want := func(kind, life, position string) completionVerdict {
		if life == "newer" {
			return verdictStaleIncarnation // 0
		}
		if kind == "success" {
			opened := (life == "same" && (position == "waiting" || position == "backoff")) || (life == "older" && position != "elsewhere")
			if opened {
				return verdictAccept // S
			}
			return verdictHistory // S'
		}
		if life == "older" {
			return verdictStaleIncarnation // R/F-old
		}
		if position != "waiting" {
			return verdictHistory // R/F-gone
		}
		if kind == "retryable of an earlier attempt" {
			return verdictStaleAttempt // F-stale
		}
		return verdictAccept // R / F
	}

	for position, base := range positions {
		for life, incarnation := range incarnations {
			for _, kind := range kinds {
				record := base
				record.Incarnation = current
				completion := Completion{SagaID: "g", IdempotencyKey: operation, CommandID: commandID(operation, incarnation, 3)}
				switch kind {
				case "success":
					completion.Success = true
				case "retryable of the waited attempt":
					completion.Retryable = true
					if life == "same" {
						completion.CommandID = waitingFor
					}
				case "retryable of an earlier attempt":
					completion.Retryable = true
					completion.CommandID = commandID(operation, incarnation, 2)
				}
				got := judgeCompletion(record, completion, commandIDIncarnation(operation, completion.CommandID))
				if expected := want(kind, life, position); got != expected {
					t.Errorf("%s from the %s incarnation, record %s: verdict %d, want %d", kind, life, position, got, expected)
				}
			}
		}
	}
}

func TestStepTransitionAloneDecidesTheIncarnation(t *testing.T) {
	now := time.Now().UTC()
	cases := []struct {
		name  string
		cause transitionCause
		phase Phase
		want  uint32
	}{
		{"timeout keeps the life", causeTimeout, PhaseForward, 3},
		{"dispatch keeps the life", causeDispatch, PhaseForward, 3},
		{"forward manual compensate keeps the life", causeManualCompensate, PhaseForward, 3},
		{"compensating manual compensate opens a life", causeManualCompensate, PhaseCompensate, 4},
		{"resume opens a life", causeResume, PhaseForward, 4},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// 分区写记录都经 round.apply → planTransition；调用方在 after 上写的代际不起作用，提交进分区的就是返回的那个。
			u := newUnitPartition(t, nil)
			before := Record{Lifecycle: testLifecycle, ID: IDOf("gift", "inc-1"), Type: "gift", DefinitionVersion: 1, BusinessKey: "inc-1", Status: StatusPending, Phase: tc.phase, Version: 1, Incarnation: 3, Data: []byte("{}"), CreatedAt: now, UpdatedAt: now, NextRunAt: now}
			u.p.sagas[before.ID] = &sagaState{Record: before}
			after := before.Clone()
			after.Version++
			after.Incarnation = 99
			r := u.p.newRound(now)
			written := r.apply(u.p.sagas[before.ID], after, transition{cause: tc.cause}).Record
			r.emit(r.get(before.ID), "test", nil)
			u.p.install(r, 1)
			stored := u.record(before.ID)
			if written.Incarnation != tc.want || stored.Incarnation != tc.want {
				t.Fatalf("cause %d from incarnation 3 with after.Incarnation=99: returned %d, stored %d, want %d", tc.cause, written.Incarnation, stored.Incarnation, tc.want)
			}
		})
	}
}

// 协调器不接收的结果：分区的输入不能 Term（日志不能改），跳过、计 saga.partition.rejected_input_total{kind=result,reason}，
// 记录不动；saga 不在分区里（从没启动或过了终态保留期）计 unknown_saga。三种都与原协调器的终态错误一一对应（simEngine 按跳过原因还原）。
// 历史：TestCompletionConsumersTermTheSameTerminalErrors（Redis 协调器时两条结果流对这些错误都 Term）。
func TestCoordinatorSkipsTheSameTerminalResults(t *testing.T) {
	ctx := context.Background()
	engine := newSimEngine(t, nil)
	record, err := engine.StartSaga(ctx, StartRequest{Type: "rally", DefinitionVersion: 1, BusinessKey: "o-s5-1"})
	if err != nil {
		t.Fatal(err)
	}
	waiting := engine.waitCommand()
	accepted := Completion{CommandID: waiting.ID, IdempotencyKey: waiting.IdempotencyKey, SagaID: record.ID, Success: true}
	if _, err := engine.Complete(ctx, accepted); err != nil {
		t.Fatal(err)
	}
	engine.waitCommand()
	conflicting := accepted
	conflicting.Data = []byte("another result under the same command id")
	neverOpened := operationKey(record.ID, record.Lifecycle, PhaseCompensate, 0) // 补偿第 0 步从没开过
	cases := []struct {
		name       string
		completion Completion
		want       error
	}{
		{"saga does not exist", Completion{CommandID: "missing:0:0:1", IdempotencyKey: "missing:0:0", SagaID: "missing", Success: true}, ErrNotFound},
		{"step is not waiting and has no history", Completion{CommandID: commandID(neverOpened, 0, 7), IdempotencyKey: neverOpened, SagaID: record.ID, Success: true}, ErrNotWaiting},
		{"same command id, different result", conflicting, ErrIdentityConflict},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := engine.record(record.ID)
			events := engine.e.Stats().Events
			if _, err := engine.Complete(ctx, tc.completion); !errors.Is(err, tc.want) {
				t.Fatalf("result = %v, want %v", err, tc.want)
			}
			if after := engine.record(record.ID); after.Version != before.Version || engine.e.Stats().Events != events {
				t.Fatalf("a skipped result moved the record: %+v → %+v", before, after)
			}
		})
	}
}

// 记录的定义版本在所有者上没有注册时（所有者用旧版本重启），这个版本的结果不接收（分区跳过，reason=definition_missing），
// 记录不动；定义回来之后，步骤超时派发的下一次尝试回放参与方的回执，结果被接收、记录前进——不会因为“定义缺失时到过一次”
// 而永远丢掉。
// 历史：TestACompletionBeforeItsDefinitionIsRegisteredIsRetriedNotTerminated（Redis 协调器时结果被 nak、重投后接收；
// 分区的输入不能 nak，等价的恢复是下一次尝试回放回执）。
func TestACompletionBeforeItsDefinitionIsRegisteredIsRetriedNotTerminated(t *testing.T) {
	ctx := context.Background()
	w := newNativeWorld(t, 5, time.Now().UTC())
	w.tick(0)
	k := w.pendingCommand()
	resultK := w.commitAttempt(k)
	gift := w.sim.e.definitions[definitionKey{typeName: "gift", version: 1}]
	w.sim.e.mu.Lock()
	delete(w.sim.e.definitions, definitionKey{typeName: "gift", version: 1}) // 所有者用还没有这个定义的旧版本重启
	w.sim.e.mu.Unlock()
	err := w.complete(resultK)
	if !errors.Is(err, ErrDefinitionMissing) {
		t.Fatalf("completion before the definition is registered = %v, want ErrDefinitionMissing (skipped, not accepted)", err)
	}
	if current := w.record(); current.Status != StatusWaiting || current.OperationKey != k.IdempotencyKey {
		t.Fatalf("the refused completion moved the record: %+v", current)
	}
	// 新定义上线：k 超时，派发的下一次尝试回放 k 的回执，被接收。
	if err := w.sim.e.Register(gift); err != nil {
		t.Fatal(err)
	}
	w.tick(5 * time.Second)
	w.tick(6*time.Second + 100*time.Millisecond)
	w.deliverCommand(w.pendingCommand())
	if current := w.record(); current.Step != 1 || current.CompletedSteps != 1 {
		t.Fatalf("after the definition came back the record is %+v, want step 0 completed by the replayed receipt", current)
	}
	w.assertEffective(k.IdempotencyKey, 1)
	_ = ctx
}

// 定义一直不来时：记录的步骤超时后，没有定义的所有者 fence 到 ManualRequired（放弃关闭这次操作），之后送达的成功不再是“定义缺失”，
// 按迟到成功收尾（告警一次）。
func TestADefinitionThatNeverArrivesEndsInTheCoordinatorFence(t *testing.T) {
	w := newNativeWorld(t, 5, time.Now().UTC())
	w.tick(0)
	k := w.pendingCommand()
	resultK := w.commitAttempt(k)
	w.sim.e.mu.Lock()
	w.sim.e.definitions = map[definitionKey]Definition{}
	w.sim.e.mu.Unlock()
	if err := w.complete(resultK); !errors.Is(err, ErrDefinitionMissing) {
		t.Fatalf("first delivery = %v, want ErrDefinitionMissing (skipped)", err)
	}
	w.tick(5*time.Second + time.Millisecond)
	if fenced := w.record(); fenced.Status != StatusManualRequired {
		t.Fatalf("precondition: the coordinator without the definition did not fence the record: %+v", fenced)
	}
	before := w.sim.e.Stats().LateAfterAbandon
	if err := w.complete(resultK); err != nil {
		t.Fatalf("delivery after the fence = %v, want settled", err)
	}
	if late := w.sim.e.Stats().LateAfterAbandon - before; late != 1 {
		t.Fatalf("LateAfterAbandon grew by %d, want the success after the fence reported once", late)
	}
}

// 协调器因定义缺失把记录 fence 到 ManualRequired 时，若当前步骤正在重试退避（已派发过、还没有结果，Attempt > 0、
// OperationKey 已清空），这个操作同样被放弃：tombstone 记“放弃关闭”，之后到达的成功告警。否则较早尝试晚到的成功被以
// ErrNotWaiting 静默丢弃，修好定义后 Resume 补偿时这一步不在 CompletedSteps 里。历史：RR-20261005-NC-250。
func TestDefinitionFenceDuringBackoffAbandonsTheOperation(t *testing.T) {
	for _, phase := range []Phase{PhaseForward, PhaseCompensate} {
		t.Run(phase.String(), func(t *testing.T) {
			ctx := context.Background()
			engine := newSimEngine(t, nil)
			now := engine.clock.Now()
			id := IDOf("rally", "nc250-"+phase.String())
			record := Record{Lifecycle: testLifecycle, ID: id, Type: "rally", DefinitionVersion: 1, BusinessKey: "nc250-" + phase.String(),
				Status: StatusPending, Phase: PhaseForward, Step: 1, CompletedSteps: 1, Version: 1, NextRunAt: now, CreatedAt: now, UpdatedAt: now}
			if phase == PhaseCompensate {
				record.Status, record.Phase, record.Step = StatusCompensating, PhaseCompensate, 0
			}
			engine.seed(record)
			engine.advance(0) // 派发第一次尝试
			waiting := engine.record(id)
			first := Completion{CommandID: waiting.CommandID, IdempotencyKey: waiting.OperationKey, SagaID: id, Success: true}
			engine.advance(time.Second + time.Millisecond) // 第一次尝试超时：还有重试额度，进入退避
			backoff := engine.record(id)
			if backoff.Status == StatusWaiting || backoff.Attempt != 1 || backoff.OperationKey != "" {
				t.Fatalf("precondition: want the step in retry backoff, got %+v", backoff)
			}
			// 退避到期，所有者没有这个定义版本：fence 到 ManualRequired。
			engine.e.mu.Lock()
			definitions := engine.e.definitions
			engine.e.definitions = map[definitionKey]Definition{}
			engine.e.mu.Unlock()
			engine.advance(backoffPassed)
			if fenced := engine.record(id); fenced.Status != StatusManualRequired {
				t.Fatalf("precondition: missing definition did not fence the record: %+v", fenced)
			}
			if tomb := engine.p.sagas[id].Ops[waiting.OperationKey]; tomb.Closure != OperationAbandoned {
				t.Errorf("the definition fence did not close the operation it abandoned: tombstone %+v", tomb)
			}
			engine.e.mu.Lock()
			engine.e.definitions = definitions
			engine.e.mu.Unlock()
			// 第一次尝试其实生效了，它的成功现在才到。
			alarmsBefore := counterValue("saga.completion.late_after_abandon_total")
			_, completeErr := engine.Complete(ctx, first)
			if completeErr != nil {
				t.Errorf("late success after the definition fence = %v, want nil (acknowledged and alarmed): ErrNotWaiting means the coordinator has no record of the abandoned operation and drops the effect silently", completeErr)
			}
			if engine.e.Stats().LateAfterAbandon != 1 || counterValue("saga.completion.late_after_abandon_total")-alarmsBefore != 1 {
				t.Errorf("a step that took effect after the definition fence abandoned it was not alarmed: LateAfterAbandon=%d, counter grew by %d, want 1 and 1",
					engine.e.Stats().LateAfterAbandon, counterValue("saga.completion.late_after_abandon_total")-alarmsBefore)
			}
			if current := engine.record(id); current.Status != StatusManualRequired {
				t.Errorf("the late success changed the fenced saga's status: %+v", current)
			}
		})
	}
}

// 重试退避期间送达的成功不能丢：同一生里一次尝试已经生效，它的成功不论何时送达，这一步要么计入 CompletedSteps
// （之后放弃 saga 时随前缀补偿），要么作为“放弃后迟到的成功”被补偿 / 告警。退避中（Pending 且 Attempt≥1）送达的成功
// 被接收；之后截止、人工 Compensate 或定义缺失关闭这个操作时，这一步仍被计入。历史：RR-20261006-42。
func TestNativeStepSuccessDeliveredDuringBackoffIsNotLostWhenTheOperationIsClosed(t *testing.T) {
	notify := Step{Name: "notify", ForwardTopic: "gift.notify", Timeout: 5 * time.Second, MaxAttempts: 5, BackoffMin: 100 * time.Millisecond, BackoffMax: time.Second}
	const afterNextStepTimeout = 11*time.Second + 100*time.Millisecond // 第 2 步在 6s 派发，5s 超时
	for _, closure := range []struct {
		name  string
		close func(w *nativeWorld)
	}{
		// 分区在接收 k 的那一轮里就派发了第 2 步，所以截止 / 人工补偿 / 定义缺失都在第 2 步的尝试超时之后才关闭操作。
		{"saga deadline", func(w *nativeWorld) { w.tick(afterNextStepTimeout) }},
		{"manual compensate", func(w *nativeWorld) {
			w.tick(afterNextStepTimeout) // 第 2 步的尝试超时 → 退避，此时没有结果在途
			if _, err := w.compensate(w.ctx, w.sagaID, "operator", w.at(afterNextStepTimeout+10*time.Millisecond)); err != nil {
				w.t.Fatal(err)
			}
		}},
		{"definition missing", func(w *nativeWorld) {
			// 所有者用还没有这个定义版本的旧版本重启：到期时 fence 到 ManualRequired。
			w.sim.e.mu.Lock()
			w.sim.e.definitions = map[definitionKey]Definition{}
			w.sim.e.mu.Unlock()
			w.tick(afterNextStepTimeout)
		}},
	} {
		t.Run(closure.name, func(t *testing.T) {
			deadline := time.Duration(0)
			if closure.name == "saga deadline" {
				deadline = 6*time.Second + 20*time.Millisecond // 晚于 k 的超时（6s），早于第 2 步的尝试超时
			}
			w := newNativeWorldWithSteps(t, 15, time.Now().UTC(), deadline, notify)
			w.tick(0)
			first := w.pendingCommand()
			w.deliver(w.commitAttempt(first))
			w.tick(time.Second) // 派发第 1 步的尝试 k，命令截止 t0+6s
			k := w.pendingCommand()
			resultK := w.commitAttempt(k) // k 在截止前生效，结果还在路上
			w.tick(6 * time.Second)       // k 超时 → 重试退避
			if record := w.record(); record.Status != StatusPending || record.Attempt != 1 || record.Step != 1 {
				t.Fatalf("precondition: want step 1 in retry backoff, got status=%s step=%d attempt=%d", record.Status, record.Step, record.Attempt)
			}
			lateBefore := counterValue("saga.completion.late_after_abandon_total")
			w.deliver(resultK) // k 的成功在退避期间送达
			closure.close(w)
			w.assertEffective(k.IdempotencyKey, 1)
			record := w.record()
			late := counterValue("saga.completion.late_after_abandon_total") - lateBefore
			if record.CompletedSteps != 2 && record.LateStep != 2 {
				t.Fatalf("step 1 (%s) took effect and its success arrived during retry backoff; after the %s closed the operation the saga is %s phase=%s step=%d completed=%d late_step=%d late_alarm_delta=%d: the step is neither counted nor compensated nor alarmed",
					k.IdempotencyKey, closure.name, record.Status, record.Phase, record.Step, record.CompletedSteps, record.LateStep, late)
			}
			switch closure.name {
			case "definition missing":
				if record.Status != StatusManualRequired {
					t.Fatalf("definition fence: status=%s, want ManualRequired with step 1 counted", record.Status)
				}
			default:
				// 补偿从第 1 步开始：已生效的那一步在补偿范围里（第 2 步被放弃，没有生效）。
				if !compensatingStep(record, 1) {
					t.Fatalf("%s: status=%s phase=%s step=%d, want compensating step 1 first", closure.name, record.Status, record.Phase, record.Step)
				}
			}
			if late != 0 {
				t.Errorf("late_after_abandon_total grew by %d, want 0: the success arrived before the operation was abandoned", late)
			}
		})
	}
}

// 协调器上：退避中接收的成功以成功回执“带结果关闭”这个操作，之后的重投按回执去重。
// 历史：TestRedisStoreSuccessDuringBackoffClosesTheOperationWithItsResult。
func TestCoordinatorSuccessDuringBackoffClosesTheOperationWithItsResult(t *testing.T) {
	ctx := context.Background()
	engine := newSimEngine(t, nil)
	record, err := engine.StartSaga(ctx, StartRequest{Type: "rally", DefinitionVersion: 1, BusinessKey: "backoff-success"})
	if err != nil {
		t.Fatal(err)
	}
	waiting := engine.waitCommand()
	success := Completion{CommandID: waiting.ID, IdempotencyKey: waiting.IdempotencyKey, SagaID: record.ID, Success: true}
	engine.advance(time.Second + time.Millisecond) // 尝试 1 超时 → 退避
	if backoff := engine.record(record.ID); backoff.Status != StatusPending || backoff.Attempt != 1 {
		t.Fatalf("precondition: want retry backoff, got %+v", backoff)
	}
	if _, err := engine.Complete(ctx, success); err != nil {
		t.Fatalf("success of attempt 1 during backoff = %v, want accepted", err)
	}
	if after := engine.record(record.ID); after.CompletedSteps != 1 || after.Step != 1 {
		t.Fatalf("success during backoff not counted: status=%s step=%d completed=%d", after.Status, after.Step, after.CompletedSteps)
	}
	st := engine.p.sagas[record.ID]
	if _, ok := st.Receipts[success.CommandID]; !ok || st.Ops[success.IdempotencyKey].Closure != OperationClosedWithResult {
		t.Fatalf("after accepting the success: receipts %v ops %+v, want its receipt and a result closure", st.Receipts, st.Ops)
	}
	duplicates := engine.e.Stats().Duplicates
	if _, err := engine.Complete(ctx, success); err != nil || engine.e.Stats().Duplicates != duplicates+1 {
		t.Fatalf("redelivery of the accepted success = %v duplicates=%d, want a duplicate", err, engine.e.Stats().Duplicates-duplicates)
	}
}
