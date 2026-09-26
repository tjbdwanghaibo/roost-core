package syncbus

import (
	"github.com/spf13/viper"
	"testing"
	"time"
)

func TestCanonicalSyncBusConfigAndLegacyPrecedence(t *testing.T) {
	for _, section := range []string{"sync", "room", "syncbus"} {
		t.Run(section, func(t *testing.T) {
			cfg := viper.New()
			cfg.Set(section+".transport", "jetstream")
			cfg.Set(section+".prefix", "chosen")
			cfg.Set(section+".stream", "EVENTS")
			cfg.Set(section+".replicas", 3)
			cfg.Set(section+".ack_wait", "7s")
			mod := NewSyncBusMod(7)
			if err := mod.Init(cfg); err != nil {
				t.Fatal(err)
			}
			if !mod.useJetStream() || mod.prefix != "chosen" || mod.jsCfg.Stream != "EVENTS" || mod.jsCfg.Replicas != 3 || mod.jsCfg.AckWait != 7*time.Second {
				t.Fatalf("config ignored: %+v", mod)
			}
		})
	}
	cfg := viper.New()
	cfg.Set("sync.transport", "nats")
	cfg.Set("room.transport", "nats")
	cfg.Set("syncbus.transport", "jetstream")
	mod := NewSyncBusMod(7)
	if err := mod.Init(cfg); err != nil {
		t.Fatal(err)
	}
	if !mod.useJetStream() {
		t.Fatal("legacy overrode canonical config")
	}
}
