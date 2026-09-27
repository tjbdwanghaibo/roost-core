package engine

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"

	fmongo "github.com/tjbdwanghaibo/roost-core/mongo"
	"github.com/tjbdwanghaibo/roost-core/mongo/mongotest"
)

// OPEN-ITEMS B15：RR-20260926-34 的“快照读没命中却撞键”分支（stageEffect）。
//
// 投影事务里 effect 先按 _id 做快照读，未命中才 InsertOne；两步之间若有并发写者插入同 _id（真实 Mongo 快照隔离下表现为
// WriteConflict，伪 Mongo 用注入模拟），InsertOne 得到 ErrDuplicateKey。承诺：这不是身份冲突，**不 fatal**——错误保留
// errors.Is(fmongo.ErrDuplicateKey)、不带 ErrTransactionIdentity / ErrProjectionConflict / ErrReceiptIdentity，整笔事务回滚
// （mutation、transaction marker 都不落库），由重试时的快照读裁决：同一事务身份视为已暂存（幂等成功），不同事务才是 ErrTransactionIdentity。
func TestStageEffectDuplicateAfterSnapshotMissIsRetryable(t *testing.T) {
	for _, tc := range []struct {
		name       string
		concurrent string // 并发写者暂存的 effect 属于哪个事务
		retry      func(t *testing.T, err error)
	}{
		{name: "same transaction identity", retry: func(t *testing.T, err error) {
			if err != nil {
				t.Fatalf("retry after the concurrent stage of the same effect = %v, want idempotent success", err)
			}
		}},
		{name: "other transaction", concurrent: "other-transaction", retry: func(t *testing.T, err error) {
			if !errors.Is(err, ErrTransactionIdentity) {
				t.Fatalf("retry after another transaction staged the same effect id = %v, want ErrTransactionIdentity", err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := mongotest.NewClient()
			racing := &outboxInsertRace{Client: client}
			store, err := NewMongoStore(racing, MongoStoreConfig{DefaultDatabase: testDatabase, ServerID: 3})
			if err != nil {
				t.Fatal(err)
			}
			record := projectorRecord(1, true)
			record.Mutations[0].Key.Database = testDatabase
			txID := record.ID.String()
			owner := txID
			if tc.concurrent != "" {
				owner = tc.concurrent
			}
			concurrentDoc := bson.M{"_id": record.Effects[0].ID, "effect_id": record.Effects[0].ID, "transaction_id": owner, "topic": record.Effects[0].Topic}
			racing.inject.Store(&concurrentDoc)

			err = store.Project(context.Background(), record)
			if !errors.Is(err, fmongo.ErrDuplicateKey) {
				t.Fatalf("Project = %v, want the duplicate key from the racing insert", err)
			}
			if errors.Is(err, ErrTransactionIdentity) || errors.Is(err, ErrProjectionConflict) || errors.Is(err, ErrReceiptIdentity) {
				t.Fatalf("Project = %v: a duplicate after a snapshot miss must not be classified as fatal", err)
			}
			if racing.injected.Load() != 1 {
				t.Fatalf("premise: racing insert injected %d times, want 1", racing.injected.Load())
			}
			// 整笔回滚：mutation 与 marker 都没有落库。
			if _, ok := client.Collection(testDatabase, "heroes").Lookup(int64(1)); ok {
				t.Fatal("mutation of the aborted projection transaction is visible")
			}
			if markerCollection(client).Len() != 0 {
				t.Fatal("transaction marker of the aborted projection transaction is visible")
			}

			// 并发写者已提交（伪 Mongo 的回滚是全局快照恢复，会一并抹掉注入的文档，这里按已提交重新放回），重试由快照读裁决。
			if err := client.Collection(testDatabase, OutboxCollection).Seed(concurrentDoc); err != nil {
				t.Fatal(err)
			}
			err = store.Project(context.Background(), record)
			tc.retry(t, err)
			if err == nil {
				if _, ok := client.Collection(testDatabase, "heroes").Lookup(int64(1)); !ok {
					t.Fatal("retry succeeded without applying the mutation")
				}
				if n := client.Collection(testDatabase, OutboxCollection).Len(); n != 1 {
					t.Fatalf("outbox holds %d documents after the idempotent retry, want 1", n)
				}
			}
		})
	}
}

// outboxInsertRace 在 outbox 集合的 FindOne 未命中之后、返回之前，按注入的文档直接写入同 _id（绕过事务），
// 让随后的 InsertOne 撞键；只注入一次。
type outboxInsertRace struct {
	*mongotest.Client
	inject   atomic.Pointer[bson.M]
	injected atomic.Int32
}

func (m *outboxInsertRace) Database(name string) fmongo.IDatabase {
	return racingDatabase{IDatabase: m.Client.Database(name), race: m, name: name}
}

type racingDatabase struct {
	fmongo.IDatabase
	race *outboxInsertRace
	name string
}

func (d racingDatabase) Collection(name string) fmongo.ICollection {
	coll := d.IDatabase.Collection(name)
	if name != OutboxCollection {
		return coll
	}
	return racingCollection{ICollection: coll, race: d.race, raw: d.race.Client.Collection(d.name, name)}
}

type racingCollection struct {
	fmongo.ICollection
	race *outboxInsertRace
	raw  *mongotest.Collection
}

func (c racingCollection) FindOne(ctx context.Context, filter any, result any) error {
	err := c.ICollection.FindOne(ctx, filter, result)
	if errors.Is(err, fmongo.ErrNotFound) {
		if doc := c.race.inject.Swap(nil); doc != nil {
			if seedErr := c.raw.Seed(*doc); seedErr != nil {
				return seedErr
			}
			c.race.injected.Add(1)
		}
	}
	return err
}
