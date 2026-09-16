package syncstream

import (
	"errors"
	"testing"
	"time"
)

// U-0215 · C8 · RR-20260915-06:同一 observer/stream 身份在一个 epoch 内被清理后重建,序号不能
// 从 1 重来。DeleteStream / DeleteObserver / SweepIdle 删掉全部序号状态却保留全局 epoch:新快照
// 又是 epoch 7 / sequence 1,旧连接的 Resync(AfterSequence=1) 得到"无需发送"、旧 ACK(7,1) 返回
// nil;PruneAcknowledged 时新快照被裁掉。承诺:新建(含重建)的流从本 epoch 内已分配过的最大序号
// 之后开始——不需要按身份保留任何东西,一个计数器即可,RotateEpoch 时归零(那是显式的全局失效)。

func TestHistoryPromiseRecreatedStreamDoesNotReuseSequences(t *testing.T) {
	for _, mode := range []string{"delete_stream", "delete_observer", "sweep_idle", "rotate_epoch"} {
		t.Run(mode, func(t *testing.T) {
			h := NewHistory(HistoryOptions{Epoch: 7, IdleTTL: time.Second, PruneAcknowledged: true})
			o := Observer{ID: 1}
			s := Stream{Topic: "state", Key: 1}
			old, err := h.Append(Packet{Observer: o, Stream: s, Full: true, Payload: []byte("old")})
			if err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "delete_stream":
				err = h.DeleteStream(o, s)
			case "delete_observer":
				_, err = h.DeleteObserver(o)
			case "sweep_idle":
				_, err = h.SweepIdle(time.Now().Add(2 * time.Second))
			case "rotate_epoch":
				err = h.RotateEpoch(8)
			}
			if err != nil {
				t.Fatal(err)
			}
			fresh, err := h.Append(Packet{Observer: o, Stream: s, Full: true, Payload: []byte("new")})
			if err != nil {
				t.Fatal(err)
			}
			replay := h.Resync(ResyncRequest{Observer: o, Stream: s, Epoch: old.Epoch, AfterSequence: old.Sequence})
			ack := h.AcknowledgeEpoch(o, s, old.Epoch, old.Sequence)
			if !replay.FullRequired && len(replay.Packets) == 0 && ack == nil && h.Status(o, s).Retained == 0 {
				t.Fatalf("old identity silently swallowed new packet: old=%+v fresh=%+v replay=%+v status=%+v", old, fresh, replay, h.Status(o, s))
			}
			if mode == "rotate_epoch" {
				if !errors.Is(ack, ErrAckEpochMismatch) {
					t.Fatal(ack)
				}
				return
			}
			if fresh.Sequence <= old.Sequence {
				t.Fatalf("recreated stream reused sequence %d (old %d)", fresh.Sequence, old.Sequence)
			}
			if h.Status(o, s).Retained != 1 {
				t.Fatalf("new snapshot pruned by an ACK meant for the old one: %+v", h.Status(o, s))
			}
		})
	}
}

// 重建后的流经 journal 重放也要能恢复:第一条记录的序号不再是 1。
func TestHistoryPromiseRecreatedStreamSurvivesJournalReplay(t *testing.T) {
	dir := t.TempDir()
	j, err := NewFileHistoryJournal(dir, 7)
	if err != nil {
		t.Fatal(err)
	}
	h, err := NewHistoryWithJournal(HistoryOptions{}, j)
	if err != nil {
		t.Fatal(err)
	}
	o, s := Observer{ID: 1}, Stream{Topic: "state"}
	if _, err := h.Append(Packet{Observer: o, Stream: s, Full: true, Payload: []byte("old")}); err != nil {
		t.Fatal(err)
	}
	if err := h.DeleteStream(o, s); err != nil {
		t.Fatal(err)
	}
	fresh, err := h.Append(Packet{Observer: o, Stream: s, Full: true, Payload: []byte("new")})
	if err != nil {
		t.Fatal(err)
	}
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	j2, err := NewFileHistoryJournal(dir, 99)
	if err != nil {
		t.Fatal(err)
	}
	defer j2.Close()
	h2, err := NewHistoryWithJournal(HistoryOptions{}, j2)
	if err != nil {
		t.Fatalf("recreated stream cannot be replayed: %v", err)
	}
	if got := h2.Status(o, s).LatestSequence; got != fresh.Sequence {
		t.Fatalf("replayed latest=%d want %d", got, fresh.Sequence)
	}
}
