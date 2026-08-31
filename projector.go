package dataengine

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	coredata "github.com/tjbdwanghaibo/cube-core/dataengine"
	corenest "github.com/tjbdwanghaibo/cube-core/nest"
	"github.com/tjbdwanghaibo/cube-kit/nestwal"
)

var (
	errProjectorTransactionHeld = errors.New("dataengine projector: transaction is still under entity lock")
	errProjectorBatchComplete   = errors.New("dataengine projector: replay batch complete")
)

type ProjectionStore interface {
	Project(context.Context, coredata.CommitRecord) error
}

type ProjectorOptions struct {
	RetryMin           time.Duration
	RetryMax           time.Duration
	IdlePoll           time.Duration
	ReplayBatchRecords int
	CloseWAL           bool
	OnFatal            func(error)
}

func DefaultProjectorOptions() ProjectorOptions {
	return ProjectorOptions{
		RetryMin: 10 * time.Millisecond, RetryMax: 5 * time.Second,
		IdlePoll: time.Second, ReplayBatchRecords: 256, CloseWAL: true,
	}
}

type ProjectorStats struct {
	Committed                uint64
	Projected                uint64
	WALUnacked               uint64
	ProjectionFailures       uint64
	FatalProjectionConflicts uint64
	LastError                string
}

// Projector owns durable admission and WAL -> Mongo projection. Effects are
// only staged by ProjectionStore; broker delivery is owned by OutboxWorker and
// is deliberately absent from the WAL acknowledgement path.
type Projector struct {
	wal   *nestwal.WAL
	store ProjectionStore
	opts  ProjectorOptions

	ctx       context.Context
	cancel    context.CancelFunc
	kick      chan struct{}
	done      chan struct{}
	closeOnce sync.Once
	flushMu   sync.Mutex

	heldMu    sync.RWMutex
	held      map[coredata.TransactionID]struct{}
	admitted  map[coredata.TransactionID]struct{}
	errMu     sync.RWMutex
	lastErr   error
	fatalErr  error
	fatalOnce sync.Once
	ticketMu  sync.Mutex
	tickets   map[coredata.TransactionID]*projectionTicket

	committed      atomic.Uint64
	projected      atomic.Uint64
	walUnacked     atomic.Uint64
	failures       atomic.Uint64
	fatalConflicts atomic.Uint64
}

func NewProjector(wal *nestwal.WAL, store ProjectionStore, options ProjectorOptions) (*Projector, error) {
	if wal == nil || store == nil {
		return nil, errors.New("dataengine projector: WAL and store are required")
	}
	defaults := DefaultProjectorOptions()
	if options.RetryMin <= 0 {
		options.RetryMin = defaults.RetryMin
	}
	if options.RetryMax <= 0 {
		options.RetryMax = defaults.RetryMax
	}
	if options.RetryMax < options.RetryMin {
		return nil, errors.New("dataengine projector: retry max is smaller than retry min")
	}
	if options.IdlePoll <= 0 {
		options.IdlePoll = defaults.IdlePoll
	}
	if options.ReplayBatchRecords <= 0 {
		options.ReplayBatchRecords = defaults.ReplayBatchRecords
	}
	ctx, cancel := context.WithCancel(context.Background())
	projector := &Projector{
		wal: wal, store: store, opts: options, ctx: ctx, cancel: cancel,
		kick: make(chan struct{}, 1), done: make(chan struct{}), held: make(map[coredata.TransactionID]struct{}), admitted: make(map[coredata.TransactionID]struct{}),
		tickets: make(map[coredata.TransactionID]*projectionTicket),
	}
	go projector.run()
	projector.signal()
	return projector, nil
}

type projectionTicket struct {
	done chan struct{}
	err  error
}

func (ticket *projectionTicket) Done() <-chan struct{} { return ticket.done }
func (ticket *projectionTicket) Err() error {
	select {
	case <-ticket.done:
		return ticket.err
	default:
		return nil
	}
}

// CommitSystem durably admits an infrastructure mutation and returns a ticket
// that resolves only after Mongo projection, not merely after WAL fsync.
func (projector *Projector) CommitSystem(ctx context.Context, record coredata.CommitRecord) (coredata.ProjectionTicket, error) {
	if projector == nil || projector.wal == nil {
		return nil, errors.New("dataengine projector: not initialized")
	}
	if fatal := projector.fatal(); fatal != nil {
		return nil, fatal
	}
	if record.Durability == corenest.DurabilityMemory {
		record.Durability = corenest.DurabilityStrict
	}
	ticket := &projectionTicket{done: make(chan struct{})}
	projector.ticketMu.Lock()
	if _, duplicate := projector.tickets[record.ID]; duplicate {
		projector.ticketMu.Unlock()
		return nil, fmt.Errorf("dataengine projector: duplicate system transaction %s", record.ID.String())
	}
	projector.tickets[record.ID] = ticket
	projector.ticketMu.Unlock()
	projector.admitSystem(record.ID)
	if _, err := projector.wal.Append(ctx, record); err != nil {
		projector.discard(record.ID)
		projector.removeTicket(record.ID)
		return nil, err
	}
	projector.committed.Add(1)
	projector.signal()
	return ticket, nil
}

