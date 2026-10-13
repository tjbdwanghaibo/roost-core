//go:build integration

package nats

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gonats "github.com/nats-io/nats.go"
	gojs "github.com/nats-io/nats.go/jetstream"
	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/framework/app"
	"github.com/tjbdwanghaibo/roost-core/infra/network/bus"
	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
	natsdriver "github.com/tjbdwanghaibo/roost-core/infra/network/nats/driver"
	maildomain "github.com/tjbdwanghaibo/roost-core/service/mail"
	"github.com/tjbdwanghaibo/roost-core/wiring/mail"
	"github.com/tjbdwanghaibo/roost-core/wiring/mods"
	kitmods "github.com/tjbdwanghaibo/roost-core/wiring/mods"
)

// summaryMail 只实现 Summary；其余方法这里用不到。
type summaryMail struct{ maildomain.Mail }

func (summaryMail) Summary(_ context.Context, playerID int64) (maildomain.Summary, error) {
	return maildomain.Summary{PlayerID: playerID, Unread: 3}, nil
}

// nats.rpc.transport=jetstream 的部署里，生成的 ClientMod 要能调用服务：ClientMod 按本进程的 nats.rpc.transport 选择传输，
// 两端都配 jetstream 时调用成功（不走被请求流截获的轻量 request-reply）。真实 NATS（scripts/mirror-local.sh）。
// 放在 wiring/nats：它是传输用例，跟随故障矩阵已列出的包（根包 TestFaultMatrixScriptNamesEveryFullEnvironmentSuite）。
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

// NatsMod 停机口径的真实依赖验收：经正式的 Init → Provide → Start 接真实 NATS（JetStream 集群），钉住并发 Stop 无竞争（-race）
// 且全部返回 nil；重复 Stop 返回 nil；Stop 之后经 Mod 发布的能力（IClient / IRpc / IJetStream / IBus）的调用返回可 errors.Is 到
// fnats.ErrClosed 的错误；连接未排空就被硬关时 ErrClosedUndrained 只报给那一次 Stop。

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

// 真实 NATS 上，服务端开 JetStream RPC、调用端用轻量 Call / CallTo：调用方拿到 bus.ErrRPCCapturedByJetStream，服务端不执行、
// 消息被确认（不能被请求流存下后无期限执行）。

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

// JetStream RPC 的 handler 跑得比 AckWait 久时不能被重复执行：同一个请求只执行一次。真实 NATS（scripts/mirror-local.sh）。
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

// 真实 NATS 上，JetStream RPC handler 在途时停止服务端 NatsMod：handler 未退出时停止返回预算的 ctx 错误并保留 bus / asm；
// handler 退出后重试停止返回 nil 并释放；回包送达调用方，不重投。

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

// 真实 NATS 上，连接 drain 超出停止预算时 Assembly.Close 已硬关连接并返回 ctx 错误：资源已经释放，之后的停止要能收敛——
// 返回 nil 且 m.asm 置空，不能每次重试都对已关闭的连接 Drain 而永远失败。

func TestRealNatsModStopAfterConnectionDrainBudgetConverges(t *testing.T) {
	url := os.Getenv("ROOST_DATAENGINE_IT_NATS_URL")
	if url == "" {
		t.Skip("ROOST_DATAENGINE_IT_NATS_URL is not set; run through scripts/integration/dataengine-env.sh")
	}
	asm, err := natsdriver.Assemble(fnats.DefaultConfig(url), natsdriver.ClientOptions{})
	if err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var enterOnce, releaseOnce sync.Once
	releaseHandler := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(func() {
		releaseHandler()
		asm.Client.Close()
	})

	// 不经过 Bus 的直连订阅：它的 handler 阻塞时，连接 drain 只能等它或被预算截断。
	subject := fmt.Sprintf("roost.it.rr20261004_08.%d", time.Now().UnixNano())
	if _, err := asm.Client.Subscribe(subject, func(*fnats.Msg) {
		enterOnce.Do(func() { close(entered) })
		<-release
	}); err != nil {
		t.Fatal(err)
	}
	// 两条：第一条占住 handler，第二条留在订阅的待处理队列里，drain 必须等它。
	for i := 0; i < 2; i++ {
		if err := asm.Client.Publish(subject, []byte("x")); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("subscription handler never entered; not a counterexample")
	}

	m := &NatsMod{asm: asm}
	budget, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	first := stopModWithin(t, m, budget, "budgeted stop")
	t.Logf("first_stop_err=%v asm_retained=%v connection_up=%v", first, m.asm != nil, asm.Connected())
	if !errors.Is(first, context.DeadlineExceeded) {
		t.Fatalf("a stop whose connection drain ran out of budget must report it: %v", first)
	}
	if asm.Connected() {
		t.Fatal("connection still up after the drain budget ran out; not the hard-close path")
	}

	releaseHandler()
	retry := stopModWithin(t, m, context.Background(), "retry stop")
	t.Logf("retry_err=%v asm_retained=%v", retry, m.asm != nil)
	if retry != nil || m.asm != nil {
		t.Fatal("stop after the connection was already closed hard never converges")
	}
	// 修后契约：硬关闭是可识别的终态，第一次停止就释放引用。
	if !errors.Is(first, natsdriver.ErrClosedUndrained) {
		t.Fatalf("hard close after the drain budget is not reported as terminal: %v", first)
	}
}

// Provide 先连 NATS，连上之后缺健康注册表要拒绝；开了可靠总线却没有 redis Mod 也要拒绝。natsdriver.Assemble 会真实连接，
// 所以这两条只能对集成环境的 NATS 验证。
func TestRealNatsModProvideRefusesBareRegistriesAndReliableWithoutRedis(t *testing.T) {
	url := os.Getenv("ROOST_DATAENGINE_IT_NATS_URL")
	if url == "" {
		t.Skip("ROOST_DATAENGINE_IT_NATS_URL is not set; run through scripts/integration/dataengine-env.sh")
	}
	ctx := context.Background()
	bare := viper.New()
	bare.Set("nats.url", url)
	mod := NewNatsMod(nil)
	if err := mod.Init(bare); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mod.StopWithContext(ctx) })
	if err := mod.Provide(&app.Registry{}); err == nil || !strings.Contains(err.Error(), "health registry not found") {
		t.Fatalf("Provide with a bare registry = %v", err)
	}

	reliable := viper.New()
	reliable.Set("nats.url", url)
	reliable.Set("nats.reliable.enabled", true)
	reliable.Set("sid", 1)
	reliable.Set("server_type", "game")
	other := NewNatsMod(nil)
	if err := other.Init(reliable); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = other.StopWithContext(ctx) })
	if err := other.Provide(app.NewRegistry(reliable)); err == nil || !strings.Contains(err.Error(), "nats reliable bus requires redis mod") {
		t.Fatalf("Provide with reliable enabled and no redis = %v", err)
	}
}
