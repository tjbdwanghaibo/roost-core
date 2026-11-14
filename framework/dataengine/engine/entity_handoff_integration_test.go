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
// newRealMongoUnloadFixture 是 newUnloadFixtureOfKind 的真实副本集版：一次性库里 level=1、_version=4，正式 Nest、
// 文件 WAL、后台投影循环与共享冷加载；实体还没有加载。返回 fixture、DAO 集合与 WAL。
func newRealMongoUnloadFixture(t *testing.T, uniqueID int64, kind entity.EntityKind) (*unloadFixture, fmongo.ICollection, *nestwal.WAL) {
	t.Helper()
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
	id, err := entity.BuildEntityID(uniqueID, kind)
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
	return f, heroes, wal
}

func TestRealMongoEntityHandOffThenAnotherWriterThenRelease(t *testing.T) {
	f, heroes, wal := newRealMongoUnloadFixture(t, 9301, unloadTestKind)
	ctx, id, projector := f.ctx, f.id, f.projector
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

// 常驻实体（lifetime=resident）的强制刷新与换服中止后的载回，在真实副本集上走一遍（KNOWN-LIMITS“常驻的刷新与载回只有
// 替身存储的用例”；替身版是 TestResidentEntityUnloadIsRefusedAndForceRefreshesIt、TestResidentEntityIsHandedOffAndReloadedOnRelease）：
//   - 运维卸载被拒（entity.ErrEntityResident），实例不动；
//   - 库里的文档被人工修过之后 RefreshEntity：新实例就是库里的状态，之后的写从那个版本接着写；
//   - HandOffEntity 交出后实体不在内存、写被拒；交出期间另一个写者推进了文档；Release（换服中止）立刻把它载回内存
//     （不等第一次访问），载回的就是另一个写者的版本，之后的写接着写、投影器不 fence。
func TestRealMongoResidentEntityRefreshAndReloadAfterAbortedHandOff(t *testing.T) {
	f, heroes, wal := newRealMongoUnloadFixture(t, 9331, unloadResidentKind)
	ctx, id, projector := f.ctx, f.id, f.projector
	if _, err := f.access.Get(ctx, id, entity.EntityCategoryNone); err != nil || f.manager.Get(id) == nil {
		t.Fatalf("initial load: %v", err)
	}
	for range 2 {
		if err := f.levelUpRequest(); err != nil {
			t.Fatal(err)
		}
	}
	if err := projector.WaitEntityProjection(ctx, id); err != nil {
		t.Fatal(err)
	}
	if got := readRealHero(t, ctx, heroes, id); got.Version != 6 || got.Level != 3 {
		t.Fatalf("stored after two writes = %+v, want level 3 at version 6", got)
	}
	before := f.manager.Get(id)
	if got := receiveUnload(t, f.unloadAsync(5*time.Second)); !errors.Is(got.err, entity.ErrEntityResident) || f.manager.Get(id) != before {
		t.Fatalf("UnloadEntity on a resident entity = %+v, %v; same instance kept: %v; want entity.ErrEntityResident", got.result, got.err, f.manager.Get(id) == before)
	}

	// 人工修库：版本 6 → 7、level 50。强制刷新把内存换成库里的状态。
	if result, err := heroes.UpdateOne(ctx, bson.M{"_id": id, "_version": uint64(6)},
		bson.M{"$set": bson.M{"_version": uint64(7), "_last_tx": "operator-fix", "level": int32(50)}}); err != nil || result.MatchedCount != 1 {
		t.Fatalf("operator fix: %+v, %v", result, err)
	}
	refreshCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	refreshed, err := f.runtime.RefreshEntity(refreshCtx, id)
	if err != nil || !refreshed.Reloaded {
		t.Fatalf("RefreshEntity = %+v, %v; want the entity reloaded", refreshed, err)
	}
	after := f.manager.Get(id)
	if after == nil || after == before {
		t.Fatalf("after RefreshEntity loaded=%v new instance=%v", after != nil, after != before)
	}
	if level, version, ok := f.heroState(); !ok || level != 50 || version != 7 {
		t.Fatalf("refreshed instance level=%d version=%d loaded=%v, want the operator's 50 at version 7", level, version, ok)
	}
	if err := f.levelUpRequest(); err != nil {
		t.Fatalf("write after the refresh: %v", err)
	}
	if err := projector.WaitEntityProjection(ctx, id); err != nil {
		t.Fatal(err)
	}
	if got := readRealHero(t, ctx, heroes, id); got.Version != 8 || got.Level != 51 {
		t.Fatalf("stored after the write that followed the refresh = %+v, want level 51 at version 8", got)
	}

	// 换服：交出后不在内存、写被拒；目标服写到版本 9；换服中止，Release 立刻载回目标服写下的状态。
	handOff, err := f.runtime.HandOffEntity(refreshCtx, id)
	if err != nil || handOff == nil || !handOff.Resident || !handOff.Unloaded {
		t.Fatalf("HandOffEntity on a resident entity = %+v, %v; want it handed off and unloaded", handOff, err)
	}
	if f.manager.Get(id) != nil {
		t.Fatal("the resident entity is still in memory after the hand-off")
	}
	if pending, err := wal.HasUnacknowledgedRecords(ctx); err != nil || pending {
		t.Fatalf("WAL still has unacknowledged records after the hand-off: %v, %v", pending, err)
	}
	if err := f.levelUpRequest(); !refusedAsHandedOff(err) {
		t.Fatalf("write after the hand-off = %v, want ErrEntityHandedOff", err)
	}
	if result, err := heroes.UpdateOne(ctx, bson.M{"_id": id, "_version": uint64(8)},
		bson.M{"$set": bson.M{"_version": uint64(9), "_last_tx": "target-server", "level": int32(100)}}); err != nil || result.MatchedCount != 1 {
		t.Fatalf("target server's write: %+v, %v", result, err)
	}
	if err := handOff.Release(refreshCtx); err != nil {
		t.Fatalf("Release = %v", err)
	}
	if level, version, ok := f.heroState(); !ok || level != 100 || version != 9 {
		t.Fatalf("after Release the resident entity level=%d version=%d loaded=%v, want it back in memory at the target's 100 / version 9 before any access", level, version, ok)
	}
	if stats := projector.Stats(); stats.HandedOffEntities != 0 || stats.UnloadingEntities != 0 {
		t.Fatalf("barriers after Release = %+v", stats)
	}
	if err := f.levelUpRequest(); err != nil {
		t.Fatalf("write after Release: %v", err)
	}
	if err := projector.WaitEntityProjection(ctx, id); err != nil {
		t.Fatal(err)
	}
	if got := readRealHero(t, ctx, heroes, id); got.Version != 10 || got.Level != 101 {
		t.Fatalf("stored after the write that followed Release = %+v, want level 101 at version 10", got)
	}
	if err := projector.fatal(); err != nil {
		t.Fatalf("the projector fenced: %v", err)
	}
	if stats := projector.Stats(); stats.FatalProjectionConflicts != 0 || stats.ProjectionFailures != 0 {
		t.Fatalf("projector stats = %+v", stats)
	}
}
