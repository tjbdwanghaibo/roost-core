package saga

import (
	"context"
	"errors"
	"testing"
	"time"
)

// 协调器外观的承诺：正向推进、倒序补偿、启动身份（业务键 + 启动摘要）、定义版本、缺定义 fence、结果去重、可重试失败的新尝试、
// 协调器时钟、截止后的成功进入补偿、Resume 的截止策略与新一生的 CommandID。驱动是 simEngine（单元分区，见
// jetstream_coordinator_test.go）：每个操作是一轮判定 + 提交，到期由用例推进时钟触发。
// 历史：Redis 协调器（内存 Store + 后台循环）上的同名用例，第 4 批换驱动、断言不变（设计 §11.1）。

func testDefinition() Definition {
	return Definition{Type: "rally", Version: 1, Steps: []Step{{Name: "reserve", ForwardTopic: "reserve", CompensateTopic: "reserve.cancel", Timeout: time.Second, MaxAttempts: 2, BackoffMin: time.Millisecond, BackoffMax: 10 * time.Millisecond}, {Name: "march", ForwardTopic: "march", CompensateTopic: "march.cancel", Timeout: time.Second, MaxAttempts: 2, BackoffMin: time.Millisecond, BackoffMax: 10 * time.Millisecond}}}
}

// backoffPassed 长于 testDefinition 的最大退避（10ms），推进它之后重试的下一次尝试一定已到期。
const backoffPassed = 20 * time.Millisecond

func TestEngineCompletesForwardSteps(t *testing.T) {
	engine := newSimEngine(t, nil)
	record, err := engine.StartSaga(context.Background(), StartRequest{Type: "rally", DefinitionVersion: 1, BusinessKey: "r-1", Data: []byte("initial")})
	if err != nil {
		t.Fatal(err)
	}
	first := engine.waitCommand()
	if first.Step != 0 || first.Phase != PhaseForward {
		t.Fatalf("unexpected first command: %+v", first)
	}
	if _, err = engine.Complete(context.Background(), Completion{CommandID: first.ID, IdempotencyKey: first.IdempotencyKey, SagaID: record.ID, Success: true, Data: []byte("reserved")}); err != nil {
		t.Fatal(err)
	}
	second := engine.waitCommand()
	if second.Step != 1 || string(second.Payload) != "reserved" {
		t.Fatalf("unexpected second command: %+v", second)
	}
	final, err := engine.Complete(context.Background(), Completion{CommandID: second.ID, IdempotencyKey: second.IdempotencyKey, SagaID: record.ID, Success: true, Data: []byte("done")})
	if err != nil {
		t.Fatal(err)
	}
	if final.Status != StatusCompleted || final.CompletedSteps != 2 || string(final.Data) != "done" {
		t.Fatalf("unexpected final record: %+v", final)
	}
}

// 一笔两步正向 saga 在日志里写几条事件（取代 Redis 协调器的“每笔脚本往返数”）：启动 + 派发第 0 步合成一条，接收结果 + 派发第 1 步
// 一条，接收结果到 Completed 一条——每次推进恰好一条事件，命令就在事件里，没有另外的 outbox 写。
func TestATwoStepSagaWritesOneEventPerTransition(t *testing.T) {
	engine := newSimEngine(t, nil)
	record, err := engine.StartSaga(context.Background(), StartRequest{Type: "rally", DefinitionVersion: 1, BusinessKey: "events"})
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		command := engine.waitCommand()
		if _, err := engine.Complete(context.Background(), Completion{CommandID: command.ID, IdempotencyKey: command.IdempotencyKey, SagaID: record.ID, Success: true}); err != nil {
			t.Fatal(err)
		}
	}
	if got := engine.e.Stats().Events; got != 3 || len(engine.events) != 3 {
		t.Fatalf("events = %d (%d decoded), want 3: start+dispatch, result+dispatch, result→completed", got, len(engine.events))
	}
	if engine.events[2].Saga.Record.Status != StatusCompleted || engine.events[0].Command == nil || engine.events[1].Command == nil || engine.events[2].Command != nil {
		t.Fatalf("event shapes = %+v", engine.events)
	}
}

