package saga

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/nestwal"
	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
	"github.com/tjbdwanghaibo/roost-core/infra/storage/mongo/mongotest"
)

// 完成判定与代际变更按实际结果验证，不约束内部函数必须如何组织。
func TestJudgeCompletionIsTheUnifiedRule(t *testing.T) {
	const current uint32 = 2
	operation := operationKey("g", PhaseForward, 1)
	other := operationKey("g", PhaseForward, 0)
	waitingFor := commandID(operation, current, 3)
	positions := map[string]Record{
		"waiting":     {ID: "g", Phase: PhaseForward, Step: 1, Status: StatusWaiting, OperationKey: operation, CommandID: waitingFor, Attempt: 3},
		"backoff":     {ID: "g", Phase: PhaseForward, Step: 1, Status: StatusPending, Attempt: 3},
		"to dispatch": {ID: "g", Phase: PhaseForward, Step: 1, Status: StatusPending},
		"elsewhere":   {ID: "g", Phase: PhaseForward, Step: 0, Status: StatusWaiting, OperationKey: other, CommandID: commandID(other, current, 1), Attempt: 1},
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
	ctx := context.Background()
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
			store := newMemoryStore()
			before := Record{ID: "inc-1", Type: "gift", DefinitionVersion: 1, Status: StatusPending, Phase: tc.phase, Version: 1, Incarnation: 3, Data: []byte("{}"), CreatedAt: now, UpdatedAt: now, NextRunAt: now}
			if err := store.Create(ctx, before); err != nil {
				t.Fatal(err)
			}
			after := before.Clone()
			after.Version++
			after.Incarnation = 99
			written, _, err := (&Engine{store: store}).stepTransition(ctx, before, after, transition{cause: tc.cause})
			if err != nil {
				t.Fatal(err)
			}
			stored, err := store.Get(ctx, before.ID)
			if err != nil {
				t.Fatal(err)
			}
			if written.Incarnation != tc.want || stored.Incarnation != tc.want {
				t.Fatalf("cause %d from incarnation 3 with after.Incarnation=99: returned %d, stored %d, want %d", tc.cause, written.Incarnation, stored.Incarnation, tc.want)
			}
		})
	}
}

func TestCompletionConsumersTermTheSameTerminalErrors(t *testing.T) {
	ctx := context.Background()
	mongoClient := mongotest.NewClient()
	store, err := NewMongoStore(mongoClient, MongoStoreOptions{Database: "o_s5_1"})
	if err != nil {
		t.Fatal(err)
	}
	engine, err := NewEngine(store, PublishFunc(func(context.Context, Command) error { return nil }), DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Register(testDefinition()); err != nil {
		t.Fatal(err)
	}
	record, err := engine.StartSaga(ctx, StartRequest{Type: "rally", DefinitionVersion: 1, BusinessKey: "o-s5-1"})
	if err != nil {
		t.Fatal(err)
	}
	operation := operationKey(record.ID, PhaseForward, 0)
	conflicting := Completion{CommandID: commandID(operation, 0, 1), IdempotencyKey: operation, SagaID: record.ID, Success: true, CompletedAt: time.Now()}
	other := conflicting
	other.Data = []byte("another result under the same command id")
	digest, err := completionDigest(other)
	if err != nil {
		t.Fatal(err)
	}
	if err := mongoClient.Collection("o_s5_1", defaultCompletionCollection).Seed(completionDoc{ID: conflicting.CommandID, Digest: digest, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name       string
		completion Completion
		want       error
	}{
		{"saga does not exist", Completion{CommandID: "missing:0:0:1", IdempotencyKey: "missing:0:0", SagaID: "missing", Success: true}, ErrNotFound},
		{"step is not waiting and has no history", Completion{CommandID: commandID(operation, 0, 7), IdempotencyKey: operation, SagaID: record.ID, Success: true}, ErrNotWaiting},
		{"same command id, different result", conflicting, ErrIdentityConflict},
	}
	plain := &startJetStream{}
	if _, err := SubscribeCompletions(ctx, plain, CompletionConsumerConfig{Stream: "SAGA", Durable: "results", SubjectPrefix: "roost.saga"}, engine); err != nil {
		t.Fatal(err)
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(completionEnvelope{Version: WireVersion, Completion: tc.completion})
			if err != nil {
				t.Fatal(err)
			}
			err = plain.handler(ctx, &fnats.JetStreamMsg{Data: raw})
			if !errors.Is(err, tc.want) || !fnats.IsPermanent(err) {
				t.Fatalf("plain result stream = %v (permanent=%v), want %v terminated: a redelivery cannot change it, and naking it holds a MaxAckPending slot until MaxDeliver", err, fnats.IsPermanent(err), tc.want)
			}
			effect, err := NewCompletionEffect(tc.completion)
			if err != nil {
				t.Fatal(err)
			}
			raw, err = json.Marshal(nestwal.EffectEnvelope{TransactionID: "tx-1", EffectID: effect.ID, Topic: effect.Topic, Key: effect.Key, Payload: effect.Payload})
			if err != nil {
				t.Fatal(err)
			}
			if err := handleNestCompletion(ctx, &fnats.JetStreamMsg{Data: raw}, engine); !errors.Is(err, tc.want) || !fnats.IsPermanent(err) {
				t.Fatalf("native result stream = %v (permanent=%v), want %v terminated like the plain stream", err, fnats.IsPermanent(err), tc.want)
			}
		})
	}
}

