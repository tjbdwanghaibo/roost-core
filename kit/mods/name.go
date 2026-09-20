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

	ModRedis      app.ModName = "redis"
	ModRedisLock  app.ModName = "redis.lock"
	ModRedisVLock app.ModName = "redis.versioned_lock"

	ModNats          app.ModName = "nats"
	ModNatsRpc       app.ModName = "nats.rpc"
	ModNatsJetStream app.ModName = "nats.jetstream"
	ModBus           app.ModName = "bus"

	ModEtcd         app.ModName = "etcd"
	ModEtcdDiscov   app.ModName = "etcd.discovery"
	ModEtcdElection app.ModName = "etcd.election"

	ModRoom                    app.ModName = "room"
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
