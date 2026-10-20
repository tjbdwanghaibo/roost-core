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
	"github.com/tjbdwanghaibo/roost-core/framework/nest/entitytimer"
	"github.com/tjbdwanghaibo/roost-core/framework/nestwal"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
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
	// 先停投递循环再关 Nest：停机之后循环不能再向已关闭的引擎投递。未启动时 Stop 直接返回；close 也登记在
	// t.Cleanup 里，用例失败提前退出时同样先停循环。
	if err := playerentity.TimerDriver.Stop(ctx); err != nil {
		t.Error(err)
	}
	if err := h.scheduler.Shutdown(ctx); err != nil {
		t.Error(err)
	}
	if err := h.runtime.Shutdown(ctx); err != nil {
		t.Error(err)
	}
}

// mergeTimerCounter 读 Player 驱动的一个 entitytimer 计数（进程内缺省注册表）；用前后差值判断循环投递了几次。
func mergeTimerCounter(name string) int64 {
	var total int64
	for _, m := range metrics.Snapshot() {
		if m.Name == name && m.Labels["driver"] == playerentity.TimerDriver.Name() {
			total += m.Value
		}
	}
	return total
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
	// 冷加载时 Host 把存量挑战期限报告给驱动索引。到这里投递循环还没启动，上面直接读 DAO 不会与 Tick 并发。
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
	// 启动驱动自己的投递循环（与进程装配同一个入口），用例不手发 Tick、不调用 DeliverDue：循环发现离线期间
	// 已过期的挑战，给这个 Player 投递一次加载后的补触发，判负。业务时钟在两次推进之间冻结，循环按 MaxWait
	// 重读时钟，所以“何时到期”完全由用例推进时钟决定。
	deliveredBefore, redeliveredBefore := mergeTimerCounter(entitytimer.DeliveredMetric), mergeTimerCounter(entitytimer.RedeliveredMetric)
	if err := playerentity.TimerDriver.Start(); err != nil {
		t.Fatal(err)
	}
	// awaitLoop 等循环投递的 Tick 提交（经 Nest 读视图，done 判断业务结果），再等投递计数恰为 delivered、索引
	// 里的期限为 deadline（0 表示不再有期限）。期限在 Tick 提交后（nest.AfterCommit）报告、计数在投递准入之后
	// 才加，都可能略晚于读到的提交结果，所以有界轮询；计数超过 delivered 立即失败。
	awaitLoop := func(step string, done func(casual.View) bool, delivered, deadline int64) {
		t.Helper()
		until := time.Now().Add(10 * time.Second)
		for {
			view, err := sender.Sync_MergeView(ctx, h.playerID)
			if err != nil {
				t.Fatal(err)
			}
			if done(view) {
				break
			}
			if time.Now().After(until) {
				t.Fatalf("driver loop did not deliver %s (business time %d): challenge=%+v", step, h.clock.Load(), view.Challenge)
			}
			time.Sleep(10 * time.Millisecond)
		}
		for {
			due, ok := playerentity.TimerDriver.DueOf(p)
			got := mergeTimerCounter(entitytimer.DeliveredMetric) - deliveredBefore
			if got == delivered && (deadline == 0 && !ok || ok && due.UnixMilli() == deadline) {
				return
			}
			if got > delivered || time.Now().After(until) {
				t.Fatalf("after %s: loop deliveries=%d want %d; due=%v ok=%v want %d", step, got, delivered, due, ok, deadline)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	// 加载后补触发：离线一小时只判负一次，按当前业务时间补体力 / 次数（封顶），挑战结束后不再挂期限。
	awaitLoop("the overdue challenge expiry", func(v casual.View) bool { return v.Challenge.Status == "lost" }, 1, 0)
	_, expired := mergeRuntimeState(t, h)
	if expired.Challenge.ID != state.Challenge.ID || expired.Revision != state.Revision+1 || expired.LastAt != h.clock.Load() || expired.Energy != 10 || expired.Charges != 3 {
		t.Fatal("cold deadline / capped recovery incorrect")
	}
	mergeRuntimeCommand(t, h, "claim", 0, 0, int64(expired.Challenge.ID))
	// 下一个期限：新开的挑战经提交进入索引（循环还没有再投递）；业务时间直接推进到期限本身，循环（至多
	// MaxWait 后重读时钟）按时投递一次，挑战在期限时刻判负。期限之前业务时间冻结，条目不会到期，计数也就
	// 不会多出一次。
	mergeRuntimeCommand(t, h, "start", 0, 0, 0)
	_, started := mergeRuntimeState(t, h)
	next := started.Challenge.Deadline
	awaitLoop("nothing before the new deadline", func(casual.View) bool { return true }, 1, next)
	h.clock.Store(next)
	awaitLoop("the new challenge deadline", func(v casual.View) bool { return v.Challenge.Status == "lost" }, 2, 0)
	_, timedOut := mergeRuntimeState(t, h)
	if timedOut.Challenge.ID != started.Challenge.ID || timedOut.Revision != started.Revision+1 || timedOut.LastAt != next {
		t.Fatal("loop did not expire the new challenge exactly once at its deadline")
	}
	mergeRuntimeCommand(t, h, "claim", 0, 0, int64(timedOut.Challenge.ID))
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
	// 第三局获胜时挑战结束、期限移出索引，业务时间停在第二个期限，循环一直在跑但没有再投递。
	if got := mergeTimerCounter(entitytimer.DeliveredMetric) - deliveredBefore; got != 2 {
		t.Fatalf("driver loop delivered %d ticks in total, want 2", got)
	}
	if got := mergeTimerCounter(entitytimer.RedeliveredMetric) - redeliveredBefore; got != 0 {
		t.Fatalf("driver redelivered %d ticks: a Tick commit did not report its next deadline", got)
	}
	// 先停投递循环再关 Nest（close 里也会再停一次）。
	if err := playerentity.TimerDriver.Stop(ctx); err != nil {
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
	t.Logf("SIGKILL: %d unprojected strict records; exact board / assets / timer restored, driver loop delivered one overdue expiry after cold load and the next challenge expiry at its deadline, server goal completed and rewards retried after third-process cold load", barrier.Unprojected)
}
