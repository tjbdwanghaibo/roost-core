package skillsync

import (
	"container/heap"
	"encoding/json"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/sync/syncstream"
)

var (
	ErrOutboxStoreRequired       = errors.New("skillsync: durable outbox store is required")
	ErrOutboxRecordStoreRequired = errors.New("skillsync: outbox store must persist record age and retry metadata")
	ErrOutboxCapacityExceeded    = errors.New("skillsync: outbox capacity exceeded")
	ErrOutboxPendingTooOld       = errors.New("skillsync: oldest outbox packet exceeds maximum age")
	ErrOutboxStoreLimit          = errors.New("skillsync: outbox store exceeds configured limits")
)

type OutboxStore interface {
	Load() ([]syncstream.Packet, error)
	Put(syncstream.Packet) error
	Delete(syncstream.Observer, syncstream.Stream, uint64, uint64) error
}

type OutboxDelete struct {
	Observer syncstream.Observer
	Stream   syncstream.Stream
	Epoch    uint64
	Sequence uint64
}

// BatchOutboxStore can make range ACK deletion atomic. Implementations must
// either delete every key or none of them.
type BatchOutboxStore interface {
	DeleteBatch([]OutboxDelete) error
}

type OutboxRecord struct {
	Packet      syncstream.Packet `json:"packet"`
	CreatedAt   time.Time         `json:"created_at"`
	Attempts    uint32            `json:"attempts,omitempty"`
	NextAttempt time.Time         `json:"next_attempt,omitempty"`
}

// RecordOutboxStore persists retry metadata and packet age across restarts.
// PutRecord is an atomic upsert for the packet identity.
type RecordOutboxStore interface {
	LoadRecords() ([]OutboxRecord, error)
	PutRecord(OutboxRecord) error
}

// BatchRecordOutboxStore persists a group atomically. Stores backed by a
// transactional database should implement it; other stores must make each
// PutRecord upsert crash-safe and idempotent.
type BatchRecordOutboxStore interface {
	PutRecords([]OutboxRecord) error
}

type pendingID struct {
	observer syncstream.Observer
	stream   syncstream.Stream
	epoch    uint64
	sequence uint64
}

type pendingPacket struct {
	packet      syncstream.Packet
	attempts    uint32
	nextAttempt time.Time
	createdAt   time.Time
	bytes       int64
	publishing  bool
	// delivered 只记录本进程曾成功交付；重启后保守重发，不能靠尝试次数推断成功。
	delivered bool
}

type pendingStreamID struct {
	observer syncstream.Observer
	stream   syncstream.Stream
	epoch    uint64
}

type OutboxOptions struct {
	Store               OutboxStore
	RequireDurable      bool
	AckRetryInterval    time.Duration
	FailureRetryDelay   time.Duration
	MaxRetryDelay       time.Duration
	MaxPendingPackets   int
	MaxPendingBytes     int64
	MaxPendingPerStream int
	MaxPendingAge       time.Duration
	MaxPublishBatch     int
}

type OutboxMetrics struct {
	Pending          int
	PendingBytes     int64
	OldestPendingAge time.Duration
	PublishAttempts  uint64
	PublishSuccesses uint64
	PublishFailures  uint64
	Acknowledged     uint64
}

// Outbox retains every appended packet until client ACK, not merely until the
// transport accepts it. This closes the undetectable "last publish failed"
// window.
type Outbox struct {
	mutex           sync.Mutex
	store           OutboxStore
	ackRetry        time.Duration
	failureRetry    time.Duration
	maxRetry        time.Duration
	maxPackets      int
	maxBytes        int64
	maxPerStream    int
	maxAge          time.Duration
	maxPublish      int
	pendingBytes    int64
	pending         map[pendingID]*pendingPacket
	streamPending   map[pendingStreamID]map[uint64]pendingID
	oldestCreatedAt time.Time
	metrics         OutboxMetrics
}

