package syncstream

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
)

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
				if err := h.AcknowledgeEpoch(o, s, old.Epoch, old.Sequence); err != nil {
					t.Fatal(err)
				}
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

func counterTotal(name string) int64 {
	var total int64
	for _, metric := range metrics.Snapshot() {
		if metric.Name == name {
			total += metric.Value
		}
	}
	return total
}

// (a) 目录里还没有 checkpoint 时，第一条 WAL 只写了半截（崩溃 / 断电，Record 从未返回成功）。旧实现 Load 因
// wal-1 非空把 Epoch 置 0 等第一条完整记录来定，回放跳过半尾后一条完整记录也没有，Epoch 仍为 0，Import 报
// `epoch is required`，History 永久打不开，只能人工删 wal-1。承诺：与 nestwal 的尾部规则一致——从未确认的
// 半尾不算数，按空日志打开（用本次的 initialEpoch），首次续写前截掉半尾，打开时计数并告警。
func TestHistoryOpensWhenTheFirstWALRecordIsTornBeforeAnyCheckpoint(t *testing.T) {
	dir := t.TempDir()
	j, err := NewFileHistoryJournal(dir, 7)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(j.walPath(1), []byte(`{"Version":1,"Kind":"append","Epoch":123`), 0o600); err != nil {
		t.Fatal(err)
	}
	before := counterTotal("syncstream.recovery.tail_truncated.total")
	h, err := NewHistoryWithJournal(HistoryOptions{}, j)
	if err != nil {
		t.Fatalf("history with a torn first WAL record cannot open: %v", err)
	}
	if got := counterTotal("syncstream.recovery.tail_truncated.total") - before; got != 1 {
		t.Fatalf("torn tail counted %d times, want 1", got)
	}
	if h.Epoch() != 7 {
		t.Fatalf("epoch = %d, want the journal's initial epoch 7", h.Epoch())
	}
	s := Stream{Topic: "state", Key: 1}
	if _, err := h.Append(Packet{Stream: s, Full: true, Payload: []byte("first")}); err != nil {
		t.Fatal(err)
	}
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	j2, err := NewFileHistoryJournal(dir, 99)
	if err != nil {
		t.Fatal(err)
	}
	h2, err := NewHistoryWithJournal(HistoryOptions{}, j2)
	if err != nil {
		t.Fatalf("reopen after recovery: %v", err)
	}
	defer j2.Close()
	if h2.Epoch() != 7 || h2.Status(Observer{}, s).LatestSequence != 1 {
		t.Fatalf("reopened history lost the committed record: epoch=%d status=%+v", h2.Epoch(), h2.Status(Observer{}, s))
	}
}

// (b) 回放 WAL 不恢复 LastActivity：checkpoint 之后活跃过的流，重启后仍带着 checkpoint 时的活动时间，首次
// SweepIdle 把它当作空闲删掉。承诺：WAL 里的 Append / 推进 ACK 带上活动时间，回放据此恢复；没有时间的
// 记录按“刚活动过”处理（宁可晚删，不误删）。
func TestHistoryReplayRestoresLastActivity(t *testing.T) {
	for _, mode := range []string{"append", "acknowledge"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			options := HistoryOptions{IdleTTL: time.Hour}
			j, err := NewFileHistoryJournal(dir, 7)
			if err != nil {
				t.Fatal(err)
			}
			h, err := NewHistoryWithJournal(options, j)
			if err != nil {
				t.Fatal(err)
			}
			s := Stream{Topic: "state", Key: 1}
			if _, err := h.Append(Packet{Stream: s, Full: true}); err != nil {
				t.Fatal(err)
			}
			// checkpoint 里这条流两小时没动过。
			snapshot := h.Export()
			snapshot.Streams[0].LastActivityUnixNano = time.Now().Add(-2 * time.Hour).UnixNano()
			if err := h.Import(snapshot); err != nil {
				t.Fatal(err)
			}
			// checkpoint 之后它又活跃了（只记在 WAL 里）。
			if mode == "append" {
				if _, err := h.Append(Packet{Stream: s, Payload: []byte("delta")}); err != nil {
					t.Fatal(err)
				}
			} else if err := h.Acknowledge(Observer{}, s, 1); err != nil {
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
			h2, err := NewHistoryWithJournal(options, j2)
			if err != nil {
				t.Fatal(err)
			}
			removed, err := h2.SweepIdle(time.Now())
			if err != nil {
				t.Fatal(err)
			}
			if removed != 0 || h2.Status(Observer{}, s).LatestSequence == 0 {
				t.Fatalf("SweepIdle after restart removed %d stream(s) active since the checkpoint", removed)
			}
		})
	}
}