func TestACompletionBeforeItsDefinitionIsRegisteredIsRetriedNotTerminated(t *testing.T) {
	type deliver func(t *testing.T, completer Completer, completion Completion) error
	streams := map[string]deliver{
		"plain result stream": func(t *testing.T, completer Completer, completion Completion) error {
			t.Helper()
			engine := completer.(*Engine)
			plain := &startJetStream{}
			if _, err := SubscribeCompletions(context.Background(), plain, CompletionConsumerConfig{Stream: "SAGA", Durable: "results", SubjectPrefix: "roost.saga"}, engine); err != nil {
				t.Fatal(err)
			}
			raw, err := json.Marshal(completionEnvelope{Version: WireVersion, Completion: completion})
			if err != nil {
				t.Fatal(err)
			}
			return plain.handler(context.Background(), &fnats.JetStreamMsg{Data: raw})
		},
		"native result stream": func(t *testing.T, completer Completer, completion Completion) error {
			t.Helper()
			effect, err := NewCompletionEffect(completion)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := json.Marshal(nestwal.EffectEnvelope{TransactionID: "tx-1", EffectID: effect.ID, Topic: effect.Topic, Key: effect.Key, Payload: effect.Payload})
			if err != nil {
				t.Fatal(err)
			}
			return handleNestCompletion(context.Background(), &fnats.JetStreamMsg{Data: raw}, completer)
		},
	}
	for name, deliver := range streams {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			store, _, rollingIn, waiting := waitingOnANewDefinition(t)
			completion := Completion{CommandID: waiting.CommandID, IdempotencyKey: waiting.OperationKey, SagaID: waiting.ID, Success: true}

			// 结果先到了还没有这个定义版本的协调器。
			err := deliver(t, rollingIn, completion)
			if !errors.Is(err, ErrDefinitionMissing) || fnats.IsPermanent(err) {
				t.Fatalf("completion before the definition is registered = %v (permanent=%v); want ErrDefinitionMissing nak'd with backoff — the definition arrives with the new process, a terminated result is gone", err, fnats.IsPermanent(err))
			}
			if current, _ := store.Get(ctx, waiting.ID); current.Status != StatusWaiting || current.OperationKey != waiting.OperationKey {
				t.Fatalf("the refused completion moved the record: %+v", current)
			}

			// 新定义上线，同一条消息重投：被接收，记录推进到下一步。
			if err := rollingIn.Register(testDefinition()); err != nil {
				t.Fatal(err)
			}
			if err := deliver(t, rollingIn, completion); err != nil {
				t.Fatalf("redelivery after the definition is registered = %v, want accepted", err)
			}
			if current, _ := store.Get(ctx, waiting.ID); current.Status == StatusWaiting || current.Step != 1 || current.CompletedSteps != 1 {
				t.Fatalf("after the redelivery the record is %+v, want step 0 completed", current)
			}
		})
	}
}

