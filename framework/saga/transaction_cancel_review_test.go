package saga

import (
	"context"
	"errors"
	"testing"

	fmongo "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo"
	"github.com/tjbdwanghaibo/roost-core/infra/storage/mongo/mongotest"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// N06 review：在事务写到指定阶段后确定性取消，检查无半写，并用新 ctx 重试到最终收敛。
// 替身事务模型只能证明框架使用同一个 txCtx，不能证明真实 Mongo 未知提交/HA。
type sagaCancelWriteClient struct {
	fmongo.IMongo
	collection string
	method     string
	cancel     context.CancelFunc
	fired      bool
}

func (c *sagaCancelWriteClient) Database(name string) fmongo.IDatabase {
	return sagaCancelWriteDatabase{IDatabase: c.IMongo.Database(name), client: c}
}

type sagaCancelWriteDatabase struct {
	fmongo.IDatabase
	client *sagaCancelWriteClient
}

func (d sagaCancelWriteDatabase) Collection(name string) fmongo.ICollection {
	return sagaCancelWriteCollection{ICollection: d.IDatabase.Collection(name), name: name, client: d.client}
}

type sagaCancelWriteCollection struct {
	fmongo.ICollection
	name   string
	client *sagaCancelWriteClient
}

func (c sagaCancelWriteCollection) afterWrite(method string, err error) {
	if err == nil && c.client.cancel != nil && c.name == c.client.collection && method == c.client.method {
		c.client.fired = true
		c.client.cancel()
		c.client.cancel = nil
	}
}
func (c sagaCancelWriteCollection) InsertOne(ctx context.Context, doc any) (string, error) {
	id, err := c.ICollection.InsertOne(ctx, doc)
	c.afterWrite("InsertOne", err)
	return id, err
}
func (c sagaCancelWriteCollection) ReplaceOne(ctx context.Context, filter, replacement any) (*fmongo.UpdateResult, error) {
	result, err := c.ICollection.ReplaceOne(ctx, filter, replacement)
	c.afterWrite("ReplaceOne", err)
	return result, err
}

func TestSagaCompletionTransactionCancellationAndRetry(t *testing.T) {
	for _, stage := range []string{"state", "receipt", "operation", "callback_retry"} {
		t.Run(stage, func(t *testing.T) {
			ctx := context.Background()
			backend := mongotest.NewClient()
			client := &sagaCancelWriteClient{IMongo: backend}
			store, err := NewMongoStore(client, MongoStoreOptions{Database: "cancel_review"})
			if err != nil {
				t.Fatal(err)
			}
			if err := store.EnsureInfrastructure(ctx); err != nil {
				t.Fatal(err)
			}
			e, err := NewEngine(store, PublishFunc(func(context.Context, Command) error { return nil }), DefaultOptions())
			if err != nil {
				t.Fatal(err)
			}
			if err := e.Register(testDefinition()); err != nil {
				t.Fatal(err)
			}
			r, err := e.StartSaga(ctx, StartRequest{ID: "cancel-review", Type: "rally", BusinessKey: "cancel-review", DefinitionVersion: 1, Data: []byte("initial")})
			if err != nil {
				t.Fatal(err)
			}
			if err := e.processClaimed(ctx, r, r.CreatedAt); err != nil {
				t.Fatal(err)
			}
			before, err := store.Get(ctx, r.ID)
			if err != nil {
				t.Fatal(err)
			}
			completion := Completion{SagaID: r.ID, CommandID: before.CommandID, IdempotencyKey: before.OperationKey, Success: true, Data: []byte("step-result")}
			if stage == "callback_retry" {
				backend.TransientRetries = 1
			} else {
				cancelCtx, cancel := context.WithCancel(ctx)
				defer cancel()
				client.cancel = cancel
				switch stage {
				case "state":
					client.collection, client.method = defaultSagaCollection, "ReplaceOne"
				case "receipt":
					client.collection, client.method = defaultCompletionCollection, "InsertOne"
				case "operation":
					client.collection, client.method = defaultOperationCollection, "InsertOne"
				}
				if _, err := e.Complete(cancelCtx, completion); !errors.Is(err, context.Canceled) {
					t.Fatalf("cancelled completion=%v", err)
				}
				if !client.fired {
					t.Fatal("cancellation stage never executed")
				}
				after, err := store.Get(ctx, r.ID)
				if err != nil || after.Version != before.Version || after.Status != StatusWaiting || string(after.Data) != "initial" || after.StartDigest != r.StartDigest {
					t.Fatalf("partial saga write=%+v err=%v", after, err)
				}
				if recorded, err := store.CompletionRecorded(ctx, completion); err != nil || recorded {
					t.Fatalf("partial receipt: recorded=%t err=%v", recorded, err)
				}
				if count, err := store.outbox().CountDocuments(ctx, bson.M{}); err != nil || count != 1 {
					t.Fatalf("outbox prematurely removed: count=%d err=%v", count, err)
				}
			}
			attempts := backend.Attempts()
			for i := 0; i < 2; i++ {
				after, err := e.Complete(ctx, completion)
				if err != nil || after.Version != before.Version+1 || after.Status != StatusPending || string(after.Data) != "step-result" || after.StartDigest != r.StartDigest {
					t.Fatalf("retry/duplicate=%+v err=%v", after, err)
				}
			}
			if stage == "callback_retry" && backend.Attempts()-attempts < 2 {
				t.Fatal("callback retry did not run")
			}
			if count, err := store.outbox().CountDocuments(ctx, bson.M{}); err != nil || count != 0 {
				t.Fatalf("outbox not settled: count=%d err=%v", count, err)
			}
		})
	}
}

func TestMongoCommandInboxCancelledBusinessWriteCanRetryAtomically(t *testing.T) {
	ctx := context.Background()
	client := mongotest.NewClient()
	inbox, err := NewMongoCommandInbox(client, "inbox_cancel", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := inbox.EnsureInfrastructure(ctx); err != nil {
		t.Fatal(err)
	}
	command := dataEngineCommand("cancel-command", "cancel-operation", "initial")
	business := client.Database("inbox_cancel").Collection("business")
	cancelCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	handlerCalls := 0
	handler := func(txCtx context.Context, _ Command) (Completion, error) {
		handlerCalls++
		if _, err := business.InsertOne(txCtx, bson.M{"_id": "award", "value": 42}); err != nil {
			return Completion{}, err
		}
		if handlerCalls == 1 {
			cancel()
		}
		return Completion{Success: true}, nil
	}
	if _, _, err := inbox.Handle(cancelCtx, command, handler); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled handler=%v", err)
	}
	if handlerCalls != 1 {
		t.Fatal("business cancellation not reached")
	}
	if count, err := business.CountDocuments(ctx, bson.M{}); err != nil || count != 0 {
		t.Fatalf("business escaped transaction: count=%d err=%v", count, err)
	}
	if _, found, err := inbox.Replay(ctx, command); err != nil || found {
		t.Fatalf("aborted receipt visible: found=%t err=%v", found, err)
	}
	for i := 0; i < 2; i++ {
		completion, duplicate, err := inbox.Handle(ctx, command, handler)
		if err != nil || !completion.Success || duplicate != (i == 1) {
			t.Fatalf("retry/duplicate=%+v duplicate=%t err=%v", completion, duplicate, err)
		}
	}
	if handlerCalls != 2 {
		t.Fatalf("business replayed after commit: calls=%d", handlerCalls)
	}
	if count, err := business.CountDocuments(ctx, bson.M{}); err != nil || count != 1 {
		t.Fatalf("business retry count=%d err=%v", count, err)
	}
}
