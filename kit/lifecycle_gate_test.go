package kit_test

import (
	"testing"

	"github.com/tjbdwanghaibo/roost-core/app"
	kitconfigdata "github.com/tjbdwanghaibo/roost-kit/configdata"
	kitdataengine "github.com/tjbdwanghaibo/roost-kit/dataengine"
	kitetcd "github.com/tjbdwanghaibo/roost-kit/etcd"
	kitlock "github.com/tjbdwanghaibo/roost-kit/lock"
	kitmongo "github.com/tjbdwanghaibo/roost-kit/mongo"
	kitnats "github.com/tjbdwanghaibo/roost-kit/nats"
	kitnest "github.com/tjbdwanghaibo/roost-kit/nest"
	kitops "github.com/tjbdwanghaibo/roost-kit/ops"
	kitredis "github.com/tjbdwanghaibo/roost-kit/redis"
	kitremoteentity "github.com/tjbdwanghaibo/roost-kit/remoteentity"
	kitroom "github.com/tjbdwanghaibo/roost-kit/room"
	kitsaga "github.com/tjbdwanghaibo/roost-kit/saga"
	kitstatslog "github.com/tjbdwanghaibo/roost-kit/statslog"
)

// This compile-time list is the lifecycle gate for every infrastructure Mod
// shipped by roost-kit. New built-ins must join it instead of relying on App's
// legacy unbounded Stop fallback.
func TestBuiltInModsImplementContextStop(t *testing.T) {
	implementations := []app.ModStopperWithContext{
		(*kitdataengine.Mod)(nil),
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
		(*kitroom.RoomMod)(nil),
	}
	if len(implementations) != 13 {
		t.Fatalf("lifecycle gate list = %d, want 13", len(implementations))
	}
}
