package skillsync

import (
	"errors"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/syncstream"
)

type deleteFailureStore struct {
	records map[pendingID]OutboxRecord
	err     error
	failAt  int
	deletes int
}

type acknowledgeFailureJournal struct {
	fail error
}

func (journal *acknowledgeFailureJournal) Load() (syncstream.HistorySnapshot, error) {
	return syncstream.HistorySnapshot{Version: syncstream.HistorySnapshotVersion, Epoch: 1}, nil
}
func (journal *acknowledgeFailureJournal) Record(mutation syncstream.HistoryMutation) error {
	if mutation.Kind == syncstream.HistoryMutationAcknowledge {
		return journal.fail
	}
	return nil
}
func (*acknowledgeFailureJournal) Checkpoint(syncstream.HistorySnapshot) error { return nil }

func coordinatorForDurabilityTest(history *syncstream.History, box *Outbox) *Coordinator {
	return &Coordinator{history: history, outbox: box, viewLocks: make(map[observerKey]*viewLockEntry), closedObservers: make(map[syncstream.Observer]struct{})}
}

func (store *deleteFailureStore) Load() ([]syncstream.Packet, error) {
	result := make([]syncstream.Packet, 0, len(store.records))
	for _, record := range store.records {
		result = append(result, record.Packet.Clone())
	}
	return result, nil
}
func (store *deleteFailureStore) LoadRecords() ([]OutboxRecord, error) {
	result := make([]OutboxRecord, 0, len(store.records))
	for _, record := range store.records {
		result = append(result, record)
	}
	return result, nil
}
func (store *deleteFailureStore) Put(packet syncstream.Packet) error {
	return store.PutRecord(OutboxRecord{Packet: packet})
}
func (store *deleteFailureStore) PutRecord(record OutboxRecord) error {
	if store.records == nil {
		store.records = make(map[pendingID]OutboxRecord)
	}
	store.records[packetID(record.Packet)] = record
	return nil
}
func (store *deleteFailureStore) Delete(observer syncstream.Observer, stream syncstream.Stream, epoch, sequence uint64) error {
	store.deletes++
	if store.err != nil && (store.failAt == 0 || store.deletes == store.failAt) {
		return store.err
	}
	delete(store.records, pendingID{observer: observer, stream: stream, epoch: epoch, sequence: sequence})
	return nil
}

func TestCoordinatorRepairsPartialOutboxDeleteFailure(t *testing.T) {
	injected := errors.New("second delete failed")
	store := &deleteFailureStore{err: injected, failAt: 2}
	box, err := NewOutbox(OutboxOptions{Store: store, RequireDurable: true})
	if err != nil {
		t.Fatal(err)
	}
	history := syncstream.NewHistory(syncstream.HistoryOptions{SchemaVersion: 1})
	var latest syncstream.Packet
	for index := 0; index < 2; index++ {
		latest, err = history.Append(syncstream.Packet{Observer: syncstream.Observer{ID: 1}, Stream: syncstream.Stream{Topic: TopicState, Key: 1}, SchemaVersion: 1})
		if err != nil {
			t.Fatal(err)
		}
		if err := box.Put(latest); err != nil {
			t.Fatal(err)
		}
	}
	coordinator := coordinatorForDurabilityTest(history, box)
	if err := coordinator.Acknowledge(latest.Observer, latest.Stream, latest.Epoch, latest.Sequence); !errors.Is(err, injected) {
		t.Fatalf("ack error = %v", err)
	}
	if metrics := box.Metrics(); metrics.Pending != 2 {
		t.Fatalf("partial delete was not reconciled: %#v", metrics)
	}
	if status := history.Status(latest.Observer, latest.Stream); status.AckedSequence != 0 {
		t.Fatalf("partial outbox failure advanced History: %#v", status)
	}
}

