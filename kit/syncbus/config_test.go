package syncbus

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/spf13/viper"
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

// captureWarnings 收集 fn 期间默认 logger 的 WARN 及以上输出（JSON 行）。
func captureWarnings(t *testing.T, fn func()) string {
	t.Helper()
	var out bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&out, &slog.HandlerOptions{Level: slog.LevelWarn})))
	defer slog.SetDefault(previous)
	fn()
	return out.String()
}

// RR-20260926-12（复核残留）：配置不被读取不能是静默的。
//
// 旧 room / sync 段仍兼容，但要告警弃用；被正式 syncbus 段遮住的旧键、三个段里本 Mod
// 不认识的键都没有任何效果，也要在启动日志里说出来——RR-12 的根因就是配置段改名后
// 整段被忽略而没有任何提示。sync.entity 属于 kit/nest 的 Entity 同步配置，不算未知键。
// transport 写错（例如 jetsream）旧实现静默退回普通 NATS，现在 Init 直接报错。
func TestSyncBusConfigThatIsNotReadIsReported(t *testing.T) {
	t.Run("room section is deprecated", func(t *testing.T) {
		cfg := viper.New()
		cfg.Set("room.transport", "jetstream")
		logs := captureWarnings(t, func() {
			if err := NewSyncBusMod(7).Init(cfg); err != nil {
				t.Fatal(err)
			}
		})
		if !strings.Contains(logs, `room.transport`) || !strings.Contains(logs, "deprecated") {
			t.Fatalf("room section read without a deprecation warning: %q", logs)
		}
	})
	t.Run("shadowed legacy keys and unknown keys", func(t *testing.T) {
		cfg := viper.New()
		cfg.Set("syncbus.transport", "jetstream")
		cfg.Set("syncbus.publish_timeut", "3s") // typo
		cfg.Set("room.transport", "nats")
		cfg.Set("room.prefix", "legacy")
		cfg.Set("sync.entity.mode", "on_change") // kit/nest owns this
		logs := captureWarnings(t, func() {
			if err := NewSyncBusMod(7).Init(cfg); err != nil {
				t.Fatal(err)
			}
		})
		for _, want := range []string{"syncbus.publish_timeut", "room.transport", "room.prefix"} {
			if !strings.Contains(logs, want) {
				t.Errorf("no warning names %s: %q", want, logs)
			}
		}
		if strings.Contains(logs, "sync.entity") {
			t.Errorf("kit/nest's sync.entity was reported as a syncbus key: %q", logs)
		}
	})
	t.Run("canonical config is quiet", func(t *testing.T) {
		cfg := viper.New()
		cfg.Set("syncbus.transport", "jetstream")
		cfg.Set("syncbus.prefix", "roost.sync")
		cfg.Set("syncbus.storage", "file")
		cfg.Set("syncbus.replicas", 1)
		cfg.Set("syncbus.publish_timeout", "3s")
		cfg.Set("sync.entity.mode", "periodic")
		logs := captureWarnings(t, func() {
			if err := NewSyncBusMod(7).Init(cfg); err != nil {
				t.Fatal(err)
			}
		})
		if logs != "" {
			t.Fatalf("the generated syncbus section produced warnings: %q", logs)
		}
	})
	t.Run("unknown transport is refused", func(t *testing.T) {
		cfg := viper.New()
		cfg.Set("syncbus.transport", "jetsream")
		if err := NewSyncBusMod(7).Init(cfg); err == nil {
			t.Fatal("a misspelled transport silently fell back to plain NATS")
		}
	})
}
