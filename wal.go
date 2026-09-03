// Package nestwal provides a single-writer segmented commit log for Nest
// transactions. Records are checksummed, group committed, replayed in append
// order, and acknowledged through a double-buffered durable checkpoint.
package nestwal

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tjbdwanghaibo/roost-core/metrics"
	corenest "github.com/tjbdwanghaibo/roost-core/nest"
)

const (
	frameMagic      = uint32(0x5253574c) // RSWL
	frameVersion    = uint16(1)
	frameHeaderSize = 20
)

var (
	ErrClosed         = errors.New("nestwal: closed")
	ErrLocked         = errors.New("nestwal: directory is already locked")
	ErrCorrupt        = errors.New("nestwal: corrupt log")
	ErrRecordTooLarge = errors.New("nestwal: record exceeds configured limit")
	ErrCapacity       = errors.New("nestwal: disk capacity limit reached")
	errStatsStop      = errors.New("nestwal: stats stop")
)

type Options struct {
	Dir string
	// WriterVersion controls record encoding only. Readers always accept both
	// deployed v1 and Data Engine v2 records. Zero defaults to v1 for rollout.
	WriterVersion       WriterVersion
	SegmentBytes        int64
	MaxRecordBytes      int
	QueueCapacity       int
	BatchMaxRecords     int
	BatchMaxBytes       int
	BatchDelay          time.Duration
	GroupCommitInterval time.Duration
	RetainSegments      int
	MaxDiskBytes        int64
	MaxUnackedAge       time.Duration
	FileMode            os.FileMode
	// OnFatal fences the hosting process when a physical write or fsync has an
	// indeterminate outcome. It must initiate shutdown rather than retry writes.
	OnFatal func(error)
}

func DefaultOptions(dir string) Options {
	return Options{
		Dir:                 dir,
		WriterVersion:       WriterVersionV1,
		SegmentBytes:        256 << 20,
		MaxRecordBytes:      16 << 20,
		QueueCapacity:       8192,
		BatchMaxRecords:     256,
		BatchMaxBytes:       4 << 20,
		BatchDelay:          500 * time.Microsecond,
		GroupCommitInterval: 10 * time.Millisecond,
		RetainSegments:      2,
		MaxDiskBytes:        8 << 30,
		MaxUnackedAge:       24 * time.Hour,
		FileMode:            0o600,
	}
}

type Stats struct {
	Segment          uint64
	Offset           int64
	Queued           int
	Appended         uint64
	Bytes            uint64
	Syncs            uint64
	Replayed         uint64
	Acknowledged     uint64
	TerminalError    string
	DiskBytes        int64
	SegmentFiles     int
	OldestUnackedAge time.Duration
}

type WAL struct {
	opts Options

	appendCh chan appendRequest
	closeCh  chan struct{}
	doneCh   chan struct{}

	lifecycleMu sync.RWMutex
	closed      bool
	closeOnce   sync.Once
	closeErr    error

	stateMu      sync.RWMutex
	active       *os.File
	segment      uint64
	offset       int64
	unsynced     bool
	lockHandle   *os.File
	terminalMu   sync.RWMutex
	terminalErr  error
	fatalOnce    sync.Once
	replayMu     sync.Mutex
	checkpointMu sync.Mutex
	checkpoint   checkpointState

	appended     atomic.Uint64
	bytesWritten atomic.Uint64
	syncs        atomic.Uint64
	replayed     atomic.Uint64
	acked        atomic.Uint64
	diskBytes    atomic.Int64
	batchBuffers sync.Pool

	// Pipelined commit state (see NEST_PIPELINED_COMMIT.md in roost-core).
	// enqueueMu serializes LSN assignment with queue admission so LSN order
	// equals physical log order; reservedBytes moves the capacity check into
	// Enqueue, which must be the only rejection point; writtenLSN (guarded by
	// stateMu) tracks the newest ticketed record physically written; tickets
	// resolve on fsync. Lock order: stateMu -> ticketMu.
	enqueueMu     sync.Mutex
	nextLSN       uint64 // guarded by enqueueMu
	reservedBytes atomic.Int64
	writtenLSN    uint64 // guarded by stateMu
	durableLSN    atomic.Uint64
	ticketMu      sync.Mutex
	tickets       []*walTicket // FIFO, ascending LSN

	// statsCacheMu guards the cached oldest-unacked timestamp so periodic
	// health checks do not touch the disk on every probe. The age derived
	// from the cached CreatedAt stays exact while the cache holds (it grows
	// with the clock); Ack invalidates it because acknowledgement is the only
	// event that can move the oldest record forward.
	statsCacheMu       sync.Mutex
	statsOldestCreated int64
	statsOldestValid   time.Time
}

const statsOldestCacheTTL = 5 * time.Second

