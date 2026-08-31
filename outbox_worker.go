package dataengine

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

var ErrOutboxHardLimit = errors.New("dataengine outbox: hard backlog limit exceeded")

type OutboxPublisher interface {
	// Publish must use item.Effect.ID as the broker deduplication/MsgID key.
	Publish(context.Context, OutboxItem) error
}

type OutboxWorkerOptions struct {
	Owner         string
	Workers       int
	BatchSize     int
	LeaseDuration time.Duration
	PollInterval  time.Duration
	RetryMin      time.Duration
	RetryMax      time.Duration
	MaxPending    int64
	MaxOldestAge  time.Duration
	OnHardLimit   func(error)
}

type OutboxWorkerStats struct {
	Claimed         uint64
	Published       uint64
	PublishFailures uint64
	StoreFailures   uint64
	Pending         int64
	OldestAge       time.Duration
	HardLimitError  string
}

type OutboxWorker struct {
	store     OutboxStore
	publisher OutboxPublisher
	opts      OutboxWorkerOptions
	now       func() time.Time

	claimed         atomic.Uint64
	published       atomic.Uint64
	publishFailures atomic.Uint64
	storeFailures   atomic.Uint64
	pending         atomic.Int64
	oldestAgeNanos  atomic.Int64
	hardMu          sync.RWMutex
	hardErr         error
	hardOnce        sync.Once

	cancel    context.CancelFunc
	done      chan struct{}
	startOnce sync.Once
	closeOnce sync.Once
}

func NewOutboxWorker(store OutboxStore, publisher OutboxPublisher, options OutboxWorkerOptions) (*OutboxWorker, error) {
	if store == nil || publisher == nil {
		return nil, errors.New("dataengine outbox: store and publisher are required")
	}
	if options.Owner == "" {
		return nil, errors.New("dataengine outbox: worker owner is required")
	}
	if options.Workers <= 0 {
		options.Workers = 1
	}
	if options.BatchSize <= 0 {
		options.BatchSize = 64
	}
	if options.LeaseDuration <= 0 {
		options.LeaseDuration = 30 * time.Second
	}
	if options.PollInterval <= 0 {
		options.PollInterval = 100 * time.Millisecond
	}
	if options.RetryMin <= 0 {
		options.RetryMin = time.Second
	}
	if options.RetryMax <= 0 {
		options.RetryMax = time.Minute
	}
	if options.RetryMax < options.RetryMin {
		return nil, errors.New("dataengine outbox: retry max is smaller than retry min")
	}
	return &OutboxWorker{store: store, publisher: publisher, opts: options, now: time.Now, done: make(chan struct{})}, nil
}

// RunOnce claims a bounded batch. Publish failures are persisted through Nack
// and intentionally do not fail the call or fence entity writes.
func (worker *OutboxWorker) RunOnce(ctx context.Context) (int, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	now := worker.now().UTC()
	items, err := worker.store.Claim(ctx, worker.opts.Owner, now, worker.opts.BatchSize, worker.opts.LeaseDuration)
	if err != nil {
		worker.storeFailures.Add(1)
		return 0, err
	}
	worker.claimed.Add(uint64(len(items)))
	for i := range items {
		item := items[i]
		if err := worker.publisher.Publish(ctx, item); err != nil {
			worker.publishFailures.Add(1)
			next := now.Add(worker.retryDelay(item.Attempt))
			if nackErr := worker.store.Nack(ctx, item.Effect.ID, item.Lease, next, err.Error()); nackErr != nil {
				worker.storeFailures.Add(1)
				return i, nackErr
			}
			continue
		}
		if err := worker.store.Ack(ctx, item.Effect.ID, item.Lease); err != nil {
			worker.storeFailures.Add(1)
			return i, err
		}
		worker.published.Add(1)
	}
	if err := worker.refreshBacklog(ctx, worker.now().UTC()); err != nil {
		worker.storeFailures.Add(1)
		return len(items), err
	}
	return len(items), nil
}

func (worker *OutboxWorker) refreshBacklog(ctx context.Context, now time.Time) error {
	backlog, err := worker.store.Backlog(ctx, now)
	if err != nil {
		return err
	}
	worker.pending.Store(backlog.Pending)
	worker.oldestAgeNanos.Store(int64(backlog.OldestAge))
	var limitErr error
	if worker.opts.MaxPending > 0 && backlog.Pending > worker.opts.MaxPending {
		limitErr = fmt.Errorf("%w: pending=%d max=%d", ErrOutboxHardLimit, backlog.Pending, worker.opts.MaxPending)
	}
	if worker.opts.MaxOldestAge > 0 && backlog.OldestAge > worker.opts.MaxOldestAge {
		limitErr = errors.Join(limitErr, fmt.Errorf("%w: oldest_age=%s max=%s", ErrOutboxHardLimit, backlog.OldestAge, worker.opts.MaxOldestAge))
	}
	if limitErr != nil {
		worker.hardOnce.Do(func() {
			worker.hardMu.Lock()
			worker.hardErr = limitErr
			worker.hardMu.Unlock()
			if worker.opts.OnHardLimit != nil {
				worker.opts.OnHardLimit(limitErr)
			}
		})
	}
	return nil
}

func (worker *OutboxWorker) retryDelay(attempt uint32) time.Duration {
	delay := worker.opts.RetryMin
	for i := uint32(0); i < attempt && delay < worker.opts.RetryMax; i++ {
		if delay > worker.opts.RetryMax/2 {
			return worker.opts.RetryMax
		}
		delay *= 2
	}
	if delay > worker.opts.RetryMax {
		return worker.opts.RetryMax
	}
	return delay
}

func (worker *OutboxWorker) Start(parent context.Context) {
	worker.startOnce.Do(func() {
		if parent == nil {
			parent = context.Background()
		}
		ctx, cancel := context.WithCancel(parent)
		worker.cancel = cancel
		go worker.run(ctx)
	})
}

func (worker *OutboxWorker) run(ctx context.Context) {
	defer close(worker.done)
	var group sync.WaitGroup
	for range worker.opts.Workers {
		group.Add(1)
		go func() {
			defer group.Done()
			ticker := time.NewTicker(worker.opts.PollInterval)
			defer ticker.Stop()
			for {
				_, _ = worker.RunOnce(ctx)
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
				}
			}
		}()
	}
	group.Wait()
}

func (worker *OutboxWorker) Close(ctx context.Context) error {
	worker.closeOnce.Do(func() {
		if worker.cancel != nil {
			worker.cancel()
		} else {
			close(worker.done)
		}
	})
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-worker.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (worker *OutboxWorker) Stats() OutboxWorkerStats {
	stats := OutboxWorkerStats{
		Claimed: worker.claimed.Load(), Published: worker.published.Load(),
		PublishFailures: worker.publishFailures.Load(), StoreFailures: worker.storeFailures.Load(),
		Pending: worker.pending.Load(), OldestAge: time.Duration(worker.oldestAgeNanos.Load()),
	}
	worker.hardMu.RLock()
	if worker.hardErr != nil {
		stats.HardLimitError = worker.hardErr.Error()
	}
	worker.hardMu.RUnlock()
	return stats
}

func (worker *OutboxWorker) Healthy() error {
	worker.hardMu.RLock()
	err := worker.hardErr
	worker.hardMu.RUnlock()
	return err
}
