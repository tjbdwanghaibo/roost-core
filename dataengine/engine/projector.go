package engine

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	coredata "github.com/tjbdwanghaibo/roost-core/dataengine"
	corenest "github.com/tjbdwanghaibo/roost-core/nest"
	"github.com/tjbdwanghaibo/roost-core/nestwal"
)

var (
	errProjectorTransactionHeld = errors.New("dataengine projector: transaction is still under entity lock")
	errProjectorBatchComplete   = errors.New("dataengine projector: replay batch complete")
	ErrProjectionBackpressure   = errors.New("dataengine projector: unacknowledged transaction limit reached")
)

type ProjectionStore interface {
	Project(context.Context, coredata.CommitRecord) error
}

type BatchProjectionStore interface {
	ProjectionStore
	ProjectBatch(context.Context, []coredata.CommitRecord) error
}

// MultiMutationBatchProjectionStore 显式扩展旧 Store 的单 mutation 批量契约。
// 未声明该能力的 Store 仍逐笔接收多 DAO 事务。
// 实现方须持久记录批次中每笔事务及单笔多 mutation 的身份，支持后续版本落库后的重放。
type MultiMutationBatchProjectionStore interface {
	BatchProjectionStore
	SupportsMultiMutationBatch() bool
}

// RemoteParallelProjectionStore 承诺互不重叠的纯 Remote 事务可以并发执行，
// 且已成功的事务可在后续 WAL 重放中通过持久身份识别。未知 Store 保持串行。
type RemoteParallelProjectionStore interface {
	ProjectionStore
	SupportsRemoteParallelProjection() bool
}

type ProjectorOptions struct {
	RetryMin                time.Duration
	RetryMax                time.Duration
	IdlePoll                time.Duration
	ReplayBatchRecords      int
	ReplayBatchBytes        int
	ReplayReadBytes         int           // 一轮回放保留的逻辑字节上限；首条超大记录允许独占。
	RemoteProjectionWorkers int           // 0 使用默认值 8；1 关闭 Remote 并行投影。
	CheckpointRecords       int           // 连续成功记录的 ack 阈值；1 保留逐单元确认。
	CheckpointInterval      time.Duration // 投影单元之间检查，阻塞的存储调用仍由其 context 控制。
	MaxUnackedRecords       uint64        // 0 不限制；拒绝发生于 WAL 准入前。
	WarnUnackedRecords      uint64        // 0 不设独立预警；达到准入上限也会报告预警。
	CloseWAL                bool
	OnFatal                 func(error)
}

func DefaultProjectorOptions() ProjectorOptions {
	return ProjectorOptions{
		RetryMin: 10 * time.Millisecond, RetryMax: 5 * time.Second,
		IdlePoll: time.Second, ReplayBatchRecords: 256, ReplayBatchBytes: 4 << 20, ReplayReadBytes: 4 << 20, CloseWAL: true,
		CheckpointRecords: 256, CheckpointInterval: 20 * time.Millisecond,
		RemoteProjectionWorkers: 8,
	}
}

type ProjectorStats struct {
	Committed                uint64
	Projected                uint64
	WALUnacked               uint64
	ProjectionFailures       uint64
	FatalProjectionConflicts uint64
	LastError                string
	AdmissionRejected        uint64
	BacklogWarning           bool
}

// Projector owns durable admission and WAL -> Mongo projection. Effects are
// only staged by ProjectionStore; broker delivery is owned by OutboxWorker and
// is deliberately absent from the WAL acknowledgement path.
type Projector struct {
	wal   *nestwal.WAL
	store ProjectionStore
	opts  ProjectorOptions
	ack   func(context.Context, corenest.CommitFence) error

	ctx        context.Context
	cancel     context.CancelFunc
	kick       chan struct{}
	done       chan struct{}
	closeOnce  sync.Once
	flushGate  operationGate
	replayGate operationGate

	heldMu    sync.RWMutex
	held      map[coredata.TransactionID]struct{}
	admitted  map[coredata.TransactionID]struct{}
	errMu     sync.RWMutex
	lastErr   error
	fatalErr  error
	fatalOnce sync.Once
	ticketMu  sync.Mutex
	tickets   map[coredata.TransactionID]*projectionTicket

	committed         atomic.Uint64
	projected         atomic.Uint64
	walUnacked        atomic.Uint64
	failures          atomic.Uint64
	fatalConflicts    atomic.Uint64
	admissionRejected atomic.Uint64
}

