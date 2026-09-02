// Package syncstream adapts cube-core/syncstream packets to cube-kit sync
// transports with observer isolation, confirmed publishing, compression,
// fragmentation, checksums, and bounded reassembly.
package syncstream

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"sync"
	"sync/atomic"
	"time"

	coresyncbus "github.com/tjbdwanghaibo/cube-core/syncbus"
	corestream "github.com/tjbdwanghaibo/cube-core/syncstream"
)

var (
	ErrPublisherRequired      = errors.New("syncstream adapter: publisher is required")
	ErrSubscriberRequired     = errors.New("syncstream adapter: subscriber is required")
	ErrHandlerRequired        = errors.New("syncstream adapter: handler is required")
	ErrSequenceOverflow       = errors.New("syncstream adapter: sequence exceeds transport version range")
	ErrEpochRequired          = errors.New("syncstream adapter: packet epoch is required")
	ErrEnvelopeMismatch       = errors.New("syncstream adapter: transport and packet envelopes differ")
	ErrObserverMismatch       = errors.New("syncstream adapter: packet observer mismatch")
	ErrPayloadTooLarge        = errors.New("syncstream adapter: payload exceeds configured limit")
	ErrBackpressure           = errors.New("syncstream adapter: publish queue is full")
	ErrPublisherClosed        = errors.New("syncstream adapter: publisher is closed")
	ErrConfirmationRequired   = errors.New("syncstream adapter: transport does not provide publish confirmation")
	ErrChecksumMismatch       = errors.New("syncstream adapter: payload checksum mismatch")
	ErrCompressionUnsupported = errors.New("syncstream adapter: compression is unsupported")
	ErrFragmentInvalid        = errors.New("syncstream adapter: fragmented envelope is invalid")
)

type ErrorHandler func(error)

// ConfirmedSyncPublisher returns only after the broker durably accepts a frame.
// JetStream implements this capability; plain NATS intentionally does not.
type ConfirmedSyncPublisher interface {
	PublishConfirmed(*coresyncbus.SyncMsg) error
}

type Publisher struct {
	bus                  coresyncbus.IPublisher
	confirmed            ConfirmedSyncPublisher
	fromSid              int32
	onError              ErrorHandler
	expectedObserver     *corestream.Observer
	maxPayloadBytes      int
	compressionThreshold int
	maxFrameBytes        int
	requireConfirmation  bool
	published            atomic.Uint64
	frames               atomic.Uint64
	failures             atomic.Uint64
}

type PublisherOptions struct {
	FromSID              int32
	OnError              ErrorHandler
	ExpectedObserver     *corestream.Observer
	MaxPayloadBytes      int
	CompressionThreshold int
	MaxFrameBytes        int
	RequireConfirmation  bool
}

type gzipEncoder struct {
	buffer bytes.Buffer
	writer *gzip.Writer
}

var gzipEncoderPool = sync.Pool{New: func() any {
	writer, err := gzip.NewWriterLevel(io.Discard, gzip.BestSpeed)
	if err != nil {
		panic(err) // gzip validates this constant; construction cannot fail
	}
	return &gzipEncoder{writer: writer}
}}

func NewPublisher(bus coresyncbus.IPublisher, fromSid int32, onError ErrorHandler) (*Publisher, error) {
	return NewPublisherWithOptions(bus, PublisherOptions{FromSID: fromSid, OnError: onError})
}

func NewPublisherWithOptions(bus coresyncbus.IPublisher, options PublisherOptions) (*Publisher, error) {
	if bus == nil {
		return nil, ErrPublisherRequired
	}
	confirmed, _ := bus.(ConfirmedSyncPublisher)
	if options.RequireConfirmation && confirmed == nil {
		return nil, ErrConfirmationRequired
	}
	var observer *corestream.Observer
	if options.ExpectedObserver != nil {
		value := *options.ExpectedObserver
		observer = &value
	}
	return &Publisher{bus: bus, confirmed: confirmed, fromSid: options.FromSID, onError: options.OnError, expectedObserver: observer, maxPayloadBytes: options.MaxPayloadBytes, compressionThreshold: options.CompressionThreshold, maxFrameBytes: options.MaxFrameBytes, requireConfirmation: options.RequireConfirmation}, nil
}

