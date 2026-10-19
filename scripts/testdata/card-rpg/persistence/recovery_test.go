package handler

import (
	"context"
	"encoding/json"
	"errors"
	db "example.com/card-rpg-practice/db"
	"example.com/card-rpg-practice/game/cardrpg"
	runentity "example.com/card-rpg-practice/game/entities/card_run"
	playerentity "example.com/card-rpg-practice/game/entities/player"
	sender "example.com/card-rpg-practice/game/handler/syncsender"
	"fmt"
	"github.com/tjbdwanghaibo/roost-core/framework/dataengine/engine"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/nest"
	"github.com/tjbdwanghaibo/roost-core/framework/nestwal"
	fmongo "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo"
	mongodriver "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo/driver"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type cardNoEffects struct{}

func (cardNoEffects) Publish(context.Context, engine.OutboxItem) error {
	return errors.New("unexpected effect in local card RPG workflow")
}

type cardRuntime struct {
	runtime         *engine.Runtime
	projector       *engine.Projector
	scheduler       *nest.NestMgr
	access          *entity.ManagerAccess
	mongo           *mongodriver.Client
	database        string
	playerID, runID int64
	clock           atomic.Int64
}

func openCardRuntime(t *testing.T, manual bool, now int64) *cardRuntime {
	t.Helper()
	uri := os.Getenv("ROOST_DATAENGINE_IT_MONGO_URI")
	if uri == "" {
		t.Fatal("isolated Mongo replica set required")
	}
	database := db.NewPlayerDao().DbName()
	if database != os.Getenv("ROOST_CARD_RPG_DB") || !strings.HasPrefix(database, "roost_card_rpg_it_") {
		t.Fatal("refusing a database outside this consumer")
	}
	playerentity.RegisterEntity()
	runentity.RegisterEntity()
	RegisterCardRpgNestHandlers()
	RegisterTickCardRunTimerGenNestHandlers()
	h := &cardRuntime{database: database}
	h.clock.Store(now)
	runentity.TimerDriver.SetClock(func() time.Time { return time.UnixMilli(h.clock.Load()) })
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
	wal, err := nestwal.Open(nestwal.DefaultOptions(filepath.Join(os.Getenv("ROOST_CARD_RPG_WAL"), "wal")))
	if err != nil {
		t.Fatal(err)
	}
	projector, err := engine.NewProjector(wal, store, engine.ProjectorOptions{ManualReplay: manual})
	if err != nil {
		t.Fatal(err)
	}
	h.projector = projector
	outboxStore, err := engine.NewMongoOutboxStore(store)
	if err != nil {
		t.Fatal(err)
	}
	outbox, err := engine.NewOutboxWorker(outboxStore, cardNoEffects{}, engine.OutboxWorkerOptions{Owner: "card-rpg-practice"})
	if err != nil {
		t.Fatal(err)
	}
	h.access = entity.NewManagerAccess(entity.NewEntityManager())
	h.runtime, err = engine.NewRuntime(store, wal, projector, outbox, h.access, nil, nil, engine.PipelinedRuntimeConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.runtime.Start(ctx); err != nil {
		t.Fatal(err)
	}
	h.scheduler = nest.NewEngine(append(h.runtime.NestOptions(), nest.NestOptionWithGetter(h.access))...)
	if err := h.scheduler.Start(); err != nil {
		t.Fatal(err)
	}
	if err := runentity.TimerDriver.Bind(h.scheduler, func(id int64) entity.IThreadSafeEntity { return h.access.Manager().Get(id) }); err != nil {
		t.Fatal(err)
	}
	h.playerID, err = entity.BuildEntityID(81001, playerentity.EntityKindPlayer)
	if err != nil {
		t.Fatal(err)
	}
	h.runID, err = entity.BuildEntityID(82001, runentity.EntityKindCardRun)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.close(t) })
	return h
}
func (h *cardRuntime) close(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := h.scheduler.Shutdown(ctx); err != nil {
		t.Error(err)
	}
	if err := h.runtime.Shutdown(ctx); err != nil {
		t.Error(err)
	}
}

