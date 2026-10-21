package alliance

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	authority "example.com/slg-practice/game/entities/alliance_authority"
	handler "example.com/slg-practice/game/handler"
	"github.com/tjbdwanghaibo/roost-core/framework/dataengine/engine"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/nest"
	"github.com/tjbdwanghaibo/roost-core/framework/nestwal"
	"github.com/tjbdwanghaibo/roost-core/framework/remoteentity"
	syncdriver "github.com/tjbdwanghaibo/roost-core/framework/sync/syncbus/driver"
	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
	natsdriver "github.com/tjbdwanghaibo/roost-core/infra/network/nats/driver"
	fmongo "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo"
	mongodriver "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo/driver"
)

type allianceOwner struct {
	nest    *nest.NestMgr
	manager *remoteentity.Manager
	id      int64
	close   func()
}

func openAllianceOwner(t *testing.T, prefix string, sid int32, create bool) *allianceOwner {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	// Remote 控制库（回执 / 事务）与 DAO 文档都写本次唯一物理库；DAO 的逻辑库经同一张映射表换算。
	database, databases := isolatedDatabases(t)
	authority.RegisterEntity()
	handler.RegisterAllianceAuthorityNestHandlers()
	mongo, e := mongodriver.NewClient(fmongo.DefaultConfig(os.Getenv("ROOST_DATAENGINE_IT_MONGO_URI")), mongodriver.IndexMigrationPolicy{})
	check(t, e)
	t.Cleanup(func() { _ = mongo.Close(context.Background()) })
	check(t, mongo.Ping(ctx))
	access := entity.NewManagerAccess(entity.NewEntityManager())
	backend, e := remoteentity.NewBackend(access, remoteentity.NewMongoCommitter(mongo, database, sid, 0))
	check(t, e)
	// Remote 在 DataEngine 接上投影之前启动（Start 可能恢复上一轮未完成的提交、写 DAO 文档），所以先显式绑上
	// DataEngine 的同一张表；之后 SetRemoteProjection 再绑同一张是无操作。没绑定时拒绝写（dataengine.ErrDatabaseMapNotBound）。
	check(t, backend.BindDatabaseMap(databases))
	cfg := remoteentity.DefaultConfig()
	cfg.LockKey = prefix + ":lock"
	cfg.SnapshotL2KeyPrefix = prefix + ":snapshot"
	cfg.LockTTL = 3 * time.Second
	assembly, e := remoteentity.Assemble(remoteentity.AssemblyDeps{Redis: redisClient(t), Backend: backend}, cfg, sid, remoteentity.MongoBackendConfig{})
	check(t, e)
	nc, e := natsdriver.NewClient(fnats.DefaultConfig(os.Getenv("ROOST_DATAENGINE_IT_NATS_URL")), natsdriver.ClientOptions{})
	check(t, e)
	t.Cleanup(nc.Close)
	check(t, assembly.Start(ctx, syncdriver.NewNatsSyncBus(nc, sid, prefix+".remote")))
	store, e := engine.NewMongoStore(mongo, engine.MongoStoreConfig{DefaultDatabase: database, Databases: databases})
	check(t, e)
	check(t, store.EnsureInfrastructure(ctx))
	check(t, store.SetRemoteProjection(backend, assembly.Manager))
	wal, e := nestwal.Open(nestwal.DefaultOptions(filepath.Join(os.Getenv("ROOST_SLG_WAL"), fmt.Sprintf("alliance-%d", sid))))
	check(t, e)
	projector, e := engine.NewProjector(wal, store, engine.ProjectorOptions{})
	check(t, e)
	outstore, e := engine.NewMongoOutboxStore(store)
	check(t, e)
	outbox, e := engine.NewOutboxWorker(outstore, noRewardEffects{}, engine.OutboxWorkerOptions{Owner: fmt.Sprintf("alliance-%d", sid)})
	check(t, e)
	rt, e := engine.NewRuntime(store, wal, projector, outbox, access, assembly.Manager, nil, engine.PipelinedRuntimeConfig{})
	check(t, e)
	check(t, rt.Start(ctx))
	n := nest.NewEngine(append(rt.NestOptions(), nest.NestOptionWithGetter(access), nest.NestOptionWithRemoteEntityManager(assembly.Manager))...)
	check(t, n.Start())
	var once sync.Once
	close := func() {
		once.Do(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			check(t, n.Shutdown(ctx))
			check(t, rt.Shutdown(ctx))
			check(t, assembly.Stop(ctx))
		})
	}
	t.Cleanup(close)
	id, e := entity.BuildEntityID(99123, authority.EntityKindAllianceAuthority)
	check(t, e)
	if create {
		_, e = access.Create(&entity.EntityCreateParam{IsCreate: true, Id: id, Kind: authority.EntityKindAllianceAuthority})
		check(t, e)
	}
	lease, e := assembly.Manager.ClaimRemoteOwnership(ctx, id)
	check(t, e)
	if !lease.Shared {
		_, e = assembly.Manager.EnterRemoteSharedMode(ctx, id)
		check(t, e)
	}
	return &allianceOwner{n, assembly.Manager, id, close}
}