func TestEngineCompensatesInReverseOrder(t *testing.T) {
	engine := newSimEngine(t, nil)
	record, err := engine.StartSaga(context.Background(), StartRequest{Type: "rally", DefinitionVersion: 1, BusinessKey: "r-2"})
	if err != nil {
		t.Fatal(err)
	}
	first := engine.waitCommand()
	_, err = engine.Complete(context.Background(), Completion{CommandID: first.ID, IdempotencyKey: first.IdempotencyKey, SagaID: record.ID, Success: true})
	if err != nil {
		t.Fatal(err)
	}
	second := engine.waitCommand()
	_, err = engine.Complete(context.Background(), Completion{CommandID: second.ID, IdempotencyKey: second.IdempotencyKey, SagaID: record.ID, Success: false, Retryable: false, Error: "world rejected"})
	if err != nil {
		t.Fatal(err)
	}
	compensate := engine.waitCommand()
	if compensate.Step != 0 || compensate.Phase != PhaseCompensate || compensate.Topic != "reserve.cancel" {
		t.Fatalf("unexpected compensation: %+v", compensate)
	}
	final, err := engine.Complete(context.Background(), Completion{CommandID: compensate.ID, IdempotencyKey: compensate.IdempotencyKey, SagaID: record.ID, Success: true})
	if err != nil {
		t.Fatal(err)
	}
	if final.Status != StatusCompensated {
		t.Fatalf("status=%v", final.Status)
	}
}

func TestDefinitionVersionsRunSideBySideAndArePartOfIdentity(t *testing.T) {
	engine := newSimEngine(t, nil)
	v2 := testDefinition()
	v2.Version = 2
	v2.Steps[0].ForwardTopic = "reserve.v2"
	if err := engine.e.Register(v2); err != nil {
		t.Fatal(err)
	}
	record, err := engine.StartSaga(context.Background(), StartRequest{Type: "rally", DefinitionVersion: 2, BusinessKey: "versioned"})
	if err != nil {
		t.Fatal(err)
	}
	command := engine.waitCommand()
	if record.DefinitionVersion != 2 || command.DefinitionVersion != 2 || command.Topic != "reserve.v2" {
		t.Fatalf("record=%+v command=%+v", record, command)
	}
	if _, err := engine.StartSaga(context.Background(), StartRequest{Type: "rally", DefinitionVersion: 1, BusinessKey: "versioned"}); !errors.Is(err, ErrIdentityConflict) {
		t.Fatalf("definition version identity err=%v", err)
	}
}

func TestMissingDefinitionVersionIsFencedForManualRepair(t *testing.T) {
	engine := newSimEngine(t, nil)
	now := engine.clock.Now()
	id := IDOf("rally", "missing-version")
	engine.seed(Record{ID: id, Type: "rally", DefinitionVersion: 99, BusinessKey: "missing-version", Status: StatusPending, Phase: PhaseForward, Version: 1, NextRunAt: now, CreatedAt: now, UpdatedAt: now})
	engine.advance(0)
	if current, err := engine.Get(context.Background(), id); err != nil || current.Status != StatusManualRequired {
		t.Fatalf("missing definition was not fenced: %+v %v", current, err)
	}
}

