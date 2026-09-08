package nettransport

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	core "github.com/tjbdwanghaibo/roost-core/statesync"
)

type Channel uint8

const (
	ChannelDatagram Channel = iota + 1
	ChannelReliable
)

type SendError struct {
	Session core.SessionID
	Channel Channel
	Err     error
}

// AdmissionError identifies the session that made an otherwise atomic batch
// impossible to admit. Higher layers can evict that slow/failed session and
// retry the remaining independent recipients.
type AdmissionError struct {
	Session core.SessionID
	Err     error
}

func (e AdmissionError) Error() string {
	if e.Err == nil {
		return "replication transport: batch admission failed"
	}
	return fmt.Sprintf("replication transport: session %d admission: %v", e.Session, e.Err)
}
func (e AdmissionError) Unwrap() error { return e.Err }

func (sendError SendError) Error() string {
	if sendError.Err == nil {
		return "replication transport: unknown send error"
	}
	return sendError.Err.Error()
}
func (sendError SendError) Unwrap() error { return sendError.Err }

// ErrorHandler must return promptly. Panics are contained and counted, but a
// blocking handler still blocks the affected session lane.
type ErrorHandler func(SendError)

type AsyncTransportConfig struct {
	MaxSessions          int
	ReliableQueueSize    int
	MaxDatagramsPerFrame int
	MaxDatagramBytes     int
	MaxReliableBytes     int
	SendTimeout          time.Duration
	// AllowOpaqueDatagrams disables replication header, checksum, and complete
	// frame-batch validation. Keep false unless a trusted upstream has already
	// performed equivalent validation.
	AllowOpaqueDatagrams bool
	OnError              ErrorHandler
}

func DefaultAsyncTransportConfig() AsyncTransportConfig {
	return AsyncTransportConfig{
		MaxSessions:          4096,
		ReliableQueueSize:    256,
		MaxDatagramsPerFrame: core.DefaultLimits().MaxFragments,
		MaxDatagramBytes:     core.DefaultMaxDatagram,
		MaxReliableBytes:     1 << 20,
		SendTimeout:          250 * time.Millisecond,
	}
}

type AsyncTransport struct {
	mu         sync.RWMutex
	downstream core.Transport
	config     AsyncTransportConfig
	ctx        context.Context
	cancel     context.CancelFunc
	sessions   map[core.SessionID]*sessionQueue
	closed     bool
	wait       sync.WaitGroup
	closeOnce  sync.Once
	closeDone  chan struct{}
	stats      asyncCounters
}

type sessionQueue struct {
	id     core.SessionID
	owner  *AsyncTransport
	ctx    context.Context
	cancel context.CancelFunc

	mu           sync.Mutex
	closing      bool
	failure      error
	latest       map[uint64][][]byte
	latestOrder  []uint64
	reliable     [][]byte
	datagramBusy bool
	reliableBusy bool
	latestWake   chan struct{}
	reliableWake chan struct{}
	workersDone  atomic.Uint32
}

type asyncCounters struct {
	datagramFramesQueued  atomic.Uint64
	datagramFramesSent    atomic.Uint64
	datagramFramesDropped atomic.Uint64
	datagramBytesSent     atomic.Uint64
	reliableQueued        atomic.Uint64
	reliableSent          atomic.Uint64
	reliableBytesSent     atomic.Uint64
	reliableBackpressure  atomic.Uint64
	reliableAbandoned     atomic.Uint64
	sendErrors            atomic.Uint64
	handlerPanics         atomic.Uint64
}

type AsyncTransportStats struct {
	ActiveSessions        int
	DrainingSessions      int
	PendingDatagramFrames int
	PendingReliable       int
	DatagramSendsInFlight int
	ReliableSendsInFlight int
	DatagramFramesQueued  uint64
	DatagramFramesSent    uint64
	DatagramFramesDropped uint64
	DatagramBytesSent     uint64
	ReliableQueued        uint64
	ReliableSent          uint64
	ReliableBytesSent     uint64
	ReliableBackpressure  uint64
	ReliableAbandoned     uint64
	SendErrors            uint64
	ErrorHandlerPanics    uint64
}

func NewAsyncTransport(downstream core.Transport, config AsyncTransportConfig) (*AsyncTransport, error) {
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
	if config.MaxDatagramsPerFrame <= 0 {
		config.MaxDatagramsPerFrame = defaults.MaxDatagramsPerFrame
	}
	if config.MaxDatagramBytes <= 0 {
		config.MaxDatagramBytes = defaults.MaxDatagramBytes
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
		sessions: make(map[core.SessionID]*sessionQueue), closeDone: make(chan struct{}),
	}, nil
}

