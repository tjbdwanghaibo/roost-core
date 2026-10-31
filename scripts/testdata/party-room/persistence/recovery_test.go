package handler

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	db "example.com/party-room-practice/db"
	roomentity "example.com/party-room-practice/game/entities/party_room"
	playerentity "example.com/party-room-practice/game/entities/player"
	sender "example.com/party-room-practice/game/handler/syncsender"
	gameheartbeat "example.com/party-room-practice/game/heartbeat"
	party "example.com/party-room-practice/game/partyroom"
	"example.com/party-room-practice/protocol/msgid"
	"github.com/spf13/viper"
	coredata "github.com/tjbdwanghaibo/roost-core/framework/dataengine"
	"github.com/tjbdwanghaibo/roost-core/framework/dataengine/engine"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/nest"
	"github.com/tjbdwanghaibo/roost-core/framework/nestwal"
	"github.com/tjbdwanghaibo/roost-core/infra/base/clock"
	fmongo "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo"
	mongodriver "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo/driver"
	wiringnest "github.com/tjbdwanghaibo/roost-core/wiring/nest"
	"github.com/tjbdwanghaibo/roost-core/wiring/selfsync"
)

// 真实依赖准入（ROOST_PARTY_ROOM_REAL=1，scripts/test-party-room-generated.sh）：真实 nestwal + Mongo 投影 + DataEngine
// Runtime（冷加载、删档准入），子进程 SIGKILL。缺省模式只编译（go vet），不运行。

type partyNoEffects struct{}

func (partyNoEffects) Publish(context.Context, engine.OutboxItem) error {
	return errors.New("unexpected effect in local party room workflow")
}

type partyRuntime struct {
	runtime   *engine.Runtime
	projector *engine.Projector
	scheduler *nest.NestMgr
	syncMod   *wiringnest.Mod // 只用它建 self_sync 的 Manager 并装上 SyncGroup（不 Provide / Start）
	access    *entity.ManagerAccess
	mongo     *mongodriver.Client
	database  string
	room      int64
	ids       [3]int64
	now       atomic.Int64
}

