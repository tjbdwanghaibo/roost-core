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
	database := db.NewPlayerDao().DbName()
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
	store, err := engine.NewMongoStore(client, engine.MongoStoreConfig{DefaultDatabase: database})
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
	if due, ok := runentity.TimerDriver.DueOf(m); !ok || due.UnixMilli() != state.Deadline {
		t.Fatal("cold load did not report deadline")
	}
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
	waitChess(t, func() bool { return view().Phase == "prep" })
	v := view()
	if v.Round != 2 || v.Deadline != h.clock.Load()+state.Rules.PrepMillis {
		t.Fatal("offline loop skipped stages")
	}
	// 驱动在冻结业务时钟下每次只推进一个阶段；同一循环继续准时处理下一期限。
	for n := 0; n < 32 && v.Phase != "done"; n++ {
		waitChess(t, func() bool { due, ok := runentity.TimerDriver.DueOf(m); return ok && due.UnixMilli() == v.Deadline })
		before := v
		h.clock.Store(v.Deadline)
		waitChess(t, func() bool { v = view(); return v.Round != before.Round || v.Phase != before.Phase })
	}
	if v.Phase != "done" {
		t.Fatal("match did not terminate")
	}
	waitChess(t, func() bool { _, ok := runentity.TimerDriver.DueOf(m); return !ok })
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
	t.Logf("SIGKILL: %d unprojected strict records; eight-seat frozen battle restored, driver loop settled and advanced deadlines, third process retried all commands and eight rewards", barrier.Unprojected)
}
