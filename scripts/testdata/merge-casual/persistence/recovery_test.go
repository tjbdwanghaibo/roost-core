package handler

import (
	"context"
	"encoding/json"
	"errors"
	db "example.com/merge-casual-practice/db"
	playerentity "example.com/merge-casual-practice/game/entities/player"
	sender "example.com/merge-casual-practice/game/handler/syncsender"
	casual "example.com/merge-casual-practice/game/mergecasual"
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

type mergeNoEffects struct{}

func (mergeNoEffects) Publish(context.Context, engine.OutboxItem) error {
	return errors.New("unexpected effect in local merge casual workflow")
}

type mergeRuntime struct {
	runtime   *engine.Runtime
	projector *engine.Projector
	scheduler *nest.NestMgr
	access    *entity.ManagerAccess
	mongo     *mongodriver.Client
	database  string
	playerID  int64
	clock     atomic.Int64
}

func openMergeRuntime(t *testing.T, manual bool, now int64) *mergeRuntime {
	t.Helper()
	uri := os.Getenv("ROOST_DATAENGINE_IT_MONGO_URI")
	if uri == "" {
		t.Fatal("isolated Mongo replica set required")
	}
	database := db.NewPlayerDao().DbName()
	if database != os.Getenv("ROOST_MERGE_CASUAL_DB") || !strings.HasPrefix(database, "roost_merge_casual_it_") {
		t.Fatal("refusing a database outside this consumer")
	}
	playerentity.RegisterEntity()
	RegisterMergeCasualNestHandlers()
	RegisterTickPlayerTimerGenNestHandlers()
	h := &mergeRuntime{database: database}
	h.clock.Store(now)
	playerentity.TimerDriver.SetClock(func() time.Time { return time.UnixMilli(h.clock.Load()) })
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
	wal, err := nestwal.Open(nestwal.DefaultOptions(filepath.Join(os.Getenv("ROOST_MERGE_CASUAL_WAL"), "wal")))
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
	outbox, err := engine.NewOutboxWorker(outboxStore, mergeNoEffects{}, engine.OutboxWorkerOptions{Owner: "merge-casual-practice"})
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
	if err := playerentity.TimerDriver.Bind(h.scheduler, func(id int64) entity.IThreadSafeEntity { return h.access.Manager().Get(id) }); err != nil {
		t.Fatal(err)
	}
	h.playerID, err = entity.BuildEntityID(81001, playerentity.EntityKindPlayer)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.close(t) })
	return h
}
func (h *mergeRuntime) close(t *testing.T) {
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

type mergeBarrier struct {
	Digest      string
	View        casual.View
	Timer       int64
	Unprojected uint64
}

func mergeProof(owner int64) casual.SignedReward {
	return casual.SignReward(casual.SignedReward{ID: "crash-ad", PlayerID: owner, ExpiresAt: mergeNow + 1000, Kind: "energy"}, adKey)
}
func mergeRuntimeState(t *testing.T, h *mergeRuntime) (*playerentity.Player, casual.State) {
	t.Helper()
	e, err := h.access.Get(context.Background(), h.playerID, entity.EntityCategoryOther)
	if err != nil {
		t.Fatal(err)
	}
	p := e.(*playerentity.Player)
	s, err := casual.Decode(p.Dao().GetState())
	if err != nil {
		t.Fatal(err)
	}
	return p, s
}
func mergeRuntimeCommand(t *testing.T, h *mergeRuntime, kind string, a, b int, value int64) casual.Receipt {
	t.Helper()
	_, state := mergeRuntimeState(t, h)
	c := casual.Command{Epoch: state.Epoch, Seq: uint64(len(state.Receipts)) + 1, Base: state.Revision, Kind: kind, A: a, B: b, Value: value}
	r, err := sender.NewMergeCasualSender(h.scheduler).Sync_MergeBatch(context.Background(), h.playerID, []casual.Command{c}, h.clock.Load())
	if err != nil {
		t.Fatal(kind, err)
	}
	return r[0]
}

// 父进程只 SIGKILL 自己创建的子进程。strict 已确认但尚未投影时崩溃，保留原 WAL 恢复；不模拟断电或结果未知。
func TestMergeCasualRealCrashRecovery(t *testing.T) {
	role := os.Getenv("ROOST_MERGE_CASUAL_CHILD")
	root := os.Getenv("ROOST_MERGE_CASUAL_WAL")
	ctx := context.Background()
	if role == "write" {
		h := openMergeRuntime(t, true, mergeNow)
		s := sender.NewMergeCasualSender(h.scheduler)
		if _, err := h.access.Create(&entity.EntityCreateParam{IsCreate: true, Id: h.playerID, Kind: playerentity.EntityKindPlayer}); err != nil {
			t.Fatal(err)
		}
		// 配置目录放在本次 WAL 根下而不是 t.TempDir：子进程被 SIGKILL，不会执行 TempDir 清理，否则每次运行都在
		// TMPDIR 留下一份残留目录；脚本成功后整体删除 WAL 根。
		catalogDir := filepath.Join(root, "catalog")
		rawRules, _ := json.Marshal(casual.DemoRules())
		if err := os.MkdirAll(catalogDir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(catalogDir, "merge.json"), rawRules, 0600); err != nil {
			t.Fatal(err)
		}
		store, err := casual.OpenCatalog(catalogDir)
		if err != nil {
			t.Fatal(err)
		}
		rules, err := casual.CaptureRules(store.Current())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Sync_MergeLogin(ctx, h.playerID, mergeKey, 1, mergeNow, rules); err != nil {
			t.Fatal(err)
		}
		mergeRuntimeCommand(t, h, "produce", 10, 0, 0)
		mergeRuntimeCommand(t, h, "produce", 11, 0, 0)
		if _, err := s.Sync_MergeAd(ctx, h.playerID, mergeProof(h.playerID), adKey, 1, mergeNow); err != nil {
			t.Fatal(err)
		}
		mergeRuntimeCommand(t, h, "start", 0, 0, 0)
		p, state := mergeRuntimeState(t, h)
		stats := h.projector.Stats()
		// 首登、两次生产、广告到账、开挑战各一条 strict 记录；后台投影关闭，五条都未投影。
		if stats.Projected != 0 || stats.WALUnacked != 5 || p.Dao().GetWakeTimer() == 0 {
			t.Fatal("missing unprojected strict records / timer")
		}
		raw, _ := json.Marshal(mergeBarrier{state.Digest(), state.View(), p.Dao().GetWakeTimer(), stats.WALUnacked})
		if err := os.WriteFile(filepath.Join(root, "ready.json"), raw, 0600); err != nil {
			t.Fatal(err)
		}
		select {}
	}
	if role == "verify" {
		h := openMergeRuntime(t, false, mergeNow+7200000)
		p, state := mergeRuntimeState(t, h)
		raw, err := os.ReadFile(filepath.Join(root, "claimed.json"))
		if err != nil {
			t.Fatal(err)
		}
		var expected mergeBarrier
		if err := json.Unmarshal(raw, &expected); err != nil {
			t.Fatal(err)
		}
		if state.Digest() != expected.Digest || state.View() != expected.View || !state.Challenge.Claimed || p.Dao().GetWakeTimer() != 0 {
			t.Fatal("cold facts differ before retry")
		}
		sender := sender.NewMergeCasualSender(h.scheduler)
		for _, c := range []casual.Command{state.Receipts[0].Command, state.Receipts[len(state.Receipts)-1].Command} {
			if _, err := sender.Sync_MergeBatch(ctx, h.playerID, []casual.Command{c}, h.clock.Load()); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := sender.Sync_MergeAd(ctx, h.playerID, mergeProof(h.playerID), adKey, 1, h.clock.Load()); err != nil {
			t.Fatal(err)
		}
		_, again := mergeRuntimeState(t, h)
		if again.Digest() != expected.Digest {
			t.Fatal("restart retry duplicated production / advertisement / challenge reward")
		}
		return
	}
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
	child := exec.Command(os.Args[0], "-test.run=^TestMergeCasualRealCrashRecovery$", "-test.v", "-test.timeout=90s")
	child.Env = append(os.Environ(), "ROOST_MERGE_CASUAL_CHILD=write")
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
	var barrier mergeBarrier
	if err := json.Unmarshal(raw, &barrier); err != nil {
		t.Fatal(err)
	}
	if err := child.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err == nil {
		t.Fatal("child did not crash")
	}
	h := openMergeRuntime(t, false, mergeNow+3600000)
	p, state := mergeRuntimeState(t, h)
	if state.Digest() != barrier.Digest || state.View() != barrier.View || p.Dao().GetWakeTimer() != barrier.Timer {
		t.Fatal("WAL recovery lost board / asset / receipt / timer before mutations")
	}
	// 冷加载时 Host 把存量挑战期限报告给驱动索引。本用例不启动投递循环，下面由用例手发一次生成的 Tick
	// （循环每轮做的也是向到期实体投递这条 Tick）；投递循环本身见卡牌 RPG / 自走棋的恢复用例。
	if due, ok := playerentity.TimerDriver.DueOf(p); !ok || due.UnixMilli() != state.Challenge.Deadline {
		t.Fatalf("cold load did not report the stored challenge deadline: due=%v ok=%v want=%d", due, ok, state.Challenge.Deadline)
	}
	sender := sender.NewMergeCasualSender(h.scheduler)
	first := state.Receipts[0]
	if result, err := sender.Sync_MergeBatch(ctx, h.playerID, []casual.Command{first.Command}, h.clock.Load()); err != nil || !reflect.DeepEqual(result, []casual.Receipt{first}) {
		t.Fatal("recovered producer rerolled", err)
	}
	if _, err := sender.Sync_MergeAd(ctx, h.playerID, mergeProof(h.playerID), adKey, 1, h.clock.Load()); err != nil {
		t.Fatal("expired original proof lost receipt", err)
	}
	if _, err := senderTick(h).Sync_TickPlayerTimer(ctx, h.playerID, h.clock.Load()); err != nil {
		t.Fatal(err)
	}
	_, expired := mergeRuntimeState(t, h)
	if expired.Challenge.Status != "lost" || expired.Energy != 10 || expired.Charges != 3 {
		t.Fatal("cold deadline / capped recovery incorrect")
	}
	mergeRuntimeCommand(t, h, "claim", 0, 0, int64(expired.Challenge.ID))
	mergeRuntimeCommand(t, h, "start", 0, 0, 0)
	mergeWin(t, h.scheduler, h.playerID, h.clock.Load())
	_, won := mergeRuntimeState(t, h)
	if won.Challenge.Status != "won" {
		t.Fatal("server merge goal absent")
	}
	claim := mergeRuntimeCommand(t, h, "claim", 0, 0, int64(won.Challenge.ID))
	for range 3 {
		if _, err := sender.Sync_MergeBatch(ctx, h.playerID, []casual.Command{claim.Command}, h.clock.Load()); err != nil {
			t.Fatal(err)
		}
	}
	p, final := mergeRuntimeState(t, h)
	if final.Gold != barrier.View.Gold+casual.DemoRules().ChallengeGold || len(final.Ads) != 1 || final.Board[10].Item != barrier.View.Board[10].Item || final.Board[11].Item != barrier.View.Board[11].Item || p.Dao().GetWakeTimer() != 0 {
		t.Fatal("recovered claim duplicated or changed producer output")
	}
	expectedRaw, _ := json.Marshal(mergeBarrier{Digest: final.Digest(), View: final.View()})
	if err := os.WriteFile(filepath.Join(root, "claimed.json"), expectedRaw, 0600); err != nil {
		t.Fatal(err)
	}
	h.close(t)
	verify := exec.Command(os.Args[0], "-test.run=^TestMergeCasualRealCrashRecovery$", "-test.v", "-test.timeout=60s")
	verify.Env = append(os.Environ(), "ROOST_MERGE_CASUAL_CHILD=verify")
	if output, err := verify.CombinedOutput(); err != nil {
		t.Fatalf("cold verification failed: %v\n%s", err, output)
	}
	if err := h.mongo.Database(h.database).Drop(ctx); err != nil {
		t.Fatal(err)
	}
	t.Logf("SIGKILL: %d unprojected strict records; exact board / assets / timer restored, one overdue expiry, server goal completed and rewards retried after third-process cold load", barrier.Unprojected)
}
func senderTick(h *mergeRuntime) *sender.TickPlayerTimerGenSender {
	return sender.NewTickPlayerTimerGenSender(h.scheduler)
}
