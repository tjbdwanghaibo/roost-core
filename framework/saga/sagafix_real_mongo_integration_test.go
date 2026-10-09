//go:build integration

package saga

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// RR-20261006-42 在真实 Mongo 副本集上：退避中送达的成功与协调器派发下一次尝试并发。不论谁先写，这一步都恰好计入一次
// （成功先到：接收、带结果关闭操作、派发冲突；派发先到：在等下一次尝试时按规则 3 接收较早那次的成功），回执经服务端往返。
func TestRealMongoSuccessDuringBackoffRacesTheNextDispatch(t *testing.T) {
	client, database := realMongoU0280(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
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
	for round := 0; round < 20; round++ {
		id := fmt.Sprintf("backoff-race-%d", round)
		now := time.Now().UTC().Truncate(time.Millisecond)
		if err := store.Create(ctx, Record{ID: id, Type: "rally", DefinitionVersion: 1, BusinessKey: id, Status: StatusPending, Phase: PhaseForward, Version: 1, NextRunAt: now, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatal(err)
		}
		get := func() Record {
			t.Helper()
			record, err := store.Get(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			return record
		}
		if err := engine.processClaimed(ctx, get(), now); err != nil { // 尝试 1
			t.Fatal(err)
		}
		waiting := get()
		success := Completion{CommandID: waiting.CommandID, IdempotencyKey: waiting.OperationKey, SagaID: id, Success: true}
		if err := engine.processClaimed(ctx, waiting, waiting.NextRunAt); err != nil { // 尝试 1 超时 → 退避
			t.Fatal(err)
		}
		backoff := get()
		var wg sync.WaitGroup
		var completeErr, dispatchErr error
		start := make(chan struct{})
		wg.Add(2)
		go func() { defer wg.Done(); <-start; _, completeErr = engine.Complete(ctx, success) }()
		go func() { defer wg.Done(); <-start; dispatchErr = engine.processClaimed(ctx, backoff, backoff.NextRunAt) }()
		close(start)
		wg.Wait()
		if completeErr != nil {
			t.Fatalf("round %d: success during backoff = %v, want accepted", round, completeErr)
		}
		if dispatchErr != nil && !errors.Is(dispatchErr, ErrConflict) {
			t.Fatalf("round %d: dispatch = %v", round, dispatchErr)
		}
		if record := get(); record.CompletedSteps != 1 || record.Step != 1 || record.Status != StatusPending {
			t.Fatalf("round %d: the step was not counted exactly once: status=%s step=%d completed=%d", round, record.Status, record.Step, record.CompletedSteps)
		}
		history, err := store.CompletionHistory(ctx, success)
		if err != nil || !history.Receipt {
			t.Fatalf("round %d: history = %+v err=%v, want the success's receipt", round, history, err)
		}
	}
}

// RR-20261006-45 在真实 Mongo 上：ClaimDue / List 跳过一条校验不过的记录，同批其他记录照常返回，坏记录不被改写。
func TestRealMongoClaimDueAndListSkipACorruptRecord(t *testing.T) {
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
	now := time.Now().UTC().Truncate(time.Millisecond)
	record := func(id string, offset time.Duration) Record {
		return Record{ID: id, Type: "rally", DefinitionVersion: 1, BusinessKey: "key-" + id, Status: StatusPending, Phase: PhaseForward, Version: 1,
			NextRunAt: now.Add(offset), CreatedAt: now, UpdatedAt: now.Add(offset)}
	}
	for _, r := range []Record{record("good-a", -2*time.Second), record("good-c", -time.Second)} {
		if err := store.Create(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	corrupt := toRecordDoc(record("corrupt-b", -3*time.Second))
	corrupt.BusinessKey = ""
	if _, err := store.sagas().InsertOne(ctx, corrupt); err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimDue(ctx, ClaimRequest{Owner: "coordinator", Now: now, LeaseDuration: 15 * time.Second, Limit: 10})
	if err != nil || len(claimed) != 2 {
		t.Fatalf("ClaimDue = %d records, %v; want the two good records and no error", len(claimed), err)
	}
	listed, err := store.List(ctx, Query{Limit: 100})
	if err != nil || len(listed) != 2 {
		t.Fatalf("List = %d records, %v; want the two good records and no error", len(listed), err)
	}
	var raw recordDoc
	if err := store.sagas().FindOne(ctx, map[string]any{"_id": "corrupt-b"}, &raw); err != nil || raw.BusinessKey != "" || raw.Status != StatusPending {
		t.Fatalf("the corrupt record was changed: %+v err=%v", raw, err)
	}
}
