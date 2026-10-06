package session

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/robot/protocol"
	"github.com/tjbdwanghaibo/roost-core/robot/transport"
)

// scriptedConn 是可控的 transport.Conn：读来自 inbound 通道，写可以被 blockWrites 卡住，记录写出的包。
type scriptedConn struct {
	inbound chan *transport.Packet

	mu          sync.Mutex
	written     []*transport.Packet
	blockWrites chan struct{} // 非 nil 时写阻塞到它被关闭（或连接关闭）
	writeEnter  chan struct{} // 每次写开始时发一个信号（有缓冲）

	closeOnce sync.Once
	closed    chan struct{}
}

func newScriptedConn() *scriptedConn {
	return &scriptedConn{
		inbound:    make(chan *transport.Packet, 16),
		writeEnter: make(chan struct{}, 16),
		closed:     make(chan struct{}),
	}
}

func (c *scriptedConn) ReadPacket() (*transport.Packet, error) {
	select {
	case p := <-c.inbound:
		return p, nil
	case <-c.closed:
		return nil, errors.New("scripted conn closed")
	}
}

func (c *scriptedConn) WritePackets(packets []*transport.Packet) error {
	c.writeEnter <- struct{}{}
	c.mu.Lock()
	block := c.blockWrites
	c.mu.Unlock()
	if block != nil {
		select {
		case <-block:
		case <-c.closed:
			return errors.New("scripted conn closed")
		}
	}
	c.mu.Lock()
	c.written = append(c.written, packets...)
	c.mu.Unlock()
	return nil
}

func (c *scriptedConn) Close() error {
	c.closeOnce.Do(func() { close(c.closed) })
	return nil
}

func (c *scriptedConn) RemoteAddr() string { return "scripted" }

func (c *scriptedConn) lastWritten(t *testing.T) *transport.Packet {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.written) == 0 {
		t.Fatal("nothing was written")
	}
	return c.written[len(c.written)-1]
}

type echoMsg struct {
	Text string `json:"text"`
}

const msgEcho = 11

func newScriptedSession(t *testing.T) (*Session, *scriptedConn) {
	t.Helper()
	protocols := protocol.NewRegistry(protocol.JSONCodec{})
	if err := protocol.RegisterMessage[echoMsg](protocols, msgEcho); err != nil {
		t.Fatal(err)
	}
	conn := newScriptedConn()
	s := New(conn, protocols)
	t.Cleanup(func() { _ = s.Close() })
	return s, conn
}

// RR-20261005-NC-261（N12 观察 O8 前半）：Call 受调用方 ctx 约束，包括发送阶段。
// 旧行为：Call 同步调用 conn.WritePackets，TCP 发送缓冲满（服务端不读）时写一直阻塞，Call 越过自己的超时，
// 直到会话被关闭才返回；Notify 已经用 sendWithContext 受 ctx 约束，Call 没有。
func TestCallReturnsWhenTheWriteBlocksPastItsContext(t *testing.T) {
	s, conn := newScriptedSession(t)
	release := make(chan struct{})
	conn.mu.Lock()
	conn.blockWrites = release
	conn.mu.Unlock()
	defer close(release)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := s.Call(ctx, msgEcho, msgEcho, &echoMsg{Text: "ping"})
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Call with a blocked write = %v, want context.DeadlineExceeded", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Call ignored its context while the write was blocked (still waiting 2s after a 50ms deadline)")
	}
	s.mu.Lock()
	pending := len(s.pending)
	s.mu.Unlock()
	if pending != 0 {
		t.Fatalf("pending calls after the timeout = %d, want 0", pending)
	}
}

// RR-20261005-NC-262（N12 观察 O8 后半）：超时之后才到的应答（seq 非 0、已无等待者）丢弃，不当成推送。
// 传输层约定 seq 0 才是推送（transport.Packet 注释）。旧行为：dispatch 找不到 pending 就落到推送分发，
// 同 msg id 的 WaitPush / 推送 handler 收到一条旧应答；请求与应答共用 msg id 的协议里把旧应答当成了推送。
func TestALateResponseIsNotDeliveredAsAPush(t *testing.T) {
	s, conn := newScriptedSession(t)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := s.Call(ctx, msgEcho, msgEcho, &echoMsg{Text: "ping"}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Call without a response = %v, want context.DeadlineExceeded", err)
	}
	request := conn.lastWritten(t)

	var mu sync.Mutex
	var handled []string
	pushSeen := make(chan struct{}, 4)
	s.RegisterPushHandler(msgEcho, func(m *Message) {
		mu.Lock()
		handled = append(handled, m.Value.(*echoMsg).Text)
		mu.Unlock()
		pushSeen <- struct{}{}
	})
	waitCtx, waitCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer waitCancel()
	waited := make(chan *Message, 1)
	waitReady := make(chan struct{})
	go func() {
		close(waitReady)
		m, err := s.WaitPush(waitCtx, msgEcho, nil)
		if err != nil {
			t.Errorf("WaitPush: %v", err)
		}
		waited <- m
	}()
	<-waitReady
	// WaitPush 登记是异步的：等它进入 waiters 再投递，避免把“还没登记”误判为“已丢弃”。
	deadline := time.Now().Add(2 * time.Second)
	for {
		s.mu.Lock()
		n := len(s.waiters)
		s.mu.Unlock()
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("WaitPush did not register")
		}
		time.Sleep(time.Millisecond)
	}

	late, _ := s.protocols.Encode(msgEcho, &echoMsg{Text: "late response"})
	push, _ := s.protocols.Encode(msgEcho, &echoMsg{Text: "push"})
	conn.inbound <- &transport.Packet{MsgID: msgEcho, Seq: request.Seq, Payload: late}
	conn.inbound <- &transport.Packet{MsgID: msgEcho, Seq: 0, Payload: push}

	select {
	case <-pushSeen:
	case <-time.After(2 * time.Second):
		t.Fatal("the real push never reached the handler")
	}
	m := <-waited
	if m == nil {
		t.Fatal("WaitPush returned no message")
	}
	if got := m.Value.(*echoMsg).Text; got != "push" {
		t.Errorf("WaitPush delivered %q, want the push: a response that arrived after its Call timed out was delivered as a push", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(handled) == 0 || handled[0] != "push" {
		t.Errorf("push handler saw %q, want only [push]: the late response reached the push handler", handled)
	}
}
