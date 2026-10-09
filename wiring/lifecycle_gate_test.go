package kit_test

import (
	"testing"

	"github.com/tjbdwanghaibo/roost-core/framework/app"
	kitconfigdata "github.com/tjbdwanghaibo/roost-core/wiring/configdata"
	kitdataengine "github.com/tjbdwanghaibo/roost-core/wiring/dataengine"
	kitetcd "github.com/tjbdwanghaibo/roost-core/wiring/etcd"
	kitlock "github.com/tjbdwanghaibo/roost-core/wiring/lock"
	kitmanager "github.com/tjbdwanghaibo/roost-core/wiring/manager"
	kitmongo "github.com/tjbdwanghaibo/roost-core/wiring/mongo"
	kitnats "github.com/tjbdwanghaibo/roost-core/wiring/nats"
	kitnest "github.com/tjbdwanghaibo/roost-core/wiring/nest"
	kitops "github.com/tjbdwanghaibo/roost-core/wiring/ops"
	kitredis "github.com/tjbdwanghaibo/roost-core/wiring/redis"
	kitremoteentity "github.com/tjbdwanghaibo/roost-core/wiring/remoteentity"
	kitsaga "github.com/tjbdwanghaibo/roost-core/wiring/saga"
	kitstatslog "github.com/tjbdwanghaibo/roost-core/wiring/statslog"
	kitsyncbus "github.com/tjbdwanghaibo/roost-core/wiring/syncbus"
)

// This compile-time list is the lifecycle gate for every infrastructure Mod
// shipped by roost-kit. New built-ins must join it instead of relying on App's
// legacy unbounded Stop fallback.
func TestBuiltInModsImplementContextStop(t *testing.T) {
	implementations := []app.ModStopperWithContext{
		(*kitdataengine.Mod)(nil),
		(*kitmanager.ManagerMod)(nil),
		(*kitconfigdata.Mod)(nil),
		(*kitetcd.EtcdMod)(nil),
		(*kitlock.LockMod)(nil),
		(*kitmongo.MongoMod)(nil),
		(*kitnats.NatsMod)(nil),
		(*kitnest.Mod)(nil),
		(*kitops.OpsMod)(nil),
		(*kitredis.RedisMod)(nil),
		(*kitremoteentity.RemoteEntityMod)(nil),
		(*kitsaga.Mod)(nil),
		(*kitstatslog.StatsLogMod)(nil),
		(*kitsyncbus.SyncBusMod)(nil),
	}
	if len(implementations) != 14 {
		t.Fatalf("lifecycle gate list = %d, want 14", len(implementations))
	}
}