func NewOutbox(options OutboxOptions) (*Outbox, error) {
	if options.RequireDurable && options.Store == nil {
		return nil, ErrOutboxStoreRequired
	}
	if options.Store != nil {
		if _, ok := options.Store.(RecordOutboxStore); !ok {
			return nil, ErrOutboxRecordStoreRequired
		}
	}
	if options.AckRetryInterval <= 0 {
		options.AckRetryInterval = 5 * time.Second
	}
	if options.FailureRetryDelay <= 0 {
		options.FailureRetryDelay = 100 * time.Millisecond
	}
	if options.MaxRetryDelay <= 0 {
		options.MaxRetryDelay = 30 * time.Second
	}
	if options.MaxPendingPackets <= 0 {
		options.MaxPendingPackets = 100000
	}
	if options.MaxPendingBytes <= 0 {
		options.MaxPendingBytes = 256 << 20
	}
	if options.MaxPendingPerStream <= 0 {
		options.MaxPendingPerStream = 4096
	}
	if options.MaxPendingAge <= 0 {
		options.MaxPendingAge = 24 * time.Hour
	}
	if options.MaxPublishBatch <= 0 {
		options.MaxPublishBatch = 512
	}
	box := &Outbox{store: options.Store, ackRetry: options.AckRetryInterval, failureRetry: options.FailureRetryDelay, maxRetry: options.MaxRetryDelay, maxPackets: options.MaxPendingPackets, maxBytes: options.MaxPendingBytes, maxPerStream: options.MaxPendingPerStream, maxAge: options.MaxPendingAge, maxPublish: options.MaxPublishBatch, pending: make(map[pendingID]*pendingPacket), streamPending: make(map[pendingStreamID]map[uint64]pendingID)}
	if options.Store != nil {
		records, err := loadOutboxRecords(options.Store)
		if err != nil {
			return nil, err
		}
		for _, record := range records {
			packet := record.Packet
			if packet.Stream.Topic == "" || packet.Epoch == 0 || packet.Sequence == 0 {
				return nil, ErrRecordInvalid
			}
			size, err := outboxPacketBytes(packet)
			if err != nil {
				return nil, err
			}
			if record.CreatedAt.IsZero() {
				return nil, ErrRecordInvalid
			}
			id := packetID(packet)
			if _, duplicate := box.pending[id]; duplicate {
				return nil, ErrRecordInvalid
			}
			box.addPendingLocked(id, &pendingPacket{packet: packet.Clone(), attempts: record.Attempts, nextAttempt: record.NextAttempt, createdAt: record.CreatedAt, bytes: size})
		}
		if err := box.capacityError(time.Now(), nil, 0); err != nil {
			return nil, err
		}
	}
	return box, nil
}

func packetID(packet syncstream.Packet) pendingID {
	return pendingID{observer: packet.Observer, stream: packet.Stream, epoch: packet.Epoch, sequence: packet.Sequence}
}

func streamID(packet syncstream.Packet) pendingStreamID {
	return pendingStreamID{observer: packet.Observer, stream: packet.Stream, epoch: packet.Epoch}
}

func loadOutboxRecords(store OutboxStore) ([]OutboxRecord, error) {
	if records, ok := store.(RecordOutboxStore); ok {
		return records.LoadRecords()
	}
	return nil, ErrOutboxRecordStoreRequired
}

func outboxPacketBytes(packet syncstream.Packet) (int64, error) {
	data, err := json.Marshal(packet)
	return int64(len(data)), err
}

func (box *Outbox) capacityError(now time.Time, packet *syncstream.Packet, additionalBytes int64) error {
	additionalPackets := 0
	if packet != nil {
		additionalPackets = 1
	}
	if len(box.pending)+additionalPackets > box.maxPackets || box.pendingBytes+additionalBytes > box.maxBytes {
		return ErrOutboxCapacityExceeded
	}
	if packet != nil {
		if len(box.streamPending[streamID(*packet)])+1 > box.maxPerStream {
			return ErrOutboxCapacityExceeded
		}
	}
	if packet != nil {
		return box.streamAgeError(now, streamID(*packet))
	}
	return nil
}

// 过龄只限制同流继续入账，不能阻止旧包重发、ACK 或其他 observer 恢复。
func (box *Outbox) streamAgeError(now time.Time, stream pendingStreamID) error {
	for _, id := range box.streamPending[stream] {
		if entry := box.pending[id]; box.maxAge > 0 && now.Sub(entry.createdAt) > box.maxAge {
			return ErrOutboxPendingTooOld
		}
	}
	return nil
}

