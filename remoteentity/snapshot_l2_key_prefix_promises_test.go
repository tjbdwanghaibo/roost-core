package remoteentity

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/entity"
	fredis "github.com/tjbdwanghaibo/roost-core/redis"
)

// RR-20260927-17（OPEN-ITEMS C11）：共享 L2 快照键 remote_entity:snapshot:* 不带部署前缀，同一 Redis db 上的多个部署
// 读写同一份快照。承诺：Config.SnapshotL2KeyPrefix（kit：remote_entity.snapshot_l2_key_prefix）非空时全部 L2 读写删都落在
// "<prefix>:remote_entity:snapshot:…"，两个前缀不同的部署互不可见；为空（默认）时键与旧版本逐字相同，不需要迁移。

func l2TestKey(t *testing.T, rawID int64) entity.RemoteSnapshotKey {
	t.Helper()
	const kind entity.EntityKind = 127
	entity.MustRegisterEntityKindDefs(entity.EntityKindDef{Kind: kind, Category: 1, RemotePolicy: entity.RemotePolicyManaged})
	id, err := entity.BuildEntityID(rawID, kind)
	if err != nil {
		t.Fatal(err)
	}
	return entity.RemoteSnapshotKey{EntityID: id, Kind: kind, Scope: 1}
}

func l2Envelope(key entity.RemoteSnapshotKey, version uint64, payload string) entity.RemoteSnapshotEnvelope {
	return entity.RemoteSnapshotEnvelope{Key: key, StateVersion: version, BaseVersion: version - 1, MarkerEpoch: 1, RouteEpoch: 1, Schema: 1, Full: true,
		Payload: entity.CopyFrozenRemoteSnapshotPayload([]byte(payload))}
}

func (f *snapshotRedisFake) keys() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	keys := make([]string, 0, len(f.values))
	for key := range f.values {
		keys = append(keys, key)
	}
	return keys
}

