//go:build integration

package nats

import (
	"context"
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/infra/network/bus"
)

// RR-20261006-70（F05-3）：JetStream RPC 的 handler 跑得比 AckWait 久时不能被重复执行。
//
// 修前 handler 的期限取调用方截止（可以远长于 nats.rpc.ack_wait），消费回调执行期间不发 in-progress ack，
// 服务级 durable 在实例之间共享：AckWait 一到 broker 把未确认的请求重投给另一个实例（或同一实例排队），
// 两次执行并发，服务端没有按 MsgID 去重。承诺：同一个请求只执行一次。真实 NATS（scripts/mirror-local.sh）。
func TestRealJetStreamRPCLongerThanAckWaitRunsOnce(t *testing.T) {
	url := os.Getenv("ROOST_DATAENGINE_IT_NATS_URL")
	if url == "" {
		t.Skip("ROOST_DATAENGINE_IT_NATS_URL is not set; run against a private environment (scripts/mirror-local.sh)")
	}
	suffix := fmt.Sprintf("%d_%d", os.Getpid(), time.Now().UnixNano())
	prefix, reqStream, respStream := "rr69"+suffix, "RR_69_REQ_"+suffix, "RR_69_RESP_"+suffix
	t.Cleanup(func() { deleteStreamsOn(t, url, reqStream, respStream) })
	// 两个服务实例共享服务级 durable（ack_wait 1s，见 startJetStreamRPCMod）。
	_, first := startJetStreamRPCMod(t, url, prefix, reqStream, respStream, 6901)
	_, second := startJetStreamRPCMod(t, url, prefix, reqStream, respStream, 6902)
	_, client := startJetStreamRPCMod(t, url, prefix, reqStream, respStream, 6903)
	var runs atomic.Int32
	handler := func(*bus.RpcContext) (any, error) {
		runs.Add(1)
		time.Sleep(2500 * time.Millisecond) // 超过 AckWait（1s）两倍多
		return map[string]string{"ok": "1"}, nil
	}
	for _, b := range []*bus.Bus{first, second} {
		if err := b.HandleRpc("Slow69", handler); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	var resp map[string]string
	err := client.CallReliable(ctx, "nc90", "Slow69", map[string]string{}, &resp)
	t.Logf("call: err=%v resp=%v runs=%d", err, resp, runs.Load())
	// 等过可能的重投都跑完。
	time.Sleep(4 * time.Second)
	if got := runs.Load(); got != 1 {
		t.Fatalf("a JetStream RPC whose handler outlived AckWait ran %d times, want exactly once (call err=%v)", got, err)
	}
	if err != nil {
		t.Fatalf("call failed: %v", err)
	}
}
