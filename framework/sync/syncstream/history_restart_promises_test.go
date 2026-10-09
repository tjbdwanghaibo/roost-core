package syncstream

import (
	"os"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
)

// RR-20261006-50（F04-15）syncstream 重启与恢复的三条承诺。

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
