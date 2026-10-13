package integration

import (
	"reflect"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/framework/app"
	kitmods "github.com/tjbdwanghaibo/roost-core/wiring/mods"

	"github.com/tjbdwanghaibo/roost-core/wiring/account"
	"github.com/tjbdwanghaibo/roost-core/wiring/activity"
	"github.com/tjbdwanghaibo/roost-core/wiring/chat"
	"github.com/tjbdwanghaibo/roost-core/wiring/global"
	"github.com/tjbdwanghaibo/roost-core/wiring/mail"
	"github.com/tjbdwanghaibo/roost-core/wiring/match"
	"github.com/tjbdwanghaibo/roost-core/wiring/platform"
	"github.com/tjbdwanghaibo/roost-core/wiring/rank"
	"github.com/tjbdwanghaibo/roost-core/wiring/session"
)

// app 按 Mod 名解析 DependsOn：生成的 ClientMod 依赖发布总线的 NATS Mod，而不是总线的能力名（没有 Mod 叫 "bus"，依赖它启动即报
// unknown mod dependency）。这里对本模块发出的每个客户端钉住这一点。
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
