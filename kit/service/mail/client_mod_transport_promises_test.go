package mail

import (
	"context"
	"testing"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/app"
	"github.com/tjbdwanghaibo/roost-core/kit/mods"
	"github.com/tjbdwanghaibo/roost-core/servicerpc"
)

// RR-20261006-74（F09-R3）：生成的 ClientMod 按本进程的 nats.rpc.transport 选择传输；构造参数里显式给的
// 传输优先于配置。真实 NATS 上的红绿见 kit/nats/client_mod_transport_real_promises_test.go（integration）。
type transportRecordingBus struct {
	*fakeBus
	reliable int
}

func (b *transportRecordingBus) CallReliable(ctx context.Context, svcType string, method string, req any, resp any) error {
	b.reliable++
	return b.fakeBus.Call(ctx, svcType, method, req, resp)
}

func (b *transportRecordingBus) CallToReliable(ctx context.Context, svcType string, _ int32, method string, req any, resp any) error {
	return b.CallReliable(ctx, svcType, method, req, resp)
}

func TestClientModFollowsTheConfiguredRPCTransport(t *testing.T) {
	for _, tc := range []struct {
		name      string
		transport string
		options   []servicerpc.Option
		reliable  int
	}{
		{"jetstream", "jetstream", nil, 1},
		{"js", "js", nil, 1},
		{"core", "core", nil, 0},
		{"unset", "", nil, 0},
		{"explicit_option_wins", "jetstream", []servicerpc.Option{servicerpc.WithTransport(servicerpc.TransportLightweight)}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := &transportRecordingBus{fakeBus: newFakeBus()}
			if err := RegisterHandlers(b.fakeBus, newHarness(t).service); err != nil {
				t.Fatal(err)
			}
			cfg := viper.New()
			if tc.transport != "" {
				cfg.Set("nats.rpc.transport", tc.transport)
			}
			registry := app.NewRegistry(cfg)
			if err := registry.Register(mods.ModBus, b); err != nil {
				t.Fatal(err)
			}
			client := NewClientMod(tc.options...)
			if err := client.Init(cfg); err != nil {
				t.Fatal(err)
			}
			if err := client.Provide(registry); err != nil {
				t.Fatal(err)
			}
			remote, _ := app.Lookup[Mail](registry, CapabilityName)
			if _, err := remote.Summary(context.Background(), 42); err != nil {
				t.Fatal(err)
			}
			if b.reliable != tc.reliable {
				t.Fatalf("transport=%q: %d reliable calls, want %d", tc.transport, b.reliable, tc.reliable)
			}
		})
	}
}
