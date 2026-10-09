//go:build integration

// RR-20261006-10 的真实依赖验收（APP-7，真实进程演练第 4 项，docs/bugfix/REAL-PROCESS-DRILLS-2026-10-06.md）：
// 修复时 kit NatsMod 的 Close 口径只用不可达地址 / 无连接替身验证过。这里经正式的 Init → Provide →
// Start 接真实 NATS（JetStream 集群），钉住：并发 Stop 无竞争（-race）且全部返回 nil；重复 Stop 返回
// nil；Stop 之后经 Mod 发布的能力（IClient / IRpc / IJetStream / IBus）的调用返回可 errors.Is 到
// fnats.ErrClosed 的错误；连接未排空就被硬关时 ErrClosedUndrained 只报给那一次 Stop。
// 订阅、JetStream 与 RPC CallAsync 这几条在真实 NATS 上首跑为红，登记并修复为 RR-20261006-24。
package nats

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/spf13/viper"

	"github.com/tjbdwanghaibo/roost-core/framework/app"
	"github.com/tjbdwanghaibo/roost-core/infra/network/bus"
	"github.com/tjbdwanghaibo/roost-core/wiring/mods"
	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
	natsdriver "github.com/tjbdwanghaibo/roost-core/infra/network/nats/driver"
)

func realNatsURL(t *testing.T) string {
	t.Helper()
	url := os.Getenv("ROOST_DATAENGINE_IT_NATS_URL")
	if url == "" {
		t.Skip("ROOST_DATAENGINE_IT_NATS_URL is not set; run against an isolated environment (scripts/mirror-local.sh or kit/scripts/integration/dataengine-env.sh)")
	}
	return url
}

// startRealNatsMod 走正式装配：Init 读配置、Provide 真实连接并登记能力、Start 启动 bus 订阅。
func startRealNatsMod(t *testing.T) (*NatsMod, *app.Registry) {
	t.Helper()
	cfg := viper.New()
	cfg.Set("nats.url", realNatsURL(t))
	cfg.Set("nats.prefix", fmt.Sprintf("rr1006_10_%d", time.Now().UnixNano()))
	cfg.Set("sid", 1)
	cfg.Set("server_type", "game")
	m := NewNatsMod(nil)
	if err := m.Init(cfg); err != nil {
		t.Fatal(err)
	}
	r := app.NewRegistry(cfg)
	if err := m.Provide(r); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.StopWithContext(context.Background()) })
	if err := m.Start(); err != nil {
		t.Fatal(err)
	}
	return m, r
}

func TestRealNatsModCloseContract(t *testing.T) {
	m, r := startRealNatsMod(t)
	client, _ := app.Lookup[fnats.IClient](r, mods.ModNats)
	rpc, _ := app.Lookup[fnats.IRpc](r, mods.ModNatsRpc)
	js, _ := app.Lookup[fnats.IJetStream](r, mods.ModNatsJetStream)
	b, _ := app.Lookup[bus.IBus](r, mods.ModBus)
	if client == nil || rpc == nil || js == nil || b == nil {
		t.Fatalf("capabilities not published: client=%v rpc=%v jetstream=%v bus=%v", client, rpc, js, b)
	}
	subject := fmt.Sprintf("rr1006_10.real.%d", time.Now().UnixNano())
	if err := client.Publish(subject, []byte("before")); err != nil {
		t.Fatalf("Publish before Stop = %v; the connection is not usable, not a real-dependency run", err)
	}

	// 并发 Stop：-race 下无竞争，全部 nil（后到者等第一个做完）。
	var wg sync.WaitGroup
	errs := make([]error, 4)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			errs[i] = m.StopWithContext(ctx)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent Stop #%d = %v, want nil (all = %v)", i, err, errs)
		}
	}
	m.mu.Lock()
	retained := m.asm
	m.mu.Unlock()
	if retained != nil {
		t.Fatal("assembly still held after every Stop returned nil")
	}
	// 重复 Stop 返回 nil。
	for i := 0; i < 2; i++ {
		if err := m.StopWithContext(context.Background()); err != nil {
			t.Fatalf("Stop #%d after the Mod stopped = %v, want nil", i+2, err)
		}
	}

	// Stop 之后经 Mod 发布的能力：每个调用都要快速返回可 errors.Is 到 fnats.ErrClosed 的错误。
	type call struct {
		name string
		do   func() error
	}
	calls := []call{
		{"IClient.Publish", func() error { return client.Publish(subject, []byte("after")) }},
		{"IClient.Request", func() error { _, err := client.Request(subject, []byte("after"), time.Second); return err }},
		{"IClient.Subscribe", func() error {
			sub, err := client.Subscribe(subject, func(*fnats.Msg) {})
			if sub != nil {
				_ = sub.Unsubscribe()
			}
			return err
		}},
		{"IClient.QueueSubscribe", func() error {
			sub, err := client.QueueSubscribe(subject, "q", func(*fnats.Msg) {})
			if sub != nil {
				_ = sub.Unsubscribe()
			}
			return err
		}},
		{"IRpc.Call", func() error {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			_, err := rpc.Call(ctx, subject, []byte("after"))
			return err
		}},
		{"IRpc.CallAsync", func() error {
			done := make(chan error, 1)
			rpc.CallAsync(subject, []byte("after"), func(_ []byte, err error) { done <- err })
			select {
			case err := <-done:
				return err
			case <-time.After(time.Second):
				return errors.New("CallAsync callback did not run within 1s")
			}
		}},
		{"IJetStream.EnsureStream", func() error {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			return js.EnsureStream(ctx, fnats.JetStreamConfig{Name: "RR1006_10_CLOSED", Subjects: []string{subject + ".js"}})
		}},
		{"IJetStream.Publish", func() error {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			_, err := js.Publish(ctx, subject, []byte("after"), fnats.JetStreamPublishOptions{})
			return err
		}},
		{"IJetStream.Subscribe", func() error {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			sub, err := js.Subscribe(ctx, fnats.JetStreamConsumerConfig{Stream: "RR1006_10_CLOSED", Durable: "closed"},
				func(context.Context, *fnats.JetStreamMsg) error { return nil })
			if sub != nil {
				sub.Stop()
			}
			return err
		}},
		{"IBus.Send", func() error { return b.Send(1, "close", &fnats.NatsMsg{MsgName: "after"}) }},
		{"IBus.Call", func() error {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			return b.Call(ctx, "game", "close", &fnats.NatsMsg{MsgName: "after"}, &fnats.NatsMsg{})
		}},
	}
	for _, c := range calls {
		started := time.Now()
		err := c.do()
		elapsed := time.Since(started)
		t.Logf("%s after Stop = %v (%s)", c.name, err, elapsed.Round(time.Millisecond))
		if !errors.Is(err, fnats.ErrClosed) {
			t.Errorf("%s after Stop = %v, want an error that errors.Is fnats.ErrClosed", c.name, err)
		}
		if elapsed > 500*time.Millisecond {
			t.Errorf("%s after Stop took %s; a closed connection must fail fast", c.name, elapsed)
		}
	}
}

