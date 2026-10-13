package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
)

// ErrOutboxHardLimit 表示 outbox 积压条数或最老年龄超过硬上限；首次触发时调用 OnHardLimit。
var ErrOutboxHardLimit = errors.New("dataengine outbox: hard backlog limit exceeded")

// OutboxPublisher 把暂存的 effect 投递到消息代理。
type OutboxPublisher interface {
	// Publish 必须以 item.Effect.ID 作为代理侧去重键（MsgID）。
	Publish(context.Context, OutboxItem) error
}

// OutboxWorkerOptions 配置 outbox 认领循环；零值字段在 NewOutboxWorker 中补默认值。
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
	// BacklogInterval 限制积压采样频率，零值为 1 秒。一次采样是一次集合计数加一次
	// 最老文档查询；每次认领都采样（默认配置下每秒约 20 次）会让测量开销随积压增长。
	BacklogInterval time.Duration
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

// OutboxWorker 以租约认领 outbox 中到期的 effect 并投递：成功 Ack 删除，失败 Nack 退避重试。
// 投递失败不影响实体写入；积压超过硬上限只报告健康错误并回调一次 OnHardLimit。
type OutboxWorker struct {
	store     OutboxStore
	publisher OutboxPublisher
	opts      OutboxWorkerOptions
	now       func() time.Time

	claimed          atomic.Uint64
	published        atomic.Uint64
	publishFailures  atomic.Uint64
	storeFailures    atomic.Uint64
	failing          atomic.Bool
	pending          atomic.Int64
	oldestAgeNanos   atomic.Int64
	backlogSampledAt atomic.Int64
	hardMu           sync.RWMutex
	hardErr          error
	hardOnce         sync.Once

	// 启动和关闭共享同一把锁，避免 Close 尚未看到 cancel 时提前关闭 done。
	// closed 是终态；done 只能由未启动的 Close 或已启动的 run 之一关闭。
	lifecycleMu sync.Mutex
	cancel      context.CancelFunc
	done        chan struct{}
	closed      bool
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
	if options.BacklogInterval <= 0 {
		options.BacklogInterval = time.Second
	}
	return &OutboxWorker{store: store, publisher: publisher, opts: options, now: time.Now, done: make(chan struct{})}, nil
}

// RunOnce 认领并投递一批 effect。投递失败经 Nack 持久化退避时间，不让本次调用失败，
// 也不阻挡实体写入；只有存储（Claim/Ack/Nack/积压采样）错误才返回。
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
			// 退避从本次失败返回时开始；整批开始时间可能已被慢发布耗尽。
			next := worker.now().UTC().Add(worker.retryDelay(item.Attempt))
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
	if err := worker.maybeRefreshBacklog(ctx, worker.now().UTC()); err != nil {
		worker.storeFailures.Add(1)
		return len(items), err
	}
	return len(items), nil
}

// maybeRefreshBacklog 在全部 worker 之间限流积压采样：每个 BacklogInterval 一次足够
// 驱动指标与硬上限；逐次认领采样只会加重投影器正在写的同一个 Mongo 的负载。
func (worker *OutboxWorker) maybeRefreshBacklog(ctx context.Context, now time.Time) error {
	last := worker.backlogSampledAt.Load()
	if last != 0 && now.UnixNano()-last < int64(worker.opts.BacklogInterval) {
		return nil
	}
	if !worker.backlogSampledAt.CompareAndSwap(last, now.UnixNano()) {
		// 另一个 worker 正在采样这一轮。
		return nil
	}
	return worker.refreshBacklog(ctx, now)
}

// RefreshBacklog 绕过采样间隔立即采样；健康检查与关闭统计需要当前读数，
// 不能用可能过期一整个间隔的旧值。
func (worker *OutboxWorker) RefreshBacklog(ctx context.Context) error {
	if worker == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	now := worker.now().UTC()
	worker.backlogSampledAt.Store(now.UnixNano())
	if err := worker.refreshBacklog(ctx, now); err != nil {
		worker.storeFailures.Add(1)
		return err
	}
	return nil
}

func (worker *OutboxWorker) refreshBacklog(ctx context.Context, now time.Time) error {
	backlog, err := worker.store.Backlog(ctx, now)
	if err != nil {
		return err
	}
	worker.pending.Store(backlog.Pending)
	worker.oldestAgeNanos.Store(int64(backlog.OldestAge))
	metrics.SetGauge("dataengine.outbox.pending", nil, backlog.Pending)
	metrics.SetGauge("dataengine.outbox.oldest_age_ms", nil, backlog.OldestAge.Milliseconds())
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

// Start 启动 Workers 个认领循环；已启动或已关闭时不做任何事。
func (worker *OutboxWorker) Start(parent context.Context) {
	worker.lifecycleMu.Lock()
	defer worker.lifecycleMu.Unlock()
	if worker.closed || worker.cancel != nil {
		return
	}
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancel(parent)
	worker.cancel = cancel
	go worker.run(ctx)
}

func (worker *OutboxWorker) run(ctx context.Context) {
	defer close(worker.done)
	var group sync.WaitGroup
	for range worker.opts.Workers {
		group.Go(func() {
			ticker := time.NewTicker(worker.opts.PollInterval)
			defer ticker.Stop()
			for {
				if _, err := worker.RunOnce(ctx); err != nil {
					worker.noteRunFailure(ctx, err)
				} else {
					worker.noteRunRecovered()
				}
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
				}
			}
		})
	}
	group.Wait()
}

// Close 停止认领循环并等待其退出；ctx 到期返回 ctx 错误，可再次调用继续等待。
// 已认领未投递的 effect 留在 Mongo，租约到期后由下次启动重新认领。
func (worker *OutboxWorker) Close(ctx context.Context) error {
	worker.lifecycleMu.Lock()
	if !worker.closed {
		worker.closed = true
		if worker.cancel != nil {
			worker.cancel()
		} else {
			close(worker.done)
		}
	}
	worker.lifecycleMu.Unlock()
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

// noteRunFailure 记录一次失败的轮询。数量由计数器承载，日志只记录状态变化（连续失败
// 开始一行、恢复一行）：按轮询频率逐条记录会淹没运维真正需要的“从何时起、到何时止”。
// 因关闭而中断的轮询不算失败。
func (worker *OutboxWorker) noteRunFailure(ctx context.Context, err error) {
	if ctx.Err() != nil && errors.Is(err, ctx.Err()) {
		return
	}
	if worker.failing.CompareAndSwap(false, true) {
		slog.Warn("dataengine: outbox claim loop failing; effects stay queued until the store answers again", "owner", worker.opts.Owner, "err", err)
	}
}

func (worker *OutboxWorker) noteRunRecovered() {
	if worker.failing.CompareAndSwap(true, false) {
		slog.Info("dataengine: outbox claim loop recovered", "owner", worker.opts.Owner, "store_failures", worker.storeFailures.Load())
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
