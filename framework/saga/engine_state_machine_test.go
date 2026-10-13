package saga

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/infra/storage/mongo/mongotest"
)

// idleEngine 的协调循环不运行，记录停在测试放置的位置。
func idleEngine(t *testing.T) (*Engine, *memoryStore) {
	t.Helper()
	store := newMemoryStore()
	engine, err := NewEngine(store, PublishFunc(func(context.Context, Command) error { return nil }), DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Register(testDefinition()); err != nil {
		t.Fatal(err)
	}
	return engine, store
}

func seedRecord(t *testing.T, store *memoryStore, id string, status Status, completed int) Record {
	t.Helper()
	now := time.Now().UTC()
	record := Record{ID: id, Type: "rally", DefinitionVersion: 1, BusinessKey: "key-" + id, Status: status, Phase: PhaseForward, Step: completed, CompletedSteps: completed, Version: 3, NextRunAt: now, CreatedAt: now, UpdatedAt: now}
	if status == StatusWaiting {
		record.OperationKey, record.CommandID, record.Attempt = "op", "cmd", 1
	}
	if status == StatusCompensating {
		record.Phase = PhaseCompensate
	}
	if err := record.Validate(); err != nil {
		t.Fatalf("seed %s: %v", id, err)
	}
	if err := store.Create(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	return record
}

// 引擎公开入口守住状态机：只有部分状态可以 Resume / Compensate，定义必须已知，请求有界。
// 协调循环不运行，拒绝是唯一可能的结果。
func TestEngineRegisterAndStartRefuseUnknownOrDuplicateDefinitions(t *testing.T) {
	engine, _ := idleEngine(t)
	if err := engine.Register(testDefinition()); !errors.Is(err, ErrAlreadyExists) || !strings.Contains(err.Error(), "rally/1") {
		t.Fatalf("duplicate Register = %v", err)
	}
	if _, err := engine.StartSaga(context.Background(), StartRequest{Type: "rally", DefinitionVersion: 2, BusinessKey: "b"}); !errors.Is(err, ErrDefinitionMissing) || !strings.Contains(err.Error(), "rally/2") {
		t.Fatalf("unknown definition version = %v", err)
	}
	for name, req := range map[string]StartRequest{
		"blank business key":        {Type: "rally", DefinitionVersion: 1, BusinessKey: "   "},
		"business key too long":     {Type: "rally", DefinitionVersion: 1, BusinessKey: strings.Repeat("k", 513)},
		"data over MaxPayloadBytes": {Type: "rally", DefinitionVersion: 1, BusinessKey: "b", Data: make([]byte, DefaultOptions().MaxPayloadBytes+1)},
	} {
		if _, err := engine.StartSaga(context.Background(), req); !errors.Is(err, ErrInvalidRecord) {
			t.Fatalf("%s = %v, want ErrInvalidRecord", name, err)
		}
	}
	if _, err := engine.List(context.Background(), Query{Limit: 1001}); !errors.Is(err, ErrInvalidRecord) {
		t.Fatalf("List limit 1001 = %v", err)
	}
	if _, err := engine.List(context.Background(), Query{Limit: 1000}); err != nil {
		t.Fatalf("List limit 1000 = %v", err)
	}
}

func TestEngineResumeRefusesEachIllegalRequest(t *testing.T) {
	engine, store := idleEngine(t)
	seedRecord(t, store, "pending", StatusPending, 0)
	seedRecord(t, store, "done", StatusCompleted, 2)
	failed := seedRecord(t, store, "failed", StatusFailed, 1)
	orphan := seedRecord(t, store, "orphan", StatusFailed, 0)
	orphan.DefinitionVersion = 9
	if _, err := store.Apply(context.Background(), ApplyRequest{ExpectedVersion: orphan.Version, After: func() Record { o := orphan; o.Version++; return o }()}); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	now := time.Now().UTC()
	cases := []struct {
		name    string
		request ResumeRequest
		want    error
		text    string
	}{
		{"id with wildcard", ResumeRequest{ID: "bad.>"}, ErrInvalidRecord, ""},
		{"clear-deadline together with a deadline", ResumeRequest{ID: "failed", ClearDeadline: true, DeadlineAt: now.Add(time.Hour)}, ErrInvalidRecord, ""},
		{"deadline already passed", ResumeRequest{ID: "failed", Now: now, DeadlineAt: now.Add(-time.Second)}, ErrDeadlineExpired, ""},
		{"pending cannot resume", ResumeRequest{ID: "pending"}, nil, "status 1 cannot resume"},
		{"completed cannot resume", ResumeRequest{ID: "done"}, nil, "status 4 cannot resume"},
		{"definition gone", ResumeRequest{ID: "orphan"}, ErrDefinitionMissing, "rally/9"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := engine.Resume(ctx, tc.request)
			if err == nil || (tc.want != nil && !errors.Is(err, tc.want)) || !strings.Contains(err.Error(), tc.text) {
				t.Fatalf("Resume = %v, want %v %q", err, tc.want, tc.text)
			}
		})
	}
	resumed, err := engine.Resume(ctx, ResumeRequest{ID: "failed", Now: now})
	if err != nil {
		t.Fatalf("legal resume: %v", err)
	}
	if resumed.Incarnation != failed.Incarnation+1 || resumed.Status != StatusCompensating || resumed.Phase != PhaseCompensate {
		t.Fatalf("resumed failed saga with completed steps = %+v, want a new incarnation compensating", resumed)
	}
}

func TestEngineCompensateRefusesInFlightAndNothingToUndo(t *testing.T) {
	engine, store := idleEngine(t)
	seedRecord(t, store, "waiting", StatusWaiting, 1)
	seedRecord(t, store, "fresh", StatusPending, 0)
	already := seedRecord(t, store, "already", StatusCompensating, 1)
	ctx := context.Background()
	if _, err := engine.Compensate(ctx, "waiting", "operator", time.Time{}); err == nil || !strings.Contains(err.Error(), "cannot force compensation while a step result is in flight") {
		t.Fatalf("Compensate while waiting = %v", err)
	}
	if _, err := engine.Compensate(ctx, "fresh", "operator", time.Time{}); err == nil || !strings.Contains(err.Error(), "no completed steps to compensate") {
		t.Fatalf("Compensate with nothing done = %v", err)
	}
	got, err := engine.Compensate(ctx, "already", "operator", time.Time{})
	if err != nil || got.Version != already.Version {
		t.Fatalf("Compensate on an already compensating saga must be a no-op: %+v, %v", got, err)
	}
	if _, err := engine.Complete(ctx, Completion{CommandID: "c", IdempotencyKey: "k", SagaID: "waiting", Success: true, Data: make([]byte, DefaultOptions().MaxPayloadBytes+1)}); !errors.Is(err, ErrInvalidRecord) {
		t.Fatalf("Complete with data over MaxPayloadBytes = %v", err)
	}
	if _, err := engine.Complete(ctx, Completion{CommandID: "c", IdempotencyKey: "k", SagaID: "waiting", Success: false}); !errors.Is(err, ErrInvalidRecord) {
		t.Fatalf("Complete failing validation = %v", err)
	}
}

// Completed 是业务结论（全部步骤已生效，按终态行动的一方已据此行动）：人工 Compensate 返回 ErrSagaCompleted，
// 记录原样不动、不计 saga.reopened_total。撤销已完成的业务走业务自己的冲正流程。
// 历史：RR-20261006-47（维护者 2026-10-07 选择拒绝）。
func TestManualCompensateRefusesACompletedSaga(t *testing.T) {
	ctx := context.Background()
	engine, store := idleEngine(t)
	seeded := seedRecord(t, store, "done", StatusCompleted, len(testDefinition().Steps))
	reopenedBefore := counterValue("saga.reopened_total")

	got, err := engine.Compensate(ctx, "done", "operator", seeded.UpdatedAt)
	if !errors.Is(err, ErrSagaCompleted) {
		t.Fatalf("Compensate on a Completed saga = (status=%s phase=%s step=%d completed=%d, err=%v), want ErrSagaCompleted: a completed saga is a business conclusion others already acted on",
			got.Status, got.Phase, got.Step, got.CompletedSteps, err)
	}
	after, err := store.Get(ctx, "done")
	if err != nil {
		t.Fatal(err)
	}
	if after.Status != StatusCompleted || after.Phase != PhaseForward || after.Version != seeded.Version || after.Incarnation != seeded.Incarnation {
		t.Fatalf("the refused Compensate changed the record: %+v", after)
	}
	if grown := counterValue("saga.reopened_total") - reopenedBefore; grown != 0 {
		t.Fatalf("saga.reopened_total grew by %d on a refused Compensate", grown)
	}
	// 守卫：其余终态与非终态的口径不变（Compensated 幂等返回、Failed 照常进入补偿由既有用例覆盖）。
	seedRecord(t, store, "undone", StatusCompensated, 0)
	if record, err := engine.Compensate(ctx, "undone", "operator", seeded.UpdatedAt); err != nil || record.Status != StatusCompensated {
		t.Fatalf("Compensate on a Compensated saga = (%s, %v), want it returned unchanged", record.Status, err)
	}
}

// 一次状态迁移只把记录版本推进 1：MongoStore.Apply 只接受 After.Version == ExpectedVersion+1。
// 不可重试的步骤失败进入补偿、超时用尽进入补偿、各类 completion 都必须满足；内存 store 只比对当前版本，
// 所以补偿入口要在 MongoStore 上验证。历史：U-0225（版本加两次，拒绝卡在 waiting）。
// waitingOnMongo 在 MongoStore（mongotest 副本集）上直接写一条 rally saga：第 0 步完成、第 1 步命令已发出、
// 协调器在等结果。不经领取循环，被测的只有存储对 Apply 的版本规则。
func waitingOnMongo(t *testing.T) (*Engine, *MongoStore, Record) {
	t.Helper()
	store, err := NewMongoStore(mongotest.NewClient(), MongoStoreOptions{Database: "saga"})
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
	now := time.Now().UTC().Truncate(time.Millisecond)
	record := Record{
		ID: "gift-1", Type: "rally", DefinitionVersion: 1, BusinessKey: "gift-1",
		Status: StatusWaiting, Phase: PhaseForward, Step: 1, CompletedSteps: 1, Attempt: 1, Version: 3,
		OperationKey: operationKey("gift-1", PhaseForward, 1), CommandID: commandID(operationKey("gift-1", PhaseForward, 1), 0, 1),
		NextRunAt: now.Add(time.Minute), CreatedAt: now, UpdatedAt: now,
	}
	if err := record.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := store.Create(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	return engine, store, record
}

func TestStepRefusalMovesSagaIntoCompensationOnMongoStore(t *testing.T) {
	engine, store, record := waitingOnMongo(t)
	after, err := engine.Complete(context.Background(), Completion{CommandID: record.CommandID, IdempotencyKey: record.OperationKey, SagaID: record.ID, Success: false, Retryable: false, Error: "recipient has never entered the game"})
	if err != nil {
		t.Fatalf("Complete rejected a valid step refusal: %v", err)
	}
	if after.Status != StatusCompensating || after.Phase != PhaseCompensate || after.Step != 0 || after.Version != record.Version+1 {
		t.Fatalf("after refusal: status=%v phase=%v step=%d version=%d", after.Status, after.Phase, after.Step, after.Version)
	}
	stored, err := store.Get(context.Background(), record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != StatusCompensating || stored.LastError != "recipient has never entered the game" {
		t.Fatalf("stored: status=%v last_error=%q", stored.Status, stored.LastError)
	}
}

// 超时路径同样收敛到补偿：retryOrCompensate 无论走重试还是补偿分支，交给存储的记录都恰好前进一个版本。
func TestRetryOrCompensateAdvancesVersionByOne(t *testing.T) {
	engine := &Engine{}
	now := time.Now().UTC()
	definition := testDefinition()
	base := Record{ID: "s-2", Type: "rally", DefinitionVersion: 1, BusinessKey: "k", Status: StatusWaiting, Phase: PhaseForward, Step: 1, CompletedSteps: 1, Version: 7, OperationKey: "s-2:1:1", CommandID: "s-2:1:1:1", NextRunAt: now, CreatedAt: now, UpdatedAt: now}
	retry := base
	retry.Attempt = 1 // below MaxAttempts 2: schedules another attempt
	exhausted := base
	exhausted.Attempt = 2 // at MaxAttempts: compensates
	for name, record := range map[string]Record{"retry": retry, "exhausted": exhausted} {
		after := engine.retryOrCompensate(record, definition, "step result timeout", now)
		if after.Version != record.Version+1 {
			t.Errorf("%s: version %d → %d, want %d (status %v)", name, record.Version, after.Version, record.Version+1, after.Status)
		}
	}
}

// 同一承诺的单元视角：一份 completion 一次版本推进，与结果无关。
func TestApplyCompletionAdvancesVersionByOne(t *testing.T) {
	engine := &Engine{}
	now := time.Now().UTC()
	definition := testDefinition()
	record := Record{ID: "s-1", Type: "rally", DefinitionVersion: 1, BusinessKey: "k", Status: StatusWaiting, Phase: PhaseForward, Step: 1, CompletedSteps: 1, Attempt: 1, Version: 7, OperationKey: "s-1:1:1", CommandID: "s-1:1:1:1", NextRunAt: now, CreatedAt: now, UpdatedAt: now}
	for name, completion := range map[string]Completion{
		"success":         {Success: true, CompletedAt: now},
		"refusal":         {Success: false, Retryable: false, Error: "no", CompletedAt: now},
		"retryable-final": {Success: false, Retryable: true, Error: "later", CompletedAt: now},
	} {
		if after := engine.applyCompletion(record, definition, completion); after.Version != record.Version+1 {
			t.Errorf("%s: version %d → %d, want %d", name, record.Version, after.Version, record.Version+1)
		}
	}
}