func TestSnapshotL2DefaultKeyIsUnchanged(t *testing.T) {
	key := l2TestKey(t, 1911)
	redis := newSnapshotRedisFake()
	store, err := NewSnapshotL2StoreWithKeyPrefix(redis, time.Minute, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Set(context.Background(), l2Envelope(key, 1, "v1")); err != nil {
		t.Fatal(err)
	}
	// 旧版本的键格式逐字写出来，不经 remoteSnapshotL2Key，防止两边一起改。
	want := fmt.Sprintf("remote_entity:snapshot:%d:%d:%d:%d:%d", key.Tenant, key.Kind, key.EntityID, key.Scope, key.Policy)
	if got := redis.keys(); len(got) != 1 || got[0] != want {
		t.Fatalf("default L2 keys=%v, want exactly %q (unchanged, no migration)", got, want)
	}
	legacy := NewSnapshotL2Store(redis, time.Minute)
	if got, ok, err := legacy.Get(context.Background(), key); err != nil || !ok || string(got.Payload.BytesCopy()) != "v1" {
		t.Fatalf("store without a prefix does not read the default store's snapshot: ok=%v err=%v", ok, err)
	}
}

func TestSnapshotL2KeyPrefixIsolatesDeploymentsSharingOneRedis(t *testing.T) {
	key := l2TestKey(t, 1912)
	redis := newSnapshotRedisFake()
	stores := map[string]*remoteSnapshotL2Store{}
	for _, prefix := range []string{"roost:game-a", "roost:game-b"} {
		store, err := NewSnapshotL2StoreWithKeyPrefix(redis, time.Minute, prefix)
		if err != nil {
			t.Fatal(err)
		}
		stores[prefix] = store
	}
	ctx := context.Background()
	if err := stores["roost:game-a"].Set(ctx, l2Envelope(key, 5, "a")); err != nil {
		t.Fatal(err)
	}
	// 另一个部署的同一实体版本更低：各自的 CAS 只看自己的键，不会被对方的更高版本挡住。
	if err := stores["roost:game-b"].Set(ctx, l2Envelope(key, 2, "b")); err != nil {
		t.Fatal(err)
	}
	for prefix, want := range map[string]string{"roost:game-a": "a", "roost:game-b": "b"} {
		got, ok, err := stores[prefix].Get(ctx, key)
		if err != nil || !ok || string(got.Payload.BytesCopy()) != want {
			t.Fatalf("deployment %s reads %q ok=%v err=%v, want its own snapshot %q", prefix, got.Payload.BytesCopy(), ok, err, want)
		}
	}
	if _, ok, _ := NewSnapshotL2Store(redis, time.Minute).Get(ctx, key); ok {
		t.Fatal("an unprefixed deployment sees a prefixed deployment's snapshot")
	}
	for _, stored := range redis.keys() {
		if !strings.HasPrefix(stored, "roost:game-a:remote_entity:snapshot:") && !strings.HasPrefix(stored, "roost:game-b:remote_entity:snapshot:") {
			t.Fatalf("L2 key %q is outside both deployment prefixes", stored)
		}
	}
	if err := stores["roost:game-a"].DeleteAtVersion(ctx, key, 5); err != nil {
		t.Fatal(err)
	}
	if err := stores["roost:game-a"].Delete(ctx, key); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := stores["roost:game-a"].Get(ctx, key); ok {
		t.Fatal("delete did not remove deployment a's snapshot")
	}
	if got, ok, err := stores["roost:game-b"].Get(ctx, key); err != nil || !ok || string(got.Payload.BytesCopy()) != "b" {
		t.Fatalf("deleting deployment a's snapshot touched deployment b: ok=%v err=%v", ok, err)
	}
}

func TestSnapshotL2KeyPrefixValidation(t *testing.T) {
	for prefix, ok := range map[string]bool{
		"": true, "roost:game-a": true, "g1": true,
		" roost": false, "roost ": false, "roost game": false, "roost\tgame": false,
		"{roost:game}": false, "roost:{a}": false,
	} {
		err := ValidateSnapshotL2KeyPrefix(prefix)
		if (err == nil) != ok {
			t.Errorf("ValidateSnapshotL2KeyPrefix(%q) = %v, want ok=%v", prefix, err, ok)
		}
		if _, err := NewSnapshotL2StoreWithKeyPrefix(newSnapshotRedisFake(), time.Minute, prefix); (err == nil) != ok {
			t.Errorf("NewSnapshotL2StoreWithKeyPrefix(%q) = %v, want ok=%v", prefix, err, ok)
		}
	}
}

// Assemble 把 Config.SnapshotL2KeyPrefix 接到 Manager 实际使用的 L2 上，非法前缀在装配时拒绝。
func TestAssembleWiresSnapshotL2KeyPrefix(t *testing.T) {
	key := l2TestKey(t, 1913)
	backend, err := NewBackend(newRemoteTestLoader(), NewMongoCommitter(newRemoteMongoFake(), "control", 1000, 0))
	if err != nil {
		t.Fatal(err)
	}
	redis := &l2OnlyRedis{fake: newSnapshotRedisFake()}
	cfg := DefaultConfig()
	cfg.SnapshotL2KeyPrefix = "roost:game-a"
	asm, err := Assemble(AssemblyDeps{Redis: redis, Backend: backend}, cfg, 1000, MongoBackendConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if err := asm.Manager.remote.cache.Publish(context.Background(), l2Envelope(key, 3, "wired")); err != nil {
		t.Fatal(err)
	}
	keys := redis.fake.keys()
	if len(keys) != 1 || !strings.HasPrefix(keys[0], "roost:game-a:remote_entity:snapshot:") {
		t.Fatalf("assembled manager wrote L2 keys %v, want one key under the configured prefix", keys)
	}

	bad := DefaultConfig()
	bad.SnapshotL2KeyPrefix = "{roost:game}"
	if _, err := Assemble(AssemblyDeps{Redis: redis, Backend: backend}, bad, 1000, MongoBackendConfig{}); err == nil {
		t.Fatal("Assemble accepted a hash-tagged L2 key prefix")
	}
}

// l2OnlyRedis 只实现 L2 快照用到的三个方法，其余 IRedis 方法在本用例里不应被调用（调用即 nil panic）。
type l2OnlyRedis struct {
	fredis.IRedis
	fake *snapshotRedisFake
}

func (r *l2OnlyRedis) HGet(ctx context.Context, key, field string) ([]byte, error) {
	return r.fake.HGet(ctx, key, field)
}

func (r *l2OnlyRedis) Eval(ctx context.Context, script string, keys []string, args ...any) (any, error) {
	return r.fake.Eval(ctx, script, keys, args...)
}

func (r *l2OnlyRedis) Del(ctx context.Context, keys ...string) (int64, error) {
	return r.fake.Del(ctx, keys...)
}
