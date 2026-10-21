//go:build integration

package movement

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/remoteentity"
	syncdriver "github.com/tjbdwanghaibo/roost-core/framework/sync/syncbus/driver"
	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
	natsdriver "github.com/tjbdwanghaibo/roost-core/infra/network/nats/driver"
	fmongo "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo"
	mongodriver "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo/driver"
	fredis "github.com/tjbdwanghaibo/roost-core/infra/storage/redis"
	redisdriver "github.com/tjbdwanghaibo/roost-core/infra/storage/redis/driver"
	"github.com/tjbdwanghaibo/roost-core/infra/storage/redis/redistest"
)

// 真实 Redis + Mongo + NATS 下的同一组场景（shared_test.go 的进程内用例用 memRemoteAuthority 替身）：每个进程的 Manager
// 经正式装配 remoteentity.Assemble（Redis 版本锁与快照 L2、Mongo 持久写权威 = 所有权记录、NATS 复制）构造，与生成
// 工程的 Remote 装配相同；两个进程共用同一个 Mongo 库与 Redis 键前缀，相当于同一部署的两个服务。
//
// 运行：整套随 wiring/scripts/integration/dataengine-env.sh test；单跑时 source 隔离环境的 env.sh、持有
// $ROOST_DATAENGINE_IT_ROOT/remote-acceptance.lock（common.sh 的 acquire_acceptance_lock）后
//
//	GOWORK=off go test -race -tags=integration -run '^TestRealRemote' -count=1 ./gameplay/movement
//
// 隔离：Mongo 库名、Redis 键前缀（锁键与快照 L2）、NATS 主题前缀都是本次运行唯一的 roost_hb_remote_<pid>_<纳秒>，
// 不碰共享的 game / saga / remote_entity 库与 ROOST_* 流；用例结束时删库、删键。

// realRemoteManagers 返回用正式装配构造 Manager 的工厂；没有共享隔离环境时跳过。
func realRemoteManagers(t *testing.T) npcManagerFactory {
	t.Helper()
	if os.Getenv("ROOST_DATAENGINE_IT") != "1" {
		t.Skip("set ROOST_DATAENGINE_IT=1 with the isolated environment (wiring/scripts/integration/dataengine-env.sh)")
	}
	uri, addr, natsURL := os.Getenv("ROOST_DATAENGINE_IT_MONGO_URI"), os.Getenv("ROOST_DATAENGINE_IT_REDIS_ADDR"), os.Getenv("ROOST_DATAENGINE_IT_NATS_URL")
	if uri == "" || addr == "" || natsURL == "" {
		t.Fatal("isolated Mongo, Redis and NATS are required")
	}
	unique := fmt.Sprintf("roost_hb_remote_%d_%d", os.Getpid(), time.Now().UnixNano())
	mongo, err := mongodriver.NewClient(fmongo.DefaultConfig(uri), mongodriver.IndexMigrationPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := mongo.Database(unique).Drop(ctx); err != nil {
			t.Errorf("drop %s: %v", unique, err)
		}
		_ = mongo.Close(context.Background())
	})
	redistest.Cleanup(t, addr, unique) // 在建 Redis 客户端之前登记：最后执行，删掉键名含本次前缀的键
	redis, err := redisdriver.NewClient(fredis.DefaultConfig(addr))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = redis.Close() })
	return func(t *testing.T, sid int32, loader entity.IRemoteEntityLoader) *remoteentity.Manager {
		t.Helper()
		cfg := remoteentity.DefaultConfig()
		cfg.LockKey = unique
		cfg.SnapshotL2KeyPrefix = unique
		cfg.LockTTL = 3 * time.Second
		assembly, err := remoteentity.Assemble(remoteentity.AssemblyDeps{Redis: redis, Mongo: mongo, Loader: loader}, cfg, sid,
			remoteentity.MongoBackendConfig{Database: unique})
		if err != nil {
			t.Fatal(err)
		}
		client, err := natsdriver.NewClient(fnats.DefaultConfig(natsURL), natsdriver.ClientOptions{})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(client.Close)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := assembly.Start(ctx, syncdriver.NewNatsSyncBus(client, sid, unique)); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := assembly.Stop(context.Background()); err != nil {
				t.Errorf("Remote stop: %v", err)
			}
		})
		return assembly.Manager
	}
}

func TestRealRemoteSharedModeRoundTripStopsAndResumesEveryHeartbeat(t *testing.T) {
	for _, delivery := range npcDeliveries {
		t.Run(string(delivery), func(t *testing.T) { testSharedModeRoundTrip(t, realRemoteManagers(t), 11, delivery) })
	}
}

func TestRealRemoteOwnershipTransferMovesTheHeartbeatsWithoutDoubleAdvance(t *testing.T) {
	for _, delivery := range npcDeliveries {
		t.Run(string(delivery), func(t *testing.T) { testOwnershipTransfer(t, realRemoteManagers(t), 12, delivery) })
	}
}

func TestRealRemoteUnloadAndReloadLeavesNoStrayBeat(t *testing.T) {
	for _, delivery := range npcDeliveries {
		t.Run(string(delivery), func(t *testing.T) { testUnloadAndReload(t, realRemoteManagers(t), 13, delivery) })
	}
}
