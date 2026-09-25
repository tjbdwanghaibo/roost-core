package engine

import (
	"context"
	"testing"
	"time"
)

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