// (c) Recover 用请求方的 SchemaVersion 覆盖 provider 包上的版本：provider（如 skillsync）按自己的 schema 编码、
// 也如实标了版本，却被改成请求方的数字，载荷与标签不一致，随后生产方按真实 schema 发的 delta 被拒，直到下一个
// Full。承诺：provider 标了版本就以它为准；provider 没标（0）时才用请求方的版本补上（原有行为）。
func TestRecoverKeepsTheProvidersSchemaVersion(t *testing.T) {
	s := Stream{Topic: "state", Key: 1}
	for _, tc := range []struct {
		name     string
		provider uint32
		want     uint32
	}{{"provider_labelled", 3, 3}, {"provider_unlabelled", 0, 1}} {
		t.Run(tc.name, func(t *testing.T) {
			h := NewHistory(HistoryOptions{SchemaVersion: 3, Epoch: 7})
			result, err := h.Recover(ResyncRequest{Stream: s, Epoch: 7, SchemaVersion: 1}, snapshotProviderFunc(func(ResyncRequest) (Packet, error) {
				return Packet{SchemaVersion: tc.provider, Payload: []byte("encoded-by-provider")}, nil
			}))
			if err != nil {
				t.Fatal(err)
			}
			if len(result.Packets) != 1 || result.Packets[0].SchemaVersion != tc.want {
				t.Fatalf("recovered packet schema = %+v, want %d", result.Packets, tc.want)
			}
		})
	}
}

