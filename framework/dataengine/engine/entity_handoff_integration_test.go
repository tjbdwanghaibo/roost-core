//go:build integration

package engine

import (
	"context"
	"errors"
	"testing"
	"time"

	coredata "github.com/tjbdwanghaibo/roost-core/framework/dataengine"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	corenest "github.com/tjbdwanghaibo/roost-core/framework/nest"
	"github.com/tjbdwanghaibo/roost-core/framework/nestwal"
	fmongo "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// 交出实体在真实副本集上的两条：它依据的现状（TestUnconfirmedProjectionReplayedAfterAnotherWriterAdvancedTheDocument 的
// 真实 Mongo 版），以及经正式 Nest 与后台投影循环的整条流程。资源：ROOST_DATAENGINE_IT_MONGO_URI 指向的副本集，
// 只用 isolatedRealMongo 给的一次性库，用后删除。

type realHero struct {
	Version uint64 `bson:"_version"`
	Level   int32  `bson:"level"`
	LastTx  string `bson:"_last_tx"`
}

func readRealHero(t *testing.T, ctx context.Context, heroes fmongo.ICollection, id int64) realHero {
	t.Helper()
	var doc realHero
	if err := heroes.FindOne(ctx, bson.M{"_id": id}, &doc); err != nil {
		t.Fatal(err)
	}
	return doc
}

// 真实 Mongo 上的同一个现状：单 mutation 快路记录已投影、checkpoint 丢失，另一个写者推进文档之后，重启后的启动回放
// 得到 fatal 的 ErrProjectionConflict；带回执的记录（写事务标记）在同样的情形下重放安全。
func TestRealMongoUnconfirmedProjectionReplayedAfterAnotherWriterAdvancedTheDocument(t *testing.T) {
	for _, tc := range []struct {
		name      string
		marked    bool
		wantFatal bool
	}{
		{name: "fast_path_record_is_fatal", wantFatal: true},
		{name: "record_with_a_transaction_marker_replays_safely", marked: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, client, database := isolatedRealMongo(t)
			store, err := NewMongoStore(client, MongoStoreConfig{Databases: identityDatabases(t), DefaultDatabase: database})
			if err != nil {
				t.Fatal(err)
			}
			if err := store.EnsureInfrastructure(ctx); err != nil {
				t.Fatal(err)
			}
			heroes := client.Database(database).Collection("heroes")
			if _, err := heroes.InsertOne(ctx, bson.M{"_id": int64(7), "_version": uint64(4), "level": int32(1)}); err != nil {
				t.Fatal(err)
			}
			record := testMutationRecord(coredata.MutationPatch)
			record.Mutations[0].Key.Database = database
			if tc.marked {
				record.Receipts = []coredata.Receipt{{Namespace: "saga-step", ID: "step-1", Digest: []byte{2}}}
			}
			if projectsOnFastPath(record) == tc.marked {
				t.Fatalf("premise: fast path=%v for marked=%v", projectsOnFastPath(record), tc.marked)
			}

			walOptions := nestwal.DefaultOptions(t.TempDir())
			open := func() (*nestwal.WAL, *Projector) {
				wal, err := nestwal.Open(walOptions)
				if err != nil {
					t.Fatal(err)
				}
				projector, err := NewProjector(wal, store, ProjectorOptions{ManualReplay: true, CloseWAL: true})
				if err != nil {
					t.Fatal(err)
				}
				return wal, projector
			}
			_, projector := open()
			if err := projector.Commit(ctx, record); err != nil {
				t.Fatal(err)
			}
			projector.TransactionReleased(record.ID)
			lost := errors.New("process exited before the checkpoint reached the disk")
			projector.OverrideAck(func(context.Context, corenest.CommitFence) error { return lost })
			if err := projector.Flush(ctx); !errors.Is(err, lost) {
				t.Fatalf("flush with a lost checkpoint = %v", err)
			}
			if got := readRealHero(t, ctx, heroes, 7); got.Version != 5 || got.Level != 5 || got.LastTx != record.ID.String() {
				t.Fatalf("premise: the record must be projected, stored=%+v", got)
			}
			if err := projector.WaitEntityProjection(ctx, 7); err != nil {
				t.Fatalf("premise: the entity's projection wait must already pass, got %v", err)
			}
			if err := projector.Close(ctx); err != nil {
				t.Fatal(err)
			}

			// 另一个进程接手，写下一版。
			result, err := heroes.UpdateOne(ctx, bson.M{"_id": int64(7), "_version": uint64(5)},
				bson.M{"$set": bson.M{"_version": uint64(6), "_last_tx": "another-process", "level": int32(9)}})
			if err != nil || result.MatchedCount != 1 {
				t.Fatalf("another writer's update: %+v, %v", result, err)
			}

			_, restarted := open()
			defer restarted.Close(ctx)
			err = restarted.Flush(ctx)
			if tc.wantFatal {
				if !errors.Is(err, ErrProjectionConflict) || restarted.fatal() == nil {
					t.Fatalf("startup replay after another writer advanced the document = %v (fatal=%v), want a fatal ErrProjectionConflict", err, restarted.fatal())
				}
			} else if err != nil || restarted.fatal() != nil {
				t.Fatalf("startup replay of a marked record = %v (fatal=%v), want nil", err, restarted.fatal())
			}
			if got := readRealHero(t, ctx, heroes, 7); got.Version != 6 || got.Level != 9 {
				t.Fatalf("the replay touched the other writer's version: %+v", got)
			}
		})
	}
}

