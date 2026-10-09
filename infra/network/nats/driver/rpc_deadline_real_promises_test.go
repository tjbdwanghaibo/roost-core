//go:build integration

package driver

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
)

// RR-20261006-73（F09-R2）：轻量传输的 RPC 按调用方期限计时，超时 / 取消带 ctx 语义。
//
// 修前 RPCClient.Call 每次尝试固定 context.WithTimeout(ctx, 5s)，缺省 MaxAttempts = 1：调用方的
// call_timeout 大于 5s 时（例如 8s），处理 6s 的服务端永远等不到，5s 就返回 fnats.ErrTimeout；超时错误
// 不满足 errors.Is(err, context.DeadlineExceeded)，取消不满足 errors.Is(err, context.Canceled)。
// 承诺：调用方带期限时就按它计时（没有期限才用 5s 的单次上限）；超时同时 errors.Is 到 fnats.ErrTimeout 与
// context.DeadlineExceeded，取消同时 errors.Is 到 fnats.ErrCancelled 与 context.Canceled。需要
// ROOST_DATAENGINE_IT_NATS_URL（scripts/mirror-local.sh 的私有环境）。
func TestRealNatsRPCHonoursTheCallerDeadlineBeyondFiveSeconds(t *testing.T) {
	url := os.Getenv("ROOST_DATAENGINE_IT_NATS_URL")
	if url == "" {
		t.Skip("ROOST_DATAENGINE_IT_NATS_URL is not set; run against a private environment (scripts/mirror-local.sh)")
	}
	a, err := Assemble(fnats.DefaultConfig(url), ClientOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close(context.Background()) })
	prefix := fmt.Sprintf("rpcdeadline.%d", time.Now().UnixNano())
	sub, err := a.Client.Subscribe(prefix+".slow", func(msg *fnats.Msg) {
		time.Sleep(6 * time.Second)
		_ = a.Client.Publish(msg.Reply, []byte("late-but-in-budget"))
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	silent, err := a.Client.Subscribe(prefix+".silent", func(*fnats.Msg) {})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = silent.Unsubscribe() })

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	started := time.Now()
	resp, err := a.RPC.Call(ctx, prefix+".slow", []byte("x"))
	cancel()
	if err != nil {
		t.Fatalf("a 6s handler under an 8s caller deadline failed after %v: %v", time.Since(started).Round(time.Millisecond), err)
	}
	if string(resp) != "late-but-in-budget" {
		t.Fatalf("resp = %q", resp)
	}

	ctx, cancel = context.WithTimeout(context.Background(), 300*time.Millisecond)
	_, err = a.RPC.Call(ctx, prefix+".silent", []byte("x"))
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, fnats.ErrTimeout) {
		t.Fatalf("timeout = %v; want errors.Is to both context.DeadlineExceeded and fnats.ErrTimeout", err)
	}

	ctx, cancel = context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	_, err = a.RPC.Call(ctx, prefix+".silent", []byte("x"))
	if !errors.Is(err, context.Canceled) || !errors.Is(err, fnats.ErrCancelled) {
		t.Fatalf("cancel = %v; want errors.Is to both context.Canceled and fnats.ErrCancelled", err)
	}
}
