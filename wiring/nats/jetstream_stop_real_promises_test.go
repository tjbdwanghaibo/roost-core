//go:build integration

// RR-20261005-NC-90：真实 NATS 上，JetStream RPC handler 在途时停止服务端 NatsMod。旧行为：
// Bus 只排空自己的 pool，JetStream handler 在 nats.go consume 回调里，Stop 50ms 返回 nil，
// NatsMod 随即关闭连接；handler 的回包随 Bus 停止被取消、settle 报 connection closed，调用方等满
// 超时，请求在 AckWait 后重投再执行一次。承诺：handler 未退出时停止返回预算的 ctx 错误并保留
// bus / asm；handler 退出后重试停止返回 nil 并释放；回包送达调用方，不重投。
package nats

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"

	gonats "github.com/nats-io/nats.go"
	gojs "github.com/nats-io/nats.go/jetstream"
	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/framework/app"
	"github.com/tjbdwanghaibo/roost-core/infra/network/bus"
	"github.com/tjbdwanghaibo/roost-core/wiring/mods"
)

func startJetStreamRPCMod(t *testing.T, url, prefix, reqStream, respStream string, sid int32) (*NatsMod, *bus.Bus) {
	t.Helper()
	cfg := viper.New()
	cfg.Set("sid", sid)
	cfg.Set("server_type", "nc90")
	cfg.Set("nats.url", url)
	cfg.Set("nats.prefix", prefix)
	cfg.Set("nats.rpc.transport", "jetstream")
	cfg.Set("nats.rpc.request_stream", reqStream)
	cfg.Set("nats.rpc.response_stream", respStream)
	cfg.Set("nats.rpc.ack_wait", time.Second)
	cfg.Set("nats.rpc.max_bytes", int64(8<<20))
	cfg.Set("nats.rpc.setup_timeout", 20*time.Second)
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
	ibus, ok := app.Lookup[bus.IBus](registry, mods.ModBus)
	if !ok {
		t.Fatal("bus capability not published")
	}
	return mod, ibus.(*bus.Bus)
}

func deleteStreamsOn(t *testing.T, url string, streams ...string) {
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

func TestRealJetStreamStopWaitsForInFlightHandler(t *testing.T) {
	url := os.Getenv("ROOST_DATAENGINE_IT_NATS_URL")
	if url == "" {
		t.Skip("ROOST_DATAENGINE_IT_NATS_URL is not set; run through scripts/integration/dataengine-env.sh")
	}
	suffix := fmt.Sprintf("%d_%d", os.Getpid(), time.Now().UnixNano())
	prefix, reqStream, respStream := "rrnc90"+suffix, "RR_NC90_REQ_"+suffix, "RR_NC90_RESP_"+suffix
	t.Cleanup(func() { deleteStreamsOn(t, url, reqStream, respStream) })
	server, serverBus := startJetStreamRPCMod(t, url, prefix, reqStream, respStream, 9901)
	_, clientBus := startJetStreamRPCMod(t, url, prefix, reqStream, respStream, 9902)

	entered, release := make(chan struct{}, 4), make(chan struct{})
	var runs atomic.Int32
	if err := serverBus.HandleRpc("Slow", func(*bus.RpcContext) (any, error) {
		runs.Add(1)
		entered <- struct{}{}
		<-release
		return map[string]string{"ok": "yes"}, nil
	}); err != nil {
		t.Fatal(err)
	}
	released := false
	t.Cleanup(func() {
		if !released {
			close(release)
		}
	})
	callDone := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		var resp map[string]string
		err := clientBus.CallReliable(ctx, "nc90", "Slow", map[string]string{}, &resp)
		if err == nil && resp["ok"] != "yes" {
			err = fmt.Errorf("unexpected response %v", resp)
		}
		callDone <- err
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("handler never entered")
	}

	budget, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	first := server.StopWithContext(budget)
	t.Logf("budgeted stop with the handler in flight: err=%v bus_retained=%v asm_retained=%v", first, server.bus != nil, server.asm != nil)
	if !errors.Is(first, context.DeadlineExceeded) || server.asm == nil {
		t.Fatal("stop reported success, or released the connection, while the JetStream handler was still running")
	}

	close(release)
	released = true
	retry := server.StopWithContext(context.Background())
	t.Logf("retry stop after the handler returned: err=%v asm_retained=%v", retry, server.asm != nil)
	if retry != nil || server.asm != nil {
		t.Fatal("stop did not converge after the in-flight handler returned")
	}
	select {
	case err := <-callDone:
		if err != nil {
			t.Fatalf("caller of the request in flight at the stop: %v (want the response)", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("caller never returned")
	}
	// The request must be settled: acknowledged once, nothing pending for redelivery.
	nc, err := gonats.Connect(url, gonats.Timeout(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	js, _ := gojs.New(nc)
	ctx, cancelInfo := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelInfo()
	consumer, err := js.Consumer(ctx, reqStream, "rpc_nc90_Slow")
	if err != nil {
		t.Fatal(err)
	}
	info, err := consumer.Info(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("request consumer: delivered=%d ack_floor=%d ack_pending=%d redelivered=%d runs=%d", info.Delivered.Consumer, info.AckFloor.Consumer, info.NumAckPending, info.NumRedelivered, runs.Load())
	if info.NumAckPending != 0 || info.AckFloor.Consumer != info.Delivered.Consumer || runs.Load() != 1 {
		t.Fatal("the request in flight at the stop was not acknowledged; the broker will redeliver it")
	}
}
