package remoteentity

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/entity"
)

func l2CASKey(t *testing.T, kind entity.EntityKind, unique int64) entity.RemoteSnapshotKey {
	t.Helper()
	entity.MustRegisterEntityKindDefs(entity.EntityKindDef{Kind: kind, Category: 1, RemotePolicy: entity.RemotePolicyManaged})
	id, err := entity.BuildEntityID(unique, kind)
	if err != nil {
		t.Fatal(err)
	}
	return entity.RemoteSnapshotKey{EntityID: id, Kind: kind, Scope: 1}
}

// U-0176 · C8 · RR-20260913-07：L2 的同版本冲突比较必须包含 schema 与 codec。
//
// CAS 只比 marker / route / version / checksum,而 checksum 只覆盖 payload 字节。于是同一个
// 版本、同样的字节,换一个 schema 就被接受了 —— 同一份字节的解释方式被悄悄改掉,跨进程解码契约
// 分叉。本地的 RemoteSnapshotCache.Publish 对同样的输入是拒绝的(它比了 Schema/Codec/Checksum),
// 两层规则不一致。
func TestL2RejectsSameVersionWithADifferentSchemaOrCodec(t *testing.T) {
	key := l2CASKey(t, 241, 9321)
	base := entity.RemoteSnapshotEnvelope{
		Key: key, StateVersion: 5, MarkerEpoch: 2, RouteEpoch: 1, Schema: 1, Codec: 1, Full: true,
		Payload: entity.CopyFrozenRemoteSnapshotPayload([]byte("same-bytes")),
	}
	for _, tc := range []struct {
		name  string
		twist func(entity.RemoteSnapshotEnvelope) entity.RemoteSnapshotEnvelope
	}{
		{"schema", func(v entity.RemoteSnapshotEnvelope) entity.RemoteSnapshotEnvelope { v.Schema = 2; return v }},
		{"codec", func(v entity.RemoteSnapshotEnvelope) entity.RemoteSnapshotEnvelope { v.Codec = 2; return v }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := NewSnapshotL2Store(newSnapshotRedisFake(), time.Minute)
			if err := store.Set(context.Background(), base); err != nil {
				t.Fatal(err)
			}
			if err := store.Set(context.Background(), tc.twist(base)); !errors.Is(err, entity.ErrRemoteVersionConflict) {
				t.Fatalf("same version with a different %s = %v, want ErrRemoteVersionConflict", tc.name, err)
			}
			stored, ok, err := store.Get(context.Background(), key)
			if err != nil || !ok {
				t.Fatalf("get after the refusal: ok=%v err=%v", ok, err)
			}
			if stored.Schema != 1 || stored.Codec != 1 {
				t.Fatalf("the refused write still changed the stored interpretation: schema=%d codec=%d", stored.Schema, stored.Codec)
			}
		})
	}

	// An identical re-publish is still accepted, and a genuinely newer version
	// may of course change schema.
	store := NewSnapshotL2Store(newSnapshotRedisFake(), time.Minute)
	if err := store.Set(context.Background(), base); err != nil {
		t.Fatal(err)
	}
	if err := store.Set(context.Background(), base); err != nil {
		t.Fatalf("an identical re-publish must be accepted: %v", err)
	}
	newer := base
	newer.StateVersion = 6
	newer.BaseVersion = 5
	newer.Schema = 2
	if err := store.Set(context.Background(), newer); err != nil {
		t.Fatalf("a newer version may change schema: %v", err)
	}
}

// U-0177 · C8 · RR-20260913-10：版本比较必须在 uint64 全域精确,不能过浮点。
//
// 脚本用 tonumber 把版本转成 Lua 数值再比较,而 Lua 的数值是 float64:超过 2^53 之后相邻的
// uint64 会塌到同一个值。实测真实 Redis 8.8.0:先写 9007199254740993 再写同字节的
// 9007199254740992,旧值被接受、wire 里的版本回退;反过来先写 ...992 再写不同字节的 ...993,
// 被当成"同版本内容冲突"拒绝。Go 侧的契约是 uint64,两边的比较域必须一致。
func TestL2ComparesVersionsExactlyBeyondFloatPrecision(t *testing.T) {
	key := l2CASKey(t, 242, 9322)
	const (
		lower uint64 = 1<<53 + 0 // 9007199254740992
		upper uint64 = 1<<53 + 1 // 9007199254740993, indistinguishable as float64
	)
	same := []byte("identical-bytes")

	t.Run("an older version is refused even one apart", func(t *testing.T) {
		store := NewSnapshotL2Store(newSnapshotRedisFake(), time.Minute)
		newer := entity.RemoteSnapshotEnvelope{
			Key: key, StateVersion: upper, MarkerEpoch: 1, RouteEpoch: 1, Schema: 1, Codec: 1, Full: true,
			Payload: entity.CopyFrozenRemoteSnapshotPayload(same),
		}
		if err := store.Set(context.Background(), newer); err != nil {
			t.Fatal(err)
		}
		older := newer
		older.StateVersion = lower
		if err := store.Set(context.Background(), older); err != nil {
			t.Fatalf("writing the older version = %v, want it silently ignored as stale", err)
		}
		stored, ok, err := store.Get(context.Background(), key)
		if err != nil || !ok {
			t.Fatalf("get: ok=%v err=%v", ok, err)
		}
		if stored.StateVersion != upper {
			t.Fatalf("stored version = %d, want %d; the older write was accepted and the version went backwards",
				stored.StateVersion, upper)
		}
	})

	t.Run("a newer version with different bytes is not a same-version conflict", func(t *testing.T) {
		store := NewSnapshotL2Store(newSnapshotRedisFake(), time.Minute)
		first := entity.RemoteSnapshotEnvelope{
			Key: key, StateVersion: lower, MarkerEpoch: 1, RouteEpoch: 1, Schema: 1, Codec: 1, Full: true,
			Payload: entity.CopyFrozenRemoteSnapshotPayload([]byte("first")),
		}
		if err := store.Set(context.Background(), first); err != nil {
			t.Fatal(err)
		}
		next := first
		next.StateVersion = upper
		next.BaseVersion = lower
		next.Payload = entity.CopyFrozenRemoteSnapshotPayload([]byte("second"))
		if err := store.Set(context.Background(), next); err != nil {
			t.Fatalf("a genuinely newer version = %v, want it accepted", err)
		}
		stored, _, err := store.Get(context.Background(), key)
		if err != nil {
			t.Fatal(err)
		}
		if stored.StateVersion != upper {
			t.Fatalf("stored version = %d, want %d", stored.StateVersion, upper)
		}
	})
}
