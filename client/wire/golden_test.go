package wire_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/client/wire"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/entitysync"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/frame"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/lockstep"
	"github.com/tjbdwanghaibo/roost-core/robot"
	"strconv"
)

var updateGolden = flag.Bool("update", false, "rewrite shared Go/C# packet golden")

func TestClientProtocolGolden(t *testing.T) {
	update, err := entitysync.EncodeSubjectUpdate(entity.SubjectSyncUpdate{
		SubjectID: 9007199254740993, SubjectKind: 7, Version: 3, Full: true,
		Namespace: "avatar", Profile: entity.SyncProfile{Key: "owner", SchemaVersion: 1},
		Payload: entity.CopyFrozenSyncPayload(1, []byte{8, 42}),
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	state, err := frame.Encode(frame.Frame{SnapshotMeta: frame.SnapshotMeta{StreamID: 1, Epoch: 2, Tick: 3, SchemaVersion: 1}, Kind: frame.Full,
		Objects: []frame.ObjectDelta{{Operation: frame.ObjectCreate, Ref: frame.ObjectRef{ID: 1, Generation: 2}, Archetype: 1,
			Components: []frame.ComponentDelta{{Operation: frame.ComponentSet, TypeID: 1, SchemaVersion: 1, Data: update}}}},
	}, frame.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	frames := []lockstep.Frame{{ID: 1, Inputs: []lockstep.Input{{Player: 7, Payload: []byte{8, 42}}, {Player: -1, Payload: []byte{255}}}}, {ID: 2}}
	hasher := robot.NewFrameHasher()
	for _, f := range frames {
		hasher.Fold(f)
	}
	commands := []lockstep.Command{{Operation: lockstep.OpInput, Frame: 1, Payload: []byte{8, 42}}, {Operation: lockstep.OpHash, Frame: 2, Hash: ^uint64(0)}, {Operation: lockstep.OpCatchup, Frame: 1}}
	packets := []*wire.Packet{{MsgID: 42, Seq: 7, Payload: []byte{8, 150, 1}}, {Flags: wire.FlagPush | wire.FlagSync, MsgID: 10103, Seq: 9, Payload: state}, {MsgID: 0, Seq: 1, Payload: []byte("ticket")}}
	packets = append(packets, &wire.Packet{Flags: wire.FlagPush | wire.FlagLockstep, MsgID: 50002, Seq: 10, Payload: lockstep.EncodeBroadcast(frames)})
	for _, c := range commands {
		data, err := lockstep.EncodeCommand(c)
		if err != nil {
			t.Fatal(err)
		}
		packets = append(packets, &wire.Packet{Flags: wire.FlagLockstep, MsgID: 50001, Seq: 11, Payload: data})
	}
	// 恢复失败通知（C9 v2）与快照分块（CA 01）都与广播 / 追帧页同一路由下发。
	packets = append(packets, &wire.Packet{Flags: wire.FlagPush | wire.FlagLockstep, MsgID: 50002, Seq: 12, Payload: lockstep.EncodeCatchupUnservable(300, 70000)})
	packets = append(packets, &wire.Packet{Flags: wire.FlagPush | wire.FlagLockstep, MsgID: 50002, Seq: 13, Payload: lockstep.EncodeRecoveryFailure(lockstep.RecoverySnapshotUnavailable, 90000, 70000)})
	// 快照：帧 1234、24 字节、状态 hash 最高位为 1（检验 uvarint64），按 16 字节切两块；两块拼起来的 SHA-256 等于首块摘要。
	snapshot := []byte("roost-snapshot-golden-24")
	first, err := lockstep.EncodeSnapshotChunk(lockstep.SnapshotChunk{Transfer: 3, Offset: 0, Frame: 1234, Total: len(snapshot),
		StateHash: 1<<63 | 0x1234, HasStateHash: true, Digest: sha256.Sum256(snapshot), Data: snapshot[:16]})
	if err != nil {
		t.Fatal(err)
	}
	next, err := lockstep.EncodeSnapshotChunk(lockstep.SnapshotChunk{Transfer: 3, Offset: 16, Data: snapshot[16:]})
	if err != nil {
		t.Fatal(err)
	}
	packets = append(packets, &wire.Packet{Flags: wire.FlagPush | wire.FlagLockstep, MsgID: 50002, Seq: 14, Payload: first},
		&wire.Packet{Flags: wire.FlagPush | wire.FlagLockstep, MsgID: 50002, Seq: 15, Payload: next})
	type golden struct {
		Name string `json:"name"`
		Hex  string `json:"hex"`
		Hash string `json:"hash,omitempty"`
	}
	want := []golden{}
	for i, name := range []string{"pb", "sync", "auth", "lockstep", "lockstep_input", "lockstep_hash", "lockstep_catchup", "catchup_unservable",
		"recovery_failed_snapshot_unavailable", "snapshot_first_chunk", "snapshot_next_chunk"} {
		raw, err := wire.Encode([]*wire.Packet{packets[i]}, 0)
		if err != nil {
			t.Fatal(err)
		}
		entry := golden{Name: name, Hex: hex.EncodeToString(raw)}
		if name == "lockstep" {
			entry.Hash = strconv.FormatUint(hasher.Sum(), 10)
		}
		if name == "snapshot_first_chunk" {
			entry.Hash = strconv.FormatUint(1<<63|0x1234, 10) // 首块携带的状态 hash，C# 解码后核对
		}
		want = append(want, entry)
	}
	content, err := json.MarshalIndent(want, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	content = append(content, '\n')
	path := "../spec/packets.json"
	if *updateGolden {
		if err := os.WriteFile(path, content, 0644); err != nil {
			t.Fatal(err)
		}
	}
	actual, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Git autocrlf只能改变JSON文件换行；hex中的实际协议字节仍必须完全一致。
	actual = bytes.ReplaceAll(actual, []byte("\r\n"), []byte("\n"))
	if !bytes.Equal(content, actual) {
		t.Fatal("cross-language golden changed: regenerate deliberately and run C# consumer")
	}
}

// Go 客户端消费同一份金样（C# 在 LockstepTests 消费）：C9 v2 解出原因，两块 CA 分块经 FrameAssembler.Receive
// 重组成帧 1234 的快照（摘要已核对、状态 hash 与金样记录的一致），恢复后续于 1235；FrameAssembler 与
// robot.LockstepBot 走同一条解码路径。
func TestClientProtocolGoldenDecodesThroughTheGoClient(t *testing.T) {
	content, err := os.ReadFile("../spec/packets.json")
	if err != nil {
		t.Fatal(err)
	}
	var entries []struct {
		Name string `json:"name"`
		Hex  string `json:"hex"`
		Hash string `json:"hash"`
	}
	if err := json.Unmarshal(content, &entries); err != nil {
		t.Fatal(err)
	}
	payloads := map[string][]byte{}
	hashes := map[string]string{}
	for _, entry := range entries {
		raw, err := hex.DecodeString(entry.Hex)
		if err != nil {
			t.Fatal(err)
		}
		packets, err := wire.Decode(raw, 0)
		if err != nil || len(packets) != 1 {
			t.Fatalf("%s: %d packets, %v", entry.Name, len(packets), err)
		}
		payloads[entry.Name], hashes[entry.Name] = packets[0].Payload, entry.Hash
	}
	for name, want := range map[string]lockstep.CatchupUnservableError{
		"catchup_unservable":                   {Reason: lockstep.RecoveryHistoryTrimmed, From: 300, Start: 70000},
		"recovery_failed_snapshot_unavailable": {Reason: lockstep.RecoverySnapshotUnavailable, From: 90000, Start: 70000},
	} {
		_, err := lockstep.NewFrameAssembler(0).Receive(payloads[name])
		var notice *lockstep.CatchupUnservableError
		if !errors.As(err, &notice) || *notice != want || !errors.Is(err, lockstep.ErrCatchupUnservable) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	assembler := lockstep.NewFrameAssembler(0)
	first, err := assembler.Receive(payloads["snapshot_first_chunk"])
	if err != nil || first.Snapshot != nil || !assembler.SnapshotInProgress() {
		t.Fatalf("first chunk: %+v, %v", first, err)
	}
	last, err := assembler.Receive(payloads["snapshot_next_chunk"])
	if err != nil || last.Snapshot == nil {
		t.Fatalf("next chunk: %+v, %v", last, err)
	}
	snapshot := last.Snapshot
	if snapshot.Frame != 1234 || string(snapshot.Data) != "roost-snapshot-golden-24" || !snapshot.HasStateHash ||
		strconv.FormatUint(snapshot.StateHash, 10) != hashes["snapshot_first_chunk"] {
		t.Fatalf("golden snapshot frame %d data %q hash %v/%d (golden %s)", snapshot.Frame, snapshot.Data, snapshot.HasStateHash, snapshot.StateHash, hashes["snapshot_first_chunk"])
	}
	assembler.ResumeAfterSnapshot(snapshot.Frame)
	if assembler.Next() != 1235 {
		t.Fatalf("next after the golden snapshot %d", assembler.Next())
	}
	// 同一份金样经 robot：纯线协议 bot 以金样的状态 hash 续链。
	bot, err := robot.NewLockstepBot(robot.LockstepBotConfig{Sink: nopLockstepSink{}})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"snapshot_first_chunk", "snapshot_next_chunk"} {
		if err := bot.HandleBroadcast(payloads[name]); err != nil {
			t.Fatalf("robot %s: %v", name, err)
		}
	}
	if bot.Next() != 1235 || bot.Stats().SnapshotsRestored != 1 {
		t.Fatalf("robot after the golden snapshot: next %d, stats %+v", bot.Next(), bot.Stats())
	}
	if err := bot.HandleBroadcast(payloads["recovery_failed_snapshot_unavailable"]); !errors.Is(err, robot.ErrLockstepBotTerminal) || !errors.Is(err, lockstep.ErrCatchupUnservable) {
		t.Fatalf("robot on the golden recovery-failed notice: %v", err)
	}
}

type nopLockstepSink struct{}

func (nopLockstepSink) SubmitInput(lockstep.FrameID, []byte) error { return nil }
func (nopLockstepSink) ReportHash(lockstep.FrameID, uint64) error  { return nil }
func (nopLockstepSink) RequestCatchup(lockstep.FrameID) error      { return nil }
