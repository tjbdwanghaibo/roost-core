package handler

import (
	"context"
	"encoding/json"
	"errors"
	db "example.com/auto-chess-practice/db"
	chess "example.com/auto-chess-practice/game/autochess"
	runentity "example.com/auto-chess-practice/game/entities/chess_match"
	playerentity "example.com/auto-chess-practice/game/entities/player"
	sender "example.com/auto-chess-practice/game/handler/syncsender"
	"fmt"
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

type chessNoEffects struct{}

func (chessNoEffects) Publish(context.Context, engine.OutboxItem) error {
	return errors.New("unexpected effect in local card RPG workflow")
}

type chessRuntime struct {
	runtime         *engine.Runtime
	projector       *engine.Projector
	scheduler       *nest.NestMgr
	access          *entity.ManagerAccess
	mongo           *mongodriver.Client
	database        string
	playerID, runID int64
	ids             [chess.Seats]int64
	clock           atomic.Int64
}

func openChessRuntime(t *testing.T, manual bool, now int64) *chessRuntime {
	t.Helper()
	uri := os.Getenv("ROOST_DATAENGINE_IT_MONGO_URI")
	if uri == "" {
		t.Fatal("isolated Mongo replica set required")
	}
	// DAO 写逻辑库 db=main（骨架默认）；本次运行的唯一物理库经映射表给出，与进程的 dataengine.databases 同一条规则。
	if logical := db.NewPlayerDao().DbName(); logical != db.DatabaseMain {
		t.Fatalf("Player DAO logical database %q, want %q", logical, db.DatabaseMain)
	}
	database := os.Getenv("ROOST_AUTO_CHESS_DB")
	databases, _, err := coredata.NewDatabaseMap(coredata.DatabaseMapConfig{Declared: coredata.DeclaredDatabases(), Configured: map[string]string{db.DatabaseMain: database}})
	if err != nil {
		t.Fatal(err)
	}
	if database != os.Getenv("ROOST_AUTO_CHESS_DB") || !strings.HasPrefix(database, "roost_auto_chess_it_") {
		t.Fatal("refusing a database outside this consumer")
	}
	playerentity.RegisterEntity()
	runentity.RegisterEntity()
	RegisterAutoChessNestHandlers()
	RegisterTickChessMatchTimerGenNestHandlers()
	h := &chessRuntime{database: database}
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
	wal, err := nestwal.Open(nestwal.DefaultOptions(filepath.Join(os.Getenv("ROOST_AUTO_CHESS_WAL"), "wal")))
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
	outbox, err := engine.NewOutboxWorker(outboxStore, chessNoEffects{}, engine.OutboxWorkerOptions{Owner: "auto-chess-practice"})
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
	h.runID, err = entity.BuildEntityID(82001, runentity.EntityKindChessMatch)
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
func (h *chessRuntime) close(t *testing.T) {
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

// chessTimerCounter 读 ChessMatch 驱动的一个 entitytimer 计数（进程内缺省注册表）；用前后差值判断循环投递了几次。
func chessTimerCounter(name string) int64 {
	var total int64
	for _, m := range metrics.Snapshot() {
		if m.Name == name && m.Labels["driver"] == runentity.TimerDriver.Name() {
			total += m.Value
		}
	}
	return total
}

type chessBarrier struct {
	State       string
	Timer       int64
	Gold        [chess.Seats]int64
	Unprojected uint64
}

func chessRuntimeState(t *testing.T, h *chessRuntime) (*runentity.ChessMatch, chess.State) {
	t.Helper()
	v, e := h.access.Get(context.Background(), h.runID, entity.EntityCategoryWorld)
	if e != nil {
		t.Fatal(e)
	}
	m := v.(*runentity.ChessMatch)
	s, e := chess.Decode(m.Dao().GetState())
	if e != nil {
		t.Fatal(e)
	}
	return m, s
}
func chessFacts(t *testing.T, h *chessRuntime) chessBarrier {
	t.Helper()
	m, s := chessRuntimeState(t, h)
	b := chessBarrier{State: s.Encode(), Timer: m.Dao().GetWakeTimer()}
	for i, id := range h.ids {
		v, e := h.access.Get(context.Background(), id, entity.EntityCategoryOther)
		if e != nil {
			t.Fatal(e)
		}
		b.Gold[i] = v.(*playerentity.Player).Dao().GetGold()
	}
	return b
}
func writeChessBarrier(t *testing.T, root, name string, b chessBarrier) {
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
func TestAutoChessRealCrashRecovery(t *testing.T) {
	role := os.Getenv("ROOST_AUTO_CHESS_CHILD")
	root := os.Getenv("ROOST_AUTO_CHESS_WAL")
	ctx := context.Background()
	if role == "write" {
		h := openChessRuntime(t, true, chessNow)
		s := sender.NewAutoChessSender(h.scheduler)
		for _, id := range append(h.ids[:], h.runID) {
			kind := playerentity.EntityKindPlayer
			if id == h.runID {
				kind = runentity.EntityKindChessMatch
			}
			if _, e := h.access.Create(&entity.EntityCreateParam{IsCreate: true, Id: id, Kind: kind}); e != nil {
				t.Fatal(e)
			}
		}
		for _, id := range h.ids {
			if e := s.Sync_ChessPlayerInit(ctx, id); e != nil {
				t.Fatal(e)
			}
		}
		dir := filepath.Join(root, "catalog")
		if e := os.MkdirAll(dir, 0700); e != nil {
			t.Fatal(e)
		}
		raw, _ := json.Marshal(chess.DemoRules())
		if e := os.WriteFile(filepath.Join(dir, "chess.json"), raw, 0600); e != nil {
			t.Fatal(e)
		}
		store, e := chess.OpenCatalog(dir)
		if e != nil {
			t.Fatal(e)
		}
		rules, e := chess.CaptureRules(store.Current())
		if e != nil {
			t.Fatal(e)
		}
		if _, e = s.Sync_ChessInit(ctx, h.runID, h.ids, rules, fmt.Sprintf("%064x", 17), chessNow); e != nil {
			t.Fatal(e)
		}
		for i, id := range h.ids {
			for j, kind := range []string{"buy", "move", "ready"} {
				a, b := 0, 0
				if kind == "move" {
					a = 3
				}
				if _, e = s.Sync_ChessCommand(ctx, h.runID, id, chess.Command{Seq: uint64(j + 1), Epoch: 1, Round: 1, Kind: kind, A: a, B: b}, chessNow); e != nil {
					t.Fatal(i, kind, e)
				}
			}
		}
		m, state := chessRuntimeState(t, h)
		stats := h.projector.Stats()
		if stats.Projected != 0 || stats.WALUnacked != 33 || state.Phase != "battle" || len(state.Fights) != 4 || m.Dao().GetWakeTimer() == 0 {
			t.Fatal("not at frozen unprojected battle boundary")
		}
		b := chessBarrier{State: state.Encode(), Timer: m.Dao().GetWakeTimer(), Unprojected: stats.WALUnacked}
		writeChessBarrier(t, root, "ready.json", b)
		select {}
	}
	if role == "verify" {
		h := openChessRuntime(t, false, chessNow+7200000)
		raw, e := os.ReadFile(filepath.Join(root, "claimed.json"))
		if e != nil {
			t.Fatal(e)
		}
		var want chessBarrier
		if e = json.Unmarshal(raw, &want); e != nil {
			t.Fatal(e)
		}
		if got := chessFacts(t, h); got != want {
			t.Fatal("cold facts differ before retries")
		}
		s := sender.NewAutoChessSender(h.scheduler)
		_, state := chessRuntimeState(t, h)
		for i, id := range h.ids {
			for _, r := range state.Seats[i].Receipts {
				got, e := s.Sync_ChessCommand(ctx, h.runID, id, r.Command, h.clock.Load())
				if e != nil || got != r {
					t.Fatal("command replay changed", e)
				}
			}
			if _, e = s.MultiSync_ChessClaim(ctx, id, h.runID); e != nil {
				t.Fatal(e)
			}
		}
		if got := chessFacts(t, h); got != want {
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
	child := exec.Command(os.Args[0], "-test.run=^TestAutoChessRealCrashRecovery$", "-test.v", "-test.timeout=90s")
	child.Env = append(os.Environ(), "ROOST_AUTO_CHESS_CHILD=write")
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
	var barrier chessBarrier
	if e = json.Unmarshal(raw, &barrier); e != nil {
		t.Fatal(e)
	}
	if e = child.Process.Kill(); e != nil {
		t.Fatal(e)
	}
	if e = <-done; e == nil {
		t.Fatal("child did not crash")
	}
	h := openChessRuntime(t, false, chessNow+3600000)
	m, state := chessRuntimeState(t, h)
	if state.Encode() != barrier.State || m.Dao().GetWakeTimer() != barrier.Timer {
		t.Fatal("lost pool, frozen battle, receipts or timer")
	}
	s := sender.NewAutoChessSender(h.scheduler)
	for i, id := range h.ids {
		for _, r := range state.Seats[i].Receipts {
			got, e := s.Sync_ChessCommand(ctx, h.runID, id, r.Command, h.clock.Load())
			if e != nil || got != r {
				t.Fatal("retry changed purchase or phase", e)
			}
		}
	}
	// 冷加载时 Host 把存量期限报告给驱动索引；到这里投递循环还没启动，直接读 DAO 不会与 Tick 并发。
	if due, ok := runentity.TimerDriver.DueOf(m); !ok || due.UnixMilli() != state.Deadline {
		t.Fatal("cold load did not report deadline")
	}
	// 启动驱动自己的投递循环（与进程装配同一入口），用例不调用 DeliverDue、不手发 Tick。
	// 计数来源：本进程里 ChessMatch 驱动只投递 TimerPhase 一种节点，Tick 回调每次恰好推进一个阶段
	// （准备→战斗、战斗→下一回合准备或终局），所以投递计数与阶段推进一一对应。下面重试的玩家命令都命中
	// 原回执、不重排定时器；即使业务命令经 saveChess 重排，也只是改索引里的期限，不算投递。
	deliveredBefore, redeliveredBefore := chessTimerCounter(entitytimer.DeliveredMetric), chessTimerCounter(entitytimer.RedeliveredMetric)
	if e = runentity.TimerDriver.Start(); e != nil {
		t.Fatal(e)
	}
	view := func() chess.View {
		v, e := s.Sync_ChessView(ctx, h.runID, h.ids[0])
		if e != nil {
			t.Fatal(e)
		}
		return v
	}
	// awaitDelivered 等新期限重新进入索引（终局时 deadline=0，要求索引里不再有期限）且投递计数恰为 want：
	// 新期限在 Tick 提交后报告、计数在投递准入后才加，都可能略晚于读到的提交结果，所以有界轮询；
	// 超过 want 立即失败（多投或重投）。
	awaitDelivered := func(want int64, deadline int64) {
		t.Helper()
		until := time.Now().Add(8 * time.Second)
		for {
			due, ok := runentity.TimerDriver.DueOf(m)
			got := chessTimerCounter(entitytimer.DeliveredMetric) - deliveredBefore
			if got == want && (deadline == 0 && !ok || ok && due.UnixMilli() == deadline) {
				return
			}
			if got > want || time.Now().After(until) {
				t.Fatalf("deliveries=%d want %d; due=%v ok=%v want %d", got, want, due, ok, deadline)
			}
			time.Sleep(time.Millisecond)
		}
	}
	// 加载后补触发：离线一小时的战斗阶段只结算这一次（框架不追帧），进入第 2 回合准备、期限从当前业务时间起算。
	waitChess(t, func() bool { return view().Phase == "prep" })
	v := view()
	if v.Round != 2 || v.Deadline != h.clock.Load()+state.Rules.PrepMillis {
		t.Fatal("offline loop skipped stages")
	}
	awaitDelivered(1, v.Deadline)
	// 之后每个期限由用例把业务时钟推进到期限本身，循环（至多 MaxWait 后重读时钟）按时投递一次、恰好推进一个阶段；
	// 两次推进之间业务时钟冻结，条目不会提前到期，计数是确定的。
	delivered := int64(1)
	for n := 0; n < 32 && v.Phase != "done"; n++ {
		before := v
		h.clock.Store(v.Deadline)
		waitChess(t, func() bool { v = view(); return v.Round != before.Round || v.Phase != before.Phase })
		delivered++
		awaitDelivered(delivered, v.Deadline)
	}
	if v.Phase != "done" {
		t.Fatal("match did not terminate")
	}
	// 总数：补触发 1 次（第 1 回合战斗→第 2 回合准备），第 2～R 回合每回合准备→战斗、战斗→准备 / 终局各 1 次，
	// 共 1 + 2×(R-1)；第 1 回合的准备→战斗由写进程里第八人准备触发，不经定时器，也不在本进程计数里。
	if want := int64(2*v.Round - 1); delivered != want {
		t.Fatalf("deliveries=%d over %d rounds, want %d (one catch-up plus one per later stage)", delivered, v.Round, want)
	}
	if got := chessTimerCounter(entitytimer.RedeliveredMetric) - redeliveredBefore; got != 0 {
		t.Fatalf("driver redelivered %d ticks: a Tick commit did not report its next deadline", got)
	}
	if e = runentity.TimerDriver.Stop(ctx); e != nil {
		t.Fatal(e)
	}
	for _, id := range h.ids {
		for range 3 {
			if _, e = s.MultiSync_ChessClaim(ctx, id, h.runID); e != nil {
				t.Fatal(e)
			}
		}
	}
	final := chessFacts(t, h)
	_, state = chessRuntimeState(t, h)
	for i, gold := range final.Gold {
		if gold != int64((9-state.Seats[i].Rank)*10) {
			t.Fatal("wrong rank reward")
		}
	}
	writeChessBarrier(t, root, "claimed.json", final)
	h.close(t)
	verify := exec.Command(os.Args[0], "-test.run=^TestAutoChessRealCrashRecovery$", "-test.v", "-test.timeout=60s")
	verify.Env = append(os.Environ(), "ROOST_AUTO_CHESS_CHILD=verify")
	if output, e := verify.CombinedOutput(); e != nil {
		t.Fatalf("third process: %v\n%s", e, output)
	}
	if e = h.mongo.Database(h.database).Drop(ctx); e != nil {
		t.Fatal(e)
	}
	t.Logf("SIGKILL: %d unprojected strict records; eight-seat frozen battle restored, driver loop delivered one catch-up stage after cold load and %d later stages at their deadlines (redelivered 0), third process retried all commands and eight rewards", barrier.Unprojected, delivered-1)
}
