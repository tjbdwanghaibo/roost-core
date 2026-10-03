package player

import (
	"context"
	"errors"
	guild "example.com/planet/game/entities/guild"
	"fmt"
	coredata "github.com/tjbdwanghaibo/roost-core/dataengine"
	"github.com/tjbdwanghaibo/roost-core/dataengine/engine"
	fmongo "github.com/tjbdwanghaibo/roost-core/mongo"
	"github.com/tjbdwanghaibo/roost-core/mongo/driver"
	"github.com/tjbdwanghaibo/roost-core/nest"
	"github.com/tjbdwanghaibo/roost-core/nestwal"
	"github.com/tjbdwanghaibo/roost-core/remoteentity"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/entity"
	"github.com/tjbdwanghaibo/roost-core/mongo/mongotest"
	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestGuildIDsSurviveIncarnationsAndConcurrentCreators(t *testing.T) {
	client := mongotest.NewClient()
	seen := make(map[int64]bool)
	var mu sync.Mutex
	for range 3 {
		// Recreate every controller, retaining only the authoritative store.
		var wg sync.WaitGroup
		for range 8 {
			controller := &Controller{serverSID: 1000, guildIDs: client.Database("game").Collection("_guild_id_sequence")}
			wg.Go(func() {
				for range 16 {
					id, err := controller.nextGuildID(context.Background())
					if err != nil {
						t.Error(err)
						return
					}
					mu.Lock()
					if seen[id] || id <= 0 || uint64(id) >= entity.StaticUniqueIDBase {
						t.Errorf("invalid or reused guild id %d", id)
					}
					seen[id] = true
					mu.Unlock()
				}
			})
		}
		wg.Wait()
	}
	if len(seen) != 384 {
		t.Fatalf("unique IDs=%d, want 384", len(seen))
	}
}

// RR-20260926-22（复核残留）：号段用尽要报“超出 dynamic 半区”，不能回绕、不能退回
// 进程内 RuntimeIDs。只断言 err != nil 分不出修前修后——修前的范围过滤在上限处匹配不到
// 计数文档，upsert 去插第二份 _id=guild，得到的是 duplicate key，同样非 nil。
// 名字以 TestGuildIDs 开头，CI 的 -run '^TestGuildIDs' 才覆盖得到。
func TestGuildIDsExhaustionDoesNotWrapOrUseRuntimeIDs(t *testing.T) {
	coll := mongotest.NewClient().Collection("game", "_guild_id_sequence")
	if err := coll.Seed(bson.M{"_id": "guild", "next": int64(entity.StaticUniqueIDBase - 2)}); err != nil {
		t.Fatal(err)
	}
	controller := &Controller{guildIDs: coll}
	last, err := controller.nextGuildID(context.Background())
	if err != nil || uint64(last) != entity.StaticUniqueIDBase-1 {
		t.Fatalf("the last dynamic id: id=%d err=%v, want %d", last, err, entity.StaticUniqueIDBase-1)
	}
	for range 2 {
		id, err := controller.nextGuildID(context.Background())
		if id != 0 || err == nil || !strings.Contains(err.Error(), "outside dynamic range") || strings.Contains(err.Error(), "duplicate key") {
			t.Fatalf("past the last dynamic id: id=%d err=%v, want the exhausted-range error", id, err)
		}
	}
	if id, err := (&Controller{}).nextGuildID(context.Background()); err == nil || id != 0 || !strings.Contains(err.Error(), "persistent counter is unavailable") {
		t.Fatalf("unconfigured id=%d err=%v", id, err)
	}
}

// Three incarnations share only Mongo and the WAL directory. The second
// intentionally loses its acknowledgement; the third must replay it safely.
func TestGuildIDsRealMongoWALRestart(t *testing.T) {
	uri := os.Getenv("ROOST_GUILD_IT_MONGO_URI")
	if uri == "" {
		t.Skip("set ROOST_GUILD_IT_MONGO_URI to an isolated replica set")
	}
	entity.MustRegisterEntityKindDefs(entity.EntityKindDef{Kind: guild.EntityKindGuild, Category: entity.EntityCategoryRemote, RemotePolicy: entity.RemotePolicyManaged})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client, err := driver.NewClient(fmongo.DefaultConfig(uri), driver.IndexMigrationPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close(context.Background())
	database := fmt.Sprintf("roost_guild_it_%d_%d", os.Getpid(), time.Now().UnixNano())
	defer client.Database(database).Drop(context.Background())
	remote := remoteentity.NewMongoCommitter(client, database, 1000, time.Hour)
	if err := remote.EnsureRemoteStorage(ctx); err != nil {
		t.Fatal(err)
	}
	store, err := engine.NewMongoStore(client, engine.MongoStoreConfig{DefaultDatabase: database, ServerID: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.EnsureInfrastructure(ctx); err != nil {
		t.Fatal(err)
	}
	if err := store.SetRemoteProjection(remote, guildReplayApplier{remote}); err != nil {
		t.Fatal(err)
	}
	opts := nestwal.DefaultOptions(t.TempDir())
	opts.WriterVersion = nestwal.WriterVersionV2
	seen := map[int64]bool{}
	var created []coredata.CommitRecord
	for incarnation := byte(1); incarnation <= 3; incarnation++ {
		controller := &Controller{serverSID: 1000, guildIDs: client.Database(database).Collection("_guild_id_sequence")}
		wal, err := nestwal.Open(opts)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { wal.Close(context.Background()) })
		replayed := 0
		var last nest.CommitFence
		err = wal.Replay(ctx, func(f nest.CommitFence, r nest.CommitRecord) error {
			replayed++
			last = f
			return store.Project(ctx, r)
		})
		if err != nil {
			wal.Close(ctx)
			t.Fatal(err)
		}
		if incarnation == 3 && replayed != 1 {
			t.Fatalf("replayed=%d, want lost-ack transaction", replayed)
		}
		if replayed > 0 {
			if err := wal.Ack(ctx, last); err != nil {
				t.Fatal(err)
			}
		}
		unique, err := controller.nextGuildID(ctx)
		if err != nil {
			t.Fatal(err)
		}
		id, err := entity.BuildEntityID(unique, guild.EntityKindGuild)
		if err != nil {
			t.Fatal(err)
		}
		if seen[id] {
			t.Fatalf("reused persistent entity %d", id)
		}
		seen[id] = true
		tx := coredata.TransactionID{}
		tx[0] = incarnation
		tx[15] = 1
		data, err := bson.Marshal(bson.M{"_id": id, "name": fmt.Sprintf("incarnation-%d", incarnation)})
		if err != nil {
			t.Fatal(err)
		}
		// 与正式 Remote 准入一致：先取得 Mongo 权威许可，不能伪造 fence=0 的写入。
		if _, err := remote.ClaimOwnership(ctx, id, 1000); err != nil {
			t.Fatal(err)
		}
		grant, err := remote.GrantWrite(ctx, id, fmt.Sprintf("incarnation-%d", incarnation), 1000)
		if err != nil {
			t.Fatal(err)
		}
		commit := entity.RemoteCommit{
			TransactionID: entity.RemoteTransactionID(tx), EntityID: id, Kind: guild.EntityKindGuild,
			BaseVersion: 0, NextVersion: 1, MarkerEpoch: grant.Ownership.MarkerEpoch, RouteEpoch: grant.Ownership.RouteEpoch, LockFence: grant.Fence, Schema: 1, Codec: 1,
			Mutations: []entity.RemoteDataMutation{{
				Database: database, Collection: "guild", ID: id, Version: 1, Data: data,
			}},
		}
		record := coredata.CommitRecord{
			ID: tx, Durability: nest.DurabilityStrict,
			Mutations: []coredata.Mutation{{
				Key:  coredata.DocumentKey{Resource: "remote_entity", ID: id},
				Kind: coredata.MutationPut, ExpectedVersion: 0, NextVersion: 1,
				Schema: 1, Codec: "remote", Remote: &commit,
			}},
		}
		created = append(created, record)
		fence, err := wal.Append(ctx, record)
		if err != nil {
			t.Fatal(err)
		}
		if err := wal.Replay(ctx, func(_ nest.CommitFence, replay nest.CommitRecord) error { return store.Project(ctx, replay) }); err != nil {
			t.Fatal(err)
		}
		if incarnation != 2 {
			if err := wal.Ack(ctx, fence); err != nil {
				t.Fatal(err)
			}
		}
		if err := wal.Close(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if count, err := client.Database(database).Collection("guild").CountDocuments(ctx, bson.M{}); err != nil || count != 3 {
		t.Fatalf("guilds=%d err=%v", count, err)
	}
	// A different transaction creating an existing ID is a real conflict,
	// never a benign replay. This is the historical W-02 shape.
	collision := coredata.CloneCommitRecord(created[0])
	collision.ID[0] = 99
	collision.Mutations[0].Remote.TransactionID = entity.RemoteTransactionID(collision.ID)
	if err := store.Project(ctx, collision); !errors.Is(err, fmongo.ErrVersionConflict) {
		t.Fatalf("collision err=%v", err)
	}
}

type guildReplayApplier struct{ store *remoteentity.MongoCommitter }

func (a guildReplayApplier) ApplyRemoteCommits(ctx context.Context, _ entity.RemoteTransactionID, commits []entity.RemoteCommit) ([]entity.RemoteCommitReceipt, error) {
	return a.store.CommitRemoteBatch(ctx, commits)
}

// RR-20260926-22（复核残留）：首次发号的并发 upsert 不能撞 duplicate key。
//
// 真实触发场景是“集合已存在、计数文档还没有”：修前过滤条件带范围谓词，Mongo 不对这种
// upsert 做 duplicate key 的服务端重试，每个并发者都去插 _id=guild，复核实测 30 轮 30 轮
// 失败；集合都不存在的全新库修前只有少数轮失败，旧用例只测了这一种。两种都测、各跑多轮，
// 并断言号从 1 起连续——每次 $inc 都恰好成功一次。
func TestGuildIDsRealMongoConcurrentFirstMint(t *testing.T) {
	uri := os.Getenv("ROOST_GUILD_IT_MONGO_URI")
	if uri == "" {
		t.Skip("set ROOST_GUILD_IT_MONGO_URI to an isolated replica set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	client, err := driver.NewClient(fmongo.DefaultConfig(uri), driver.IndexMigrationPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close(context.Background())
	const rounds, creators = 5, 64
	for _, collectionExists := range []bool{true, false} {
		name := "fresh database"
		if collectionExists {
			name = "collection exists without the counter"
		}
		t.Run(name, func(t *testing.T) {
			for round := range rounds {
				database := fmt.Sprintf("roost_guild_it_first_%d_%d_%d", os.Getpid(), time.Now().UnixNano(), round)
				t.Cleanup(func() { client.Database(database).Drop(context.Background()) })
				sequence := client.Database(database).Collection("_guild_id_sequence")
				if collectionExists {
					if _, err := sequence.InsertOne(ctx, bson.M{"_id": "unrelated"}); err != nil {
						t.Fatal(err)
					}
				}
				controller := &Controller{serverSID: 1000, guildIDs: sequence}
				start := make(chan struct{})
				ids := make(chan int64, creators)
				failures := make(chan error, creators)
				var group sync.WaitGroup
				for range creators {
					group.Go(func() {
						<-start
						id, err := controller.nextGuildID(ctx)
						if err != nil {
							failures <- err
							return
						}
						ids <- id
					})
				}
				close(start)
				group.Wait()
				close(failures)
				close(ids)
				for err := range failures {
					t.Errorf("round %d: %v", round, err)
				}
				seen := make(map[int64]bool, creators)
				for id := range ids {
					if seen[id] || id < 1 || id > creators {
						t.Errorf("round %d: id %d reused or outside 1..%d", round, id, creators)
					}
					seen[id] = true
				}
				if len(seen) != creators {
					t.Fatalf("round %d: minted %d/%d ids", round, len(seen), creators)
				}
			}
		})
	}
}
