// Package nestwal 是 Nest 事务的单写者分段提交日志。
//
// 记录带校验和、按批组提交（group commit）、按追加顺序回放，确认进度保存在双槽轮换的
// 持久 checkpoint 中。目录由 writer.lock 独占，同一时刻只允许一个进程写入。
package nestwal

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/dataengine"
	corenest "github.com/tjbdwanghaibo/roost-core/framework/nest"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
	"github.com/tjbdwanghaibo/roost-core/internal/operation"
)

const (
	frameMagic      = uint32(0x5253574c) // RSWL
	frameVersion    = uint16(1)
	frameHeaderSize = 20
)

var (
	// ErrClosed 表示 WAL 已开始关闭，不再接受追加、确认或回放。
	ErrClosed = errors.New("nestwal: closed")
	// ErrLocked 表示目录的 writer.lock 已被其他进程持有。
	ErrLocked = errors.New("nestwal: directory is already locked")
	// ErrCorrupt 表示日志或 checkpoint 无法按格式解析；不会自动跳过坏数据。
	ErrCorrupt = errors.New("nestwal: corrupt log")
	// ErrRecordTooLarge 表示编码后的记录超过 Options.MaxRecordBytes。
	ErrRecordTooLarge = errors.New("nestwal: record exceeds configured limit")
	// ErrCapacity 是准入拒绝：磁盘上限或追加队列已满，记录未写入。
	ErrCapacity = errors.New("nestwal: disk capacity limit reached")
	// errStatsStop 让 oldestUnackedAge 读到第一条未确认记录后停止回放。
	errStatsStop = errors.New("nestwal: stats stop")
)

// Options 配置一个 WAL 目录。零值字段在 Open 时取 DefaultOptions 的值。
type Options struct {
	Dir                 string
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

	// 以下是包内测试缝，生产路径均为 nil。
	//
	// beforeProcessBatch 在 collectBatch 与 processBatch 之间调用：此时 writer 持有已收集的批、
	// 不持任何锁，测试可以把 writer 停在这里。
	beforeProcessBatch func()
	// syncFile 替代段文件 fsync，用来注入 EIO 与 Linux“同一 fd 的写回错误只报告一次”语义。
	syncFile func(*os.File) error
	// afterAckAdmitted 在 Ack 进入 operations 生命周期后立即调用，让测试把在途 Ack 停在 Close 之前。
	afterAckAdmitted func()
}

