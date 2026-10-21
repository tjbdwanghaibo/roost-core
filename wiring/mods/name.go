package mods

import "github.com/tjbdwanghaibo/roost-core/framework/app"

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
	// ModSingletonIncarnation 是本进程持有的单实例锁身份（app.SingletonIncarnation）；singleton.enabled=false 时不登记。
	ModSingletonIncarnation app.ModName = app.ModSingletonIncarnation

	// redis.lock（IDistLockFactory）与 etcd.election（IElectionFactory）不再作为 capability 发布：
	// 进程 / sid 级单例由 App 单实例锁（app.Singleton）提供；键级用途直接用 core 的
	// redis/driver、etcd/driver 装配。
	ModRedis      app.ModName = "redis"
	ModRedisVLock app.ModName = "redis.versioned_lock"

	ModNats          app.ModName = "nats"
	ModNatsRaw       app.ModName = "nats.raw"
	ModNatsRpc       app.ModName = "nats.rpc"
	ModNatsJetStream app.ModName = "nats.jetstream"
	ModBus           app.ModName = "bus"

	ModEtcd       app.ModName = "etcd"
	ModEtcdDiscov app.ModName = "etcd.discovery"

	ModSyncBus    app.ModName = "syncbus"
	ModDataEngine app.ModName = "dataengine"
	// ModDataEngineDatabases 是 DataEngine Mod 启动时冻结的逻辑库 → 物理库表（*dataengine.DatabaseMap）。
	// 业务代码要直接打开 DAO 所在的 Mongo 库时经它取物理名，不要把 DAO 的 db= 常量当库名。
	ModDataEngineDatabases     app.ModName = "dataengine.databases"
	ModRemoteEntity            app.ModName = "remote_entity"
	ModRemoteEntityAtomicStore app.ModName = "remote_entity.atomic_store"
	// ModRemoteMirror 是只读快照能力（entity.RemoteSnapshotReadOnly，Mirror 第 5 步）：只读服务由
	// wiring/remoteentity.RemoteMirrorMod 提供，owner 进程由 RemoteEntityMod 以 Manager.SnapshotClient() 提供。
	ModRemoteMirror  app.ModName = "remote_entity.mirror"
	ModLock          app.ModName = "lock"
	ModOps           app.ModName = "ops"
	ModStatsLog      app.ModName = "stats_log"
	ModConfigData    app.ModName = "config_data"
	ModNest          app.ModName = "nest"
	ModSaga          app.ModName = "saga"
	ModEntityRuntime app.ModName = "entity.runtime"
	ModManager       app.ModName = "manager"
	// ModEntityTimer 是按实体持久定时器的驱动装配（wiring/entitytimer）。
	ModEntityTimer app.ModName = "entity_timer"
	// ModHeartbeat 是按实体心跳的驱动装配（wiring/heartbeat），同名登记 *gameplay/heartbeat.Driver。
	ModHeartbeat app.ModName = "heartbeat"
	// ModWeb 是业务 HTTP 路由的监听 Mod（wiring/web），同名登记 webroute.Registerer。
	ModWeb app.ModName = "web"
	// ModAccountServingGuard 是装配了 account 的 game 进程的启动自检（wiring/account.ServingGuardMod）：本服已并入
	// 另一服就拒绝启动。DataEngine Mod 把它列为可选依赖，让检查先于 WAL 与 outbox 打开。
	ModAccountServingGuard app.ModName = "service.account.serving_guard"
)
