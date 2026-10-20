package handler

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
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
	"github.com/tjbdwanghaibo/roost-core/framework/nest/entitytimer"
	"github.com/tjbdwanghaibo/roost-core/framework/nestwal"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
	fmongo "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo"
	mongodriver "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo/driver"
)

// 没有业务 effect 的这条链路不应产生任何发布；若出现则失败，不以空发布器吞掉 effect。
type mmoNoEffects struct{}

func (mmoNoEffects) Publish(context.Context, engine.OutboxItem) error {
	return errors.New("unexpected effect in local MMO workflow")
}

// 两个真实依赖用例共用的业务时间起点（UnixMilli）；Buff 2 秒、复活 3 秒的期限都从这里起算。
const mmoRealT0 int64 = 1800000000000

var (
	mmoRealSeed = nest.NewHandlerName("mmo_real_seed")
	mmoRealView = nest.NewHandlerName("mmo_real_view")
	mmoRealOnce sync.Once
)

// mmoRealState 是经 Nest 读出的一个实体：Raw 是 DAO 的完整持久字节（与写入 Mongo 的文档同一份编码），
// 其余字段只为断言好读。投递循环运行后，读状态必须走 Nest（实体锁内），不能与循环投递的 Tick 并发读 DAO。
type mmoRealState struct {
	Raw                                          []byte
	Gold, Items, HP, QuestKills, QuestClaimed    int64
	BuffValue, BuffTimer, ReviveTimer            int64
	Timers                                       int
	TradeStatus, SellerConfirmed, BuyerConfirmed int64
}

// registerMMORealHandlers 注册实体、生成的 handler 与本夹具的两个测试 handler（种子数据、只读视图）；每个进程一次。
func registerMMORealHandlers() {
	mmoRealOnce.Do(func() {
		player.RegisterEntity()
		scene.RegisterEntity()
		trade.RegisterEntity()
		monster.RegisterEntity()
		RegisterMmoTasksNestHandlers()
		RegisterMmoGameplayNestHandlers()
		RegisterTickPlayerTimerGenNestHandlers()
		// 种子：参数为 gold / items / hp / sceneID，怪物只用 hp / sceneID。只在第一次写入时使用。
		nest.MustRegisterHandlerWithMeta(mmoRealSeed, func(es []entity.IThreadSafeEntity, args []any, _ ...nest.HandlerOption) (any, error) {
			gold, items, hp, area := args[0].(int64), args[1].(int64), args[2].(int64), args[3].(int64)
			switch e := es[0].(type) {
			case *player.Player:
				e.Dao().SetGold(gold)
				e.Dao().SetItems(items)
				e.Dao().SetHP(hp)
				e.Dao().SetSceneID(area)
			case *monster.Monster:
				e.Dao().SetHP(hp)
				e.Dao().SetSceneID(area)
			default:
				return nil, errMMORule
			}
			return nil, nil
		}, nest.HandlerMeta{Rollback: nest.RollbackUndo, Durability: nest.DurabilityStrict})
		nest.MustRegisterHandlerWithMeta(mmoRealView, func(es []entity.IThreadSafeEntity, _ []any, _ ...nest.HandlerOption) (any, error) {
			switch e := es[0].(type) {
			case *player.Player:
				d := e.Dao()
				return mmoRealState{Raw: d.Marshal(), Gold: d.GetGold(), Items: d.GetItems(), HP: d.GetHP(), QuestKills: d.GetQuestKills(), QuestClaimed: d.GetQuestClaimed(), BuffValue: d.GetBuffValue(), BuffTimer: d.GetBuffTimer(), ReviveTimer: d.GetReviveTimer(), Timers: d.TimersLen()}, nil
			case *trade.Trade:
				d := e.Dao()
				return mmoRealState{Raw: d.Marshal(), TradeStatus: d.GetStatus(), SellerConfirmed: d.GetSellerConfirmed(), BuyerConfirmed: d.GetBuyerConfirmed()}, nil
			case *monster.Monster:
				return mmoRealState{Raw: e.Dao().Marshal(), HP: e.Dao().GetHP()}, nil
			}
			return nil, errMMORule
		}, nest.HandlerMeta{Rollback: nest.RollbackUndo, Durability: nest.DurabilityStrict})
	})
}

