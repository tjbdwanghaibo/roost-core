// RR-20261004-08（W-2026-10-04-02）：连接 drain 超出停止预算时，Assembly.Close
// 已经硬关闭连接并返回 ctx 错误；NatsMod 旧实现仍保留 m.asm，重试时
// Client.DrainWithContext 对已关闭的连接拿到 nats.go 的 ErrConnectionClosed，
// 重试永远失败、引用永不置空。Assembly.Client 是具体的 *driver.Client，IClient
// 替身进不了这条路径，所以这里用一个只会握手、应答 PING、按需投递 MSG 的
// 进程内 NATS 协议桩让 drain 确定性地等住；真实 NATS 上的同一场景见
// nats_mod_drain_budget_real_promises_test.go（-tags integration）。
package nats

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	fnats "github.com/tjbdwanghaibo/roost-core/nats"
	natsdriver "github.com/tjbdwanghaibo/roost-core/nats/driver"
)

// stubNatsServer speaks just enough of the NATS client protocol for one
// connection: INFO on accept, PONG for every PING, and MSG on demand for the
// subscriptions the client registered.
type stubNatsServer struct {
	ln   net.Listener
	mu   sync.Mutex
	conn net.Conn
	sids map[string]string // subject -> sid
	subs chan struct{}     // signalled on every SUB
}

func startStubNatsServer(t *testing.T) *stubNatsServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &stubNatsServer{ln: ln, sids: map[string]string{}, subs: make(chan struct{}, 16)}
	t.Cleanup(func() {
		_ = ln.Close()
		s.mu.Lock()
		if s.conn != nil {
			_ = s.conn.Close()
		}
		s.mu.Unlock()
	})
	go s.serve()
	return s
}

func (s *stubNatsServer) url() string { return "nats://" + s.ln.Addr().String() }

func (s *stubNatsServer) serve() {
	conn, err := s.ln.Accept()
	if err != nil {
		return
	}
	s.mu.Lock()
	s.conn = conn
	s.mu.Unlock()
	addr := s.ln.Addr().(*net.TCPAddr)
	s.write(fmt.Sprintf(`INFO {"server_id":"STUB","server_name":"stub","version":"2.10.0","proto":1,"host":"127.0.0.1","port":%d,"max_payload":1048576}`+"\r\n", addr.Port))
	r := bufio.NewReader(conn)
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		switch strings.ToUpper(fields[0]) {
		case "PING":
			s.write("PONG\r\n")
		case "SUB":
			if len(fields) >= 3 {
				s.mu.Lock()
				s.sids[fields[1]] = fields[len(fields)-1]
				s.mu.Unlock()
				s.subs <- struct{}{}
			}
		}
	}
}

func (s *stubNatsServer) write(frame string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conn != nil {
		_, _ = s.conn.Write([]byte(frame))
	}
}

func (s *stubNatsServer) deliver(t *testing.T, subject string, n int) {
	t.Helper()
	s.mu.Lock()
	sid := s.sids[subject]
	s.mu.Unlock()
	if sid == "" {
		t.Fatalf("no subscription registered for %s", subject)
	}
	for i := 0; i < n; i++ {
		s.write(fmt.Sprintf("MSG %s %s 1\r\nx\r\n", subject, sid))
	}
}

func TestNatsModStopAfterConnectionDrainBudgetConverges(t *testing.T) {
	srv := startStubNatsServer(t)
	cfg := fnats.DefaultConfig(srv.url())
	cfg.MaxReconnects = 0
	asm, err := natsdriver.Assemble(cfg, natsdriver.ClientOptions{})
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

	const subject = "rr20261004_08.drain"
	if _, err := asm.Client.Subscribe(subject, func(*fnats.Msg) {
		enterOnce.Do(func() { close(entered) })
		<-release
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-srv.subs:
	case <-time.After(2 * time.Second):
		t.Fatal("stub server never saw the subscription")
	}
	// 第一条占住 handler，第二条留在订阅的待处理队列里，连接 drain 必须等它。
	srv.deliver(t, subject, 2)
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("subscription handler never entered; not a counterexample")
	}

	m := &NatsMod{asm: asm}
	budget, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
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

// 连接在停止前已经关闭（服务端断开且不重连）：drain 不可能再进行，停止要报告
// 终态并释放引用，而不是保留 asm 让每次重试都拿到 ErrConnectionClosed。
func TestNatsModStopReleasesAssemblyWhoseConnectionIsAlreadyClosed(t *testing.T) {
	srv := startStubNatsServer(t)
	cfg := fnats.DefaultConfig(srv.url())
	cfg.MaxReconnects = 0
	asm, err := natsdriver.Assemble(cfg, natsdriver.ClientOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(asm.Client.Close)
	_ = srv.ln.Close()
	srv.mu.Lock()
	_ = srv.conn.Close()
	srv.mu.Unlock()
	deadline := time.Now().Add(2 * time.Second)
	for asm.Connected() {
		if time.Now().After(deadline) {
			t.Fatal("client never noticed the server closing the connection")
		}
		time.Sleep(5 * time.Millisecond)
	}

	m := &NatsMod{asm: asm}
	first := stopModWithin(t, m, context.Background(), "stop")
	t.Logf("first_stop_err=%v asm_retained=%v", first, m.asm != nil)
	if first == nil {
		t.Fatal("stop hid that the connection could not be drained")
	}
	retry := stopModWithin(t, m, context.Background(), "retry stop")
	t.Logf("retry_err=%v asm_retained=%v", retry, m.asm != nil)
	if retry != nil || m.asm != nil {
		t.Fatal("stop of an already closed connection never converges")
	}
	if !errors.Is(first, natsdriver.ErrClosedUndrained) {
		t.Fatalf("closed connection is not reported as terminal: %v", first)
	}
}