// RegisterSession starts two independent workers: latest-only datagrams and
// bounded reliable messages. This avoids reliable stream stalls blocking state.
func (transport *AsyncTransport) RegisterSession(info core.SessionInfo) error {
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
	if lifecycle, ok := transport.downstream.(core.SessionTransport); ok {
		if err := lifecycle.RegisterSession(info); err != nil {
			return err
		}
	}
	ctx, cancel := context.WithCancel(transport.ctx)
	queue := &sessionQueue{
		id: id, owner: transport, ctx: ctx, cancel: cancel,
		latest:     make(map[uint64][][]byte),
		latestWake: make(chan struct{}, 1), reliableWake: make(chan struct{}, 1),
	}
	transport.sessions[id] = queue
	transport.wait.Add(2)
	go queue.runDatagrams()
	go queue.runReliable()
	return nil
}

// RemoveSession cancels queued work immediately. Room shutdown should use
// Close when reliable messages need a bounded graceful drain.
func (transport *AsyncTransport) RemoveSession(id core.SessionID) bool {
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

func (transport *AsyncTransport) SendDatagram(ctx context.Context, id core.SessionID, payload []byte) error {
	return transport.SendDatagramBatch(ctx, id, [][]byte{payload})
}

// SendDatagramBatch atomically replaces the pending state frame. It copies all
// packets before admission, so callers may safely reuse their buffers.
func (transport *AsyncTransport) SendDatagramBatch(ctx context.Context, id core.SessionID, packets [][]byte) error {
	return transport.AdmitBatch(ctx, []OutboundFrame{{Session: id, Datagrams: packets}})
}

func (transport *AsyncTransport) SendReliable(ctx context.Context, id core.SessionID, payload []byte) error {
	return transport.AdmitBatch(ctx, []OutboundFrame{{Session: id, Reliable: payload}})
}

type preparedOutboundFrame struct {
	session   core.SessionID
	stream    uint64
	datagrams [][]byte
	reliable  []byte
}

// AdmitBatch validates and copies the entire batch before taking queue locks.
// Queue capacity and session state are then checked under a stable lock order,
// making admission atomic across every affected session.
func (transport *AsyncTransport) AdmitBatch(ctx context.Context, frames []OutboundFrame) error {
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
	if len(frames) == 0 {
		return nil
	}
	prepared := make([]preparedOutboundFrame, len(frames))
	for index, frame := range frames {
		if frame.Session == 0 || (len(frame.Datagrams) == 0) == (len(frame.Reliable) == 0) {
			return ErrProtocolConfig
		}
		item := preparedOutboundFrame{session: frame.Session}
		if len(frame.Datagrams) != 0 {
			stream, err := validateAndCopyDatagramBatch(frame.Datagrams, transport.config, &item.datagrams)
			if err != nil {
				return err
			}
			item.stream = stream
		} else {
			if len(frame.Reliable) > transport.config.MaxReliableBytes {
				return ErrReliableMessageTooBig
			}
			item.reliable = append([]byte(nil), frame.Reliable...)
		}
		prepared[index] = item
	}

	transport.mu.RLock()
	if transport.closed {
		transport.mu.RUnlock()
		return ErrTransportClosed
	}
	queuesByID := make(map[core.SessionID]*sessionQueue, len(prepared))
	for _, item := range prepared {
		queue := transport.sessions[item.session]
		if queue == nil {
			transport.mu.RUnlock()
			return ErrSessionNotRegistered
		}
		queuesByID[item.session] = queue
	}
	ids := make([]core.SessionID, 0, len(queuesByID))
	for id := range queuesByID {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for _, id := range ids {
		queuesByID[id].mu.Lock()
	}
	defer func() {
		for index := len(ids) - 1; index >= 0; index-- {
			queuesByID[ids[index]].mu.Unlock()
		}
		transport.mu.RUnlock()
	}()
	for _, id := range ids {
		if err := queuesByID[id].admissionErrorLocked(); err != nil {
			return AdmissionError{Session: id, Err: err}
		}
	}
	reliableAdds := make(map[core.SessionID]int)
	for _, item := range prepared {
		if item.reliable != nil {
			reliableAdds[item.session]++
		}
	}
	for id, additions := range reliableAdds {
		if len(queuesByID[id].reliable)+additions > transport.config.ReliableQueueSize {
			transport.stats.reliableBackpressure.Add(1)
			return AdmissionError{Session: id, Err: ErrReliableBackpressure}
		}
	}
	for _, item := range prepared {
		queue := queuesByID[item.session]
		if item.datagrams != nil {
			if _, exists := queue.latest[item.stream]; exists {
				transport.stats.datagramFramesDropped.Add(1)
			} else {
				queue.latestOrder = append(queue.latestOrder, item.stream)
			}
			queue.latest[item.stream] = item.datagrams
			transport.stats.datagramFramesQueued.Add(1)
			signal(queue.latestWake)
			continue
		}
		queue.reliable = append(queue.reliable, item.reliable)
		transport.stats.reliableQueued.Add(1)
		signal(queue.reliableWake)
	}
	return nil
}

func (transport *AsyncTransport) session(id core.SessionID) (*sessionQueue, error) {
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

// Close rejects new work, drains each session's currently admitted work, and
// then stops. If ctx expires, in-flight sends are cancelled and ctx.Err is
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
	active, draining, pendingDatagrams, pendingReliable, datagramBusy, reliableBusy := 0, 0, 0, 0, 0, 0
	for _, queue := range transport.sessions {
		queue.mu.Lock()
		if queue.closing {
			draining++
		} else {
			active++
		}
		pendingDatagrams += len(queue.latest)
		pendingReliable += len(queue.reliable)
		if queue.datagramBusy {
			datagramBusy++
		}
		if queue.reliableBusy {
			reliableBusy++
		}
		queue.mu.Unlock()
	}
	transport.mu.RUnlock()
	return AsyncTransportStats{
		ActiveSessions:        active,
		DrainingSessions:      draining,
		PendingDatagramFrames: pendingDatagrams,
		PendingReliable:       pendingReliable,
		DatagramSendsInFlight: datagramBusy,
		ReliableSendsInFlight: reliableBusy,
		DatagramFramesQueued:  transport.stats.datagramFramesQueued.Load(),
		DatagramFramesSent:    transport.stats.datagramFramesSent.Load(),
		DatagramFramesDropped: transport.stats.datagramFramesDropped.Load(),
		DatagramBytesSent:     transport.stats.datagramBytesSent.Load(),
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
	signal(queue.latestWake)
	signal(queue.reliableWake)
}

func (queue *sessionQueue) cancelNow() {
	queue.mu.Lock()
	queue.closing = true
	queue.owner.stats.datagramFramesDropped.Add(uint64(len(queue.latest)))
	queue.owner.stats.reliableAbandoned.Add(uint64(len(queue.reliable)))
	queue.latest = nil
	queue.latestOrder = nil
	queue.reliable = nil
	queue.mu.Unlock()
	queue.cancel()
}

func (queue *sessionQueue) runDatagrams() {
	defer queue.workerDone()
	for {
		select {
		case <-queue.ctx.Done():
			return
		case <-queue.latestWake:
		}
		for {
			batch, exit := queue.takeLatest()
			if exit {
				return
			}
			if batch == nil {
				break
			}
			queue.sendDatagrams(batch)
		}
	}
}

func (queue *sessionQueue) runReliable() {
	defer queue.workerDone()
	for {
		select {
		case <-queue.ctx.Done():
			return
		case <-queue.reliableWake:
		}
		for {
			message, exit := queue.takeReliable()
			if exit {
				return
			}
			if message == nil {
				break
			}
			if !queue.sendReliable(message) {
				return
			}
		}
	}
}

func (queue *sessionQueue) takeLatest() ([][]byte, bool) {
	queue.mu.Lock()
	defer queue.mu.Unlock()
	if len(queue.latestOrder) == 0 {
		return nil, queue.closing
	}
	stream := queue.latestOrder[0]
	queue.latestOrder = queue.latestOrder[1:]
	batch := queue.latest[stream]
	delete(queue.latest, stream)
	if batch != nil {
		queue.datagramBusy = true
	}
	return batch, false
}

func (queue *sessionQueue) takeReliable() ([]byte, bool) {
	queue.mu.Lock()
	defer queue.mu.Unlock()
	if len(queue.reliable) == 0 {
		return nil, queue.closing
	}
	message := queue.reliable[0]
	queue.reliable[0] = nil
	queue.reliable = queue.reliable[1:]
	queue.reliableBusy = true
	return message, false
}

func (queue *sessionQueue) sendDatagrams(batch [][]byte) {
	ctx, cancel := context.WithTimeout(queue.ctx, queue.owner.config.SendTimeout)
	defer cancel()
	downstream := queue.owner.downstream
	var err error
	if batched, ok := downstream.(core.DatagramBatchTransport); ok {
		err = batched.SendDatagramBatch(ctx, queue.id, batch)
	} else {
		for _, packet := range batch {
			if err = downstream.SendDatagram(ctx, queue.id, packet); err != nil {
				break
			}
		}
	}
	if err != nil {
		queue.owner.report(SendError{Session: queue.id, Channel: ChannelDatagram, Err: err})
		queue.mu.Lock()
		queue.datagramBusy = false
		queue.mu.Unlock()
		return
	}
	queue.owner.stats.datagramFramesSent.Add(1)
	for _, packet := range batch {
		queue.owner.stats.datagramBytesSent.Add(uint64(len(packet)))
	}
	queue.mu.Lock()
	queue.datagramBusy = false
	queue.mu.Unlock()
}

func (queue *sessionQueue) sendReliable(message []byte) bool {
	ctx, cancel := context.WithTimeout(queue.ctx, queue.owner.config.SendTimeout)
	defer cancel()
	if err := queue.owner.downstream.SendReliable(ctx, queue.id, message); err != nil {
		queue.owner.report(SendError{Session: queue.id, Channel: ChannelReliable, Err: err})
		queue.fail(err)
		return false
	}
	queue.owner.stats.reliableSent.Add(1)
	queue.owner.stats.reliableBytesSent.Add(uint64(len(message)))
	queue.mu.Lock()
	queue.reliableBusy = false
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
	queue.reliableBusy = false
	queue.owner.stats.datagramFramesDropped.Add(uint64(len(queue.latest)))
	queue.owner.stats.reliableAbandoned.Add(uint64(len(queue.reliable)))
	queue.latest = nil
	queue.latestOrder = nil
	queue.reliable = nil
	queue.mu.Unlock()
	queue.cancel()
}

func (queue *sessionQueue) workerDone() {
	if queue.workersDone.Add(1) == 2 {
		if lifecycle, ok := queue.owner.downstream.(core.SessionTransport); ok {
			lifecycle.RemoveSession(queue.id)
		}
		queue.owner.mu.Lock()
		if queue.owner.sessions[queue.id] == queue {
			delete(queue.owner.sessions, queue.id)
		}
		queue.owner.mu.Unlock()
	}
	queue.owner.wait.Done()
}

func signal(channel chan struct{}) {
	select {
	case channel <- struct{}{}:
	default:
	}
}

func validateDatagramBatch(packets [][]byte, config AsyncTransportConfig) error {
	_, err := inspectDatagramBatch(packets, config)
	return err
}

func validateAndCopyDatagramBatch(packets [][]byte, config AsyncTransportConfig, destination *[][]byte) (uint64, error) {
	if len(packets) == 0 || len(packets) > config.MaxDatagramsPerFrame {
		return 0, ErrInvalidDatagramBatch
	}
	stream := uint64(0)
	if !config.AllowOpaqueDatagrams {
		header, err := inspectDatagramBatch(packets, config)
		if err != nil {
			return 0, err
		}
		stream = header.RoomID
	}
	copyOfPackets := make([][]byte, len(packets))
	for index, packet := range packets {
		if len(packet) == 0 || len(packet) > config.MaxDatagramBytes {
			return 0, ErrInvalidDatagramBatch
		}
		copyOfPackets[index] = append([]byte(nil), packet...)
	}
	*destination = copyOfPackets
	return stream, nil
}

func inspectDatagramBatch(packets [][]byte, config AsyncTransportConfig) (core.DatagramHeader, error) {
	if len(packets) == 0 || len(packets) > config.MaxDatagramsPerFrame {
		return core.DatagramHeader{}, ErrInvalidDatagramBatch
	}
	limits := core.DefaultLimits()
	limits.MaxDatagramBytes = config.MaxDatagramBytes
	limits.MaxFragments = config.MaxDatagramsPerFrame
	var first core.DatagramHeader
	seen := make([]bool, len(packets))
	for index, packet := range packets {
		header, err := core.InspectDatagram(packet, limits)
		if err != nil {
			return core.DatagramHeader{}, fmt.Errorf("%w: packet %d: %v", ErrInvalidDatagramBatch, index, err)
		}
		if int(header.ChunkCount) != len(packets) || int(header.ChunkIndex) >= len(seen) || seen[header.ChunkIndex] {
			return core.DatagramHeader{}, ErrInvalidDatagramBatch
		}
		seen[header.ChunkIndex] = true
		if index == 0 {
			first = header
			continue
		}
		if header.RoomID != first.RoomID || header.Epoch != first.Epoch || header.Tick != first.Tick ||
			header.BaseTick != first.BaseTick || header.Sequence != first.Sequence || header.ChunkCount != first.ChunkCount || header.Flags != first.Flags {
			return core.DatagramHeader{}, ErrInvalidDatagramBatch
		}
	}
	return first, nil
}

var _ core.Transport = (*AsyncTransport)(nil)
var _ core.DatagramBatchTransport = (*AsyncTransport)(nil)
var _ core.SessionTransport = (*AsyncTransport)(nil)
var _ AtomicBatchTransport = (*AsyncTransport)(nil)
