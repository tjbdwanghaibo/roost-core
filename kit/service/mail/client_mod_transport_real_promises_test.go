//go:build integration

package mail

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	gonats "github.com/nats-io/nats.go"
	gojs "github.com/nats-io/nats.go/jetstream"
	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/app"
	"github.com/tjbdwanghaibo/roost-core/bus"
	kitmods "github.com/tjbdwanghaibo/roost-core/kit/mods"
	kitnats "github.com/tjbdwanghaibo/roost-core/kit/nats"
)

// RR-20261006-74（F09-R3）：nats.rpc.transport=jetstream 的部署里，生成的 ClientMod 要能调用服务。
//
// 修前服务端开 JetStream RPC 后只订阅 JetStream（bus.HandleRpc），生成的 ClientMod 不读 transport
// （OptionsFromConfig / WithTransport 零调用），总走轻量 request-reply：请求被请求流存下，调用方拿到
// bus.ErrRPCCapturedByJetStream。文档写的“两端 transport 一致即可”不成立。承诺：ClientMod 按本进程的
// nats.rpc.transport 选择传输，两端都配 jetstream 时调用成功。真实 NATS（scripts/mirror-local.sh）。
func TestRealGeneratedClientModCallsAJetStreamDeployment(t *testing.T) {
	url := os.Getenv("ROOST_DATAENGINE_IT_NATS_URL")
	if url == "" {
		t.Skip("ROOST_DATAENGINE_IT_NATS_URL is not set; run against a private environment (scripts/mirror-local.sh)")
	}
	suffix := fmt.Sprintf("%d_%d", os.Getpid(), time.Now().UnixNano())
	prefix, reqStream, respStream := "rr73"+suffix, "RR_73_REQ_"+suffix, "RR_73_RESP_"+suffix
	t.Cleanup(func() { deleteRR73Streams(t, url, reqStream, respStream) })
	natsConfig := func(sid int32, serverType string) *viper.Viper {
		cfg := viper.New()
		cfg.Set("sid", sid)
		cfg.Set("server_type", serverType)
		cfg.Set("nats.url", url)
		cfg.Set("nats.prefix", prefix)
		cfg.Set("nats.rpc.transport", "jetstream")
		cfg.Set("nats.rpc.request_stream", reqStream)
		cfg.Set("nats.rpc.response_stream", respStream)
		cfg.Set("nats.rpc.max_bytes", int64(8<<20))
		cfg.Set("nats.rpc.setup_timeout", 20*time.Second)
		return cfg
	}
	startNats := func(cfg *viper.Viper) *app.Registry {
		mod := kitnats.NewNatsMod(nil)
		if err := mod.Init(cfg); err != nil {
			t.Fatal(err)
		}
		registry := app.NewRegistry(cfg)
		if err := mod.Provide(registry); err != nil {
			t.Fatal(err)
		}
		if err := mod.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = mod.StopWithContext(context.Background()) })
		return registry
	}

	serverRegistry := startNats(natsConfig(7301, "mail"))
	serverBus, _ := app.Lookup[bus.IBus](serverRegistry, kitmods.ModBus)
	if err := RegisterHandlers(serverBus, newHarness(t).service); err != nil {
		t.Fatal(err)
	}

	clientCfg := natsConfig(7302, "game")
	clientRegistry := startNats(clientCfg)
	client := NewClientMod()
	if err := client.Init(clientCfg); err != nil {
		t.Fatal(err)
	}
	if err := client.Provide(clientRegistry); err != nil {
		t.Fatal(err)
	}
	remote, ok := app.Lookup[Mail](clientRegistry, CapabilityName)
	if !ok {
		t.Fatal("client capability not registered")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := remote.Summary(ctx, 42); err != nil {
		if errors.Is(err, bus.ErrRPCCapturedByJetStream) {
			t.Fatalf("the generated ClientMod used the lightweight transport against a jetstream deployment: %v", err)
		}
		t.Fatalf("Summary through the ClientMod: %v", err)
	}
}

func deleteRR73Streams(t *testing.T, url string, streams ...string) {
	t.Helper()
	nc, err := gonats.Connect(url, gonats.Timeout(2*time.Second))
	if err != nil {
		t.Errorf("cleanup: connect: %v", err)
		return
	}
	defer nc.Close()
	js, err := gojs.New(nc)
	if err != nil {
		t.Errorf("cleanup: JetStream: %v", err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, stream := range streams {
		if err := js.DeleteStream(ctx, stream); err != nil && !errors.Is(err, gojs.ErrStreamNotFound) {
			t.Errorf("cleanup: delete stream %s: %v", stream, err)
		}
	}
}