type appendRequest struct {
	record      corenest.CommitRecord
	frame       []byte
	requireSync bool
	reserved    bool // capacity admitted by Enqueue; never rejected here
	lsn         uint64
	done        chan appendResult
}

// walTicket implements corenest.CommitTicket. err is written before done is
// closed and read only after it is closed, so no lock is needed.
type walTicket struct {
	lsn  uint64
	done chan struct{}
	err  error
}

func (t *walTicket) LSN() uint64           { return t.lsn }
func (t *walTicket) Done() <-chan struct{} { return t.done }
func (t *walTicket) Err() error {
	select {
	case <-t.done:
		return t.err
	default:
		return nil
	}
}

type appendResult struct {
	fence corenest.CommitFence
	err   error
}

func Open(options Options) (*WAL, error) {
	opts, err := normalizeOptions(options)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(opts.Dir, 0o750); err != nil {
		return nil, fmt.Errorf("nestwal: create directory: %w", err)
	}
	if err := syncDirectory(filepath.Dir(opts.Dir)); err != nil {
		return nil, fmt.Errorf("nestwal: sync parent directory: %w", err)
	}
	lockHandle, err := os.OpenFile(filepath.Join(opts.Dir, "writer.lock"), os.O_CREATE|os.O_RDWR, opts.FileMode)
	if err != nil {
		return nil, fmt.Errorf("nestwal: open lock: %w", err)
	}
	if err := lockFile(lockHandle); err != nil {
		_ = lockHandle.Close()
		return nil, errors.Join(ErrLocked, err)
	}

	w := &WAL{
		opts:       opts,
		appendCh:   make(chan appendRequest, opts.QueueCapacity),
		closeCh:    make(chan struct{}),
		doneCh:     make(chan struct{}),
		lockHandle: lockHandle,
	}
	w.batchBuffers.New = func() any {
		buffer := make([]byte, 0, opts.BatchMaxBytes)
		return &buffer
	}
	w.checkpoint, err = loadCheckpoint(opts.Dir)
	if err == nil {
		err = w.openActive()
	}
	if err != nil {
		_ = unlockFile(lockHandle)
		_ = lockHandle.Close()
		return nil, err
	}
	usage, _ := w.diskUsage()
	w.diskBytes.Store(usage)
	go w.writerLoop()
	return w, nil
}

func normalizeOptions(opts Options) (Options, error) {
	defaults := DefaultOptions(opts.Dir)
	if strings.TrimSpace(opts.Dir) == "" {
		return opts, errors.New("nestwal: directory is required")
	}
	abs, err := filepath.Abs(opts.Dir)
	if err != nil {
		return opts, err
	}
	opts.Dir = filepath.Clean(abs)
	if opts.SegmentBytes <= 0 {
		opts.SegmentBytes = defaults.SegmentBytes
	}
	if opts.WriterVersion == 0 {
		opts.WriterVersion = defaults.WriterVersion
	}
	if opts.WriterVersion != WriterVersionV1 && opts.WriterVersion != WriterVersionV2 {
		return opts, fmt.Errorf("nestwal: unsupported writer version %d", opts.WriterVersion)
	}
	if opts.MaxRecordBytes <= 0 {
		opts.MaxRecordBytes = defaults.MaxRecordBytes
	}
	if opts.QueueCapacity <= 0 {
		opts.QueueCapacity = defaults.QueueCapacity
	}
	if opts.BatchMaxRecords <= 0 {
		opts.BatchMaxRecords = defaults.BatchMaxRecords
	}
	if opts.BatchMaxBytes <= 0 {
		opts.BatchMaxBytes = defaults.BatchMaxBytes
	}
	if opts.BatchDelay <= 0 {
		opts.BatchDelay = defaults.BatchDelay
	}
	if opts.GroupCommitInterval <= 0 {
		opts.GroupCommitInterval = defaults.GroupCommitInterval
	}
	if opts.RetainSegments < 0 {
		return opts, errors.New("nestwal: retain segments cannot be negative")
	}
	if opts.RetainSegments == 0 {
		opts.RetainSegments = defaults.RetainSegments
	}
	if opts.MaxDiskBytes <= 0 {
		opts.MaxDiskBytes = defaults.MaxDiskBytes
	}
	if opts.MaxUnackedAge <= 0 {
		opts.MaxUnackedAge = defaults.MaxUnackedAge
	}
	if opts.FileMode == 0 {
		opts.FileMode = defaults.FileMode
	}
	if opts.SegmentBytes <= frameHeaderSize || int64(opts.MaxRecordBytes+frameHeaderSize) > opts.SegmentBytes {
		return opts, errors.New("nestwal: segment must be larger than maximum record")
	}
	return opts, nil
}

