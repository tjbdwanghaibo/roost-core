package skillsync

import (
	"errors"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/gameplay/skill"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/syncstream"
)

type sequencePublisher struct {
	sequences []uint64
	fail      uint64
}

func (p *sequencePublisher) Publish(packet syncstream.Packet) error {
	p.sequences = append(p.sequences, packet.Sequence)
	if packet.Sequence == p.fail {
		return errors.New("transport rejected packet")
	}
	return nil
}

func TestOutboxFailureBlocksLaterPacketsInSameStream(t *testing.T) {
	box, err := NewOutbox(OutboxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for sequence := uint64(1); sequence <= 3; sequence++ {
		if err := box.Put(syncstream.Packet{Observer: syncstream.Observer{ID: 1}, Stream: syncstream.Stream{Topic: TopicState}, Epoch: 1, Sequence: sequence}); err != nil {
			t.Fatal(err)
		}
	}
	p := &sequencePublisher{fail: 2}
	now := time.Now()
	if err := box.PublishDue(p, now, nil, nil); err == nil {
		t.Fatal("publish failure hidden")
	}
	if len(p.sequences) != 2 {
		t.Fatalf("sent sequences=%v; sequence 3 crossed failed sequence 2", p.sequences)
	}
	p.sequences = nil
	if err := box.PublishDue(p, now.Add(time.Millisecond), nil, nil); err != nil {
		t.Fatal(err)
	}
	if len(p.sequences) != 0 {
		t.Fatalf("retry backoff bypassed: %v", p.sequences)
	}
	p.fail = 0
	if err := box.PublishDue(p, now.Add(time.Second), nil, nil); err != nil {
		t.Fatal(err)
	}
	if len(p.sequences) != 2 || p.sequences[0] != 2 || p.sequences[1] != 3 {
		t.Fatalf("recovery order=%v", p.sequences)
	}
}

func TestOverageOutboxCanRestartPublishAndAcknowledge(t *testing.T) {
	store, err := NewFileOutboxStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	packet := syncstream.Packet{Observer: syncstream.Observer{ID: 1}, Stream: syncstream.Stream{Topic: TopicState}, Epoch: 1, Sequence: 1}
	if err := store.PutRecord(OutboxRecord{Packet: packet, CreatedAt: time.Now().Add(-2 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	box, err := NewOutbox(OutboxOptions{Store: store, MaxPendingAge: time.Hour})
	if err != nil {
		t.Fatalf("restart cannot recover old pending packet: %v", err)
	}
	p := &sequencePublisher{}
	if err := box.PublishDue(p, time.Now(), nil, nil); err != nil {
		t.Fatalf("old pending packet cannot be retried: %v", err)
	}
	if len(p.sequences) != 1 {
		t.Fatalf("published=%v", p.sequences)
	}
	if err := box.Acknowledge(packet.Observer, packet.Stream, packet.Epoch, packet.Sequence); err != nil {
		t.Fatal(err)
	}
	if box.Metrics().Pending != 0 {
		t.Fatal("ACK did not drain old packet")
	}
}

func TestIdleSweepPreservesUnacknowledgedDelivery(t *testing.T) {
	history := syncstream.NewHistory(syncstream.HistoryOptions{IdleTTL: time.Second})
	packet, err := history.Append(syncstream.Packet{Observer: syncstream.Observer{ID: 1}, Stream: syncstream.Stream{Topic: TopicState}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := history.SweepIdle(time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := history.AcknowledgeEpoch(packet.Observer, packet.Stream, packet.Epoch, packet.Sequence); err != nil {
		t.Fatalf("idle sweep orphaned pending ACK: %v", err)
	}
	if removed, err := history.SweepIdle(time.Now().Add(time.Hour)); err != nil || removed != 1 {
		t.Fatalf("acknowledged idle stream not reclaimed: removed=%d err=%v", removed, err)
	}
}

func TestDiscardObserverCannotSucceedWithPublishInFlight(t *testing.T) {
	box, err := NewOutbox(OutboxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	observer := syncstream.Observer{ID: 1}
	if err := box.Put(syncstream.Packet{Observer: observer, Stream: syncstream.Stream{Topic: TopicState}, Epoch: 1, Sequence: 1}); err != nil {
		t.Fatal(err)
	}
	publisher := &perObserverGate{entered: make(chan int64, 1), releases: map[int64]chan struct{}{1: make(chan struct{})}}
	done := make(chan error, 1)
	go func() { done <- box.PublishDue(publisher, time.Now(), nil, nil) }()
	select {
	case <-publisher.entered:
	case <-time.After(time.Second):
		close(publisher.releases[1])
		t.Fatal("publish did not enter")
	}
	err = box.DiscardObserver(observer)
	close(publisher.releases[1])
	if publishErr := <-done; publishErr != nil {
		t.Fatal(publishErr)
	}
	if !errors.Is(err, ErrApplyInProgress) {
		t.Fatalf("discard reported completion while publisher still active: %v", err)
	}
	if err := box.DiscardObserver(observer); err != nil {
		t.Fatal(err)
	}
	if box.Metrics().Pending != 0 {
		t.Fatal("drained observer retained packets")
	}
}

func TestEpochRotationRetiresOldOutboxBeforeRetry(t *testing.T) {
	runtime, _ := visibilityRuntime(t, skill.RuntimeOptions{})
	history := syncstream.NewHistory(syncstream.HistoryOptions{Epoch: 1})
	p := &sequencePublisher{}
	projector, _ := NewProjector(1)
	c, err := NewCoordinator(CoordinatorOptions{Runtime: runtime, History: history, Publisher: p, Projector: projector, Visibility: AllowAllVisibility{}})
	if err != nil {
		t.Fatal(err)
	}
	observer := syncstream.Observer{ID: 1}
	if err := c.OpenObserver(observer); err != nil {
		t.Fatal(err)
	}
	if err := c.PublishSnapshot(observer, 1); err != nil {
		t.Fatal(err)
	}
	p.sequences = nil
	if err := history.RotateEpoch(2); err != nil {
		t.Fatal(err)
	}
	if err := c.RetryPending(time.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if len(p.sequences) != 0 || c.outbox.Metrics().Pending != 0 {
		t.Fatalf("old epoch still pending: published=%v pending=%d", p.sequences, c.outbox.Metrics().Pending)
	}
	if err := c.OpenObserver(observer); err != nil {
		t.Fatal(err)
	}
	if err := c.PublishSnapshot(observer, 1); err != nil {
		t.Fatal(err)
	}
	status := history.Status(observer, syncstream.Stream{Topic: TopicState, Key: 1})
	if err := c.Acknowledge(observer, syncstream.Stream{Topic: TopicState, Key: 1}, 2, status.LatestSequence); err != nil {
		t.Fatal(err)
	}
}
