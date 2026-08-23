package nestwal

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	corenest "github.com/tjbdwanghaibo/cube-core/nest"
)

var (
	ErrMutationApplierRequired = errors.New("nestwal: mutation applier is required")
	ErrEffectPublisherRequired = errors.New("nestwal: effect publisher is required")
	errTransactionHeld         = errors.New("nestwal: transaction is still under entity lock")
	errReplayBatchComplete     = errors.New("nestwal: replay batch complete")
)

// MutationApplier must be idempotent for (transactionID, mutations). The slice
// contains the complete multi-entity transaction and lets a production backend
// apply it with a native database transaction when externally visible atomicity
// is required.
type MutationApplier interface {
	ApplyMutations(context.Context, corenest.TransactionID, []corenest.EntityMutation) error
}

// TransactionReceiptCleaner is called only after the WAL acknowledgement has
// durably advanced past every supplied transaction.
type TransactionReceiptCleaner interface {
	AcknowledgeTransactions(context.Context, []corenest.TransactionID) error
}

type MutationApplyFunc func(context.Context, corenest.TransactionID, corenest.EntityMutation) error

func (fn MutationApplyFunc) ApplyMutations(ctx context.Context, txID corenest.TransactionID, mutations []corenest.EntityMutation) error {
	for i := range mutations {
		if err := fn(ctx, txID, mutations[i]); err != nil {
			return err
		}
	}
	return nil
}

// EffectPublisher must deduplicate Effect.ID. Delivery is ordered and
// at-least-once, including across acknowledgement write failures.
type EffectPublisher interface {
	PublishEffect(context.Context, corenest.TransactionID, corenest.Effect) error
}

type EffectPublishFunc func(context.Context, corenest.TransactionID, corenest.Effect) error

func (fn EffectPublishFunc) PublishEffect(ctx context.Context, txID corenest.TransactionID, effect corenest.Effect) error {
	return fn(ctx, txID, effect)
}

type CommitterOptions struct {
	RetryMin               time.Duration
	RetryMax               time.Duration
	IdlePoll               time.Duration
	CloseWAL               bool
	ReplayBatchRecords     int
	ReceiptCleanupBatch    int
	ReceiptCleanupCapacity int
}

func DefaultCommitterOptions() CommitterOptions {
	return CommitterOptions{
		RetryMin:               10 * time.Millisecond,
		RetryMax:               5 * time.Second,
		IdlePoll:               time.Second,
		CloseWAL:               true,
		ReplayBatchRecords:     256,
		ReceiptCleanupBatch:    256,
		ReceiptCleanupCapacity: 65536,
	}
}

type CommitterStats struct {
	Committed              uint64
	Applied                uint64
	Published              uint64
	ReplayFailures         uint64
	LastError              string
	PendingReceiptCleanup  int
	ReceiptCleanupFailures uint64
	ReceiptCleanupDropped  uint64
}

// Committer implements core Nest's TransactionCommitter and owns the
// post-commit replay loop. Construction starts recovery immediately.
type Committer struct {
	wal       *WAL
	applier   MutationApplier
	publisher EffectPublisher
	opts      CommitterOptions

	ctx       context.Context
	cancel    context.CancelFunc
	kick      chan struct{}
	done      chan struct{}
	closeOnce sync.Once

	flushMu           sync.Mutex
	heldMu            sync.RWMutex
	held              map[corenest.TransactionID]struct{}
	errMu             sync.RWMutex
	lastErr           error
	cleanupErr        error
	receiptMu         sync.Mutex
	receiptCleanupMu  sync.Mutex
	pendingReceipts   []corenest.TransactionID
	pendingReceiptSet map[corenest.TransactionID]struct{}

	committed       atomic.Uint64
	applied         atomic.Uint64
	published       atomic.Uint64
	failures        atomic.Uint64
	cleanupFailures atomic.Uint64
	cleanupDropped  atomic.Uint64
}

func NewCommitter(wal *WAL, applier MutationApplier, publisher EffectPublisher, options CommitterOptions) (*Committer, error) {
	if wal == nil {
		return nil, errors.New("nestwal: nil WAL")
	}
	defaults := DefaultCommitterOptions()
	if options.RetryMin <= 0 {
		options.RetryMin = defaults.RetryMin
	}
	if options.RetryMax <= 0 {
		options.RetryMax = defaults.RetryMax
	}
	if options.RetryMax < options.RetryMin {
		return nil, errors.New("nestwal: retry max is smaller than retry min")
	}
	if options.IdlePoll <= 0 {
		options.IdlePoll = defaults.IdlePoll
	}
	if options.ReplayBatchRecords <= 0 {
		options.ReplayBatchRecords = defaults.ReplayBatchRecords
	}
	if options.ReceiptCleanupBatch <= 0 {
		options.ReceiptCleanupBatch = defaults.ReceiptCleanupBatch
	}
	if options.ReceiptCleanupCapacity <= 0 {
		options.ReceiptCleanupCapacity = defaults.ReceiptCleanupCapacity
	}
	ctx, cancel := context.WithCancel(context.Background())
	c := &Committer{
		wal:               wal,
		applier:           applier,
		publisher:         publisher,
		opts:              options,
		ctx:               ctx,
		cancel:            cancel,
		kick:              make(chan struct{}, 1),
		done:              make(chan struct{}),
		held:              make(map[corenest.TransactionID]struct{}),
		pendingReceiptSet: make(map[corenest.TransactionID]struct{}),
	}
	go c.run()
	c.signal()
	return c, nil
}

