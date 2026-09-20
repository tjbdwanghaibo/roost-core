package room

import (
	"strings"
	"testing"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/app"
	"github.com/tjbdwanghaibo/roost-kit/mods"
)

// U-0150 · C2 · gap map kit `room` 3/3：Provide 对缺健康能力的注册表、缺 NATS 客户端的注册表拒绝。
func TestRoomModRefusesRegistriesWithoutHealthOrNats(t *testing.T) {
	if err := (&RoomMod{}).Provide(&app.Registry{}); err == nil || !strings.Contains(err.Error(), string(mods.ModHealth)+`" not found`) {
		t.Fatalf("Provide with a bare registry = %v, want the health capability named", err)
	}
	if err := (&RoomMod{}).Provide(app.NewRegistry(viper.New())); err == nil || !strings.Contains(err.Error(), string(mods.ModNats)+`" not found`) {
		t.Fatalf("Provide without a nats client = %v", err)
	}
	if err := (&RoomMod{transport: "jetstream"}).Provide(app.NewRegistry(viper.New())); err == nil || !strings.Contains(err.Error(), string(mods.ModNatsJetStream)+`" not found`) {
		t.Fatalf("Provide over JetStream without a JetStream client = %v", err)
	}
}
