package syncbus

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/app"
	"github.com/tjbdwanghaibo/roost-core/kit/mods"
	fnats "github.com/tjbdwanghaibo/roost-core/nats"
)

// RR-20260926-56：kit 装配的 JetStream 流名随 prefix 隔离，默认配置流名不变，
// 显式 syncbus.stream 优先，启动日志说出实际流名。
//
// 兼容边界：生成配置写 prefix: roost.sync，未写 prefix 时 kit 缺省为 roost.room；
// 两者在修复前都落在 ROOST_SYNC 流上，修复后仍是 ROOST_SYNC（已部署的游标不变）。
func TestSyncBusStreamIsDerivedFromThePrefix(t *testing.T) {
	for _, tc := range []struct {
		name   string
		config map[string]any
		want   string
	}{
		{name: "generated default prefix", config: map[string]any{"syncbus.prefix": "roost.sync"}, want: "ROOST_SYNC"},
		{name: "no prefix configured", config: map[string]any{}, want: "ROOST_SYNC"},
		{name: "kit default prefix written out", config: map[string]any{"syncbus.prefix": "roost.room"}, want: "ROOST_SYNC"},
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
			if got := startedStream(logs); got != tc.want {
				t.Fatalf("the start log names stream %q, the bus runs on %q: %s", got, tc.want, logs)
			}
			// RR-20260927-35：导出给生成工程测试用的解析函数必须与 Mod 实际确保的流一致，
			// 含 roost.room 的兼容映射。
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
