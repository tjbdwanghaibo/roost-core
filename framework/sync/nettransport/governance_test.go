package nettransport

import (
	"context"
	"errors"
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
