package entitysync

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/entity"
	"github.com/tjbdwanghaibo/roost-core/sync/nettransport"
)

type drainingSender struct{ entered, release chan struct{} }

func (s *drainingSender) SendReliable(context.Context, nettransport.SessionID, []byte) error {
	close(s.entered)
	<-s.release
	return nil
}
func TestReopenRefusesDrainingTransportLifetime(t *testing.T) {
	sender := &drainingSender{make(chan struct{}), make(chan struct{})}
	transport, err := nettransport.NewAsyncTransport(sender, nettransport.DefaultAsyncTransportConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer transport.Close(context.Background())
	adapter, _ := NewAsyncTransport(transport)
	manager, err := NewManager(ManagerConfig{Transport: adapter})
	if err != nil {
		t.Fatal(err)
	}
	if err = manager.OpenSession(1); err != nil {
		t.Fatal(err)
	}
	if err = adapter.Push(context.Background(), 1, []byte("old")); err != nil {
		t.Fatal(err)
	}
	<-sender.entered
	defer close(sender.release)
	manager.CloseSession(1)
	if err = manager.OpenSession(1); !errors.Is(err, nettransport.ErrSessionAlreadyExists) {
		t.Fatalf("open=%v; must refuse old queue", err)
	}
	if manager.Stats().Sessions != 0 {
		t.Fatal("failed open created a fake session")
	}
}

// gatedLifecycle 让测试决定 SessionOpened 何时返回、返回什么；与 nettransport 一样，
// 只有打开成功的会话才能 Push。
type gatedLifecycle struct {
	entered    chan struct{}
	result     chan error
	mu         sync.Mutex
	registered map[SessionID]bool
}

func newGatedLifecycle() *gatedLifecycle {
	return &gatedLifecycle{entered: make(chan struct{}), result: make(chan error), registered: make(map[SessionID]bool)}
}

func (g *gatedLifecycle) SessionOpened(id SessionID) error {
	g.entered <- struct{}{}
	err := <-g.result
	if err == nil {
		g.mu.Lock()
		g.registered[id] = true
		g.mu.Unlock()
	}
	return err
}

func (g *gatedLifecycle) SessionClosed(id SessionID) {
	g.mu.Lock()
	delete(g.registered, id)
	g.mu.Unlock()
}

func (g *gatedLifecycle) Push(_ context.Context, id SessionID, _ []byte) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.registered[id] {
		return nettransport.ErrSessionNotRegistered
	}
	return nil
}

// RR-20260926-15 复核 (a)：第一次 OpenSession 的传输注册尚未返回时，同 ID 的并发 OpenSession
// 不能先得到 nil；每个调用方拿到的结果都要与注册结束后的实际会话状态一致。
func TestConcurrentOpenResultMatchesSessionState(t *testing.T) {
	for name, openErr := range map[string]error{"transport_accepts": nil, "transport_refuses": errors.New("transport refused")} {
		t.Run(name, func(t *testing.T) {
			gate := newGatedLifecycle()
			m := newTestManager(t, gate, ManagerConfig{})
			first := make(chan error, 1)
			go func() { first <- m.OpenSession(1) }()
			<-gate.entered
			second := m.OpenSession(1)
			gate.result <- openErr
			firstErr := <-first
			sessions := m.Stats().Sessions
			t.Logf("first open=%v concurrent open=%v sessions=%d", firstErr, second, sessions)
			if !errors.Is(firstErr, openErr) || (firstErr == nil) != (sessions == 1) {
				t.Fatalf("first open=%v but sessions=%d", firstErr, sessions)
			}
			if second == nil && sessions != 1 {
				t.Fatalf("concurrent OpenSession returned nil but no session exists (sessions=%d)", sessions)
			}
		})
	}
}

