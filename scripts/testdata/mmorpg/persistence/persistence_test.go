package handler

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	db "example.com/mmorpg-practice/db"
	monster "example.com/mmorpg-practice/game/entities/monster"
	player "example.com/mmorpg-practice/game/entities/player"
	scene "example.com/mmorpg-practice/game/entities/scene"
	trade "example.com/mmorpg-practice/game/entities/trade"
	sender "example.com/mmorpg-practice/game/handler/syncsender"
	"github.com/tjbdwanghaibo/roost-core/framework/dataengine/engine"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/nest"
	"github.com/tjbdwanghaibo/roost-core/framework/nestwal"
	fmongo "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo"
	mongodriver "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo/driver"
)

// 没有业务 effect 的这条链路不应产生任何发布；若出现则失败，不以空发布器吞掉 effect。
type mmoNoEffects struct{}

func (mmoNoEffects) Publish(context.Context, engine.OutboxItem) error {
	return errors.New("unexpected effect in local MMO workflow")
}

// 两次独立 go test 进程：第一阶段停在已持久化的单方确认，第二阶段冷加载后完成交易并重放战斗领奖。
// 真实依赖准入由脚本显式设置，普通纵切不复制本文件，避免把 skip 或零测试记作恢复通过。
func TestMMORPGRealPersistenceAcrossProcesses(t *testing.T) {
	uri := os.Getenv("ROOST_DATAENGINE_IT_MONGO_URI")
	if uri == "" {
		t.Fatal("isolated Mongo replica set required")
	}
	phase := os.Getenv("ROOST_MMORPG_PHASE")
	if phase != "write" && phase != "recover" {
		t.Fatal("write or recover phase required")
	}
	database := db.NewPlayerDao().DbName()
	if database != os.Getenv("ROOST_MMORPG_DB") {
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
	wal, err := nestwal.Open(nestwal.DefaultOptions(filepath.Join(os.Getenv("ROOST_MMORPG_WAL"), "wal")))
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
	outbox, err := engine.NewOutboxWorker(outboxStore, mmoNoEffects{}, engine.OutboxWorkerOptions{Owner: "mmo-practice"})
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
	options := append(runtime.NestOptions(), nest.NestOptionWithGetter(access), nest.NestOptionWithWorkerPools(nest.WorkerPoolConfig{Workers: 2, QueueCap: 32}, nest.WorkerPoolConfig{}, nest.WorkerPoolConfig{}))
	scheduler := nest.NewEngine(options...)
	if err := scheduler.Start(); err != nil {
		t.Fatal(err)
	}
	defer scheduler.Shutdown(context.Background())
	player.RegisterEntity()
	trade.RegisterEntity()
	monster.RegisterEntity()
	RegisterMmoTasksNestHandlers()
	RegisterMmoGameplayNestHandlers()
	RegisterTickPlayerTimerGenNestHandlers()
	now := time.Unix(1800000000, 0)
	player.TimerDriver.SetClock(func() time.Time { return now })
	a, _ := entity.BuildEntityID(7001, player.EntityKindPlayer)
	b, _ := entity.BuildEntityID(7002, player.EntityKindPlayer)
	tr, _ := entity.BuildEntityID(7001, trade.EntityKindTrade)
	mob, _ := entity.BuildEntityID(7001, monster.EntityKindMonster)
	area, _ := entity.BuildEntityID(7001, scene.EntityKindScene)
	s := sender.NewMmoGameplaySender(scheduler)
	if phase == "write" {
		for _, id := range []int64{a, b, tr, mob} {
			kind := player.EntityKindPlayer
			if id == tr {
				kind = trade.EntityKindTrade
			}
			if id == mob {
				kind = monster.EntityKindMonster
			}
			if _, err := access.Create(&entity.EntityCreateParam{IsCreate: true, Id: id, Kind: kind}); err != nil {
				t.Fatal(err)
			}
		}
		seed := nest.NewHandlerName("mmo_real_seed")
		nest.MustRegisterHandlerWithMeta(seed, func(es []entity.IThreadSafeEntity, _ []any, _ ...nest.HandlerOption) (any, error) {
			switch e := es[0].(type) {
			case *player.Player:
				e.Dao().SetGold(50)
				e.Dao().SetItems(3)
				e.Dao().SetHP(100)
				e.Dao().SetSceneID(area)
			case *monster.Monster:
				e.Dao().SetHP(20)
				e.Dao().SetSceneID(area)
			}
			return nil, nil
		}, nest.HandlerMeta{Rollback: nest.RollbackUndo, Durability: nest.DurabilityStrict})
		for _, id := range []int64{a, b, mob} {
			if _, err := scheduler.Request(ctx, seed, id, nil); err != nil {
				t.Fatal(err)
			}
		}
		if err := s.MultiSync_MMORPGStrike(ctx, a, mob, 1, now.UnixMilli()); err != nil {
			t.Fatal(err)
		}
		if err := s.MultiSync_MMORPGStrike(ctx, a, mob, 2, now.UnixMilli()+1000); err != nil {
			t.Fatal(err)
		}
		if err := s.MultiSync_MMORPGLoot(ctx, a, mob); err != nil {
			t.Fatal(err)
		}
		if err := s.Sync_MMORPGQuest(ctx, a); err != nil {
			t.Fatal(err)
		}
		if err := s.Sync_MMORPGBuff(ctx, a, 7); err != nil {
			t.Fatal(err)
		}
		if err := s.MultiSync_MMORPGOffer(ctx, tr, a, b, 10, 1, now.UnixMilli()); err != nil {
			t.Fatal(err)
		}
		if err := s.MultiSync_MMORPGResolveTrade(ctx, tr, a, b, a, 1, now.UnixMilli()); err != nil {
			t.Fatal(err)
		}
	} else {
		loaded, err := access.Get(ctx, tr, entity.EntityCategoryWorld)
		if err != nil {
			t.Fatal(err)
		}
		if d := loaded.(*trade.Trade).Dao(); d.GetStatus() != 1 || d.GetSellerConfirmed() != 1 || d.GetBuyerConfirmed() != 0 {
			t.Fatal("partial confirmation was not recovered")
		}
		for range 2 {
			if err := s.MultiSync_MMORPGResolveTrade(ctx, tr, a, b, b, 1, now.UnixMilli()+1000); err != nil {
				t.Fatal(err)
			}
		}
		if err := s.MultiSync_MMORPGStrike(ctx, a, mob, 2, now.UnixMilli()+1000); err != nil {
			t.Fatal(err)
		}
		if err := s.MultiSync_MMORPGLoot(ctx, a, mob); err != nil {
			t.Fatal(err)
		}
		if err := s.Sync_MMORPGQuest(ctx, a); err != nil {
			t.Fatal(err)
		}
		if _, err := sender.NewTickPlayerTimerGenSender(scheduler).Sync_TickPlayerTimer(ctx, a, now.Add(time.Hour).UnixMilli()); err != nil {
			t.Fatal(err)
		}
		value, err := access.Get(ctx, a, entity.EntityCategoryOther)
		if err != nil {
			t.Fatal(err)
		}
		d := value.(*player.Player).Dao()
		if d.GetGold() != 65 || d.GetItems() != 3 || d.GetQuestKills() != 1 || d.GetQuestClaimed() != 1 || d.GetBuffValue() != 0 {
			t.Fatalf("recovered seller gold=%d items=%d kills=%d claim=%d buff=%d", d.GetGold(), d.GetItems(), d.GetQuestKills(), d.GetQuestClaimed(), d.GetBuffValue())
		}
		peer, err := access.Get(ctx, b, entity.EntityCategoryOther)
		if err != nil {
			t.Fatal(err)
		}
		p := peer.(*player.Player).Dao()
		if p.GetGold() != 40 || p.GetItems() != 4 {
			t.Fatal("recovered buyer state disagrees")
		}
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
	// Runtime 已排空且关闭 WAL；确认不是只在内存中跑过，恢复阶段再清理本次唯一库。
	if phase == "recover" {
		if err := client.Database(database).Drop(ctx); err != nil {
			t.Fatal(err)
		}
	}
}
