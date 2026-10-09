//go:build integration

package nats

import maildomain "github.com/tjbdwanghaibo/roost-core/service/mail"

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/framework/app"
	"github.com/tjbdwanghaibo/roost-core/infra/network/bus"
	"github.com/tjbdwanghaibo/roost-core/wiring/mail"
	kitmods "github.com/tjbdwanghaibo/roost-core/wiring/mods"
)

// summaryMail 只实现 Summary；其余方法这里用不到。
type summaryMail struct{ maildomain.Mail }

func (summaryMail) Summary(_ context.Context, playerID int64) (maildomain.Summary, error) {
	return maildomain.Summary{PlayerID: playerID, Unread: 3}, nil
}

// RR-20261006-74（F09-R3）：nats.rpc.transport=jetstream 的部署里，生成的 ClientMod 要能调用服务。
//
// 修前服务端开 JetStream RPC 后只订阅 JetStream（bus.HandleRpc），生成的 ClientMod 不读 transport
// （OptionsFromConfig / WithTransport 零调用），总走轻量 request-reply：请求被请求流存下，调用方拿到
// bus.ErrRPCCapturedByJetStream。文档写的“两端 transport 一致即可”不成立。承诺：ClientMod 按本进程的
// nats.rpc.transport 选择传输，两端都配 jetstream 时调用成功。真实 NATS（scripts/mirror-local.sh）。
// 放在 kit/nats（而不是 kit/service/mail）：它是传输用例，跟随故障矩阵已列出的包（根包 TestFaultMatrixScriptNamesEveryFullEnvironmentSuite）。
func TestRealGeneratedClientModCallsAJetStreamDeployment(t *testing.T) {
	url := os.Getenv("ROOST_DATAENGINE_IT_NATS_URL")
	if url == "" {
		t.Skip("ROOST_DATAENGINE_IT_NATS_URL is not set; run against a private environment (scripts/mirror-local.sh)")
	}
	suffix := fmt.Sprintf("%d_%d", os.Getpid(), time.Now().UnixNano())
	prefix, reqStream, respStream := "rr73"+suffix, "RR_73_REQ_"+suffix, "RR_73_RESP_"+suffix
	t.Cleanup(func() { deleteStreamsOn(t, url, reqStream, respStream) })
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
		mod := NewNatsMod(nil)
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
	if err := maildomain.RegisterHandlers(serverBus, summaryMail{}); err != nil {
		t.Fatal(err)
	}

	clientCfg := natsConfig(7302, "game")
	clientRegistry := startNats(clientCfg)
	client := mail.NewClientMod()
	if err := client.Init(clientCfg); err != nil {
		t.Fatal(err)
	}
	if err := client.Provide(clientRegistry); err != nil {
		t.Fatal(err)
	}
	remote, ok := app.Lookup[maildomain.Mail](clientRegistry, maildomain.CapabilityName)
	if !ok {
		t.Fatal("client capability not registered")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	summary, err := remote.Summary(ctx, 42)
	if err == nil && (summary.PlayerID != 42 || summary.Unread != 3) {
		t.Fatalf("summary through the ClientMod = %+v", summary)
	}
	if err != nil {
		if errors.Is(err, bus.ErrRPCCapturedByJetStream) {
			t.Fatalf("the generated ClientMod used the lightweight transport against a jetstream deployment: %v", err)
		}
		t.Fatalf("Summary through the ClientMod: %v", err)
	}
}