func (projector *Projector) Commit(ctx context.Context, record corenest.CommitRecord) error {
	if projector == nil || projector.wal == nil {
		return errors.New("dataengine projector: not initialized")
	}
	if fatal := projector.fatal(); fatal != nil {
		return fatal
	}
	projector.admit(record.ID)
	if _, err := projector.wal.Append(ctx, record); err != nil {
		projector.discard(record.ID)
		return err
	}
	projector.committed.Add(1)
	projector.signal()
	return nil
}

func (projector *Projector) Enqueue(ctx context.Context, record corenest.CommitRecord) (corenest.CommitTicket, error) {
	if projector == nil || projector.wal == nil {
		return nil, errors.New("dataengine projector: not initialized")
	}
	if fatal := projector.fatal(); fatal != nil {
		return nil, fatal
	}
	projector.admit(record.ID)
	ticket, err := projector.wal.Enqueue(ctx, record)
	if err != nil {
		projector.discard(record.ID)
		return nil, err
	}
	projector.committed.Add(1)
	return ticket, nil
}

func (projector *Projector) DurableLSN() uint64 {
	if projector == nil || projector.wal == nil {
		return 0
	}
	return projector.wal.DurableLSN()
}

func (projector *Projector) TransactionReleased(id corenest.TransactionID) {
	projector.release(id)
	projector.signal()
}

func (projector *Projector) Flush(ctx context.Context) error {
	if projector == nil || projector.wal == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	projector.flushMu.Lock()
	defer projector.flushMu.Unlock()
	if fatal := projector.fatal(); fatal != nil {
		return fatal
	}
	if err := projector.wal.Sync(ctx); err != nil {
		return err
	}
	for {
		processed, err := projector.replayPass(ctx)
		if err != nil {
			projector.recordFailure(err)
			return err
		}
		if processed == 0 {
			projector.setLastError(nil)
			return nil
		}
	}
}

func (projector *Projector) replayPass(ctx context.Context) (int, error) {
	processed := 0
	var lastFence corenest.CommitFence
	processedIDs := make([]coredata.TransactionID, 0, projector.opts.ReplayBatchRecords)
	err := projector.wal.Replay(ctx, func(fence corenest.CommitFence, record corenest.CommitRecord) error {
		if projector.isHeld(record.ID) {
			return errProjectorTransactionHeld
		}
		if err := projector.store.Project(ctx, record); err != nil {
			if projector.isFatalProjection(err) {
				projector.completeProjection(record.ID, err)
			}
			return fmt.Errorf("dataengine projector: transaction %s: %w", record.ID.String(), err)
		}
		projector.completeProjection(record.ID, nil)
		projector.projected.Add(1)
		processed++
		processedIDs = append(processedIDs, record.ID)
		lastFence = fence
		if processed >= projector.opts.ReplayBatchRecords {
			return errProjectorBatchComplete
		}
		return nil
	})
	if errors.Is(err, errProjectorBatchComplete) {
		err = nil
	}
	if processed > 0 {
		if ackErr := projector.wal.Ack(ctx, lastFence); ackErr != nil {
			return processed, errors.Join(err, ackErr)
		}
		projector.acknowledge(processedIDs)
	}
	return processed, err
}

func (projector *Projector) run() {
	defer close(projector.done)
	backoff := projector.opts.RetryMin
	for {
		processed, err := projector.replayPass(projector.ctx)
		if errors.Is(err, errProjectorTransactionHeld) {
			backoff = projector.opts.RetryMin
			if !projector.wait(projector.opts.RetryMin) {
				return
			}
			continue
		}
		if err != nil && !errors.Is(err, context.Canceled) {
			projector.recordFailure(err)
			if projector.isFatalProjection(err) {
				return
			}
			if !projector.wait(jitterDuration(backoff)) {
				return
			}
			backoff = min(backoff*2, projector.opts.RetryMax)
			continue
		}
		if projector.ctx.Err() != nil {
			return
		}
		projector.setLastError(nil)
		backoff = projector.opts.RetryMin
		if processed > 0 {
			continue
		}
		if !projector.wait(projector.opts.IdlePoll) {
			return
		}
	}
}

func (projector *Projector) recordFailure(err error) {
	projector.failures.Add(1)
	projector.setLastError(err)
	projector.isFatalProjection(err)
}

func (projector *Projector) isFatalProjection(err error) bool {
	if !errors.Is(err, ErrProjectionConflict) && !errors.Is(err, ErrTransactionIdentity) && !errors.Is(err, ErrReceiptIdentity) {
		return false
	}
	projector.fatalOnce.Do(func() {
		projector.fatalConflicts.Add(1)
		projector.errMu.Lock()
		projector.fatalErr = err
		projector.errMu.Unlock()
		if projector.opts.OnFatal != nil {
			projector.opts.OnFatal(err)
		}
	})
	return true
}