// RR-20260926-15 复核 (c)：传输尚未确认打开前，会话不能被 Subscribe/Flush 看见；
// 否则 Flush 向未注册的传输 Push，触发一次假的 SessionLost，并把随后成功打开的会话删掉。
func TestSessionInvisibleUntilTransportOpened(t *testing.T) {
	for name, openErr := range map[string]error{"transport_accepts": nil, "transport_refuses": errors.New("transport refused")} {
		t.Run(name, func(t *testing.T) {
			gate := newGatedLifecycle()
			var lost []error
			m := newTestManager(t, gate, ManagerConfig{SessionLost: func(_ SessionID, err error) { lost = append(lost, err) }})
			packs := 0
			if err := m.Register(testSubject(t, 7, &packs)); err != nil {
				t.Fatal(err)
			}
			opened := make(chan error, 1)
			go func() { opened <- m.OpenSession(1) }()
			<-gate.entered
			subscribeErr := m.Subscribe(1, 7, entity.SyncProfile{})
			flushErr := m.Flush(t.Context())
			gate.result <- openErr
			openResult := <-opened
			sessions := m.Stats().Sessions
			t.Logf("during open: subscribe=%v flush=%v; open=%v SessionLost=%v sessions=%d", subscribeErr, flushErr, openResult, lost, sessions)
			if len(lost) != 0 {
				t.Fatalf("session reported lost before its transport lifetime existed: %v", lost)
			}
			if (openResult == nil) != (sessions == 1) {
				t.Fatalf("open=%v but sessions=%d", openResult, sessions)
			}
		})
	}
}

// firstSendBlocks 让第一条发送卡住直到 release，之后的发送只计数。
type firstSendBlocks struct {
	entered, release chan struct{}
	mu               sync.Mutex
	calls            int
}

func (s *firstSendBlocks) SendReliable(context.Context, nettransport.SessionID, []byte) error {
	s.mu.Lock()
	s.calls++
	first := s.calls == 1
	s.mu.Unlock()
	if first {
		close(s.entered)
		<-s.release
	}
	return nil
}

func (s *firstSendBlocks) sends() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// RR-20260926-15 复核 (a)(b)：正式 AsyncTransport 下旧发送在途时，多个调用方并发重开同一 ID，
// 每个都明确失败且可用 errors.Is 识别为可重试（ErrSessionClosing / ErrSessionOpening），
// 不会有人拿到 nil 而会话不存在；旧发送退出后重试成功，新会话照常收帧。
func TestConcurrentReopenWhileDrainingIsRetryable(t *testing.T) {
	sender := &firstSendBlocks{entered: make(chan struct{}), release: make(chan struct{})}
	transport, err := nettransport.NewAsyncTransport(sender, nettransport.DefaultAsyncTransportConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer transport.Close(context.Background())
	adapter, _ := NewAsyncTransport(transport)
	m := newTestManager(t, adapter, ManagerConfig{})
	open(t, m, 1)
	if err := adapter.Push(context.Background(), 1, []byte("old")); err != nil {
		t.Fatal(err)
	}
	<-sender.entered
	m.CloseSession(1)

	errs := make([]error, 8)
	var wg sync.WaitGroup
	for i := range errs {
		wg.Go(func() { errs[i] = m.OpenSession(1) })
	}
	wg.Wait()
	sessions := m.Stats().Sessions
	t.Logf("concurrent reopen while draining: %v sessions=%d", errs, sessions)
	for _, err := range errs {
		switch {
		case errors.Is(err, ErrSessionClosing):
			if !errors.Is(err, nettransport.ErrSessionAlreadyExists) {
				t.Fatalf("ErrSessionClosing lost the transport cause: %v", err)
			}
		case errors.Is(err, ErrSessionOpening):
		default:
			t.Fatalf("reopen while old send in flight must fail with a retryable sentinel, got %v (sessions=%d)", err, sessions)
		}
	}
	if sessions != 0 {
		t.Fatalf("failed reopen left %d sessions", sessions)
	}

	close(sender.release)
	deadline := time.Now().Add(10 * time.Second)
	for err = m.OpenSession(1); errors.Is(err, ErrSessionClosing); err = m.OpenSession(1) {
		if time.Now().After(deadline) {
			t.Fatal("old send never drained")
		}
		runtime.Gosched() // 等旧 worker 退出；只让出调度，不用 sleep 制造时序
	}
	if err != nil {
		t.Fatalf("retry after drain: %v", err)
	}
	packs := 0
	if err := m.Register(testSubject(t, 7, &packs)); err != nil {
		t.Fatal(err)
	}
	mustSubscribe(t, m, 1, 7, entity.SyncProfile{})
	mustFlush(t, m)
	if err := transport.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := sender.sends(); got != 2 || m.Stats().SessionsLost != 0 {
		t.Fatalf("reopened session did not receive its frame: sends=%d lost=%d", got, m.Stats().SessionsLost)
	}
}
