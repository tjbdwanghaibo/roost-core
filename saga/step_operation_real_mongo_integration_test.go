//go:build integration

package saga

// U-0280 在真实 Mongo 副本集上的两条承诺（mongotest 按集合检测写冲突，比服务端的文档级冲突保守，
// 证明不了这两点）：
//
//  1. 操作实例守卫：同一操作实例的多个尝试并发 Reserve（各自的租约都有效），只有一个拿到新租约，
//     其余得到 errOperationAttemptInFlight；没有守卫时它们写的是不同的 claim 文档，互不冲突，会各自执行。
//  2. 接替与投影串行化：尝试 k 已写进 WAL，投影（真实 dataengine MongoStore.Project）与尝试 k+1 的
//     Reserve（在它看来 k 的租约已过期，接替 k）并发。两者写同一个 claim 文档，只能有一个提交：
//     要么 k 生效、k+1 回放 k 的结果，要么 k 被跳过、k+1 执行；任何一轮都不能两者都生效。

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	deengine "github.com/tjbdwanghaibo/roost-core/dataengine/engine"
	fmongo "github.com/tjbdwanghaibo/roost-core/mongo"
	"github.com/tjbdwanghaibo/roost-core/mongo/driver"
	corenest "github.com/tjbdwanghaibo/roost-core/nest"
	"go.mongodb.org/mongo-driver/v2/bson"
)

func realMongoU0280(t *testing.T) (*driver.Client, string) {
	t.Helper()
	uri := os.Getenv("ROOST_DATAENGINE_IT_MONGO_URI")
	if uri == "" {
		t.Skip("ROOST_DATAENGINE_IT_MONGO_URI is not set; run through kit/scripts/integration/dataengine-env.sh")
	}
	client, err := driver.NewClient(fmongo.DefaultConfig(uri), driver.IndexMigrationPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	database := fmt.Sprintf("roost_u0280_%d_%d", os.Getpid(), time.Now().UnixNano())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := client.Database(database).Drop(ctx); err != nil {
			t.Errorf("drop %s: %v", database, err)
		}
		_ = client.Close(context.Background())
	})
	return client, database
}