func (publisher *Publisher) Publish(packet corestream.Packet) error {
	if publisher == nil || publisher.bus == nil {
		return ErrPublisherRequired
	}
	if packet.Sequence > math.MaxInt64 {
		publisher.failures.Add(1)
		return ErrSequenceOverflow
	}
	if packet.Epoch == 0 {
		publisher.failures.Add(1)
		return ErrEpochRequired
	}
	if publisher.expectedObserver != nil && packet.Observer != *publisher.expectedObserver {
		publisher.failures.Add(1)
		return ErrObserverMismatch
	}
	if publisher.maxPayloadBytes > 0 && len(packet.Payload) > publisher.maxPayloadBytes {
		publisher.failures.Add(1)
		return ErrPayloadTooLarge
	}
	raw, err := json.Marshal(packet)
	if err != nil {
		publisher.failures.Add(1)
		return err
	}
	encoded, encoding, err := encodePayload(raw, publisher.compressionThreshold)
	if err != nil {
		publisher.failures.Add(1)
		return err
	}
	checksum := sha256.Sum256(raw)
	checksumText := hex.EncodeToString(checksum[:])
	frameSize := publisher.maxFrameBytes
	if frameSize <= 0 || frameSize >= len(encoded) {
		frameSize = len(encoded)
	}
	if frameSize == 0 {
		frameSize = 1
	}
	parts := (len(encoded) + frameSize - 1) / frameSize
	for part := 0; part < parts; part++ {
		start, end := part*frameSize, (part+1)*frameSize
		if end > len(encoded) {
			end = len(encoded)
		}
		message := &coresyncbus.SyncMsg{Topic: packet.Stream.Topic, Key: packet.Stream.Key, Version: int64(packet.Sequence), Data: append([]byte(nil), encoded[start:end]...), FromSid: publisher.fromSid, Part: uint32(part), Parts: uint32(parts), Encoding: encoding, Checksum: checksumText}
		if err := publisher.publishFrame(message); err != nil {
			publisher.failures.Add(1)
			return err
		}
		publisher.frames.Add(1)
	}
	publisher.published.Add(1)
	return nil
}

func (publisher *Publisher) publishFrame(message *coresyncbus.SyncMsg) error {
	if publisher.requireConfirmation {
		return publisher.confirmed.PublishConfirmed(message)
	}
	return publisher.bus.Publish(message)
}

func encodePayload(raw []byte, threshold int) ([]byte, string, error) {
	if threshold <= 0 || len(raw) < threshold {
		return append([]byte(nil), raw...), "identity", nil
	}
	encoder := gzipEncoderPool.Get().(*gzipEncoder)
	encoder.buffer.Reset()
	encoder.writer.Reset(&encoder.buffer)
	if _, err := encoder.writer.Write(raw); err != nil {
		gzipEncoderPool.Put(encoder)
		return nil, "", err
	}
	if err := encoder.writer.Close(); err != nil {
		gzipEncoderPool.Put(encoder)
		return nil, "", err
	}
	result := append([]byte(nil), encoder.buffer.Bytes()...)
	// Do not pin unexpectedly large payload buffers in the global pool.
	if encoder.buffer.Cap() <= 1<<20 {
		gzipEncoderPool.Put(encoder)
	}
	return result, "gzip", nil
}

func (publisher *Publisher) Enqueue(packet corestream.Packet) {
	if err := publisher.Publish(packet); err != nil && publisher.onError != nil {
		publisher.onError(err)
	}
}
func (publisher *Publisher) EnqueueBatch(packets []corestream.Packet) {
	for _, packet := range packets {
		publisher.Enqueue(packet)
	}
}

type Handler func(corestream.Packet) error

type SubscribeOptions struct {
	ExpectedObserver *corestream.Observer
	MaxEnvelopeBytes int
	MaxPayloadBytes  int
	MaxAssemblyBytes int
	MaxDecodedBytes  int
	MaxChunks        uint32
	AssemblyTTL      time.Duration
	RequireChecksum  bool
}

func Subscribe(bus coresyncbus.ISubscriber, topic string, handler Handler) (func(), error) {
	return SubscribeWithOptions(bus, topic, SubscribeOptions{}, handler)
}
func SubscribeForObserver(bus coresyncbus.ISubscriber, topic string, observer corestream.Observer, handler Handler) (func(), error) {
	return SubscribeWithOptions(bus, topic, SubscribeOptions{ExpectedObserver: &observer}, handler)
}

type assemblyKey struct {
	topic    string
	key      int64
	version  int64
	from     int32
	parts    uint32
	encoding string
	checksum string
}
type assembly struct {
	created  time.Time
	chunks   [][]byte
	received uint32
	bytes    int
}
type reassembler struct {
	mutex   sync.Mutex
	options SubscribeOptions
	values  map[assemblyKey]*assembly
}

