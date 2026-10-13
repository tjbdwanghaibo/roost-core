package robot_test

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/robot"
	"github.com/tjbdwanghaibo/roost-core/robot/protocol"
	"github.com/tjbdwanghaibo/roost-core/robot/session"
	"github.com/tjbdwanghaibo/roost-core/robot/transport"
)

// echoServer answers every request packet with the same msg id and seq; a
// handler map can override per-msgID behavior.
type echoServer struct {
	listener net.Listener
	handlers map[uint32]func(*transport.Packet) *transport.Packet
	wg       sync.WaitGroup
}

func newEchoServer(t *testing.T) *echoServer {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &echoServer{listener: listener, handlers: map[uint32]func(*transport.Packet) *transport.Packet{}}
	s.wg.Add(1)
	go s.serve()
	t.Cleanup(func() {
		_ = listener.Close()
		s.wg.Wait()
	})
	return s
}

func (s *echoServer) addr() string { return s.listener.Addr().String() }

func (s *echoServer) serve() {
	defer s.wg.Done()
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer conn.Close()
			for {
				packet, err := transport.ReadPacketFrom(conn, 0)
				if err != nil {
					return
				}
				response := packet
				if handler := s.handlers[packet.MsgID]; handler != nil {
					response = handler(packet)
				}
				if response == nil {
					continue
				}
				if err := transport.WritePacketsTo(conn, []*transport.Packet{response}); err != nil {
					return
				}
			}
		}()
	}
}

type pingReq struct {
	Nonce int64 `json:"nonce"`
}

type pingResp struct {
	Nonce int64 `json:"nonce"`
	Code  int32 `json:"code"`
}

func (r *pingResp) GetCode() int32 { return r.Code }

const msgPing = 7

func newTestSession(t *testing.T, server *echoServer) (*session.Session, *protocol.Registry) {
	t.Helper()
	protocols := protocol.NewRegistry(protocol.JSONCodec{})
	if err := protocol.RegisterMessage[pingReq](protocols, msgPing); err != nil {
		t.Fatal(err)
	}
	conn, err := transport.Dial(context.Background(), transport.Config{Endpoint: server.addr()})
	if err != nil {
		t.Fatal(err)
	}
	s := session.New(conn, protocols)
	t.Cleanup(func() { _ = s.Close() })
	return s, protocols
}