// mmoRealRuntime 是一个进程里的真实依赖装配：文件 WAL + 真实 Mongo 的 DataEngine Runtime、Nest 与 Player 定时器驱动。
type mmoRealRuntime struct {
	runtime   *engine.Runtime
	projector *engine.Projector
	scheduler *nest.NestMgr
	access    *entity.ManagerAccess
	mongo     *mongodriver.Client
	database  string
	clock     atomic.Int64
	closeOnce sync.Once
}

// openMMORealRuntime 用 walRoot 下的文件 WAL 与本次唯一库装配一个进程。manual 为 true 时关掉后台投影，
// 让 strict 提交停在“已写 WAL、未投影到 Mongo”（崩溃注入用）。now 是驱动的业务时钟起点，必须在实体加载前设好。
func openMMORealRuntime(t *testing.T, walRoot string, manual bool, now int64) *mmoRealRuntime {
	t.Helper()
	uri := os.Getenv("ROOST_DATAENGINE_IT_MONGO_URI")
	if uri == "" {
		t.Fatal("isolated Mongo replica set required")
	}
	if walRoot == "" {
		t.Fatal("unique WAL directory required")
	}
	database := db.NewPlayerDao().DbName()
	if database != os.Getenv("ROOST_MMORPG_DB") || !strings.HasPrefix(database, "roost_mmo_it_") {
		t.Fatal("refusing a database outside this generated consumer")
	}
	registerMMORealHandlers()
	h := &mmoRealRuntime{database: database}
	h.clock.Store(now)
	player.TimerDriver.SetClock(func() time.Time { return time.UnixMilli(h.clock.Load()) })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client, err := mongodriver.NewClient(fmongo.DefaultConfig(uri), mongodriver.IndexMigrationPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	h.mongo = client
	t.Cleanup(func() { _ = client.Close(context.Background()) })
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
	wal, err := nestwal.Open(nestwal.DefaultOptions(filepath.Join(walRoot, "wal")))
	if err != nil {
		t.Fatal(err)
	}
	h.projector, err = engine.NewProjector(wal, store, engine.ProjectorOptions{ManualReplay: manual})
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
	h.access = entity.NewManagerAccess(entity.NewEntityManager())
	h.runtime, err = engine.NewRuntime(store, wal, h.projector, outbox, h.access, nil, nil, engine.PipelinedRuntimeConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.runtime.Start(ctx); err != nil {
		t.Fatal(err)
	}
	options := append(h.runtime.NestOptions(), nest.NestOptionWithGetter(h.access), nest.NestOptionWithWorkerPools(nest.WorkerPoolConfig{Workers: 2, QueueCap: 32}, nest.WorkerPoolConfig{}, nest.WorkerPoolConfig{}))
	h.scheduler = nest.NewEngine(options...)
	if err := h.scheduler.Start(); err != nil {
		t.Fatal(err)
	}
	// 与进程装配一样把驱动接到这个 Nest 上；只接线，不启动循环——循环由用例在核对冷加载索引之后启动。
	if err := player.TimerDriver.Bind(h.scheduler, func(id int64) entity.IThreadSafeEntity { return h.access.Manager().Get(id) }); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.close(t) })
	return h
}

// close 先停投递循环再关 Nest 与 Runtime：停机之后循环不能再向已关闭的引擎投递。未启动时 Stop 直接返回；
// close 也登记在 t.Cleanup 里，用例失败提前退出时同样先停循环。只执行一次。
func (h *mmoRealRuntime) close(t *testing.T) {
	t.Helper()
	h.closeOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if err := player.TimerDriver.Stop(ctx); err != nil {
			t.Error(err)
		}
		if err := h.scheduler.Shutdown(ctx); err != nil {
			t.Error(err)
		}
		if err := h.runtime.Shutdown(ctx); err != nil {
			t.Error(err)
		}
	})
}

// view 经 Nest 读一个实体（必要时冷加载）。
func (h *mmoRealRuntime) view(t *testing.T, id int64) mmoRealState {
	t.Helper()
	value, err := h.scheduler.Request(context.Background(), mmoRealView, id, nil)
	if err != nil {
		t.Fatal(err)
	}
	return value.(mmoRealState)
}

