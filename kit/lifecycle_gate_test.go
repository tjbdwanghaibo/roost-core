package kit_test

import (
	"testing"

	"github.com/tjbdwanghaibo/roost-core/app"
	kitconfigdata "github.com/tjbdwanghaibo/roost-core/kit/configdata"
	kitdataengine "github.com/tjbdwanghaibo/roost-core/kit/dataengine"
	kitetcd "github.com/tjbdwanghaibo/roost-core/kit/etcd"
	kitlock "github.com/tjbdwanghaibo/roost-core/kit/lock"
	kitmongo "github.com/tjbdwanghaibo/roost-core/kit/mongo"
	kitnats "github.com/tjbdwanghaibo/roost-core/kit/nats"
	kitnest "github.com/tjbdwanghaibo/roost-core/kit/nest"
	kitops "github.com/tjbdwanghaibo/roost-core/kit/ops"
	kitredis "github.com/tjbdwanghaibo/roost-core/kit/redis"
	kitremoteentity "github.com/tjbdwanghaibo/roost-core/kit/remoteentity"
	kitroom "github.com/tjbdwanghaibo/roost-core/kit/room"
	kitsaga "github.com/tjbdwanghaibo/roost-core/kit/saga"
	kitstatslog "github.com/tjbdwanghaibo/roost-core/kit/statslog"
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
