package remoteentity

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/entity"
	fmongo "github.com/tjbdwanghaibo/roost-core/mongo"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// payloadWithChecksumHighBit finds a snapshot payload whose CRC64 has the top
// bit set, and one whose CRC64 does not. Both are ordinary payloads — that is
// the point of the test below: which half of the hash space a snapshot lands
// in is not something the caller chooses.
func payloadsByChecksumSign(t *testing.T) (low, high []byte) {
	t.Helper()
	for i := 0; i < 10_000 && (low == nil || high == nil); i++ {
		candidate := []byte(fmt.Sprintf("guild-roster-%d", i))
		if entity.RemoteSnapshotChecksum(candidate) > math.MaxInt64 {
			if high == nil {
				high = candidate
			}
			continue
		}
		if low == nil {
			low = candidate
		}
	}
	if low == nil || high == nil {
		t.Fatal("could not find payloads on both sides of MaxInt64")
	}
	return low, high
}

// RR-20260920-01：snapshot 的 checksum 是完整 64 位 CRC，高位为 1 是**正常值**，
// 大约一半的载荷会落在那一半。
//
// BSON 没有无符号 64 位整数，驱动对超过 MaxInt64 的值稳定返回 `overflows int64`。
// 把 `uint64` 原样放进文档，等于这些载荷写不进去：远端提交在投影阶段失败，而这条
// WAL 记录会在**每次启动恢复时**同样地再失败一次——进程从此起不来，只能删 WAL。
//
// 用例走真正的提交路径（fake mongo 用真实 bson 编解码每一个文档），所以验的是产品
// 代码写出来的那个文档。
func TestASnapshotCommitsWhicheverHalfOfTheHashSpaceItLandsIn(t *testing.T) {
	low, high := payloadsByChecksumSign(t)
	for name, data := range map[string][]byte{
		"checksum below MaxInt64": low,
		"checksum above MaxInt64": high,
	} {
		t.Run(name, func(t *testing.T) {
			const kind entity.EntityKind = 197
			entity.MustRegisterEntityKindDefs(entity.EntityKindDef{Kind: kind, Category: 1, RemotePolicy: entity.RemotePolicyManaged})
			id, err := entity.BuildEntityID(991, kind)
			if err != nil {
				t.Fatal(err)
			}
			key := entity.RemoteSnapshotKey{EntityID: id, Kind: kind, Scope: 1}
			checksum := entity.RemoteSnapshotChecksum(data)
			var tx entity.RemoteTransactionID
			tx[15] = 1
			commit := entity.RemoteCommit{
				TransactionID: tx, EntityID: id, Kind: kind, BaseVersion: 0, NextVersion: 1,
				MarkerEpoch: 1, RouteEpoch: 1, Schema: 1, Codec: 1,
				Mutations: []entity.RemoteDataMutation{{Database: "game", Collection: "guilds", ID: id, Version: 1, Data: data}},
				Snapshots: []entity.RemoteSnapshotRecord{{
					Key: key, BaseVersion: 0, StateVersion: 1, MarkerEpoch: 1, RouteEpoch: 1,
					Schema: 1, Codec: 1, Full: true, Data: data, Checksum: checksum,
				}},
			}
			store := NewMongoCommitter(newRemoteMongoFake(), "control", 7, 0)
			if err = store.EnsureRemoteStorage(context.Background()); err != nil {
				t.Fatal(err)
			}
			if _, err = store.CommitRemote(context.Background(), commit); err != nil {
				t.Fatalf("a snapshot whose checksum is %d could not be committed: %v", checksum, err)
			}
			snapshot, ok, err2 := store.LoadRemoteSnapshot(context.Background(), key, entity.RemoteReadLinearizable, 1)
			if err2 != nil || !ok {
				t.Fatalf("snapshot not readable back: ok=%v err=%v", ok, err2)
			}
			if snapshot.Checksum != checksum {
				t.Fatalf("checksum read back as %d, want %d", snapshot.Checksum, checksum)
			}
			if string(snapshot.Payload.BytesCopy()) != string(data) {
				t.Fatal("snapshot payload did not survive the round trip")
			}
		})
	}
}

