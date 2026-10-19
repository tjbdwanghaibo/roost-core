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
	if err := h.scheduler.Shutdown(ctx); err != nil {
		t.Error(err)
	}
	if err := h.runtime.Shutdown(ctx); err != nil {
		t.Error(err)
	}
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
	if due, ok := playerentity.TimerDriver.DueOf(p); !ok || due.UnixMilli() != state.NextDeadline() {
		t.Fatal("cold load failed to register production deadline")
	}
	if n := playerentity.TimerDriver.DeliverDue(ctx); n != 1 {
		t.Fatal("cold driver delivered wrong count", n)
	}
	waitIdle(t, func() bool {
		v, err := s.Sync_IdleView(ctx, h.playerID)
		return err == nil && v.LastAt == h.clock.Load()
	})
	waitIdle(t, func() bool { _, ok := playerentity.TimerDriver.DueOf(p); return !ok })
	_, caught := idleRuntimeState(t, h)
	if caught.Horizon != barrier.View.Horizon || caught.Pending != 4 || caught.IncomeRemainder != 5000 || caught.Raw != 45 || caught.Gems != 8 || len(caught.Stations[0].Ready) != 1 || len(caught.Stations[1].Ready) != 1 || p.Dao().GetWakeTimer() != 0 {
		t.Fatal("offline catch-up repriced jobs, extended horizon or repeated completed output")
	}
	idleRuntimeCommand(t, h, "collect", 0, 0)
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
	h.close(t)
	verify := exec.Command(os.Args[0], "-test.run=^TestIdleSimRealCrashRecovery$", "-test.v", "-test.timeout=60s")
	verify.Env = append(os.Environ(), "ROOST_IDLE_SIM_CHILD=verify")
	if output, err := verify.CombinedOutput(); err != nil {
		t.Fatalf("third-process cold verification failed: %v\n%s", err, output)
	}
	if err := h.mongo.Database(h.database).Drop(ctx); err != nil {
		t.Fatal(err)
	}
	t.Logf("SIGKILL: %d unprojected strict records; two queues / boost / horizon / timer restored, one driver catch-up, third process replayed every command without duplicating costs or rewards", barrier.Unprojected)
}
