package handler

import (
	"context"
	"encoding/json"
	"errors"
	db "example.com/survival-extraction-practice/db"
	playerentity "example.com/survival-extraction-practice/game/entities/player"
	runentity "example.com/survival-extraction-practice/game/entities/survival_raid"
	sender "example.com/survival-extraction-practice/game/handler/syncsender"
	survival "example.com/survival-extraction-practice/game/survival"
	coredata "github.com/tjbdwanghaibo/roost-core/framework/dataengine"
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

type survivalNoEffects struct{}

func (survivalNoEffects) Publish(context.Context, engine.OutboxItem) error {
	return errors.New("unexpected effect in local survival workflow")
}

type survivalRuntime struct {
	runtime   *engine.Runtime
	projector *engine.Projector
	scheduler *nest.NestMgr
	access    *entity.ManagerAccess
	mongo     *mongodriver.Client
	database  string
	runID     int64
	ids       [survival.Seats]int64
	clock     atomic.Int64
}

func openSurvivalRuntime(t *testing.T, manual bool, now int64) *survivalRuntime {
	t.Helper()
	uri := os.Getenv("ROOST_DATAENGINE_IT_MONGO_URI")
	if uri == "" {
		t.Fatal("isolated Mongo replica set required")
	}
	// DAO 写逻辑库 db=main（骨架默认）；本次运行的唯一物理库经映射表给出，与进程的 dataengine.databases 同一条规则。
	if logical := db.NewPlayerDao().DbName(); logical != db.DatabaseMain {
		t.Fatalf("Player DAO logical database %q, want %q", logical, db.DatabaseMain)
	}
	database := os.Getenv("ROOST_SURVIVAL_EXTRACTION_DB")
	databases, _, err := coredata.NewDatabaseMap(coredata.DatabaseMapConfig{Declared: coredata.DeclaredDatabases(), Configured: map[string]string{db.DatabaseMain: database}})
	if err != nil {
		t.Fatal(err)
	}
	if database != os.Getenv("ROOST_SURVIVAL_EXTRACTION_DB") || !strings.HasPrefix(database, "roost_survival_extraction_it_") {
		t.Fatal("refusing a database outside this consumer")
	}
	playerentity.RegisterEntity()
	runentity.RegisterEntity()
	RegisterSurvivalExtractionNestHandlers()
	RegisterTickSurvivalRaidTimerGenNestHandlers()
	h := &survivalRuntime{database: database}
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
	store, err := engine.NewMongoStore(client, engine.MongoStoreConfig{DefaultDatabase: database, Databases: databases})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.EnsureInfrastructure(ctx); err != nil {
		t.Fatal(err)
	}
	wal, err := nestwal.Open(nestwal.DefaultOptions(filepath.Join(os.Getenv("ROOST_SURVIVAL_EXTRACTION_WAL"), "wal")))
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
	outbox, err := engine.NewOutboxWorker(outboxStore, survivalNoEffects{}, engine.OutboxWorkerOptions{Owner: "survival-extraction-practice"})
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
	h.runID, err = entity.BuildEntityID(82001, runentity.EntityKindSurvivalRaid)
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
func (h *survivalRuntime) close(t *testing.T) {
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

// survivalTimerCounter 读 SurvivalRaid 驱动的一个 entitytimer 计数（进程内缺省注册表）；用前后差值判断循环投递了几次。
func survivalTimerCounter(name string) int64 {
	var total int64
	for _, m := range metrics.Snapshot() {
		if m.Name == name && m.Labels["driver"] == runentity.TimerDriver.Name() {
			total += m.Value
		}
	}
	return total
}

type survivalBarrier struct {
	State       string
	Timer       int64
	Gold        [survival.Seats]int64
	Unprojected uint64
}

func survivalRuntimeState(t *testing.T, h *survivalRuntime) (*runentity.SurvivalRaid, survival.State) {
	t.Helper()
	v, e := h.access.Get(context.Background(), h.runID, entity.EntityCategoryWorld)
	if e != nil {
		t.Fatal(e)
	}
	m := v.(*runentity.SurvivalRaid)
	s, e := survival.Decode(m.Dao().GetState())
	if e != nil {
		t.Fatal(e)
	}
	return m, s
}
func survivalFacts(t *testing.T, h *survivalRuntime) survivalBarrier {
	t.Helper()
	m, s := survivalRuntimeState(t, h)
	b := survivalBarrier{State: s.Encode(), Timer: m.Dao().GetWakeTimer()}
	for i, id := range h.ids {
		v, e := h.access.Get(context.Background(), id, entity.EntityCategoryOther)
		if e != nil {
			t.Fatal(e)
		}
		b.Gold[i] = v.(*playerentity.Player).Dao().GetGold()
	}
	return b
}
func writeSurvivalBarrier(t *testing.T, root, name string, b survivalBarrier) {
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
func TestSurvivalExtractionRealCrashRecovery(t *testing.T) {
	role := os.Getenv("ROOST_SURVIVAL_EXTRACTION_CHILD")
	root := os.Getenv("ROOST_SURVIVAL_EXTRACTION_WAL")
	ctx := context.Background()
	if role == "write" {
		h := openSurvivalRuntime(t, true, survivalNow)
		s := sender.NewSurvivalExtractionSender(h.scheduler)
		for _, id := range append(h.ids[:], h.runID) {
			kind := playerentity.EntityKindPlayer
			if id == h.runID {
				kind = runentity.EntityKindSurvivalRaid
			}
			if _, e := h.access.Create(&entity.EntityCreateParam{IsCreate: true, Id: id, Kind: kind}); e != nil {
				t.Fatal(e)
			}
		}
		for _, id := range h.ids {
			if e := s.Sync_SurvivalPlayerInit(ctx, id); e != nil {
				t.Fatal(e)
			}
		}
		dir := filepath.Join(root, "catalog")
		if e := os.MkdirAll(dir, 0700); e != nil {
			t.Fatal(e)
		}
		raw, _ := json.Marshal(survival.DemoRules())
		if e := os.WriteFile(filepath.Join(dir, "survival.json"), raw, 0600); e != nil {
			t.Fatal(e)
		}
		store, e := survival.OpenCatalog(dir)
		if e != nil {
			t.Fatal(e)
		}
		rules, e := survival.CaptureRules(store.Current())
		if e != nil {
			t.Fatal(e)
		}
		if _, e = s.Sync_SurvivalInit(ctx, h.runID, h.ids, rules, survivalNow); e != nil {
			t.Fatal(e)
		}
		for _, id := range h.ids {
			if e := s.MultiSync_SurvivalEnter(ctx, id, h.runID, h.clock.Load()); e != nil {
				t.Fatal(e)
			}
		}
		command := func(i int, kind string, a int) {
			t.Helper()
			h.clock.Add(100)
			v, e := s.Sync_SurvivalView(ctx, h.runID, h.ids[i])
			if e != nil {
				t.Fatal(e)
			}
			if _, e = s.Sync_SurvivalCommand(ctx, h.runID, h.ids[i], survival.Command{Seq: v.Self.NextSeq, Epoch: v.Self.Epoch, Kind: kind, A: a}, h.clock.Load()); e != nil {
				t.Fatal(e)
			}
		}
		command(0, "craft", 0)
		command(1, "move", 1)
		command(1, "gather", 0)
		command(1, "move", 0)
		command(1, "extract", 0)
		command(2, "move", 1)
		m, state := survivalRuntimeState(t, h)
		stats := h.projector.Stats()
		if stats.Projected != 0 || stats.WALUnacked != 15 || state.Seats[0].CraftAt == 0 || state.Seats[1].ExtractAt == 0 || m.Dao().GetWakeTimer() == 0 {
			t.Fatal("not at unprojected crafting / extraction boundary")
		}
		b := survivalBarrier{State: state.Encode(), Timer: m.Dao().GetWakeTimer(), Unprojected: stats.WALUnacked}
		writeSurvivalBarrier(t, root, "ready.json", b)
		select {}
	}
	if role == "verify" {
		h := openSurvivalRuntime(t, false, survivalNow+7200000)
		raw, e := os.ReadFile(filepath.Join(root, "claimed.json"))
		if e != nil {
			t.Fatal(e)
		}
		var want survivalBarrier
		if e = json.Unmarshal(raw, &want); e != nil {
			t.Fatal(e)
		}
		if got := survivalFacts(t, h); got != want {
			t.Fatal("cold facts differ before retries")
		}
		s := sender.NewSurvivalExtractionSender(h.scheduler)
		_, state := survivalRuntimeState(t, h)
		for i, id := range h.ids {
			if e := s.MultiSync_SurvivalEnter(ctx, id, h.runID, h.clock.Load()); e != nil {
				t.Fatal(e)
			}
			for _, r := range state.Seats[i].Receipts {
				got, e := s.Sync_SurvivalCommand(ctx, h.runID, id, r.Command, h.clock.Load())
				if e != nil || got != r {
					t.Fatal("command replay changed", e)
				}
			}
			if _, e = s.MultiSync_SurvivalClaim(ctx, id, h.runID); e != nil {
				t.Fatal(e)
			}
		}
		if got := survivalFacts(t, h); got != want {
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
	child := exec.Command(os.Args[0], "-test.run=^TestSurvivalExtractionRealCrashRecovery$", "-test.v", "-test.timeout=90s")
	child.Env = append(os.Environ(), "ROOST_SURVIVAL_EXTRACTION_CHILD=write")
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
	var barrier survivalBarrier
	if e = json.Unmarshal(raw, &barrier); e != nil {
		t.Fatal(e)
	}
	if e = child.Process.Kill(); e != nil {
		t.Fatal(e)
	}
	if e = <-done; e == nil {
		t.Fatal("child did not crash")
	}
	h := openSurvivalRuntime(t, false, survivalNow+10000)
	m, state := survivalRuntimeState(t, h)
	if state.Encode() != barrier.State || m.Dao().GetWakeTimer() != barrier.Timer {
		t.Fatal("lost materials, jobs, extraction manifest, receipts or timer")
	}
	s := sender.NewSurvivalExtractionSender(h.scheduler)
	for i, id := range h.ids {
		for _, r := range state.Seats[i].Receipts {
			got, e := s.Sync_SurvivalCommand(ctx, h.runID, id, r.Command, h.clock.Load())
			if e != nil || got != r {
				t.Fatal("retry changed material or extraction facts", e)
			}
		}
	}
	// 冷加载时 Host 把存量最早期限报告给驱动索引；到这里投递循环还没启动，直接读 DAO 不会与 Tick 并发。
	if due, ok := runentity.TimerDriver.DueOf(m); !ok || due.UnixMilli() != state.Deadline {
		t.Fatal("cold load did not report deadline")
	}
	// 启动驱动自己的投递循环（与进程装配同一入口），用例不调用 DeliverDue。离线期间撤离、制作、两次环境期限都已过，
	// 框架只补触发一次；业务回调在这一次里完成全部已到期的制作 / 撤离，环境伤害只算一次，EndAt 不变。
	deliveredBefore, redeliveredBefore := survivalTimerCounter(entitytimer.DeliveredMetric), survivalTimerCounter(entitytimer.RedeliveredMetric)
	if e = runentity.TimerDriver.Start(); e != nil {
		t.Fatal(e)
	}
	// awaitDelivered 等新期限进入索引（deadline 为 0 表示不再有期限）且投递计数恰为 want；超过 want 立即失败。
	awaitDelivered := func(want int64, deadline int64) {
		t.Helper()
		until := time.Now().Add(8 * time.Second)
		for {
			due, ok := runentity.TimerDriver.DueOf(m)
			got := survivalTimerCounter(entitytimer.DeliveredMetric) - deliveredBefore
			if got == want && (deadline == 0 && !ok || ok && due.UnixMilli() == deadline) {
				return
			}
			if got > want || time.Now().After(until) {
				t.Fatalf("deliveries=%d want %d; due=%v ok=%v want %d", got, want, due, ok, deadline)
			}
			time.Sleep(time.Millisecond)
		}
	}
	view := func() survival.View {
		v, e := s.Sync_SurvivalView(ctx, h.runID, h.ids[0])
		if e != nil {
			t.Fatal(e)
		}
		return v
	}
	waitSurvival(t, func() bool { return view().LastAt == h.clock.Load() })
	_, caught := survivalRuntimeState(t, h)
	if caught.Seats[0].Inventory != (survival.Inventory{0, 0, 0, 1}) || caught.Seats[1].Status != "extracted" || caught.Seats[1].Extracted != (survival.Inventory{3, 0, 1, 0}) || caught.Seats[2].HP != 5 || caught.EndAt != state.EndAt {
		t.Fatal("cold driver lost materials, duplicated damage or extended raid")
	}
	awaitDelivered(1, caught.Deadline)
	for _, id := range h.ids {
		if e := s.MultiSync_SurvivalEnter(ctx, id, h.runID, h.clock.Load()); e != nil {
			t.Fatal(e)
		}
	}
	if amount, e := s.MultiSync_SurvivalClaim(ctx, h.ids[1], h.runID); e != nil || amount != 4 {
		t.Fatal("first extraction payout", amount, e)
	}
	command := func(i int, kind string, a int) {
		t.Helper()
		v, e := s.Sync_SurvivalView(ctx, h.runID, h.ids[i])
		if e != nil {
			t.Fatal(e)
		}
		if _, e = s.Sync_SurvivalCommand(ctx, h.runID, h.ids[i], survival.Command{Seq: v.Self.NextSeq, Epoch: v.Self.Epoch, Kind: kind, A: a}, h.clock.Load()); e != nil {
			t.Fatal(e)
		}
	}
	command(0, "extract", 0)
	command(2, "move", 0)
	command(2, "extract", 0)
	command(3, "extract", 0)
	v := view()
	awaitDelivered(1, v.Deadline)
	// 三人撤离读条的完成时刻相同，是下一个最早期限；业务时钟推进到它，循环按时投递一次，三人同时封存、本局结束。
	h.clock.Store(v.Deadline)
	waitSurvival(t, func() bool { return view().Phase == "done" })
	awaitDelivered(2, 0)
	if got := survivalTimerCounter(entitytimer.RedeliveredMetric) - redeliveredBefore; got != 0 {
		t.Fatalf("driver redelivered %d ticks: a Tick commit did not report its next deadline", got)
	}
	if e = runentity.TimerDriver.Stop(ctx); e != nil {
		t.Fatal(e)
	}
	for _, id := range h.ids {
		for range 3 {
			if _, e = s.MultiSync_SurvivalClaim(ctx, id, h.runID); e != nil {
				t.Fatal(e)
			}
		}
	}
	final := survivalFacts(t, h)
	_, state = survivalRuntimeState(t, h)
	if final.Gold != [survival.Seats]int64{100, 99, 98, 98} || state.Crafted != 1 {
		t.Fatal("entry fee or sealed payout duplicated", final.Gold)
	}
	writeSurvivalBarrier(t, root, "claimed.json", final)
	h.close(t)
	verify := exec.Command(os.Args[0], "-test.run=^TestSurvivalExtractionRealCrashRecovery$", "-test.v", "-test.timeout=60s")
	verify.Env = append(os.Environ(), "ROOST_SURVIVAL_EXTRACTION_CHILD=verify")
	if output, e := verify.CombinedOutput(); e != nil {
		t.Fatalf("third process: %v\n%s", e, output)
	}
	if e = h.mongo.Database(h.database).Drop(ctx); e != nil {
		t.Fatal(e)
	}
	t.Logf("SIGKILL: %d unprojected strict records; four-player materials, crafting and extraction restored; driver loop delivered one overdue tick after cold load and the next deadline on time; third process retried admission, commands and sealed payouts", barrier.Unprojected)
}
