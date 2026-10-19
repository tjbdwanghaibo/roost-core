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
	"github.com/tjbdwanghaibo/roost-core/framework/nestwal"
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
	return errors.New("unexpected effect in local card RPG workflow")
}

type towerRuntime struct {
	runtime         *engine.Runtime
	projector       *engine.Projector
	scheduler       *nest.NestMgr
	access          *entity.ManagerAccess
	mongo           *mongodriver.Client
	database        string
	playerID, runID int64
	ids             [tower.Seats]int64
	clock           atomic.Int64
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
	h.playerID, err = entity.BuildEntityID(81001, playerentity.EntityKindPlayer)
	if err != nil {
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
	if due, ok := runentity.TimerDriver.DueOf(m); !ok || due.UnixMilli() != state.Deadline {
		t.Fatal("cold load did not report deadline")
	}
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
	waitTower(t, func() bool { return view().Tick == 2 })
	v := view()
	if v.Round != 1 || v.Deadline != h.clock.Load()+state.Rules.TickMillis {
		t.Fatal("offline loop skipped stages")
	}
	// 驱动在冻结业务时钟下每次只推进一个阶段；同一循环继续准时处理下一期限。
	for n := 0; n < 100 && v.Phase != "done"; n++ {
		waitTower(t, func() bool { due, ok := runentity.TimerDriver.DueOf(m); return ok && due.UnixMilli() == v.Deadline })
		before := v
		h.clock.Store(v.Deadline)
		waitTower(t, func() bool {
			v = view()
			return v.Round != before.Round || v.Phase != before.Phase || v.Tick != before.Tick
		})
	}
	if v.Phase != "done" {
		t.Fatal("match did not terminate")
	}
	waitTower(t, func() bool { _, ok := runentity.TimerDriver.DueOf(m); return !ok })
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
	t.Logf("SIGKILL: %d unprojected strict records; four-player active wave restored, driver loop resumed one step and completed three waves, third process retried all commands and team rewards", barrier.Unprojected)
}
