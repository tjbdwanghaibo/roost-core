package skillsync

import (
	"github.com/tjbdwanghaibo/roost-core/framework/sync/syncstream"
	"testing"
)

// 旧式 store 只能存包，无法保存 CreatedAt。不能把重启时间伪装成包创建时间。
type packetOnlyStore struct{ packet syncstream.Packet }

func (s *packetOnlyStore) Load() ([]syncstream.Packet, error) {
	return []syncstream.Packet{s.packet}, nil
}
func (*packetOnlyStore) Put(syncstream.Packet) error { return nil }
func (*packetOnlyStore) Delete(syncstream.Observer, syncstream.Stream, uint64, uint64) error {
	return nil
}

func TestOutboxRefusesStoreWithoutPersistentAge(t *testing.T) {
	packet := syncstream.Packet{Observer: syncstream.Observer{ID: 1}, Stream: syncstream.Stream{Topic: TopicState}, Epoch: 1, Sequence: 1}
	if _, err := NewOutbox(OutboxOptions{Store: &packetOnlyStore{packet: packet}, RequireDurable: true}); err == nil {
		t.Error("packet-only store accepted: each restart resets packet age")
	}
	store := &failingUpdateStore{writes: 1, record: OutboxRecord{Packet: packet}}
	if _, err := NewOutbox(OutboxOptions{Store: store}); err == nil {
		t.Error("record without CreatedAt accepted: restart invents a new age")
	}
}
