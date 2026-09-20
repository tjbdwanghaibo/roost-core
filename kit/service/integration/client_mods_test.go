package integration

import (
	"reflect"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/app"
	kitmods "github.com/tjbdwanghaibo/roost-kit/mods"

	"github.com/tjbdwanghaibo/roost-kit/service/account"
	"github.com/tjbdwanghaibo/roost-kit/service/chat"
	"github.com/tjbdwanghaibo/roost-kit/service/global"
	"github.com/tjbdwanghaibo/roost-kit/service/global/activity"
	"github.com/tjbdwanghaibo/roost-kit/service/mail"
	"github.com/tjbdwanghaibo/roost-kit/service/match"
	"github.com/tjbdwanghaibo/roost-kit/service/platform"
	"github.com/tjbdwanghaibo/roost-kit/service/rank"
	"github.com/tjbdwanghaibo/roost-kit/service/session"
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
	for name, client := range clients {
		got := client.DependsOn()
		if !reflect.DeepEqual(got, []app.ModName{kitmods.ModNats}) {
			t.Errorf("%s ClientMod depends on %v; it must name the NATS mod (%q), not the bus capability", name, got, kitmods.ModNats)
		}
	}
}
