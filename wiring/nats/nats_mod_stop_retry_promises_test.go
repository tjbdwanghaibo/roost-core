// RR-20261004-07：NatsMod 在 Bus 停止超出预算时保留 bus / asm 供再次停止，这个承诺
// 只有在“再次停止能继续等同一次排空并最终关闭 Assembly”时才成立。旧实现里
// Bus.StopWithContext 第一次就丢掉 pool 并把超时错误缓存成终态，之后每次重试都返回
// 同一个 DeadlineExceeded，Assembly（连接、RPC 回调池）永不关闭；Bus 退订失败这类
// 终态错误同样被当成“稍后重试”，重试永远拿到同一个错误。
package nats

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/infra/network/bus"
	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
	natsdriver "github.com/tjbdwanghaibo/roost-core/infra/network/nats/driver"
)

type stopRetrySub struct {
	mu    sync.Mutex
	valid bool
	err   error
}

func (s *stopRetrySub) Unsubscribe() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.valid = false
	return s.err
}

func (s *stopRetrySub) IsValid() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.valid
}

// stopRetryClient loops Publish back into the subscribed handler so a real Bus
// dispatches into its real worker pool.
type stopRetryClient struct {
	mu          sync.Mutex
	subs        map[string]fnats.MsgHandler
	unsubscribe error
}

func (c *stopRetryClient) Publish(subject string, data []byte) error {
	c.mu.Lock()
	h := c.subs[subject]
	c.mu.Unlock()
	if h != nil {
		h(&fnats.Msg{Subject: subject, Data: data})
	}
	return nil
}

func (c *stopRetryClient) Request(string, []byte, time.Duration) ([]byte, error) { return nil, nil }

func (c *stopRetryClient) Subscribe(subject string, h fnats.MsgHandler) (fnats.ISubscription, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.subs == nil {
		c.subs = map[string]fnats.MsgHandler{}
	}
	c.subs[subject] = h
	return &stopRetrySub{valid: true, err: c.unsubscribe}, nil
}

func (c *stopRetryClient) QueueSubscribe(subject, _ string, h fnats.MsgHandler) (fnats.ISubscription, error) {
	return c.Subscribe(subject, h)
}

func (c *stopRetryClient) Drain() error { return nil }
func (c *stopRetryClient) Close()       {}

type stopRetryPing struct{}

// stopModWithin runs one NatsMod stop on its own goroutine so a stop that
// never returns fails the test with a readable assertion instead of hanging
// until the go test -timeout.
func stopModWithin(t *testing.T, m *NatsMod, ctx context.Context, what string) error {
	t.Helper()
	result := make(chan error, 1)
	go func() { result <- m.StopWithContext(ctx) }()
	select {
	case err := <-result:
		return err
	case <-time.After(2 * time.Second):
		t.Fatalf("%s did not return within 2s", what)
		return nil
	}
}

func TestNatsModStopRetryAfterBusBudgetClosesAssembly(t *testing.T) {
	client := &stopRetryClient{}
	rpc := natsdriver.NewRPCClient(nil, fnats.DefaultRetryPolicy(), 1)
	b := bus.New(client, rpc, nil, bus.Config{Sid: 1, SvcType: "game", WorkerNum: 1, QueueCap: 4})
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	if err := b.Handle("m", "*nats.stopRetryPing", func(*bus.MsgContext) { close(entered); <-release }); err != nil {
		t.Fatal(err)
	}
	if err := b.Start(); err != nil {
		t.Fatal(err)
	}
	if err := b.Send(1, "m", &stopRetryPing{}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("bus handler never entered; not a counterexample")
	}
	m := &NatsMod{bus: b, asm: &natsdriver.Assembly{RPC: rpc}}

	budget, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	first := stopModWithin(t, m, budget, "budgeted stop")
	t.Logf("first_stop_err=%v bus_retained=%v asm_retained=%v", first, m.bus != nil, m.asm != nil)
	if !errors.Is(first, context.DeadlineExceeded) || m.bus == nil || m.asm == nil {
		t.Fatal("a stop that ran out of budget must report it and retain bus and assembly for a later stop")
	}

	once.Do(func() { close(release) }) // 业务 handler 真实退出
	retry := stopModWithin(t, m, context.Background(), "retry stop")
	t.Logf("retry_after_handler_exit_err=%v asm_retained=%v", retry, m.asm != nil)
	if retry != nil || m.bus != nil || m.asm != nil {
		t.Fatal("retry after the blocking handler exited never closes the retained Assembly")
	}
	if again := stopModWithin(t, m, context.Background(), "repeated stop"); again != nil {
		t.Fatalf("stop after success = %v", again)
	}
}

func TestNatsModStopClosesAssemblyAfterTerminalBusError(t *testing.T) {
	unsubscribe := errors.New("unsubscribe refused")
	client := &stopRetryClient{unsubscribe: unsubscribe}
	rpc := natsdriver.NewRPCClient(nil, fnats.DefaultRetryPolicy(), 1)
	b := bus.New(client, rpc, nil, bus.Config{Sid: 1, SvcType: "game", WorkerNum: 1, QueueCap: 4})
	if err := b.Start(); err != nil {
		t.Fatal(err)
	}
	m := &NatsMod{bus: b, asm: &natsdriver.Assembly{RPC: rpc}}

	first := stopModWithin(t, m, context.Background(), "stop")
	t.Logf("first_stop_err=%v asm_retained=%v", first, m.asm != nil)
	if !errors.Is(first, unsubscribe) {
		t.Fatalf("stop lost the bus teardown error: %v", first)
	}
	if m.asm != nil {
		retry := stopModWithin(t, m, context.Background(), "retry stop")
		t.Logf("retry_err=%v asm_retained=%v", retry, m.asm != nil)
		t.Fatal("a terminal bus error kept the Assembly retained although no later stop can make progress")
	}
	if m.bus != nil {
		t.Fatal("bus reference retained after its stop became terminal")
	}
}