func TestCoordinatorAckKeepsHistoryWhenOutboxDeleteFails(t *testing.T) {
	injected := errors.New("delete failed")
	store := &deleteFailureStore{err: injected}
	box, err := NewOutbox(OutboxOptions{Store: store, RequireDurable: true})
	if err != nil {
		t.Fatal(err)
	}
	history := syncstream.NewHistory(syncstream.HistoryOptions{SchemaVersion: 1})
	packet, err := history.Append(syncstream.Packet{Observer: syncstream.Observer{ID: 1}, Stream: syncstream.Stream{Topic: TopicState, Key: 1}, SchemaVersion: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := box.Put(packet); err != nil {
		t.Fatal(err)
	}
	coordinator := coordinatorForDurabilityTest(history, box)
	if err := coordinator.Acknowledge(packet.Observer, packet.Stream, packet.Epoch, packet.Sequence); !errors.Is(err, injected) {
		t.Fatalf("ack error = %v", err)
	}
	status := history.Status(packet.Observer, packet.Stream)
	if status.AckedSequence != 0 || status.Retained != 1 {
		t.Fatalf("history advanced before outbox durability: %#v", status)
	}
}

func TestCoordinatorRejectsAckAheadBeforeDeletingOutbox(t *testing.T) {
	box, err := NewOutbox(OutboxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	history := syncstream.NewHistory(syncstream.HistoryOptions{SchemaVersion: 1})
	packet, err := history.Append(syncstream.Packet{Observer: syncstream.Observer{ID: 1}, Stream: syncstream.Stream{Topic: TopicState, Key: 1}, SchemaVersion: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := box.Put(packet); err != nil {
		t.Fatal(err)
	}
	coordinator := coordinatorForDurabilityTest(history, box)
	if err := coordinator.Acknowledge(packet.Observer, packet.Stream, packet.Epoch, packet.Sequence+1); !errors.Is(err, syncstream.ErrAckAhead) {
		t.Fatalf("ack error = %v", err)
	}
	if metrics := box.Metrics(); metrics.Pending != 1 {
		t.Fatalf("outbox deleted before ACK validation: %#v", metrics)
	}
}

func TestCoordinatorRepairsOutboxWhenHistoryAckFails(t *testing.T) {
	injected := errors.New("journal failed")
	journal := &acknowledgeFailureJournal{fail: injected}
	history, err := syncstream.NewHistoryWithJournal(syncstream.HistoryOptions{SchemaVersion: 1, Epoch: 1}, journal)
	if err != nil {
		t.Fatal(err)
	}
	packet, err := history.Append(syncstream.Packet{Observer: syncstream.Observer{ID: 1}, Stream: syncstream.Stream{Topic: TopicState, Key: 1}, SchemaVersion: 1})
	if err != nil {
		t.Fatal(err)
	}
	box, err := NewOutbox(OutboxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := box.Put(packet); err != nil {
		t.Fatal(err)
	}
	coordinator := coordinatorForDurabilityTest(history, box)
	if err := coordinator.Acknowledge(packet.Observer, packet.Stream, packet.Epoch, packet.Sequence); !errors.Is(err, injected) {
		t.Fatalf("ack error = %v", err)
	}
	if status := history.Status(packet.Observer, packet.Stream); status.AckedSequence != 0 {
		t.Fatalf("failed ACK advanced History: %#v", status)
	}
	if metrics := box.Metrics(); metrics.Pending != 1 {
		t.Fatalf("failed ACK did not repair outbox: %#v", metrics)
	}
}

func TestCoordinatorCloseKeepsHistoryWhenOutboxDeleteFails(t *testing.T) {
	injected := errors.New("delete failed")
	store := &deleteFailureStore{err: injected}
	box, err := NewOutbox(OutboxOptions{Store: store, RequireDurable: true})
	if err != nil {
		t.Fatal(err)
	}
	history := syncstream.NewHistory(syncstream.HistoryOptions{SchemaVersion: 1})
	packet, err := history.Append(syncstream.Packet{Observer: syncstream.Observer{ID: 1}, Stream: syncstream.Stream{Topic: TopicState, Key: 1}, SchemaVersion: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := box.Put(packet); err != nil {
		t.Fatal(err)
	}
	coordinator := &Coordinator{history: history, outbox: box, closedObservers: make(map[syncstream.Observer]struct{}), closingObservers: make(map[syncstream.Observer]struct{}), viewLocks: make(map[observerKey]*viewLockEntry), cursors: make(map[observerKey]sourceCursor)}
	if err := coordinator.CloseObserver(packet.Observer); !errors.Is(err, injected) {
		t.Fatalf("close error = %v", err)
	}
	if status := history.Status(packet.Observer, packet.Stream); status.Retained != 1 {
		t.Fatalf("history deleted before outbox durability: %#v", status)
	}
}