func (c *Committer) Commit(ctx context.Context, record corenest.CommitRecord) error {
	if c == nil || c.wal == nil {
		return errors.New("nestwal: committer is not initialized")
	}
	c.hold(record.ID)
	if _, err := c.wal.Append(ctx, record); err != nil {
		c.release(record.ID)
		return err
	}
	c.committed.Add(1)
	c.signal()
	return nil
}

// TransactionReleased is called by core Nest after the entity guard has
// released every lock involved in the transaction.
func (c *Committer) TransactionReleased(id corenest.TransactionID) {
	c.release(id)
	c.signal()
}

// Flush actively applies and acknowledges every record visible when each
// replay pass starts. Concurrent commits are included until a complete empty
// pass is observed or the context expires.
func (c *Committer) Flush(ctx context.Context) error {
	if c == nil || c.wal == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	c.flushMu.Lock()
	defer c.flushMu.Unlock()
	if err := c.wal.Sync(ctx); err != nil {
		return err
	}
	for {
		processed, err := c.replayPass(ctx)
		if err != nil {
			c.setLastError(err)
			return err
		}
		if processed == 0 {
			if err := c.flushReceiptCleanup(ctx); err != nil {
				return err
			}
			c.setLastError(nil)
			return nil
		}
	}
}

func (c *Committer) Stats() CommitterStats {
	stats := CommitterStats{
		Committed:              c.committed.Load(),
		Applied:                c.applied.Load(),
		Published:              c.published.Load(),
		ReplayFailures:         c.failures.Load(),
		ReceiptCleanupFailures: c.cleanupFailures.Load(),
		ReceiptCleanupDropped:  c.cleanupDropped.Load(),
	}
	c.receiptMu.Lock()
	stats.PendingReceiptCleanup = len(c.pendingReceipts)
	c.receiptMu.Unlock()
	c.errMu.RLock()
	if c.lastErr != nil {
		stats.LastError = c.lastErr.Error()
	} else if c.cleanupErr != nil {
		stats.LastError = c.cleanupErr.Error()
	}
	c.errMu.RUnlock()
	return stats
}

func (c *Committer) Healthy() error {
	if c == nil || c.wal == nil {
		return errors.New("nestwal: committer is not initialized")
	}
	if err := c.wal.Healthy(); err != nil {
		return err
	}
	c.errMu.RLock()
	defer c.errMu.RUnlock()
	return errors.Join(c.lastErr, c.cleanupErr)
}