func openPartyRuntime(t *testing.T, manual bool, now int64) *partyRuntime {
	t.Helper()
	uri := os.Getenv("ROOST_DATAENGINE_IT_MONGO_URI")
	if uri == "" {
		t.Fatal("isolated Mongo replica set required")
	}
	// DAO 写逻辑库 db=main（骨架默认）；本次运行的唯一物理库经映射表给出，与进程的 dataengine.databases 同一条规则。
	if logical := db.NewPlayerDao().DbName(); logical != db.DatabaseMain {
		t.Fatalf("Player DAO logical database %q, want %q", logical, db.DatabaseMain)
	}
	database := os.Getenv("ROOST_PARTY_ROOM_DB")
	if !strings.HasPrefix(database, "roost_party_room_it_") {
		t.Fatal("refusing a database outside this consumer")
	}
	databases, _, err := coredata.NewDatabaseMap(coredata.DatabaseMapConfig{Declared: coredata.DeclaredDatabases(), Configured: map[string]string{db.DatabaseMain: database}})
	if err != nil {
		t.Fatal(err)
	}
	playerentity.RegisterEntity()
	roomentity.RegisterEntity()
	RegisterPartyRoomNestHandlers()
	RegisterTickPartyRoomTimerGenNestHandlers()
	h := &partyRuntime{database: database}
	h.now.Store(now)
	clock.SetSource(func() time.Time { return time.UnixMilli(h.now.Load()) })
	t.Cleanup(func() { clock.SetSource(nil) })
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
	wal, err := nestwal.Open(nestwal.DefaultOptions(filepath.Join(os.Getenv("ROOST_PARTY_ROOM_WAL"), "wal")))
	if err != nil {
		t.Fatal(err)
	}
	if h.projector, err = engine.NewProjector(wal, store, engine.ProjectorOptions{ManualReplay: manual}); err != nil {
		t.Fatal(err)
	}
	outboxStore, err := engine.NewMongoOutboxStore(store)
	if err != nil {
		t.Fatal(err)
	}
	outbox, err := engine.NewOutboxWorker(outboxStore, partyNoEffects{}, engine.OutboxWorkerOptions{Owner: "party-room-practice"})
	if err != nil {
		t.Fatal(err)
	}
	h.access = entity.NewManagerAccess(entity.NewEntityManager())
	if h.runtime, err = engine.NewRuntime(store, wal, h.projector, outbox, h.access, nil, nil, engine.PipelinedRuntimeConfig{}); err != nil {
		t.Fatal(err)
	}
	if err := h.runtime.Start(ctx); err != nil {
		t.Fatal(err)
	}
	// 连接 handler 在提交后把玩家加入房间同步组（roomentity.SyncGroup）：与生成的 bootstrap 一样把它装到 self_sync 的
	// Manager 上。本用例不推送（没有玩家连接、不 Flush），Manager 只用来承接成员关系与 Nest 的提交观察。
	selfView := selfsync.MustNew(selfsync.Config{Entities: h.access, Kind: playerentity.EntityKindPlayer, MessageID: msgid.MsgSelfSync, Groups: []*selfsync.Groups{roomentity.SyncGroup}})
	h.syncMod = wiringnest.NewModWithEntitySync(h.access, selfView.EntitySyncSetup())
	if err := h.syncMod.Init(viper.New()); err != nil {
		t.Fatal(err)
	}
	h.scheduler = nest.NewEngine(append(h.runtime.NestOptions(), nest.NestOptionWithGetter(h.access), nest.NestOptionWithEntitySync(h.syncMod.EntitySync()))...)
	if err := h.scheduler.Start(); err != nil {
		t.Fatal(err)
	}
	lookup := func(id int64) entity.IThreadSafeEntity { return h.access.Manager().Get(id) }
	if err := roomentity.TimerDriver.Bind(h.scheduler, lookup); err != nil {
		t.Fatal(err)
	}
	if err := gameheartbeat.Driver.Bind(h.scheduler, lookup); err != nil {
		t.Fatal(err)
	}
	if h.room, err = PartyRoomIDForMatch("party-real-match"); err != nil {
		t.Fatal(err)
	}
	for i := range h.ids {
		if h.ids[i], err = entity.BuildEntityID(int64(91001+i), playerentity.EntityKindPlayer); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { h.close(t) })
	return h
}

func (h *partyRuntime) close(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := gameheartbeat.Driver.Stop(ctx); err != nil {
		t.Error(err)
	}
	if err := roomentity.TimerDriver.Stop(ctx); err != nil {
		t.Error(err)
	}
	if err := h.scheduler.Shutdown(ctx); err != nil {
		t.Error(err)
	}
	if err := h.runtime.Shutdown(ctx); err != nil {
		t.Error(err)
	}
	if err := h.syncMod.EntitySync().Close(ctx); err != nil {
		t.Error(err)
	}
}

type partyBarrier struct {
	State       string
	Wake        int64
	Receipt     party.Receipt
	Coins       [3]int64
	Unprojected uint64
}

func (h *partyRuntime) coins(t *testing.T) [3]int64 {
	t.Helper()
	var out [3]int64
	for i, id := range h.ids {
		v, err := h.access.Get(context.Background(), id, entity.EntityCategoryOther)
		if err != nil {
			t.Fatal(err)
		}
		out[i] = v.(*playerentity.Player).Dao().GetCoins()
	}
	return out
}

func writePartyBarrier(t *testing.T, root, name string, b partyBarrier) {
	t.Helper()
	raw, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, name), raw, 0600); err != nil {
		t.Fatal(err)
	}
}

func readPartyBarrier(t *testing.T, root, name string) partyBarrier {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root, name))
	if err != nil {
		t.Fatal(err)
	}
	var b partyBarrier
	if err := json.Unmarshal(raw, &b); err != nil {
		t.Fatal(err)
	}
	return b
}

