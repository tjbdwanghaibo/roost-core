package skillsync

import (
	"errors"
	"fmt"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/syncstream"
)

// U-0117 · C8（快慢路径不对称）· classscan 观察 O-4。
//
// Outbox.Put 与 Outbox.PutBatch 对同一批数据包各自实现了一遍准入判断：Put 用
// capacityError 逐包判总量 / 每流上限 / 最老待发年龄，PutBatch 先用
// capacityError 判当前状态，再自己累加总量与每流增量。两条路径今天答案一
// 致，但没有任何测试钉住"一致"——改了其中一处的边界，另一处不会跟着红。
// 这里对同一输入分别走两条路，要求：要么都接受且待发数量 / 字节数相同，
// 要么都以同一个哨兵拒绝。

func batchTestPackets(spec ...[2]int) []syncstream.Packet {
	var out []syncstream.Packet
	for _, item := range spec {
		stream, count := item[0], item[1]
		for seq := 1; seq <= count; seq++ {
			out = append(out, syncstream.Packet{
				Observer: syncstream.Observer{ID: 1},
				Stream:   syncstream.Stream{Topic: TopicState, Key: int64(stream)},
				Epoch:    1, Sequence: uint64(seq), SchemaVersion: 1,
				Payload: []byte(fmt.Sprintf("stream-%d-seq-%d-payload", stream, seq)),
			})
		}
	}
	return out
}

func TestPutAndPutBatchReachTheSameAdmissionVerdict(t *testing.T) {
	wide := OutboxOptions{MaxPendingPackets: 100, MaxPendingBytes: 1 << 20, MaxPendingPerStream: 100}
	duplicated := batchTestPackets([2]int{1, 2})
	duplicated = append(duplicated, duplicated[0]) // same packet twice in one batch
	cases := []struct {
		name    string
		options OutboxOptions
		packets []syncstream.Packet
	}{
		{"within every limit", wide, batchTestPackets([2]int{1, 3}, [2]int{2, 2})},
		{"total packet limit", OutboxOptions{MaxPendingPackets: 3, MaxPendingBytes: 1 << 20, MaxPendingPerStream: 100}, batchTestPackets([2]int{1, 2}, [2]int{2, 2})},
		{"total byte limit", OutboxOptions{MaxPendingPackets: 100, MaxPendingBytes: 64, MaxPendingPerStream: 100}, batchTestPackets([2]int{1, 4})},
		{"per-stream limit", OutboxOptions{MaxPendingPackets: 100, MaxPendingBytes: 1 << 20, MaxPendingPerStream: 2}, batchTestPackets([2]int{1, 3}, [2]int{2, 1})},
		{"duplicates in the batch", wide, duplicated},
		{"exactly at the packet limit", OutboxOptions{MaxPendingPackets: 4, MaxPendingBytes: 1 << 20, MaxPendingPerStream: 100}, batchTestPackets([2]int{1, 2}, [2]int{2, 2})},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sequential, err := NewOutbox(tc.options)
			if err != nil {
				t.Fatal(err)
			}
			var putErr error
			for _, packet := range tc.packets {
				if putErr = sequential.Put(packet); putErr != nil {
					break
				}
			}
			batched, err := NewOutbox(tc.options)
			if err != nil {
				t.Fatal(err)
			}
			batchErr := batched.PutBatch(tc.packets)

			if (putErr == nil) != (batchErr == nil) {
				t.Fatalf("verdicts differ: sequential Put=%v, PutBatch=%v", putErr, batchErr)
			}
			if putErr != nil {
				if !errors.Is(batchErr, putErr) {
					t.Fatalf("refusals differ: sequential Put=%v, PutBatch=%v", putErr, batchErr)
				}
				// 拒绝的批不能留下半批：PutBatch 是全有或全无。
				if batched.Metrics().Pending != 0 {
					t.Fatalf("refused PutBatch left %d packets pending", batched.Metrics().Pending)
				}
				return
			}
			seq, bat := sequential.Metrics(), batched.Metrics()
			if seq.Pending != bat.Pending || seq.PendingBytes != bat.PendingBytes {
				t.Fatalf("accepted state differs: sequential pending=%d bytes=%d, batch pending=%d bytes=%d", seq.Pending, seq.PendingBytes, bat.Pending, bat.PendingBytes)
			}
		})
	}
}
