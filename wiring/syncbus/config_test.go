package syncbus

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/framework/app"
	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
	"github.com/tjbdwanghaibo/roost-core/wiring/mods"
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

// transport 写错（例如 jetsream）由声明的枚举拒绝，不能静默退回普通 NATS。
func TestSyncBusRefusesAnUnknownTransport(t *testing.T) {
	for _, key := range []string{"syncbus.transport", "syncbus.storage"} {
		cfg := viper.New()
		cfg.Set(key, "jetsream")
		if err := NewSyncBusMod(7).Init(cfg); err == nil || !strings.Contains(err.Error(), key) {
			t.Fatalf("%s: jetsream: Init = %v, want a refusal naming %s", key, err, key)
		}
	}
}

// 装配的 JetStream 流名随 prefix 隔离，默认配置流名不变，显式 syncbus.stream 优先，启动日志说出实际流名。
//
// 默认边界：显式写 prefix: roost.sync（旧生成配置的值）与未写 prefix（缺省 roost.sync）流名都是 ROOST_SYNC；
// 新生成配置按工程名写 <project>.sync，流名 <PROJECT>_SYNC，由下面的隔离前缀用例覆盖。
// room 时代的 roost.room 不再特例映射到 ROOST_SYNC，按规则推出自己的流。
func TestSyncBusStreamIsDerivedFromThePrefix(t *testing.T) {
	for _, tc := range []struct {
		name   string
		config map[string]any
		want   string
	}{
		{name: "explicit default prefix", config: map[string]any{"syncbus.prefix": "roost.sync"}, want: "ROOST_SYNC"},
		{name: "no prefix configured", config: map[string]any{}, want: "ROOST_SYNC"},
		{name: "room-era prefix is not mapped", config: map[string]any{"syncbus.prefix": "roost.room"}, want: "ROOST_ROOM"},
		{name: "isolated prefix", config: map[string]any{"syncbus.prefix": "zz3640.sync"}, want: "ZZ3640_SYNC"},
		{name: "explicit stream wins", config: map[string]any{"syncbus.prefix": "zz3640.sync", "syncbus.stream": "ZZ3640_OWN"}, want: "ZZ3640_OWN"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := viper.New()
			cfg.Set("syncbus.transport", "jetstream")
			for key, value := range tc.config {
				cfg.Set(key, value)
			}
			js := &recordingJetStream{}
			logs := startSyncBus(t, cfg, js)
			if len(js.streams) != 1 || js.streams[0].Name != tc.want {
				t.Fatalf("ensured streams %+v, want exactly %s", js.streams, tc.want)
			}
			if _, written := tc.config["syncbus.prefix"]; !written && !slices.Equal(js.streams[0].Subjects, []string{"roost.sync.>"}) {
				t.Fatalf("with no prefix configured the stream holds %v, want the default prefix roost.sync", js.streams[0].Subjects)
			}
			if got := startedStream(logs); got != tc.want {
				t.Fatalf("the start log names stream %q, the bus runs on %q: %s", got, tc.want, logs)
			}
			// RR-20260927-35：导出给生成工程测试用的解析函数必须与 Mod 实际确保的流一致。
			if got := JetStreamStreamFromConfig(cfg); got != tc.want {
				t.Fatalf("JetStreamStreamFromConfig = %q, the bus runs on %q", got, tc.want)
			}
		})
	}
	t.Run("two prefixes on one NATS do not share a stream", func(t *testing.T) {
		names := map[string]bool{}
		for _, prefix := range []string{"roost.sync", "zz3640.sync", "zz3641.sync"} {
			cfg := viper.New()
			cfg.Set("syncbus.transport", "jetstream")
			cfg.Set("syncbus.prefix", prefix)
			js := &recordingJetStream{}
			startSyncBus(t, cfg, js)
			if names[js.streams[0].Name] {
				t.Fatalf("prefix %s reuses stream %s", prefix, js.streams[0].Name)
			}
			names[js.streams[0].Name] = true
		}
	})
}

func startSyncBus(t *testing.T, cfg *viper.Viper, js fnats.IJetStream) string {
	t.Helper()
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	defer slog.SetDefault(previous)
	// NewRegistry 自带 health 能力。
	registry := app.NewRegistry(cfg)
	if err := registry.Register(mods.ModNatsJetStream, js); err != nil {
		t.Fatal(err)
	}
	mod := NewSyncBusMod(7)
	if err := mod.Init(cfg); err != nil {
		t.Fatal(err)
	}
	if err := mod.Provide(registry); err != nil {
		t.Fatal(err)
	}
	if err := mod.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mod.StopWithContext(context.Background()) })
	return logs.String()
}

func startedStream(logs string) string {
	for line := range strings.Lines(logs) {
		var entry struct {
			Msg    string `json:"msg"`
			Stream string `json:"stream"`
		}
		if json.Unmarshal([]byte(line), &entry) == nil && entry.Msg == "syncbus mod: started" {
			return entry.Stream
		}
	}
	return ""
}

type recordingJetStream struct {
	streams []fnats.JetStreamConfig
}

func (js *recordingJetStream) EnsureStream(_ context.Context, cfg fnats.JetStreamConfig) error {
	js.streams = append(js.streams, cfg)
	return nil
}

func (*recordingJetStream) Publish(context.Context, string, []byte, fnats.JetStreamPublishOptions) (fnats.JetStreamPublishAck, error) {
	return fnats.JetStreamPublishAck{}, nil
}

func (*recordingJetStream) Subscribe(context.Context, fnats.JetStreamConsumerConfig, fnats.JetStreamHandler) (fnats.IJetStreamSubscription, error) {
	return nil, nil
}

// Provide 对缺健康能力的注册表、缺 NATS 客户端的注册表拒绝。
func TestSyncBusModRefusesRegistriesWithoutHealthOrNats(t *testing.T) {
	if err := (&SyncBusMod{}).Provide(&app.Registry{}); err == nil || !strings.Contains(err.Error(), string(mods.ModHealth)+`" not found`) {
		t.Fatalf("Provide with a bare registry = %v, want the health capability named", err)
	}
	if err := (&SyncBusMod{}).Provide(app.NewRegistry(viper.New())); err == nil || !strings.Contains(err.Error(), string(mods.ModNats)+`" not found`) {
		t.Fatalf("Provide without a nats client = %v", err)
	}
	if err := (&SyncBusMod{transport: "jetstream"}).Provide(app.NewRegistry(viper.New())); err == nil || !strings.Contains(err.Error(), string(mods.ModNatsJetStream)+`" not found`) {
		t.Fatalf("Provide over JetStream without a JetStream client = %v", err)
	}
}