// DefaultOptions 返回 dir 目录的默认配置。
func DefaultOptions(dir string) Options {
	return Options{
		Dir:                 dir,
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

// Stats 是 WAL 的瞬时观测值，只用于监控与诊断，不能代替原子准入判断。
type Stats struct {
	Segment uint64
	Offset  int64
	Queued  int
	// Admitted 是 Open 以来进入追加队列的记录数。Admitted − Appended 是在途记录（排队中或已被
	// writer 取成批、尚未写入）；writer 取走一批后 Queued 已看不到这部分。
	Admitted         uint64
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

// WAL 是单写者分段提交日志。追加经 appendCh 交给唯一的 writer goroutine 成批写入；
// Ack / Replay 受 operations 生命周期约束，Close 等它们全部退出后才释放目录锁。
//
// 锁顺序：lifecycleMu → enqueueMu → ticketMu；checkpointMu → stateMu → ticketMu。
type WAL struct {
	operations operation.Lifetime
	opts       Options

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
	terminalErr  atomic.Pointer[terminalState] // 只写一次，热路径免锁读
	terminated   chan struct{}                 // 首个 terminal 错误写入后关闭（Terminated）
	fatalOnce    sync.Once
	replayMu     sync.Mutex
	checkpointMu sync.Mutex
	checkpoint   checkpointState

	appended     atomic.Uint64
	admitted     atomic.Uint64
	bytesWritten atomic.Uint64
	syncs        atomic.Uint64
	replayed     atomic.Uint64
	acked        atomic.Uint64
	diskBytes    atomic.Int64
	batchBuffers sync.Pool

	// pipelined 提交状态（见 docs/framework/NEST_PIPELINED_COMMIT.md）：
	//   - enqueueMu 让 LSN 分配与入队原子化，LSN 顺序即物理日志顺序；
	//   - reservedBytes 把容量检查前移到 Enqueue，Enqueue 是票据记录唯一的拒绝点；
	//   - writtenLSN（stateMu 保护）是已物理写入的最大票据 LSN，fsync 成功后据此完成票据。
	enqueueMu     sync.Mutex
	nextLSN       uint64 // guarded by enqueueMu
	reservedBytes atomic.Int64
	writtenLSN    uint64 // guarded by stateMu
	durableLSN    atomic.Uint64
	ticketMu      sync.Mutex
	tickets       []*walTicket // FIFO, ascending LSN

	// 最老未确认记录的 CreatedAt 缓存，避免每次健康检查都读盘。缓存有效期内由 CreatedAt
	// 推算的年龄仍然精确（随时钟增长）；只有 Ack 能让最老记录前移，所以 Ack 使缓存失效。
	statsCacheMu       sync.Mutex
	statsOldestCreated int64
	statsOldestValid   time.Time
}

const statsOldestCacheTTL = 5 * time.Second

// appendRequest 是交给 writer 的一项工作：一条待写记录，或一个 Sync 屏障。
type appendRequest struct {
	record      corenest.CommitRecord
	frame       []byte
	requireSync bool
	reserved    bool // Enqueue 已预留容量，writer 不得再拒绝
	lsn         uint64
	done        chan appendResult
	// barrier 标记 Sync 屏障：不带帧，结束它所在的批；排在它之前的请求全部写入（或以 terminal
	// 失败）后才回复。
	barrier bool
}

// walTicket 实现 corenest.CommitTicket。err 在关闭 done 之前写入、只在 done 关闭之后读取，因此不需要锁。
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

// Open 取得目录锁、加载 checkpoint、修复最后一段的可截断尾部，然后启动 writer。
// 已确认前缀内的任何损坏都拒绝启动，且不改动文件。
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
		terminated: make(chan struct{}),
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

// Append 是阻塞式提交入口（Projector.Commit / CommitSystem）：记录写入所在批后返回 fence。
// async 只等写入，由组提交 ticker 周期刷盘；strict 与 pipelined 等所在批 fsync 成功后才返回。
// ctx 只约束入队；入队后必须等 writer 给出结论，否则提交结果不确定。
func (w *WAL) Append(ctx context.Context, record corenest.CommitRecord) (corenest.CommitFence, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	payload, err := encodeRecord(record)
	if err != nil {
		return corenest.CommitFence{}, err
	}
	if len(payload) > w.opts.MaxRecordBytes {
		return corenest.CommitFence{}, ErrRecordTooLarge
	}
	frame := encodeFrame(payload)
	req := appendRequest{
		record: record,
		frame:  frame,
		// 经 Append 到达的 pipelined 记录来自回退到 strict 路径的事务（broadcast 未提前放锁、带 Remote
		// 批次的两阶段协议，见 nest/execution.go）：Nest 在 Append 返回后就放锁、Confirm Sync、执行
		// AfterCommit，所以必须与 strict 一样等 fsync，不能让成功先于持久对外可见。正常 pipelined
		// 记录走 Enqueue，不经这里；encodeRecord 已拒绝大于 pipelined 的取值。历史：RR-20260928-11。
		requireSync: record.Durability >= dataengine.DurabilityStrict,
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
		w.admitted.Add(1)
		w.lifecycleMu.RUnlock()
	case <-ctx.Done():
		w.lifecycleMu.RUnlock()
		return corenest.CommitFence{}, ctx.Err()
	}
	// 取消只作用于入队；已入队后提前返回会让提交结果不确定，所以无条件等 writer 回复。
	result := <-req.done
	return result.fence, result.err
}

// Enqueue 准入一条 pipelined 记录，返回在记录持久后完成的票据。
//
// 调用方持有实体锁，所以全部可拒绝的检查（编码、大小、容量预留、terminal、入队）都在这里
// 同步完成，且不等待 I/O：队列满即同步拒绝。成功返回后唯一可能的失败是票据上的
// ErrCommitIndeterminate，经 WAL terminal 路径给出。
func (w *WAL) Enqueue(ctx context.Context, record corenest.CommitRecord) (corenest.CommitTicket, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	payload, err := encodeRecord(record)
	if err != nil {
		return nil, err
	}
	if len(payload) > w.opts.MaxRecordBytes {
		return nil, ErrRecordTooLarge
	}
	frame := encodeFrame(payload)
	// 现在就预留容量：批写入时调用方已放弃回滚权利，processBatch 不能再拒绝票据记录。
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
	// enqueueMu 让 LSN 分配、入队与票据登记原子化：LSN 顺序即日志顺序，票据 FIFO 保持升序。
	// 入队不阻塞，队列满是同步拒绝（调用方持有实体锁）。
	w.enqueueMu.Lock()
	req.lsn = w.nextLSN + 1
	select {
	case w.appendCh <- req:
		w.admitted.Add(1)
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

// DurableLSN 是 pipelined 提交水位：LSN 不大于它的票据记录都已持久。
func (w *WAL) DurableLSN() uint64 {
	return w.durableLSN.Load()
}

// resolveDurableLocked 完成最近一次成功 fsync 覆盖的全部票据。调用方持 stateMu，ticketMu 嵌套在内。
func (w *WAL) resolveDurableLocked() {
	upto := w.writtenLSN
	if upto == 0 {
		return
	}
	w.ticketMu.Lock()
	// 先发布水位、再唤醒等待方：票据完成即承诺 DurableLSN 已覆盖它，外部化门在 Done() 触发后
	// 立即读水位。水位推进与是否有人等待无关，所以无条件更新。
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

// failPendingTickets 以 terminal 错误完成全部待决票据：WAL 被 fence 后已入队记录的写入结果
// 未知，只能判为不确定。例外是覆盖它的 fsync 完成 FIFO 之后才登记的票据：它的 LSN 在
// DurableLSN 之内，而 DurableLSN 只可能由 terminal 之前的成功 fsync 发布，所以按已持久完成。
func (w *WAL) failPendingTickets(err error) {
	w.ticketMu.Lock()
	durable := w.durableLSN.Load()
	for _, ticket := range w.tickets {
		if ticket.lsn > durable {
			ticket.err = err
		}
		close(ticket.done)
	}
	w.tickets = nil
	w.ticketMu.Unlock()
}

// Ack 把确认进度推进到 fence 并持久化 checkpoint，然后清理已确认的旧段。
// fence 不前进时返回 nil；超过日志末尾或 WAL 已 terminal 时拒绝。
func (w *WAL) Ack(_ context.Context, fence corenest.CommitFence) error {
	if !w.operations.Begin() {
		return ErrClosed
	}
	defer w.operations.End()
	if w.opts.afterAckAdmitted != nil {
		w.opts.afterAckAdmitted()
	}

	if fence.Segment == 0 || fence.Offset <= 0 {
		return errors.New("nestwal: invalid acknowledgement fence")
	}
	w.checkpointMu.Lock()
	defer w.checkpointMu.Unlock()
	// terminal 之后任何 fsync“成功”都不能证明失败那批页已落盘，checkpoint 不得再前进。
	// 顺序：checkpointMu 内先查 terminal，再刷段，最后存 checkpoint。
	if terminal := w.terminal(); terminal != nil {
		return terminal
	}
	if !fenceAfter(fence, w.checkpoint.fence) {
		return nil
	}
	w.stateMu.RLock()
	beyondEnd := fence.Segment > w.segment || fence.Segment == w.segment && fence.Offset > w.offset
	w.stateMu.RUnlock()
	if beyondEnd {
		return errors.New("nestwal: acknowledgement is beyond log end")
	}
	// checkpoint 持久化前先刷覆盖段，不能让 async 的 ack 超过已落盘内容。
	// 失败已在 syncActiveLocked 内粘滞为 ErrCommitIndeterminate terminal。
	if err := w.syncActive(); err != nil {
		return err
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

// Replay 按追加顺序把 checkpoint 之后的记录交给 consume，扫描截止到调用时的日志末尾。
// consume 返回错误即停止并原样返回；多次回放由 replayMu 串行。
func (w *WAL) Replay(ctx context.Context, consume func(corenest.CommitFence, corenest.CommitRecord) error) error {
	if !w.operations.Begin() {
		return ErrClosed
	}
	defer w.operations.End()

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
		// 确认 fence 是帧结束偏移，可作为扫描起点：跳过已完全确认的段和 fence 所在段的已确认前缀，
		// 不在每次回放时重读、重校验。
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

// Sync 把已准入的异步追加刷到稳定存储：Sync 调用前已返回票据的 Enqueue 记录，在 Sync 返回
// nil 时都已持久。
//
// 票据记录可能还在 appendCh 或 writer 正在收集的批里（BatchDelay 窗口），fsync 当前段覆盖
// 不到尚未写入的字节。所以 Sync 先经同一队列送一个屏障：FIFO 保证 writer 回复屏障时排在它
// 之前的记录都已写入，然后才 fsync 当前段。等待受 ctx 约束；取消的 Sync 不改变任何状态，
// 下一次 Sync 会重做。
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
	if err := w.awaitWriteBarrier(ctx); err != nil {
		return err
	}
	// 失败已在 syncActiveLocked 内粘滞为 ErrCommitIndeterminate terminal。
	return w.syncActive()
}

// awaitWriteBarrier 入队一个屏障并等待 writer 回复。
//
// enqueueMu 下取的 nextLSN 对票据是精确边界：LSN 不大于它的记录都已在 appendCh 中。所以
// durableLSN 已覆盖边界且队列为空时无需等待，省去一次往返；否则屏障与记录走同一 FIFO。
//
// 关闭分两步：Close 置 closed 只停止准入；writer 排空全部队列并 sync 后才关闭 doneCh。两者
// 之间 writer 可能仍持有未写入的批，屏障不能视为已满足；此时照常发送屏障，drainAndClose
// 会回复队列中的全部请求。只有 doneCh 关闭后屏障才天然满足，这时以 WAL 的 terminal 状态
// 作答——最后一次 sync 失败不能读成成功。这样干净关闭后 Sync 仍幂等，又不会抢在排空之前返回。
// 历史：RR-20260914-01（曾在 closed 时直接返回 nil）。
func (w *WAL) awaitWriteBarrier(ctx context.Context) error {
	w.enqueueMu.Lock()
	boundary := w.nextLSN
	w.enqueueMu.Unlock()
	if w.durableLSN.Load() >= boundary && len(w.appendCh) == 0 {
		return nil
	}
	req := appendRequest{barrier: true, done: make(chan appendResult, 1)}
	select {
	case w.appendCh <- req:
	case <-ctx.Done():
		return ctx.Err()
	case <-w.doneCh:
		return w.terminal()
	}
	select {
	case result := <-req.done:
		return result.err
	case <-ctx.Done():
		return ctx.Err()
	case <-w.doneCh:
		// writer 只在 drainAndClose 回复全部排队请求后退出，所以回复已在缓冲中。
		select {
		case result := <-req.done:
			return result.err
		default:
			return w.terminal()
		}
	}
}

// Stats 返回当前计数与磁盘占用；最老未确认记录年龄来自 5 秒缓存。
func (w *WAL) Stats() Stats {
	w.stateMu.RLock()
	segment, offset := w.segment, w.offset
	w.stateMu.RUnlock()
	stats := Stats{
		Segment:      segment,
		Offset:       offset,
		Queued:       len(w.appendCh),
		Admitted:     w.admitted.Load(),
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

// Healthy 在 terminal、磁盘占用超限或最老未确认记录超龄时返回错误。
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

// Close 停止准入并等待 writer 排空、刷盘、释放目录锁。首次调用发起关闭，之后每次调用都在
// 自己的 ctx 内等待同一结果；超时返回 ctx 错误，关闭仍在后台继续。
func (w *WAL) Close(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	w.closeOnce.Do(func() {
		w.lifecycleMu.Lock()
		w.closed = true
		w.operations.Stop()
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

// writerLoop 是唯一的 writer：按批写入追加请求，周期 fsync 组提交，收到关闭信号后排空退出。
func (w *WAL) writerLoop() {
	ticker := time.NewTicker(w.opts.GroupCommitInterval)
	defer ticker.Stop()
	defer close(w.doneCh)
	for {
		select {
		case first := <-w.appendCh:
			batch := w.collectBatch(first)
			if w.opts.beforeProcessBatch != nil {
				w.opts.beforeProcessBatch()
			}
			w.processBatch(batch)
		case <-ticker.C:
			// 失败在 syncActiveLocked 内粘滞为 terminal；terminal 后这里不再 fsync。
			_ = w.syncActive()
		case <-w.closeCh:
			w.drainAndClose()
			return
		}
	}
}

// collectBatch 以 first 开始收集一批请求，直到达到记录数或字节上限、BatchDelay 到期、遇到
// Sync 屏障或开始关闭。
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
			if req.barrier {
				// 有 Sync 在等：立即结束本批，不让屏障再等完 BatchDelay。
				return batch
			}
		case <-timer.C:
			return batch
		case <-w.closeCh:
			return batch
		}
	}
	return batch
}

// processBatch 写入一批记录，然后回复同批收集到的 Sync 屏障。屏障最后回复，保证“屏障已回复”
// 意味着“排在它之前的记录都已写入”；WAL 进入 terminal 时屏障带回该错误。
func (w *WAL) processBatch(batch []appendRequest) {
	if len(batch) == 0 {
		return
	}
	var barriers []appendRequest
	records := batch[:0]
	for i := range batch {
		if batch[i].barrier {
			barriers = append(barriers, batch[i])
			continue
		}
		records = append(records, batch[i])
	}
	err := w.processRecords(records)
	for i := range barriers {
		barriers[i].done <- appendResult{err: err}
	}
}

// processRecords 写入一批记录并回复每个请求，返回让整批失败的 terminal 错误（若有）。
// 步骤：terminal 检查 → 未预留记录的容量准入 → 写盘与 fsync → 回复。
// 未预留记录的容量拒绝不是持久失败，不经返回值报告。
func (w *WAL) processRecords(batch []appendRequest) error {
	if len(batch) == 0 {
		return w.terminal()
	}
	if terminal := w.terminal(); terminal != nil {
		for i := range batch {
			batch[i].done <- appendResult{err: terminal}
		}
		// 与 terminal 转换并发登记的票据可能错过 setTerminal 的清扫，它的记录落到这里，
		// 所以再清扫一次，不让等待方悬挂。
		w.failPendingTickets(terminal)
		return terminal
	}
	batch, reservedBytes := w.admitUnreserved(batch)
	if len(batch) == 0 {
		return nil
	}
	// 记录进入写路径后预留的使命结束：此后字节计入 diskBytes，下面的任何失败都是 terminal 而非拒绝。
	if reservedBytes > 0 {
		w.reservedBytes.Add(-reservedBytes)
	}
	fences := make([]corenest.CommitFence, len(batch))
	if err := w.writeBatch(batch, fences); err != nil {
		err = indeterminate(err)
		w.setTerminal(err)
		for i := range batch {
			batch[i].done <- appendResult{err: err}
		}
		return err
	}
	bytes := int64(0)
	for i := range batch {
		w.appended.Add(1)
		w.bytesWritten.Add(uint64(len(batch[i].frame)))
		bytes += int64(len(batch[i].frame))
		batch[i].done <- appendResult{fence: fences[i]}
	}
	// 每批指标：记录数 / 批数是组提交放大倍数，磁盘字节是保留压力。
	metrics.IncCounter("nestwal.batch.total", nil, 1)
	metrics.IncCounter("nestwal.append.total", nil, int64(len(batch)))
	metrics.IncCounter("nestwal.bytes.total", nil, bytes)
	metrics.SetGauge("nestwal.disk.bytes", nil, w.diskBytes.Load())
	return nil
}

// admitUnreserved 对批中未预留容量的记录做磁盘上限准入，超限时整体拒绝这些记录并回复
// ErrCapacity。票据记录已在 Enqueue 预留容量、调用方已据此放锁，这里不能再拒绝。
// 返回保留下来的批（复用 batch 底层数组）和其中预留记录的总字节数。
func (w *WAL) admitUnreserved(batch []appendRequest) ([]appendRequest, int64) {
	unreservedBytes := int64(0)
	reservedBytes := int64(0)
	for i := range batch {
		if batch[i].reserved {
			reservedBytes += int64(len(batch[i].frame))
		} else {
			unreservedBytes += int64(len(batch[i].frame))
		}
	}
	if w.opts.MaxDiskBytes <= 0 || unreservedBytes == 0 ||
		w.diskBytes.Load()+w.reservedBytes.Load()+unreservedBytes <= w.opts.MaxDiskBytes {
		return batch, reservedBytes
	}
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
	return kept, reservedBytes
}

// writeBatch 在 stateMu 内把整批帧按段写入（段满先轮转），推进 writtenLSN；批内有记录
// 要求持久时再 fsync 当前段。fences[i] 填写 batch[i] 的结束位置。返回的错误都意味着写入
// 结果不确定，由调用方转为 terminal。
func (w *WAL) writeBatch(batch []appendRequest, fences []corenest.CommitFence) error {
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
		// 取出能放进当前段的最长前缀，一次写入。
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
	return err
}

// drainAndClose 在 Close 后由 writer 调用：写完队列中剩余的请求，刷盘并关闭当前段，等
// Ack / Replay 全部退出后释放目录锁。
func (w *WAL) drainAndClose() {
	for {
		batch := w.takeQueued()
		if len(batch) == 0 {
			break
		}
		w.processBatch(batch)
	}
	if err := w.syncAndCloseActive(); err != nil {
		// 最后一次 sync 失败时已入队记录的结果未知：进入 terminal 并让剩余票据失败，
		// 不让 pipelined 等待方在关闭后悬挂。
		w.setTerminal(indeterminate(err))
		w.closeErr = errors.Join(w.closeErr, err)
	}
	w.closeErr = errors.Join(w.closeErr, w.terminal())
	<-w.operations.Stop() // Replay/Ack 全部退出前不能交出目录锁。
	if w.lockHandle != nil {
		w.closeErr = errors.Join(w.closeErr, unlockFile(w.lockHandle), w.lockHandle.Close())
	}
}

// takeQueued 非阻塞地取出至多 BatchMaxRecords 个已排队请求；队列为空时返回空批。
func (w *WAL) takeQueued() []appendRequest {
	batch := make([]appendRequest, 0, w.opts.BatchMaxRecords)
	for len(batch) < w.opts.BatchMaxRecords {
		select {
		case req := <-w.appendCh:
			batch = append(batch, req)
		default:
			return batch
		}
	}
	return batch
}

// openActive 打开最后一段作为当前段：检查段号连续、修复可截断尾部并定位到段末。
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
	lastGood, err := w.recoverTail(file, info.Size())
	if err != nil {
		_ = file.Close()
		return err
	}
	if _, err := file.Seek(lastGood, io.SeekStart); err != nil {
		_ = file.Close()
		return err
	}
	w.active = file
	w.offset = lastGood
	return nil
}

// tailBlockSize 是零尾判据的对齐块：页缓存按页写回，断电丢写回表现为整块读出为 0。
const tailBlockSize = 4096

// recoverTail 扫描最后一段并决定可截断的尾部，返回恢复后的段末偏移。
//
// 只有最后一段会被修复，此前的段在轮转时已 fsync，其中任何损坏都由 Replay 报 ErrCorrupt。
// 可截断的尾部只有两种：
//   - 不完整尾帧（帧头不足 20 字节，或帧体越过文件末尾）——崩溃时未写完的最后一帧；
//   - 零填充尾：第一个无法解析的帧起点（坏点）至文件末尾全为 0，即坏帧头所在 4 KiB 块的剩余部分
//     及其后所有块全零——文件长度已持久而数据块未写回。CRC 不符的非零数据仍是 ErrCorrupt。
//
// 两种情形都要求坏点不早于 checkpoint fence：fence 是最后一个已确认帧的结束偏移，坏点 ≥ fence 表示
// 截掉的内容从未被确认。否则拒绝启动，且拒绝前不改动文件。截断后告警并计指标（reason 仅 torn_frame / zero_fill）。
func (w *WAL) recoverTail(file *os.File, size int64) (int64, error) {
	lastGood, scanErr := scanFramesEnd(file, w.segment, size, w.opts.MaxRecordBytes)
	if scanErr != nil && !errors.Is(scanErr, ErrCorrupt) {
		return 0, scanErr
	}
	if lastGood == size {
		return lastGood, w.checkRecoveredEnd(lastGood, nil)
	}
	zeroTail, err := allZeroFrom(file, lastGood, size)
	if err != nil {
		return 0, err
	}
	if scanErr != nil && !zeroTail {
		return 0, scanErr
	}
	if err := w.checkRecoveredEnd(lastGood, scanErr); err != nil {
		return 0, err
	}
	if err := file.Truncate(lastGood); err != nil {
		return 0, err
	}
	reason := "torn_frame"
	if zeroTail {
		reason = "zero_fill"
	}
	truncated := size - lastGood
	metrics.IncCounter("nestwal.recovery.tail_truncated.total", metrics.Labels{"reason": reason}, 1)
	metrics.IncCounter("nestwal.recovery.tail_truncated.bytes", metrics.Labels{"reason": reason}, truncated)
	slog.Warn("nestwal: truncated unacknowledged WAL tail on open",
		"reason", reason, "segment", w.segment, "offset", lastGood, "bytes", truncated,
		"block_offset", lastGood-lastGood%tailBlockSize,
		"checkpoint_segment", w.checkpoint.fence.Segment, "checkpoint_offset", w.checkpoint.fence.Offset)
	return lastGood, nil
}

// checkRecoveredEnd 拒绝恢复末端早于 checkpoint fence 的日志：那意味着已确认的记录丢失或损坏。
func (w *WAL) checkRecoveredEnd(end int64, scanErr error) error {
	if !fenceAfter(w.checkpoint.fence, corenest.CommitFence{Segment: w.segment, Offset: end}) {
		return nil
	}
	if scanErr != nil {
		return errors.Join(scanErr, fmt.Errorf("corrupt frame at segment %d offset %d is inside the acknowledged prefix (checkpoint %d/%d)",
			w.segment, end, w.checkpoint.fence.Segment, w.checkpoint.fence.Offset))
	}
	return errors.Join(ErrCorrupt, errors.New("acknowledgement is beyond recovered WAL end"))
}

// allZeroFrom 报告 [offset, size) 是否全为 0。
func allZeroFrom(file *os.File, offset, size int64) (bool, error) {
	buffer := make([]byte, 64<<10)
	for offset < size {
		chunk := buffer[:min(int64(len(buffer)), size-offset)]
		n, err := file.ReadAt(chunk, offset)
		if n < len(chunk) {
			if err == nil {
				err = io.ErrUnexpectedEOF
			}
			return false, err
		}
		for _, b := range chunk {
			if b != 0 {
				return false, nil
			}
		}
		offset += int64(n)
	}
	return true, nil
}

// rotateLocked 刷盘并关闭当前段，创建下一段并持久化目录项。调用方持 stateMu。
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
	if err := w.fsyncSegment(file); err != nil {
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

// syncActiveLocked 刷当前段并解析被覆盖的票据，调用方持 stateMu。Ack、ticker、Sync、
// 批量写、轮转和关闭排空都经由这里刷段。
//
// fsync 失败是粘滞的：Linux 同一 fd 的写回错误只报告一次，之后的 fsync
// 返回 0 并不证明失败那批页已落盘。所以失败时在 stateMu 内立即进入 ErrCommitIndeterminate
// terminal；terminal 之后不再 fsync、不清 unsynced、不推进 DurableLSN，直接返回 terminal。
func (w *WAL) syncActiveLocked() error {
	if terminal := w.terminal(); terminal != nil {
		// 与 setTerminal 清扫并发登记的票据在这里兜底，不让等待方悬挂。
		w.failPendingTickets(terminal)
		return terminal
	}
	if !w.unsynced {
		// 已写内容都已持久（干净关闭后 active 为 nil 也属此类）；晚登记、已被当前水位覆盖的票据
		// 仍要在这里完成。
		w.resolveDurableLocked()
		return nil
	}
	if w.active == nil {
		// 段已关闭仍有未刷写入：只有关闭时最后一次 sync 失败才会如此，而那时 terminal 已在上面
		// 返回；这里防御性兜底，绝不当作“已全部落盘”。
		err := indeterminate(errors.New("nestwal: segment closed with unsynced writes"))
		w.setTerminal(err)
		return err
	}
	if err := w.fsyncSegment(w.active); err != nil {
		err = indeterminate(err)
		w.setTerminal(err)
		return err
	}
	w.unsynced = false
	w.syncs.Add(1)
	w.resolveDurableLocked()
	return nil
}

// fsyncSegment 刷段文件；测试缝 syncFile 仅在包内测试设置。
func (w *WAL) fsyncSegment(file *os.File) error {
	if w.opts.syncFile != nil {
		return w.opts.syncFile(file)
	}
	return file.Sync()
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

// indeterminate 把物理写 / fsync 失败归为提交结果不确定；已归类的错误原样返回，避免重复包装。
func indeterminate(err error) error {
	if errors.Is(err, corenest.ErrCommitIndeterminate) {
		return err
	}
	return errors.Join(corenest.ErrCommitIndeterminate, err)
}

// terminalState 包一层，使 atomic.Pointer 能以 CompareAndSwap(nil, …) 表达“首个错误胜出”。
type terminalState struct{ err error }

func (w *WAL) terminal() error {
	if state := w.terminalErr.Load(); state != nil {
		return state.err
	}
	return nil
}

// Terminated 在 WAL 进入 terminal（物理写入或 fsync 结果不确定，错误 errors.Is
// corenest.ErrCommitIndeterminate）后关闭，之后 Healthy 返回该错误。只报告 terminal，
// 正常 Close 不关闭它。供依赖 WAL 的等待方（例如 DataEngine 投影等待）立即感知“本进程
// 不会再确认任何记录”，而不是轮询 Healthy。
func (w *WAL) Terminated() <-chan struct{} {
	return w.terminated
}

// setTerminal 记录首个 terminal 错误：完成全部待决票据、关闭 Terminated，并异步通知 OnFatal。
// 之后的错误不覆盖首个错误。
func (w *WAL) setTerminal(err error) {
	if err == nil {
		return
	}
	first := w.terminalErr.CompareAndSwap(nil, &terminalState{err: err})
	if first {
		// fence 后已入队记录的写入结果未知，等待方必须看到 terminal 结论而不是永远阻塞。
		w.failPendingTickets(err)
		close(w.terminated)
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

// pruneAcked 删除确认段之前、超出 RetainSegments 保留数的旧段。删除失败只是少回收空间，不报错。
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

// encodeFrame 生成一帧：20 字节帧头（magic、版本、长度、payload CRC、帧头 CRC）+ payload。
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

// scanFramesEnd 从段首扫描到 limit，返回最后一个完整合法帧的结束偏移；不完整尾帧不算错误。
func scanFramesEnd(file *os.File, segment uint64, limit int64, maxRecord int) (int64, error) {
	lastGood := int64(0)
	err := scanFramesFrom(file, segment, 0, limit, maxRecord, true, func(fence corenest.CommitFence, _ []byte) error {
		lastGood = fence.Offset
		return nil
	})
	return lastGood, err
}

// scanFramesFrom 从 start 开始逐帧校验并交给 consume。start 必须是帧边界（0 或某帧的结束偏移，
// 例如确认 fence）；limit < 0 表示扫到文件末尾。allowTornTail 为 true 时不完整尾帧视为正常结束，
// 否则报 ErrCorrupt。
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
	slices.Sort(segments)
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
