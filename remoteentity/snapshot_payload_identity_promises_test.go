package remoteentity

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/entity"
	"github.com/tjbdwanghaibo/roost-core/mirror"
)

// U-0179 · C2 · RR-20260913-03：payload 里的身份必须和信封绑定后才准写入。
//
// 通用 Replicator 校验外层 SyncMsg 与 mirror.Envelope 的 key/version,但 Remote 自己这一层的
// `wire.Key`、`wire.Update.Key` 和 `Update.StateVersion` 谁都没有和它们交叉校验过。于是把
// payload 里 Update.Key.Scope 从 1 改成 2、外层两层 key/version 一字不动,这条消息照样通过,
// 实际写进了 scope=2 的缓存并返回 nil —— 路由/顺序用的身份和实际被改的视图不是同一个。
//
// 这是协议完整性,不是授权:授权仍由传输与服务身份边界负责。
func TestApplyReplicaRefusesPayloadIdentityThatContradictsTheEnvelope(t *testing.T) {
	const kind entity.EntityKind = 243
	entity.MustRegisterEntityKindDefs(entity.EntityKindDef{Kind: kind, Category: 1, RemotePolicy: entity.RemotePolicyManaged})
	id, err := entity.BuildEntityID(9331, kind)
	if err != nil {
		t.Fatal(err)
	}
	declared := entity.RemoteSnapshotKey{EntityID: id, Kind: kind, Scope: 1}
	foreign := entity.RemoteSnapshotKey{EntityID: id, Kind: kind, Scope: 2}

	manager := NewManager(newMockVersionedLockFactory(), DefaultConfig(), 1000)
	store := SnapshotReplicaStore{mgr: manager}
	record := entity.RemoteSnapshotRecord{
		Key: declared, StateVersion: 4, MarkerEpoch: 1, RouteEpoch: 1, Schema: 1, Codec: 1,
		Full: true, Data: []byte("payload"),
	}
	record.Checksum = entity.RemoteSnapshotChecksum(record.Data)

	envelopeFor := func(w remoteSnapshotWire, key entity.RemoteSnapshotKey, version uint64) mirror.Envelope {
		raw, err := json.Marshal(w)
		if err != nil {
			t.Fatal(err)
		}
		return mirror.Envelope{Key: remoteSnapshotReplicaKey(key), Version: int64(version), Op: mirror.OpUpsert, Payload: raw}
	}

	// The honest message applies.
	good := remoteSnapshotWire{Key: declared, Update: record}
	if err := store.ApplyReplica(context.Background(), envelopeFor(good, declared, record.StateVersion)); err != nil {
		t.Fatalf("a well-formed message must apply: %v", err)
	}

	for _, tc := range []struct {
		name  string
		twist func(remoteSnapshotWire) remoteSnapshotWire
	}{
		{"payload key disagrees with the wire key", func(w remoteSnapshotWire) remoteSnapshotWire {
			w.Update.Key = foreign
			return w
		}},
		{"wire key disagrees with the envelope key", func(w remoteSnapshotWire) remoteSnapshotWire {
			w.Key = foreign
			w.Update.Key = foreign
			return w
		}},
		{"payload version disagrees with the envelope version", func(w remoteSnapshotWire) remoteSnapshotWire {
			w.Update.StateVersion = record.StateVersion + 7
			return w
		}},
		{"delete names a different key than the envelope", func(w remoteSnapshotWire) remoteSnapshotWire {
			return remoteSnapshotWire{Delete: true, Key: foreign}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before, hadBefore, err := manager.remote.cache.Get(context.Background(), foreign, entity.RemoteReadCached, 0)
			if err != nil {
				t.Fatal(err)
			}
			env := envelopeFor(tc.twist(remoteSnapshotWire{Key: declared, Update: record}), declared, record.StateVersion)
			if err := store.ApplyReplica(context.Background(), env); err == nil {
				t.Fatal("a message whose payload identity contradicts its envelope was applied")
			}
			after, hasAfter, err := manager.remote.cache.Get(context.Background(), foreign, entity.RemoteReadCached, 0)
			if err != nil {
				t.Fatal(err)
			}
			if hasAfter != hadBefore || after.StateVersion != before.StateVersion {
				t.Fatalf("the refused message still changed the foreign scope: before=(%d,%v) after=(%d,%v)",
					before.StateVersion, hadBefore, after.StateVersion, hasAfter)
			}
		})
	}
}