func (w *WAL) Append(ctx context.Context, record corenest.CommitRecord) (corenest.CommitFence, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	payload, err := encodeRecordVersion(record, w.opts.WriterVersion)
	if err != nil {
		return corenest.CommitFence{}, err
	}
	if len(payload) > w.opts.MaxRecordBytes {
		return corenest.CommitFence{}, ErrRecordTooLarge
	}
	frame := encodeFrame(payload)
	req := appendRequest{
		record:      record,
		frame:       frame,
		requireSync: record.Durability == corenest.DurabilityStrict,
		done:        make(chan appendResult, 1),
	}

	w.lifecycleMu.RLock()
	if w.closed {
		w.lifecycleMu.RUnlock()
		return corenest.CommitFence{}, ErrClosed
	}
	if terminal := w.terminal(); terminal != nil {
		w.lifecycleMu.RUnlock()
		return corenest.CommitFence{}, terminal
	}
	select {
	case w.appendCh <- req:
		w.lifecycleMu.RUnlock()
	case <-ctx.Done():
		w.lifecycleMu.RUnlock()
		return corenest.CommitFence{}, ctx.Err()
	}
	// Cancellation controls queue admission only. Once admitted, returning
	// before the writer decides the outcome would make commit ambiguous.
	result := <-req.done
	return result.fence, result.err
}

// Enqueue admits one pipelined record and returns a ticket that resolves when
// the record is durable. It is called with entity locks held, so it performs
// every rejectable check synchronously (encoding, size, capacity reservation,
// terminal state, queue admission) and never blocks on I/O. After it returns
// successfully the only possible failure is ErrCommitIndeterminate on the
// ticket, delivered through the WAL terminal path.
func (w *WAL) Enqueue(ctx context.Context, record corenest.CommitRecord) (corenest.CommitTicket, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	payload, err := encodeRecordVersion(record, w.opts.WriterVersion)
	if err != nil {
		return nil, err
	}
	if len(payload) > w.opts.MaxRecordBytes {
		return nil, ErrRecordTooLarge
	}
	frame := encodeFrame(payload)
	// Reserve capacity now: processBatch must never reject a ticketed record,
	// because the caller has already released its right to roll back by the
	// time the batch is written.
	frameBytes := int64(len(frame))
	if w.opts.MaxDiskBytes > 0 {
		if w.diskBytes.Load()+w.reservedBytes.Add(frameBytes) > w.opts.MaxDiskBytes {
			w.reservedBytes.Add(-frameBytes)
			return nil, fmt.Errorf("%w: current=%d incoming=%d limit=%d", ErrCapacity, w.diskBytes.Load(), frameBytes, w.opts.MaxDiskBytes)
		}
	}
	req := appendRequest{
		record:      record,
		frame:       frame,
		requireSync: true,
		reserved:    true,
		done:        make(chan appendResult, 1),
	}

	w.lifecycleMu.RLock()
	if w.closed {
		w.lifecycleMu.RUnlock()
		w.reservedBytes.Add(-frameBytes)
		return nil, ErrClosed
	}
	if terminal := w.terminal(); terminal != nil {
		w.lifecycleMu.RUnlock()
		w.reservedBytes.Add(-frameBytes)
		return nil, terminal
	}
	// enqueueMu makes LSN assignment atomic with queue admission and ticket
	// registration, so LSN order equals log order and the ticket FIFO stays
	// sorted. Sends never block long: capacity rejections above and the
	// bounded queue are the only backpressure, and a full queue is a
	// synchronous rejection because the caller holds entity locks.
	w.enqueueMu.Lock()
	req.lsn = w.nextLSN + 1
	select {
	case w.appendCh <- req:
		w.nextLSN++
	default:
		w.enqueueMu.Unlock()
		w.lifecycleMu.RUnlock()
		w.reservedBytes.Add(-frameBytes)
		metrics.IncCounter("nestwal.reject.total", metrics.Labels{"reason": "queue_full"}, 1)
		return nil, fmt.Errorf("%w: append queue is full", ErrCapacity)
	}
	ticket := &walTicket{lsn: req.lsn, done: make(chan struct{})}
	w.ticketMu.Lock()
	w.tickets = append(w.tickets, ticket)
	w.ticketMu.Unlock()
	w.enqueueMu.Unlock()
	w.lifecycleMu.RUnlock()
	return ticket, nil
}

// DurableLSN is the pipelined-commit watermark: every ticketed record with
// LSN <= this value is durable.
func (w *WAL) DurableLSN() uint64 {
	return w.durableLSN.Load()
}

