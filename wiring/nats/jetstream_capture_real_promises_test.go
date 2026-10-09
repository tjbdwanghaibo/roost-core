//go:build integration

// RR-20261005-NC-92：真实 NATS 上，服务端开 JetStream RPC，调用端用轻量 Call / CallTo。旧行为：请求流
// （<prefix>.rpc.>）把 core request 存下并回 PubAck，调用方报 “unsupported rpc response version 0”，
// 同一请求随后被 JetStream 消费者无期限执行（handler runs=1、无 deadline）。承诺：调用方拿到
// bus.ErrRPCCapturedByJetStream，服务端不执行、消息被确认。
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
	"github.com/tjbdwanghaibo/roost-core/infra/network/bus"
)

func TestRealLightweightCallIntoJetStreamDeploymentIsRefused(t *testing.T) {
	url := os.Getenv("ROOST_DATAENGINE_IT_NATS_URL")
	if url == "" {
		t.Skip("ROOST_DATAENGINE_IT_NATS_URL is not set; run through scripts/integration/dataengine-env.sh")
	}
	suffix := fmt.Sprintf("%d_%d", os.Getpid(), time.Now().UnixNano())
	prefix, reqStream, respStream := "rrnc92"+suffix, "RR_NC92_REQ_"+suffix, "RR_NC92_RESP_"+suffix
	t.Cleanup(func() { deleteStreamsOn(t, url, reqStream, respStream) })
	_, serverBus := startJetStreamRPCMod(t, url, prefix, reqStream, respStream, 9921)
	_, clientBus := startJetStreamRPCMod(t, url, prefix, reqStream, respStream, 9922)
	var runs atomic.Int32
	if err := serverBus.HandleRpc("Grant", func(*bus.RpcContext) (any, error) {
		runs.Add(1)
		return map[string]string{"granted": "1"}, nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, call := range []struct {
		name string
		do   func(context.Context) error
	}{
		{"Call", func(ctx context.Context) error {
			var resp map[string]string
			return clientBus.Call(ctx, "nc90", "Grant", map[string]string{}, &resp)
		}},
		{"CallTo", func(ctx context.Context) error {
			var resp map[string]string
			return clientBus.CallTo(ctx, "nc90", 9921, "Grant", map[string]string{}, &resp)
		}},
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		err := call.do(ctx)
		cancel()
		t.Logf("lightweight %s into the JetStream deployment: %v", call.name, err)
		if !errors.Is(err, bus.ErrRPCCapturedByJetStream) {
			t.Errorf("%s = %v, want bus.ErrRPCCapturedByJetStream", call.name, err)
		}
	}
	time.Sleep(1500 * time.Millisecond) // longer than AckWait
	nc, err := gonats.Connect(url, gonats.Timeout(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	js, _ := gojs.New(nc)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pending := 0
	for _, name := range []string{"rpc_nc90_Grant", "rpc_nc90_9921_Grant"} {
		consumer, err := js.Consumer(ctx, reqStream, name)
		if err != nil {
			t.Fatal(err)
		}
		info, err := consumer.Info(ctx)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("%s: delivered=%d ack_pending=%d", name, info.Delivered.Consumer, info.NumAckPending)
		pending += info.NumAckPending
	}
	if runs.Load() != 0 || pending != 0 {
		t.Fatalf("captured lightweight requests: handler runs=%d ack_pending=%d, want 0 / 0", runs.Load(), pending)
	}
}
