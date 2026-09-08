package synce2e

import (
	"os"
	"testing"
	"time"

	corestream "github.com/tjbdwanghaibo/roost-core/syncstream"
	streamadapter "github.com/tjbdwanghaibo/roost-kit/syncstream"
	"github.com/tjbdwanghaibo/roost-skill/skill"
	"github.com/tjbdwanghaibo/roost-skill/skillsync"
)

// TestProtocolSoak is opt-in so normal CI remains fast. Production release CI
// should run it with CUBE_SYNC_SOAK=1 (30 minutes by default).
func TestProtocolSoak(t *testing.T) {
	if os.Getenv("ROOST_SYNC_SOAK") != "1" {
		t.Skip("set CUBE_SYNC_SOAK=1 to run the protocol soak")
	}
	duration := 30 * time.Minute
	if value := os.Getenv("ROOST_SYNC_SOAK_DURATION"); value != "" {
		parsed, err := time.ParseDuration(value)
		if err != nil {
			t.Fatal(err)
		}
		duration = parsed
	}

	observer := corestream.Observer{ID: 88, Scope: "soak"}
	stream := corestream.Stream{Topic: skillsync.TopicState, Key: 1}
	bus := &confirmedBus{}
	consumer := &stateConsumer{}
	applier, _ := skillsync.NewApplier(skillsync.ApplierOptions{Observer: observer, SchemaVersion: 1, State: consumer})
	_, err := streamadapter.SubscribeWithOptions(bus, stream.Topic, streamadapter.SubscribeOptions{ExpectedObserver: &observer, RequireChecksum: true}, func(packet corestream.Packet) error {
		_, applyErr := applier.Apply(packet)
		return applyErr
	})
	if err != nil {
		t.Fatal(err)
	}
	publisher, _ := streamadapter.NewPublisherWithOptions(bus, streamadapter.PublisherOptions{CompressionThreshold: 1, MaxFrameBytes: 64, RequireConfirmation: true})
	projector, _ := skillsync.NewProjector(1)
	history := corestream.NewHistory(corestream.HistoryOptions{Epoch: 77, PruneAcknowledged: true})
	deadline := time.Now().Add(duration)
	iterations := 0
	for time.Now().Before(deadline) {
		packet, projectErr := projector.StateSnapshotPacket(observer, stream.Key, skill.RuntimeStateSnapshot{Tick: skill.Tick(iterations)})
		if projectErr != nil {
			t.Fatal(projectErr)
		}
		packet, err = history.Append(packet)
		if err == nil {
			err = publisher.Publish(packet)
		}
		if err == nil {
			err = history.AcknowledgeEpoch(observer, stream, packet.Epoch, packet.Sequence)
		}
		if err != nil {
			t.Fatalf("iteration %d: %v", iterations, err)
		}
		iterations++
	}
	if iterations == 0 || history.Status(observer, stream).Retained != 0 {
		t.Fatalf("iterations=%d status=%#v", iterations, history.Status(observer, stream))
	}
}
