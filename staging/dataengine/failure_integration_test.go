package dataengine

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-kit/nestwal"
)

type recoveringPublisher struct {
	mu   sync.Mutex
	down bool
	ids  map[string]int
}

func (publisher *recoveringPublisher) Publish(_ context.Context, item OutboxItem) error {
	publisher.mu.Lock()
	defer publisher.mu.Unlock()
	if publisher.down {
		return context.DeadlineExceeded
	}
	publisher.ids[item.Effect.ID]++
	return nil
}

func TestNATSOutageDoesNotBlockProjectionAndBacklogRecoversByEffectID(t *testing.T) {
	options := nestwal.DefaultOptions(t.TempDir())
	options.WriterVersion = nestwal.WriterVersionV2
	wal, err := nestwal.Open(options)
	if err != nil {
		t.Fatal(err)
	}
	store := newProjectorOutboxFake()
	projector, err := NewProjector(wal, store, ProjectorOptions{CloseWAL: true, IdlePoll: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer projector.Close(context.Background())
	const records = 100
	for sequence := 1; sequence <= records; sequence++ {
		record := projectorRecord(byte(sequence), true)
		record.Effects[0].ID = "effect-" + record.ID.String()
		if err := projector.Commit(context.Background(), record); err != nil {
			t.Fatal(err)
		}
		projector.TransactionReleased(record.ID)
	}
	if err := projector.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	projected, pending := len(store.records), len(store.pending)
	store.mu.Unlock()
	if projected != records || pending != records || projector.Stats().WALUnacked != 0 {
		t.Fatalf("projected=%d pending=%d stats=%+v", projected, pending, projector.Stats())
	}
	publisher := &recoveringPublisher{down: true, ids: make(map[string]int)}
	worker, _ := NewOutboxWorker(store, publisher, OutboxWorkerOptions{Owner: "worker", BatchSize: 32, RetryMin: time.Nanosecond})
	if _, err := worker.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	publisher.mu.Lock()
	publisher.down = false
	publisher.mu.Unlock()
	for {
		store.mu.Lock()
		remaining := len(store.pending)
		store.mu.Unlock()
		if remaining == 0 {
			break
		}
		if _, err := worker.RunOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	publisher.mu.Lock()
	defer publisher.mu.Unlock()
	if len(publisher.ids) != records {
		t.Fatalf("published unique effects=%d", len(publisher.ids))
	}
	for id, count := range publisher.ids {
		if count != 1 {
			t.Fatalf("effect %s published %d times", id, count)
		}
	}
}