// resolveDurableLocked resolves every pending ticket whose record is covered
// by the latest successful fsync. Caller holds stateMu; ticketMu nests inside.
func (w *WAL) resolveDurableLocked() {
	upto := w.writtenLSN
	if upto == 0 {
		return
	}
	w.ticketMu.Lock()
	// Publish the watermark BEFORE waking any waiter: a resolved ticket is a
	// promise that DurableLSN already covers its record — the externalization
	// gates read the watermark right after Done() fires. The store is also
	// unconditional on pending tickets: durability advanced regardless of who
	// is waiting.
	if w.durableLSN.Load() < upto {
		w.durableLSN.Store(upto)
	}
	resolved := 0
	for _, ticket := range w.tickets {
		if ticket.lsn > upto {
			break
		}
		close(ticket.done)
		resolved++
	}
	if resolved > 0 {
		w.tickets = w.tickets[resolved:]
	}
	metrics.SetGauge("nestwal.pending.tickets", nil, int64(len(w.tickets)))
	w.ticketMu.Unlock()
}

// failPendingTickets resolves every pending ticket with the terminal error.
// The write outcome of enqueued records is unknown once the WAL is fenced, so
// indeterminate is the only honest verdict.
func (w *WAL) failPendingTickets(err error) {
	w.ticketMu.Lock()
	for _, ticket := range w.tickets {
		ticket.err = err
		close(ticket.done)
	}
	w.tickets = nil
	w.ticketMu.Unlock()
}

func (w *WAL) Ack(_ context.Context, fence corenest.CommitFence) error {
	if fence.Segment == 0 || fence.Offset <= 0 {
		return errors.New("nestwal: invalid acknowledgement fence")
	}
	w.checkpointMu.Lock()
	defer w.checkpointMu.Unlock()
	if !fenceAfter(fence, w.checkpoint.fence) {
		return nil
	}
	w.stateMu.RLock()
	beyondEnd := fence.Segment > w.segment || fence.Segment == w.segment && fence.Offset > w.offset
	w.stateMu.RUnlock()
	if beyondEnd {
		return errors.New("nestwal: acknowledgement is beyond log end")
	}
	next := checkpointState{generation: w.checkpoint.generation + 1, fence: fence}
	if err := storeCheckpoint(w.opts.Dir, next, w.opts.FileMode); err != nil {
		return fmt.Errorf("nestwal: store acknowledgement: %w", err)
	}
	w.checkpoint = next
	w.acked.Add(1)
	w.statsCacheMu.Lock()
	w.statsOldestValid = time.Time{}
	w.statsCacheMu.Unlock()
	w.pruneAcked(fence.Segment)
	return nil
}