func SubscribeWithOptions(bus coresyncbus.ISubscriber, topic string, options SubscribeOptions, handler Handler) (func(), error) {
	if bus == nil {
		return nil, ErrSubscriberRequired
	}
	if handler == nil {
		return nil, ErrHandlerRequired
	}
	if options.MaxChunks == 0 {
		options.MaxChunks = 256
	}
	if options.MaxAssemblyBytes <= 0 {
		options.MaxAssemblyBytes = 8 << 20
	}
	if options.MaxDecodedBytes <= 0 {
		options.MaxDecodedBytes = options.MaxAssemblyBytes
	}
	if options.AssemblyTTL <= 0 {
		options.AssemblyTTL = 30 * time.Second
	}
	assembler := &reassembler{options: options, values: make(map[assemblyKey]*assembly)}
	unsub, err := bus.Subscribe(topic, func(message *coresyncbus.SyncMsg) error {
		if message == nil {
			return nil
		}
		if options.MaxEnvelopeBytes > 0 && len(message.Data) > options.MaxEnvelopeBytes {
			return ErrPayloadTooLarge
		}
		payload, ready, err := assembler.accept(message, time.Now())
		if err != nil || !ready {
			return err
		}
		packet, err := decodePacket(payload, message, options)
		if err != nil {
			return err
		}
		return handler(packet.Clone())
	})
	if err != nil {
		return nil, err
	}
	return func() {
		unsub()
		assembler.mutex.Lock()
		assembler.values = make(map[assemblyKey]*assembly)
		assembler.mutex.Unlock()
	}, nil
}

func (assembler *reassembler) accept(message *coresyncbus.SyncMsg, now time.Time) ([]byte, bool, error) {
	parts := message.Parts
	if parts == 0 {
		parts = 1
	}
	if parts > assembler.options.MaxChunks || message.Part >= parts {
		return nil, false, ErrFragmentInvalid
	}
	if assembler.options.RequireChecksum && message.Checksum == "" {
		return nil, false, ErrChecksumMismatch
	}
	if parts == 1 {
		return assembler.decode(message.Data, message.Encoding, message.Checksum)
	}
	key := assemblyKey{message.Topic, message.Key, message.Version, message.FromSid, parts, message.Encoding, message.Checksum}
	assembler.mutex.Lock()
	defer assembler.mutex.Unlock()
	for existing, value := range assembler.values {
		if now.Sub(value.created) > assembler.options.AssemblyTTL {
			delete(assembler.values, existing)
		}
	}
	value := assembler.values[key]
	if value == nil {
		value = &assembly{created: now, chunks: make([][]byte, parts)}
		assembler.values[key] = value
	}
	if value.chunks[message.Part] == nil {
		value.chunks[message.Part] = append([]byte(nil), message.Data...)
		value.received++
		value.bytes += len(message.Data)
		if value.bytes > assembler.options.MaxAssemblyBytes {
			delete(assembler.values, key)
			return nil, false, ErrPayloadTooLarge
		}
	} else if !bytes.Equal(value.chunks[message.Part], message.Data) {
		delete(assembler.values, key)
		return nil, false, ErrFragmentInvalid
	}
	if value.received != parts {
		return nil, false, nil
	}
	encoded := make([]byte, 0, value.bytes)
	for _, chunk := range value.chunks {
		encoded = append(encoded, chunk...)
	}
	delete(assembler.values, key)
	return assembler.decode(encoded, message.Encoding, message.Checksum)
}

func (assembler *reassembler) decode(encoded []byte, encoding, checksum string) ([]byte, bool, error) {
	var reader io.Reader = bytes.NewReader(encoded)
	switch encoding {
	case "", "identity":
	case "gzip":
		gzipReader, err := gzip.NewReader(reader)
		if err != nil {
			return nil, false, err
		}
		defer gzipReader.Close()
		reader = gzipReader
	default:
		return nil, false, ErrCompressionUnsupported
	}
	decoded, err := io.ReadAll(io.LimitReader(reader, int64(assembler.options.MaxDecodedBytes)+1))
	if err != nil {
		return nil, false, err
	}
	if len(decoded) > assembler.options.MaxDecodedBytes {
		return nil, false, ErrPayloadTooLarge
	}
	if checksum != "" {
		sum := sha256.Sum256(decoded)
		if hex.EncodeToString(sum[:]) != checksum {
			return nil, false, ErrChecksumMismatch
		}
	}
	return decoded, true, nil
}

func decodePacket(data []byte, message *coresyncbus.SyncMsg, options SubscribeOptions) (corestream.Packet, error) {
	var packet corestream.Packet
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&packet); err != nil {
		return packet, fmt.Errorf("syncstream adapter: decode: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return packet, fmt.Errorf("syncstream adapter: decode trailing content")
	}
	if packet.Stream.Topic != message.Topic || packet.Stream.Key != message.Key || packet.Sequence != uint64(message.Version) || message.Version < 0 {
		return packet, ErrEnvelopeMismatch
	}
	if options.ExpectedObserver != nil && packet.Observer != *options.ExpectedObserver {
		return packet, ErrObserverMismatch
	}
	if options.MaxPayloadBytes > 0 && len(packet.Payload) > options.MaxPayloadBytes {
		return packet, ErrPayloadTooLarge
	}
	return packet, nil
}