func TestRealMongoConcurrentAttemptsOfOneOperationReserveOnce(t *testing.T) {
	client, database := realMongoU0280(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	for round := 0; round < 20; round++ {
		inbox, err := NewDataEngineStepInbox(client, database, DataEngineStepInboxOptions{Owner: fmt.Sprintf("u0280-%d", round), LeaseDuration: time.Minute})
		if err != nil {
			t.Fatal(err)
		}
		if err := inbox.EnsureInfrastructure(ctx); err != nil {
			t.Fatal(err)
		}
		operation := fmt.Sprintf("gift-r%d:1:0", round)
		const attempts = 6
		var wg sync.WaitGroup
		results := make([]error, attempts)
		winners := make([]bool, attempts)
		start := make(chan struct{})
		for i := 0; i < attempts; i++ {
			command := dataEngineCommand(fmt.Sprintf("%s:%d", operation, i+1), operation, "x")
			command.Attempt = uint32(i + 1)
			wg.Add(1)
			go func(i int, command Command) {
				defer wg.Done()
				<-start
				reservation, err := inbox.Reserve(ctx, command)
				results[i] = err
				winners[i] = err == nil && !reservation.Duplicate && reservation.Token > 0
			}(i, command)
		}
		close(start)
		wg.Wait()
		won := 0
		for i := range results {
			if winners[i] {
				won++
				continue
			}
			if !errors.Is(results[i], errOperationAttemptInFlight) && !errors.Is(results[i], fmongo.ErrDuplicateKey) {
				t.Fatalf("round %d attempt %d: err=%v, want errOperationAttemptInFlight (or a duplicate-key retry exhaustion on the first guard upsert)", round, i+1, results[i])
			}
		}
		if won != 1 {
			t.Fatalf("round %d: %d attempts of one operation reserved a live lease at the same time, want exactly 1 (results=%v)", round, won, results)
		}
	}
}

func TestRealMongoSupersedeAndProjectionOfTheSameAttemptSerialize(t *testing.T) {
	client, database := realMongoU0280(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	projector, err := deengine.NewMongoStore(client, deengine.MongoStoreConfig{DefaultDatabase: database, ServerID: 3, TransactionReceiptTTL: 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if err := projector.EnsureInfrastructure(ctx); err != nil {
		t.Fatal(err)
	}
	projectedK, executedK1 := 0, 0
	for round := 0; round < 40; round++ {
		inboxA, _ := NewDataEngineStepInbox(client, database, DataEngineStepInboxOptions{Owner: "u0280-a", LeaseDuration: time.Minute})
		inboxB, _ := NewDataEngineStepInbox(client, database, DataEngineStepInboxOptions{Owner: "u0280-b", LeaseDuration: time.Minute})
		if round == 0 {
			if err := inboxA.EnsureInfrastructure(ctx); err != nil {
				t.Fatal(err)
			}
		}
		operation := fmt.Sprintf("gift-s%d:1:0", round)
		k := dataEngineCommand(operation+":1", operation, "x")
		k.SagaID = fmt.Sprintf("gift-s%d", round)
		k.DeadlineAt = time.Now().UTC().Add(2 * time.Second)
		reservation, err := inboxA.Reserve(ctx, k)
		if err != nil || reservation.Duplicate {
			t.Fatalf("reserve k: %+v %v", reservation, err)
		}
		committer := &stepFenceCommitter{}
		completion := Completion{CommandID: k.ID, IdempotencyKey: k.IdempotencyKey, SagaID: k.SagaID, Success: true}
		if _, err := corenest.RunIsolatedTransaction(ctx, committer, "u0280", func() (any, error) {
			if err := inboxA.Bind(k, reservation); err != nil {
				return nil, err
			}
			return nil, EmitCompletion(completion)
		}); err != nil {
			t.Fatal(err)
		}
		record := committer.record
		// k+1 的收件箱时钟已过 k 的截止（协调器发出 k+1 的时刻），投影器用真实时钟、仍在 k 的租约内。
		k1 := k
		k1.ID, k1.Attempt = operation+":2", 2
		k1.DeadlineAt = k.DeadlineAt.Add(time.Minute)
		inboxB.now = func() time.Time { return k.DeadlineAt.Add(time.Millisecond) }
		var wg sync.WaitGroup
		var projectErr, reserveErr error
		var r1 Reservation
		wg.Add(2)
		go func() { defer wg.Done(); projectErr = projector.Project(ctx, record) }()
		go func() {
			defer wg.Done()
			for {
				r1, reserveErr = inboxB.Reserve(ctx, k1)
				if !errors.Is(reserveErr, ErrConflict) {
					return
				}
			}
		}()
		wg.Wait()
		if projectErr != nil || reserveErr != nil {
			t.Fatalf("round %d: project=%v reserve=%v", round, projectErr, reserveErr)
		}
		var marker bson.M
		if err := client.Database(database).Collection(deengine.TransactionCollection).FindOne(ctx, bson.M{"_id": record.ID.String()}, &marker); err != nil {
			t.Fatal(err)
		}
		kEffective := marker["skipped"] != true
		k1Executes := !r1.Duplicate && r1.Token > 0
		switch {
		case kEffective && k1Executes:
			t.Fatalf("round %d: attempt k was projected AND attempt k+1 got a fresh lease: the step takes effect twice", round)
		case kEffective && (!r1.Duplicate || r1.Completion.CommandID != k.ID):
			t.Fatalf("round %d: k took effect but k+1 did not replay it: %+v", round, r1)
		case !kEffective && !k1Executes:
			t.Fatalf("round %d: k was skipped and k+1 did not run either: %+v", round, r1)
		}
		if kEffective {
			projectedK++
		} else {
			executedK1++
		}
	}
	t.Logf("40 rounds: k projected and replayed %d times, k superseded and k+1 executed %d times", projectedK, executedK1)
}

// 接替先提交的一边（确定性）：k 已写进 WAL、还没投影，k+1 的收件箱时钟已过 k 的截止并接替它；之后投影器（真实时钟、
// 仍在 k 的租约内）投影 k 的记录，状态文档的当前尝试已是 k+1、token 已加一，fence 不匹配，k 被跳过；k+1 照常执行。
// 上面的并发用例由时序决定哪一边先赢（多数轮次是投影先赢），这里固定接替先赢，证明 token fence 落在状态文档上。
func TestRealMongoTakeoverFencesTheEarlierAttemptsProjection(t *testing.T) {
	client, database := realMongoU0280(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	projector, err := deengine.NewMongoStore(client, deengine.MongoStoreConfig{DefaultDatabase: database, ServerID: 4, TransactionReceiptTTL: 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if err := projector.EnsureInfrastructure(ctx); err != nil {
		t.Fatal(err)
	}
	inboxA, _ := NewDataEngineStepInbox(client, database, DataEngineStepInboxOptions{Owner: "takeover-a", LeaseDuration: time.Minute})
	inboxB, _ := NewDataEngineStepInbox(client, database, DataEngineStepInboxOptions{Owner: "takeover-b", LeaseDuration: time.Minute})
	if err := inboxA.EnsureInfrastructure(ctx); err != nil {
		t.Fatal(err)
	}
	operation := "gift-t:1:0"
	k := dataEngineCommand(operation+":1", operation, "x")
	k.SagaID = "gift-t"
	k.DeadlineAt = time.Now().UTC().Add(time.Minute)
	reservation, err := inboxA.Reserve(ctx, k)
	if err != nil || reservation.Duplicate {
		t.Fatalf("reserve k: %+v %v", reservation, err)
	}
	committer := &stepFenceCommitter{}
	if _, err := corenest.RunIsolatedTransaction(ctx, committer, "takeover", func() (any, error) {
		if err := inboxA.Bind(k, reservation); err != nil {
			return nil, err
		}
		return nil, EmitCompletion(Completion{CommandID: k.ID, IdempotencyKey: k.IdempotencyKey, SagaID: k.SagaID, Success: true})
	}); err != nil {
		t.Fatal(err)
	}
	k1 := k
	k1.ID, k1.Attempt = operation+":2", 2
	k1.DeadlineAt = k.DeadlineAt.Add(time.Minute)
	inboxB.now = func() time.Time { return k.DeadlineAt.Add(time.Millisecond) }
	r1, err := inboxB.Reserve(ctx, k1)
	if err != nil || r1.Duplicate || r1.Token != reservation.Token+1 {
		t.Fatalf("k+1 reserve after k's deadline: %+v err=%v, want it to take over with token %d", r1, err, reservation.Token+1)
	}
	if err := projector.Project(ctx, committer.record); err != nil {
		t.Fatal(err)
	}
	var marker bson.M
	if err := client.Database(database).Collection(deengine.TransactionCollection).FindOne(ctx, bson.M{"_id": committer.record.ID.String()}, &marker); err != nil {
		t.Fatal(err)
	}
	if marker["skipped"] != true {
		t.Fatalf("attempt k was projected after k+1 took the operation over: marker=%v, want it skipped by the lease fence", marker)
	}
	if _, found, err := inboxA.Replay(ctx, k); err != nil || found {
		t.Fatalf("receipt of the fenced attempt k: found=%v err=%v, want none", found, err)
	}
}