func TestStartSagaIdentityUsesStorageSafeDeadlinePrecision(t *testing.T) {
	engine := newSimEngine(t, nil)
	deadline := time.Date(2026, 8, 24, 20, 0, 0, 987654321, time.FixedZone("test", 8*60*60))
	one, err := engine.StartSaga(context.Background(), StartRequest{Type: "rally", DefinitionVersion: 1, BusinessKey: "deadline-precision", DeadlineAt: deadline})
	if err != nil {
		t.Fatal(err)
	}
	two, err := engine.StartSaga(context.Background(), StartRequest{Type: "rally", DefinitionVersion: 1, BusinessKey: "deadline-precision", DeadlineAt: deadline})
	if err != nil || one.ID != two.ID {
		t.Fatalf("redelivery was not idempotent: one=%+v two=%+v err=%v", one, two, err)
	}
	if got := one.DeadlineAt.Nanosecond(); got != 987000000 {
		t.Fatalf("deadline precision=%d", got)
	}
}

func TestNestStartEffectHasStableIdentity(t *testing.T) {
	one, err := NewStartEffect(StartRequest{Type: "rally", DefinitionVersion: 1, BusinessKey: "r-1", Data: []byte("one")})
	if err != nil {
		t.Fatal(err)
	}
	two, err := NewStartEffect(StartRequest{Type: "rally", DefinitionVersion: 1, BusinessKey: "r-1", Data: []byte("two")})
	if err != nil {
		t.Fatal(err)
	}
	if one.ID == two.ID || one.Topic != StartEffectTopic {
		t.Fatalf("different intents must have different effect identities: %+v %+v", one, two)
	}
	replay, err := NewStartEffect(StartRequest{Type: "rally", DefinitionVersion: 1, BusinessKey: "r-1", Data: []byte("one"), Now: time.Now()})
	if err != nil || replay.ID != one.ID {
		t.Fatalf("exact intent identity is not stable: %+v err=%v", replay, err)
	}
	decoded, err := DecodeStartEffect(one.Payload)
	if err != nil || decoded.BusinessKey != "r-1" {
		t.Fatalf("decoded=%+v err=%v", decoded, err)
	}
}

func TestLateDuplicateCompletionIsAcknowledgedAfterNextStep(t *testing.T) {
	engine := newSimEngine(t, nil)
	record, err := engine.StartSaga(context.Background(), StartRequest{Type: "rally", DefinitionVersion: 1, BusinessKey: "late"})
	if err != nil {
		t.Fatal(err)
	}
	first := engine.waitCommand()
	completion := Completion{CommandID: first.ID, IdempotencyKey: first.IdempotencyKey, SagaID: record.ID, Success: true, Data: []byte("reserved")}
	if _, err = engine.Complete(context.Background(), completion); err != nil {
		t.Fatal(err)
	}
	_ = engine.waitCommand()
	if _, err = engine.Complete(context.Background(), completion); err != nil {
		t.Fatalf("late duplicate was not acknowledged: %v", err)
	}
	obsolete := completion
	obsolete.CommandID = "unseen-retry-delivery"
	if _, err = engine.Complete(context.Background(), obsolete); err != nil {
		t.Fatalf("closed operation result was not acknowledged: %v", err)
	}
}

func TestRetryableCompletionCreatesNewDeliveryForSameOperation(t *testing.T) {
	engine := newSimEngine(t, nil)
	record, err := engine.StartSaga(context.Background(), StartRequest{Type: "rally", DefinitionVersion: 1, BusinessKey: "retry"})
	if err != nil {
		t.Fatal(err)
	}
	first := engine.waitCommand()
	if _, err = engine.Complete(context.Background(), Completion{CommandID: first.ID, IdempotencyKey: first.IdempotencyKey, SagaID: record.ID, Success: false, Retryable: true, Error: "busy"}); err != nil {
		t.Fatal(err)
	}
	engine.advance(backoffPassed)
	second := engine.waitCommand()
	if second.ID == first.ID || second.IdempotencyKey != first.IdempotencyKey || second.Attempt != 2 {
		t.Fatalf("unexpected retry: first=%+v second=%+v", first, second)
	}
	after, err := engine.Complete(context.Background(), Completion{CommandID: second.ID, IdempotencyKey: second.IdempotencyKey, SagaID: record.ID, Success: true})
	if err != nil {
		t.Fatal(err)
	}
	if after.Step != 1 {
		t.Fatalf("retry did not advance: %+v", after)
	}
}