func (box *Outbox) Put(packet syncstream.Packet) error {
	if box == nil || packet.Stream.Topic == "" || packet.Epoch == 0 || packet.Sequence == 0 {
		return ErrRecordInvalid
	}
	box.mutex.Lock()
	defer box.mutex.Unlock()
	id := packetID(packet)
	if _, exists := box.pending[id]; exists {
		return nil
	}
	size, err := outboxPacketBytes(packet)
	if err != nil {
		return err
	}
	now := time.Now()
	if err := box.capacityError(now, &packet, size); err != nil {
		return err
	}
	if box.store != nil {
		var err error
		if records, ok := box.store.(RecordOutboxStore); ok {
			err = records.PutRecord(OutboxRecord{Packet: packet.Clone(), CreatedAt: now})
		} else {
			err = box.store.Put(packet.Clone())
		}
		if err != nil {
			return err
		}
	}
	box.addPendingLocked(id, &pendingPacket{packet: packet.Clone(), createdAt: now, bytes: size})
	return nil
}

func (box *Outbox) PutBatch(packets []syncstream.Packet) error {
	if box == nil {
		return ErrRecordInvalid
	}
	box.mutex.Lock()
	defer box.mutex.Unlock()
	now := time.Now()
	if err := box.capacityError(now, nil, 0); err != nil {
		return err
	}
	records := make([]OutboxRecord, 0, len(packets))
	entries := make([]struct {
		id    pendingID
		entry *pendingPacket
	}, 0, len(packets))
	seen := make(map[pendingID]struct{}, len(packets))
	additionalBytes := int64(0)
	additionalPerStream := make(map[pendingStreamID]int)
	for _, packet := range packets {
		if packet.Stream.Topic == "" || packet.Epoch == 0 || packet.Sequence == 0 {
			return ErrRecordInvalid
		}
		id := packetID(packet)
		if _, exists := box.pending[id]; exists {
			continue
		}
		if _, duplicate := seen[id]; duplicate {
			continue
		}
		seen[id] = struct{}{}
		size, err := outboxPacketBytes(packet)
		if err != nil {
			return err
		}
		additionalBytes += size
		key := streamID(packet)
		additionalPerStream[key]++
		record := OutboxRecord{Packet: packet.Clone(), CreatedAt: now}
		records = append(records, record)
		entries = append(entries, struct {
			id    pendingID
			entry *pendingPacket
		}{id: id, entry: &pendingPacket{packet: packet.Clone(), createdAt: now, bytes: size}})
	}
	if len(box.pending)+len(entries) > box.maxPackets || box.pendingBytes+additionalBytes > box.maxBytes {
		return ErrOutboxCapacityExceeded
	}
	for key, additional := range additionalPerStream {
		if len(box.streamPending[key])+additional > box.maxPerStream {
			return ErrOutboxCapacityExceeded
		}
		if err := box.streamAgeError(now, key); err != nil {
			return err
		}
	}
	if batch, ok := box.store.(BatchRecordOutboxStore); ok {
		if err := batch.PutRecords(records); err != nil {
			return err
		}
	} else if box.store != nil {
		for _, record := range records {
			var err error
			if store, ok := box.store.(RecordOutboxStore); ok {
				err = store.PutRecord(record)
			} else {
				err = box.store.Put(record.Packet)
			}
			if err != nil {
				return err
			}
		}
	}
	for _, value := range entries {
		box.addPendingLocked(value.id, value.entry)
	}
	return nil
}

