package integration

import (
	"reflect"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/framework/app"
	kitmods "github.com/tjbdwanghaibo/roost-core/wiring/mods"

	"github.com/tjbdwanghaibo/roost-core/wiring/account"
	"github.com/tjbdwanghaibo/roost-core/wiring/chat"
	"github.com/tjbdwanghaibo/roost-core/wiring/global"
	"github.com/tjbdwanghaibo/roost-core/wiring/activity"
	"github.com/tjbdwanghaibo/roost-core/wiring/mail"
	"github.com/tjbdwanghaibo/roost-core/wiring/match"
	"github.com/tjbdwanghaibo/roost-core/wiring/platform"
	"github.com/tjbdwanghaibo/roost-core/wiring/rank"
	"github.com/tjbdwanghaibo/roost-core/wiring/session"
)

// app resolves a Mod's DependsOn by Mod NAME. The generated ClientMods used
// to depend on kitmods.ModBus — the name of the bus CAPABILITY, which no Mod
// is called — so a process assembling a client next to the NATS mod failed at
// startup with `unknown mod dependency "bus"`. Every generated client had this
// and nothing here ran a real app to notice; a generated game template did
// (roost-codegen U-0024). The dependency now names the Mod that publishes the
// bus, and this test pins that for every client this module ships.
func TestEveryClientModDependsOnTheNATSMod(t *testing.T) {
	clients := map[string]app.ModDependencyProvider{
		"account":  account.NewClientMod(),
		"chat":     chat.NewClientMod(),
		"global":   global.NewClientMod(),
		"activity": activity.NewClientMod(),
		"mail":     mail.NewClientMod(),
		"match":    match.NewClientMod(),
		"platform": platform.NewClientMod(),
		"rank":     rank.NewClientMod(),
		"session":  session.NewClientMod(),
	}
	// match and activity route methods by an affinity key, which needs the
	// etcd Mod's discovery (RR-20261006-59); the others need only the bus.
	affinity := map[string]bool{"match": true, "activity": true}
	for name, client := range clients {
		want := []app.ModName{kitmods.ModNats}
		if affinity[name] {
			want = append(want, kitmods.ModEtcd)
		}
		got := client.DependsOn()
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s ClientMod depends on %v; want %v (the NATS mod by name, not the bus capability)", name, got, want)
		}
	}
}
