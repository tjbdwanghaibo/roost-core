package kit_test

import (
	"testing"

	"github.com/tjbdwanghaibo/roost-core/app"
	"github.com/tjbdwanghaibo/roost-core/entity"
	"github.com/tjbdwanghaibo/roost-kit/configdata"
	"github.com/tjbdwanghaibo/roost-kit/dataengine"
	"github.com/tjbdwanghaibo/roost-kit/etcd"
	"github.com/tjbdwanghaibo/roost-kit/lock"
	"github.com/tjbdwanghaibo/roost-kit/manager"
	"github.com/tjbdwanghaibo/roost-kit/mongo"
	"github.com/tjbdwanghaibo/roost-kit/nats"
	"github.com/tjbdwanghaibo/roost-kit/nest"
	"github.com/tjbdwanghaibo/roost-kit/ops"
	"github.com/tjbdwanghaibo/roost-kit/redis"
	"github.com/tjbdwanghaibo/roost-kit/remoteentity"
	"github.com/tjbdwanghaibo/roost-kit/room"
	"github.com/tjbdwanghaibo/roost-kit/saga"
	"github.com/tjbdwanghaibo/roost-kit/statslog"
)

// app resolves DependsOn / OptionalDependsOn by Mod NAME. A dependency that
// names a capability instead — "health" (a registry built-in), "bus" or
// "nats.jetstream" (published by the NATS Mod) — is `unknown mod dependency`
// at startup, and no test in this repository assembled a real app to notice:
// every process with the data engine, saga or remote entities failed to start
// until a generated project was run (roost-codegen U-0025). Every name a kit
// Mod depends on must be the Name() of a kit Mod.
func TestEveryModDependencyNamesAKitMod(t *testing.T) {
	access := entity.NewManagerAccess(entity.NewEntityManager())
	all := []app.Mod{
		configdata.NewConfigDataMod(),
		dataengine.NewMod(dataengine.WithEntityAccess(access), dataengine.WithRemoteProjection(true)),
		etcd.NewEtcdMod(),
		lock.NewLockMod(),
		manager.NewManagerMod(),
		mongo.NewMongoMod(),
		nats.NewNatsMod(nil),
		nest.NewMod(access),
		ops.NewOpsMod(),
		redis.NewRedisMod(),
		remoteentity.NewRemoteEntityMod(0, remoteentity.WithMongoStorage(access)),
		room.NewRoomMod(0),
		saga.NewMod(),
		statslog.NewStatsLogMod(),
	}
	names := map[app.ModName]bool{}
	for _, mod := range all {
		names[mod.Name()] = true
	}
	for _, mod := range all {
		if provider, ok := mod.(app.ModDependencyProvider); ok {
			for _, dep := range provider.DependsOn() {
				if !names[dep] {
					t.Errorf("%s depends on %q, which is not the name of any kit Mod", mod.Name(), dep)
				}
			}
		}
		if provider, ok := mod.(app.ModOptionalDependencyProvider); ok {
			for _, dep := range provider.OptionalDependsOn() {
				if !names[dep] {
					t.Errorf("%s optionally depends on %q, which is not the name of any kit Mod", mod.Name(), dep)
				}
			}
		}
	}
}