// Reconcile repairs the crash window between a committed History append and
// outbox persistence. Every retained packet newer than ACK becomes pending.
func (box *Outbox) Reconcile(snapshot syncstream.HistorySnapshot) error {
	// History 是身份权威。epoch 切换、删除流或已落盘 ACK 留下的派生记录
	// 必须退休；不能无限重发客户端已无法确认的身份。
	streams := make(map[pendingStreamID]uint64, len(snapshot.Streams))
	for _, stream := range snapshot.Streams {
		streams[pendingStreamID{observer: stream.Observer, stream: stream.Stream, epoch: snapshot.Epoch}] = stream.Acked
	}
	box.mutex.Lock()
	obsolete := make([]pendingID, 0)
	for id, entry := range box.pending {
		acked, exists := streams[streamID(entry.packet)]
		if !exists || id.sequence <= acked {
			if entry.publishing {
				box.mutex.Unlock()
				return ErrApplyInProgress
			}
			obsolete = append(obsolete, id)
		}
	}
	if err := box.deletePendingLocked(obsolete, false); err != nil {
		box.mutex.Unlock()
		return err
	}
	box.mutex.Unlock()
	packets := make([]syncstream.Packet, 0)
	for _, stream := range snapshot.Streams {
		for _, packet := range stream.Packets {
			if packet.Sequence > stream.Acked {
				packets = append(packets, packet)
			}
		}
	}
	return box.PutBatch(packets)
}

func (box *Outbox) Acknowledge(observer syncstream.Observer, stream syncstream.Stream, epoch, sequence uint64) error {
	if box == nil {
		return nil
	}
	box.mutex.Lock()
	defer box.mutex.Unlock()
	key := pendingStreamID{observer: observer, stream: stream, epoch: epoch}
	ids := make([]pendingID, 0, len(box.streamPending[key]))
	for pendingSequence, id := range box.streamPending[key] {
		if pendingSequence <= sequence {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i].sequence < ids[j].sequence })
	return box.deletePendingLocked(ids, true)
}

