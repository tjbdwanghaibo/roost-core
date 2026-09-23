package nettransport

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// U-0131 · C2（空洞测试）· nightly gap map core `nettransport` 10/20。
//
// 准入对会话状态的三种拒绝——未注册、正在排空（RemoveSession 后下游仍在发）、
// 已失败（下游出错后队列进入终态）——各有自己的哨兵，且失败态要带上下游
// 的原因；nil 传输的 SendReliable / session 报 ErrTransportClosed。
// （M-18 之后 AsyncTransport 只有 reliable 一条 lane，控制面与 datagram 批准入的
// 断言随实现一起删除。）

// failingReliableTransport blocks its first send until released and then
// fails it, so a second message can be queued behind the failure.
type failingReliableTransport struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (transport *failingReliableTransport) SendReliable(context.Context, SessionID, []byte) error {
	transport.once.Do(func() { close(transport.started) })
	<-transport.release
	return errors.New("stream reset by peer")
}

func waitStats(t *testing.T, transport *AsyncTransport, ready func(AsyncTransportStats) bool) AsyncTransportStats {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		stats := transport.Stats()
		if ready(stats) {
			return stats
		}
		if time.Now().After(deadline) {
			t.Fatalf("transport never reached the expected state: %+v", stats)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestSendReliableRefusesUnregisteredDrainingAndFailedSessions(t *testing.T) {
	ctx := context.Background()
	var none *AsyncTransport
	if err := none.SendReliable(ctx, 1, []byte("x")); !errors.Is(err, ErrTransportClosed) {
		t.Fatalf("SendReliable on a nil transport = %v", err)
	}
	if _, err := none.session(1); !errors.Is(err, ErrTransportClosed) {
		t.Fatalf("session on a nil transport = %v", err)
	}

	// 未注册。
	transport := admissionTransport(t, nil)
	if err := transport.SendReliable(ctx, 99, []byte("x")); !errors.Is(err, ErrSessionNotRegistered) {
		t.Fatalf("SendReliable to an unregistered session = %v", err)
	}
	if _, err := transport.session(99); !errors.Is(err, ErrSessionNotRegistered) {
		t.Fatalf("session(99) = %v", err)
	}

	// 排空中：下游忽略 ctx，RemoveSession 后会话仍在表里但 closing。
	stubborn := &stubbornReliableTransport{started: make(chan struct{}), release: make(chan struct{})}
	config := DefaultAsyncTransportConfig()
	config.SendTimeout = time.Second
	draining, err := NewAsyncTransport(stubborn, config)
	if err != nil {
		t.Fatal(err)
	}
	if err := draining.RegisterSession(SessionInfo{ID: 41}); err != nil {
		t.Fatal(err)
	}
	if err := draining.SendReliable(ctx, 41, []byte{1}); err != nil {
		t.Fatal(err)
	}
	await(t, stubborn.started)
	if !draining.RemoveSession(41) {
		t.Fatal("session should begin draining")
	}
	waitStats(t, draining, func(s AsyncTransportStats) bool { return s.DrainingSessions == 1 })
	if err := draining.SendReliable(ctx, 41, []byte("late")); !errors.Is(err, ErrSessionNotRegistered) {
		t.Fatalf("SendReliable to a draining session = %v, want ErrSessionNotRegistered", err)
	}
	if draining.Stats().ReliableQueued != 1 {
		t.Fatal("a draining session accepted new reliable work")
	}
	close(stubborn.release)
	closeCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	if err := draining.Close(closeCtx); err != nil {
		t.Fatal(err)
	}

	// 已失败：下游出错是该会话的终态——排在后面的消息作废并计数，ErrorHandler
	// 拿到下游原因，会话随 worker 退出而注销（下游的 RemoveSession 被调用、id 可复用），
	// 之后再发报 ErrSessionNotRegistered。
	failing := &failingReliableTransport{started: make(chan struct{}), release: make(chan struct{})}
	var reported []SendError
	var reportedMu sync.Mutex
	failingConfig := config
	failingConfig.OnError = func(e SendError) {
		reportedMu.Lock()
		reported = append(reported, e)
		reportedMu.Unlock()
	}
	failedTransport, err := NewAsyncTransport(failing, failingConfig)
	if err != nil {
		t.Fatal(err)
	}
	if err := failedTransport.RegisterSession(SessionInfo{ID: 51}); err != nil {
		t.Fatal(err)
	}
	if err := failedTransport.SendReliable(ctx, 51, []byte("first")); err != nil {
		t.Fatal(err)
	}
	await(t, failing.started)
	if err := failedTransport.SendReliable(ctx, 51, []byte("second")); err != nil {
		t.Fatal(err)
	}
	close(failing.release)
	stats := waitStats(t, failedTransport, func(s AsyncTransportStats) bool {
		return s.SendErrors == 1 && s.ActiveSessions == 0 && s.DrainingSessions == 0
	})
	if stats.ReliableAbandoned != 1 || stats.ReliableSent != 0 {
		t.Fatalf("the message queued behind the failure was not abandoned: %+v", stats)
	}
	reportedMu.Lock()
	if len(reported) != 1 || reported[0].Session != 51 || !strings.Contains(reported[0].Error(), "stream reset by peer") {
		t.Fatalf("ErrorHandler did not get the downstream cause: %+v", reported)
	}
	reportedMu.Unlock()
	if err := failedTransport.SendReliable(ctx, 51, []byte("third")); !errors.Is(err, ErrSessionNotRegistered) {
		t.Fatalf("SendReliable after the session failed = %v, want ErrSessionNotRegistered", err)
	}
	if err := failedTransport.RegisterSession(SessionInfo{ID: 51}); err != nil {
		t.Fatalf("a failed session's id was not released: %v", err)
	}
	closeCtx2, cancel2 := context.WithTimeout(ctx, time.Second)
	defer cancel2()
	if err := failedTransport.Close(closeCtx2); err != nil {
		t.Fatal(err)
	}
}
