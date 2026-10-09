package skillsync

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/gameplay/skill"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/syncstream"
)

// RR-20261007-09：新 observer 以当前快照为基线，不重播加入前的一次性表现。
func TestSnapshotStartsPresentationAtCurrentRuntime(t *testing.T) {
	runtime, program := visibilityRuntime(t, skill.RuntimeOptions{})
	if _, err := runtime.Start(program, skill.CastInput{Caster: 1, Target: 2}); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Advance(10); err != nil {
		t.Fatal(err)
	}
	publisher := &recordingPublisher{}
	projector, _ := NewProjector(1)
	c, err := NewCoordinator(CoordinatorOptions{Runtime: runtime, History: syncstream.NewHistory(syncstream.HistoryOptions{SchemaVersion: 1}), Publisher: publisher, Projector: projector, Visibility: AllowAllVisibility{}})
	if err != nil {
		t.Fatal(err)
	}
	observer := syncstream.Observer{ID: 9}
	if err := c.OpenObserver(observer); err != nil {
		t.Fatal(err)
	}
	if err := c.PublishSnapshot(observer, 1); err != nil {
		t.Fatal(err)
	}
	if err := c.Flush(observer, 1); err != nil {
		t.Fatal(err)
	}
	reset := false
	for _, packet := range publisher.packets {
		if packet.Stream.Topic != TopicPresentation {
			continue
		}
		var record PresentationRecord
		if err := json.Unmarshal(packet.Payload, &record); err != nil {
			t.Fatal(err)
		}
		if record.Event != nil {
			t.Error("late observer received historical presentation event")
		}
		reset = reset || record.Reset != nil
	}
	if !reset {
		t.Error("late observer did not receive current persistent presentation reset")
	}
}

type rejectingPacketStore struct{ fail bool }

func (*rejectingPacketStore) Load() ([]syncstream.Packet, error)    { return nil, nil }
func (*rejectingPacketStore) LoadRecords() ([]OutboxRecord, error)  { return nil, nil }
func (s *rejectingPacketStore) PutRecord(record OutboxRecord) error { return s.Put(record.Packet) }
func (s *rejectingPacketStore) Put(syncstream.Packet) error {
	if s.fail {
		return errors.New("outbox unavailable")
	}
	return nil
}
func (*rejectingPacketStore) Delete(syncstream.Observer, syncstream.Stream, uint64, uint64) error {
	return nil
}

// RR-20261007-10：History 已接受的 mutation 在 outbox 拒收后只能补交付，不能再次追加。
func TestOutboxFailureDoesNotAppendTheSameSourceAgain(t *testing.T) {
	runtime, _ := visibilityRuntime(t, skill.RuntimeOptions{})
	if err := runtime.Advance(1); err != nil {
		t.Fatal(err)
	}
	store := &rejectingPacketStore{fail: true}
	box, err := NewOutbox(OutboxOptions{Store: store})
	if err != nil {
		t.Fatal(err)
	}
	history := syncstream.NewHistory(syncstream.HistoryOptions{SchemaVersion: 1})
	projector, _ := NewProjector(1)
	c, err := NewCoordinator(CoordinatorOptions{Runtime: runtime, History: history, Outbox: box, Publisher: &recordingPublisher{}, Projector: projector, Visibility: AllowAllVisibility{}})
	if err != nil {
		t.Fatal(err)
	}
	observer := syncstream.Observer{ID: 10}
	if err := c.OpenObserver(observer); err != nil {
		t.Fatal(err)
	}
	stream := syncstream.Stream{Topic: TopicState, Key: 1}
	for range 3 {
		if err := c.Flush(observer, 1); err == nil {
			t.Fatal("outbox refusal hidden")
		}
	}
	if got := history.Status(observer, stream).Retained; got != 1 {
		t.Fatalf("history retained=%d want one accepted source mutation", got)
	}
	store.fail = false
	if err := c.Flush(observer, 1); err != nil {
		t.Fatal(err)
	}
	if got := history.Status(observer, stream).Retained; got != 1 {
		t.Fatalf("retry appended duplicate: %d", got)
	}
}

func TestFullRecoveryAdvancesSourceCursor(t *testing.T) {
	runtime, _ := visibilityRuntime(t, skill.RuntimeOptions{})
	if err := runtime.Advance(1); err != nil {
		t.Fatal(err)
	}
	history := syncstream.NewHistory(syncstream.HistoryOptions{SchemaVersion: 1})
	projector, _ := NewProjector(1)
	c, err := NewCoordinator(CoordinatorOptions{Runtime: runtime, History: history, Publisher: &recordingPublisher{}, Projector: projector, Visibility: AllowAllVisibility{}})
	if err != nil {
		t.Fatal(err)
	}
	observer := syncstream.Observer{ID: 11}
	if err := c.OpenObserver(observer); err != nil {
		t.Fatal(err)
	}
	stream := syncstream.Stream{Topic: TopicState, Key: 1}
	if _, err := c.Recover(syncstream.ResyncRequest{Observer: observer, Stream: stream, SchemaVersion: 1}); err != nil {
		t.Fatal(err)
	}
	if err := c.Flush(observer, 1); err != nil {
		t.Fatal(err)
	}
	if got := history.Status(observer, stream).Retained; got != 1 {
		t.Fatalf("full recovery replayed %d old state records", got-1)
	}
}
