package nettransport

import (
	"context"
	"errors"
	"testing"
	"time"
)

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
