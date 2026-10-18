package handler

import (
	"context"
	db "example.com/moba-practice/db"
	directoryentity "example.com/moba-practice/game/entities/battle_directory"
	roundentity "example.com/moba-practice/game/entities/battle_round"
	player "example.com/moba-practice/game/entities/player"
	sender "example.com/moba-practice/game/handler/syncsender"
	"example.com/moba-practice/game/moba"
	"github.com/tjbdwanghaibo/roost-core/framework/dataengine/engine"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/nest"
	"github.com/tjbdwanghaibo/roost-core/framework/nestwal"
	fmongo "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo"
	mongodriver "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo/driver"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type activeRuntime struct {
	runtime   *engine.Runtime
	projector *engine.Projector
	scheduler *nest.NestMgr
	access    *entity.ManagerAccess
	mongo     *mongodriver.Client
	database  string
	roundID   int64
}

func openActiveRuntime(t *testing.T, scope string, manual bool) *activeRuntime {
	t.Helper()
	player.RegisterEntity()
	roundentity.RegisterEntity()
	directoryentity.RegisterEntity()
	RegisterMobaRoundNestHandlers()
	uri := os.Getenv("ROOST_DATAENGINE_IT_MONGO_URI")
	if uri == "" {
		t.Fatal("isolated Mongo replica set required")
	}
	database := db.NewPlayerDao().DbName()
	if database != os.Getenv("ROOST_MOBA_DB") || !strings.HasPrefix(database, "roost_moba_it_") {
		t.Fatal("refusing a database outside this consumer")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client, err := mongodriver.NewClient(fmongo.DefaultConfig(uri), mongodriver.IndexMigrationPolicy{})
	if err != nil {
		t.Fatal(err)
	}
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
	wal, err := nestwal.Open(nestwal.DefaultOptions(filepath.Join(os.Getenv("ROOST_MOBA_WAL"), scope)))
	if err != nil {
		t.Fatal(err)
	}
	projector, err := engine.NewProjector(wal, store, engine.ProjectorOptions{ManualReplay: manual})
	if err != nil {
		t.Fatal(err)
	}
	outboxStore, err := engine.NewMongoOutboxStore(store)
	if err != nil {
		t.Fatal(err)
	}
	outbox, err := engine.NewOutboxWorker(outboxStore, mobaNoEffects{}, engine.OutboxWorkerOptions{Owner: "moba-" + scope})
	if err != nil {
		t.Fatal(err)
	}
	access := entity.NewManagerAccess(entity.NewEntityManager())
	runtime, err := engine.NewRuntime(store, wal, projector, outbox, access, nil, nil, engine.PipelinedRuntimeConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.Start(ctx); err != nil {
		t.Fatal(err)
	}
	scheduler := nest.NewEngine(append(runtime.NestOptions(), nest.NestOptionWithGetter(access))...)
	if err := scheduler.Start(); err != nil {
		t.Fatal(err)
	}
	id, err := entity.BuildEntityID(30000, roundentity.EntityKindBattleRound)
	if err != nil {
		t.Fatal(err)
	}
	h := &activeRuntime{runtime: runtime, projector: projector, scheduler: scheduler, access: access, mongo: client, database: database, roundID: id}
	player.RegisterEntity()
	roundentity.RegisterEntity()
	directoryentity.RegisterEntity()
	RegisterMobaRoundNestHandlers()
	t.Cleanup(func() { h.close(t) })
	return h
}
func (h *activeRuntime) close(t *testing.T) {
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
func (h *activeRuntime) apply(ctx context.Context, c moba.Change) (moba.Round, error) {
	return sender.NewMobaRoundSender(h.scheduler).Sync_MOBARound(ctx, h.roundID, c)
}
func (h *activeRuntime) mustApply(t *testing.T, c moba.Change) moba.Round {
	t.Helper()
	r, err := h.apply(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func (h *activeRuntime) createRound(t *testing.T) moba.Round {
	t.Helper()
	if _, err := h.access.Create(&entity.EntityCreateParam{IsCreate: true, Id: h.roundID, Kind: roundentity.EntityKindBattleRound}); err != nil {
		t.Fatal(err)
	}
	members := [moba.ArenaSeats]moba.Member{}
	for i := range members {
		id, err := entity.BuildEntityID(int64(40000+i), player.EntityKindPlayer)
		if err != nil {
			t.Fatal(err)
		}
		members[i] = moba.Member{PlayerID: id, SID: int32(1000 + i%2)}
	}
	initial := moba.Change{Op: "init", HostSID: 2000, MatchID: "durable-three-lanes", Members: members, Seed: 42, Deadline: 100, Now: 1}
	directoryID, err := entity.BuildEntityID(31000, directoryentity.EntityKindBattleDirectory)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.access.Create(&entity.EntityCreateParam{IsCreate: true, Id: directoryID, Kind: directoryentity.EntityKindBattleDirectory}); err != nil {
		t.Fatal(err)
	}
	reserved, err := sender.NewMobaRoundSender(h.scheduler).Sync_MOBAReserveRound(context.Background(), directoryID, initial.MatchID, h.roundID, moba.RoundBinding(initial))
	if err != nil || reserved != h.roundID {
		t.Fatal("directory reserve", err)
	}
	r := h.mustApply(t, initial)
	for i := range members {
		r = h.mustApply(t, moba.Change{Op: "pick", HostSID: 2000, Epoch: 1, Seat: i, Hero: i%5 + 1, Now: 2})
		r = h.mustApply(t, moba.Change{Op: "ready", HostSID: 2000, Epoch: 1, Seat: i, Ready: true, Now: 2})
	}
	return h.mustApply(t, moba.Change{Op: "start", HostSID: r.HostSID, Epoch: r.Epoch, Now: 3})
}
func (h *activeRuntime) createPlayers(t *testing.T, r moba.Round, sid int32) {
	t.Helper()
	s := sender.NewMobaRoundSender(h.scheduler)
	for _, m := range r.Members {
		if m.SID != sid {
			continue
		}
		if _, err := h.access.Create(&entity.EntityCreateParam{IsCreate: true, Id: m.PlayerID, Kind: player.EntityKindPlayer}); err != nil {
			t.Fatal(err)
		}
		if err := s.Sync_MOBAPreparePlayer(context.Background(), m.PlayerID); err != nil {
			t.Fatal(err)
		}
	}
}
