package dataengine

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-kit/nestwal"
)

type recordingHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *recordingHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *recordingHandler) Handle(_ context.Context, record slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, record.Clone())
	return nil
}
func (h *recordingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *recordingHandler) WithGroup(string) slog.Handler      { return h }

func (h *recordingHandler) matching(level slog.Level, needle string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, record := range h.records {
		if record.Level == level && strings.Contains(record.Message, needle) {
			n++
		}
	}
	return n
}

func captureLogs(t *testing.T) *recordingHandler {
	t.Helper()
	handler := &recordingHandler{}
	previous := slog.Default()
	slog.SetDefault(slog.New(handler))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return handler
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// The claim loop keeps polling when the store fails, which is right: Mongo
// coming back should resume publishing without a restart. But a store that is
// down for an hour must not be silent for an hour. The failure is logged once
// when the streak starts and once when it ends — not per poll, which at a
// 100ms interval would be ten lines a second of the same error.
func TestOutboxWorkerLogsStoreFailureStreakOnceAndItsRecovery(t *testing.T) {
	logs := captureLogs(t)
	store := newProjectorOutboxFake()
	store.mu.Lock()
	store.claimErr = errors.New("mongo: server selection timeout")
	store.mu.Unlock()
	worker, err := NewOutboxWorker(store, &successfulOutboxPublisher{}, OutboxWorkerOptions{Owner: "worker-1", BatchSize: 1, RetryMin: time.Millisecond, PollInterval: 2 * time.Millisecond, Workers: 1})
	if err != nil {
		t.Fatal(err)
	}
	worker.Start(context.Background())
	t.Cleanup(func() { _ = worker.Close(context.Background()) })

	waitFor(t, 2*time.Second, func() bool { return worker.Stats().StoreFailures >= 5 }, "five failed polls")
	if got := logs.matching(slog.LevelWarn, "outbox claim loop failing"); got != 1 {
		t.Fatalf("warn lines after a failure streak = %d, want exactly 1", got)
	}

	store.mu.Lock()
	store.claimErr = nil
	store.mu.Unlock()
	waitFor(t, 2*time.Second, func() bool { return logs.matching(slog.LevelInfo, "outbox claim loop recovered") == 1 }, "recovery line")
	failuresAtRecovery := worker.Stats().StoreFailures
	time.Sleep(20 * time.Millisecond)
	if got := logs.matching(slog.LevelWarn, "outbox claim loop failing"); got != 1 {
		t.Fatalf("warn lines after recovery = %d, want still 1", got)
	}
	if worker.Stats().StoreFailures != failuresAtRecovery {
		t.Fatalf("store failures kept growing after recovery: %d -> %d", failuresAtRecovery, worker.Stats().StoreFailures)
	}
}

// blockingClaimStore behaves like a store whose driver honours the context: a
// claim in flight when the worker is shut down returns ctx.Err().
type blockingClaimStore struct{ *projectorOutboxFake }

func (store blockingClaimStore) Claim(ctx context.Context, _ string, _ time.Time, _ int, _ time.Duration) ([]OutboxItem, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// Shutdown is not a failure: the poll cut short by the worker's own context
// must not leave a "failing" line as the last thing in the log.
func TestOutboxWorkerDoesNotReportShutdownAsAStoreFailure(t *testing.T) {
	logs := captureLogs(t)
	worker, err := NewOutboxWorker(blockingClaimStore{newProjectorOutboxFake()}, &successfulOutboxPublisher{}, OutboxWorkerOptions{Owner: "worker-1", BatchSize: 1, RetryMin: time.Millisecond, PollInterval: time.Millisecond, Workers: 1})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	worker.Start(ctx)
	time.Sleep(10 * time.Millisecond)
	cancel()
	if err := worker.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := logs.matching(slog.LevelWarn, "outbox claim loop failing"); got != 0 {
		t.Fatalf("shutdown produced %d failure lines, want 0", got)
	}
	if worker.Stats().StoreFailures == 0 {
		t.Fatalf("the cancelled claim still counts as a store failure (one per claim goroutine); got 0")
	}
}

// The health line is what an operator reads first. Publish failures were on
// it; store failures — the claim/ack/nack side, i.e. Mongo — were not, even
// though saga's health line reports both.
func TestDataEngineHealthMessageReportsStoreFailures(t *testing.T) {
	message := dataEngineHealthMessage(nestwal.Stats{}, ProjectorStats{}, OutboxWorkerStats{StoreFailures: 7, PublishFailures: 2})
	if !strings.Contains(message, "publish_failures=2") || !strings.Contains(message, "store_failures=7") {
		t.Fatalf("health message %q must report both publish and store failures", message)
	}
}
