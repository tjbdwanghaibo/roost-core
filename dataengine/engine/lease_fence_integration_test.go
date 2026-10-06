//go:build integration

package engine

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	coredata "github.com/tjbdwanghaibo/roost-core/dataengine"
	fmongo "github.com/tjbdwanghaibo/roost-core/mongo"
	"github.com/tjbdwanghaibo/roost-core/mongo/driver"
	corenest "github.com/tjbdwanghaibo/roost-core/nest"
	"github.com/tjbdwanghaibo/roost-core/saga"
	"go.mongodb.org/mongo-driver/v2/bson"
)

type capturingCommitter struct{ record coredata.CommitRecord }

func (committer *capturingCommitter) Commit(_ context.Context, record corenest.CommitRecord) error {
	committer.record = coredata.CloneCommitRecord(record)
	return nil
}

// isolatedRealMongo 连接隔离副本集并使用一次性库名；绝不写共享 game 库，结束时删库。
func isolatedRealMongo(t *testing.T) (context.Context, fmongo.IMongo, string) {
	t.Helper()
	uri := os.Getenv("ROOST_DATAENGINE_IT_MONGO_URI")
	if uri == "" {
		t.Skip("ROOST_DATAENGINE_IT_MONGO_URI is not set; run through kit/scripts/integration/dataengine-env.sh")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	client, err := driver.NewClient(fmongo.DefaultConfig(uri), driver.IndexMigrationPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	database := fmt.Sprintf("roost_rr30_it_%d_%d", os.Getpid(), time.Now().UnixNano())
	t.Cleanup(func() {
		_ = client.Database(database).Drop(context.Background())
		_ = client.Close(context.Background())
	})
	return ctx, client, database
}

// RR-20260926-30 §6 写偏斜：投影事务只读 claim、Reserve 的接管在另一事务里写 claim。
// 租约到期附近两者并发时，投影事务的快照读到租约有效，接管事务在到期后提交 token+1；
// 投影事务没写 claim，不产生写冲突，两边都提交——同一步骤被执行两次。
// 受控顺序：投影在租约校验通过之后、业务写入之前停在测试缝上，同步执行一次真实的
// DataEngineStepInbox.Reserve 接管（另一 owner），再让投影继续提交。
// 承诺：两者至多一个生效——投影落库时接管不能成功拿到新 token。
func TestRealMongoFencedProjectionSerializesWithLeaseTakeover(t *testing.T) {
	ctx, client, database := isolatedRealMongo(t)
	store, err := NewMongoStore(client, MongoStoreConfig{DefaultDatabase: database})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.EnsureInfrastructure(ctx); err != nil {
		t.Fatal(err)
	}
	owner, err := saga.NewDataEngineStepInbox(client, database, saga.DataEngineStepInboxOptions{Owner: "worker-1", LeaseDuration: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if err := owner.EnsureInfrastructure(ctx); err != nil {
		t.Fatal(err)
	}
	successor, err := saga.NewDataEngineStepInbox(client, database, saga.DataEngineStepInboxOptions{Owner: "worker-2", LeaseDuration: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	command := saga.Command{
		ID: "rr30-skew", IdempotencyKey: "rr30-skew-op", SagaID: "saga-rr30", SagaType: "gift", DefinitionVersion: 1,
		BusinessKey: "hero-7", StepName: "debit", Phase: saga.PhaseForward, Attempt: 1,
		Topic: "gift.debit", CreatedAt: now, DeadlineAt: now.Add(time.Hour),
	}
	reservation, err := owner.Reserve(ctx, command)
	if err != nil || reservation.Duplicate || reservation.Token != 1 {
		t.Fatalf("first reservation=%v err=%v", reservation, err)
	}

	// 正式 Bind + EmitCompletion 组成原生步骤的 fence、step receipt 与 completion effect，
	// 再附上本地 DAO 的 4 -> 5 修改。
	heroes := client.Database(database).Collection("heroes")
	if _, err := heroes.InsertOne(ctx, bson.M{"_id": int64(7), "_version": uint64(4), "level": int32(1)}); err != nil {
		t.Fatal(err)
	}
	committer := &capturingCommitter{}
	if _, err := corenest.RunIsolatedTransaction(ctx, committer, "rr30-debit", func() (any, error) {
		if err := owner.Bind(command, reservation); err != nil {
			return nil, err
		}
		return nil, saga.EmitCompletion(saga.Completion{CommandID: command.ID, IdempotencyKey: command.IdempotencyKey, SagaID: command.SagaID, Success: true})
	}); err != nil {
		t.Fatal(err)
	}
	record := committer.record
	patch, _ := bson.Marshal(bson.D{{Key: "level", Value: int32(5)}})
	record.Mutations = append(record.Mutations, coredata.Mutation{
		Key:  coredata.DocumentKey{Database: database, Resource: "heroes", ID: 7},
		Kind: coredata.MutationPatch, ExpectedVersion: 4, NextVersion: 5, Mask: 1, Schema: 1, Codec: "bson-v2",
		Patch: coredata.FieldPatch{SetBSON: patch},
	})

	// 租约边界：接管方（真实时钟）看到租约已过期；投影方的时钟仍在到期之前。
	// 原生步骤的操作状态文档（saga 收件箱，每个操作一份，_id = IdempotencyKey）。
	operations := client.Database(database).Collection("_dataengine_step_operations")
	if _, err := operations.UpdateOne(ctx, bson.M{"_id": command.IdempotencyKey}, bson.M{"$set": bson.M{"lease_until": now.Add(-time.Second)}}); err != nil {
		t.Fatal(err)
	}
	store.now = func() time.Time { return now.Add(-time.Minute) }

	var takeover saga.Reservation
	var takeoverErr error
	var once sync.Once
	store.afterLeaseFence = func(context.Context) {
		once.Do(func() {
			bounded, cancel := context.WithTimeout(ctx, 3*time.Second)
			defer cancel()
			takeover, takeoverErr = successor.Reserve(bounded, command)
		})
	}
	projectErr := store.Project(ctx, record)
	store.afterLeaseFence = nil

	var hero struct {
		Version uint64 `bson:"_version"`
	}
	if err := heroes.FindOne(ctx, bson.M{"_id": int64(7)}, &hero); err != nil {
		t.Fatal(err)
	}
	applied := projectErr == nil && hero.Version == 5
	tookOver := takeoverErr == nil && !takeover.Duplicate && takeover.Token > reservation.Token
	t.Logf("projection err=%v hero_version=%d; concurrent takeover=%v err=%v", projectErr, hero.Version, takeover, takeoverErr)
	if applied && tookOver {
		t.Fatalf("write skew: the fenced step was applied (version=%d) while a concurrent Reserve took the claim over (token=%d)", hero.Version, takeover.Token)
	}
	if !applied && !tookOver {
		t.Fatalf("neither side won: projection err=%v version=%d, takeover=%v err=%v", projectErr, hero.Version, takeover, takeoverErr)
	}
	if applied {
		// 投影先提交：之后的接管必须看到 receipt，只能得到已完成的重复投递。
		again, err := successor.Reserve(ctx, command)
		if err != nil || !again.Duplicate || again.Completion.CommandID != command.ID {
			t.Fatalf("reserve after the projection committed=%v err=%v, want duplicate with completion", again, err)
		}
	}
}
