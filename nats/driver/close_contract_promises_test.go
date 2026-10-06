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

// RR-20261006-24：Close 之后的订阅、JetStream 与 RPC 调用同样返回可 errors.Is 到 fnats.ErrClosed 的错误
// （RR-20261006-10 统一口径第 3 条）。旧行为（真实进程演练第 4 项在真实 NATS 上发现）：Publish / Request /
// RPC.Call 已映射到 fnats.ErrClosed，而 Subscribe / QueueSubscribe、JetStream 的 EnsureStream / Publish /
// Subscribe 与 RPC.CallAsync 的订阅收件箱原样返回 nats.go 的 ErrConnectionClosed，文本同为
// "nats: connection closed"，却 errors.Is 不到驱动的已关闭错误。
func TestClientSubscribeAndJetStreamAfterCloseReportErrClosed(t *testing.T) {
	a := closeContractAssembly(t)
	js, err := NewJetStreamClient(a.Client)
	if err != nil {
		t.Fatal(err)
	}
	a.Client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	calls := map[string]func() error{
		"Subscribe": func() error {
			_, err := a.Client.Subscribe("roost.close", func(*fnats.Msg) {})
			return err
		},
		"QueueSubscribe": func() error {
			_, err := a.Client.QueueSubscribe("roost.close", "q", func(*fnats.Msg) {})
			return err
		},
		"JetStream.EnsureStream": func() error {
			return js.EnsureStream(ctx, fnats.JetStreamConfig{Name: "ROOST_CLOSE", Subjects: []string{"roost.close.js"}})
		},
		"JetStream.Publish": func() error {
			_, err := js.Publish(ctx, "roost.close.js", nil, fnats.JetStreamPublishOptions{})
			return err
		},
		"JetStream.Subscribe": func() error {
			_, err := js.Subscribe(ctx, fnats.JetStreamConsumerConfig{Stream: "ROOST_CLOSE", Durable: "close"}, func(context.Context, *fnats.JetStreamMsg) error { return nil })
			return err
		},
	}
	for name, call := range calls {
		if err := call(); !errors.Is(err, fnats.ErrClosed) {
			t.Errorf("%s after Close = %v, want an error that errors.Is fnats.ErrClosed", name, err)
		}
	}
	// 连接关了、RPC 客户端没停（只关 Client 的调用方）：CallAsync 订阅收件箱失败。
	if err := callAsyncResult(a.RPC); !errors.Is(err, fnats.ErrClosed) {
		t.Errorf("RPC.CallAsync after Client.Close = %v, want an error that errors.Is fnats.ErrClosed", err)
	}
}

// RR-20261006-24：Assembly.Close 停了 RPC 客户端之后发起的 CallAsync 同样是“已关闭”。旧行为只回
// fnats.ErrCancelled（与 Close 时在途调用的取消同一个错误）；现在同时 errors.Is 到 ErrCancelled 与
// ErrClosed，按 ErrCancelled 判断的调用方不受影响。
func TestRPCCallAsyncAfterAssemblyCloseReportsErrClosed(t *testing.T) {
	a := closeContractAssembly(t)
	_ = a.Close(context.Background()) // 不可达地址：ErrClosedUndrained，见 TestAssemblyTerminalCloseErrorIsReportedOnce
	err := callAsyncResult(a.RPC)
	if !errors.Is(err, fnats.ErrClosed) || !errors.Is(err, fnats.ErrCancelled) {
		t.Fatalf("RPC.CallAsync after Assembly.Close = %v, want an error that errors.Is both fnats.ErrClosed and fnats.ErrCancelled", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := a.RPC.Call(ctx, "roost.close", nil); !errors.Is(err, fnats.ErrClosed) {
		t.Fatalf("RPC.Call after Assembly.Close = %v, want fnats.ErrClosed", err)
	}
}

// callAsyncResult 发起一次 CallAsync，返回回调拿到的错误；回调 1s 内不来按失败处理。
func callAsyncResult(rpc *RPCClient) error {
	done := make(chan error, 1)
	rpc.CallAsync("roost.close", nil, func(_ []byte, err error) { done <- err })
	select {
	case err := <-done:
		return err
	case <-time.After(time.Second):
		return errors.New("CallAsync callback did not run within 1s")
	}
}
