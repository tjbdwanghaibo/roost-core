package nettransport

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// SendError is what the ErrorHandler sees when a downstream send fails.
type SendError struct {
	Session SessionID
	Err     error
}

func (sendError SendError) Error() string {
	if sendError.Err == nil {
		return "nettransport: unknown send error"
	}
	return sendError.Err.Error()
}
func (sendError SendError) Unwrap() error { return sendError.Err }

// ErrorHandler must return promptly. Panics are contained and counted, but a
// blocking handler still blocks the affected session's worker.
type ErrorHandler func(SendError)

type AsyncTransportConfig struct {
	MaxSessions       int
	ReliableQueueSize int
	MaxReliableBytes  int
	// MaxQueuedReliableBytes 限制每会话待发送字节，不含正在发送的一条；0 保持旧行为。
	MaxQueuedReliableBytes int
	// MaxResidentReliableBytes 限制全部会话可靠消息的驻留字节（排队+在途）；0 关闭。
	MaxResidentReliableBytes int64
	// MaxReliableAge 从入队开始计时，包含排队与发送；0 仅使用 SendTimeout。
	MaxReliableAge time.Duration
	SendTimeout    time.Duration
	OnError        ErrorHandler
}

func DefaultAsyncTransportConfig() AsyncTransportConfig {
	return AsyncTransportConfig{
		MaxSessions:       4096,
		ReliableQueueSize: 256,
		MaxReliableBytes:  1 << 20,
		SendTimeout:       250 * time.Millisecond,
	}
}

// AsyncTransport is a per-session bounded queue in front of a reliable,
// ordered downstream (KCP / QUIC stream, or any ReliableSender). It exists so
// that one slow receiver never blocks the tick that produced its frame:
// SendReliable returns as soon as the message is queued, a worker per
// session drains in order, and a full queue is refused with
// ErrReliableBackpressure — which entitysync treats as that session's
// failure. A downstream send error is terminal for the session.
//
// It has exactly one lane. The latest-only datagram lane the old Replicator
// used was removed (ARCH-12, M-18): a frame that carries only what changed
// cannot survive being replaced by the next one. Unreliable sends go straight
// to a DatagramSender (lockstep does this).
type AsyncTransport struct {
	mu         sync.RWMutex
	downstream ReliableSender
	config     AsyncTransportConfig
	ctx        context.Context
	cancel     context.CancelFunc
	sessions   map[SessionID]*sessionQueue
	closed     bool
	wait       sync.WaitGroup
	closeOnce  sync.Once
	closeDone  chan struct{}
	stats      asyncCounters
}

type queuedReliable struct {
	data     []byte
	queuedAt time.Time
}

type sessionQueue struct {
	id     SessionID
	owner  *AsyncTransport
	ctx    context.Context
	cancel context.CancelFunc

	mu          sync.Mutex
	closing     bool
	failure     error
	reliable    []queuedReliable
	head        int
	queuedBytes int64
	busyBytes   int64
	busySince   time.Time
	busy        bool
	wake        chan struct{}
}

type asyncCounters struct {
	reliableQueued       atomic.Uint64
	reliableSent         atomic.Uint64
	reliableBytesSent    atomic.Uint64
	reliableBackpressure atomic.Uint64
	reliableAbandoned    atomic.Uint64
	sendErrors           atomic.Uint64
	handlerPanics        atomic.Uint64
	residentBytes        atomic.Int64
	activeSessions       atomic.Int64
	globalBackpressure   atomic.Uint64
	expired              atomic.Uint64
}

type AsyncTransportStats struct {
	PendingReliableBytes  int64
	ReliableBytesInFlight int64
	// OldestReliableAge 包含队列头与正在发送的消息，自准入时计时。
	OldestReliableAge     time.Duration
	ActiveSessions        int
	DrainingSessions      int
	PendingReliable       int
	ReliableSendsInFlight int
	ReliableQueued        uint64
	ReliableSent          uint64
	ReliableBytesSent     uint64
	ReliableBackpressure  uint64
	ReliableAbandoned     uint64
	SendErrors            uint64
	ErrorHandlerPanics    uint64
}