func TestPacketCodecRoundTrip(t *testing.T) {
	packets := []*transport.Packet{
		{MsgID: 1, Seq: 42, Payload: []byte("hello")},
		{MsgID: 2, Seq: 0, Payload: nil},
	}
	encoded, err := transport.EncodePackets(packets)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := transport.DecodePackets(encoded, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(decoded) != 2 || decoded[0].MsgID != 1 || decoded[0].Seq != 42 || string(decoded[0].Payload) != "hello" {
		t.Fatalf("decoded = %+v", decoded[0])
	}
	if _, err := transport.DecodePackets([]byte{1, 2, 3}, 0); err == nil {
		t.Fatal("short frame accepted")
	}
}

func TestSessionCallEchoAndPush(t *testing.T) {
	server := newEchoServer(t)
	// Push handler: msg 9 packets fan out as seq-0 pushes.
	server.handlers[msgPing] = func(p *transport.Packet) *transport.Packet {
		return &transport.Packet{MsgID: msgPing, Seq: p.Seq, Payload: p.Payload}
	}
	s, protocols := newTestSession(t, server)

	msg, err := s.Call(context.Background(), msgPing, msgPing, &pingReq{Nonce: 99})
	if err != nil {
		t.Fatal(err)
	}
	// The registered decoder is for pingReq (echo!), so decode as request.
	echoed, ok := msg.Value.(*pingReq)
	if !ok || echoed.Nonce != 99 {
		t.Fatalf("echo = %+v", msg.Value)
	}
	_ = protocols

	// Call timeout: a server that never answers msg 8.
	server.handlers[8] = func(*transport.Packet) *transport.Packet { return nil }
	if err := protocol.RegisterMessage[pingReq](protocols, 8); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := s.Call(ctx, 8, 8, &pingReq{}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout = %v", err)
	}
}

func TestSessionCloseFansOutToPendingAndWaiters(t *testing.T) {
	server := newEchoServer(t)
	server.handlers[8] = func(*transport.Packet) *transport.Packet { return nil } // black hole
	s, protocols := newTestSession(t, server)
	if err := protocol.RegisterMessage[pingReq](protocols, 8); err != nil {
		t.Fatal(err)
	}
	callErr := make(chan error, 1)
	go func() {
		_, err := s.Call(context.Background(), 8, 8, &pingReq{})
		callErr <- err
	}()
	waitErr := make(chan error, 1)
	go func() {
		_, err := s.WaitPush(context.Background(), 55, nil)
		waitErr <- err
	}()
	time.Sleep(50 * time.Millisecond)
	_ = s.Close()
	if err := awaitChan(t, callErr, "the pending call to fail"); !errors.Is(err, session.ErrClosed) {
		t.Fatalf("pending call err = %v", err)
	}
	if err := awaitChan(t, waitErr, "the waiter to fail"); !errors.Is(err, session.ErrClosed) {
		t.Fatalf("waiter err = %v", err)
	}
}

func TestTypedKeyAndBlackboard(t *testing.T) {
	rb := robot.NewContext(robot.Config{ID: 1})
	bossID := robot.Key[int64]("boss_id")
	if _, ok := bossID.Get(rb); ok {
		t.Fatal("unset key returned a value")
	}
	bossID.Set(rb, 0) // zero is a real value, not "unset"
	if v, ok := bossID.Get(rb); !ok || v != 0 {
		t.Fatalf("zero value lost: %v %v", v, ok)
	}
	bossID.Set(rb, 42)
	if v := bossID.GetOr(rb, -1); v != 42 {
		t.Fatalf("GetOr = %d", v)
	}
	bossID.Clear(rb)
	if _, ok := bossID.Get(rb); ok {
		t.Fatal("cleared key still set")
	}
}

func TestCoalescerDedupsAndFlushes(t *testing.T) {
	var mu sync.Mutex
	flushed := map[string]int64{}
	c := robot.NewCoalescer[string](20*time.Millisecond, func(batch map[string]int64) {
		mu.Lock()
		for k, v := range batch {
			if v > flushed[k] {
				flushed[k] = v
			}
		}
		mu.Unlock()
	})
	c.Add("topic:1", 3)
	c.Add("topic:1", 9) // higher seq wins
	c.Add("topic:1", 5) // lower seq ignored
	c.Add("topic:2", 1)
	time.Sleep(80 * time.Millisecond)
	_ = c.Close()
	mu.Lock()
	defer mu.Unlock()
	if flushed["topic:1"] != 9 || flushed["topic:2"] != 1 {
		t.Fatalf("flushed = %+v", flushed)
	}
}

func TestBoundedQueueDropsOldest(t *testing.T) {
	q := robot.NewBoundedQueue[int](2)
	if q.Push(1) || q.Push(2) {
		t.Fatal("premature drop")
	}
	if !q.Push(3) {
		t.Fatal("full queue did not drop")
	}
	v, ok := q.TryPop()
	if !ok || v != 2 {
		t.Fatalf("head = %d (oldest must be dropped)", v)
	}
}

func TestContextCloseHooksLIFO(t *testing.T) {
	rb := robot.NewContext(robot.Config{})
	var order []int
	rb.AddCloseHook(func() error { order = append(order, 1); return nil })
	rb.AddCloseHook(func() error { order = append(order, 2); return errors.New("hook2") })
	err := rb.Close()
	if err == nil || len(order) != 2 || order[0] != 2 || order[1] != 1 {
		t.Fatalf("order=%v err=%v", order, err)
	}
}

func TestEnsurePushCaptureIsIdempotent(t *testing.T) {
	server := newEchoServer(t)
	s, _ := newTestSession(t, server)
	rb := robot.NewContext(robot.Config{})
	rb.SetSession(s)
	var count atomic.Int32
	handler := func(*session.Message) { count.Add(1) }
	if err := rb.EnsurePushCapture("sync", 12, handler); err != nil {
		t.Fatal(err)
	}
	if err := rb.EnsurePushCapture("sync", 12, handler); err != nil {
		t.Fatal(err) // second install is a no-op
	}
	// Feed one push through: msg 12 seq 0.
	server.handlers[msgPing] = func(*transport.Packet) *transport.Packet {
		return &transport.Packet{MsgID: 12, Seq: 0, Payload: mustJSON(t, &pingReq{})}
	}
	pushCtx, cancelPush := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancelPush()
	_, _ = s.Call(pushCtx, msgPing, 0, &pingReq{}) // the reply arrives as a push, so the call itself times out
	deadline := time.Now().Add(time.Second)
	for count.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := count.Load(); got != 1 {
		t.Fatalf("push handler fired %d times (double registration?)", got)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// awaitChan receives from ch with an upper bound. A bare receive made a broken
// property fail as a go test timeout — a stack dump after the default ten
// minutes, naming no expectation — so every wait that IS the assertion is
// bounded and says what it was waiting for.
func awaitChan[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
		var zero T
		return zero
	}
}

// 重连（剧本里再跑一次 connect，SetSession 换上新会话并关闭旧会话、清空旧会话的 handler 表）之后
// 再次 EnsurePushCapture 在新会话上装上 push handler：重连后服务端重发的推送（例如整份 EntitySync）
// 照常被捕获，不因黑板上残留的 "capture:<key>" 标记被当成已装过。
// 历史：RR-20261005-NC-162

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

// Coalescer.Close 返回时最后一批已经交给 flush 并处理完：调用方随后关闭会话，最后一批确认不会
// 发在已关闭的会话上或丢失。
// 历史：RR-20261005-NC-265
func TestCoalescerCloseReturnsAfterTheFinalFlush(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{})
	var once sync.Once
	var mu sync.Mutex
	flushed := map[string]int64{}
	c := robot.NewCoalescer[string](time.Hour, func(batch map[string]int64) {
		once.Do(func() { close(entered) })
		<-release
		mu.Lock()
		for k, v := range batch {
			flushed[k] = v
		}
		mu.Unlock()
	})
	c.Add("topic:1", 7) // 起 worker；间隔 1h，只有 Close 会触发 flush

	closed := make(chan struct{})
	go func() {
		_ = c.Close()
		close(closed)
	}()
	select {
	case <-closed:
		close(release)
		t.Fatal("Close returned while the final flush had not finished (the batch is still being sent)")
	case <-time.After(200 * time.Millisecond):
	}
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("Close never flushed the pending batch")
	}
	close(release)
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not return after the final flush finished")
	}
	mu.Lock()
	defer mu.Unlock()
	if flushed["topic:1"] != 7 {
		t.Fatalf("flushed = %v, want topic:1=7", flushed)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("second Close = %v", err)
	}
}
