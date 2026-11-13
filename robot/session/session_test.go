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

// Call 受调用方 ctx 约束，包括发送阶段：写被卡住（服务端不读、发送缓冲满）时 Call 在自己的超时
// 返回，不等到会话关闭。
// 历史：RR-20261005-NC-261
func TestCallReturnsWhenTheWriteBlocksPastItsContext(t *testing.T) {
	s, conn := newScriptedSession(t)
	release := make(chan struct{})
	conn.mu.Lock()
	conn.blockWrites = release
	conn.mu.Unlock()
	defer close(release)

	// 期限在写已经开始并卡住之后才到：原来用 50ms 的 WithTimeout，重负载下 Call 的 goroutine 可能在期限之后才被调度，
	// 发送前的 ctx.Err() 检查直接返回（没开始写、不关连接——那是正确行为），最后一条断言就会误报“transport open”。
	ctx := newManualDeadline()
	done := make(chan error, 1)
	go func() {
		_, err := s.Call(ctx, msgEcho, msgEcho, &echoMsg{Text: "ping"})
		done <- err
	}()
	select {
	case <-conn.writeEnter:
	case <-time.After(2 * time.Second):
		t.Fatal("Call never started writing")
	}
	ctx.expire()
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
	select {
	case <-conn.closed:
	default:
		t.Fatal("timed-out write left transport open; repeated calls can accumulate blocked writers")
	}

}

// manualDeadline 是由用例决定何时到期的 ctx：到期后 Done 关闭、Err 为 context.DeadlineExceeded，与真实超时同形。
type manualDeadline struct {
	context.Context
	done chan struct{}
	once sync.Once
}

func newManualDeadline() *manualDeadline {
	return &manualDeadline{Context: context.Background(), done: make(chan struct{})}
}

func (c *manualDeadline) Done() <-chan struct{} { return c.done }

func (c *manualDeadline) Err() error {
	select {
	case <-c.done:
		return context.DeadlineExceeded
	default:
		return nil
	}
}

func (c *manualDeadline) expire() { c.once.Do(func() { close(c.done) }) }

// 期限在 Call 开始写之前就已经到了：不写、不留等待者，连接保持打开（没有半帧，可以继续复用）。
func TestCallPastItsDeadlineBeforeWritingLeavesTheTransportOpen(t *testing.T) {
	s, conn := newScriptedSession(t)
	ctx := newManualDeadline()
	ctx.expire()
	if _, err := s.Call(ctx, msgEcho, msgEcho, &echoMsg{Text: "ping"}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Call past its deadline = %v, want context.DeadlineExceeded", err)
	}
	select {
	case <-conn.writeEnter:
		t.Fatal("Call wrote although its deadline had passed")
	default:
	}
	select {
	case <-conn.closed:
		t.Fatal("Call closed the transport although it never started writing")
	default:
	}
}

// 超时之后才到的应答（seq 非 0、已无等待者）丢弃，不当成推送：传输层约定只有 seq 0 是推送，
// 同 msg id 的 WaitPush / 推送 handler 收不到旧应答。
// 历史：RR-20261005-NC-262
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
