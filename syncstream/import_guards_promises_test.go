package syncstream

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// U-0127 · C2（空洞测试）· U-0126 全包采样余下 22 条无覆盖。
//
// Import 是快照进入 History 的唯一入口，它的十几条自洽校验（版本、epoch、流有
// topic、不重复、序号元数据、包身份、首包基线、schema 跃迁、full 包无基线、latest
// 相符、schema 必填、流数 / 载荷上限）此前只有"断链"一条被测过；每一条都要以
// 对应哨兵拒绝，并且被拒的 Import 不能改动 History 的现状。同包其余入口守卫
// （Save / Restore 无存储、Append 无 topic、异 epoch 确认、需要全量却没有提供者、
// BufferedPublisher 的 nil / 关闭、适配器解码拖尾内容）一并钉住。

// importBaseline exports a valid two-packet snapshot: seq 1 full, seq 2 delta,
// acknowledged through 1, schema 2.
func importBaseline(t *testing.T) HistorySnapshot {
	t.Helper()
	source := NewHistory(HistoryOptions{MaxPacketsPerStream: 4, SchemaVersion: 2})
	observer, stream := Observer{ID: 5}, Stream{Topic: "state", Key: 3}
	if _, err := source.Append(Packet{Observer: observer, Stream: stream, Full: true, Payload: []byte("full")}); err != nil {
		t.Fatal(err)
	}
	if _, err := source.Append(Packet{Observer: observer, Stream: stream, Payload: []byte("delta")}); err != nil {
		t.Fatal(err)
	}
	if err := source.Acknowledge(observer, stream, 1); err != nil {
		t.Fatal(err)
	}
	return source.Export()
}

func TestImportRefusesEveryInconsistentSnapshotAndLeavesTheHistoryUntouched(t *testing.T) {
	cases := []struct {
		name    string
		options HistoryOptions
		mutate  func(*HistorySnapshot)
		want    error
		text    string
	}{
		{"unsupported version", HistoryOptions{}, func(s *HistorySnapshot) { s.Version++ }, ErrInvalidSnapshot, "unsupported version"},
		{"epoch missing", HistoryOptions{}, func(s *HistorySnapshot) { s.Epoch = 0 }, ErrInvalidSnapshot, "epoch is required"},
		{"more streams than MaxStreams", HistoryOptions{MaxStreams: 1}, func(s *HistorySnapshot) {
			second := s.Streams[0]
			second.Stream.Key = 4
			s.Streams = append(s.Streams, second)
		}, ErrStreamLimit, ""},
		{"stream without topic", HistoryOptions{}, func(s *HistorySnapshot) { s.Streams[0].Stream.Topic = "" }, ErrInvalidSnapshot, "has no topic"},
		{"duplicate stream", HistoryOptions{}, func(s *HistorySnapshot) { s.Streams = append(s.Streams, s.Streams[0]) }, ErrInvalidSnapshot, "duplicate stream"},
		{"acked past latest", HistoryOptions{}, func(s *HistorySnapshot) { s.Streams[0].Acked = 3 }, ErrInvalidSnapshot, "inconsistent sequence metadata"},
		{"latest without packets and not fully acked", HistoryOptions{}, func(s *HistorySnapshot) { s.Streams[0].Packets = nil }, ErrInvalidSnapshot, "inconsistent sequence metadata"},
		{"packet from another observer", HistoryOptions{}, func(s *HistorySnapshot) { s.Streams[0].Packets[0].Observer.ID = 6 }, ErrInvalidSnapshot, "packet identity mismatch"},
		{"packet from another epoch", HistoryOptions{}, func(s *HistorySnapshot) { s.Streams[0].Packets[0].Epoch++ }, ErrInvalidSnapshot, "packet identity mismatch"},
		{"packet without schema", HistoryOptions{}, func(s *HistorySnapshot) { s.Streams[0].Packets[1].SchemaVersion = 0 }, ErrInvalidSnapshot, "packet identity mismatch"},
		{"packet payload over MaxPayloadBytes", HistoryOptions{MaxPayloadBytes: 4}, func(*HistorySnapshot) {}, ErrPayloadTooLarge, ""},
		{"first delta with a wrong base", HistoryOptions{}, func(s *HistorySnapshot) {
			s.Streams[0].Packets[0].Full = false
			s.Streams[0].Packets[0].BaseSequence = 5
		}, ErrInvalidSnapshot, "invalid first packet base"},
		{"schema transition without a full packet", HistoryOptions{}, func(s *HistorySnapshot) { s.Streams[0].Packets[1].SchemaVersion = 3 }, ErrInvalidSnapshot, "schema transition without full packet"},
		{"full packet carrying a base", HistoryOptions{}, func(s *HistorySnapshot) { s.Streams[0].Packets[0].BaseSequence = 1 }, ErrInvalidSnapshot, "full packet has a base"},
		{"latest disagrees with the packets", HistoryOptions{}, func(s *HistorySnapshot) { s.Streams[0].Latest = 3 }, ErrInvalidSnapshot, "latest sequence mismatch"},
		{"stream without schema", HistoryOptions{}, func(s *HistorySnapshot) {
			s.Streams[0].Packets, s.Streams[0].Acked, s.Streams[0].Schema = nil, s.Streams[0].Latest, 0
		}, ErrInvalidSnapshot, "stream schema is required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			target := NewHistory(tc.options)
			if tc.options.MaxPayloadBytes == 0 {
				if err := target.Import(importBaseline(t)); err != nil {
					t.Fatalf("baseline import: %v", err)
				}
			}
			before := target.Export()
			snapshot := importBaseline(t)
			tc.mutate(&snapshot)
			err := target.Import(snapshot)
			if !errors.Is(err, tc.want) || !strings.Contains(err.Error(), tc.text) {
				t.Fatalf("Import = %v, want %v containing %q", err, tc.want, tc.text)
			}
			if after := target.Export(); !reflect.DeepEqual(before, after) {
				t.Fatalf("a refused import changed the history:\nbefore %+v\nafter  %+v", before, after)
			}
		})
	}
}

