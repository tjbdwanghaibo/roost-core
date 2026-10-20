package alliance

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	db "example.com/slg-alliance-practice/db"
	player "example.com/slg-alliance-practice/game/entities/player"
	handler "example.com/slg-alliance-practice/game/handler"
	coredata "github.com/tjbdwanghaibo/roost-core/framework/dataengine"
	"github.com/tjbdwanghaibo/roost-core/framework/dataengine/engine"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/nest"
	"github.com/tjbdwanghaibo/roost-core/framework/nestwal"
	fmongo "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo"
	mongodriver "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo/driver"
)

type noRewardEffects struct{}

func (noRewardEffects) Publish(context.Context, engine.OutboxItem) error {
	return errors.New("unexpected reward effect")
}

type rewardRuntime struct {
	runtime   *engine.Runtime
	projector *engine.Projector
	nest      *nest.NestMgr
	access    *entity.ManagerAccess
	id        int64
}

func openRewardRuntime(t *testing.T, sid int32, manual bool) *rewardRuntime {
	t.Helper()
	database := os.Getenv("ROOST_SLG_REWARD_DB")
	if db.NewPlayerDao().DbName() != db.DatabaseMain {
		t.Fatal("expected logical main")
	}
	databases, _, e := coredata.NewDatabaseMap(coredata.DatabaseMapConfig{Declared: coredata.DeclaredDatabases(), Configured: map[string]string{db.DatabaseMain: database}})
	check(t, e)
	if database != os.Getenv("ROOST_SLG_REWARD_DB") || !strings.HasPrefix(database, "roost_slg_reward_it_") {
		t.Fatal("isolated generated database required")
	}
	root := os.Getenv("ROOST_SLG_REWARD_WAL")
	if root == "" {
		t.Fatal("isolated WAL required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	mongo, e := mongodriver.NewClient(fmongo.DefaultConfig(os.Getenv("ROOST_DATAENGINE_IT_MONGO_URI")), mongodriver.IndexMigrationPolicy{})
	check(t, e)
	t.Cleanup(func() { check(t, mongo.Close(context.Background())) })
	check(t, mongo.Ping(ctx))
	player.RegisterEntity()
	handler.RegisterAllianceRewardsNestHandlers()
	store, e := engine.NewMongoStore(mongo, engine.MongoStoreConfig{DefaultDatabase: database, Databases: databases})
	check(t, e)
	check(t, store.EnsureInfrastructure(ctx))
	wal, e := nestwal.Open(nestwal.DefaultOptions(filepath.Join(root, "wal")))
	check(t, e)
	projector, e := engine.NewProjector(wal, store, engine.ProjectorOptions{ManualReplay: manual})
	check(t, e)
	outstore, e := engine.NewMongoOutboxStore(store)
	check(t, e)
	outbox, e := engine.NewOutboxWorker(outstore, noRewardEffects{}, engine.OutboxWorkerOptions{Owner: "slg-home-reward"})
	check(t, e)
	access := entity.NewManagerAccess(entity.NewEntityManager())
	rt, e := engine.NewRuntime(store, wal, projector, outbox, access, nil, nil, engine.PipelinedRuntimeConfig{})
	check(t, e)
	check(t, rt.Start(ctx))
	n := nest.NewEngine(append(rt.NestOptions(), nest.NestOptionWithGetter(access))...)
	check(t, n.Start())
	id, e := entity.BuildEntityID(int64(sid)*100, player.EntityKindPlayer)
	check(t, e)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		check(t, n.Shutdown(ctx))
		check(t, rt.Shutdown(ctx))
	})
	return &rewardRuntime{rt, projector, n, access, id}
}