// 接收可重试失败只把记录放回退避（Pending、同一步、NextRunAt 在未来、没有 CommandID），不在同一轮派发下一次尝试。
// 历史：Redis 协调器的 TestCompleteDoesNotFoldRetryBackoff（当时断言 outbox 里只有一条命令）。
func TestCompleteDoesNotFoldRetryBackoff(t *testing.T) {
	engine := newSimEngine(t, nil)
	record, err := engine.StartSaga(context.Background(), StartRequest{Type: "rally", DefinitionVersion: 1, BusinessKey: "backoff"})
	if err != nil {
		t.Fatal(err)
	}
	first := engine.waitCommand()
	after, err := engine.Complete(context.Background(), Completion{CommandID: first.ID, IdempotencyKey: first.IdempotencyKey, SagaID: record.ID, Retryable: true, Error: "try again"})
	if err != nil {
		t.Fatal(err)
	}
	if after.Status != StatusPending || after.Step != 0 || after.Attempt != 1 || !after.NextRunAt.After(after.UpdatedAt) || after.CommandID != "" {
		t.Fatalf("after a retryable failure: %+v; want Pending on the same step with a future NextRunAt", after)
	}
	if len(engine.commands) != 0 {
		t.Fatalf("%d commands dispatched in the result's round; the backoff must not queue the next attempt", len(engine.commands))
	}
}