func TestRecoverPromiseRejectsReplacementAtTheSamePosition(t *testing.T) {
	for _, mode := range []string{"no_change", "append_control", "append_delete_absent", "same_position_import", "delete_observer_absent"} {
		t.Run(mode, func(t *testing.T) {
			h := NewHistory(HistoryOptions{Epoch: 7})
			s := Stream{Topic: "state"}
			appendFull := func(x *History, payload string) {
				t.Helper()
				if _, err := x.Append(Packet{Stream: s, Full: true, Payload: []byte(payload)}); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "same_position_import" {
				appendFull(h, "before")
			}
			result, err := h.Recover(ResyncRequest{Stream: s, Epoch: 999}, snapshotProviderFunc(func(ResyncRequest) (Packet, error) {
				// Everything below happens after the capture began and before
				// it is committed: the interleaving the position check misses.
				switch mode {
				case "append_delete_absent":
					appendFull(h, "new")
					if e := h.DeleteStream(Observer{}, s); e != nil {
						t.Fatal(e)
					}
				case "delete_observer_absent":
					appendFull(h, "new")
					if _, e := h.DeleteObserver(Observer{}); e != nil {
						t.Fatal(e)
					}
				case "same_position_import":
					fresh := NewHistory(HistoryOptions{Epoch: 7})
					appendFull(fresh, "replacement")
					if e := h.Import(fresh.Export()); e != nil {
						t.Fatal(e)
					}
				case "append_control":
					appendFull(h, "new")
				}
				return Packet{Payload: []byte("stale")}, nil
			}))
			if mode == "no_change" {
				if err != nil || len(result.Packets) != 1 || string(result.Packets[0].Payload) != "stale" {
					t.Fatalf("plain recover: %+v %v", result, err)
				}
				return
			}
			if !errors.Is(err, ErrRecoverStale) {
				payload := ""
				if len(result.Packets) > 0 {
					payload = string(result.Packets[0].Payload)
				}
				t.Fatalf("stale capture accepted after %s: err=%v committed payload=%q", mode, err, payload)
			}
			// The newer state is untouched: whatever the interleaving left is
			// what a subsequent reader sees, never the stale capture.
			for _, item := range h.Export().Streams {
				for _, p := range item.Packets {
					if string(p.Payload) == "stale" {
						t.Fatalf("stale capture is in the history after %s", mode)
					}
				}
			}
		})
	}
}

func TestRecoverPromiseDoesNotCommitAStaleCapture(t *testing.T) {
	for _, mode := range []string{"no_change", "same_stream_append", "epoch_rotation", "provider_error", "replay_skips_provider"} {
		t.Run(mode, func(t *testing.T) {
			h := NewHistory(HistoryOptions{Epoch: 7})
			s := Stream{Topic: "state"}
			if mode == "replay_skips_provider" {
				h.Append(Packet{Stream: s, Full: true, Payload: []byte("current")})
				r, err := h.Recover(ResyncRequest{Stream: s, Epoch: 7}, snapshotProviderFunc(func(ResyncRequest) (Packet, error) {
					t.Fatal("provider called for replay")
					return Packet{}, nil
				}))
				if err != nil || len(r.Packets) != 1 {
					t.Fatal(err)
				}
				return
			}
			entered, release := make(chan struct{}), make(chan struct{})
			type outcome struct {
				r ResyncResult
				e error
			}
			done := make(chan outcome, 1)
			go func() {
				r, e := h.Recover(ResyncRequest{Stream: s, Epoch: 7}, snapshotProviderFunc(func(ResyncRequest) (Packet, error) {
					captured := Packet{Payload: []byte("old")}
					close(entered)
					<-release
					if mode == "provider_error" {
						return Packet{}, errors.New("capture failed")
					}
					return captured, nil
				}))
				done <- outcome{r, e}
			}()
			<-entered
			if mode == "same_stream_append" {
				h.Append(Packet{Stream: s, Full: true, Payload: []byte("new")})
			}
			if mode == "epoch_rotation" {
				h.RotateEpoch(8)
				h.Append(Packet{Stream: s, Full: true, Payload: []byte("new")})
			}
			close(release)
			got := <-done
			switch mode {
			case "provider_error":
				if got.e == nil || h.Metrics().Streams != 0 {
					t.Fatal("provider failure mutated history")
				}
			case "no_change":
				if got.e != nil || len(got.r.Packets) != 1 || string(got.r.Packets[0].Payload) != "old" {
					t.Fatalf("plain recover: %+v %v", got.r, got.e)
				}
			default:
				if got.e == nil {
					p := got.r.Packets[0]
					t.Fatalf("stale capture appended after newer state: epoch=%d seq=%d payload=%q", p.Epoch, p.Sequence, p.Payload)
				}
				status := h.Status(Observer{}, s)
				if string(h.Export().Streams[0].Packets[len(h.Export().Streams[0].Packets)-1].Payload) != "new" || status.LatestSequence != 1 {
					t.Fatalf("stale recover disturbed the newer state: %+v", status)
				}
			}
		})
	}
}

type snapshotStore struct {
	s   HistorySnapshot
	err error
}

func (s snapshotStore) Load() (HistorySnapshot, error) { return s.s, s.err }
func (s snapshotStore) Save(HistorySnapshot) error     { return s.err }

// 绑定了 journal 的 History，Import / Restore 先经 journal.Checkpoint 发布，成功才切换内存；失败保持旧状态（否则重启后替换丢失或
// WAL 混入不同 epoch）。构造期加载走内部路径，不经 journal。
// 历史：RR-20260915-08。
func TestHistoryPromiseImportOnAJournaledHistoryIsDurable(t *testing.T) {
	for _, mode := range []string{"import_same_epoch", "restore_same_epoch", "import_new_epoch", "restore_new_epoch", "checkpoint_same_epoch", "checkpoint_new_epoch"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			j, err := NewFileHistoryJournal(dir, 7)
			if err != nil {
				t.Fatal(err)
			}
			defer j.Close()
			h, err := NewHistoryWithJournal(HistoryOptions{}, j)
			if err != nil {
				t.Fatal(err)
			}
			s := Stream{Topic: "state"}
			if _, err := h.Append(Packet{Stream: s, Full: true, Payload: []byte("old")}); err != nil {
				t.Fatal(err)
			}
			epoch := uint64(7)
			if mode == "import_new_epoch" || mode == "restore_new_epoch" || mode == "checkpoint_new_epoch" {
				epoch = 8
			}
			replacement := NewHistory(HistoryOptions{Epoch: epoch})
			replacement.Append(Packet{Stream: s, Full: true, Payload: []byte("replacement")})
			snapshot := replacement.Export()
			if mode == "restore_same_epoch" || mode == "restore_new_epoch" {
				err = h.Restore(snapshotStore{s: snapshot})
			} else {
				err = h.Import(snapshot)
			}
			if err != nil {
				t.Fatal(err)
			}
			if mode == "checkpoint_same_epoch" || mode == "checkpoint_new_epoch" {
				if err := h.Checkpoint(); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := h.Append(Packet{Stream: s, Payload: []byte("next")}); err != nil {
				t.Fatal(err)
			}
			j.Close()
			j2, err := NewFileHistoryJournal(dir, 99)
			if err != nil {
				t.Fatal(err)
			}
			defer j2.Close()
			restored, err := NewHistoryWithJournal(HistoryOptions{}, j2)
			if err != nil {
				t.Fatalf("successful replacement and append cannot restart: %v", err)
			}
			snap := restored.Export()
			if string(snap.Streams[0].Packets[0].Payload) != "replacement" {
				t.Fatalf("replacement reverted on restart: %q", snap.Streams[0].Packets[0].Payload)
			}
			if restored.Epoch() != epoch {
				t.Fatalf("epoch after restart=%d want %d", restored.Epoch(), epoch)
			}
		})
	}
}

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
