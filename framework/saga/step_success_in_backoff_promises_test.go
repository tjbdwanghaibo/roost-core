package saga

import (
	"context"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/infra/storage/mongo/mongotest"
)

// RR-20261006-42（F06-S1）：重试退避期间送达的成功不能丢。
//
// 承诺：同一生里一次尝试已经生效，它的成功不论何时送达，这一步要么计入 CompletedSteps（之后放弃 saga 时随前缀补偿），
// 要么作为“放弃后迟到的成功”被补偿 / 告警（saga 方向 ④）。
//
// 旧行为：尝试 k 在命令截止前生效，结果在 k 超时之后、下一次派发之前送达，此时记录是 Pending 且 Attempt≥1。
// Complete 同一生只在 Waiting 时接收，completeNotWaiting 查不到回执也查不到 tombstone（操作还开着）→ ErrNotWaiting，
// 两条结果流都 Term。随后截止、人工 Compensate 或定义缺失关闭这个操作，再也不会派发它的尝试，没有任何投递会经收件箱
// 回放这份成功：这一步已生效，却不在 CompletedSteps 里，不补偿、不告警（LateStep=0，late_after_abandon 不增加）。
func TestNativeStepSuccessDeliveredDuringBackoffIsNotLostWhenTheOperationIsClosed(t *testing.T) {
	notify := Step{Name: "notify", ForwardTopic: "gift.notify", Timeout: 5 * time.Second, MaxAttempts: 5, BackoffMin: 100 * time.Millisecond, BackoffMax: time.Second}
	for _, closure := range []struct {
		name  string
		close func(w *nativeWorld)
	}{
		{"saga deadline", func(w *nativeWorld) { w.tick(7 * time.Second) }},
		{"manual compensate", func(w *nativeWorld) {
			if _, err := w.engine.Compensate(w.ctx, w.sagaID, "operator", w.at(6*time.Second+10*time.Millisecond)); err != nil {
				w.t.Fatal(err)
			}
		}},
		{"definition missing", func(w *nativeWorld) {
			// 滚动发布中还没有这个定义版本的协调器领到记录：fence 到 ManualRequired。
			missing, err := NewEngine(w.store, PublishFunc(func(context.Context, Command) error { return nil }), w.engine.opts)
			if err != nil {
				w.t.Fatal(err)
			}
			w.clock = w.at(7 * time.Second)
			records, err := w.store.ClaimDue(w.ctx, ClaimRequest{Owner: "rolling", Now: w.clock, LeaseDuration: time.Second, Limit: 10})
			if err != nil {
				w.t.Fatal(err)
			}
			for _, record := range records {
				if err := missing.processClaimed(w.ctx, record, w.clock); err != nil {
					w.t.Fatal(err)
				}
			}
		}},
	} {
		t.Run(closure.name, func(t *testing.T) {
			deadline := time.Duration(0)
			if closure.name == "saga deadline" {
				deadline = 6*time.Second + 20*time.Millisecond // 晚于 k 的超时（6s），早于退避到期后的下一次派发
			}
			w := newNativeWorldWithSteps(t, 15, time.Now().UTC(), deadline, notify)
			w.tick(0)
			first := w.pendingCommand()
			recordFirst := w.commitAttempt(w.inboxA, first)
			if !w.project(recordFirst) {
				t.Fatal("step 0 projected within its deadline was skipped")
			}
			w.deliverEffect(recordFirst)
			w.tick(time.Second) // 派发第 1 步的尝试 k，命令截止 t0+6s
			k := w.pendingCommand()
			recordK := w.commitAttempt(w.inboxA, k)
			if !w.project(recordK) { // k 在截止前生效，completion effect 还在路上
				t.Fatal("attempt k projected within its deadline was skipped")
			}
			w.tick(6 * time.Second) // k 超时 → 重试退避
			if record := w.record(); record.Status != StatusPending || record.Attempt != 1 || record.Step != 1 {
				t.Fatalf("precondition: want step 1 in retry backoff, got status=%s step=%d attempt=%d", record.Status, record.Step, record.Attempt)
			}
			lateBefore := counterValue("saga.completion.late_after_abandon_total")
			w.deliverEffect(recordK) // k 的成功在退避期间送达
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
				// 补偿从第 1 步开始：已生效的那一步在补偿范围里。
				if record.Status != StatusCompensating || record.Phase != PhaseCompensate || record.Step != 1 {
					t.Fatalf("%s: status=%s phase=%s step=%d, want compensating step 1 first", closure.name, record.Status, record.Phase, record.Step)
				}
			}
			if late != 0 {
				t.Errorf("late_after_abandon_total grew by %d, want 0: the success arrived before the operation was abandoned", late)
			}
		})
	}
}

// 同一承诺在正式 MongoStore 上：退避中接收成功要以成功回执“带结果关闭”这个操作，之后的重投按回执去重。
func TestMongoStoreSuccessDuringBackoffClosesTheOperationWithItsResult(t *testing.T) {
	ctx := context.Background()
	store, err := NewMongoStore(mongotest.NewClient(), MongoStoreOptions{Database: "backoff_success"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.EnsureInfrastructure(ctx); err != nil {
		t.Fatal(err)
	}
	engine, err := NewEngine(store, PublishFunc(func(context.Context, Command) error { return nil }), DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Register(testDefinition()); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	record := Record{ID: "backoff-success", Type: "rally", DefinitionVersion: 1, BusinessKey: "backoff-success",
		Status: StatusPending, Phase: PhaseForward, Version: 1, NextRunAt: now, CreatedAt: now, UpdatedAt: now}
	if err := store.Create(ctx, record); err != nil {
		t.Fatal(err)
	}
	get := func() Record {
		t.Helper()
		current, err := store.Get(ctx, record.ID)
		if err != nil {
			t.Fatal(err)
		}
		return current
	}
	if err := engine.processClaimed(ctx, get(), now); err != nil { // 派发尝试 1
		t.Fatal(err)
	}
	waiting := get()
	success := Completion{CommandID: waiting.CommandID, IdempotencyKey: waiting.OperationKey, SagaID: record.ID, Success: true}
	if err := engine.processClaimed(ctx, waiting, waiting.NextRunAt); err != nil { // 尝试 1 超时 → 退避
		t.Fatal(err)
	}
	if backoff := get(); backoff.Status != StatusPending || backoff.Attempt != 1 {
		t.Fatalf("precondition: want retry backoff, got %+v", backoff)
	}
	if _, err := engine.Complete(ctx, success); err != nil {
		t.Fatalf("success of attempt 1 during backoff = %v, want accepted", err)
	}
	if after := get(); after.CompletedSteps != 1 || after.Step != 1 {
		t.Fatalf("success during backoff not counted: status=%s step=%d completed=%d", after.Status, after.Step, after.CompletedSteps)
	}
	history, err := store.CompletionHistory(ctx, success)
	if err != nil || !history.Receipt {
		t.Fatalf("history after accepting the success = %+v err=%v, want its receipt recorded", history, err)
	}
	duplicates := engine.Stats().Duplicates
	if _, err := engine.Complete(ctx, success); err != nil || engine.Stats().Duplicates != duplicates+1 {
		t.Fatalf("redelivery of the accepted success = %v duplicates=%d, want a duplicate", err, engine.Stats().Duplicates-duplicates)
	}
}
