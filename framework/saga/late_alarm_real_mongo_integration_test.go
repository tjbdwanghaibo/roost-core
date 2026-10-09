//go:build integration

package saga

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// B1 在真实 Mongo 副本集上：同一个放弃后迟到的成功并发送达多次，tombstone 上的告警标记只让一个调用得到
// first=true（单文档条件更新的原子性；mongotest 的锁粒度证明不了服务端行为）。
func TestRealMongoLateSuccessAlarmIsMarkedOnce(t *testing.T) {
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
	for round := 0; round < 20; round++ {
		sagaID := fmt.Sprintf("gift-b1-%d", round)
		operation := sagaID + ":1:0"
		if _, err := store.operations().InsertOne(ctx, operationDoc{ID: operation, SagaID: sagaID, Closure: operationClosureAbandoned, CreatedAt: time.Now().UTC()}); err != nil {
			t.Fatal(err)
		}
		late := Completion{CommandID: operation + ":1", IdempotencyKey: operation, SagaID: sagaID, Success: true}
		const deliveries = 8
		var wg sync.WaitGroup
		firsts := make([]bool, deliveries)
		errs := make([]error, deliveries)
		start := make(chan struct{})
		for i := 0; i < deliveries; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				firsts[i], errs[i] = store.MarkLateSuccessAlarm(ctx, late, 0)
			}(i)
		}
		close(start)
		wg.Wait()
		winners := 0
		for i := range firsts {
			if errs[i] != nil {
				t.Fatalf("round %d delivery %d: %v", round, i, errs[i])
			}
			if firsts[i] {
				winners++
			}
		}
		if winners != 1 {
			t.Fatalf("round %d: %d concurrent deliveries of one late success were each told they are the first alarm, want exactly 1", round, winners)
		}
		var doc operationDoc
		if err := store.operations().FindOne(ctx, bson.M{"_id": operation}, &doc); err != nil {
			t.Fatal(err)
		}
		if _, ok := doc.LateAlarms["r0"]; len(doc.LateAlarms) != 1 || !ok {
			t.Fatalf("round %d: tombstone late_alarms = %v, want only r0", round, doc.LateAlarms)
		}
		if doc.Closure != operationClosureAbandoned || doc.SagaID != sagaID {
			t.Fatalf("round %d: marking changed the tombstone: %+v", round, doc)
		}
	}
}