func (c *Committer) Close(ctx context.Context) error {
	if c == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	c.closeOnce.Do(c.cancel)
	select {
	case <-c.done:
		if c.opts.CloseWAL {
			return c.wal.Close(ctx)
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Shutdown performs a delivery flush before stopping the recovery worker and
// closing the WAL. Applications should call it after they stop accepting new
// Nest requests and after all entity guards have drained.
func (c *Committer) Shutdown(ctx context.Context) error {
	flushErr := c.Flush(ctx)
	closeErr := c.Close(ctx)
	return errors.Join(flushErr, closeErr)
}

func (c *Committer) run() {
	defer close(c.done)
	backoff := c.opts.RetryMin
	for {
		processed, err := c.replayPass(c.ctx)
		if errors.Is(err, errTransactionHeld) {
			backoff = c.opts.RetryMin
			select {
			case <-c.kick:
			case <-c.ctx.Done():
				return
			case <-time.After(c.opts.RetryMin):
			}
			continue
		}
		if err != nil && !errors.Is(err, context.Canceled) {
			c.failures.Add(1)
			c.setLastError(err)
			if !waitContext(c.ctx, jitter(backoff)) {
				return
			}
			backoff *= 2
			if backoff > c.opts.RetryMax {
				backoff = c.opts.RetryMax
			}
			continue
		}
		if c.ctx.Err() != nil {
			return
		}
		c.setLastError(nil)
		backoff = c.opts.RetryMin
		if processed > 0 {
			continue
		}
		timer := time.NewTimer(c.opts.IdlePoll)
		select {
		case <-c.kick:
			if !timer.Stop() {
				<-timer.C
			}
		case <-timer.C:
		case <-c.ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return
		}
	}
}

func (c *Committer) replayPass(ctx context.Context) (int, error) {
	// Receipt deletion is off the commit hot path, but every failed deletion is
	// retained in a bounded retry set and retried on each replay pass.
	_ = c.retryReceiptCleanup(ctx)
	processed := 0
	var lastFence corenest.CommitFence
	processedIDs := make([]corenest.TransactionID, 0, c.opts.ReplayBatchRecords)
	err := c.wal.Replay(ctx, func(fence corenest.CommitFence, record corenest.CommitRecord) error {
		if c.isHeld(record.ID) {
			return errTransactionHeld
		}
		if len(record.Mutations) > 0 && c.applier == nil {
			return ErrMutationApplierRequired
		}
		if len(record.Effects) > 0 && c.publisher == nil {
			return ErrEffectPublisherRequired
		}
		if len(record.Mutations) > 0 {
			if err := c.applier.ApplyMutations(ctx, record.ID, record.Mutations); err != nil {
				return fmt.Errorf("nestwal: apply transaction %s: %w", record.ID.String(), err)
			}
			c.applied.Add(uint64(len(record.Mutations)))
		}
		for i := range record.Effects {
			if err := c.publisher.PublishEffect(ctx, record.ID, record.Effects[i]); err != nil {
				return fmt.Errorf("nestwal: publish transaction %s effect %q: %w", record.ID.String(), record.Effects[i].ID, err)
			}
			c.published.Add(1)
		}
		processed++
		processedIDs = append(processedIDs, record.ID)
		lastFence = fence
		if processed >= c.opts.ReplayBatchRecords {
			return errReplayBatchComplete
		}
		return nil
	})
	if errors.Is(err, errReplayBatchComplete) {
		err = nil
	}
	if processed > 0 {
		if ackErr := c.wal.Ack(ctx, lastFence); ackErr != nil {
			return processed, errors.Join(err, ackErr)
		}
		if _, ok := c.applier.(TransactionReceiptCleaner); ok {
			c.enqueueReceiptCleanup(processedIDs)
			_ = c.retryReceiptCleanup(ctx)
		}
	}
	return processed, err
}

func (c *Committer) enqueueReceiptCleanup(ids []corenest.TransactionID) {
	if len(ids) == 0 {
		return
	}
	c.receiptMu.Lock()
	defer c.receiptMu.Unlock()
	for _, id := range ids {
		if _, exists := c.pendingReceiptSet[id]; exists {
			continue
		}
		if len(c.pendingReceipts) >= c.opts.ReceiptCleanupCapacity {
			c.cleanupDropped.Add(1)
			c.setCleanupError(fmt.Errorf("nestwal: transaction receipt cleanup queue is full"))
			continue
		}
		c.pendingReceiptSet[id] = struct{}{}
		c.pendingReceipts = append(c.pendingReceipts, id)
	}
}

func (c *Committer) retryReceiptCleanup(ctx context.Context) error {
	c.receiptCleanupMu.Lock()
	defer c.receiptCleanupMu.Unlock()
	cleaner, ok := c.applier.(TransactionReceiptCleaner)
	if !ok {
		return nil
	}
	c.receiptMu.Lock()
	n := min(len(c.pendingReceipts), c.opts.ReceiptCleanupBatch)
	batch := append([]corenest.TransactionID(nil), c.pendingReceipts[:n]...)
	c.receiptMu.Unlock()
	if len(batch) == 0 {
		c.setCleanupError(nil)
		return nil
	}
	if err := cleaner.AcknowledgeTransactions(ctx, batch); err != nil {
		c.cleanupFailures.Add(1)
		wrapped := fmt.Errorf("nestwal: transaction receipt cleanup: %w", err)
		c.setCleanupError(wrapped)
		slog.Warn("nestwal: transaction receipt cleanup failed", "err", err, "count", len(batch))
		return wrapped
	}
	c.receiptMu.Lock()
	for _, id := range batch {
		delete(c.pendingReceiptSet, id)
	}
	c.pendingReceipts = c.pendingReceipts[len(batch):]
	c.receiptMu.Unlock()
	c.setCleanupError(nil)
	return nil
}

func (c *Committer) flushReceiptCleanup(ctx context.Context) error {
	for {
		c.receiptMu.Lock()
		pending := len(c.pendingReceipts)
		c.receiptMu.Unlock()
		if pending == 0 {
			return nil
		}
		if err := c.retryReceiptCleanup(ctx); err != nil {
			return err
		}
	}
}

func (c *Committer) hold(id corenest.TransactionID) {
	c.heldMu.Lock()
	c.held[id] = struct{}{}
	c.heldMu.Unlock()
}

func (c *Committer) release(id corenest.TransactionID) {
	c.heldMu.Lock()
	delete(c.held, id)
	c.heldMu.Unlock()
}

func (c *Committer) isHeld(id corenest.TransactionID) bool {
	c.heldMu.RLock()
	_, ok := c.held[id]
	c.heldMu.RUnlock()
	return ok
}

func (c *Committer) signal() {
	select {
	case c.kick <- struct{}{}:
	default:
	}
}

func (c *Committer) setLastError(err error) {
	c.errMu.Lock()
	c.lastErr = err
	c.errMu.Unlock()
}

func (c *Committer) setCleanupError(err error) {
	c.errMu.Lock()
	c.cleanupErr = err
	c.errMu.Unlock()
}

func waitContext(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

func jitter(duration time.Duration) time.Duration {
	if duration <= 1 {
		return duration
	}
	half := duration / 2
	return half + time.Duration(rand.Int64N(int64(duration-half)))
}

var _ corenest.TransactionCommitter = (*Committer)(nil)
var _ corenest.TransactionReleaseNotifier = (*Committer)(nil)