// 版本 / epoch / fence 也是 uint64，但它们参与 Mongo 的数值比较（快照读用 `$gte`），
// 所以不能改成字节。业务上限就是 MaxInt64，超出必须在**写进任何地方之前**被拒绝，
// 而不是等到投影阶段才失败——那时记录已经持久化，恢复会一遍遍重放它。
func TestACommitWhoseCountersCannotBeEncodedIsRefusedUpFront(t *testing.T) {
	data := []byte("state")
	var tx entity.RemoteTransactionID
	tx[15] = 1
	const kind entity.EntityKind = 196
	entity.MustRegisterEntityKindDefs(entity.EntityKindDef{Kind: kind, Category: 1, RemotePolicy: entity.RemotePolicyManaged})
	id, err := entity.BuildEntityID(992, kind)
	if err != nil {
		t.Fatal(err)
	}
	base := entity.RemoteCommit{
		TransactionID: tx, EntityID: id, Kind: kind, BaseVersion: 0, NextVersion: 1,
		MarkerEpoch: 1, RouteEpoch: 1, Schema: 1, Codec: 1,
		Mutations: []entity.RemoteDataMutation{{Database: "game", Collection: "guilds", ID: id, Version: 1, Data: data}},
	}
	for name, mutate := range map[string]func(*entity.RemoteCommit){
		// A legitimate increment that lands one past what BSON can hold.
		"next version": func(c *entity.RemoteCommit) {
			c.BaseVersion, c.NextVersion = math.MaxInt64, math.MaxInt64+1
			c.Mutations[0].Version = math.MaxInt64 + 1
		},
		"marker epoch": func(c *entity.RemoteCommit) { c.MarkerEpoch = math.MaxInt64 + 1 },
		"route epoch":  func(c *entity.RemoteCommit) { c.RouteEpoch = math.MaxInt64 + 1 },
		"lock fence":   func(c *entity.RemoteCommit) { c.LockFence = math.MaxInt64 + 1 },
	} {
		t.Run(name, func(t *testing.T) {
			commit := base.Clone()
			mutate(&commit)
			store := NewMongoCommitter(newRemoteMongoFake(), "control", 7, 0)
			if err := store.EnsureRemoteStorage(context.Background()); err != nil {
				t.Fatal(err)
			}
			_, commitErr := store.CommitRemote(context.Background(), commit)
			if commitErr == nil {
				t.Fatalf("a %s beyond MaxInt64 was accepted", name)
			}
			// The promise is not which refusal it is — a CAS conflict is a
			// perfectly good one — but that the caller never gets a storage
			// overflow. That error means the record reached the encoder, and
			// on the durable path it means it reached the WAL first.
			if strings.Contains(commitErr.Error(), "overflows int64") {
				t.Fatalf("a %s beyond MaxInt64 failed at the encoder instead of up front: %v", name, commitErr)
			}
			if !errors.Is(commitErr, entity.ErrRemoteRejected) && !errors.Is(commitErr, fmongo.ErrVersionConflict) {
				t.Fatalf("a %s beyond MaxInt64 was refused with an unexpected error: %v", name, commitErr)
			}
		})
	}
}

// 升级之前写进去的 checksum 是 BSON 数字。升级之后必须还能读回来，否则升级本身
// 会让既有快照不可用。
func TestAChecksumStoredAsANumberStillReads(t *testing.T) {
	for name, stored := range map[string]any{
		"int64": int64(1234567890123),
		"int32": int32(4242),
		"absent": nil,
	} {
		t.Run(name, func(t *testing.T) {
			raw, err := bson.Marshal(bson.M{"checksum": stored})
			if err != nil {
				t.Fatal(err)
			}
			var back struct {
				Checksum entity.RemoteChecksum `bson:"checksum"`
			}
			if err := bson.Unmarshal(raw, &back); err != nil {
				t.Fatalf("an existing %s checksum can no longer be read: %v", name, err)
			}
			var want uint64
			switch typed := stored.(type) {
			case int64:
				want = uint64(typed)
			case int32:
				want = uint64(typed)
			}
			if uint64(back.Checksum) != want {
				t.Fatalf("old checksum read back as %d, want %d", uint64(back.Checksum), want)
			}
		})
	}
}

// 新形态写出去的就是 8 字节，不是数字——否则"兼容旧的"会被误读成"还在写旧的"。
func TestAChecksumIsStoredAsEightBytes(t *testing.T) {
	raw, err := bson.Marshal(bson.M{"checksum": entity.RemoteChecksum(1)})
	if err != nil {
		t.Fatal(err)
	}
	var back bson.M
	if err := bson.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	binaryValue, ok := back["checksum"].(bson.Binary)
	if !ok {
		t.Fatalf("checksum stored as %T, want bson.Binary", back["checksum"])
	}
	if len(binaryValue.Data) != 8 {
		t.Fatalf("checksum stored as %d bytes, want 8", len(binaryValue.Data))
	}
}
