package mods

import "github.com/tjbdwanghaibo/roost-core/app"

// ModName constants for reusable service runtime mods.
const (
	ModMongo          app.ModName = "mongo"
	ModHealth         app.ModName = app.ModHealth
	ModMetrics        app.ModName = app.ModMetrics
	ModAdmin          app.ModName = app.ModAdmin
	ModAdminMetadata  app.ModName = app.ModAdminMetadata
	ModLifecycle      app.ModName = app.ModLifecycle
	ModRuntimeFailure app.ModName = app.ModRuntimeFailure
	ModSingleton      app.ModName = app.ModSingleton

	// redis.lock（IDistLockFactory）与 etcd.election（IElectionFactory）不再作为 capability 发布：
	// 进程 / sid 级单例由 App 单实例锁（app.Singleton）提供；键级用途直接用 core 的
	// redis/driver、etcd/driver 装配。
	ModRedis      app.ModName = "redis"
	ModRedisVLock app.ModName = "redis.versioned_lock"

	ModNats          app.ModName = "nats"
	ModNatsRpc       app.ModName = "nats.rpc"
	ModNatsJetStream app.ModName = "nats.jetstream"
	ModBus           app.ModName = "bus"

	ModEtcd       app.ModName = "etcd"
	ModEtcdDiscov app.ModName = "etcd.discovery"

	ModSyncBus                 app.ModName = "syncbus"
	ModDataEngine              app.ModName = "dataengine"
	ModRemoteEntity            app.ModName = "remote_entity"
	ModRemoteEntityAtomicStore app.ModName = "remote_entity.atomic_store"
	ModLock                    app.ModName = "lock"
	ModOps                     app.ModName = "ops"
	ModStatsLog                app.ModName = "stats_log"
	ModConfigData              app.ModName = "config_data"
	ModNest                    app.ModName = "nest"
	ModSaga                    app.ModName = "saga"
	ModEntityRuntime           app.ModName = "entity.runtime"
	ModManager                 app.ModName = "manager"
)
