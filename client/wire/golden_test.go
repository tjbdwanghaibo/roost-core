package wire_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
