package skillsync

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/syncstream"
)

type failingUpdateStore struct {
	record    OutboxRecord
	writes    int
	updateErr error
}

func (store *failingUpdateStore) Load() ([]syncstream.Packet, error) { return nil, nil }
func (store *failingUpdateStore) LoadRecords() ([]OutboxRecord, error) {
	if store.writes == 0 {
		return nil, nil
	}
	return []OutboxRecord{store.record}, nil
}
func (store *failingUpdateStore) Put(packet syncstream.Packet) error {
	return store.PutRecord(OutboxRecord{Packet: packet, CreatedAt: time.Now()})
}
func (store *failingUpdateStore) PutRecord(record OutboxRecord) error {
	store.writes++
	if store.writes > 1 {
		return store.updateErr
	}
	store.record = record
	return nil
}
func (*failingUpdateStore) Delete(syncstream.Observer, syncstream.Stream, uint64, uint64) error {
	return nil
}

type fixedErrorPublisher struct{ err error }

func (publisher fixedErrorPublisher) Publish(syncstream.Packet) error { return publisher.err }

type blockingPublisher struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (publisher *blockingPublisher) Publish(syncstream.Packet) error {
	publisher.once.Do(func() { close(publisher.started) })
	<-publisher.release
	return nil
}

func TestPublishPersistsRetryMetadata(t *testing.T) {
	store, err := NewFileOutboxStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	box, err := NewOutbox(OutboxOptions{Store: store, RequireDurable: true, AckRetryInterval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	packet := syncstream.Packet{Observer: syncstream.Observer{ID: 1}, Stream: syncstream.Stream{Topic: TopicState, Key: 1}, Epoch: 1, Sequence: 1}
	if err := box.Put(packet); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := box.PublishDue(&recordingPublisher{}, now, nil, nil); err != nil {
		t.Fatal(err)
	}
	records, err := store.LoadRecords()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].Attempts != 1 || !records[0].NextAttempt.Equal(now.Add(time.Hour)) {
		t.Fatalf("records=%#v", records)
	}
	restarted, err := NewOutbox(OutboxOptions{Store: store, RequireDurable: true, AckRetryInterval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	publisher := &recordingPublisher{}
	if err := restarted.PublishDue(publisher, now.Add(time.Minute), nil, nil); err != nil || len(publisher.packets) != 0 {
		t.Fatalf("packets=%d err=%v", len(publisher.packets), err)
	}
}

func TestPublishDueHonorsBatchLimit(t *testing.T) {
	box, err := NewOutbox(OutboxOptions{MaxPublishBatch: 2})
	if err != nil {
		t.Fatal(err)
	}
	for sequence := uint64(1); sequence <= 3; sequence++ {
		if err := box.Put(syncstream.Packet{Observer: syncstream.Observer{ID: 1}, Stream: syncstream.Stream{Topic: TopicState, Key: 1}, Epoch: 1, Sequence: sequence}); err != nil {
			t.Fatal(err)
		}
	}
	publisher := &recordingPublisher{}
	if err := box.PublishDue(publisher, time.Now(), nil, nil); err != nil {
		t.Fatal(err)
	}
	if len(publisher.packets) != 2 {
		t.Fatalf("published=%d", len(publisher.packets))
	}
}

func TestConcurrentPublishDueDoesNotPublishSamePacket(t *testing.T) {
	box, err := NewOutbox(OutboxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	packet := syncstream.Packet{Observer: syncstream.Observer{ID: 1}, Stream: syncstream.Stream{Topic: TopicState, Key: 1}, Epoch: 1, Sequence: 1}
	if err := box.Put(packet); err != nil {
		t.Fatal(err)
	}
	publisher := &blockingPublisher{started: make(chan struct{}), release: make(chan struct{})}
	firstDone := make(chan error, 1)
	go func() { firstDone <- box.PublishDue(publisher, time.Now(), nil, nil) }()
	select {
	case <-publisher.started:
	case <-time.After(time.Second):
		t.Fatal("first publisher did not start")
	}

	second := &recordingPublisher{}
	if err := box.PublishDue(second, time.Now(), nil, nil); err != nil {
		t.Fatal(err)
	}
	if len(second.packets) != 0 {
		t.Fatalf("concurrent duplicate publishes=%d", len(second.packets))
	}
	close(publisher.release)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
}

func TestPublishDueJoinsPublisherAndMetadataErrors(t *testing.T) {
	publishErr, updateErr := errors.New("publish"), errors.New("metadata")
	store := &failingUpdateStore{updateErr: updateErr}
	box, err := NewOutbox(OutboxOptions{Store: store, RequireDurable: true})
	if err != nil {
		t.Fatal(err)
	}
	packet := syncstream.Packet{Observer: syncstream.Observer{ID: 1}, Stream: syncstream.Stream{Topic: TopicState, Key: 1}, Epoch: 1, Sequence: 1}
	if err := box.Put(packet); err != nil {
		t.Fatal(err)
	}
	err = box.PublishDue(fixedErrorPublisher{err: publishErr}, time.Now(), nil, nil)
	if !errors.Is(err, publishErr) || !errors.Is(err, updateErr) {
		t.Fatalf("joined error = %v", err)
	}
}

func TestFileOutboxEnforcesRecordLimitOnPut(t *testing.T) {
	store, err := NewFileOutboxStoreWithOptions(t.TempDir(), FileOutboxOptions{MaxRecords: 1})
	if err != nil {
		t.Fatal(err)
	}
	first := syncstream.Packet{Observer: syncstream.Observer{ID: 1}, Stream: syncstream.Stream{Topic: TopicState, Key: 1}, Epoch: 1, Sequence: 1}
	if err := store.Put(first); err != nil {
		t.Fatal(err)
	}
	second := first
	second.Sequence = 2
	if err := store.Put(second); !errors.Is(err, ErrOutboxStoreLimit) {
		t.Fatalf("limit error = %v", err)
	}
}
