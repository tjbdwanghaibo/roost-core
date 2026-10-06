package syncbus

import (
	"strings"
	"testing"
	"time"

	"github.com/spf13/viper"
)

// A4 ①：syncbus 只读 syncbus 段（room / sync 同名回退删除，线上未部署不做旧格式兼容）；写在旧段里的键
// syncbus 不再读取，App 启动时作为没有声明的键告警、doctor 报出。
func TestSyncBusReadsTheSyncbusSection(t *testing.T) {
	cfg := viper.New()
	cfg.Set("syncbus.transport", "jetstream")
	cfg.Set("syncbus.prefix", "chosen")
	cfg.Set("syncbus.stream", "EVENTS")
	cfg.Set("syncbus.replicas", 3)
	cfg.Set("syncbus.ack_wait", "7s")
	cfg.Set("syncbus.storage", "Memory")
	cfg.Set("room.transport", "nats")
	mod := NewSyncBusMod(7)
	if err := mod.Init(cfg); err != nil {
		t.Fatal(err)
	}
	if !mod.useJetStream() || mod.prefix != "chosen" || mod.jsCfg.Stream != "EVENTS" || mod.jsCfg.Replicas != 3 || mod.jsCfg.AckWait != 7*time.Second || mod.jsCfg.Storage != "memory" {
		t.Fatalf("config ignored: %+v", mod)
	}
	legacy := viper.New()
	legacy.Set("room.transport", "jetstream")
	mod = NewSyncBusMod(7)
	if err := mod.Init(legacy); err != nil {
		t.Fatal(err)
	}
	if mod.useJetStream() {
		t.Fatal("the removed room section still selects the transport")
	}
}

// transport 写错（例如 jetsream）旧实现静默退回普通 NATS（RR-20260926-12），现在声明的枚举拒绝。
func TestSyncBusRefusesAnUnknownTransport(t *testing.T) {
	for _, key := range []string{"syncbus.transport", "syncbus.storage"} {
		cfg := viper.New()
		cfg.Set(key, "jetsream")
		if err := NewSyncBusMod(7).Init(cfg); err == nil || !strings.Contains(err.Error(), key) {
			t.Fatalf("%s: jetsream: Init = %v, want a refusal naming %s", key, err, key)
		}
	}
}
