package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	gonats "github.com/nats-io/nats.go"
	gojs "github.com/nats-io/nats.go/jetstream"
	"github.com/spf13/viper"
	"go.mongodb.org/mongo-driver/v2/bson"

	db "example.com/planet/db"
	player "example.com/planet/game/entities/player"
	syncsender "example.com/planet/game/handler/syncsender"
	registry "example.com/planet/internal/registry"
	serviceGame "example.com/planet/internal/service/game"
	gift "example.com/planet/saga/gift"
	"github.com/tjbdwanghaibo/roost-core/framework/app"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	corenest "github.com/tjbdwanghaibo/roost-core/framework/nest"
	coresaga "github.com/tjbdwanghaibo/roost-core/framework/saga"
	fmongo "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo"
	kitdataengine "github.com/tjbdwanghaibo/roost-core/wiring/dataengine"
	"github.com/tjbdwanghaibo/roost-core/wiring/mods"
	kitmongo "github.com/tjbdwanghaibo/roost-core/wiring/mongo"
	kitnats "github.com/tjbdwanghaibo/roost-core/wiring/nats"
	kitnest "github.com/tjbdwanghaibo/roost-core/wiring/nest"
	kitsaga "github.com/tjbdwanghaibo/roost-core/wiring/saga"
)

// 生成器产物在真实依赖上端到端（D12 方向 B）：roost add saga -native 与 roost add effect 的骨架填上最小业务后，
// 经正式的 Mongo / NATS / DataEngine / Nest / saga Mod 与本工程的 game 服务 Init（StartSteps + effect 消费者）：
//
//   - 正向：A 送 B 30 金币，debit、credit（原生步骤）与 audit（Mongo 步骤）都完成；
//   - 补偿：A 送 C 20 金币，C 的钱包将满，credit 业务拒绝（step.Complete(false, …)），协调器补偿 debit（refund）；
//   - effect：A 升级，handler 在同一事务里 Emit player.level_up，消费者在 Mongo 事务里写一条通知。
//
// 核对恰好一次：两笔 saga 的终态；Mongo 里三个钱包的余额（A 100→70、B 100→130、C 995 不变）；audit 记录只有
// 正向那一笔一条；升级通知一条、回执一条（再等一会儿仍是一条）。
//
// 资源：DAO 物理库 sagagen_dao_<…>（逻辑库 main 经 dataengine.databases 映射过去，核对文档不落在名为 main 的库）、库 sagagen_de_<pid>_<ns> / sagagen_saga_<…>、
// 流 SAGAGEN_SAGA_<…> / SAGAGEN_EFFECTS_<…>，用后删除。由 codegen/internal/roost 的
// TestGeneratedSagaNativeStepsAndEffectConsumer 复制进新生成的工程后运行（scripts/test-demo-generated.sh）。
func TestSagaNativeStepsAndEffectRealDependencies(t *testing.T) {
	mongoURI, natsURL := os.Getenv("ROOST_DATAENGINE_IT_MONGO_URI"), os.Getenv("ROOST_DATAENGINE_IT_NATS_URL")
	if mongoURI == "" || natsURL == "" {
		t.Skip("source ~/.roost-it/roost-dataengine-it/env.sh: ROOST_DATAENGINE_IT_MONGO_URI and ROOST_DATAENGINE_IT_NATS_URL are required")
	}
	if err := registry.RegisterAll(); err != nil {
		t.Fatal(err)
	}
	suffix := fmt.Sprintf("%d_%d", os.Getpid(), time.Now().UnixNano()%1_000_000_000)
	token := strings.ReplaceAll(suffix, "_", "x")
	sagaDatabase, dataDatabase, playerDatabase := "sagagen_saga_"+suffix, "sagagen_de_"+suffix, "sagagen_dao_"+suffix
	sagaStream, effectStream := "SAGAGEN_SAGA_"+suffix, "SAGAGEN_EFFECTS_"+suffix
	cfg := viper.New()
	for key, value := range map[string]any{
		"sid": int32(903), "server_type": "game", "persistence.engine": "dataengine",
		"mongo.uri": mongoURI, "mongo.require_replica_set": true, "mongo.connect_timeout": 5 * time.Second, "mongo.transaction_timeout": 15 * time.Second,
		"nats.url": natsURL, "nats.prefix": "sagagen" + token,
		"dataengine.database": dataDatabase, "dataengine.wal.dir": t.TempDir(),
		"dataengine.databases":      map[string]any{db.DatabaseMain: playerDatabase},
		"dataengine.effects.stream": effectStream, "dataengine.effects.subject_prefix": "sagageneffect" + token,
		"dataengine.effects.replicas": 3, "dataengine.effects.max_bytes": int64(64 << 20),
		"dataengine.outbox.poll_interval": 50 * time.Millisecond, "dataengine.outbox.retry_min": 100 * time.Millisecond, "dataengine.outbox.retry_max": time.Second,
		"saga.database": sagaDatabase, "saga.stream": sagaStream, "saga.subject_prefix": "sagagensaga" + token, "saga.replicas": 3,
		"saga.stream_max_bytes": int64(64 << 20), "saga.poll_interval": 50 * time.Millisecond,
		"saga.step_defaults.timeout": 10 * time.Second, "saga.step_defaults.max_attempts": 5,
		"saga.step_defaults.backoff_min": 100 * time.Millisecond, "saga.step_defaults.backoff_max": time.Second,
	} {
		cfg.Set(key, value)
	}
	reg := app.NewRegistry(cfg)
	mongoMod := kitmongo.NewMongoMod()
	sagaMod := kitsaga.NewMod(kitsaga.CombineDefinitions(gift.Definitions())...)
	type lifecycle interface {
		app.Mod
		Init(*viper.Viper) error
		Provide(*app.Registry) error
		Start() error
		StopWithContext(context.Context) error
	}
	all := []lifecycle{mongoMod, kitnats.NewNatsMod(nil), kitdataengine.NewMod(kitdataengine.WithEntityAccess(EntityAccess)), kitnest.NewMod(EntityAccess), sagaMod}
	checked := make([]app.Mod, 0, len(all))
	for _, mod := range all {
		checked = append(checked, mod)
	}
	if err := app.CheckConfig(cfg, checked...); err != nil {
		t.Fatal(err)
	}
	var mongoClient fmongo.IMongo
	var started []lifecycle
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		for i := len(started) - 1; i >= 0; i-- {
			if i == 0 && mongoClient != nil { // Mongo 最后停：先删本次的库
				for _, database := range []string{playerDatabase, dataDatabase, sagaDatabase} {
					if err := mongoClient.Database(database).Drop(ctx); err != nil {
						t.Errorf("cleanup: drop %s: %v", database, err)
					}
				}
			}
			if err := started[i].StopWithContext(ctx); err != nil {
				t.Errorf("cleanup: stop %s: %v", started[i].Name(), err)
			}
		}
		deleteStreams(t, natsURL, sagaStream, effectStream)
	})
	for _, mod := range all {
		if err := mod.Init(cfg); err != nil {
			t.Fatal(err)
		}
	}
	for _, mod := range all {
		if err := mod.Provide(reg); err != nil {
			t.Fatal(err)
		}
		if err := mod.Start(); err != nil {
			t.Fatal(err)
		}
		started = append(started, mod)
		if mod == lifecycle(mongoMod) {
			mongoClient = mongoMod.Client()
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	// 三个钱包直接写进 DAO 集合（与冷加载读的是同一份文档）。
	players := map[string]int64{}
	for i, wallet := range []struct {
		name string
		gold int64
	}{{"a", 100}, {"b", 100}, {"c", 995}} {
		playerID := int64(9_101 + i)
		entityID, err := entity.BuildEntityID(playerID, player.EntityKindPlayer)
		if err != nil {
			t.Fatal(err)
		}
		players[wallet.name] = playerID
		if _, err := mongoClient.Database(playerDatabase).Collection(db.PlayerDaoCollection).InsertOne(ctx,
			bson.M{"_id": entityID, "_version": uint64(1), "_schema": db.PlayerDaoSchemaVersion, "gold": wallet.gold, "level": int32(1)}); err != nil {
			t.Fatal(err)
		}
	}
	entityOf := func(name string) int64 {
		id, err := entity.BuildEntityID(players[name], player.EntityKindPlayer)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}

	// 服务 Init 按 next 提示接的两行：StartSteps 与 effect 消费者。
	service := serviceGame.New()
	if err := service.Init(reg); err != nil {
		t.Fatal(err)
	}
	stopped := false
	defer func() {
		if !stopped {
			_ = service.Shutdown(context.Background())
		}
	}()

	nest := app.MustLookup[corenest.Client](reg, mods.ModNest)
	starts := syncsender.NewStartGiftSender(nest)
	for _, gift := range []struct{ key, to string }{{"ok-" + token, "b"}, {"refuse-" + token, "c"}} {
		amount := int64(30)
		if gift.to == "c" {
			amount = 20
		}
		if err := starts.Sync_StartGift(ctx, entityOf("a"), players["a"], players[gift.to], amount, gift.key); err != nil {
			t.Fatalf("start gift %s: %v", gift.key, err)
		}
	}
	level, err := syncsender.NewLevelUpSender(nest).Sync_LevelUp(ctx, entityOf("a"), players["a"])
	if err != nil || level != 2 {
		t.Fatalf("level up: level=%d err=%v", level, err)
	}

	want := map[string]coresaga.Status{"ok-" + token: coresaga.StatusCompleted, "refuse-" + token: coresaga.StatusCompensated}
	sagaIDs := map[string]string{}
	for key, status := range want {
		deadline := time.Now().Add(90 * time.Second)
		for {
			record, err := sagaMod.Store().GetByBusinessKey(ctx, gift.Type, key)
			if err == nil && record.Status.Terminal() {
				if record.Status != status {
					t.Fatalf("saga %s ended %s (%s), want %s", key, record.Status, record.LastError, status)
				}
				sagaIDs[key] = record.ID
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("saga %s did not finish: %+v err=%v", key, record, err)
			}
			time.Sleep(100 * time.Millisecond)
		}
	}

	// 投影是异步的：余额等到对上为止（等不到就是多扣 / 少扣 / 多加）。
	wantGold := map[string]int64{"a": 70, "b": 130, "c": 995}
	deadline := time.Now().Add(30 * time.Second)
	for {
		got := map[string]int64{}
		for name := range wantGold {
			var document struct {
				Gold int64 `bson:"gold"`
			}
			if err := mongoClient.Database(playerDatabase).Collection(db.PlayerDaoCollection).FindOne(ctx, bson.M{"_id": entityOf(name)}, &document); err != nil {
				t.Fatal(err)
			}
			got[name] = document.Gold
		}
		if fmt.Sprint(got) == fmt.Sprint(wantGold) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("stored gold = %v, want %v: each step operation takes effect exactly once", got, wantGold)
		}
		time.Sleep(100 * time.Millisecond)
	}
	// DAO 的 db=main 是逻辑名：名为 main 的库里不能有本次的文档。
	for name := range wantGold {
		if err := mongoClient.Database(db.PlayerDaoDBName).Collection(db.PlayerDaoCollection).FindOne(ctx, bson.M{"_id": entityOf(name)}, &bson.M{}); !errors.Is(err, fmongo.ErrNotFound) {
			t.Fatalf("player %s found in the logical database %q (err %v); it must only be in %s", name, db.PlayerDaoDBName, err, playerDatabase)
		}
	}
	count := func(database, collection string, filter bson.M) int64 {
		n, err := mongoClient.Database(database).Collection(collection).CountDocuments(ctx, filter)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := count(playerDatabase, "gift_audit", bson.M{"saga_id": sagaIDs["ok-"+token]}); n != 1 {
		t.Errorf("audit records for the completed gift = %d, want 1", n)
	}
	if n := count(playerDatabase, "gift_audit", bson.M{"saga_id": sagaIDs["refuse-"+token]}); n != 0 {
		t.Errorf("audit records for the compensated gift = %d, want 0 (audit comes after the refused credit)", n)
	}

	// effect：通知一条、回执一条；再等一会儿仍是一条（没有重复消费）。
	deadline = time.Now().Add(60 * time.Second)
	for count(playerDatabase, "level_up_notices", bson.M{"player_id": players["a"], "level": int32(2)}) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("no level-up notice: the player.level_up effect was not consumed")
		}
		time.Sleep(100 * time.Millisecond)
	}
	time.Sleep(2 * time.Second)
	if n := count(playerDatabase, "level_up_notices", bson.M{}); n != 1 {
		t.Errorf("level-up notices = %d, want exactly 1", n)
	}
	if n := count(dataDatabase, "_effect_inbox_"+serviceGame.PlayerLevelUpEffectDurable, bson.M{}); n != 1 {
		t.Errorf("effect receipts = %d, want 1", n)
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer shutdownCancel()
	stopped = true
	if err := service.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("service Shutdown (steps.Drain): %v", err)
	}
}

func deleteStreams(t *testing.T, url string, streams ...string) {
	t.Helper()
	nc, err := gonats.Connect(url, gonats.Timeout(2*time.Second))
	if err != nil {
		t.Errorf("cleanup: connect: %v", err)
		return
	}
	defer nc.Close()
	js, err := gojs.New(nc)
	if err != nil {
		t.Errorf("cleanup: JetStream: %v", err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, stream := range streams {
		if err := js.DeleteStream(ctx, stream); err != nil && !errors.Is(err, gojs.ErrStreamNotFound) {
			t.Errorf("cleanup: delete stream %s: %v", stream, err)
		}
	}
}