func (w *WAL) Replay(ctx context.Context, consume func(corenest.CommitFence, corenest.CommitRecord) error) error {
	if consume == nil {
		return errors.New("nestwal: nil replay consumer")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	w.replayMu.Lock()
	defer w.replayMu.Unlock()

	w.stateMu.RLock()
	activeSegment, activeLimit := w.segment, w.offset
	w.stateMu.RUnlock()
	w.checkpointMu.Lock()
	ack := w.checkpoint.fence
	w.checkpointMu.Unlock()
	segments, err := listSegments(w.opts.Dir)
	if err != nil {
		return err
	}
	for _, segment := range segments {
		if segment > activeSegment {
			break
		}
		// The acknowledgement fence is the end offset of a frame, so it is a
		// valid scan start: skip fully acknowledged segments and the
		// acknowledged prefix of the fence segment instead of re-reading and
		// re-checksumming them on every pass.
		if segment < ack.Segment {
			continue
		}
		start := int64(0)
		if segment == ack.Segment {
			start = ack.Offset
		}
		path := filepath.Join(w.opts.Dir, segmentName(segment))
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		limit := int64(-1)
		if segment == activeSegment {
			limit = activeLimit
		}
		err = scanFramesFrom(file, segment, start, limit, w.opts.MaxRecordBytes, false, func(fence corenest.CommitFence, payload []byte) error {
			if !fenceAfter(fence, ack) {
				return nil
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			record, err := decodeRecord(payload)
			if err != nil {
				return errors.Join(ErrCorrupt, err)
			}
			fence.TransactionID = record.ID
			if err := consume(fence, record); err != nil {
				return err
			}
			w.replayed.Add(1)
			return nil
		})
		closeErr := file.Close()
		if err == nil {
			err = closeErr
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// Sync forces all accepted asynchronous appends to stable storage. Append
// already waits for the physical write, so no queue barrier is required.
func (w *WAL) Sync(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if terminal := w.terminal(); terminal != nil {
		return terminal
	}
	if err := w.syncActive(); err != nil {
		err = errors.Join(corenest.ErrCommitIndeterminate, err)
		w.setTerminal(err)
		return err
	}
	return nil
}

func (w *WAL) Stats() Stats {
	w.stateMu.RLock()
	segment, offset := w.segment, w.offset
	w.stateMu.RUnlock()
	stats := Stats{
		Segment:      segment,
		Offset:       offset,
		Queued:       len(w.appendCh),
		Appended:     w.appended.Load(),
		Bytes:        w.bytesWritten.Load(),
		Syncs:        w.syncs.Load(),
		Replayed:     w.replayed.Load(),
		Acknowledged: w.acked.Load(),
	}
	if err := w.terminal(); err != nil {
		stats.TerminalError = err.Error()
	}
	stats.DiskBytes, stats.SegmentFiles = w.diskUsage()
	stats.OldestUnackedAge = w.oldestUnackedAge()
	return stats
}

func (w *WAL) Healthy() error {
	if err := w.terminal(); err != nil {
		return err
	}
	stats := w.Stats()
	if w.opts.MaxDiskBytes > 0 && stats.DiskBytes > w.opts.MaxDiskBytes {
		return fmt.Errorf("nestwal: disk usage %d exceeds limit %d", stats.DiskBytes, w.opts.MaxDiskBytes)
	}
	if w.opts.MaxUnackedAge > 0 && stats.OldestUnackedAge > w.opts.MaxUnackedAge {
		return fmt.Errorf("nestwal: oldest unacknowledged record age %s exceeds limit %s", stats.OldestUnackedAge, w.opts.MaxUnackedAge)
	}
	return nil
}

func (w *WAL) diskUsage() (int64, int) {
	entries, err := os.ReadDir(w.opts.Dir)
	if err != nil {
		return 0, 0
	}
	var bytes int64
	segments := 0
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		bytes += info.Size()
		if strings.HasPrefix(entry.Name(), "segment-") && strings.HasSuffix(entry.Name(), ".wal") {
			segments++
		}
	}
	return bytes, segments
}

func (w *WAL) oldestUnackedAge() time.Duration {
	w.statsCacheMu.Lock()
	if time.Now().Before(w.statsOldestValid) {
		createdAt := w.statsOldestCreated
		w.statsCacheMu.Unlock()
		return ageSince(createdAt)
	}
	w.statsCacheMu.Unlock()

	// Replay starts at the acknowledgement fence and stops on the first
	// record, so a cache miss costs one frame read, not a log scan.
	var createdAt int64
	err := w.Replay(context.Background(), func(_ corenest.CommitFence, record corenest.CommitRecord) error {
		createdAt = record.CreatedAt
		return errStatsStop
	})
	if err != nil && !errors.Is(err, errStatsStop) {
		return 0
	}
	w.statsCacheMu.Lock()
	w.statsOldestCreated = createdAt
	w.statsOldestValid = time.Now().Add(statsOldestCacheTTL)
	w.statsCacheMu.Unlock()
	return ageSince(createdAt)
}

func ageSince(createdAt int64) time.Duration {
	if createdAt <= 0 {
		return 0
	}
	age := time.Since(time.Unix(0, createdAt))
	if age < 0 {
		return 0
	}
	return age
}

func (w *WAL) Close(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	w.closeOnce.Do(func() {
		w.lifecycleMu.Lock()
		w.closed = true
		close(w.closeCh)
		w.lifecycleMu.Unlock()
	})
	select {
	case <-w.doneCh:
		return w.closeErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (w *WAL) writerLoop() {
	ticker := time.NewTicker(w.opts.GroupCommitInterval)
	defer ticker.Stop()
	defer close(w.doneCh)
	for {
		select {
		case first := <-w.appendCh:
			w.processBatch(w.collectBatch(first))
		case <-ticker.C:
			if err := w.syncActive(); err != nil {
				w.setTerminal(errors.Join(corenest.ErrCommitIndeterminate, err))
			}
		case <-w.closeCh:
			w.drainAndClose()
			return
		}
	}
}

func (w *WAL) collectBatch(first appendRequest) []appendRequest {
	batch := make([]appendRequest, 0, w.opts.BatchMaxRecords)
	batch = append(batch, first)
	bytesTotal := len(first.frame)
	timer := time.NewTimer(w.opts.BatchDelay)
	defer timer.Stop()
	for len(batch) < w.opts.BatchMaxRecords && bytesTotal < w.opts.BatchMaxBytes {
		select {
		case req := <-w.appendCh:
			batch = append(batch, req)
			bytesTotal += len(req.frame)
		case <-timer.C:
			return batch
		case <-w.closeCh:
			return batch
		}
	}
	return batch
}

func (w *WAL) processBatch(batch []appendRequest) {
	if len(batch) == 0 {
		return
	}
	if terminal := w.terminal(); terminal != nil {
		for i := range batch {
			batch[i].done <- appendResult{err: terminal}
		}
		// A ticket registered concurrently with the terminal transition may
		// have missed the setTerminal sweep; its record lands here, so fail
		// the pending set again rather than leave the waiter blocked.
		w.failPendingTickets(terminal)
		return
	}
	// Reserved (ticketed) requests were capacity-admitted in Enqueue and can
	// no longer be rejected: their callers already released entity locks on
	// the strength of that admission. Only unreserved requests compete for
	// the remaining capacity here.
	unreservedBytes := int64(0)
	reservedBytes := int64(0)
	for i := range batch {
		if batch[i].reserved {
			reservedBytes += int64(len(batch[i].frame))
		} else {
			unreservedBytes += int64(len(batch[i].frame))
		}
	}
	if w.opts.MaxDiskBytes > 0 && unreservedBytes > 0 &&
		w.diskBytes.Load()+w.reservedBytes.Load()+unreservedBytes > w.opts.MaxDiskBytes {
		metrics.IncCounter("nestwal.reject.total", metrics.Labels{"reason": "disk_cap"}, 1)
		err := fmt.Errorf("%w: current=%d incoming=%d limit=%d", ErrCapacity, w.diskBytes.Load(), unreservedBytes, w.opts.MaxDiskBytes)
		kept := batch[:0]
		for i := range batch {
			if batch[i].reserved {
				kept = append(kept, batch[i])
				continue
			}
			batch[i].done <- appendResult{err: err}
		}
		batch = kept
		if len(batch) == 0 {
			return
		}
	}
	// The reservation's job ends once the batch is committed to the write
	// path: from here on the bytes are accounted through diskBytes, and every
	// failure below is terminal rather than a rejection.
	if reservedBytes > 0 {
		w.reservedBytes.Add(-reservedBytes)
	}
	fences := make([]corenest.CommitFence, len(batch))
	requireSync := false
	w.stateMu.Lock()
	var err error
	bufferPtr := w.batchBuffers.Get().(*[]byte)
	buffer := (*bufferPtr)[:0]
	for start := 0; start < len(batch); {
		if w.offset > 0 && w.offset+int64(len(batch[start].frame)) > w.opts.SegmentBytes {
			if err = w.rotateLocked(); err != nil {
				break
			}
		}
		buffer = buffer[:0]
		end := start
		nextOffset := w.offset
		for end < len(batch) {
			frameSize := int64(len(batch[end].frame))
			if end > start && nextOffset+frameSize > w.opts.SegmentBytes {
				break
			}
			buffer = append(buffer, batch[end].frame...)
			nextOffset += frameSize
			fences[end] = corenest.CommitFence{TransactionID: batch[end].record.ID, Segment: w.segment, Offset: nextOffset}
			requireSync = requireSync || batch[end].requireSync
			end++
		}
		if err = writeFull(w.active, buffer); err != nil {
			break
		}
		w.diskBytes.Add(int64(len(buffer)))
		w.offset = nextOffset
		w.unsynced = true
		for i := start; i < end; i++ {
			if batch[i].lsn > w.writtenLSN {
				w.writtenLSN = batch[i].lsn
			}
		}
		start = end
	}
	if err == nil && requireSync {
		syncStart := time.Now()
		err = w.syncActiveLocked()
		metrics.ObserveDuration("nestwal.fsync.duration", nil, time.Since(syncStart))
	}
	w.stateMu.Unlock()
	if cap(buffer) <= w.opts.BatchMaxBytes*2 {
		*bufferPtr = buffer[:0]
		w.batchBuffers.Put(bufferPtr)
	}
	if err != nil {
		err = errors.Join(corenest.ErrCommitIndeterminate, err)
		w.setTerminal(err)
		for i := range batch {
			batch[i].done <- appendResult{err: err}
		}
		return
	}
	bytes := int64(0)
	for i := range batch {
		w.appended.Add(1)
		w.bytesWritten.Add(uint64(len(batch[i].frame)))
		bytes += int64(len(batch[i].frame))
		batch[i].done <- appendResult{fence: fences[i]}
	}
	// Per-batch pipeline metrics: records/batches ratio is the group-commit
	// amplification, disk bytes the retention pressure.
	metrics.IncCounter("nestwal.batch.total", nil, 1)
	metrics.IncCounter("nestwal.append.total", nil, int64(len(batch)))
	metrics.IncCounter("nestwal.bytes.total", nil, bytes)
	metrics.SetGauge("nestwal.disk.bytes", nil, w.diskBytes.Load())
}

func (w *WAL) drainAndClose() {
drainLoop:
	for {
		batch := make([]appendRequest, 0, w.opts.BatchMaxRecords)
		for len(batch) < w.opts.BatchMaxRecords {
			select {
			case req := <-w.appendCh:
				batch = append(batch, req)
			default:
				if len(batch) > 0 {
					w.processBatch(batch)
					continue drainLoop
				}
				if err := w.syncAndCloseActive(); err != nil {
					// A failed final sync leaves enqueued outcomes unknown;
					// fence and fail the remaining tickets so no pipelined
					// waiter blocks past close.
					w.setTerminal(errors.Join(corenest.ErrCommitIndeterminate, err))
					w.closeErr = errors.Join(w.closeErr, err)
				}
				w.closeErr = errors.Join(w.closeErr, w.terminal())
				if w.lockHandle != nil {
					w.closeErr = errors.Join(w.closeErr, unlockFile(w.lockHandle), w.lockHandle.Close())
				}
				return
			}
		}
		w.processBatch(batch)
	}
}

func (w *WAL) openActive() error {
	segments, err := listSegments(w.opts.Dir)
	if err != nil {
		return err
	}
	if len(segments) == 0 {
		segments = []uint64{1}
	} else {
		for i := 1; i < len(segments); i++ {
			if segments[i] != segments[i-1]+1 {
				return errors.Join(ErrCorrupt, errors.New("non-contiguous WAL segments"))
			}
		}
		if segments[0] > 1 && w.checkpoint.fence.Segment == 0 {
			return errors.Join(ErrCorrupt, errors.New("WAL starts after segment 1 without an acknowledgement"))
		}
	}
	w.segment = segments[len(segments)-1]
	path := filepath.Join(w.opts.Dir, segmentName(w.segment))
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, w.opts.FileMode)
	if err != nil {
		return err
	}
	if info, statErr := file.Stat(); statErr == nil && info.Size() == 0 {
		if err := syncDirectory(w.opts.Dir); err != nil {
			_ = file.Close()
			return err
		}
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return err
	}
	lastGood, err := scanFramesEnd(file, w.segment, info.Size(), w.opts.MaxRecordBytes)
	if err != nil {
		_ = file.Close()
		return err
	}
	if lastGood != info.Size() {
		if err := file.Truncate(lastGood); err != nil {
			_ = file.Close()
			return err
		}
	}
	if _, err := file.Seek(lastGood, io.SeekStart); err != nil {
		_ = file.Close()
		return err
	}
	w.active = file
	w.offset = lastGood
	if fenceAfter(w.checkpoint.fence, corenest.CommitFence{Segment: w.segment, Offset: w.offset}) {
		_ = file.Close()
		w.active = nil
		return errors.Join(ErrCorrupt, errors.New("acknowledgement is beyond recovered WAL end"))
	}
	return nil
}

func (w *WAL) rotateLocked() error {
	if err := w.syncActiveLocked(); err != nil {
		return err
	}
	if err := w.active.Close(); err != nil {
		return err
	}
	w.segment++
	path := filepath.Join(w.opts.Dir, segmentName(w.segment))
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, w.opts.FileMode)
	if err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := syncDirectory(w.opts.Dir); err != nil {
		_ = file.Close()
		return err
	}
	w.active = file
	w.offset = 0
	return nil
}

func (w *WAL) syncActive() error {
	w.stateMu.Lock()
	defer w.stateMu.Unlock()
	return w.syncActiveLocked()
}

func (w *WAL) syncActiveLocked() error {
	if w.active == nil || !w.unsynced {
		// Everything written is already durable; late-registered tickets
		// covered by the current watermark still need resolution.
		w.resolveDurableLocked()
		return nil
	}
	if err := w.active.Sync(); err != nil {
		return err
	}
	w.unsynced = false
	w.syncs.Add(1)
	w.resolveDurableLocked()
	return nil
}

func (w *WAL) syncAndCloseActive() error {
	w.stateMu.Lock()
	defer w.stateMu.Unlock()
	if w.active == nil {
		return nil
	}
	err := w.syncActiveLocked()
	err = errors.Join(err, w.active.Close())
	w.active = nil
	return err
}

func (w *WAL) terminal() error {
	w.terminalMu.RLock()
	defer w.terminalMu.RUnlock()
	return w.terminalErr
}

func (w *WAL) setTerminal(err error) {
	if err == nil {
		return
	}
	first := false
	w.terminalMu.Lock()
	if w.terminalErr == nil {
		w.terminalErr = err
		first = true
	}
	w.terminalMu.Unlock()
	if first {
		// Enqueued records have unknown write outcomes once the WAL is
		// fenced; their waiters must observe the terminal verdict instead of
		// blocking forever.
		w.failPendingTickets(err)
	}
	if first && w.opts.OnFatal != nil {
		w.fatalOnce.Do(func() {
			go func() {
				defer func() { _ = recover() }()
				w.opts.OnFatal(err)
			}()
		})
	}
}

func (w *WAL) pruneAcked(ackSegment uint64) {
	if ackSegment <= uint64(w.opts.RetainSegments) {
		return
	}
	removeBefore := ackSegment - uint64(w.opts.RetainSegments)
	segments, err := listSegments(w.opts.Dir)
	if err != nil {
		return
	}
	removed := false
	for _, segment := range segments {
		if segment >= removeBefore {
			break
		}
		path := filepath.Join(w.opts.Dir, segmentName(segment))
		var size int64
		if info, statErr := os.Stat(path); statErr == nil {
			size = info.Size()
		}
		if os.Remove(path) == nil {
			removed = true
			w.diskBytes.Add(-size)
		}
	}
	if removed {
		_ = syncDirectory(w.opts.Dir)
	}
}

func encodeFrame(payload []byte) []byte {
	frame := make([]byte, frameHeaderSize+len(payload))
	binary.BigEndian.PutUint32(frame[0:4], frameMagic)
	binary.BigEndian.PutUint16(frame[4:6], frameVersion)
	binary.BigEndian.PutUint32(frame[8:12], uint32(len(payload)))
	binary.BigEndian.PutUint32(frame[12:16], crc32.ChecksumIEEE(payload))
	binary.BigEndian.PutUint32(frame[16:20], crc32.ChecksumIEEE(frame[:16]))
	copy(frame[frameHeaderSize:], payload)
	return frame
}

func scanFramesEnd(file *os.File, segment uint64, limit int64, maxRecord int) (int64, error) {
	lastGood := int64(0)
	err := scanFrames(file, segment, limit, maxRecord, true, func(fence corenest.CommitFence, _ []byte) error {
		lastGood = fence.Offset
		return nil
	})
	return lastGood, err
}

func scanFrames(file *os.File, segment uint64, limit int64, maxRecord int, allowTornTail bool, consume func(corenest.CommitFence, []byte) error) error {
	return scanFramesFrom(file, segment, 0, limit, maxRecord, allowTornTail, consume)
}

// scanFramesFrom scans frames starting at the given offset, which must be a
// frame boundary (0 or the end offset of a previously scanned frame, e.g. an
// acknowledgement fence).
func scanFramesFrom(file *os.File, segment uint64, start, limit int64, maxRecord int, allowTornTail bool, consume func(corenest.CommitFence, []byte) error) error {
	if limit < 0 {
		info, err := file.Stat()
		if err != nil {
			return err
		}
		limit = info.Size()
	}
	offset := start
	header := make([]byte, frameHeaderSize)
	for offset < limit {
		n, err := file.ReadAt(header, offset)
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		if n < frameHeaderSize {
			if allowTornTail {
				return nil
			}
			return errors.Join(ErrCorrupt, io.ErrUnexpectedEOF)
		}
		if binary.BigEndian.Uint32(header[0:4]) != frameMagic || binary.BigEndian.Uint16(header[4:6]) != frameVersion {
			return errors.Join(ErrCorrupt, errors.New("invalid frame header"))
		}
		if crc32.ChecksumIEEE(header[:16]) != binary.BigEndian.Uint32(header[16:20]) {
			return errors.Join(ErrCorrupt, errors.New("invalid frame header checksum"))
		}
		size := int(binary.BigEndian.Uint32(header[8:12]))
		if size <= 0 || size > maxRecord {
			return errors.Join(ErrCorrupt, ErrRecordTooLarge)
		}
		end := offset + frameHeaderSize + int64(size)
		if end > limit {
			if allowTornTail {
				return nil
			}
			return errors.Join(ErrCorrupt, io.ErrUnexpectedEOF)
		}
		payload := make([]byte, size)
		if _, err := file.ReadAt(payload, offset+frameHeaderSize); err != nil {
			return err
		}
		if crc32.ChecksumIEEE(payload) != binary.BigEndian.Uint32(header[12:16]) {
			return errors.Join(ErrCorrupt, errors.New("invalid frame payload checksum"))
		}
		if consume != nil {
			if err := consume(corenest.CommitFence{Segment: segment, Offset: end}, payload); err != nil {
				return err
			}
		}
		offset = end
	}
	return nil
}

func listSegments(dir string) ([]uint64, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	segments := make([]uint64, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasPrefix(name, "segment-") || !strings.HasSuffix(name, ".wal") {
			continue
		}
		raw := strings.TrimSuffix(strings.TrimPrefix(name, "segment-"), ".wal")
		segment, err := strconv.ParseUint(raw, 10, 64)
		if err != nil || segment == 0 {
			continue
		}
		segments = append(segments, segment)
	}
	sort.Slice(segments, func(i, j int) bool { return segments[i] < segments[j] })
	return segments, nil
}

func segmentName(segment uint64) string {
	return fmt.Sprintf("segment-%020d.wal", segment)
}

func writeFull(writer io.Writer, raw []byte) error {
	for len(raw) > 0 {
		n, err := writer.Write(raw)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		raw = raw[n:]
	}
	return nil
}
