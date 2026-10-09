package engine

import (
	"context"
	"sync"
	"testing"
	"time"
)

type lifecycleOutboxStore struct {
	*projectorOutboxFake
	claims chan struct{}
}

func (s *lifecycleOutboxStore) Claim(ctx context.Context, _ string, _ time.Time, _ int, _ time.Duration) ([]OutboxItem, error) {
	select {
	case s.claims <- struct{}{}:
	default:
	}
	return nil, ctx.Err()
}
func TestOutboxCloseBeforeStartIsTerminal(t *testing.T) {
	store := &lifecycleOutboxStore{newProjectorOutboxFake(), make(chan struct{}, 1)}
	worker, err := NewOutboxWorker(store, failingOutboxPublisher{}, OutboxWorkerOptions{Owner: "lifecycle"})
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	worker.Start(ctx)
	select {
	case <-store.claims:
		t.Fatal("closed outbox started claiming work")
	case <-time.After(20 * time.Millisecond):
	}
	if err := worker.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}
func TestOutboxConcurrentStartClose(t *testing.T) {
	for range 100 {
		worker, err := NewOutboxWorker(newProjectorOutboxFake(), failingOutboxPublisher{}, OutboxWorkerOptions{Owner: "lifecycle"})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		var wg sync.WaitGroup
		wg.Go(func() { worker.Start(ctx) })
		wg.Go(func() {
			if err := worker.Close(ctx); err != nil {
				t.Error(err)
			}
		})
		wg.Wait()
		if err := worker.Close(ctx); err != nil {
			t.Error(err)
		}
		cancel()
	}
}
