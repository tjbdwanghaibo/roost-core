package synce2e

import (
	"errors"
	"sync"
	"testing"
	"time"

	coresyncbus "github.com/tjbdwanghaibo/roost-core/syncbus"
	corestream "github.com/tjbdwanghaibo/roost-core/syncstream"
	streamadapter "github.com/tjbdwanghaibo/roost-kit/syncstream"
	"github.com/tjbdwanghaibo/roost-skill/skill"
	"github.com/tjbdwanghaibo/roost-skill/skillsync"
)

type confirmedBus struct {
	mutex    sync.Mutex
	handlers map[string][]coresyncbus.Handler
	failNext bool
	frames   int
}

func (bus *confirmedBus) Publish(message *coresyncbus.SyncMsg) error { return bus.publish(message) }
func (bus *confirmedBus) PublishConfirmed(message *coresyncbus.SyncMsg) error {
	return bus.publish(message)
}
func (bus *confirmedBus) publish(message *coresyncbus.SyncMsg) error {
	bus.mutex.Lock()
	if bus.failNext {
		bus.failNext = false
		bus.mutex.Unlock()
		return errors.New("injected broker confirmation failure")
	}
	bus.frames++
	handlers := append([]coresyncbus.Handler(nil), bus.handlers[message.Topic]...)
	bus.mutex.Unlock()
	for _, handler := range handlers {
		copy := *message
		copy.Data = append([]byte(nil), message.Data...)
		if err := handler(&copy); err != nil {
			return err
		}
	}
	return nil
}
func (bus *confirmedBus) Subscribe(topic string, handler coresyncbus.Handler) (func(), error) {
	bus.mutex.Lock()
	if bus.handlers == nil {
		bus.handlers = make(map[string][]coresyncbus.Handler)
	}
	bus.handlers[topic] = append(bus.handlers[topic], handler)
	bus.mutex.Unlock()
	return func() {}, nil
}

type stateConsumer struct{ snapshots int }

func (consumer *stateConsumer) ApplyStateSnapshot(int64, skill.RuntimeStateSnapshot) error {
	consumer.snapshots++
	return nil
}
func (*stateConsumer) ApplyStateDelta(int64, skill.StateMutation) error { return nil }

func TestCrashRecoveryThroughConfirmedFragmentedTransport(t *testing.T) {
	observer := corestream.Observer{ID: 71, Scope: "match-9"}
	stream := corestream.Stream{Topic: skillsync.TopicState, Key: 44}
	bus := &confirmedBus{failNext: true}
	consumer := &stateConsumer{}
	applier, err := skillsync.NewApplier(skillsync.ApplierOptions{Observer: observer, SchemaVersion: 1, State: consumer})
	if err != nil {
		t.Fatal(err)
	}
	_, err = streamadapter.SubscribeWithOptions(bus, stream.Topic, streamadapter.SubscribeOptions{ExpectedObserver: &observer, MaxEnvelopeBytes: 96, MaxAssemblyBytes: 1 << 20, MaxDecodedBytes: 1 << 20, MaxChunks: 256, RequireChecksum: true}, func(packet corestream.Packet) error {
		_, applyErr := applier.Apply(packet)
		return applyErr
	})
	if err != nil {
		t.Fatal(err)
	}
	publisher, err := streamadapter.NewPublisherWithOptions(bus, streamadapter.PublisherOptions{ExpectedObserver: &observer, CompressionThreshold: 1, MaxFrameBytes: 64, RequireConfirmation: true})
	if err != nil {
		t.Fatal(err)
	}

	historyDirectory, outboxDirectory := t.TempDir(), t.TempDir()
	journal, err := corestream.NewFileHistoryJournal(historyDirectory, 9001)
	if err != nil {
		t.Fatal(err)
	}
	history, err := corestream.NewHistoryWithJournal(corestream.HistoryOptions{SchemaVersion: 1, PruneAcknowledged: true}, journal)
	if err != nil {
		t.Fatal(err)
	}
	store, _ := skillsync.NewFileOutboxStore(outboxDirectory)
	outbox, err := skillsync.NewOutbox(skillsync.OutboxOptions{Store: store, RequireDurable: true, FailureRetryDelay: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	projector, _ := skillsync.NewProjector(1)
	runtime := skill.NewRuntime(skill.NewMemoryHost(skill.AuthorityIdentity{}), skill.RuntimeOptions{})
	coordinator, err := skillsync.NewCoordinator(skillsync.CoordinatorOptions{Runtime: runtime, History: history, Publisher: publisher, Projector: projector, Visibility: skillsync.AllowAllVisibility{}, Outbox: outbox, RequireDurableOutbox: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := coordinator.PublishSnapshot(observer, stream.Key); err == nil {
		t.Fatal("expected injected broker failure")
	}
	if outbox.Metrics().Pending != 1 || consumer.snapshots != 0 {
		t.Fatalf("before restart pending=%d snapshots=%d", outbox.Metrics().Pending, consumer.snapshots)
	}
	// roost-core v1.8.0 does not expose a journal Close method. A checkpoint is
	// the supported durable boundary and also releases the resident WAL handle,
	// which makes the restart lifecycle portable to Windows.
	if err := history.Checkpoint(); err != nil {
		t.Fatal(err)
	}

	// Reconstruct all reliable-send state from disk, as a new process would.
	reopenedJournal, err := corestream.NewFileHistoryJournal(historyDirectory, 1)
	if err != nil {
		t.Fatal(err)
	}
	reopenedHistory, err := corestream.NewHistoryWithJournal(corestream.HistoryOptions{SchemaVersion: 1, PruneAcknowledged: true}, reopenedJournal)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := reopenedHistory.Checkpoint(); err != nil {
			t.Errorf("checkpoint reopened history: %v", err)
		}
	})
	reopenedStore, _ := skillsync.NewFileOutboxStore(outboxDirectory)
	reopenedOutbox, err := skillsync.NewOutbox(skillsync.OutboxOptions{Store: reopenedStore, RequireDurable: true, FailureRetryDelay: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	restarted, err := skillsync.NewCoordinator(skillsync.CoordinatorOptions{Runtime: runtime, History: reopenedHistory, Publisher: publisher, Projector: projector, Visibility: skillsync.AllowAllVisibility{}, Outbox: reopenedOutbox, RequireDurableOutbox: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.RetryPending(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if consumer.snapshots != 1 || applier.Sequence(stream) != 1 || bus.frames < 2 {
		t.Fatalf("snapshots=%d sequence=%d frames=%d", consumer.snapshots, applier.Sequence(stream), bus.frames)
	}
	if err := restarted.Acknowledge(observer, stream, reopenedHistory.Epoch(), 1); err != nil {
		t.Fatal(err)
	}
	if reopenedOutbox.Metrics().Pending != 0 || reopenedHistory.Status(observer, stream).Retained != 0 {
		t.Fatalf("after ACK outbox=%#v history=%#v", reopenedOutbox.Metrics(), reopenedHistory.Status(observer, stream))
	}
}
