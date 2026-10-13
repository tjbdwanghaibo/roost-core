// 传输治理：全局常驻字节预算、可靠队列字节与时效、会话登记与发送的准入拒绝。

package nettransport

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestConcurrentSessionsShareOneHardResidentBudget(t *testing.T) {
	d := newBlockingTransport()
	tr, err := NewAsyncTransport(d, AsyncTransportConfig{MaxResidentReliableBytes: 64, SendTimeout: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errorsFound := make(chan error, 100)
	for id := SessionID(1); id <= 100; id++ {
		if err := tr.RegisterSession(SessionInfo{ID: id}); err != nil {
			t.Fatal(err)
		}
		wg.Go(func() {
			if err := tr.SendReliable(context.Background(), id, make([]byte, 8)); err != nil && !errors.Is(err, ErrReliableBackpressure) {
				errorsFound <- err
			}
		})
	}
	wg.Wait()
	close(errorsFound)
	for err := range errorsFound {
		t.Fatal(err)
	}
	if c := tr.Counters(); c.ResidentReliableBytes != 64 || c.ReliableQueued != 8 || c.GlobalBackpressure != 92 {
		t.Fatalf("hard budget bypassed: %+v", c)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_ = tr.Close(ctx)
	ctx, cancel = context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := tr.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if c := tr.Counters(); c.ResidentReliableBytes != 0 || c.ActiveSessions != 0 {
		t.Fatalf("leak: %+v", c)
	}
}

func TestGlobalResidentBudgetIncludesInFlightAndReleasesOnCancel(t *testing.T) {
	d := newBlockingTransport()
	tr, err := NewAsyncTransport(d, AsyncTransportConfig{MaxResidentReliableBytes: 10, SendTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []SessionID{1, 2} {
		if err := tr.RegisterSession(SessionInfo{ID: id}); err != nil {
			t.Fatal(err)
		}
	}
	if err := tr.SendReliable(context.Background(), 1, []byte("123456")); err != nil {
		t.Fatal(err)
	}
	await(t, d.reliableStarted)
	if err := tr.SendReliable(context.Background(), 2, []byte("12345")); !errors.Is(err, ErrReliableBackpressure) {
		t.Fatal(err)
	}
	if err := tr.SendReliable(context.Background(), 1, []byte("1234")); err != nil {
		t.Fatal(err)
	}
	if got := tr.Counters(); got.ResidentReliableBytes != 10 || got.GlobalBackpressure != 1 {
		t.Fatalf("counters: %+v", got)
	}
	tr.RemoveSession(1)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := tr.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if got := tr.Counters(); got.ResidentReliableBytes != 0 || got.ActiveSessions != 0 {
		t.Fatalf("leaked quota: %+v", got)
	}
}

func TestReliableAgeIncludesQueueWaitAndDoesNotSendExpiredSuffix(t *testing.T) {
	d := newBlockingTransport()
	tr, err := NewAsyncTransport(d, AsyncTransportConfig{MaxReliableAge: time.Hour, SendTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if err := tr.RegisterSession(SessionInfo{ID: 1}); err != nil {
		t.Fatal(err)
	}
	if err := tr.SendReliable(context.Background(), 1, []byte("first")); err != nil {
		t.Fatal(err)
	}
	await(t, d.reliableStarted)
	if err := tr.SendReliable(context.Background(), 1, []byte("expired")); err != nil {
		t.Fatal(err)
	}
	// 控制队列头的准入时刻，不靠 sleep 猜测 worker 顺序。
	tr.mu.RLock()
	q := tr.sessions[1]
	q.mu.Lock()
	q.reliable[q.head].queuedAt = time.Now().Add(-2 * time.Hour)
	q.mu.Unlock()
	tr.mu.RUnlock()
	d.reliableRelease <- struct{}{}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := tr.Close(ctx); err != nil {
		t.Fatal(err)
	}
	got := tr.Counters()
	if got.ReliableExpired != 1 || got.ReliableSent != 1 || got.ResidentReliableBytes != 0 {
		t.Fatalf("counters: %+v", got)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.reliable) != 1 {
		t.Fatal("expired payload sent")
	}
}

func TestCloseDeadlineReleasesQueuedAndBusyBudget(t *testing.T) {
	d := newBlockingTransport()
	tr, err := NewAsyncTransport(d, AsyncTransportConfig{SendTimeout: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if err := tr.RegisterSession(SessionInfo{ID: 1}); err != nil {
		t.Fatal(err)
	}
	if err := tr.SendReliable(context.Background(), 1, []byte("busy")); err != nil {
		t.Fatal(err)
	}
	await(t, d.reliableStarted)
	if err := tr.SendReliable(context.Background(), 1, []byte("queued")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := tr.Close(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	finished, cancelWait := context.WithTimeout(context.Background(), time.Second)
	defer cancelWait()
	if err := tr.Close(finished); err != nil {
		t.Fatal(err)
	}
	if got := tr.Counters(); got.ResidentReliableBytes != 0 || got.ReliableAbandoned != 1 {
		t.Fatalf("counters: %+v", got)
	}
}

func admissionTransport(t *testing.T, mutate func(*AsyncTransportConfig)) *AsyncTransport {
	t.Helper()
	downstream := TransportFunc{
		Datagram: func(context.Context, SessionID, []byte) error { return nil },
		Reliable: func(context.Context, SessionID, []byte) error { return nil },
	}
	cfg := DefaultAsyncTransportConfig()
	if mutate != nil {
		mutate(&cfg)
	}
	transport, err := NewAsyncTransport(downstream, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = transport.Close(context.Background()) })
	return transport
}

// Session registration is bounded and exclusive: a zero id, a second
// registration of the same id, more sessions than configured, and any
// registration after Close are refused with their own sentinel.
func TestRegisterSessionRefusesZeroDuplicateOverLimitAndClosed(t *testing.T) {
	transport := admissionTransport(t, func(c *AsyncTransportConfig) { c.MaxSessions = 1 })
	if err := transport.RegisterSession(SessionInfo{ID: 0}); !errors.Is(err, ErrSessionNotRegistered) {
		t.Fatalf("zero session id = %v", err)
	}
	if err := transport.RegisterSession(SessionInfo{ID: 7}); err != nil {
		t.Fatal(err)
	}
	if err := transport.RegisterSession(SessionInfo{ID: 7}); !errors.Is(err, ErrSessionAlreadyExists) {
		t.Fatalf("duplicate session = %v", err)
	}
	if err := transport.RegisterSession(SessionInfo{ID: 8}); !errors.Is(err, ErrSessionLimit) {
		t.Fatalf("over MaxSessions = %v", err)
	}
	if err := transport.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := transport.RegisterSession(SessionInfo{ID: 9}); !errors.Is(err, ErrTransportClosed) {
		t.Fatalf("register after Close = %v", err)
	}
}

// SendReliable validates before touching the queue: a session id, a
// non-empty payload, and a payload within MaxReliableBytes; a message exactly
// at the limit is admitted.
func TestSendReliableRefusesEachMalformedMessage(t *testing.T) {
	transport := admissionTransport(t, func(c *AsyncTransportConfig) { c.MaxReliableBytes = 8 })
	if err := transport.RegisterSession(SessionInfo{ID: 7}); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	cases := []struct {
		name    string
		session SessionID
		payload []byte
		want    error
	}{
		{"no session", 0, []byte("x"), ErrSessionNotRegistered},
		{"empty payload", 7, nil, ErrProtocolConfig},
		{"too big", 7, make([]byte, 9), ErrReliableMessageTooBig},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := transport.SendReliable(ctx, tc.session, tc.payload); !errors.Is(err, tc.want) {
				t.Fatalf("SendReliable = %v, want %v", err, tc.want)
			}
		})
	}
	if err := transport.SendReliable(ctx, 7, []byte("12345678")); err != nil {
		t.Fatalf("a message at the limit must be admitted: %v", err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := transport.SendReliable(cancelled, 7, []byte("x")); !errors.Is(err, context.Canceled) {
		t.Fatalf("SendReliable with a cancelled context = %v", err)
	}
}

func TestReliableQueueByteBudgetAndAge(t *testing.T) {
	downstream := newBlockingTransport()
	config := DefaultAsyncTransportConfig()
	config.MaxQueuedReliableBytes = 5
	config.SendTimeout = time.Second
	tr, err := NewAsyncTransport(downstream, config)
	if err != nil {
		t.Fatal(err)
	}
	if err := tr.RegisterSession(SessionInfo{ID: 1}); err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = tr.Close(ctx)
	}()
	if err := tr.SendReliable(context.Background(), 1, []byte("busy")); err != nil {
		t.Fatal(err)
	}
	await(t, downstream.reliableStarted)
	payload := []byte("12345")
	if err := tr.SendReliable(context.Background(), 1, payload); err != nil {
		t.Fatal(err)
	}
	payload[0] = 'x'
	if err := tr.SendReliable(context.Background(), 1, []byte("6")); !errors.Is(err, ErrReliableBackpressure) {
		t.Fatal(err)
	}
	tr.mu.RLock()
	queue := tr.sessions[1]
	tr.mu.RUnlock()
	queue.mu.Lock()
	queue.busySince = time.Now().Add(-time.Second)
	queue.mu.Unlock()
	stats := tr.Stats()
	if stats.PendingReliableBytes != 5 || stats.ReliableBytesInFlight != 4 || stats.OldestReliableAge <= 0 {
		t.Fatalf("stats: %+v", stats)
	}
	downstream.reliableRelease <- struct{}{}
	await(t, downstream.reliableStarted)
	stats = tr.Stats()
	if stats.PendingReliableBytes != 0 || stats.ReliableBytesInFlight != 5 {
		t.Fatalf("take accounting: %+v", stats)
	}
	downstream.reliableRelease <- struct{}{}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := tr.Close(ctx); err != nil {
		t.Fatal(err)
	}
	stats = tr.Stats()
	if stats.PendingReliableBytes != 0 || stats.ReliableBytesInFlight != 0 || stats.OldestReliableAge != 0 {
		t.Fatalf("drain accounting: %+v", stats)
	}
	downstream.mu.Lock()
	defer downstream.mu.Unlock()
	if string(downstream.reliable[1]) != "12345" {
		t.Fatal("queue retained caller-owned payload")
	}
}

func TestQueueReusesStorageWithoutReordering(t *testing.T) {
	q := &sessionQueue{reliable: make([]queuedReliable, 0, 4)}
	for n := 0; n < 20; n++ {
		for j := 0; j < 4; j++ {
			q.reliable = append(q.reliable, queuedReliable{data: []byte{byte(n*4 + j)}, queuedAt: time.Now()})
			q.queuedBytes++
		}
		for j := 0; j < 4; j++ {
			data, exit := q.take()
			if exit || len(data) != 1 || data[0] != byte(n*4+j) {
				t.Fatal("queue order changed")
			}
		}
		if len(q.reliable) != 0 || cap(q.reliable) != 4 || q.queuedBytes != 0 {
			t.Fatal("empty queue did not retain bounded storage")
		}
	}
}

// 准入对会话状态的三种拒绝——未注册、正在排空（RemoveSession 后下游仍在发）、已失败（下游出错后队列进入终态）——各有自己的
// 哨兵，失败态带上下游的原因；nil 传输的 SendReliable / session 报 ErrTransportClosed。

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

	// 未注册。
	transport := admissionTransport(t, nil)
	if err := transport.SendReliable(ctx, 99, []byte("x")); !errors.Is(err, ErrSessionNotRegistered) {
		t.Fatalf("SendReliable to an unregistered session = %v", err)
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
