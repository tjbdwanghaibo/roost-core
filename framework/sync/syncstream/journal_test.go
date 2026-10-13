// 历史 journal：文件完整性缺陷 fail-closed、结果未知即 fail-stop、未终止尾部截断、WAL 重放必须接续 checkpoint。

package syncstream

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
)

// checkpointedJournal returns a closed journal directory holding generation 2
// (one checkpoint) plus one WAL record after it, and the generation number.
func checkpointedJournal(t *testing.T) (string, uint64) {
	t.Helper()
	directory := t.TempDir()
	journal, err := NewFileHistoryJournal(directory, 91)
	if err != nil {
		t.Fatal(err)
	}
	history, err := NewHistoryWithJournal(HistoryOptions{MaxPacketsPerStream: 8}, journal)
	if err != nil {
		t.Fatal(err)
	}
	observer, stream := Observer{ID: 4}, Stream{Topic: "state"}
	if _, err := history.Append(Packet{Observer: observer, Stream: stream, Full: true, Payload: []byte("first")}); err != nil {
		t.Fatal(err)
	}
	if err := history.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	if _, err := history.Append(Packet{Observer: observer, Stream: stream, Payload: []byte("second")}); err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	return directory, journal.generation
}

// The journal is the durable memory of every observer's stream position.
// A checkpoint whose body claims another generation, a generation whose WAL
// is gone, or a WAL line that does not decode must fail closed on Load —
// replaying a wrong or partial history would hand clients a resync they
// cannot reconcile.
func TestFileHistoryJournalLoadFailsClosedOnEachIntegrityDefect(t *testing.T) {
	t.Run("checkpoint claims another generation", func(t *testing.T) {
		directory, generation := checkpointedJournal(t)
		probe := &FileHistoryJournal{directory: directory}
		raw, err := os.ReadFile(probe.checkpointPath(generation))
		if err != nil {
			t.Fatal(err)
		}
		var checkpoint fileCheckpoint
		if err := json.Unmarshal(raw, &checkpoint); err != nil {
			t.Fatal(err)
		}
		checkpoint.Generation++
		forged, _ := json.Marshal(checkpoint)
		if err := os.WriteFile(probe.checkpointPath(generation), forged, 0o600); err != nil {
			t.Fatal(err)
		}
		journal, err := NewFileHistoryJournal(directory, 1)
		if err != nil {
			t.Fatal(err)
		}
		defer journal.Close()
		if _, err := journal.Load(); !errors.Is(err, ErrInvalidSnapshot) {
			t.Fatalf("Load with a forged generation = %v, want ErrInvalidSnapshot", err)
		}
	})
	t.Run("WAL of the newest generation is missing", func(t *testing.T) {
		directory, generation := checkpointedJournal(t)
		probe := &FileHistoryJournal{directory: directory}
		if err := os.Remove(probe.walPath(generation)); err != nil {
			t.Fatal(err)
		}
		journal, err := NewFileHistoryJournal(directory, 1)
		if err != nil {
			t.Fatal(err)
		}
		defer journal.Close()
		_, err = journal.Load()
		if err == nil || !strings.Contains(err.Error(), "WAL generation missing") {
			t.Fatalf("Load without the WAL = %v", err)
		}
	})
	t.Run("WAL line does not decode", func(t *testing.T) {
		directory, generation := checkpointedJournal(t)
		probe := &FileHistoryJournal{directory: directory}
		file, err := os.OpenFile(probe.walPath(generation), os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.WriteString("{not json}\n"); err != nil {
			t.Fatal(err)
		}
		_ = file.Close()
		journal, err := NewFileHistoryJournal(directory, 1)
		if err != nil {
			t.Fatal(err)
		}
		defer journal.Close()
		_, err = journal.Load()
		if err == nil || !strings.Contains(err.Error(), "decode WAL") {
			t.Fatalf("Load with a corrupt WAL line = %v", err)
		}
	})
}

// A closed journal accepts nothing, and a mutation from another wire version
// is refused before it reaches the WAL.
func TestFileHistoryJournalRefusesRecordsAfterCloseAndFromOtherVersions(t *testing.T) {
	journal, err := NewFileHistoryJournal(t.TempDir(), 91)
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.Record(HistoryMutation{Version: HistoryMutationVersion + 1}); !errors.Is(err, ErrInvalidSnapshot) {
		t.Fatalf("Record with a foreign version = %v", err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	if err := journal.Record(HistoryMutation{Version: HistoryMutationVersion}); !errors.Is(err, ErrHistoryJournalClosed) {
		t.Fatalf("Record after Close = %v", err)
	}
	if _, err := journal.Load(); !errors.Is(err, ErrHistoryJournalClosed) {
		t.Fatalf("Load after Close = %v", err)
	}
	if err := journal.Checkpoint(HistorySnapshot{}); !errors.Is(err, ErrHistoryJournalClosed) {
		t.Fatalf("Checkpoint after Close = %v", err)
	}
}

// 写入 / 发布结果不确定之后 journal 不再用落后的内存状态写：Write / Sync / 发布这三步出错即进入 fail-stop（ErrHistoryJournalFailed），
// 拒绝后续 Record / Checkpoint，由重开 journal 从真正落盘的内容恢复；副作用之前的错误（临时文件、下一代 WAL 创建）仍可重试。
// 注入方式：让 syncFile / publish 先真做、再报错——“副作用已完成但调用方收到错误”唯一能稳定构造的形态。
// 历史：RR-20260916-01。

func failStopHistory(t *testing.T, dir string) (*FileHistoryJournal, *History) {
	t.Helper()
	j, err := NewFileHistoryJournal(dir, 7)
	if err != nil {
		t.Fatal(err)
	}
	h, err := NewHistoryWithJournal(HistoryOptions{}, j)
	if err != nil {
		t.Fatal(err)
	}
	return j, h
}

func reopenHistory(t *testing.T, dir string) *History {
	t.Helper()
	j, err := NewFileHistoryJournal(dir, 99)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { j.Close() })
	h, err := NewHistoryWithJournal(HistoryOptions{}, j)
	if err != nil {
		t.Fatalf("restart after an indeterminate outcome must recover: %v", err)
	}
	return h
}

func TestJournalPromiseIndeterminateSyncStopsTheJournal(t *testing.T) {
	dir := t.TempDir()
	j, h := failStopHistory(t, dir)
	s := Stream{Topic: "state"}
	if _, err := h.Append(Packet{Stream: s, Full: true, Payload: []byte("one")}); err != nil {
		t.Fatal(err)
	}
	// 第二条:真 fsync 成功,回复却丢了。
	injected := errors.New("fsync reply lost")
	armed := true
	j.syncFile = func(f *os.File) error {
		if err := f.Sync(); err != nil {
			return err
		}
		if armed {
			armed = false
			return injected
		}
		return nil
	}
	if _, err := h.Append(Packet{Stream: s, Payload: []byte("two")}); !errors.Is(err, ErrHistoryJournalFailed) {
		t.Fatalf("indeterminate sync must fail-stop: %v", err)
	}
	// 直接重试不能再成功:否则 WAL 里出现两条 sequence 2。
	if _, err := h.Append(Packet{Stream: s, Payload: []byte("two-retry")}); !errors.Is(err, ErrHistoryJournalFailed) {
		t.Fatalf("retry after an indeterminate outcome must be refused: %v", err)
	}
	if err := h.Checkpoint(); !errors.Is(err, ErrHistoryJournalFailed) {
		t.Fatalf("checkpoint on a failed journal must be refused: %v", err)
	}
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	// 重开:落盘的是两条(1、2),各出现一次。
	restored := reopenHistory(t, dir)
	if got := restored.Status(Observer{}, s).LatestSequence; got != 2 {
		t.Fatalf("recovered latest=%d want 2 (the synced record is durable exactly once)", got)
	}
}

func TestJournalPromiseIndeterminatePublishStopsTheJournal(t *testing.T) {
	dir := t.TempDir()
	j, h := failStopHistory(t, dir)
	s := Stream{Topic: "state"}
	if _, err := h.Append(Packet{Stream: s, Full: true, Payload: []byte("one")}); err != nil {
		t.Fatal(err)
	}
	// 发布真的完成了(rename 已落),函数却报错。
	j.publish = func(from, to, directory string) error {
		if err := durableReplace(from, to, directory); err != nil {
			return err
		}
		return errors.New("directory sync reply lost")
	}
	if err := h.Checkpoint(); !errors.Is(err, ErrHistoryJournalFailed) {
		t.Fatalf("indeterminate publish must fail-stop: %v", err)
	}
	// 旧实现这里会成功,却写进旧 generation 的 WAL,重启后消失。
	if _, err := h.Append(Packet{Stream: s, Payload: []byte("two")}); !errors.Is(err, ErrHistoryJournalFailed) {
		t.Fatalf("append after an indeterminate publish must be refused: %v", err)
	}
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	restored := reopenHistory(t, dir)
	if got := restored.Status(Observer{}, s).LatestSequence; got != 1 {
		t.Fatalf("recovered latest=%d want 1", got)
	}
}

// 对照:副作用之前的失败(临时文件建不出来)不是不确定结果,修复后可以重试。
func TestJournalPromisePreSideEffectFailureIsRetryable(t *testing.T) {
	dir := t.TempDir()
	j, h := failStopHistory(t, dir)
	defer j.Close()
	s := Stream{Topic: "state"}
	if _, err := h.Append(Packet{Stream: s, Full: true}); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	err := h.Checkpoint()
	_ = os.Chmod(dir, 0o700)
	if err == nil {
		t.Skip("checkpoint succeeded in a read-only directory (running as root?)")
	}
	if errors.Is(err, ErrHistoryJournalFailed) {
		t.Fatalf("a failure before any side effect must not fail-stop: %v", err)
	}
	if err := h.Checkpoint(); err != nil {
		t.Fatalf("retry after a pre-side-effect failure: %v", err)
	}
	if _, err := h.Append(Packet{Stream: s, Payload: []byte("two")}); err != nil {
		t.Fatal(err)
	}
}

func appendWALLine(t *testing.T, directory string, generation uint64, mutation HistoryMutation) {
	t.Helper()
	probe := &FileHistoryJournal{directory: directory}
	data, err := json.Marshal(mutation)
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(probe.walPath(generation), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write(append(data, '\n')); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func loadJournalDirectory(t *testing.T, directory string) (HistorySnapshot, error) {
	t.Helper()
	journal, err := NewFileHistoryJournal(directory, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	return journal.Load()
}

func TestWALReplayRefusesMutationsThatDoNotContinueTheCheckpoint(t *testing.T) {
	// 先读一次合法的目录，拿到检查点所述的世界：epoch、唯一一条流、latest。
	directory, _ := checkpointedJournal(t)
	base, err := loadJournalDirectory(t, directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(base.Streams) != 1 || base.Streams[0].Latest != 2 || base.Epoch == 0 {
		t.Fatalf("baseline snapshot = %+v, want one stream at latest 2 with an epoch", base)
	}
	observer, stream, epoch := base.Streams[0].Observer, base.Streams[0].Stream, base.Epoch
	packet := func(sequence uint64) Packet {
		return Packet{Observer: observer, Stream: stream, Epoch: epoch, Sequence: sequence, SchemaVersion: 1, Payload: []byte("forged")}
	}
	acknowledge := func(target Observer, sequence uint64) HistoryMutation {
		return HistoryMutation{Version: HistoryMutationVersion, Kind: HistoryMutationAcknowledge, Epoch: epoch, Observer: target, Stream: stream, Sequence: sequence}
	}
	cases := []struct {
		name     string
		mutation HistoryMutation
	}{
		{"foreign wire version", func() HistoryMutation { m := acknowledge(observer, 1); m.Version++; return m }()},
		{"another epoch", func() HistoryMutation { m := acknowledge(observer, 1); m.Epoch++; return m }()},
		{"append skipping a sequence", HistoryMutation{Version: HistoryMutationVersion, Kind: HistoryMutationAppend, Epoch: epoch, Packet: packet(4)}},
		{"append repeating the latest sequence", HistoryMutation{Version: HistoryMutationVersion, Kind: HistoryMutationAppend, Epoch: epoch, Packet: packet(2)}},
		{"acknowledge of an unknown stream", acknowledge(Observer{ID: 99}, 1)},
		{"acknowledge past the latest", acknowledge(observer, 3)},
		{"unknown mutation kind", HistoryMutation{Version: HistoryMutationVersion, Kind: "compact", Epoch: epoch}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			directory, generation := checkpointedJournal(t)
			appendWALLine(t, directory, generation, tc.mutation)
			if _, err := loadJournalDirectory(t, directory); !errors.Is(err, ErrInvalidSnapshot) {
				t.Fatalf("Load with a WAL line that does not continue the checkpoint = %v, want ErrInvalidSnapshot", err)
			}
		})
	}

	// 对照：真正相接的续写（追加 3、确认到 3）必须被回放进快照。
	directory, generation := checkpointedJournal(t)
	appendWALLine(t, directory, generation, HistoryMutation{Version: HistoryMutationVersion, Kind: HistoryMutationAppend, Epoch: epoch, Packet: packet(3)})
	appendWALLine(t, directory, generation, acknowledge(observer, 3))
	snapshot, err := loadJournalDirectory(t, directory)
	if err != nil {
		t.Fatalf("Load with a well-formed continuation = %v", err)
	}
	if len(snapshot.Streams) != 1 || snapshot.Streams[0].Latest != 3 || snapshot.Streams[0].Acked != 3 {
		t.Fatalf("replayed snapshot = %+v, want latest 3 acked 3", snapshot.Streams)
	}
}

func TestEntryPointsRefuseMissingDependencies(t *testing.T) {
	if _, err := NewFileHistoryJournal("", 1); !errors.Is(err, ErrHistoryJournalRequired) {
		t.Fatalf("NewFileHistoryJournal(\"\") = %v", err)
	}
	var nilJournal *FileHistoryJournal
	if err := nilJournal.Record(HistoryMutation{Version: HistoryMutationVersion}); !errors.Is(err, ErrHistoryJournalClosed) {
		t.Fatalf("Record on a nil journal = %v", err)
	}
	if _, err := nilJournal.Load(); !errors.Is(err, ErrHistoryJournalClosed) {
		t.Fatalf("Load on a nil journal = %v", err)
	}
	if err := nilJournal.Checkpoint(HistorySnapshot{}); !errors.Is(err, ErrHistoryJournalClosed) {
		t.Fatalf("Checkpoint on a nil journal = %v", err)
	}

	if _, err := NewHistoryWithJournal(HistoryOptions{}, nil); !errors.Is(err, ErrHistoryJournalRequired) {
		t.Fatalf("NewHistoryWithJournal(nil) = %v", err)
	}
	if err := NewHistory(HistoryOptions{}).Checkpoint(); !errors.Is(err, ErrHistoryJournalRequired) {
		t.Fatalf("Checkpoint on a history without a journal = %v", err)
	}
	var nilHistory *History
	if err := nilHistory.Checkpoint(); !errors.Is(err, ErrHistoryJournalRequired) {
		t.Fatalf("Checkpoint on a nil history = %v", err)
	}

	if _, err := NewPublisher(nil, 1, nil); !errors.Is(err, ErrPublisherRequired) {
		t.Fatalf("NewPublisher(nil) = %v", err)
	}
	var nilPublisher *Publisher
	if err := nilPublisher.Publish(Packet{}); !errors.Is(err, ErrPublisherRequired) {
		t.Fatalf("Publish on a nil publisher = %v", err)
	}
	if err := (&Publisher{}).Publish(Packet{}); !errors.Is(err, ErrPublisherRequired) {
		t.Fatalf("Publish on a publisher without a bus = %v", err)
	}

	handler := func(Packet) error { return nil }
	if _, err := SubscribeWithOptions(nil, "state", SubscribeOptions{}, handler); !errors.Is(err, ErrSubscriberRequired) {
		t.Fatalf("SubscribeWithOptions(nil bus) = %v", err)
	}
	bus := &memoryBus{}
	if _, err := SubscribeWithOptions(bus, "state", SubscribeOptions{}, nil); !errors.Is(err, ErrHandlerRequired) {
		t.Fatalf("SubscribeWithOptions(nil handler) = %v", err)
	}
	if bus.handler != nil {
		t.Fatal("a refused subscribe still installed a bus handler")
	}
}

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
