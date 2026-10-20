package handler

import (
	"context"
	"encoding/json"
	"errors"
	db "example.com/idle-sim-practice/db"
	playerentity "example.com/idle-sim-practice/game/entities/player"
	sender "example.com/idle-sim-practice/game/handler/syncsender"
	idle "example.com/idle-sim-practice/game/idlesim"
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

type idleNoEffects struct{}

func (idleNoEffects) Publish(context.Context, engine.OutboxItem) error {
	return errors.New("unexpected effect in local idle simulation workflow")
}

type idleRuntime struct {
	runtime   *engine.Runtime
	projector *engine.Projector
	scheduler *nest.NestMgr
	access    *entity.ManagerAccess
	mongo     *mongodriver.Client
	database  string
	playerID  int64
	clock     atomic.Int64
}

func openIdleRuntime(t *testing.T, manual bool, now int64) *idleRuntime {
	t.Helper()
	uri := os.Getenv("ROOST_DATAENGINE_IT_MONGO_URI")
	if uri == "" {
		t.Fatal("isolated Mongo replica set required")
	}
	database := db.NewPlayerDao().DbName()
	if database != os.Getenv("ROOST_IDLE_SIM_DB") || !strings.HasPrefix(database, "roost_idle_sim_it_") {
		t.Fatal("refusing a database outside this consumer")
	}
	playerentity.RegisterEntity()
	RegisterIdleSimNestHandlers()
	RegisterTickPlayerTimerGenNestHandlers()
	h := &idleRuntime{database: database}
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
	wal, err := nestwal.Open(nestwal.DefaultOptions(filepath.Join(os.Getenv("ROOST_IDLE_SIM_WAL"), "wal")))
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
	outbox, err := engine.NewOutboxWorker(outboxStore, idleNoEffects{}, engine.OutboxWorkerOptions{Owner: "idle-sim-practice"})
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
func (h *idleRuntime) close(t *testing.T) {
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

// idleTimerCounter 读 Player 驱动的一个 entitytimer 计数（进程内缺省注册表）；用前后差值判断循环投递了几次。
func idleTimerCounter(name string) int64 {
	var total int64
	for _, m := range metrics.Snapshot() {
		if m.Name == name && m.Labels["driver"] == playerentity.TimerDriver.Name() {
			total += m.Value
		}
	}
	return total
}

type idleBarrier struct {
	Digest      string
	View        idle.View
	Timer       int64
	Unprojected uint64
}

func idleRuntimeState(t *testing.T, h *idleRuntime) (*playerentity.Player, idle.State) {
	t.Helper()
	e, err := h.access.Get(context.Background(), h.playerID, entity.EntityCategoryOther)
	if err != nil {
		t.Fatal(err)
	}
	p := e.(*playerentity.Player)
	s, err := idle.Decode(p.Dao().GetState())
	if err != nil {
		t.Fatal(err)
	}
	return p, s
}
func idleRuntimeCommand(t *testing.T, h *idleRuntime, kind string, station, recipe int) idle.Receipt {
	t.Helper()
	_, state := idleRuntimeState(t, h)
	c := idle.Command{Seq: uint64(len(state.Receipts)) + 1, Kind: kind, Station: station, Recipe: recipe}
	r, err := sender.NewIdleSimSender(h.scheduler).Sync_IdleCommand(context.Background(), h.playerID, c, h.clock.Load())
	if err != nil {
		t.Fatal(kind, err)
	}
	return r
}

// 只终止自己启动的写入子进程。已确认 strict WAL 保留在原目录；不模拟磁盘断电或未知提交回复。
func TestIdleSimRealCrashRecovery(t *testing.T) {
	role := os.Getenv("ROOST_IDLE_SIM_CHILD")
	root := os.Getenv("ROOST_IDLE_SIM_WAL")
	ctx := context.Background()
	if role == "write" {
		h := openIdleRuntime(t, true, idleNow)
		s := sender.NewIdleSimSender(h.scheduler)
		if _, err := h.access.Create(&entity.EntityCreateParam{IsCreate: true, Id: h.playerID, Kind: playerentity.EntityKindPlayer}); err != nil {
			t.Fatal(err)
		}
		// 崩溃子进程不能依赖 t.TempDir 清理，配置跟随本次 WAL 根保留 / 清理。
		dir := filepath.Join(root, "catalog")
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(idle.DemoRules())
		if err := os.WriteFile(filepath.Join(dir, "idle.json"), raw, 0600); err != nil {
			t.Fatal(err)
		}
		store, err := idle.OpenCatalog(dir)
		if err != nil {
			t.Fatal(err)
		}
		rules, err := idle.CaptureRules(store.Current())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Sync_IdleInit(ctx, h.playerID, idleNow, rules); err != nil {
			t.Fatal(err)
		}
		idleRuntimeCommand(t, h, "queue", 0, 0)
		idleRuntimeCommand(t, h, "queue", 1, 1)
		idleRuntimeCommand(t, h, "boost", 0, 0)
		h.clock.Add(5000)
		idleRuntimeCommand(t, h, "settle", 0, 0)
		p, state := idleRuntimeState(t, h)
		stats := h.projector.Stats()
		if stats.Projected != 0 || stats.WALUnacked != 5 || p.Dao().GetWakeTimer() == 0 || len(state.Stations[0].Ready) != 1 || state.Stations[1].Queue[0].Progress != 10000 {
			t.Fatal("missing strict crash boundary")
		}
		raw, _ = json.Marshal(idleBarrier{state.Digest(), state.View(), p.Dao().GetWakeTimer(), stats.WALUnacked})
		if err := os.WriteFile(filepath.Join(root, "ready.json"), raw, 0600); err != nil {
			t.Fatal(err)
		}
		select {}
	}
	if role == "verify" {
		h := openIdleRuntime(t, false, idleNow+7200000)
		p, state := idleRuntimeState(t, h)
		raw, err := os.ReadFile(filepath.Join(root, "claimed.json"))
		if err != nil {
			t.Fatal(err)
		}
		var expected idleBarrier
		if err := json.Unmarshal(raw, &expected); err != nil {
			t.Fatal(err)
		}
		if state.Digest() != expected.Digest || !reflect.DeepEqual(state.View(), expected.View) || p.Dao().GetWakeTimer() != expected.Timer {
			t.Fatal("cold facts differ before replay")
		}
		s := sender.NewIdleSimSender(h.scheduler)
		for _, r := range state.Receipts {
			got, err := s.Sync_IdleCommand(ctx, h.playerID, r.Command, h.clock.Load())
			if err != nil || got != r {
				t.Fatal("recovered identity changed", err)
			}
		}
		_, again := idleRuntimeState(t, h)
		if again.Digest() != expected.Digest {
			t.Fatal("cold replay advanced time / duplicated input or rewards")
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
	child := exec.Command(os.Args[0], "-test.run=^TestIdleSimRealCrashRecovery$", "-test.v", "-test.timeout=90s")
	child.Env = append(os.Environ(), "ROOST_IDLE_SIM_CHILD=write")
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
	var barrier idleBarrier
	if err := json.Unmarshal(raw, &barrier); err != nil {
		t.Fatal(err)
	}
	if err := child.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err == nil {
		t.Fatal("child did not crash")
	}
	h := openIdleRuntime(t, false, idleNow+3600000)
	p, state := idleRuntimeState(t, h)
	if state.Digest() != barrier.Digest || !reflect.DeepEqual(state.View(), barrier.View) || p.Dao().GetWakeTimer() != barrier.Timer {
		t.Fatal("WAL lost task / boost / asset / horizon / timer before mutation")
	}
	s := sender.NewIdleSimSender(h.scheduler)
	for _, r := range state.Receipts {
		got, err := s.Sync_IdleCommand(ctx, h.playerID, r.Command, h.clock.Load())
		if err != nil || got != r {
			t.Fatal("retry changed progress or payment", err)
		}
	}
	// 冷加载时 Host 把存量生产期限报告给驱动索引。到这里投递循环还没启动，上面直接读 DAO 不会与 Tick 并发。
	if due, ok := playerentity.TimerDriver.DueOf(p); !ok || due.UnixMilli() != state.NextDeadline() {
		t.Fatal("cold load failed to register production deadline")
	}
	// 启动驱动自己的投递循环（与进程装配同一个入口），用例不调用 DeliverDue、不手发 Tick。
	//
	// 本例“在线每秒看到收益涨”的准点刷新不是另一套唤醒：它就是这一个 Player 实体定时器节点（期限 =
	// min(Horizon, LastAt+TickMillis)，TickMillis 1 秒），到期由同一个驱动把生成的 Tick 作为 Nest 消息投递。
	// 所以每次每秒刷新都计入同一个 delivered_total，计数里没有别的来源。业务时钟在两次推进之间冻结，循环按
	// MaxWait 重读时钟，下一秒的期限只在用例推进时钟时到期，计数因此是确定的。
	deliveredBefore, redeliveredBefore := idleTimerCounter(entitytimer.DeliveredMetric), idleTimerCounter(entitytimer.RedeliveredMetric)
	if err := playerentity.TimerDriver.Start(); err != nil {
		t.Fatal(err)
	}
	// awaitLoop 等循环投递的 Tick 提交到业务时间 lastAt（经 Nest 读视图），再等投递计数恰为 delivered、索引里的
	// 期限为 deadline（0 表示不再有期限）。期限在 Tick 提交后（nest.AfterCommit）报告、计数在投递准入之后才加，
	// 都可能略晚于读到的提交结果，所以有界轮询；计数超过 delivered 立即失败。
	awaitLoop := func(step string, lastAt, delivered, deadline int64) {
		t.Helper()
		until := time.Now().Add(10 * time.Second)
		for {
			v, err := s.Sync_IdleView(ctx, h.playerID)
			if err != nil {
				t.Fatal(err)
			}
			if v.LastAt == lastAt {
				break
			}
			if time.Now().After(until) {
				t.Fatalf("driver loop did not deliver %s (business time %d, LastAt %d)", step, h.clock.Load(), v.LastAt)
			}
			time.Sleep(10 * time.Millisecond)
		}
		for {
			due, ok := playerentity.TimerDriver.DueOf(p)
			got := idleTimerCounter(entitytimer.DeliveredMetric) - deliveredBefore
			if got == delivered && (deadline == 0 && !ok || ok && due.UnixMilli() == deadline) {
				return
			}
			if got > delivered || time.Now().After(until) {
				t.Fatalf("after %s: loop deliveries=%d want %d; due=%v ok=%v want %d", step, got, delivered, due, ok, deadline)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	// 加载后补触发：离线一小时只投递这一次，Advance 把积分截到原 Horizon；窗口已过，不再排唤醒。
	awaitLoop("the overdue production catch-up", h.clock.Load(), 1, 0)
	_, caught := idleRuntimeState(t, h)
	if caught.Horizon != barrier.View.Horizon || caught.Pending != 4 || caught.IncomeRemainder != 5000 || caught.Raw != 45 || caught.Gems != 8 || len(caught.Stations[0].Ready) != 1 || len(caught.Stations[1].Ready) != 1 || p.Dao().GetWakeTimer() != 0 {
		t.Fatal("offline catch-up repriced jobs, extended horizon or repeated completed output")
	}
	// 下一个期限：玩家命令把窗口重新推到 now+OfflineCap，下一秒的刷新期限经提交进入索引；业务时间直接推进到
	// 这个期限，循环（至多 MaxWait 后重读时钟）按时投递一次，积分恰好一秒（收入等级 1，余数 5000 -> 6000，
	// 收益箱仍是 4），再排下一秒。期限之前业务时间冻结，条目不会到期，计数也就不会多出一次。
	idleRuntimeCommand(t, h, "collect", 0, 0)
	_, collected := idleRuntimeState(t, h)
	next := collected.NextDeadline()
	if next != h.clock.Load()+collected.Rules.TickMillis {
		t.Fatal("player command did not schedule the next one-second refresh")
	}
	awaitLoop("nothing before the new deadline", h.clock.Load(), 1, next)
	h.clock.Store(next)
	awaitLoop("the next one-second refresh", next, 2, next+collected.Rules.TickMillis)
	_, refreshed := idleRuntimeState(t, h)
	if refreshed.Horizon != collected.Horizon || refreshed.Pending != 4 || refreshed.IncomeRemainder != 6000 || len(refreshed.Receipts) != len(collected.Receipts) {
		t.Fatal("loop refresh did not integrate exactly one second at its deadline")
	}
	idleRuntimeCommand(t, h, "collect", 1, 0)
	claim := idleRuntimeCommand(t, h, "claim", 0, 0)
	idleRuntimeCommand(t, h, "daily", 0, 0)
	order := idleRuntimeCommand(t, h, "order", 0, 0)
	for _, r := range []idle.Receipt{claim, order} {
		for range 3 {
			got, err := s.Sync_IdleCommand(ctx, h.playerID, r.Command, h.clock.Load())
			if err != nil || got != r {
				t.Fatal("claim / order replay changed", err)
			}
		}
	}
	p, final := idleRuntimeState(t, h)
	if final.Gold != 129 || final.Pending != 0 || final.Inventory != [3]int64{} || final.Orders != 1 || final.Raw != 45 || final.Gems != 8 {
		t.Fatal("recovery changed economy")
	}
	expectedRaw, _ := json.Marshal(idleBarrier{Digest: final.Digest(), View: final.View(), Timer: p.Dao().GetWakeTimer()})
	if err := os.WriteFile(filepath.Join(root, "claimed.json"), expectedRaw, 0600); err != nil {
		t.Fatal(err)
	}
	// 业务时间停在第二次投递的期限上，之后的命令只把下一秒的期限换成新节点，循环一直在跑但没有再投递。
	if got := idleTimerCounter(entitytimer.DeliveredMetric) - deliveredBefore; got != 2 {
		t.Fatalf("driver loop delivered %d ticks in total, want 2", got)
	}
	if got := idleTimerCounter(entitytimer.RedeliveredMetric) - redeliveredBefore; got != 0 {
		t.Fatalf("driver redelivered %d ticks: a Tick commit did not report its next deadline", got)
	}
	// 先停投递循环再关 Nest（close 里也会再停一次）。
	if err := playerentity.TimerDriver.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	h.close(t)
	verify := exec.Command(os.Args[0], "-test.run=^TestIdleSimRealCrashRecovery$", "-test.v", "-test.timeout=60s")
	verify.Env = append(os.Environ(), "ROOST_IDLE_SIM_CHILD=verify")
	if output, err := verify.CombinedOutput(); err != nil {
		t.Fatalf("third-process cold verification failed: %v\n%s", err, output)
	}
	if err := h.mongo.Database(h.database).Drop(ctx); err != nil {
		t.Fatal(err)
	}
	t.Logf("SIGKILL: %d unprojected strict records; two queues / boost / horizon / timer restored, driver loop delivered one catch-up after cold load and the next one-second refresh at its deadline, third process replayed every command without duplicating costs or rewards", barrier.Unprojected)
}
