package saga

import (
	"context"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/mongo/mongotest"
)

// U-0225 · C2 · 无 RR（game-demo 实跑发现）：步骤给出不可重试的失败之后，协调器必须能把
// "进入补偿"这一步落到存储里。旧行为：applyCompletion / retryOrCompensate 先把记录版本 +1，
// 再交给 beginCompensation，后者又 `max(after.Version, record.Version+1)` 一次——两次 +1。
// MongoStore.Apply 只接受 `After.Version == ExpectedVersion+1`，于是 Engine.Complete 对每一个
// 拒绝都返回 `saga: invalid record`，saga 卡在 waiting，步骤按超时反复重发同一个拒绝；
// 内存 store 只比对当前版本，单测里看不见（TestEngineCompensatesInReverseOrder 一直绿）。
// 显式 Compensate 与 deadline 路径传的是未加版的记录，只加一次，所以这两条路一直是对的。

// waitingOnMongo puts a rally saga into the state the live symptom had: the
// first step done, the second step's command out, the coordinator waiting
// for its result. The record is written straight to a real MongoStore
// (mongotest replica) — the claim loop is not involved, so the version rule
// the store enforces on Apply is the only thing under test.
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

// The timeout path ends the same way: attempts exhausted → compensation.
// retryOrCompensate is what the claim loop calls; it must hand the store a
// record one version ahead, whatever branch it takes.
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

// The unit view of the same promise: one completion, one version step,
// whatever the outcome.
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
