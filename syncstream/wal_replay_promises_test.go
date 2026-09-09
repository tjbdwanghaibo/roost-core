package syncstream

import (
	"encoding/json"
	"errors"
	"os"
	"testing"
)

// U-0126 · C2（空洞测试）· nightly gap map core `syncstream` 12/20。
//
// WAL 回放是恢复路径上唯一能查出"这段历史不属于这个检查点"的地方：检查点之后
// 的每条变更都必须与检查点所述的世界相接——同一 epoch、追加序号恰为 latest+1、
// 确认针对已知流且不越过 latest、变更版本是本代格式、种类可识别。任何一条不接
// 就必须以 ErrInvalidSnapshot 拒绝整次 Load，而不是把拼不上的历史交给 History
// （Import 只查快照自洽，查不出 WAL 属于另一个 epoch 或跳了序号）。另把构造器与
// 入口对缺失依赖的拒绝一并钉住。U-0074 已记 `Record` 的锁外关闭检查与锁内一份
// 互为冗余，本单元不再计。

// appendWALLine appends one encoded mutation to the WAL of the given generation
// in a closed journal directory produced by checkpointedJournal.
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
