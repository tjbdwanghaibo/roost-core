package syncstream

import (
	"errors"
	"testing"

	coresyncbus "github.com/tjbdwanghaibo/roost-core/syncbus"
	corestream "github.com/tjbdwanghaibo/roost-core/syncstream"
)

// mintEnvelope publishes one packet through a real Publisher and returns the
// SyncMsg that went on the wire, so tests mutate a genuine envelope instead
// of hand-assembling one.
func mintEnvelope(t *testing.T, payload []byte) *coresyncbus.SyncMsg {
	t.Helper()
	captured := &capturingPublisher{}
	publisher, err := NewPublisher(captured, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := publisher.Publish(corestream.Packet{Stream: corestream.Stream{Topic: "state", Key: 1}, Epoch: 1, Sequence: 1, Full: true, Payload: payload}); err != nil {
		t.Fatal(err)
	}
	if captured.last == nil {
		t.Fatal("publisher produced no envelope")
	}
	return captured.last
}

// The subscriber is the trust boundary for everything that arrives on the
// sync bus. Each limit and each fragment rule is pinned with a real envelope
// that is valid except for the rule under test.
func TestSubscribeRefusesEachOversizedOrMalformedEnvelope(t *testing.T) {
	deliver := func(t *testing.T, options SubscribeOptions, mutate func(*coresyncbus.SyncMsg)) error {
		t.Helper()
		bus := &memoryBus{}
		if _, err := SubscribeWithOptions(bus, "state", options, func(corestream.Packet) error { return nil }); err != nil {
			t.Fatal(err)
		}
		msg := mintEnvelope(t, []byte("payload-0123456789"))
		mutate(msg)
		return bus.Publish(msg)
	}
	cases := []struct {
		name    string
		options SubscribeOptions
		mutate  func(*coresyncbus.SyncMsg)
		want    error
	}{
		{"envelope over MaxEnvelopeBytes", SubscribeOptions{MaxEnvelopeBytes: 8}, func(*coresyncbus.SyncMsg) {}, ErrPayloadTooLarge},
		{"more parts than MaxChunks", SubscribeOptions{MaxChunks: 2}, func(m *coresyncbus.SyncMsg) { m.Parts = 3 }, ErrFragmentInvalid},
		{"part index beyond parts", SubscribeOptions{}, func(m *coresyncbus.SyncMsg) { m.Parts = 2; m.Part = 2 }, ErrFragmentInvalid},
		{"checksum required but absent", SubscribeOptions{RequireChecksum: true}, func(m *coresyncbus.SyncMsg) { m.Checksum = "" }, ErrChecksumMismatch},
		{"checksum lies", SubscribeOptions{}, func(m *coresyncbus.SyncMsg) { m.Checksum = "00" }, ErrChecksumMismatch},
		{"decoded over MaxDecodedBytes", SubscribeOptions{MaxDecodedBytes: 16}, func(*coresyncbus.SyncMsg) {}, ErrPayloadTooLarge},
		{"packet payload over MaxPayloadBytes", SubscribeOptions{MaxPayloadBytes: 4}, func(*coresyncbus.SyncMsg) {}, ErrPayloadTooLarge},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := deliver(t, tc.options, tc.mutate); !errors.Is(err, tc.want) {
				t.Fatalf("subscriber returned %v, want %v", err, tc.want)
			}
		})
	}
	if err := deliver(t, SubscribeOptions{}, func(*coresyncbus.SyncMsg) {}); err != nil {
		t.Fatalf("the untouched envelope must be accepted: %v", err)
	}
}