// 定义一直不来时 nak 不会无限占着：记录的步骤超时后，没有定义的协调器 fence 到 ManualRequired（放弃关闭这次操作），
// 之后重投的成功不再是“定义缺失”，按迟到成功收尾，不再 nak。
func TestADefinitionThatNeverArrivesEndsInTheCoordinatorFence(t *testing.T) {
	ctx := context.Background()
	store, _, rollingIn, waiting := waitingOnANewDefinition(t)
	completion := Completion{CommandID: waiting.CommandID, IdempotencyKey: waiting.OperationKey, SagaID: waiting.ID, Success: true}
	effect, err := NewCompletionEffect(completion)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(nestwal.EffectEnvelope{TransactionID: "tx-1", EffectID: effect.ID, Topic: effect.Topic, Key: effect.Key, Payload: effect.Payload})
	if err != nil {
		t.Fatal(err)
	}
	message := &fnats.JetStreamMsg{Data: raw}
	if err := handleNestCompletion(ctx, message, rollingIn); !errors.Is(err, ErrDefinitionMissing) || fnats.IsPermanent(err) {
		t.Fatalf("first delivery = %v (permanent=%v), want a retryable ErrDefinitionMissing", err, fnats.IsPermanent(err))
	}
	if err := rollingIn.processClaimed(ctx, waiting, waiting.NextRunAt.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if fenced, _ := store.Get(ctx, waiting.ID); fenced.Status != StatusManualRequired {
		t.Fatalf("precondition: the coordinator without the definition did not fence the record: %+v", fenced)
	}
	err = handleNestCompletion(ctx, message, rollingIn)
	if err != nil && !fnats.IsPermanent(err) {
		t.Fatalf("redelivery after the fence = %v, want it settled (ack or Term), not nak'd again", err)
	}
	if late := rollingIn.Stats().LateAfterAbandon; late != 1 {
		t.Fatalf("LateAfterAbandon = %d, want the success after the fence reported once", late)
	}
}

// waitingOnANewDefinition 建一条在等第 0 步结果的记录：upgraded 是有这个定义版本的新协调器（派发了它），
// rollingIn 是同一存储上还没有注册这个定义的旧协调器。
func waitingOnANewDefinition(t *testing.T) (*MongoStore, *Engine, *Engine, Record) {
	t.Helper()
	ctx := context.Background()
	store, err := NewMongoStore(mongotest.NewClient(), MongoStoreOptions{Database: "rollout"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.EnsureInfrastructure(ctx); err != nil {
		t.Fatal(err)
	}
	publish := PublishFunc(func(context.Context, Command) error { return nil })
	upgraded, err := NewEngine(store, publish, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	if err := upgraded.Register(testDefinition()); err != nil {
		t.Fatal(err)
	}
	rollingIn, err := NewEngine(store, publish, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	record, err := upgraded.StartSaga(ctx, StartRequest{Type: "rally", DefinitionVersion: 1, BusinessKey: "rollout"})
	if err != nil {
		t.Fatal(err)
	}
	if err := upgraded.processClaimed(ctx, record, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	waiting, err := store.Get(ctx, record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if waiting.Status != StatusWaiting || waiting.Step != 0 {
		t.Fatalf("precondition: want step 0 dispatched and waiting, got %+v", waiting)
	}
	return store, upgraded, rollingIn, waiting
}

// 协调器因定义缺失把记录 fence 到 ManualRequired 时，若当前步骤正在重试退避（已派发过、还没有结果，Attempt > 0、
// OperationKey 已清空），这个操作同样被放弃：tombstone 记“放弃关闭”、删掉仍排队的命令，之后到达的成功告警。
// 否则较早尝试晚到的成功被以 ErrNotWaiting 静默丢弃，修好定义后 Resume 补偿时这一步不在 CompletedSteps 里。
// 历史：RR-20261005-NC-250。
func TestDefinitionFenceDuringBackoffAbandonsTheOperation(t *testing.T) {
	for _, phase := range []Phase{PhaseForward, PhaseCompensate} {
		t.Run(phase.String(), func(t *testing.T) {
			ctx := context.Background()
			store, err := NewMongoStore(mongotest.NewClient(), MongoStoreOptions{Database: "nc250"})
			if err != nil {
				t.Fatal(err)
			}
			if err := store.EnsureInfrastructure(ctx); err != nil {
				t.Fatal(err)
			}
			publish := PublishFunc(func(context.Context, Command) error { return nil })
			// withDefinition 是正常协调器；withoutDefinition 是滚动升级中还没有这个定义版本的协调器。
			withDefinition, err := NewEngine(store, publish, DefaultOptions())
			if err != nil {
				t.Fatal(err)
			}
			if err := withDefinition.Register(testDefinition()); err != nil {
				t.Fatal(err)
			}
			withoutDefinition, err := NewEngine(store, publish, DefaultOptions())
			if err != nil {
				t.Fatal(err)
			}

			now := time.Now().UTC().Truncate(time.Millisecond)
			record := Record{ID: "nc250-" + phase.String(), Type: "rally", DefinitionVersion: 1, BusinessKey: "nc250-" + phase.String(),
				Status: StatusPending, Phase: PhaseForward, Step: 1, CompletedSteps: 1, Version: 1, NextRunAt: now, CreatedAt: now, UpdatedAt: now}
			if phase == PhaseCompensate {
				record.Status, record.Phase, record.Step = StatusCompensating, PhaseCompensate, 0
			}
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

			// 派发第一次尝试；它的命令留在 outbox（发布一直失败，或者已经发出去、步骤还在执行）。
			if err := withDefinition.processClaimed(ctx, get(), now); err != nil {
				t.Fatal(err)
			}
			waiting := get()
			first := Completion{CommandID: waiting.CommandID, IdempotencyKey: waiting.OperationKey, SagaID: record.ID, Success: true}
			// 第一次尝试超时：还有重试额度，进入退避（Attempt=1，OperationKey 清空）。
			if err := withDefinition.processClaimed(ctx, waiting, now.Add(2*time.Second)); err != nil {
				t.Fatal(err)
			}
			backoff := get()
			if backoff.Status == StatusWaiting || backoff.Attempt != 1 || backoff.OperationKey != "" {
				t.Fatalf("precondition: want the step in retry backoff, got %+v", backoff)
			}
			// 退避到期，被没有定义的协调器领到：fence 到 ManualRequired。
			if err := withoutDefinition.processClaimed(ctx, backoff, now.Add(3*time.Second)); err != nil {
				t.Fatal(err)
			}
			if fenced := get(); fenced.Status != StatusManualRequired {
				t.Fatalf("precondition: missing definition did not fence the record: %+v", fenced)
			}

			// 关闭操作会删掉它仍排队的命令；没有关闭时，被放弃的尝试仍会被发布出去。
			queued, err := store.ClaimOutbox(ctx, ClaimRequest{Owner: "nc250", Now: now.Add(time.Hour), LeaseDuration: time.Minute, Limit: 8})
			if err != nil {
				t.Fatal(err)
			}
			if len(queued) != 0 {
				t.Errorf("the fenced operation still has %d queued command(s) (first %s): the definition fence did not close the operation it abandoned", len(queued), queued[0].Command.ID)
			}

			// 第一次尝试其实生效了，它的成功现在才到。
			alarmsBefore := counterValue("saga.completion.late_after_abandon_total")
			_, completeErr := withDefinition.Complete(ctx, first)
			if completeErr != nil {
				t.Errorf("late success after the definition fence = %v, want nil (acknowledged and alarmed): ErrNotWaiting means the coordinator has no record of the abandoned operation and drops the effect silently", completeErr)
			}
			if errors.Is(completeErr, ErrNotWaiting) || withDefinition.Stats().LateAfterAbandon != 1 || counterValue("saga.completion.late_after_abandon_total")-alarmsBefore != 1 {
				t.Errorf("a step that took effect after the definition fence abandoned it was not alarmed: LateAfterAbandon=%d, counter grew by %d, want 1 and 1",
					withDefinition.Stats().LateAfterAbandon, counterValue("saga.completion.late_after_abandon_total")-alarmsBefore)
			}
			if current := get(); current.Status != StatusManualRequired || current.CompletedSteps != record.CompletedSteps {
				t.Errorf("the late success changed the fenced saga: %+v", current)
			}
		})
	}
}

// 重试退避期间送达的成功不能丢：同一生里一次尝试已经生效，它的成功不论何时送达，这一步要么计入 CompletedSteps
// （之后放弃 saga 时随前缀补偿），要么作为“放弃后迟到的成功”被补偿 / 告警。退避中（Pending 且 Attempt≥1）送达的成功
// 被接收；之后截止、人工 Compensate 或定义缺失关闭这个操作时，这一步仍被计入。历史：RR-20261006-42。
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

// MongoStore 上：退避中接收的成功以成功回执“带结果关闭”这个操作，之后的重投按回执去重。
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
