package driver

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	gonats "github.com/nats-io/nats.go"
	fnats "github.com/tjbdwanghaibo/roost-core/nats"
)

// RR-20261006-10（W-2026-10-06-02 转 RR）：驱动 Close 的统一口径——重复 Close 幂等返回 nil，第一次的
// 错误只报一次；并发 Close 的后到者等第一个做完；Close 之后的其他调用返回已关闭错误（fnats.ErrClosed）。
//
// Assembly.Close 第一次排空失败时硬关连接并返回 ErrClosedUndrained（RR-20261004-08 有意的终态错误，
// 保留）。旧行为：之后每次 Close 都再去排空已关闭的连接，每次都返回 ErrClosedUndrained；并发 Close 的
// 后到者在第一个排空的同时再排空一次、失败就硬关连接。Client.Publish 在连接关闭后空等 3 × 20ms 重试，
// 返回的错误不能 errors.Is 到 fnats.ErrClosed（Request 能）。
//
// 连接指向不可达地址并开启 RetryOnFailedConnect：连接处于重连中，排空必然失败，不需要真实 NATS。
func closeContractAssembly(t *testing.T) *Assembly {
	t.Helper()
	conn, err := gonats.Connect("nats://127.0.0.1:1",
		gonats.RetryOnFailedConnect(true),
		gonats.ReconnectWait(50*time.Millisecond),
		gonats.Timeout(100*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	c := &Client{conn: conn, cfg: &fnats.Config{}, state: &natsLifecycleState{}}
	return &Assembly{Client: c, RPC: NewRPCClient(c, fnats.DefaultRetryPolicy(), 1)}
}

func TestAssemblyTerminalCloseErrorIsReportedOnce(t *testing.T) {
	a := closeContractAssembly(t)
	ctx := context.Background()
	if err := a.Close(ctx); !errors.Is(err, ErrClosedUndrained) {
		t.Fatalf("first Close on a reconnecting connection = %v, want ErrClosedUndrained", err)
	}
	for attempt := 2; attempt <= 3; attempt++ {
		if err := a.Close(ctx); err != nil {
			t.Fatalf("Close #%d = %v, want nil: the connection is already closed, the terminal error is reported once", attempt, err)
		}
	}
}

func TestAssemblyConcurrentCloseReportsTheTerminalErrorOnce(t *testing.T) {
	for round := 0; round < 5; round++ {
		a := closeContractAssembly(t)
		var wg sync.WaitGroup
		errs := make([]error, 4)
		for i := range errs {
			wg.Add(1)
			go func(i int) { defer wg.Done(); errs[i] = a.Close(context.Background()) }(i)
		}
		wg.Wait()
		failed := 0
		for _, err := range errs {
			if err == nil {
				continue
			}
			failed++
			if !errors.Is(err, ErrClosedUndrained) {
				t.Fatalf("round %d: concurrent Close = %v, want nil or ErrClosedUndrained (all = %v)", round, err, errs)
			}
		}
		if failed != 1 {
			t.Fatalf("round %d: %d concurrent Close calls reported the terminal error, want exactly 1 (all = %v)", round, failed, errs)
		}
	}
}

func TestClientPublishAfterCloseReportsErrClosed(t *testing.T) {
	a := closeContractAssembly(t)
	a.Client.Close()
	if err := a.Client.Publish("roost.close", nil); !errors.Is(err, fnats.ErrClosed) {
		t.Fatalf("Publish after Close = %v, want fnats.ErrClosed", err)
	}
}