type cardBarrier struct {
	BattleDigest             string
	Gems, Gold, Dust, Energy int64
	Unprojected              uint64
	TurnTimer                int64
}

// strict 记录未投影时由父进程 SIGKILL 自己的子进程。新进程重放原 WAL、冷加载定时器、续战并重试领奖。
// 这里只覆盖已确认提交，不模拟磁盘断电或 strict 回复未知。
func TestCardRPGRealCrashRecovery(t *testing.T) {
	const baseNow int64 = 1800000000000
	if os.Getenv("ROOST_CARD_RPG_CHILD") == "verify" {
		h := openCardRuntime(t, false, baseNow+7200000)
		ctx := context.Background()
		s := sender.NewCardRpgSender(h.scheduler)
		raw, err := os.ReadFile(filepath.Join(os.Getenv("ROOST_CARD_RPG_WAL"), "claimed.json"))
		if err != nil {
			t.Fatal(err)
		}
		var expected struct {
			Profile cardrpg.ProfileView
			Digest  string
		}
		if err := json.Unmarshal(raw, &expected); err != nil {
			t.Fatal(err)
		}
		view, err := s.Sync_CardProfile(ctx, h.playerID)
		if err != nil || !reflect.DeepEqual(view, expected.Profile) {
			t.Fatal("cold reward facts differ before retry", err)
		}
		run, err := h.access.Get(ctx, h.runID, entity.EntityCategoryWorld)
		if err != nil {
			t.Fatal(err)
		}
		b, err := cardrpg.DecodeBattle(run.(*runentity.CardRun).Dao().GetState())
		if err != nil || b.Digest() != expected.Digest || !b.Claimed || run.(*runentity.CardRun).Dao().GetTurnTimer() != 0 {
			t.Fatal("cold terminal / timer not retained", err)
		}
		for range 2 {
			if _, err := s.MultiSync_CardClaim(ctx, h.playerID, h.runID); err != nil {
				t.Fatal(err)
			}
		}
		patched := cardrpg.DemoRules()
		patched.Version = "after-restart"
		patched.DrawCost = 99
		if _, err := s.Sync_CardDraw(ctx, h.playerID, 1, 10, patched); err != nil {
			t.Fatal(err)
		}
		view, err = s.Sync_CardProfile(ctx, h.playerID)
		if err != nil || !reflect.DeepEqual(view, expected.Profile) {
			t.Fatal("cold retry duplicated rewards / spend", err)
		}
		return
	}
	if os.Getenv("ROOST_CARD_RPG_CHILD") == "write" {
		h := openCardRuntime(t, true, baseNow)
		ctx := context.Background()
		s := sender.NewCardRpgSender(h.scheduler)
		rules := loadCardRules(t, cardrpg.DemoRules())
		for _, id := range []int64{h.playerID, h.runID} {
			kind := playerentity.EntityKindPlayer
			if id == h.runID {
				kind = runentity.EntityKindCardRun
			}
			if _, err := h.access.Create(&entity.EntityCreateParam{IsCreate: true, Id: id, Kind: kind}); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := s.Sync_CardInit(ctx, h.playerID, strings.Repeat("ab", 32), baseNow); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Sync_CardDraw(ctx, h.playerID, 1, 10, rules); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Sync_CardManage(ctx, h.playerID, cardrpg.Manage{Seq: 1, Kind: "level", CardID: 1, Target: 2}, rules, baseNow); err != nil {
			t.Fatal(err)
		}
		if _, err := s.MultiSync_CardStart(ctx, h.playerID, h.runID, 1, 1, rules, baseNow); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Sync_CardTurn(ctx, h.runID, h.playerID, cardrpg.TurnCommand{Seq: 1, ActorID: h.playerID, Kind: "guard"}, h.clock.Add(1)); err != nil {
			t.Fatal(err)
		}
		player, err := h.access.Get(ctx, h.playerID, entity.EntityCategoryOther)
		if err != nil {
			t.Fatal(err)
		}
		run, err := h.access.Get(ctx, h.runID, entity.EntityCategoryWorld)
		if err != nil {
			t.Fatal(err)
		}
		p, err := cardrpg.DecodeProfile(player.(*playerentity.Player).Dao().GetState())
		if err != nil {
			t.Fatal(err)
		}
		b, err := cardrpg.DecodeBattle(run.(*runentity.CardRun).Dao().GetState())
		if err != nil {
			t.Fatal(err)
		}
		stats := h.projector.Stats()
		if stats.Projected != 0 || stats.WALUnacked == 0 {
			t.Fatal("no unprojected strict WAL at crash boundary")
		}
		barrier := cardBarrier{b.Digest(), p.Gems, p.Gold, p.Dust, p.Energy, stats.WALUnacked, run.(*runentity.CardRun).Dao().GetTurnTimer()}
		raw, _ := json.Marshal(barrier)
		if err := os.WriteFile(filepath.Join(os.Getenv("ROOST_CARD_RPG_WAL"), "ready.json"), raw, 0600); err != nil {
			t.Fatal(err)
		}
		select {} // 父进程读到 barrier 后 SIGKILL；不会执行 Runtime / Nest cleanup。
	}
	root := os.Getenv("ROOST_CARD_RPG_WAL")
	if root == "" {
		t.Fatal("unique WAL directory required")
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(root, "child.log")
	out, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	child := exec.Command(os.Args[0], "-test.run=^TestCardRPGRealCrashRecovery$", "-test.v", "-test.timeout=90s")
	child.Env = append(os.Environ(), "ROOST_CARD_RPG_CHILD=write")
	child.Stdout = out
	child.Stderr = out
	if err := child.Start(); err != nil {
		_ = out.Close()
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { err := child.Wait(); _ = out.Close(); done <- err; close(done) }()
	t.Cleanup(func() {
		select {
		case <-done:
		default:
			_ = child.Process.Kill()
			<-done
		}
	})
	var raw []byte
	deadline := time.NewTimer(30 * time.Second)
	defer deadline.Stop()
	poll := time.NewTicker(25 * time.Millisecond)
	defer poll.Stop()
	for raw == nil {
		value, err := os.ReadFile(filepath.Join(root, "ready.json"))
		if err == nil {
			raw = value
			break
		}
		if !os.IsNotExist(err) {
			t.Fatal(err)
		}
		select {
		case err := <-done:
			log, _ := os.ReadFile(logPath)
			t.Fatalf("child failed before barrier: %v\n%s", err, log)
		case <-deadline.C:
			t.Fatal("child deadline: " + logPath)
		case <-poll.C:
		}
	}
	var barrier cardBarrier
	if err := json.Unmarshal(raw, &barrier); err != nil {
		t.Fatal(err)
	}
	if err := child.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err == nil {
		t.Fatal("child did not crash")
	}
	h := openCardRuntime(t, false, baseNow+3600000)
	ctx := context.Background()
	s := sender.NewCardRpgSender(h.scheduler)
	player, err := h.access.Get(ctx, h.playerID, entity.EntityCategoryOther)
	if err != nil {
		t.Fatal(err)
	}
	run, err := h.access.Get(ctx, h.runID, entity.EntityCategoryWorld)
	if err != nil {
		t.Fatal(err)
	}
	p, err := cardrpg.DecodeProfile(player.(*playerentity.Player).Dao().GetState())
	if err != nil {
		t.Fatal(err)
	}
	b, err := cardrpg.DecodeBattle(run.(*runentity.CardRun).Dao().GetState())
	if err != nil {
		t.Fatal(err)
	}
	if b.Digest() != barrier.BattleDigest || b.NextSeq() != 2 || p.Gems != barrier.Gems || p.Gold != barrier.Gold || p.Dust != barrier.Dust || p.Energy != barrier.Energy || run.(*runentity.CardRun).Dao().GetTurnTimer() != barrier.TurnTimer {
		t.Fatal("WAL recovery lost state / timer")
	}
	// 已确认抽卡与养成在新配置下重试，仍沿用原结果；运行中关卡继续用旧配置。
	patched := cardrpg.DemoRules()
	patched.Version = "cards-v2"
	patched.DrawCost = 99
	patched.Heroes[0].Attack = 90
	if receipt, err := s.Sync_CardDraw(ctx, h.playerID, 1, 10, patched); err != nil || receipt.Version != "cards-v1" {
		t.Fatal("recovered draw rerolled / repriced", err)
	}
	if _, err := s.Sync_CardManage(ctx, h.playerID, cardrpg.Manage{Seq: 1, Kind: "level", CardID: 1, Target: 2}, patched, h.clock.Load()); err != nil {
		t.Fatal(err)
	}
	if _, err := sender.NewTickCardRunTimerGenSender(h.scheduler).Sync_TickCardRunTimer(ctx, h.runID, h.clock.Load()); err != nil {
		t.Fatal(err)
	}
	b, err = cardrpg.DecodeBattle(run.(*runentity.CardRun).Dao().GetState())
	if err != nil {
		t.Fatal(err)
	}
	if b.NextSeq() != 3 || b.Log[1].Command.Kind != "timeout" || b.State.Deadline != h.clock.Load()+cardrpg.TurnMillis || b.Rules.Version != "cards-v1" {
		t.Fatal("cold timer skipped turns / changed frozen rules")
	}
	if result := finishCardBattle(t, h.scheduler, h.runID, h.playerID, &h.clock); result.Status != "won" {
		t.Fatal("recovered battle did not win")
	}
	for range 3 {
		if _, err := s.MultiSync_CardClaim(ctx, h.playerID, h.runID); err != nil {
			t.Fatal(err)
		}
	}
	p, err = cardrpg.DecodeProfile(player.(*playerentity.Player).Dao().GetState())
	if err != nil {
		t.Fatal(err)
	}
	if p.Gems != barrier.Gems || p.Gold != barrier.Gold+8 || p.Dust != barrier.Dust+1 || p.Wins != 1 || len(p.Draws) != 1 || len(p.Operations) != 1 || len(p.Runs) != 1 || !p.Runs[0].Claimed {
		t.Fatal("recovery repeated spend or rewards")
	}
	finalBattle, err := cardrpg.DecodeBattle(run.(*runentity.CardRun).Dao().GetState())
	if err != nil {
		t.Fatal(err)
	}
	expectedRaw, _ := json.Marshal(struct {
		Profile cardrpg.ProfileView
		Digest  string
	}{p.View(), finalBattle.Digest()})
	if err := os.WriteFile(filepath.Join(root, "claimed.json"), expectedRaw, 0600); err != nil {
		t.Fatal(err)
	}
	h.close(t)
	verify := exec.Command(os.Args[0], "-test.run=^TestCardRPGRealCrashRecovery$", "-test.v", "-test.timeout=60s")
	verify.Env = append(os.Environ(), "ROOST_CARD_RPG_CHILD=verify")
	if output, err := verify.CombinedOutput(); err != nil {
		t.Fatalf("cold reward verification failed: %v\n%s", err, output)
	}
	if err := h.mongo.Database(h.database).Drop(ctx); err != nil {
		t.Fatal(err)
	}
	t.Log(fmt.Sprintf("SIGKILL recovery: %d unprojected strict records; draw/growth unchanged; turn timer resumed once; won, claimed and retried after another process restart exactly once", barrier.Unprojected))
}