// seed 写入一个实体的初始数值（strict 提交）。
func (h *mmoRealRuntime) seed(t *testing.T, id, gold, items, hp, area int64) {
	t.Helper()
	if _, err := h.scheduler.Request(context.Background(), mmoRealSeed, id, nest.NewParams(gold, items, hp, area)); err != nil {
		t.Fatal(err)
	}
}

// mmoTimerCounter 读 Player 驱动的一个 entitytimer 计数（进程内缺省注册表）；用前后差值判断循环投递了几次。
func mmoTimerCounter(name string) int64 {
	var total int64
	for _, m := range metrics.Snapshot() {
		if m.Name == name && m.Labels["driver"] == player.TimerDriver.Name() {
			total += m.Value
		}
	}
	return total
}

// awaitMMOLoop 等投递循环把 cond 变为真（cond 只经 Nest 读状态），再等 Player 驱动的投递计数恰为 delivered。
// 计数在投递准入之后才加，可能略晚于读到的提交结果，所以有界轮询；计数超过 delivered 立即失败。
func awaitMMOLoop(t *testing.T, step string, deliveredBefore, delivered int64, cond func() bool) {
	t.Helper()
	until := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(until) {
			t.Fatalf("driver loop did not deliver %s", step)
		}
		time.Sleep(10 * time.Millisecond)
	}
	for {
		got := mmoTimerCounter(entitytimer.DeliveredMetric) - deliveredBefore
		if got == delivered {
			return
		}
		if got > delivered || time.Now().After(until) {
			t.Fatalf("after %s: loop deliveries=%d want %d", step, got, delivered)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// 两次独立 go test 进程：第一阶段停在已持久化的单方确认后正常停机，第二阶段冷加载后完成交易、重放战斗领奖，
// 再启动 Player 驱动的投递循环，由循环投递离线期间到期的 Buff。恢复来源是正常停机时已投影到 Mongo 的文档
// （崩溃后从未投影 WAL 恢复见 TestMMORPGRealCrashRecovery）。
// 真实依赖准入由脚本显式设置，普通纵切不复制本文件，避免把 skip 或零测试记作恢复通过。
func TestMMORPGRealPersistenceAcrossProcesses(t *testing.T) {
	phase := os.Getenv("ROOST_MMORPG_PHASE")
	if phase != "write" && phase != "recover" {
		t.Fatal("write or recover phase required")
	}
	now := mmoRealT0
	if phase == "recover" {
		// 两次运行之间“离线”一小时：Buff（T0 + 2 秒）早已过期。
		now += time.Hour.Milliseconds()
	}
	h := openMMORealRuntime(t, os.Getenv("ROOST_MMORPG_WAL"), false, now)
	ctx := context.Background()
	build := func(unique int64, kind entity.EntityKind) int64 {
		id, err := entity.BuildEntityID(unique, kind)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	a, b := build(7001, player.EntityKindPlayer), build(7002, player.EntityKindPlayer)
	tr, mob, area := build(7001, trade.EntityKindTrade), build(7001, monster.EntityKindMonster), build(7001, scene.EntityKindScene)
	s := sender.NewMmoGameplaySender(h.scheduler)
	if phase == "write" {
		for _, id := range []int64{a, b, tr, mob} {
			kind := player.EntityKindPlayer
			if id == tr {
				kind = trade.EntityKindTrade
			}
			if id == mob {
				kind = monster.EntityKindMonster
			}
			if _, err := h.access.Create(&entity.EntityCreateParam{IsCreate: true, Id: id, Kind: kind}); err != nil {
				t.Fatal(err)
			}
		}
		h.seed(t, a, 50, 3, 100, area)
		h.seed(t, b, 50, 3, 100, area)
		h.seed(t, mob, 0, 0, 20, area)
		if err := s.MultiSync_MMORPGStrike(ctx, a, mob, 1, mmoRealT0); err != nil {
			t.Fatal(err)
		}
		if err := s.MultiSync_MMORPGStrike(ctx, a, mob, 2, mmoRealT0+1000); err != nil {
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
		if err := s.MultiSync_MMORPGOffer(ctx, tr, a, b, 10, 1, mmoRealT0); err != nil {
			t.Fatal(err)
		}
		if err := s.MultiSync_MMORPGResolveTrade(ctx, tr, a, b, a, 1, mmoRealT0); err != nil {
			t.Fatal(err)
		}
	} else {
		loaded, err := h.access.Get(ctx, tr, entity.EntityCategoryWorld)
		if err != nil {
			t.Fatal(err)
		}
		if d := loaded.(*trade.Trade).Dao(); d.GetStatus() != 1 || d.GetSellerConfirmed() != 1 || d.GetBuyerConfirmed() != 0 {
			t.Fatal("partial confirmation was not recovered")
		}
		for range 2 {
			if err := s.MultiSync_MMORPGResolveTrade(ctx, tr, a, b, b, 1, mmoRealT0+1000); err != nil {
				t.Fatal(err)
			}
		}
		if err := s.MultiSync_MMORPGStrike(ctx, a, mob, 2, mmoRealT0+1000); err != nil {
			t.Fatal(err)
		}
		if err := s.MultiSync_MMORPGLoot(ctx, a, mob); err != nil {
			t.Fatal(err)
		}
		if err := s.Sync_MMORPGQuest(ctx, a); err != nil {
			t.Fatal(err)
		}
		// 冷加载时 Host 把存量 Buff 期限报告给驱动索引。到这里投递循环还没启动，直接读 DAO 不会与 Tick 并发。
		seller, err := h.access.Get(ctx, a, entity.EntityCategoryOther)
		if err != nil {
			t.Fatal(err)
		}
		if seller.(*player.Player).Dao().GetBuffValue() != 7 {
			t.Fatal("Buff was not recovered before its deadline was delivered")
		}
		if due, ok := player.TimerDriver.DueOf(seller); !ok || due.UnixMilli() != mmoRealT0+2000 {
			t.Fatalf("cold load did not report the stored Buff deadline to the driver: due=%v ok=%v", due, ok)
		}
		// 启动驱动自己的投递循环（与进程装配同一个入口）；用例不手发 Tick、不调用 DeliverDue。离线一小时的 Buff
		// 只补触发这一次；之后 Player 上没有节点，索引不再有它的期限，循环不会再投递。
		deliveredBefore, redeliveredBefore := mmoTimerCounter(entitytimer.DeliveredMetric), mmoTimerCounter(entitytimer.RedeliveredMetric)
		if err := player.TimerDriver.Start(); err != nil {
			t.Fatal(err)
		}
		awaitMMOLoop(t, "the overdue Buff expiry", deliveredBefore, 1, func() bool {
			v := h.view(t, a)
			return v.BuffValue == 0 && v.BuffTimer == 0 && v.Timers == 0
		})
		if due, ok := player.TimerDriver.DueOf(seller); ok {
			t.Fatalf("expired Buff left a deadline in the driver index: %v", due)
		}
		if got := mmoTimerCounter(entitytimer.RedeliveredMetric) - redeliveredBefore; got != 0 {
			t.Fatalf("driver redelivered %d ticks: the Tick commit did not report its next deadline", got)
		}
		d := h.view(t, a)
		if d.Gold != 65 || d.Items != 3 || d.QuestKills != 1 || d.QuestClaimed != 1 || d.BuffValue != 0 {
			t.Fatalf("recovered seller gold=%d items=%d kills=%d claim=%d buff=%d", d.Gold, d.Items, d.QuestKills, d.QuestClaimed, d.BuffValue)
		}
		p := h.view(t, b)
		if p.Gold != 40 || p.Items != 4 {
			t.Fatal("recovered buyer state disagrees")
		}
		if got := mmoTimerCounter(entitytimer.DeliveredMetric) - deliveredBefore; got != 1 {
			t.Fatalf("driver loop delivered %d ticks in total, want 1", got)
		}
		// 先停投递循环再关 Nest（close 里也会再停一次）。
		if err := player.TimerDriver.Stop(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.runtime.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	h.close(t)
	if t.Failed() {
		return
	}
	// Runtime 已排空且关闭 WAL；确认不是只在内存中跑过，恢复阶段再清理本次唯一库。
	if phase == "recover" {
		if err := h.mongo.Database(h.database).Drop(ctx); err != nil {
			t.Fatal(err)
		}
	}
}