func NewProjector(wal *nestwal.WAL, store ProjectionStore, options ProjectorOptions) (*Projector, error) {
	if wal == nil || store == nil {
		return nil, errors.New("dataengine projector: WAL and store are required")
	}
	defaults := DefaultProjectorOptions()
	if options.ReplayReadBytes < 0 {
		return nil, errors.New("dataengine projector: replay read bytes must not be negative")
	}
	if options.ReplayReadBytes == 0 {
		options.ReplayReadBytes = defaults.ReplayReadBytes
	}
	if options.RemoteProjectionWorkers < 0 || options.RemoteProjectionWorkers > 64 {
		return nil, errors.New("dataengine projector: remote projection workers must be between 0 and 64")
	}
	if options.RemoteProjectionWorkers == 0 {
		options.RemoteProjectionWorkers = defaults.RemoteProjectionWorkers
	}
	if options.CheckpointRecords < 0 || options.CheckpointInterval < 0 ||
		(options.MaxUnackedRecords > 0 && options.WarnUnackedRecords > options.MaxUnackedRecords) {
		return nil, errors.New("dataengine projector: invalid checkpoint or backlog limits")
	}
	if options.CheckpointRecords == 0 {
		options.CheckpointRecords = defaults.CheckpointRecords
	}
	if options.CheckpointInterval == 0 {
		options.CheckpointInterval = defaults.CheckpointInterval
	}
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
	if options.ReplayBatchBytes <= 0 {
		options.ReplayBatchBytes = defaults.ReplayBatchBytes
	}
	ctx, cancel := context.WithCancel(context.Background())
	projector := &Projector{
		wal: wal, store: store, opts: options, ack: wal.Ack, ctx: ctx, cancel: cancel,
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
	if err := projector.reserve(record.ID, false); err != nil {
		projector.removeTicket(record.ID)
		return nil, err
	}
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
	if err := projector.reserve(record.ID, true); err != nil {
		return err
	}
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
	if err := projector.reserve(record.ID, true); err != nil {
		return nil, err
	}
	ticket, err := projector.wal.Enqueue(ctx, record)
	if err != nil {
		projector.discard(record.ID)
		return nil, err
	}
	projector.committed.Add(1)
	// Commit kicks the replay loop right after its synchronous append. A
	// pipelined record becomes durable later, so the kick has to wait for the
	// ticket: the transaction's release usually arrives before the fsync, and
	// a loop woken then finds nothing to replay and sleeps for IdlePoll.
	go projector.signalWhenDurable(ticket)
	return ticket, nil
}

func (projector *Projector) signalWhenDurable(ticket corenest.CommitTicket) {
	select {
	case <-ticket.Done():
		projector.signal()
	case <-projector.ctx.Done():
	}
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
	if err := projector.flushGate.acquire(ctx); err != nil {
		return err
	}
	defer projector.flushGate.release()
	if fatal := projector.fatal(); fatal != nil {
		return fatal
	}
	if err := projector.wal.Sync(ctx); err != nil {
		return err
	}
	for {
		processed, err := projector.ReplayPass(ctx)
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

// OverrideAck replaces the checkpoint acknowledgement hook. It exists for
// integration harnesses that inject a failure after the Mongo write succeeded
// (the "projection landed, checkpoint lost" restart scenario); production
// assembly never calls it.
func (projector *Projector) OverrideAck(ack func(context.Context, corenest.CommitFence) error) {
	if projector == nil || ack == nil {
		return
	}
	projector.ack = ack
}

func (projector *Projector) recordFailure(err error) {
	projector.failures.Add(1)
	projector.setLastError(err)
	projector.isFatalProjection(err)
}

func (projector *Projector) isFatalProjection(err error) bool {
	// A deferral is a routing signal, not a verdict: it must never fence the
	// projector, whatever path it arrives through.
	if errors.Is(err, ErrProjectionBatchNeedsPerRecord) {
		return false
	}
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
		AdmissionRejected:  projector.admissionRejected.Load(),
		ProjectionFailures: projector.failures.Load(), FatalProjectionConflicts: projector.fatalConflicts.Load(),
	}
	warning := projector.opts.WarnUnackedRecords
	if warning == 0 {
		warning = projector.opts.MaxUnackedRecords
	}
	stats.BacklogWarning = warning > 0 && stats.WALUnacked >= warning
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

// reserve 在同一锁内检查与预留额度，不能让并发 Commit 越过上限。
func (projector *Projector) reserve(id coredata.TransactionID, held bool) error {
	projector.heldMu.Lock()
	defer projector.heldMu.Unlock()
	if _, exists := projector.admitted[id]; !exists {
		if limit := projector.opts.MaxUnackedRecords; limit > 0 && uint64(len(projector.admitted)) >= limit {
			projector.admissionRejected.Add(1)
			return ErrProjectionBackpressure
		}
		projector.admitted[id] = struct{}{}
		projector.walUnacked.Add(1)
	}
	if held {
		projector.held[id] = struct{}{}
	}
	return nil
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

var _ corenest.TransactionCommitter = (*Projector)(nil)
var _ corenest.TransactionReleaseNotifier = (*Projector)(nil)
var _ corenest.PipelinedTransactionCommitter = (*Projector)(nil)
var _ coredata.SystemCommitter = (*Projector)(nil)
