package robot_test

// RR-20261005-NC-162：重连之后再次 EnsurePushCapture 必须在新会话上装上 push handler。
//
// 重连就是剧本里再跑一次 connect：SetSession 换上新会话并关闭旧会话，旧会话关闭时清空自己的
// handler 表。旧 EnsurePushCapture 只看黑板上的 "capture:<key>" 标记——标记要到 Context.Close
// 才删——所以重连后的再次安装直接当成“已装过”返回 nil，新会话上没有 handler，之后的 push
// （例如重连后服务端重发的整份 EntitySync）被会话静默丢掉。

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/robot"
	"github.com/tjbdwanghaibo/roost-core/robot/session"
	"github.com/tjbdwanghaibo/roost-core/robot/transport"
)

// chanConn 是只读的内存连接：测试往 packets 里塞服务端推送。
type chanConn struct {
	packets chan *transport.Packet
	closed  chan struct{}
	once    atomic.Bool
}

func newChanConn() *chanConn {
	return &chanConn{packets: make(chan *transport.Packet, 8), closed: make(chan struct{})}
}

func (c *chanConn) ReadPacket() (*transport.Packet, error) {
	select {
	case packet := <-c.packets:
		return packet, nil
	case <-c.closed:
		return nil, session.ErrClosed
	}
}
func (c *chanConn) WritePackets([]*transport.Packet) error { return nil }
func (c *chanConn) Close() error {
	if c.once.CompareAndSwap(false, true) {
		close(c.closed)
	}
	return nil
}
func (c *chanConn) RemoteAddr() string { return "mem" }

func TestPushCaptureIsReinstalledOnTheSessionAfterReconnect(t *testing.T) {
	const msgSync = 12
	rb := robot.NewContext(robot.Config{})
	t.Cleanup(func() { _ = rb.Close() })
	var delivered atomic.Int32
	handler := func(*session.Message) { delivered.Add(1) }

	first := newChanConn()
	rb.SetSession(session.New(first, nil))
	if err := rb.EnsurePushCapture("scene", msgSync, handler); err != nil {
		t.Fatal(err)
	}

	// 重连：connect 动作再次 SetSession，旧会话被关闭；剧本随后再次安装同一个 capture。
	second := newChanConn()
	rb.SetSession(session.New(second, nil))
	if err := rb.EnsurePushCapture("scene", msgSync, handler); err != nil {
		t.Fatal(err)
	}

	second.packets <- &transport.Packet{MsgID: msgSync, Seq: 0, Payload: []byte("{}")}
	deadline := time.Now().Add(2 * time.Second)
	for delivered.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := delivered.Load(); got != 1 {
		t.Fatalf("a push on the reconnected session reached the capture %d times; want 1 (EnsurePushCapture after reconnect must register on the new session)", got)
	}

	// 同一会话上再次安装仍是幂等的：再推一条只多计一次。会话按到达顺序同步调用 handler，
	// 所以推一条哨兵并等它到达，就说明前一条已经分发完。
	if err := rb.EnsurePushCapture("scene", msgSync, handler); err != nil {
		t.Fatal(err)
	}
	const msgMarker = 13
	marker := make(chan struct{}, 1)
	if err := rb.EnsurePushCapture("marker", msgMarker, func(*session.Message) { marker <- struct{}{} }); err != nil {
		t.Fatal(err)
	}
	second.packets <- &transport.Packet{MsgID: msgSync, Seq: 0, Payload: []byte("{}")}
	second.packets <- &transport.Packet{MsgID: msgMarker, Seq: 0, Payload: []byte("{}")}
	awaitChan(t, marker, "the marker push")
	if got := delivered.Load(); got != 2 {
		t.Fatalf("after a repeated install on the same session one push was delivered %d times in total; want 2 (no double registration)", got)
	}
}