// SIGKILL 只针对本用例创建的子进程；strict WAL 已确认不等于 Mongo 已投影，也不等于磁盘断电验收。
// 心跳不持久：冷加载后由 onHeartbeatInit 按持久阶段重新启动，离线期间错过的回合在第一拍里按时间顺序一次结算。
func TestPartyGameRealCrashRecovery(t *testing.T) {
	role := os.Getenv("ROOST_PARTY_ROOM_CHILD")
	root := os.Getenv("ROOST_PARTY_ROOM_WAL")
	ctx := context.Background()
	if role == "write" {
		h := openPartyRuntime(t, true, partyNow)
		s := sender.NewPartyRoomSender(h.scheduler)
		for _, id := range h.ids {
			if _, err := h.access.Create(&entity.EntityCreateParam{IsCreate: true, Id: id, Kind: playerentity.EntityKindPlayer}); err != nil {
				t.Fatal(err)
			}
			if err := s.Sync_PartyPlayerInit(ctx, id); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := h.access.Create(&entity.EntityCreateParam{IsCreate: true, Id: h.room, Kind: roomentity.EntityKindPartyRoom}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Sync_PartyOpenMatch(ctx, h.room, "party-real-match", h.ids[:], party.DemoRules()); err != nil {
			t.Fatal(err)
		}
		for _, id := range h.ids {
			if _, err := s.Sync_PartyConnect(ctx, h.room, id, 1, true); err != nil {
				t.Fatal(err)
			}
		}
		state := partySnapshot(t, h.scheduler, h.room)
		h.now.Store(state.State.StarAt)
		receipt, err := s.Sync_PartyInput(ctx, h.room, h.ids[0], party.Input{Epoch: 1, Seq: 1, Round: 1, Kind: "grab"})
		if err != nil || receipt.Outcome != party.OutcomeScore {
			t.Fatal("grab", receipt, err)
		}
		after := partySnapshot(t, h.scheduler, h.room)
		stats := h.projector.Stats()
		if stats.Projected != 0 || stats.WALUnacked == 0 || after.State.Phase != party.PhasePlaying || after.Wake == 0 || !after.Beat {
			t.Fatalf("not at an unprojected playing room: %+v %+v", stats, after)
		}
		writePartyBarrier(t, root, "ready.json", partyBarrier{State: after.Raw, Wake: after.Wake, Receipt: receipt, Unprojected: stats.WALUnacked})
		select {}
	}
	if role == "verify" {
		h := openPartyRuntime(t, false, partyNow+3600000)
		want := readPartyBarrier(t, root, "claimed.json")
		if got := h.coins(t); got != want.Coins {
			t.Fatalf("cold coins %v, want %v", got, want.Coins)
		}
		if _, err := h.access.Get(ctx, h.room, entity.EntityCategoryWorld); err == nil {
			t.Fatal("deleted room came back on cold load")
		}
		for _, id := range h.ids {
			if _, err := sender.NewPartyRoomSender(h.scheduler).MultiSync_PartyClaim(ctx, id, h.room); err == nil {
				t.Fatal("claim against a deleted room succeeded")
			}
		}
		if got := h.coins(t); got != want.Coins {
			t.Fatal("rewards changed after the room was deleted")
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
	child := exec.Command(os.Args[0], "-test.run=^TestPartyGameRealCrashRecovery$", "-test.v", "-test.timeout=90s")
	child.Env = append(os.Environ(), "ROOST_PARTY_ROOM_CHILD=write")
	child.Stdout, child.Stderr = out, out
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
	deadline := time.Now().Add(30 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(root, "ready.json")); err == nil {
			break
		}
		select {
		case err := <-done:
			log, _ := os.ReadFile(logPath)
			t.Fatalf("child failed: %v\n%s", err, log)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("child deadline: " + logPath)
		}
		time.Sleep(25 * time.Millisecond)
	}
	barrier := readPartyBarrier(t, root, "ready.json")
	if err := child.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err == nil {
		t.Fatal("child did not crash")
	}
	var crashed party.State
	if err := json.Unmarshal([]byte(barrier.State), &crashed); err != nil {
		t.Fatal(err)
	}
	// 新进程：冷加载房间，持久 State、定时器登记与回执原样恢复；心跳按持久阶段（playing）重新启动。
	h := openPartyRuntime(t, false, crashed.StarAt+1)
	s := sender.NewPartyRoomSender(h.scheduler)
	if _, err := h.access.Get(ctx, h.room, entity.EntityCategoryWorld); err != nil {
		t.Fatal(err)
	}
	loaded := partySnapshot(t, h.scheduler, h.room)
	if loaded.Raw != barrier.State || loaded.Wake != barrier.Wake || !loaded.Beat {
		t.Fatalf("cold load lost the room: beat=%v wake=%d/%d", loaded.Beat, loaded.Wake, barrier.Wake)
	}
	if gameheartbeat.Driver.Registered() != 1 {
		t.Fatal("heartbeat not re-armed from the persisted phase")
	}
	if got, err := s.Sync_PartyInput(ctx, h.room, h.ids[0], barrier.Receipt.Input); err != nil || got != barrier.Receipt {
		t.Fatal("retried input after crash changed the receipt", got, err)
	}
	// 业务时钟越过三个回合：重新启动的心跳一拍内按时间顺序结束回合，seat 0 唯一领先获胜。
	if err := gameheartbeat.Driver.Start(); err != nil {
		t.Fatal(err)
	}
	h.now.Store(crashed.RoundEnd + 2*party.DemoRules().RoundMillis + 500)
	waitParty(t, "re-armed heartbeat settles the match", func() bool {
		return partySnapshot(t, h.scheduler, h.room).State.Phase == party.PhaseSettled
	})
	settled := partySnapshot(t, h.scheduler, h.room)
	if settled.State.Winner != h.ids[0] || settled.Beat || settled.Wake != 0 {
		t.Fatalf("settlement after recovery %+v", settled.State)
	}
	for _, id := range h.ids {
		for range 2 {
			if _, err := s.MultiSync_PartyClaim(ctx, id, h.room); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := s.Sync_PartyRelease(ctx, h.room, id); err != nil {
			t.Fatal(err)
		}
	}
	if closed := partySnapshot(t, h.scheduler, h.room); closed.State.Phase != party.PhaseClosed {
		t.Fatalf("room not closed after every seat was released: %s", closed.State.Phase)
	}
	coins := h.coins(t)
	if coins != [3]int64{party.DemoRules().WinReward, party.DemoRules().PlayReward, party.DemoRules().PlayReward} {
		t.Fatalf("rewards %v", coins)
	}
	// 删档：DataEngine 删除准入（本地实体在隔离事务里写删除 mutation）后移出内存，心跳登记随之离开驱动。
	live := h.access.Manager().Get(h.room)
	if err := h.access.Destroy(ctx, live, entity.DestroyReasonCommon, true); err != nil {
		t.Fatal(err)
	}
	if gameheartbeat.Driver.Registered() != 0 {
		t.Fatal("deleted room left a heartbeat registration")
	}
	writePartyBarrier(t, root, "claimed.json", partyBarrier{Coins: coins})
	h.close(t)
	verify := exec.Command(os.Args[0], "-test.run=^TestPartyGameRealCrashRecovery$", "-test.v", "-test.timeout=60s")
	verify.Env = append(os.Environ(), "ROOST_PARTY_ROOM_CHILD=verify")
	if output, err := verify.CombinedOutput(); err != nil {
		t.Fatalf("third process: %v\n%s", err, output)
	}
	if err := h.mongo.Database(h.database).Drop(ctx); err != nil {
		t.Fatal(err)
	}
	t.Logf("SIGKILL: %d unprojected records; cold load restored the playing room, re-armed its heartbeat, replayed the input receipt, settled, rewarded once, deleted the room; third process saw the same coins and no room", barrier.Unprojected)
}
