package syncstream

import (
	"os"
	"testing"
)

// U-0211 · C5 · RR-20260915-07:恢复时忽略的半条 WAL 尾部必须被截掉,不能留在磁盘上让后续追加
// 拼在它后面。旧实现 replayWAL 对未换行尾部直接成功,flushBatch 用 O_APPEND 续写:半条 JSON +
// 新完整记录同处一行,第二次启动 `decode WAL: invalid character '{' after object key:value pair`,
// 整个 History 起不来。承诺:Load 记下最后一条完整记录的偏移,首次续写前在独占所有权下截断并
// fsync;完整行的非法 JSON 仍然拒绝,不把任意损坏都当半尾。
func TestJournalPromiseTruncatesAnUnterminatedTailBeforeAppending(t *testing.T) {
	for _, mode := range []string{"clean", "partial_tail", "partial_then_checkpoint"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			j, err := NewFileHistoryJournal(dir, 7)
			if err != nil {
				t.Fatal(err)
			}
			h, err := NewHistoryWithJournal(HistoryOptions{}, j)
			if err != nil {
				t.Fatal(err)
			}
			s := Stream{Topic: "state", Key: 1}
			if _, err := h.Append(Packet{Stream: s, Full: true}); err != nil {
				t.Fatal(err)
			}
			path := j.walPath(j.generation)
			if err := j.Close(); err != nil {
				t.Fatal(err)
			}
			if mode != "clean" {
				f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := f.WriteString(`{"Version":1,"Kind":"append"`); err != nil {
					t.Fatal(err)
				}
				f.Close()
			}
			j2, err := NewFileHistoryJournal(dir, 99)
			if err != nil {
				t.Fatal(err)
			}
			h2, err := NewHistoryWithJournal(HistoryOptions{}, j2)
			if err != nil {
				t.Fatal("first recovery", err)
			}
			if mode == "partial_then_checkpoint" {
				if err := h2.Checkpoint(); err != nil {
					t.Fatal(err)
				}
			}
			p, err := h2.Append(Packet{Stream: s, Payload: []byte("committed-after-recovery")})
			if err != nil {
				t.Fatal("append", err)
			}
			j2.Close()
			j3, err := NewFileHistoryJournal(dir, 99)
			if err != nil {
				t.Fatal(err)
			}
			defer j3.Close()
			h3, err := NewHistoryWithJournal(HistoryOptions{}, j3)
			if err != nil {
				t.Fatalf("acknowledged sequence %d cannot recover: %v", p.Sequence, err)
			}
			if h3.Status(Observer{}, s).LatestSequence != 2 {
				t.Fatal("lost append")
			}
		})
	}
}

// 完整一行的非法 JSON 不是半尾:仍然拒绝加载。
func TestJournalPromiseStillRejectsACorruptCompleteLine(t *testing.T) {
	dir := t.TempDir()
	j, err := NewFileHistoryJournal(dir, 7)
	if err != nil {
		t.Fatal(err)
	}
	h, err := NewHistoryWithJournal(HistoryOptions{}, j)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.Append(Packet{Stream: Stream{Topic: "state"}, Full: true}); err != nil {
		t.Fatal(err)
	}
	path := j.walPath(j.generation)
	j.Close()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("{not json}\n")
	f.Close()
	j2, err := NewFileHistoryJournal(dir, 99)
	if err != nil {
		t.Fatal(err)
	}
	defer j2.Close()
	if _, err := NewHistoryWithJournal(HistoryOptions{}, j2); err == nil {
		t.Fatal("a corrupt complete line must still fail recovery")
	}
}