func (box *Outbox) DiscardObserver(observer syncstream.Observer) error {
	if box == nil {
		return nil
	}
	box.mutex.Lock()
	defer box.mutex.Unlock()
	ids := make([]pendingID, 0)
	for id, entry := range box.pending {
		if id.observer == observer {
			// 不在这里等待未知时长的发布回调。成功删除才是关闭屏障；在飞时
			// 明确返回可重试错误，调用方不能把关闭请求当成关闭完成。
			if entry.publishing {
				return ErrApplyInProgress
			}
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool {
		if ids[i].stream.Topic != ids[j].stream.Topic {
			return ids[i].stream.Topic < ids[j].stream.Topic
		}
		if ids[i].stream.Key != ids[j].stream.Key {
			return ids[i].stream.Key < ids[j].stream.Key
		}
		return ids[i].sequence < ids[j].sequence
	})
	return box.deletePendingLocked(ids, false)
}

func (box *Outbox) deletePendingLocked(ids []pendingID, acknowledged bool) error {
	if len(ids) == 0 {
		return nil
	}
	if batch, ok := box.store.(BatchOutboxStore); ok {
		deletes := make([]OutboxDelete, len(ids))
		for index, id := range ids {
			deletes[index] = OutboxDelete{Observer: id.observer, Stream: id.stream, Epoch: id.epoch, Sequence: id.sequence}
		}
		if err := batch.DeleteBatch(deletes); err != nil {
			return err
		}
		for _, id := range ids {
			box.removePendingLocked(id, acknowledged)
		}
		return nil
	}
	for _, id := range ids {
		if box.store != nil {
			if err := box.store.Delete(id.observer, id.stream, id.epoch, id.sequence); err != nil {
				return err
			}
		}
		box.removePendingLocked(id, acknowledged)
	}
	return nil
}

func (box *Outbox) removePendingLocked(id pendingID, acknowledged bool) {
	entry := box.pending[id]
	if entry == nil {
		return
	}
	delete(box.pending, id)
	box.pendingBytes -= entry.bytes
	key := streamID(entry.packet)
	delete(box.streamPending[key], id.sequence)
	if len(box.streamPending[key]) == 0 {
		delete(box.streamPending, key)
	}
	if entry.createdAt.Equal(box.oldestCreatedAt) {
		box.recomputeOldestCreatedAt()
	}
	if box.pendingBytes < 0 {
		box.pendingBytes = 0
	}
	if acknowledged {
		box.metrics.Acknowledged++
	}
}

func (box *Outbox) addPendingLocked(id pendingID, entry *pendingPacket) {
	box.pending[id] = entry
	box.pendingBytes += entry.bytes
	key := streamID(entry.packet)
	sequences := box.streamPending[key]
	if sequences == nil {
		sequences = make(map[uint64]pendingID)
		box.streamPending[key] = sequences
	}
	sequences[id.sequence] = id
	box.noteCreatedAt(entry.createdAt)
}

func (box *Outbox) due(now time.Time, observer *syncstream.Observer, stream *syncstream.Stream, allowed func(syncstream.Observer) bool) []syncstream.Packet {
	box.mutex.Lock()
	defer box.mutex.Unlock()
	packets := make(packetMaxHeap, 0, box.maxPublish)
	// 同流有在飞批次时不能另起一批；失败退避中的包阻挡后面的首次交付。
	busy := make(map[pendingStreamID]bool)
	blockedAt := make(map[pendingStreamID]uint64)
	for _, entry := range box.pending {
		key := streamID(entry.packet)
		if entry.publishing {
			busy[key] = true
		}
		if !entry.delivered && entry.nextAttempt.After(now) {
			if current := blockedAt[key]; current == 0 || entry.packet.Sequence < current {
				blockedAt[key] = entry.packet.Sequence
			}
		}
	}
	for _, entry := range box.pending {
		if observer != nil && entry.packet.Observer != *observer || stream != nil && entry.packet.Stream != *stream {
			continue
		}
		if allowed != nil && !allowed(entry.packet.Observer) {
			continue
		}
		key := streamID(entry.packet)
		if busy[key] || blockedAt[key] != 0 && entry.packet.Sequence > blockedAt[key] {
			continue
		}
		if entry.publishing || !entry.nextAttempt.IsZero() && entry.nextAttempt.After(now) {
			continue
		}
		if len(packets) < box.maxPublish {
			heap.Push(&packets, entry.packet.Clone())
			continue
		}
		if packetLess(entry.packet, packets[0]) {
			packets[0] = entry.packet.Clone()
			heap.Fix(&packets, 0)
		}
	}
	sort.Slice(packets, func(i, j int) bool { return packetLess(packets[i], packets[j]) })
	for _, packet := range packets {
		if entry := box.pending[packetID(packet)]; entry != nil {
			entry.publishing = true
		}
	}
	return packets
}

// packetMaxHeap keeps the least preferred selected packet at index zero, so
// due can retain only the best MaxPublishBatch candidates while scanning.
type packetMaxHeap []syncstream.Packet

func (packets packetMaxHeap) Len() int { return len(packets) }
func (packets packetMaxHeap) Less(i, j int) bool {
	return packetLess(packets[j], packets[i])
}
func (packets packetMaxHeap) Swap(i, j int) { packets[i], packets[j] = packets[j], packets[i] }
func (packets *packetMaxHeap) Push(value any) {
	*packets = append(*packets, value.(syncstream.Packet))
}
func (packets *packetMaxHeap) Pop() any {
	values := *packets
	last := values[len(values)-1]
	*packets = values[:len(values)-1]
	return last
}

func packetLess(left, right syncstream.Packet) bool {
	if left.Observer != right.Observer {
		return observerLess(left.Observer, right.Observer)
	}
	if left.Stream.Topic != right.Stream.Topic {
		leftPriority, rightPriority := topicPriority(left.Stream.Topic), topicPriority(right.Stream.Topic)
		if leftPriority != rightPriority {
			return leftPriority < rightPriority
		}
		return left.Stream.Topic < right.Stream.Topic
	}
	if left.Stream.Key != right.Stream.Key {
		return left.Stream.Key < right.Stream.Key
	}
	if left.Epoch != right.Epoch {
		return left.Epoch < right.Epoch
	}
	return left.Sequence < right.Sequence
}

func topicPriority(topic string) int {
	switch topic {
	case TopicManifest:
		return 0
	case TopicState:
		return 1
	case TopicPresentation:
		return 2
	default:
		return 3
	}
}

func observerLess(left, right syncstream.Observer) bool {
	if left.Scope != right.Scope {
		return left.Scope < right.Scope
	}
	if left.Kind != right.Kind {
		return left.Kind < right.Kind
	}
	if left.ID != right.ID {
		return left.ID < right.ID
	}
	return left.Session < right.Session
}

// PublishDue invokes publisher without holding the outbox lock. Successful
// packets remain pending until ACK and are periodically retried to repair a
// lost final delivery.
func (box *Outbox) PublishDue(publisher PacketPublisher, now time.Time, observer *syncstream.Observer, stream *syncstream.Stream) error {
	return box.publishDue(publisher, now, observer, stream, nil)
}

func (box *Outbox) publishDue(publisher PacketPublisher, now time.Time, observer *syncstream.Observer, stream *syncstream.Stream, allowed func(syncstream.Observer) bool) error {
	if publisher == nil {
		return ErrPublisherRequired
	}
	packets := box.due(now, observer, stream, allowed)
	defer box.releasePublishing(packets)
	var firstError error
	failed := make(map[pendingStreamID]bool)
	for _, packet := range packets {
		if failed[streamID(packet)] {
			continue
		}
		box.mutex.Lock()
		_, pending := box.pending[packetID(packet)]
		box.mutex.Unlock()
		if !pending {
			continue
		} // 前一条回调可能已经累计 ACK 了本条。
		attemptErr := publisher.Publish(packet.Clone())
		box.mutex.Lock()
		box.metrics.PublishAttempts++
		if attemptErr == nil {
			box.metrics.PublishSuccesses++
		} else {
			box.metrics.PublishFailures++
		}
		entry := box.pending[packetID(packet)]
		if entry != nil {
			entry.publishing = false
			previousAttempts, previousNextAttempt := entry.attempts, entry.nextAttempt
			entry.attempts++
			if attemptErr == nil {
				entry.delivered = true
				entry.nextAttempt = now.Add(box.ackRetry)
			} else {
				delay := box.failureRetry
				for attempt := uint32(1); attempt < entry.attempts && delay < box.maxRetry; attempt++ {
					delay *= 2
				}
				if delay > box.maxRetry {
					delay = box.maxRetry
				}
				entry.nextAttempt = now.Add(delay)
			}
			if records, ok := box.store.(RecordOutboxStore); ok {
				persistErr := records.PutRecord(OutboxRecord{Packet: entry.packet.Clone(), CreatedAt: entry.createdAt, Attempts: entry.attempts, NextAttempt: entry.nextAttempt})
				if persistErr != nil {
					entry.attempts, entry.nextAttempt = previousAttempts, previousNextAttempt
					attemptErr = errors.Join(attemptErr, persistErr)
				}
			}
		}
		box.mutex.Unlock()
		if attemptErr != nil {
			failed[streamID(packet)] = true
			firstError = errors.Join(firstError, attemptErr)
		}
	}
	return firstError
}

func (box *Outbox) releasePublishing(packets []syncstream.Packet) {
	box.mutex.Lock()
	defer box.mutex.Unlock()
	for _, packet := range packets {
		if entry := box.pending[packetID(packet)]; entry != nil {
			entry.publishing = false
		}
	}
}

func (box *Outbox) PublishNow(publisher PacketPublisher, now time.Time, observer *syncstream.Observer, stream *syncstream.Stream) error {
	if box == nil {
		return nil
	}
	box.mutex.Lock()
	for _, entry := range box.pending {
		if observer != nil && entry.packet.Observer != *observer || stream != nil && entry.packet.Stream != *stream {
			continue
		}
		entry.nextAttempt = time.Time{}
	}
	box.mutex.Unlock()
	return box.PublishDue(publisher, now, observer, stream)
}

func (box *Outbox) Metrics() OutboxMetrics {
	if box == nil {
		return OutboxMetrics{}
	}
	box.mutex.Lock()
	defer box.mutex.Unlock()
	metrics := box.metrics
	metrics.Pending = len(box.pending)
	metrics.PendingBytes = box.pendingBytes
	if !box.oldestCreatedAt.IsZero() {
		metrics.OldestPendingAge = time.Since(box.oldestCreatedAt)
	}
	return metrics
}

func (box *Outbox) noteCreatedAt(createdAt time.Time) {
	if !createdAt.IsZero() && (box.oldestCreatedAt.IsZero() || createdAt.Before(box.oldestCreatedAt)) {
		box.oldestCreatedAt = createdAt
	}
}

func (box *Outbox) recomputeOldestCreatedAt() {
	box.oldestCreatedAt = time.Time{}
	for _, entry := range box.pending {
		box.noteCreatedAt(entry.createdAt)
	}
}
