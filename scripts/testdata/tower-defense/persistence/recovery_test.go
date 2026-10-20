package handler

import (
	"context"
	"encoding/json"
	"errors"
	db "example.com/tower-defense-practice/db"
	playerentity "example.com/tower-defense-practice/game/entities/player"
	runentity "example.com/tower-defense-practice/game/entities/tower_room"
	sender "example.com/tower-defense-practice/game/handler/syncsender"
	tower "example.com/tower-defense-practice/game/towerdefense"
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
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type towerNoEffects struct{}

func (towerNoEffects) Publish(context.Context, engine.OutboxItem) error {
	return errors.New("unexpected effect in local tower defense workflow")
}

type towerRuntime struct {
	runtime   *engine.Runtime
	projector *engine.Projector
	scheduler *nest.NestMgr
	access    *entity.ManagerAccess
	mongo     *mongodriver.Client
	database  string
	runID     int64
	ids       [tower.Seats]int64
	clock     atomic.Int64
}

func openTowerRuntime(t *testing.T, manual bool, now int64) *towerRuntime {
	t.Helper()
	uri := os.Getenv("ROOST_DATAENGINE_IT_MONGO_URI")
	if uri == "" {
		t.Fatal("isolated Mongo replica set required")
	}
	database := db.NewPlayerDao().DbName()
	if database != os.Getenv("ROOST_TOWER_DEFENSE_DB") || !strings.HasPrefix(database, "roost_tower_defense_it_") {
		t.Fatal("refusing a database outside this consumer")
	}
	playerentity.RegisterEntity()
	runentity.RegisterEntity()
	RegisterTowerDefenseNestHandlers()
	RegisterTickTowerRoomTimerGenNestHandlers()
	h := &towerRuntime{database: database}
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
	wal, err := nestwal.Open(nestwal.DefaultOptions(filepath.Join(os.Getenv("ROOST_TOWER_DEFENSE_WAL"), "wal")))
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
	outbox, err := engine.NewOutboxWorker(outboxStore, towerNoEffects{}, engine.OutboxWorkerOptions{Owner: "tower-defense-practice"})
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
	h.runID, err = entity.BuildEntityID(82001, runentity.EntityKindTowerRoom)
	if err != nil {
		t.Fatal(err)
	}
	for i := range h.ids {
		h.ids[i], err = entity.BuildEntityID(int64(81001+i), playerentity.EntityKindPlayer)
		if err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { h.close(t) })
	return h
}
func (h *towerRuntime) close(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := runentity.TimerDriver.Stop(ctx); err != nil {
		t.Error(err)
	}
	if err := h.scheduler.Shutdown(ctx); err != nil {
		t.Error(err)
	}
	if err := h.runtime.Shutdown(ctx); err != nil {
		t.Error(err)
	}
}

// towerTimerCounter 读 TowerRoom 驱动的一个 entitytimer 计数（进程内缺省注册表）；用前后差值判断循环投递了几次。
func towerTimerCounter(name string) int64 {
	var total int64
	for _, m := range metrics.Snapshot() {
		if m.Name == name && m.Labels["driver"] == runentity.TimerDriver.Name() {
			total += m.Value
		}
	}
	return total
}

type towerBarrier struct {
	State       string
	Timer       int64
	Gold        [tower.Seats]int64
	Unprojected uint64
}

func towerRuntimeState(t *testing.T, h *towerRuntime) (*runentity.TowerRoom, tower.State) {
	t.Helper()
	v, e := h.access.Get(context.Background(), h.runID, entity.EntityCategoryWorld)
	if e != nil {
		t.Fatal(e)
	}
	m := v.(*runentity.TowerRoom)
	s, e := tower.Decode(m.Dao().GetState())
	if e != nil {
		t.Fatal(e)
	}
	return m, s
}
func towerFacts(t *testing.T, h *towerRuntime) towerBarrier {
	t.Helper()
	m, s := towerRuntimeState(t, h)
	b := towerBarrier{State: s.Encode(), Timer: m.Dao().GetWakeTimer()}
	for i, id := range h.ids {
		v, e := h.access.Get(context.Background(), id, entity.EntityCategoryOther)
		if e != nil {
			t.Fatal(e)
		}
		b.Gold[i] = v.(*playerentity.Player).Dao().GetGold()
	}
	return b
}
func writeTowerBarrier(t *testing.T, root, name string, b towerBarrier) {
	t.Helper()
	raw, e := json.Marshal(b)
	if e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(filepath.Join(root, name), raw, 0600); e != nil {
		t.Fatal(e)
	}
}

// SIGKILL 仅针对本用例创建的子进程；已确认 strict WAL 不等于 Mongo 已投影，也不等于磁盘断电验收。
func TestTowerDefenseRealCrashRecovery(t *testing.T) {
	role := os.Getenv("ROOST_TOWER_DEFENSE_CHILD")
	root := os.Getenv("ROOST_TOWER_DEFENSE_WAL")
	ctx := context.Background()
	if role == "write" {
		h := openTowerRuntime(t, true, towerNow)
		s := sender.NewTowerDefenseSender(h.scheduler)
		for _, id := range append(h.ids[:], h.runID) {
			kind := playerentity.EntityKindPlayer
			if id == h.runID {
				kind = runentity.EntityKindTowerRoom
			}
			if _, e := h.access.Create(&entity.EntityCreateParam{IsCreate: true, Id: id, Kind: kind}); e != nil {
				t.Fatal(e)
			}
		}
		for _, id := range h.ids {
			if e := s.Sync_TowerPlayerInit(ctx, id); e != nil {
				t.Fatal(e)
			}
		}
		dir := filepath.Join(root, "catalog")
		if e := os.MkdirAll(dir, 0700); e != nil {
			t.Fatal(e)
		}
		raw, _ := json.Marshal(tower.DemoRules())
		if e := os.WriteFile(filepath.Join(dir, "tower.json"), raw, 0600); e != nil {
			t.Fatal(e)
		}
		store, e := tower.OpenCatalog(dir)
		if e != nil {
			t.Fatal(e)
		}
		rules, e := tower.CaptureRules(store.Current())
		if e != nil {
			t.Fatal(e)
		}
		if _, e = s.Sync_TowerInit(ctx, h.runID, h.ids, rules, towerNow); e != nil {
			t.Fatal(e)
		}
		for i, id := range h.ids {
			for j, kind := range []string{"build", "ready"} {
				a, b := 0, 0
				if kind == "build" {
					a = []int{1, 6, 11, 16}[i]
				}
				if _, e = s.Sync_TowerCommand(ctx, h.runID, id, tower.Command{Seq: uint64(j + 1), Epoch: 1, Round: 1, Kind: kind, A: a, B: b}, towerNow); e != nil {
					t.Fatal(i, kind, e)
				}
			}
		}
		m, state := towerRuntimeState(t, h)
		h.clock.Store(state.Deadline)
		waitTower(t, func() bool { due, ok := runentity.TimerDriver.DueOf(m); return ok && due.UnixMilli() == state.Deadline })
		if n := runentity.TimerDriver.DeliverDue(ctx); n != 1 {
			t.Fatal("first wave step not delivered", n)
		}
		waitTower(t, func() bool { v, e := s.Sync_TowerView(ctx, h.runID, h.ids[0]); return e == nil && v.Tick == 1 })
		m, state = towerRuntimeState(t, h)
		stats := h.projector.Stats()
		if stats.Projected != 0 || stats.WALUnacked != 14 || state.Phase != "battle" || state.Tick != 1 || m.Dao().GetWakeTimer() == 0 {
			t.Fatal("not at unprojected active wave boundary")
		}
		b := towerBarrier{State: state.Encode(), Timer: m.Dao().GetWakeTimer(), Unprojected: stats.WALUnacked}
		writeTowerBarrier(t, root, "ready.json", b)
		select {}
	}
	if role == "verify" {
		h := openTowerRuntime(t, false, towerNow+7200000)
		raw, e := os.ReadFile(filepath.Join(root, "claimed.json"))
		if e != nil {
			t.Fatal(e)
		}
		var want towerBarrier
		if e = json.Unmarshal(raw, &want); e != nil {
			t.Fatal(e)
		}
		if got := towerFacts(t, h); got != want {
			t.Fatal("cold facts differ before retries")
		}
		s := sender.NewTowerDefenseSender(h.scheduler)
		_, state := towerRuntimeState(t, h)
		for i, id := range h.ids {
			for _, r := range state.Seats[i].Receipts {
				got, e := s.Sync_TowerCommand(ctx, h.runID, id, r.Command, h.clock.Load())
				if e != nil || got != r {
					t.Fatal("command replay changed", e)
				}
			}
			if _, e = s.MultiSync_TowerClaim(ctx, id, h.runID); e != nil {
				t.Fatal(e)
			}
		}
		if got := towerFacts(t, h); got != want {
			t.Fatal("duplicate rewards after third process restart")
		}
		return
	}
	if root == "" {
		t.Fatal("unique WAL directory required")
	}
	if e := os.MkdirAll(root, 0700); e != nil {
		t.Fatal(e)
	}
	logPath := filepath.Join(root, "child.log")
	out, e := os.Create(logPath)
	if e != nil {
		t.Fatal(e)
	}
	child := exec.Command(os.Args[0], "-test.run=^TestTowerDefenseRealCrashRecovery$", "-test.v", "-test.timeout=90s")
	child.Env = append(os.Environ(), "ROOST_TOWER_DEFENSE_CHILD=write")
	child.Stdout = out
	child.Stderr = out
	if e = child.Start(); e != nil {
		_ = out.Close()
		t.Fatal(e)
	}
	done := make(chan error, 1)
	go func() { e := child.Wait(); _ = out.Close(); done <- e; close(done) }()
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
		value, e := os.ReadFile(filepath.Join(root, "ready.json"))
		if e == nil {
			raw = value
			break
		}
		if !os.IsNotExist(e) {
			t.Fatal(e)
		}
		select {
		case e := <-done:
			log, _ := os.ReadFile(logPath)
			t.Fatalf("child failed: %v\n%s", e, log)
		case <-deadline.C:
			t.Fatal("child deadline: " + logPath)
		case <-poll.C:
		}
	}
	var barrier towerBarrier
	if e = json.Unmarshal(raw, &barrier); e != nil {
		t.Fatal(e)
	}
	if e = child.Process.Kill(); e != nil {
		t.Fatal(e)
	}
	if e = <-done; e == nil {
		t.Fatal("child did not crash")
	}
	h := openTowerRuntime(t, false, towerNow+3600000)
	m, state := towerRuntimeState(t, h)
	if state.Encode() != barrier.State || m.Dao().GetWakeTimer() != barrier.Timer {
		t.Fatal("lost towers, path, enemies, receipts or timer")
	}
	s := sender.NewTowerDefenseSender(h.scheduler)
	for i, id := range h.ids {
		for _, r := range state.Seats[i].Receipts {
			got, e := s.Sync_TowerCommand(ctx, h.runID, id, r.Command, h.clock.Load())
			if e != nil || got != r {
				t.Fatal("retry changed purchase or phase", e)
			}
		}
	}
	// 冷加载时 Host 把存量期限报告给驱动索引；到这里投递循环还没启动，直接读 DAO 不会与 Tick 并发。
	if due, ok := runentity.TimerDriver.DueOf(m); !ok || due.UnixMilli() != state.Deadline {
		t.Fatal("cold load did not report deadline")
	}
	// 启动驱动自己的投递循环（与进程装配同一入口），用例不调用 DeliverDue：循环发现已过期的战斗期限，
	// 给这个实体投递一次加载后的补触发。离线一小时只算一步（框架不追帧），下一期限从实际触发时间起算。
	deliveredBefore, redeliveredBefore := towerTimerCounter(entitytimer.DeliveredMetric), towerTimerCounter(entitytimer.RedeliveredMetric)
	if e = runentity.TimerDriver.Start(); e != nil {
		t.Fatal(e)
	}
	view := func() tower.View {
		v, e := s.Sync_TowerView(ctx, h.runID, h.ids[0])
		if e != nil {
			t.Fatal(e)
		}
		return v
	}
	// awaitDelivered 等新期限重新进入索引且投递计数恰为 want：新期限在 Tick 提交后报告、计数在投递准入后才加，
	// 都可能略晚于读到的提交结果，所以有界轮询；超过 want 立即失败（多投或重投）。
	awaitDelivered := func(want int64, deadline int64) {
		t.Helper()
		until := time.Now().Add(8 * time.Second)
		for {
			due, ok := runentity.TimerDriver.DueOf(m)
			got := towerTimerCounter(entitytimer.DeliveredMetric) - deliveredBefore
			if got == want && (deadline == 0 && !ok || ok && due.UnixMilli() == deadline) {
				return
			}
			if got > want || time.Now().After(until) {
				t.Fatalf("deliveries=%d want %d; due=%v ok=%v want %d", got, want, due, ok, deadline)
			}
			time.Sleep(time.Millisecond)
		}
	}
	waitTower(t, func() bool { return view().Tick == 2 })
	v := view()
	if v.Round != 1 || v.Deadline != h.clock.Load()+state.Rules.TickMillis {
		t.Fatal("offline loop skipped stages")
	}
	awaitDelivered(1, v.Deadline)
	// 之后每个期限由用例把业务时钟推进到期限本身，循环（至多 MaxWait 后重读时钟）按时投递一次。
	delivered := int64(1)
	for n := 0; n < 100 && v.Phase != "done"; n++ {
		before := v
		h.clock.Store(v.Deadline)
		waitTower(t, func() bool {
			v = view()
			return v.Round != before.Round || v.Phase != before.Phase || v.Tick != before.Tick
		})
		delivered++
		awaitDelivered(delivered, v.Deadline)
	}
	if v.Phase != "done" {
		t.Fatal("match did not terminate")
	}
	if got := towerTimerCounter(entitytimer.RedeliveredMetric) - redeliveredBefore; got != 0 {
		t.Fatalf("driver redelivered %d ticks: a Tick commit did not report its next deadline", got)
	}
	if e = runentity.TimerDriver.Stop(ctx); e != nil {
		t.Fatal(e)
	}
	for _, id := range h.ids {
		for range 3 {
			if _, e = s.MultiSync_TowerClaim(ctx, id, h.runID); e != nil {
				t.Fatal(e)
			}
		}
	}
	final := towerFacts(t, h)
	_, state = towerRuntimeState(t, h)
	for _, gold := range final.Gold {
		if gold != 70 || state.Result != "won" || state.Killed != 9 || state.Leaked != 0 {
			t.Fatal("wrong team reward")
		}
	}
	writeTowerBarrier(t, root, "claimed.json", final)
	h.close(t)
	verify := exec.Command(os.Args[0], "-test.run=^TestTowerDefenseRealCrashRecovery$", "-test.v", "-test.timeout=60s")
	verify.Env = append(os.Environ(), "ROOST_TOWER_DEFENSE_CHILD=verify")
	if output, e := verify.CombinedOutput(); e != nil {
		t.Fatalf("third process: %v\n%s", e, output)
	}
	if e = h.mongo.Database(h.database).Drop(ctx); e != nil {
		t.Fatal(e)
	}
	t.Logf("SIGKILL: %d unprojected strict records; four-player active wave restored, driver loop delivered one overdue step after cold load and %d deliveries in total to finish three waves, third process retried all commands and team rewards", barrier.Unprojected, delivered)
}
