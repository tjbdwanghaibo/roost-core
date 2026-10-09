package robot

import (
	"slices"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/framework/sync/lockstep"
)

// RR-20261006-65 (F04-9): the bot asked for a catch-up only when the
// assembler's out-of-order buffer overflowed (256 frames by default). A loss
// longer than the redundancy depth but shorter than that bound left a gap
// nobody asked to fill: Next stopped for good and the match stalled. And a
// catch-up the server abandoned was never asked for again — the dedup on
// "same Next" swallowed every later request.

type gapSink struct{ catchups []lockstep.FrameID }

func (s *gapSink) SubmitInput(lockstep.FrameID, []byte) error { return nil }
func (s *gapSink) ReportHash(lockstep.FrameID, uint64) error  { return nil }
func (s *gapSink) RequestCatchup(from lockstep.FrameID) error {
	s.catchups = append(s.catchups, from)
	return nil
}

// broadcastPackets encodes frames 1..count the way the Room does: each
// packet carries its frame plus depth-1 predecessors.
func broadcastPackets(count int, depth int) [][]byte {
	encoder := lockstep.NewRedundantEncoder(depth)
	packets := make([][]byte, 0, count)
	for id := 1; id <= count; id++ {
		packets = append(packets, encoder.Push(lockstep.Frame{ID: lockstep.FrameID(id)}))
	}
	return packets
}

func TestLockstepBotPromiseRequestsCatchupOnAnyUnhealedGap(t *testing.T) {
	sink := &gapSink{}
	bot, err := NewLockstepBot(LockstepBotConfig{Player: 1, Sink: sink, KeyframeInterval: 1000})
	if err != nil {
		t.Fatal(err)
	}
	packets := broadcastPackets(40, 3)
	for index, packet := range packets {
		id := index + 1
		if id >= 4 && id <= 8 { // 5 lost packets: frames 4..6 are beyond redundancy
			continue
		}
		if err := bot.HandleBroadcast(packet); err != nil {
			t.Fatal(err)
		}
	}
	if bot.Next() != 4 {
		t.Fatalf("Next = %d, want 4 (frames 4..6 lost)", bot.Next())
	}
	if !slices.Equal(sink.catchups, []lockstep.FrameID{4}) {
		t.Fatalf("catch-up requests = %v, want exactly [4] once the gap is seen", sink.catchups)
	}
	// The server serves the gap: the bot runs to the head.
	if err := bot.HandleFrames([]lockstep.Frame{{ID: 4}, {ID: 5}, {ID: 6}}); err != nil {
		t.Fatal(err)
	}
	if bot.Next() != 41 {
		t.Fatalf("Next after catch-up = %d, want 41", bot.Next())
	}
}

func TestLockstepBotPromiseReRequestsAnAbandonedCatchup(t *testing.T) {
	sink := &gapSink{}
	bot, err := NewLockstepBot(LockstepBotConfig{Player: 1, Sink: sink, KeyframeInterval: 1000})
	if err != nil {
		t.Fatal(err)
	}
	packets := broadcastPackets(200, 3)
	for index, packet := range packets {
		id := index + 1
		if id >= 4 && id <= 8 {
			continue
		}
		if err := bot.HandleBroadcast(packet); err != nil {
			t.Fatal(err)
		}
	}
	// The server never answered (abandoned the catch-up): ~190 packets later
	// the gap is still open and the bot must have asked again.
	if len(sink.catchups) < 2 {
		t.Fatalf("catch-up requests = %v after ~190 packets with the gap still open; an abandoned catch-up is never retried", sink.catchups)
	}
	for _, from := range sink.catchups {
		if from != 4 {
			t.Fatalf("catch-up requested from %d, want 4", from)
		}
	}
}