func (projector *Projector) Stats() ProjectorStats {
	stats := ProjectorStats{
		Committed: projector.committed.Load(), Projected: projector.projected.Load(),
		WALUnacked:         projector.walUnacked.Load(),
		ProjectionFailures: projector.failures.Load(), FatalProjectionConflicts: projector.fatalConflicts.Load(),
	}
	projector.errMu.RLock()
	if projector.lastErr != nil {
		stats.LastError = projector.lastErr.Error()
	}
	projector.errMu.RUnlock()
	return stats
}

func (projector *Projector) Healthy() error {
	if projector == nil || projector.wal == nil {
		return errors.New("dataengine projector: not initialized")
	}
	projector.errMu.RLock()
	err := errors.Join(projector.lastErr, projector.fatalErr)
	projector.errMu.RUnlock()
	return errors.Join(projector.wal.Healthy(), err)
}

func (projector *Projector) Close(ctx context.Context) error {
	if projector == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	projector.closeOnce.Do(projector.cancel)
	select {
	case <-projector.done:
		projector.completeAllTickets(context.Canceled)
		if projector.opts.CloseWAL {
			return projector.wal.Close(ctx)
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (projector *Projector) Shutdown(ctx context.Context) error {
	return errors.Join(projector.Flush(ctx), projector.Close(ctx))
}

func (projector *Projector) admit(id coredata.TransactionID) {
	projector.heldMu.Lock()
	projector.held[id] = struct{}{}
	_, alreadyAdmitted := projector.admitted[id]
	if !alreadyAdmitted {
		projector.admitted[id] = struct{}{}
	}
	projector.heldMu.Unlock()
	if !alreadyAdmitted {
		projector.walUnacked.Add(1)
	}
}

func (projector *Projector) admitSystem(id coredata.TransactionID) {
	projector.heldMu.Lock()
	_, alreadyAdmitted := projector.admitted[id]
	if !alreadyAdmitted {
		projector.admitted[id] = struct{}{}
	}
	projector.heldMu.Unlock()
	if !alreadyAdmitted {
		projector.walUnacked.Add(1)
	}
}

func (projector *Projector) discard(id coredata.TransactionID) {
	projector.heldMu.Lock()
	delete(projector.held, id)
	if _, ok := projector.admitted[id]; ok {
		delete(projector.admitted, id)
		projector.walUnacked.Add(^uint64(0))
	}
	projector.heldMu.Unlock()
}

func (projector *Projector) acknowledge(ids []coredata.TransactionID) {
	projector.heldMu.Lock()
	for _, id := range ids {
		if _, ok := projector.admitted[id]; !ok {
			continue
		}
		delete(projector.admitted, id)
		projector.walUnacked.Add(^uint64(0))
	}
	projector.heldMu.Unlock()
}

func (projector *Projector) release(id coredata.TransactionID) {
	projector.heldMu.Lock()
	delete(projector.held, id)
	projector.heldMu.Unlock()
}

func (projector *Projector) isHeld(id coredata.TransactionID) bool {
	projector.heldMu.RLock()
	_, held := projector.held[id]
	projector.heldMu.RUnlock()
	return held
}

func (projector *Projector) signal() {
	select {
	case projector.kick <- struct{}{}:
	default:
	}
}

func (projector *Projector) wait(duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-projector.kick:
		return true
	case <-timer.C:
		return true
	case <-projector.ctx.Done():
		return false
	}
}

func (projector *Projector) setLastError(err error) {
	projector.errMu.Lock()
	projector.lastErr = err
	projector.errMu.Unlock()
}

func (projector *Projector) fatal() error {
	projector.errMu.RLock()
	err := projector.fatalErr
	projector.errMu.RUnlock()
	return err
}

func (projector *Projector) completeProjection(id coredata.TransactionID, err error) {
	projector.ticketMu.Lock()
	ticket := projector.tickets[id]
	if ticket != nil {
		delete(projector.tickets, id)
		ticket.err = err
		close(ticket.done)
	}
	projector.ticketMu.Unlock()
}

func (projector *Projector) removeTicket(id coredata.TransactionID) {
	projector.ticketMu.Lock()
	delete(projector.tickets, id)
	projector.ticketMu.Unlock()
}

func (projector *Projector) completeAllTickets(err error) {
	projector.ticketMu.Lock()
	for id, ticket := range projector.tickets {
		delete(projector.tickets, id)
		ticket.err = err
		close(ticket.done)
	}
	projector.ticketMu.Unlock()
}

func jitterDuration(duration time.Duration) time.Duration {
	if duration <= 1 {
		return duration
	}
	half := duration / 2
	return half + time.Duration(rand.Int64N(int64(duration-half)))
}

var _ corenest.TransactionCommitter = (*Projector)(nil)
var _ corenest.TransactionReleaseNotifier = (*Projector)(nil)
var _ corenest.PipelinedTransactionCommitter = (*Projector)(nil)
var _ coredata.SystemCommitter = (*Projector)(nil)
