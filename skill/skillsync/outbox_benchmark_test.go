package skillsync

import (
	"testing"

	"github.com/tjbdwanghaibo/roost-core/syncstream"
)

func BenchmarkOutboxPutSameStream(b *testing.B) {
	const existing = 10000
	box, err := NewOutbox(OutboxOptions{MaxPendingPackets: existing + 2, MaxPendingPerStream: existing + 2, MaxPendingBytes: 1 << 60})
	if err != nil {
		b.Fatal(err)
	}
	for index := 0; index < existing; index++ {
		packet := syncstream.Packet{Observer: syncstream.Observer{ID: 1}, Stream: syncstream.Stream{Topic: TopicState, Key: 2}, Epoch: 1, Sequence: uint64(index + 1)}
		if err := box.Put(packet); err != nil {
			b.Fatal(err)
		}
	}
	packet := syncstream.Packet{Observer: syncstream.Observer{ID: 1}, Stream: syncstream.Stream{Topic: TopicState, Key: 1}, Epoch: 1, Sequence: 1}
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		if err := box.Put(packet); err != nil {
			b.Fatal(err)
		}
		box.mutex.Lock()
		box.removePendingLocked(packetID(packet), false)
		box.mutex.Unlock()
	}
}
