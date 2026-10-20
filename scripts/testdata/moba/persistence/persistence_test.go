package handler

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	db "example.com/moba-practice/db"
	resultentity "example.com/moba-practice/game/entities/match_result"
	player "example.com/moba-practice/game/entities/player"
	sender "example.com/moba-practice/game/handler/syncsender"
	coredata "github.com/tjbdwanghaibo/roost-core/framework/dataengine"
	"github.com/tjbdwanghaibo/roost-core/framework/dataengine/engine"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/nest"
	"github.com/tjbdwanghaibo/roost-core/framework/nestwal"
	fmongo "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo"
	mongodriver "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo/driver"
)

type mobaNoEffects struct{}

func (mobaNoEffects) Publish(context.Context, engine.OutboxItem) error {
	return errors.New("unexpected outbox effect in MOBA local settlement")
}

// 两次独立 go test 进程：第一进程通过正式多实体 Sender 提交胜负后正常停机；第二进程冷加载，
// 先在任何结算请求之前核对结算身份 / 奖励 / 战绩已恢复，让恢复本身成为断言对象（实体随结算事务一起落库，
// 库里没有时加载直接报 entity aggregate not found），
// 再重放同一结算不能重发奖励、换胜者被拒。脚本创建唯一 DAO 库与 WAL 目录，成功恢复后只删本次库；
// 不做断电、崩溃或事务结果未知注入，也不恢复进行中的对局。
func TestMOBARealSettlementAcrossProcesses(t *testing.T) {
	uri := os.Getenv("ROOST_DATAENGINE_IT_MONGO_URI")
	if uri == "" {
		t.Fatal("isolated Mongo replica set required")
	}
	phase := os.Getenv("ROOST_MOBA_PHASE")
	if phase != "write" && phase != "recover" {
		t.Fatal("write / recover phase required")
	}
	// DAO 写逻辑库 db=main（骨架默认）；本次运行的唯一物理库经映射表给出，与进程的 dataengine.databases 同一条规则。
	if logical := db.NewPlayerDao().DbName(); logical != db.DatabaseMain {
		t.Fatalf("Player DAO logical database %q, want %q", logical, db.DatabaseMain)
	}
	database := os.Getenv("ROOST_MOBA_DB")
	databases, _, err := coredata.NewDatabaseMap(coredata.DatabaseMapConfig{Declared: coredata.DeclaredDatabases(), Configured: map[string]string{db.DatabaseMain: database}})
	if err != nil {
		t.Fatal(err)
	}
	if database != os.Getenv("ROOST_MOBA_DB") || !strings.HasPrefix(database, "roost_moba_it_") {
		t.Fatal("refusing a database outside this generated consumer")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	client, err := mongodriver.NewClient(fmongo.DefaultConfig(uri), mongodriver.IndexMigrationPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close(context.Background())
	if err := client.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	store, err := engine.NewMongoStore(client, engine.MongoStoreConfig{DefaultDatabase: database, Databases: databases})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.EnsureInfrastructure(ctx); err != nil {
		t.Fatal(err)
	}
	wal, err := nestwal.Open(nestwal.DefaultOptions(filepath.Join(os.Getenv("ROOST_MOBA_WAL"), "wal")))
	if err != nil {
		t.Fatal(err)
	}
	projector, err := engine.NewProjector(wal, store, engine.ProjectorOptions{})
	if err != nil {
		t.Fatal(err)
	}
	outboxStore, err := engine.NewMongoOutboxStore(store)
	if err != nil {
		t.Fatal(err)
	}
	outbox, err := engine.NewOutboxWorker(outboxStore, mobaNoEffects{}, engine.OutboxWorkerOptions{Owner: "moba-practice"})
	if err != nil {
		t.Fatal(err)
	}
	access := entity.NewManagerAccess(entity.NewEntityManager())
	runtime, err := engine.NewRuntime(store, wal, projector, outbox, access, nil, nil, engine.PipelinedRuntimeConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer runtime.Shutdown(context.Background())
	options := append(runtime.NestOptions(), nest.NestOptionWithGetter(access))
	scheduler := nest.NewEngine(options...)
	if err := scheduler.Start(); err != nil {
		t.Fatal(err)
	}
	defer scheduler.Shutdown(context.Background())
	player.RegisterEntity()
	resultentity.RegisterEntity()
	RegisterMobaSettlementNestHandlers()
	a, _ := entity.BuildEntityID(9001, player.EntityKindPlayer)
	b, _ := entity.BuildEntityID(9002, player.EntityKindPlayer)
	r, _ := entity.BuildEntityID(9001, resultentity.EntityKindMatchResult)
	if phase == "write" {
		for _, id := range []int64{a, b, r} {
			kind := player.EntityKindPlayer
			if id == r {
				kind = resultentity.EntityKindMatchResult
			}
			if _, err := access.Create(&entity.EntityCreateParam{IsCreate: true, Id: id, Kind: kind}); err != nil {
				t.Fatal(err)
			}
		}
	}
	s := sender.NewMobaSettlementSender(scheduler)
	digest := strings.Repeat("a", 64)
	if phase == "recover" {
		assertSettled(ctx, t, access, a, b, r, digest)
	}
	for range 2 {
		if err := s.MultiSync_MOBASettle(ctx, r, a, b, "persisted-moba-match", digest, false); err != nil {
			t.Fatal(err)
		}
	}
	if phase == "recover" {
		if err := s.MultiSync_MOBASettle(ctx, r, b, a, "persisted-moba-match", digest, false); err == nil {
			t.Fatal("recovered settlement accepted changed winner")
		}
	}
	assertSettled(ctx, t, access, a, b, r, digest)
	if err := runtime.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if err := scheduler.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if phase == "recover" {
		if err := client.Database(database).Drop(ctx); err != nil {
			t.Fatal(err)
		}
	}
}

// assertSettled 核对一局已结算且只结算一次：结算身份、摘要与终态在 MatchResult，奖励与战绩在双方 Player。
func assertSettled(ctx context.Context, t *testing.T, access *entity.ManagerAccess, a, b, r int64, digest string) {
	t.Helper()
	winner, err := access.Get(ctx, a, entity.EntityCategoryOther)
	if err != nil {
		t.Fatal(err)
	}
	loser, err := access.Get(ctx, b, entity.EntityCategoryOther)
	if err != nil {
		t.Fatal(err)
	}
	result, err := access.Get(ctx, r, entity.EntityCategoryWorld)
	if err != nil {
		t.Fatal(err)
	}
	w, l, d := winner.(*player.Player).Dao(), loser.(*player.Player).Dao(), result.(*resultentity.MatchResult).Dao()
	if w.GetGold() != 10 || w.GetWins() != 1 || l.GetLosses() != 1 || d.GetStatus() != 1 || d.GetMatchKey() != "persisted-moba-match" || d.GetDigest() != digest || d.GetWinnerID() != a || d.GetLoserID() != b {
		t.Fatalf("settlement not recovered exactly once: winner gold=%d wins=%d, loser losses=%d, result status=%d key=%q", w.GetGold(), w.GetWins(), l.GetLosses(), d.GetStatus(), d.GetMatchKey())
	}
}