// NewAsyncTransport wraps a reliable downstream. A downstream that also
// implements SessionTransport learns about sessions as they are registered
// and removed here.
func NewAsyncTransport(downstream ReliableSender, config AsyncTransportConfig) (*AsyncTransport, error) {
	if isNilInterface(downstream) {
		return nil, ErrTransportRequired
	}
	if config.MaxResidentReliableBytes < 0 || config.MaxReliableAge < 0 || config.MaxQueuedReliableBytes < 0 {
		return nil, ErrProtocolConfig
	}
	defaults := DefaultAsyncTransportConfig()
	if config.MaxSessions <= 0 {
		config.MaxSessions = defaults.MaxSessions
	}
	if config.ReliableQueueSize <= 0 {
		config.ReliableQueueSize = defaults.ReliableQueueSize
	}
	if config.MaxReliableBytes <= 0 {
		config.MaxReliableBytes = defaults.MaxReliableBytes
	}
	if config.SendTimeout <= 0 {
		config.SendTimeout = defaults.SendTimeout
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &AsyncTransport{
		downstream: downstream, config: config, ctx: ctx, cancel: cancel,
		sessions: make(map[SessionID]*sessionQueue), closeDone: make(chan struct{}),
	}, nil
}

// RegisterSession opens the session's queue and starts its worker.
func (transport *AsyncTransport) RegisterSession(info SessionInfo) error {
	id := info.ID
	if transport == nil || id == 0 {
		return ErrSessionNotRegistered
	}
	transport.mu.Lock()
	defer transport.mu.Unlock()
	if transport.closed {
		return ErrTransportClosed
	}
	if _, exists := transport.sessions[id]; exists {
		return ErrSessionAlreadyExists
	}
	if len(transport.sessions) >= transport.config.MaxSessions {
		return ErrSessionLimit
	}
	if lifecycle, ok := transport.downstream.(SessionTransport); ok {
		if err := lifecycle.RegisterSession(info); err != nil {
			return err
		}
	}
	ctx, cancel := context.WithCancel(transport.ctx)
	queue := &sessionQueue{id: id, owner: transport, ctx: ctx, cancel: cancel, wake: make(chan struct{}, 1)}
	transport.sessions[id] = queue
	transport.stats.activeSessions.Add(1)
	transport.wait.Add(1)
	go queue.run()
	return nil
}

// RemoveSession drops queued work immediately and cancels the in-flight
// send. The session id stays taken until the worker has actually returned,
// so a re-registration cannot overtake an old send. Use Close when queued
// messages need a bounded graceful drain.
func (transport *AsyncTransport) RemoveSession(id SessionID) bool {
	if transport == nil || id == 0 {
		return false
	}
	transport.mu.Lock()
	queue, exists := transport.sessions[id]
	transport.mu.Unlock()
	if exists {
		queue.cancelNow()
	}
	return exists
}

// SendReliable queues one message for one session. It copies the payload,
// so callers may reuse their buffer. Refusals: ErrTransportClosed,
// ErrSessionNotRegistered (unknown or draining), ErrSessionFailed (wrapping
// the downstream cause), ErrReliableMessageTooBig, ErrReliableBackpressure.
func (transport *AsyncTransport) SendReliable(ctx context.Context, id SessionID, payload []byte) error {
	if transport == nil {
		return ErrTransportClosed
	}
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	if id == 0 {
		return ErrSessionNotRegistered
	}
	if len(payload) == 0 {
		return ErrProtocolConfig
	}
	if len(payload) > transport.config.MaxReliableBytes {
		return ErrReliableMessageTooBig
	}

	transport.mu.RLock()
	defer transport.mu.RUnlock()
	if transport.closed {
		return ErrTransportClosed
	}
	queue := transport.sessions[id]
	if queue == nil {
		return ErrSessionNotRegistered
	}
	queue.mu.Lock()
	defer queue.mu.Unlock()
	if err := queue.admissionErrorLocked(); err != nil {
		return err
	}
	if queue.pendingCount() >= transport.config.ReliableQueueSize ||
		(transport.config.MaxQueuedReliableBytes > 0 && int64(len(payload)) > int64(transport.config.MaxQueuedReliableBytes)-queue.queuedBytes) {
		transport.stats.reliableBackpressure.Add(1)
		return ErrReliableBackpressure
	}
	if !transport.reserveReliableBytes(int64(len(payload))) {
		transport.stats.reliableBackpressure.Add(1)
		transport.stats.globalBackpressure.Add(1)
		return fmt.Errorf("%w: global resident byte budget", ErrReliableBackpressure)
	}
	// 先准入检查，再复制调用方字节；拒绝路径不为 payload 分配。
	if len(queue.reliable) == cap(queue.reliable) && queue.head > 0 {
		count := copy(queue.reliable, queue.reliable[queue.head:])
		clear(queue.reliable[count:])
		queue.reliable = queue.reliable[:count]
		queue.head = 0
	}
	queue.reliable = append(queue.reliable, queuedReliable{data: append([]byte(nil), payload...), queuedAt: time.Now()})
	queue.queuedBytes += int64(len(payload))
	transport.stats.reliableQueued.Add(1)
	signal(queue.wake)
	return nil
}

// Close rejects new work, drains each session's queued messages, and then
// stops. If ctx expires, in-flight sends are cancelled and ctx.Err is
// returned. Downstream transports must honor the provided send context.
func (transport *AsyncTransport) Close(ctx context.Context) error {
	if transport == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	transport.closeOnce.Do(func() {
		transport.mu.Lock()
		transport.closed = true
		for _, queue := range transport.sessions {
			queue.beginClose()
		}
		transport.mu.Unlock()
		go func() {
			transport.wait.Wait()
			transport.cancel()
			close(transport.closeDone)
		}()
	})
	select {
	case <-transport.closeDone:
		return nil
	default:
	}
	select {
	case <-transport.closeDone:
		return nil
	case <-ctx.Done():
		transport.cancel()
		return ctx.Err()
	}
}

func (transport *AsyncTransport) Stats() AsyncTransportStats {
	if transport == nil {
		return AsyncTransportStats{}
	}
	transport.mu.RLock()
	active, draining, pending, busy := 0, 0, 0, 0
	var pendingBytes, busyBytes int64
	var oldest time.Time
	for _, queue := range transport.sessions {
		queue.mu.Lock()
		if queue.closing {
			draining++
		} else {
			active++
		}
		pending += queue.pendingCount()
		pendingBytes += queue.queuedBytes
		busyBytes += queue.busyBytes
		if queue.pendingCount() > 0 {
			at := queue.reliable[queue.head].queuedAt
			if oldest.IsZero() || at.Before(oldest) {
				oldest = at
			}
		}
		if queue.busy && (oldest.IsZero() || queue.busySince.Before(oldest)) {
			oldest = queue.busySince
		}
		if queue.busy {
			busy++
		}
		queue.mu.Unlock()
	}
	transport.mu.RUnlock()
	var age time.Duration
	if !oldest.IsZero() {
		age = time.Since(oldest)
	}
	return AsyncTransportStats{
		PendingReliableBytes: pendingBytes, ReliableBytesInFlight: busyBytes, OldestReliableAge: age,
		ActiveSessions:        active,
		DrainingSessions:      draining,
		PendingReliable:       pending,
		ReliableSendsInFlight: busy,
		ReliableQueued:        transport.stats.reliableQueued.Load(),
		ReliableSent:          transport.stats.reliableSent.Load(),
		ReliableBytesSent:     transport.stats.reliableBytesSent.Load(),
		ReliableBackpressure:  transport.stats.reliableBackpressure.Load(),
		ReliableAbandoned:     transport.stats.reliableAbandoned.Load(),
		SendErrors:            transport.stats.sendErrors.Load(),
		ErrorHandlerPanics:    transport.stats.handlerPanics.Load(),
	}
}

func (queue *sessionQueue) beginClose() {
	queue.mu.Lock()
	queue.closing = true
	queue.mu.Unlock()
	signal(queue.wake)
}

func (queue *sessionQueue) cancelNow() {
	queue.mu.Lock()
	queue.closing = true
	queue.owner.stats.reliableAbandoned.Add(uint64(queue.pendingCount()))
	queue.owner.stats.residentBytes.Add(-queue.queuedBytes)
	queue.reliable = nil
	queue.head, queue.queuedBytes = 0, 0
	queue.mu.Unlock()
	queue.cancel()
}

func (queue *sessionQueue) run() {
	defer queue.workerDone()
	for {
		select {
		case <-queue.ctx.Done():
			return
		case <-queue.wake:
		}
		for {
			message, exit := queue.take()
			if exit {
				return
			}
			if message == nil {
				break
			}
			if !queue.send(message) {
				return
			}
		}
	}
}

func (queue *sessionQueue) pendingCount() int { return len(queue.reliable) - queue.head }

func (queue *sessionQueue) take() ([]byte, bool) {
	queue.mu.Lock()
	defer queue.mu.Unlock()
	if queue.pendingCount() == 0 {
		return nil, queue.closing
	}
	message := queue.reliable[queue.head]
	queue.reliable[queue.head] = queuedReliable{}
	queue.head++
	if queue.head == len(queue.reliable) {
		queue.reliable = queue.reliable[:0]
		queue.head = 0
	}
	queue.queuedBytes -= int64(len(message.data))
	queue.busyBytes, queue.busySince = int64(len(message.data)), message.queuedAt
	queue.busy = true
	return message.data, false
}

func (queue *sessionQueue) send(message []byte) bool {
	// busySince 是本消息准入时刻，不能在出队时重新开始计算年龄。
	deadline := time.Now().Add(queue.owner.config.SendTimeout)
	var expires time.Time
	if age := queue.owner.config.MaxReliableAge; age > 0 {
		expires = queue.busySince.Add(age)
		if expires.Before(deadline) {
			deadline = expires
		}
	}
	ctx, cancel := context.WithDeadline(queue.ctx, deadline)
	defer cancel()
	var err error
	if !expires.IsZero() && !time.Now().Before(expires) {
		err = ErrReliableExpired
	} else {
		err = queue.owner.downstream.SendReliable(ctx, queue.id, message)
	}
	if err != nil {
		if !expires.IsZero() && !time.Now().Before(expires) {
			err = fmt.Errorf("%w: %v", ErrReliableExpired, err)
			queue.owner.stats.expired.Add(1)
		}
		queue.fail(err)
		queue.owner.report(SendError{Session: queue.id, Err: err})
		return false
	}
	queue.owner.stats.reliableSent.Add(1)
	queue.owner.stats.reliableBytesSent.Add(uint64(len(message)))
	queue.mu.Lock()
	queue.owner.stats.residentBytes.Add(-queue.busyBytes)
	queue.busy = false
	queue.busyBytes, queue.busySince = 0, time.Time{}
	queue.mu.Unlock()
	return true
}

func (transport *AsyncTransport) report(sendError SendError) {
	transport.stats.sendErrors.Add(1)
	if transport.config.OnError != nil {
		func() {
			defer func() {
				if recover() != nil {
					transport.stats.handlerPanics.Add(1)
				}
			}()
			transport.config.OnError(sendError)
		}()
	}
}

func (queue *sessionQueue) admissionErrorLocked() error {
	if queue.failure != nil {
		return fmt.Errorf("%w: %v", ErrSessionFailed, queue.failure)
	}
	if queue.closing {
		return ErrSessionNotRegistered
	}
	return nil
}

func (queue *sessionQueue) fail(err error) {
	queue.mu.Lock()
	queue.failure = err
	queue.closing = true
	queue.owner.stats.residentBytes.Add(-queue.busyBytes)
	queue.busy = false
	queue.busyBytes, queue.busySince = 0, time.Time{}
	queue.owner.stats.reliableAbandoned.Add(uint64(queue.pendingCount()))
	queue.owner.stats.residentBytes.Add(-queue.queuedBytes)
	queue.reliable = nil
	queue.head, queue.queuedBytes = 0, 0
	queue.mu.Unlock()
	queue.cancel()
}

func (queue *sessionQueue) workerDone() {
	// Close 超时可能在 worker 取下一条前取消：归还尚未消费的全部额度。
	queue.cancelNow()
	queue.mu.Lock()
	queue.owner.stats.residentBytes.Add(-queue.busyBytes)
	queue.busyBytes, queue.busySince, queue.busy = 0, time.Time{}, false
	queue.mu.Unlock()
	queue.owner.stats.activeSessions.Add(-1)
	if lifecycle, ok := queue.owner.downstream.(SessionTransport); ok {
		lifecycle.RemoveSession(queue.id)
	}
	queue.owner.mu.Lock()
	if queue.owner.sessions[queue.id] == queue {
		delete(queue.owner.sessions, queue.id)
	}
	queue.owner.mu.Unlock()
	queue.owner.wait.Done()
}

func signal(channel chan struct{}) {
	select {
	case channel <- struct{}{}:
	default:
	}
}

var _ ReliableSender = (*AsyncTransport)(nil)
var _ SessionTransport = (*AsyncTransport)(nil)

// MaxReliableBytes 返回配置的单条可靠消息硬上限，供上游完整包组帧使用。
func (transport *AsyncTransport) MaxReliableBytes() int {
	if transport == nil {
		return 0
	}
	return transport.config.MaxReliableBytes
}