func TestCompletionCannotInjectRemoteClockIntoState(t *testing.T) {
	engine := newSimEngine(t, nil)
	record, err := engine.StartSaga(context.Background(), StartRequest{Type: "rally", DefinitionVersion: 1, BusinessKey: "clock"})
	if err != nil {
		t.Fatal(err)
	}
	command := engine.waitCommand()
	after, err := engine.Complete(context.Background(), Completion{CommandID: command.ID, IdempotencyKey: command.IdempotencyKey, SagaID: record.ID, Success: true, CompletedAt: time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatal(err)
	}
	if after.UpdatedAt.After(engine.clock.Now().Add(time.Minute)) {
		t.Fatalf("remote clock entered coordinator state: %s", after.UpdatedAt)
	}
}

func TestLateSuccessfulStepIsIncludedInCompensation(t *testing.T) {
	engine := newSimEngine(t, nil)
	record, err := engine.StartSaga(context.Background(), StartRequest{Type: "rally", DefinitionVersion: 1, BusinessKey: "deadline", DeadlineAt: engine.clock.Now().Add(50 * time.Millisecond)})
	if err != nil {
		t.Fatal(err)
	}
	first := engine.waitCommand()
	engine.clock.Advance(60 * time.Millisecond)
	after, err := engine.Complete(context.Background(), Completion{CommandID: first.ID, IdempotencyKey: first.IdempotencyKey, SagaID: record.ID, Success: true})
	if err != nil {
		t.Fatal(err)
	}
	if !compensatingStep(after, 0) || after.CompletedSteps != 1 {
		t.Fatalf("late success escaped compensation: %+v", after)
	}
	command := engine.waitCommand()
	if command.Phase != PhaseCompensate {
		t.Fatalf("command=%+v", command)
	}
}

func TestResumeRequiresExplicitExpiredDeadlinePolicy(t *testing.T) {
	engine := newSimEngine(t, nil)
	now := engine.clock.Now()
	id := IDOf("rally", "expired")
	engine.seed(Record{ID: id, Type: "rally", DefinitionVersion: 1, BusinessKey: "expired", Status: StatusFailed, Phase: PhaseForward, Step: 0, Version: 1, DeadlineAt: now.Add(-time.Minute), CreatedAt: now.Add(-time.Hour), UpdatedAt: now.Add(-time.Minute)})
	if _, err := engine.Resume(context.Background(), ResumeRequest{ID: id, Now: now}); !errors.Is(err, ErrDeadlineExpired) {
		t.Fatalf("err=%v", err)
	}
	resumed, err := engine.Resume(context.Background(), ResumeRequest{ID: id, Now: now, ClearDeadline: true})
	if err != nil {
		t.Fatal(err)
	}
	// 分区在 Resume 的同一轮里派发到期的第 0 步，所以判定回复带的记录已经在等它（Redis 协调器回复的是派发前的 Pending）。
	if resumed.Status != StatusWaiting || resumed.Step != 0 || !resumed.DeadlineAt.IsZero() {
		t.Fatalf("resumed=%+v", resumed)
	}
}

func TestResumeMintsCommandIDsDisjointFromPreviousLife(t *testing.T) {
	// Regression: Resume resets Attempt, so the first redispatch used to mint
	// the same CommandID as the first attempt of the failed life. A stale
	// completion receipt under that ID then blocked the resumed operation as
	// a false duplicate. Incarnation keeps every life's identifiers disjoint.
	engine := newSimEngine(t, nil)
	record, err := engine.StartSaga(context.Background(), StartRequest{Type: "rally", DefinitionVersion: 1, BusinessKey: "resume-ids"})
	if err != nil {
		t.Fatal(err)
	}
	first := engine.waitCommand()
	failed, err := engine.Complete(context.Background(), Completion{CommandID: first.ID, IdempotencyKey: first.IdempotencyKey, SagaID: record.ID, Success: false, Error: "boom"})
	if err != nil {
		t.Fatal(err)
	}
	if failed.Status != StatusFailed {
		t.Fatalf("expected failed record, got %+v", failed)
	}

	resumed, err := engine.Resume(context.Background(), ResumeRequest{ID: record.ID})
	if err != nil {
		t.Fatal(err)
	}
	if resumed.Incarnation != 1 {
		t.Fatalf("expected incarnation 1, got %d", resumed.Incarnation)
	}
	redispatched := engine.waitCommand()
	if redispatched.ID == first.ID {
		t.Fatalf("resumed dispatch reused CommandID %q from the failed life", first.ID)
	}
	if redispatched.IdempotencyKey != first.IdempotencyKey {
		t.Fatalf("operation identity must survive resume: %q != %q", redispatched.IdempotencyKey, first.IdempotencyKey)
	}
	// The resumed completion must apply as fresh progress, not a duplicate.
	after, err := engine.Complete(context.Background(), Completion{CommandID: redispatched.ID, IdempotencyKey: redispatched.IdempotencyKey, SagaID: record.ID, Success: true})
	if err != nil {
		t.Fatal(err)
	}
	if after.CompletedSteps != 1 {
		t.Fatalf("resumed completion did not advance the saga: %+v", after)
	}
}

func TestDefinitionValidation(t *testing.T) {
	tests := []Definition{{}, {Type: "x", Version: 1, Steps: []Step{{Name: "x"}}}, {Type: "x", Version: 1, Steps: []Step{{Name: "x", ForwardTopic: "x", Timeout: time.Second, MaxAttempts: 1, BackoffMin: time.Second, BackoffMax: time.Second}, {Name: "x", ForwardTopic: "y", Timeout: time.Second, MaxAttempts: 1, BackoffMin: time.Second, BackoffMax: time.Second}}}}
	for i, test := range tests {
		if err := test.Validate(); err == nil {
			t.Fatalf("case %d accepted", i)
		}
	}
}

func TestDefinitionRejectsUnsafeTransportSubject(t *testing.T) {
	definition := testDefinition()
	definition.Steps[0].ForwardTopic = "reserve.>"
	if err := definition.Validate(); err == nil {
		t.Fatal("wildcard topic accepted")
	}
	definition = testDefinition()
	definition.Steps[0].ForwardTopic = "reserve..troops"
	if err := definition.Validate(); err == nil {
		t.Fatal("empty subject token accepted")
	}
}