type stubPacketPublisher struct{ published int }

func (publisher *stubPacketPublisher) Publish(Packet) error {
	publisher.published++
	return nil
}

func TestHistoryEntryPointsRefuseMissingOrMismatchedInputs(t *testing.T) {
	history := NewHistory(HistoryOptions{})
	if err := history.Save(nil); !errors.Is(err, ErrHistoryStoreRequired) {
		t.Fatalf("Save(nil) = %v", err)
	}
	if err := history.Restore(nil); !errors.Is(err, ErrHistoryStoreRequired) {
		t.Fatalf("Restore(nil) = %v", err)
	}
	store := &memoryHistoryStore{}
	if err := history.Save(store); err != nil || store.snapshot.Epoch != history.Epoch() {
		t.Fatalf("Save into a real store: err=%v epoch=%d", err, store.snapshot.Epoch)
	}
	if _, err := history.Append(Packet{Observer: Observer{ID: 1}, Payload: []byte("x")}); !errors.Is(err, ErrTopicRequired) {
		t.Fatalf("Append without a topic = %v", err)
	}
	if history.Metrics().Streams != 0 {
		t.Fatal("a refused append created a stream")
	}
	observer, stream := Observer{ID: 1}, Stream{Topic: "state"}
	if _, err := history.Append(Packet{Observer: observer, Stream: stream, Full: true, Payload: []byte("x")}); err != nil {
		t.Fatal(err)
	}
	if err := history.AcknowledgeEpoch(observer, stream, history.Epoch()+1, 1); !errors.Is(err, ErrAckEpochMismatch) {
		t.Fatalf("AcknowledgeEpoch from another epoch = %v", err)
	}
	if status := history.Status(observer, stream); status.AckedSequence != 0 {
		t.Fatalf("an acknowledgement from another epoch was applied: %+v", status)
	}
	result, err := history.Recover(ResyncRequest{Observer: Observer{ID: 9}, Stream: stream}, nil)
	if !errors.Is(err, ErrSnapshotProviderRequired) || !result.FullRequired {
		t.Fatalf("Recover needing a full snapshot without a provider = (%+v, %v)", result, err)
	}
}

func TestBufferedPublisherRefusesWithoutAPublisherAndAfterClose(t *testing.T) {
	if _, err := NewBufferedPublisher(nil, BufferedPublisherOptions{}); !errors.Is(err, ErrPublisherRequired) {
		t.Fatalf("NewBufferedPublisher(nil) = %v", err)
	}
	var none *BufferedPublisher
	if err := none.Publish(Packet{}); !errors.Is(err, ErrPublisherRequired) {
		t.Fatalf("Publish on a nil buffered publisher = %v", err)
	}
	if err := none.TryEnqueue(Packet{}); !errors.Is(err, ErrPublisherRequired) {
		t.Fatalf("TryEnqueue on a nil buffered publisher = %v", err)
	}
	target := &stubPacketPublisher{}
	buffered, err := NewBufferedPublisher(target, BufferedPublisherOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := buffered.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := buffered.Publish(Packet{}); !errors.Is(err, ErrPublisherClosed) {
		t.Fatalf("Publish after Close = %v", err)
	}
	if err := buffered.TryEnqueue(Packet{}); !errors.Is(err, ErrPublisherClosed) {
		t.Fatalf("TryEnqueue after Close = %v", err)
	}
	if target.published != 0 {
		t.Fatalf("a closed buffered publisher still forwarded %d packets", target.published)
	}
}

func TestSubscriberRefusesAnEnvelopeWithTrailingContent(t *testing.T) {
	bus := &memoryBus{}
	delivered := 0
	if _, err := SubscribeWithOptions(bus, "state", SubscribeOptions{}, func(Packet) error { delivered++; return nil }); err != nil {
		t.Fatal(err)
	}
	msg := mintEnvelope(t, []byte("payload"))
	msg.Data = append(msg.Data, []byte(`{"Sequence":2}`)...)
	if msg.Checksum != "" {
		sum := sha256.Sum256(msg.Data)
		msg.Checksum = hex.EncodeToString(sum[:])
	}
	err := bus.Publish(msg)
	if err == nil || !strings.Contains(err.Error(), "trailing content") {
		t.Fatalf("envelope with a second JSON value = %v", err)
	}
	if delivered != 0 {
		t.Fatal("a packet with trailing content reached the handler")
	}
}