type PublisherMetrics struct {
	Published uint64
	Frames    uint64
	Failures  uint64
}

func (publisher *Publisher) Metrics() PublisherMetrics {
	if publisher == nil {
		return PublisherMetrics{}
	}
	return PublisherMetrics{Published: publisher.published.Load(), Frames: publisher.frames.Load(), Failures: publisher.failures.Load()}
}

type PacketPublisher interface{ Publish(corestream.Packet) error }
type BufferedPublisherOptions struct {
	Capacity    int
	MaxAttempts int
	RetryDelay  time.Duration
	OnError     ErrorHandler
}
type BufferedPublisherMetrics struct {
	Queued       uint64
	Published    uint64
	Failures     uint64
	Backpressure uint64
	Synchronous  uint64
}

// BufferedPublisher.Publish is confirmed and synchronous. TryEnqueue is the
// explicitly asynchronous bounded API; callers cannot confuse queue admission
// with broker confirmation.
type BufferedPublisher struct {
	mutex       sync.RWMutex
	publisher   PacketPublisher
	queue       chan corestream.Packet
	maxAttempts int
	retryDelay  time.Duration
	onError     ErrorHandler
	closed      bool
	wait        sync.WaitGroup
	queued      atomic.Uint64
	published   atomic.Uint64
	failures    atomic.Uint64
	pressure    atomic.Uint64
	synchronous atomic.Uint64
}

func NewBufferedPublisher(publisher PacketPublisher, options BufferedPublisherOptions) (*BufferedPublisher, error) {
	if publisher == nil {
		return nil, ErrPublisherRequired
	}
	if options.Capacity <= 0 {
		options.Capacity = 256
	}
	if options.MaxAttempts <= 0 {
		options.MaxAttempts = 1
	}
	buffered := &BufferedPublisher{publisher: publisher, queue: make(chan corestream.Packet, options.Capacity), maxAttempts: options.MaxAttempts, retryDelay: options.RetryDelay, onError: options.OnError}
	buffered.wait.Add(1)
	go buffered.run()
	return buffered, nil
}

func (publisher *BufferedPublisher) Publish(packet corestream.Packet) error {
	if publisher == nil {
		return ErrPublisherRequired
	}
	publisher.mutex.RLock()
	closed := publisher.closed
	publisher.mutex.RUnlock()
	if closed {
		return ErrPublisherClosed
	}
	publisher.synchronous.Add(1)
	return publisher.publishWithRetry(packet)
}

func (publisher *BufferedPublisher) TryEnqueue(packet corestream.Packet) error {
	if publisher == nil {
		return ErrPublisherRequired
	}
	publisher.mutex.RLock()
	defer publisher.mutex.RUnlock()
	if publisher.closed {
		return ErrPublisherClosed
	}
	select {
	case publisher.queue <- packet.Clone():
		publisher.queued.Add(1)
		return nil
	default:
		publisher.pressure.Add(1)
		return ErrBackpressure
	}
}

func (publisher *BufferedPublisher) publishWithRetry(packet corestream.Packet) error {
	var err error
	for attempt := 0; attempt < publisher.maxAttempts; attempt++ {
		err = publisher.publisher.Publish(packet)
		if err == nil {
			publisher.published.Add(1)
			return nil
		}
		if publisher.retryDelay > 0 && attempt+1 < publisher.maxAttempts {
			time.Sleep(publisher.retryDelay)
		}
	}
	publisher.failures.Add(1)
	return err
}

func (publisher *BufferedPublisher) run() {
	defer publisher.wait.Done()
	for packet := range publisher.queue {
		if err := publisher.publishWithRetry(packet); err != nil && publisher.onError != nil {
			publisher.onError(err)
		}
	}
}

func (publisher *BufferedPublisher) Close(ctx context.Context) error {
	if publisher == nil {
		return nil
	}
	publisher.mutex.Lock()
	if !publisher.closed {
		publisher.closed = true
		close(publisher.queue)
	}
	publisher.mutex.Unlock()
	done := make(chan struct{})
	go func() { publisher.wait.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (publisher *BufferedPublisher) Metrics() BufferedPublisherMetrics {
	if publisher == nil {
		return BufferedPublisherMetrics{}
	}
	return BufferedPublisherMetrics{Queued: publisher.queued.Load(), Published: publisher.published.Load(), Failures: publisher.failures.Load(), Backpressure: publisher.pressure.Load(), Synchronous: publisher.synchronous.Load()}
}

var _ corestream.Sink = (*Publisher)(nil)
var _ corestream.BatchSink = (*Publisher)(nil)
