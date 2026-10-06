//go:build integration

package saga

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

// saga 方向 ④ 在真实 Mongo 副本集上：同一个放弃后迟到的正向成功并发送达多次，只有一次把记录带回补偿（回执、tombstone 改带结果关闭、
// late_step / late_data 在同一事务里），其余按回执去重；记录的新字段经服务端往返不变。
func TestRealMongoLateSuccessReopensACompensatedSagaOnce(t *testing.T) {
	client, database := realMongoU0280(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	store, err := NewMongoStore(client, MongoStoreOptions{Database: database})
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
	for round := 0; round < 10; round++ {
		sagaID := fmt.Sprintf("rally-late-%d", round)
		now := time.Now().UTC().Truncate(time.Millisecond)
		// 第 1 步被放弃、第 0 步已补偿：Compensated，第 1 步的 tombstone 是放弃关闭。
		record := Record{ID: sagaID, Type: "rally", DefinitionVersion: 1, BusinessKey: sagaID, Status: StatusCompensated, Phase: PhaseCompensate, Version: 5, CreatedAt: now, UpdatedAt: now}
		if err := store.Create(ctx, record); err != nil {
			t.Fatal(err)
		}
		operation := operationKey(sagaID, PhaseForward, 1)
		if _, err := store.operations().InsertOne(ctx, operationDoc{ID: operation, SagaID: sagaID, Closure: operationClosureAbandoned, CreatedAt: now}); err != nil {
			t.Fatal(err)
		}
		late := Completion{CommandID: commandID(operation, 0, 2), IdempotencyKey: operation, SagaID: sagaID, Success: true, Data: []byte("march-data")}
		before := engine.Stats()
		const deliveries = 8
		var wg sync.WaitGroup
		errs := make([]error, deliveries)
		start := make(chan struct{})
		for i := 0; i < deliveries; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				_, errs[i] = engine.Complete(ctx, late)
			}(i)
		}
		close(start)
		wg.Wait()
		for i, err := range errs {
			if err != nil {
				t.Fatalf("round %d delivery %d: %v", round, i, err)
			}
		}
		if got := engine.Stats().LateAfterAbandon - before.LateAfterAbandon; got != 1 {
			t.Fatalf("round %d: %d of %d concurrent deliveries brought the saga back to compensation, want exactly 1", round, got, deliveries)
		}
		stored, err := store.Get(ctx, sagaID)
		if err != nil {
			t.Fatal(err)
		}
		if stored.Version != record.Version+1 || stored.Status != StatusCompensating || stored.Step != 1 || stored.LateStep != 2 || string(stored.LateData) != "march-data" || stored.CompletedSteps != 0 {
			t.Fatalf("round %d: stored record = %+v, want one write that compensates only step 1", round, stored)
		}
		history, err := store.CompletionHistory(ctx, late)
		if err != nil || !history.Receipt {
			t.Fatalf("round %d: history = %+v, %v; want the receipt written with the transition", round, history, err)
		}
		var tombstone operationDoc
		if err := store.operations().FindOne(ctx, map[string]any{"_id": operation}, &tombstone); err != nil || tombstone.Closure != operationClosureResult {
			t.Fatalf("round %d: tombstone = %+v, %v; want closed with result", round, tombstone, err)
		}
	}
}