// 整条流程，真实 Mongo、真实文件 WAL、后台投影循环、正式 Nest：交出返回时本进程的写入都在库里且已被 checkpoint 确认；
// 之后写入被拒；另一个写者推进文档，本进程的投影器不受影响（没有可重放的记录，不 fatal）；Release 之后冷加载读到
// 另一个写者的版本，在它之上继续写。
func TestRealMongoEntityHandOffThenAnotherWriterThenRelease(t *testing.T) {
	ensureUnloadKinds()
	ctx, client, database := isolatedRealMongo(t)
	// unloadHeroDAO 的逻辑库是 "game"：映射到一次性物理库，不碰共享的 game 库。
	databases, _, err := coredata.NewDatabaseMap(coredata.DatabaseMapConfig{
		Declared: []coredata.LogicalDatabase{{Name: testDatabase}}, Configured: map[string]string{testDatabase: database},
	})
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewMongoStore(client, MongoStoreConfig{Databases: databases, DefaultDatabase: database})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.EnsureInfrastructure(ctx); err != nil {
		t.Fatal(err)
	}
	id, err := entity.BuildEntityID(9301, unloadTestKind)
	if err != nil {
		t.Fatal(err)
	}
	heroes := client.Database(database).Collection(unloadTestCollection)
	if _, err := heroes.InsertOne(ctx, bson.M{"_id": id, "_version": uint64(4), "_schema": uint32(1), "level": int32(1)}); err != nil {
		t.Fatal(err)
	}
	wal, err := nestwal.Open(nestwal.DefaultOptions(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = wal.Close(context.Background()) })
	projector, err := NewProjector(wal, store, ProjectorOptions{CloseWAL: false})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = projector.Close(context.Background()) })
	f := &unloadFixture{t: t, ctx: ctx, id: id, projector: projector, manager: entity.NewEntityManager()}
	f.access = entity.NewManagerAccess(f.manager)
	repository, err := NewEntityRepository(f.manager, store, projectionGate{projector})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.access.ConfigureLoader(repository); err != nil {
		t.Fatal(err)
	}
	f.runtime = &Runtime{Store: store, Projector: projector, Repository: repository, access: f.access}
	f.runtime.ready.Store(true)
	f.scheduler = corenest.NewEngine(corenest.NestOptionWithTransactionCommitter(projector), corenest.NestOptionWithGetter(f.access),
		corenest.NestOptionWithWorkerPools(corenest.WorkerPoolConfig{Workers: 4, QueueCap: 64}, corenest.WorkerPoolConfig{}, corenest.WorkerPoolConfig{}))
	f.scheduler.MustRegisterHandlerWithMeta(corenest.NewHandlerName("unload_level_up"), f.levelUp, corenest.HandlerMeta{Rollback: corenest.RollbackUndo, Durability: corenest.DurabilityStrict})
	if err := f.scheduler.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.scheduler.Shutdown(context.Background()) })

	for range 3 {
		if err := f.levelUpRequest(); err != nil {
			t.Fatal(err)
		}
	}
	handOffCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	handOff, err := f.runtime.HandOffEntity(handOffCtx, id)
	if err != nil || handOff == nil || !handOff.Resident || !handOff.Unloaded {
		t.Fatalf("HandOffEntity = %+v, %v", handOff, err)
	}
	if got := readRealHero(t, ctx, heroes, id); got.Version != 7 || got.Level != 4 {
		t.Fatalf("stored when HandOffEntity returned = %+v, want level 4 at version 7", got)
	}
	if stats := projector.Stats(); stats.WALUnacked != 0 || stats.HandedOffEntities != 1 {
		t.Fatalf("stats when HandOffEntity returned = %+v, want every record confirmed by the checkpoint", stats)
	}
	if pending, err := wal.HasUnacknowledgedRecords(ctx); err != nil || pending {
		t.Fatalf("WAL still has unacknowledged records after the hand-off: %v, %v", pending, err)
	}
	if err := f.levelUpRequest(); !refusedAsHandedOff(err) {
		t.Fatalf("write after the hand-off = %v, want ErrEntityHandedOff", err)
	}

	// 另一个进程接手：读到版本 7，写到版本 8。
	result, err := heroes.UpdateOne(ctx, bson.M{"_id": id, "_version": uint64(7)},
		bson.M{"$set": bson.M{"_version": uint64(8), "_last_tx": "another-process", "level": int32(100)}})
	if err != nil || result.MatchedCount != 1 {
		t.Fatalf("another writer's update: %+v, %v", result, err)
	}
	time.Sleep(200 * time.Millisecond) // 后台投影循环若还有这个实体的记录可重放，此时已经 fatal
	if err := projector.fatal(); err != nil {
		t.Fatalf("the projector fenced after another writer advanced the handed-off document: %v", err)
	}
	if stats := projector.Stats(); stats.FatalProjectionConflicts != 0 || stats.ProjectionFailures != 0 {
		t.Fatalf("stats after another writer advanced the document = %+v", stats)
	}

	// 实体回到本进程：Release 卸载交出期间被那次写入加载回来的实例（它停在版本 7），之后冷加载读到版本 8。
	if err := handOff.Release(handOffCtx); err != nil {
		t.Fatalf("Release = %v", err)
	}
	if err := f.levelUpRequest(); err != nil {
		t.Fatalf("write after Release: %v", err)
	}
	if err := projector.WaitEntityProjection(handOffCtx, id); err != nil {
		t.Fatal(err)
	}
	if got := readRealHero(t, ctx, heroes, id); got.Version != 9 || got.Level != 101 {
		t.Fatalf("stored after the write that followed Release = %+v, want level 101 at version 9", got)
	}
	if err := projector.fatal(); err != nil {
		t.Fatalf("the projector fenced: %v", err)
	}
}
