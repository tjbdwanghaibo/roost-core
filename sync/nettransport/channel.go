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
	SendTimeout       time.Duration
	OnError           ErrorHandler
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

type sessionQueue struct {
	id     SessionID
	owner  *AsyncTransport
	ctx    context.Context
	cancel context.CancelFunc

	mu       sync.Mutex
	closing  bool
	failure  error
	reliable [][]byte
	busy     bool
	wake     chan struct{}
}

type asyncCounters struct {
	reliableQueued       atomic.Uint64
	reliableSent         atomic.Uint64
	reliableBytesSent    atomic.Uint64
	reliableBackpressure atomic.Uint64
	reliableAbandoned    atomic.Uint64
	sendErrors           atomic.Uint64
	handlerPanics        atomic.Uint64
}

type AsyncTransportStats struct {
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
	message := append([]byte(nil), payload...)

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
	if len(queue.reliable) >= transport.config.ReliableQueueSize {
		transport.stats.reliableBackpressure.Add(1)
		return ErrReliableBackpressure
	}
	queue.reliable = append(queue.reliable, message)
	transport.stats.reliableQueued.Add(1)
	signal(queue.wake)
	return nil
}

func (transport *AsyncTransport) session(id SessionID) (*sessionQueue, error) {
	if transport == nil {
		return nil, ErrTransportClosed
	}
	transport.mu.RLock()
	if transport.closed {
		transport.mu.RUnlock()
		return nil, ErrTransportClosed
	}
	queue := transport.sessions[id]
	transport.mu.RUnlock()
	if queue == nil {
		return nil, ErrSessionNotRegistered
	}
	return queue, nil
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
	for _, queue := range transport.sessions {
		queue.mu.Lock()
		if queue.closing {
			draining++
		} else {
			active++
		}
		pending += len(queue.reliable)
		if queue.busy {
			busy++
		}
		queue.mu.Unlock()
	}
	transport.mu.RUnlock()
	return AsyncTransportStats{
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
	queue.owner.stats.reliableAbandoned.Add(uint64(len(queue.reliable)))
	queue.reliable = nil
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

func (queue *sessionQueue) take() ([]byte, bool) {
	queue.mu.Lock()
	defer queue.mu.Unlock()
	if len(queue.reliable) == 0 {
		return nil, queue.closing
	}
	message := queue.reliable[0]
	queue.reliable[0] = nil
	queue.reliable = queue.reliable[1:]
	queue.busy = true
	return message, false
}

func (queue *sessionQueue) send(message []byte) bool {
	ctx, cancel := context.WithTimeout(queue.ctx, queue.owner.config.SendTimeout)
	defer cancel()
	if err := queue.owner.downstream.SendReliable(ctx, queue.id, message); err != nil {
		queue.owner.report(SendError{Session: queue.id, Err: err})
		queue.fail(err)
		return false
	}
	queue.owner.stats.reliableSent.Add(1)
	queue.owner.stats.reliableBytesSent.Add(uint64(len(message)))
	queue.mu.Lock()
	queue.busy = false
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
	queue.busy = false
	queue.owner.stats.reliableAbandoned.Add(uint64(len(queue.reliable)))
	queue.reliable = nil
	queue.mu.Unlock()
	queue.cancel()
}

func (queue *sessionQueue) workerDone() {
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
