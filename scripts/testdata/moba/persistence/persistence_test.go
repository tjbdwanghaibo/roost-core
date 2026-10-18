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

// 两个独立进程：第一进程通过正式 Sender 提交胜负，第二进程冷加载后重复结算不能重发奖励。
// 固定脚本创建唯一 DAO 库与 WAL 目录，成功恢复后只删本次库；不做断电或事务结果未知注入。
func TestMOBARealSettlementAcrossProcesses(t *testing.T) {
	uri := os.Getenv("ROOST_DATAENGINE_IT_MONGO_URI")
	if uri == "" {
		t.Fatal("isolated Mongo replica set required")
	}
	phase := os.Getenv("ROOST_MOBA_PHASE")
	if phase != "write" && phase != "recover" {
		t.Fatal("write / recover phase required")
	}
	database := db.NewPlayerDao().DbName()
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
	store, err := engine.NewMongoStore(client, engine.MongoStoreConfig{DefaultDatabase: database})
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
	if winner.(*player.Player).Dao().GetGold() != 10 || winner.(*player.Player).Dao().GetWins() != 1 || loser.(*player.Player).Dao().GetLosses() != 1 || result.(*resultentity.MatchResult).Dao().GetDigest() != digest {
		t.Fatal("settlement identity or assets did not survive / replayed twice")
	}
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