// 连接排空被预算截断：Stop 第一次报 ErrClosedUndrained（仍可 errors.Is 到 ctx 错误）并释放引用；
// 之后的 Stop、以及对同一个 Assembly 的 Close 都返回 nil——终态错误只报一次。
func TestRealNatsModUndrainedCloseIsReportedOnce(t *testing.T) {
	m, r := startRealNatsMod(t)
	client, _ := app.Lookup[fnats.IClient](r, mods.ModNats)
	m.mu.Lock()
	asm := m.asm
	m.mu.Unlock()

	entered, release := make(chan struct{}), make(chan struct{})
	var enterOnce, releaseOnce sync.Once
	releaseHandler := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseHandler)
	// 不经过 Bus 的直连订阅：handler 卡住时连接排空只能等它或被预算截断（同 RR-20261004-08 用例）。
	subject := fmt.Sprintf("rr1006_10.undrained.%d", time.Now().UnixNano())
	if _, err := client.Subscribe(subject, func(*fnats.Msg) {
		enterOnce.Do(func() { close(entered) })
		<-release
	}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := client.Publish(subject, []byte("x")); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("subscription handler never entered; not a counterexample")
	}

	budget, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	first := stopModWithin(t, m, budget, "budgeted stop")
	t.Logf("first Stop = %v", first)
	if !errors.Is(first, natsdriver.ErrClosedUndrained) || !errors.Is(first, context.DeadlineExceeded) {
		t.Fatalf("first Stop = %v, want ErrClosedUndrained wrapping context.DeadlineExceeded", first)
	}
	// RR-20261006-26：硬关之后连接一直是关的。nats.go 的排空协程在硬关后还会把状态翻回 DRAINING_PUBS、
	// 再空等一次 Flush（最长 5s）才再关一次；这段时间里 IsConnected 为 true。看 300ms，覆盖它约 10ms 的轮询。
	for deadline := time.Now().Add(300 * time.Millisecond); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if asm.Connected() {
			t.Fatalf("Assembly.Connected() = true after the undrained close returned; the connection was closed hard")
		}
	}
	releaseHandler()

	var wg sync.WaitGroup
	again := make([]error, 3)
	for i := range again {
		wg.Add(1)
		go func(i int) { defer wg.Done(); again[i] = stopModWithin(t, m, context.Background(), "repeated stop") }(i)
	}
	wg.Wait()
	for i, err := range again {
		if err != nil {
			t.Fatalf("Stop #%d after the undrained close = %v, want nil: ErrClosedUndrained is reported once (all = %v)", i+2, err, again)
		}
	}
	if err := asm.Close(context.Background()); err != nil {
		t.Fatalf("Assembly.Close after the undrained close = %v, want nil", err)
	}
	if err := client.Publish(subject, []byte("after")); !errors.Is(err, fnats.ErrClosed) {
		t.Fatalf("Publish after the undrained close = %v, want fnats.ErrClosed", err)
	}
}
