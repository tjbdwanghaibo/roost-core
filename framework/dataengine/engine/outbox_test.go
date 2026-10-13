// Outbox：启动 / 关闭生命周期、失败后的重试延迟、存储租约与 ack / nack、worker 硬上限与故障日志、积压指标。

package engine

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	coredata "github.com/tjbdwanghaibo/roost-core/framework/dataengine"
	"github.com/tjbdwanghaibo/roost-core/framework/nestwal"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
	"github.com/tjbdwanghaibo/roost-core/infra/storage/mongo/mongotest"
	"go.mongodb.org/mongo-driver/v2/bson"
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

type slowFailingPublisher struct {
	now *time.Time
}

func (publisher slowFailingPublisher) Publish(context.Context, OutboxItem) error {
	*publisher.now = publisher.now.Add(5 * time.Second)
	return context.DeadlineExceeded
}

// 慢发布不能耗掉失败后的退避窗口；用受控时钟验证实际 Mongo claim/nack 条件。
func TestOutboxRetryDelayStartsAfterFailedPublish(t *testing.T) {
	store, collection := newOutboxStoreTest(t)
	now := time.Date(2026, 9, 24, 0, 0, 0, 0, time.UTC)
	seedOutboxEffect(t, collection, "slow-effect", now, now, 0)
	worker, err := NewOutboxWorker(store, slowFailingPublisher{now: &now}, OutboxWorkerOptions{
		Owner: "slow-worker", BatchSize: 1, RetryMin: time.Second, RetryMax: 4 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	worker.now = func() time.Time { return now }
	for _, delay := range []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 4 * time.Second} {
		if count, err := worker.RunOnce(context.Background()); err != nil || count != 1 {
			t.Fatalf("failed publish count=%d err=%v", count, err)
		}
		failedAt := now
		now = failedAt.Add(delay - time.Millisecond)
		if count, err := worker.RunOnce(context.Background()); err != nil || count != 0 {
			t.Fatalf("claimed before retry delay %s elapsed: count=%d err=%v", delay, count, err)
		}
		now = failedAt.Add(delay)
	}
	if stats := worker.Stats(); stats.PublishFailures != 4 || stats.Published != 0 || stats.StoreFailures != 0 {
		t.Fatalf("stats=%+v", stats)
	}
}

func newOutboxStoreTest(t *testing.T) (*MongoOutboxStore, *mongotest.Collection) {
	t.Helper()
	mongoStore, client, _ := newMongoStoreTest(t)
	store, err := NewMongoOutboxStore(mongoStore)
	if err != nil {
		t.Fatal(err)
	}
	return store, client.Collection(testDatabase, OutboxCollection)
}

func seedOutboxEffect(t *testing.T, outbox *mongotest.Collection, id string, availableAt, createdAt time.Time, token int64) {
	t.Helper()
	if err := outbox.Seed(bson.M{
		"_id": id, "effect_id": id, "transaction_id": "tx-1", "topic": "hero.changed",
		"available_at": availableAt, "created_at": createdAt, "lease_token": token,
	}); err != nil {
		t.Fatal(err)
	}
}

// The claim path is a lease CAS: it must move the token forward and stamp the
// owner on the stored document, not merely return something.
func TestMongoOutboxStoreClaimTakesLeaseByTokenCAS(t *testing.T) {
	store, outbox := newOutboxStoreTest(t)
	now := time.Now().UTC()
	seedOutboxEffect(t, outbox, "effect-1", now.Add(-time.Second), now.Add(-time.Minute), 4)

	items, err := store.Claim(context.Background(), "worker-1", now, 8, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Effect.ID != "effect-1" {
		t.Fatalf("claimed=%+v", items)
	}
	if items[0].Lease != (OutboxLease{Owner: "worker-1", Token: 5}) {
		t.Fatalf("lease=%+v, want owner worker-1 token 5", items[0].Lease)
	}
	// A second worker arriving while the lease is live must claim nothing.
	again, err := store.Claim(context.Background(), "worker-2", now, 8, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 0 {
		t.Fatalf("live lease was stolen: %+v", again)
	}
	// After expiry it becomes claimable again, with the token moving forward.
	later := now.Add(2 * time.Minute)
	stolen, err := store.Claim(context.Background(), "worker-2", later, 8, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(stolen) != 1 || stolen[0].Lease != (OutboxLease{Owner: "worker-2", Token: 6}) {
		t.Fatalf("expired-lease claim=%+v", stolen)
	}
}

func TestMongoOutboxStoreSkipsEffectsNotYetAvailable(t *testing.T) {
	store, outbox := newOutboxStoreTest(t)
	now := time.Now().UTC()
	seedOutboxEffect(t, outbox, "later", now.Add(time.Minute), now, 1)
	items, err := store.Claim(context.Background(), "worker-1", now, 8, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 0 {
		t.Fatalf("claimed a delayed effect: %+v", items)
	}
}

func TestMongoOutboxStoreAckRequiresMatchingLease(t *testing.T) {
	store, outbox := newOutboxStoreTest(t)
	now := time.Now().UTC()
	seedOutboxEffect(t, outbox, "effect-1", now.Add(-time.Second), now, 4)
	items, err := store.Claim(context.Background(), "worker-1", now, 8, time.Minute)
	if err != nil || len(items) != 1 {
		t.Fatalf("claim items=%+v err=%v", items, err)
	}
	// A stale lease must not be able to remove another worker's claim.
	stale := OutboxLease{Owner: "worker-1", Token: items[0].Lease.Token - 1}
	if err := store.Ack(context.Background(), "effect-1", stale); !errors.Is(err, ErrOutboxLeaseConflict) {
		t.Fatalf("stale ack err=%v, want ErrOutboxLeaseConflict", err)
	}
	if err := store.Ack(context.Background(), "effect-1", items[0].Lease); err != nil {
		t.Fatal(err)
	}
	backlog, err := store.Backlog(context.Background(), now)
	if err != nil {
		t.Fatal(err)
	}
	if backlog.Pending != 0 {
		t.Fatalf("acked effect still pending: %+v", backlog)
	}
}

func TestMongoOutboxStoreNackReschedulesAndCountsAttempt(t *testing.T) {
	store, outbox := newOutboxStoreTest(t)
	now := time.Now().UTC()
	seedOutboxEffect(t, outbox, "effect-1", now.Add(-time.Second), now, 4)
	items, err := store.Claim(context.Background(), "worker-1", now, 8, time.Minute)
	if err != nil || len(items) != 1 {
		t.Fatalf("claim items=%+v err=%v", items, err)
	}
	next := now.Add(30 * time.Second)
	if err := store.Nack(context.Background(), "effect-1", items[0].Lease, next, "publish failed"); err != nil {
		t.Fatal(err)
	}
	// Nack must release the lease so the effect is claimable at `next`, and it
	// must not be claimable before then.
	early, err := store.Claim(context.Background(), "worker-1", now, 8, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(early) != 0 {
		t.Fatalf("nacked effect claimable before its retry time: %+v", early)
	}
	retried, err := store.Claim(context.Background(), "worker-1", next, 8, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(retried) != 1 || retried[0].Attempt != 1 || retried[0].LastError != "publish failed" {
		t.Fatalf("retried=%+v, want attempt 1 with the recorded error", retried)
	}
	// A stale lease must not be able to reschedule someone else's claim.
	stale := OutboxLease{Owner: "worker-1", Token: retried[0].Lease.Token - 1}
	if err := store.Nack(context.Background(), "effect-1", stale, next, "stale"); !errors.Is(err, ErrOutboxLeaseConflict) {
		t.Fatalf("stale nack err=%v, want ErrOutboxLeaseConflict", err)
	}
}

func TestMongoOutboxStoreBacklogReportsPendingAndOldestAge(t *testing.T) {
	store, outbox := newOutboxStoreTest(t)
	now := time.Now().UTC()
	seedOutboxEffect(t, outbox, "effect-old", now.Add(-time.Second), now.Add(-time.Hour), 1)
	seedOutboxEffect(t, outbox, "effect-new", now.Add(-time.Second), now.Add(-time.Minute), 1)

	backlog, err := store.Backlog(context.Background(), now)
	if err != nil {
		t.Fatal(err)
	}
	if backlog.Pending != 2 {
		t.Fatalf("pending=%d, want 2", backlog.Pending)
	}
	// BSON dates carry millisecond precision, exactly like real Mongo, so the
	// age is asserted within a tolerance rather than to the nanosecond.
	if drift := backlog.OldestAge - time.Hour; drift < -time.Second || drift > time.Second {
		t.Fatalf("oldest age=%s, want ~1h", backlog.OldestAge)
	}
	// An empty outbox reports a zero backlog rather than a stale age.
	items, err := store.Claim(context.Background(), "worker-1", now, 8, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range items {
		if err := store.Ack(context.Background(), item.Effect.ID, item.Lease); err != nil {
			t.Fatal(err)
		}
	}
	backlog, err = store.Backlog(context.Background(), now)
	if err != nil {
		t.Fatal(err)
	}
	if backlog.Pending != 0 || backlog.OldestAge != 0 {
		t.Fatalf("drained backlog=%+v", backlog)
	}
}

type successfulOutboxPublisher struct{ ids []string }

func (publisher *successfulOutboxPublisher) Publish(_ context.Context, item OutboxItem) error {
	publisher.ids = append(publisher.ids, item.Effect.ID)
	return nil
}

func TestOutboxWorkerAcknowledgesSuccessfulPublishByEffectID(t *testing.T) {
	store := newProjectorOutboxFake()
	store.pending["effect-1"] = OutboxItem{TransactionID: "tx-1", Effect: coredata.Effect{ID: "effect-1", Topic: "hero.changed"}}
	publisher := &successfulOutboxPublisher{}
	worker, err := NewOutboxWorker(store, publisher, OutboxWorkerOptions{Owner: "worker-1", BatchSize: 1, RetryMin: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	processed, err := worker.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if processed != 1 || len(publisher.ids) != 1 || publisher.ids[0] != "effect-1" {
		t.Fatalf("processed=%d ids=%v", processed, publisher.ids)
	}
	store.mu.Lock()
	pending := len(store.pending)
	store.mu.Unlock()
	if pending != 0 || worker.Stats().Published != 1 {
		t.Fatalf("pending=%d stats=%+v", pending, worker.Stats())
	}
}

func TestOutboxWorkerHardLimitFencesWithoutTreatingPublishFailureAsProjectionFailure(t *testing.T) {
	store := newProjectorOutboxFake()
	createdAt := time.Now().Add(-time.Hour)
	for _, id := range []string{"effect-1", "effect-2"} {
		store.pending[id] = OutboxItem{TransactionID: "tx-1", Effect: coredata.Effect{ID: id}, CreatedAt: createdAt}
	}
	fenced := make(chan error, 1)
	worker, err := NewOutboxWorker(store, failingOutboxPublisher{err: context.DeadlineExceeded}, OutboxWorkerOptions{
		Owner: "worker-1", BatchSize: 1, RetryMin: time.Millisecond,
		MaxPending: 1, MaxOldestAge: time.Minute, OnHardLimit: func(err error) { fenced <- err },
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := worker.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-fenced:
		if !errors.Is(err, ErrOutboxHardLimit) {
			t.Fatalf("fence err=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("outbox hard limit did not fence")
	}
	stats := worker.Stats()
	if stats.Pending != 2 || stats.OldestAge < time.Minute || stats.PublishFailures != 1 {
		t.Fatalf("stats=%+v", stats)
	}
}

// countingBacklogStore records how often the backlog was probed.
type countingBacklogStore struct {
	OutboxStore
	probes atomic.Int32
}

func (store *countingBacklogStore) Backlog(ctx context.Context, now time.Time) (OutboxBacklog, error) {
	store.probes.Add(1)
	return store.OutboxStore.Backlog(ctx, now)
}

// The probe costs a collection count plus an oldest-document lookup, so its
// cost grows with the very backlog it measures. Running it after every claim
// (once per worker per PollInterval) multiplied that against the same Mongo
// the projector writes to, so it is rate-limited — while a health check still
// gets a current reading on demand.
func TestOutboxWorkerRateLimitsBacklogProbe(t *testing.T) {
	inner, outbox := newOutboxStoreTest(t)
	now := time.Now().UTC()
	seedOutboxEffect(t, outbox, "effect-1", now.Add(-time.Second), now.Add(-time.Minute), 1)
	store := &countingBacklogStore{OutboxStore: inner}
	worker, err := NewOutboxWorker(store, &successfulOutboxPublisher{}, OutboxWorkerOptions{
		Owner: "worker-1", BatchSize: 8, BacklogInterval: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	worker.now = func() time.Time { return now }

	for range 5 {
		if _, err := worker.RunOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if probes := store.probes.Load(); probes != 1 {
		t.Fatalf("backlog probes=%d across 5 claim rounds, want 1", probes)
	}

	// A health check must not read a stale gauge.
	if err := worker.RefreshBacklog(context.Background()); err != nil {
		t.Fatal(err)
	}
	if probes := store.probes.Load(); probes != 2 {
		t.Fatalf("on-demand refresh did not sample: probes=%d", probes)
	}

	// Once the interval elapses the claim loop samples again.
	now = now.Add(2 * time.Hour)
	if _, err := worker.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if probes := store.probes.Load(); probes != 3 {
		t.Fatalf("probe did not resume after the interval: probes=%d", probes)
	}
}

// The oldest-document lookup sorts on created_at; without that index the sort
// scans the whole collection, which is exactly the wrong scaling for a probe
// that measures backlog size.
func TestOutboxInfrastructureIndexesCoverClaimAndBacklogQueries(t *testing.T) {
	store, client, _ := newMongoStoreTest(t)
	if err := store.EnsureInfrastructure(context.Background()); err != nil {
		t.Fatal(err)
	}
	outbox := client.Collection(testDatabase, OutboxCollection)
	for _, fields := range [][]string{
		{"available_at", "lease_until"},
		{"effect_id"},
		{"created_at"},
	} {
		if !outbox.HasIndex(fields...) {
			t.Fatalf("missing outbox index on %v: %+v", fields, outbox.Indexes)
		}
	}
}

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

func waitUntil(t *testing.T, timeout time.Duration, cond func() bool, what string) {
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

	waitUntil(t, 2*time.Second, func() bool { return worker.Stats().StoreFailures >= 5 }, "five failed polls")
	if got := logs.matching(slog.LevelWarn, "outbox claim loop failing"); got != 1 {
		t.Fatalf("warn lines after a failure streak = %d, want exactly 1", got)
	}

	store.mu.Lock()
	store.claimErr = nil
	store.mu.Unlock()
	waitUntil(t, 2*time.Second, func() bool { return logs.matching(slog.LevelInfo, "outbox claim loop recovered") == 1 }, "recovery line")
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
	message := HealthMessage(nestwal.Stats{}, ProjectorStats{}, OutboxWorkerStats{StoreFailures: 7, PublishFailures: 2})
	if !strings.Contains(message, "publish_failures=2") || !strings.Contains(message, "store_failures=7") {
		t.Fatalf("health message %q must report both publish and store failures", message)
	}
}

func requireBacklogGauge(t *testing.T, name string, want int64) {
	t.Helper()
	for _, s := range metrics.Snapshot() {
		if s.Name == name && s.Value == want {
			return
		}
	}
	t.Fatalf("backlog gauge %s=%d missing", name, want)
}
func TestProjectionAdmissionAndAckPublishBacklogGauge(t *testing.T) {
	metrics.DefaultRegistry().Reset()
	opts := nestwal.DefaultOptions(t.TempDir())
	w, err := nestwal.Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	p, err := NewProjector(w, newProjectorOutboxFake(), ProjectorOptions{CloseWAL: true, ManualReplay: true})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close(context.Background())
	r := projectorRecord(1, false)
	if err = p.reserve(r, true); err != nil {
		t.Fatal(err)
	}
	requireBacklogGauge(t, "dataengine.projection.pending", 1)
	p.discard(r.ID)
	requireBacklogGauge(t, "dataengine.projection.pending", 0)
}
func TestOutboxSamplingPublishesBacklogAndAge(t *testing.T) {
	metrics.DefaultRegistry().Reset()
	store := newProjectorOutboxFake()
	now := time.Now()
	store.pending["b7"] = OutboxItem{CreatedAt: now.Add(-time.Minute)}
	w, err := NewOutboxWorker(store, &successfulOutboxPublisher{}, OutboxWorkerOptions{Owner: "b7"})
	if err != nil {
		t.Fatal(err)
	}
	w.now = func() time.Time { return now }
	if err = w.RefreshBacklog(context.Background()); err != nil {
		t.Fatal(err)
	}
	requireBacklogGauge(t, "dataengine.outbox.pending", 1)
	requireBacklogGauge(t, "dataengine.outbox.oldest_age_ms", 60000)
	delete(store.pending, "b7")
	if err = w.RefreshBacklog(context.Background()); err != nil {
		t.Fatal(err)
	}
	requireBacklogGauge(t, "dataengine.outbox.pending", 0)
	requireBacklogGauge(t, "dataengine.outbox.oldest_age_ms", 0)
}
