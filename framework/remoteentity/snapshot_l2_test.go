// 共享 L2 快照存储：版本 CAS 拒绝迟到发布者与同版本不同内容、精确版本比较、带版本删除、键前缀隔离、墓碑等待设置。

package remoteentity

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/cache"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	fredis "github.com/tjbdwanghaibo/roost-core/infra/storage/redis"
	rediscore "github.com/tjbdwanghaibo/roost-core/infra/storage/redis"
)

type snapshotRedisFake struct {
	mu     sync.Mutex
	values map[string]map[string][]byte
}

func newSnapshotRedisFake() *snapshotRedisFake {
	return &snapshotRedisFake{values: make(map[string]map[string][]byte)}
}

func (f *snapshotRedisFake) HGet(_ context.Context, key, field string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	value := f.values[key][field]
	if value == nil {
		return nil, rediscore.ErrNil
	}
	return append([]byte(nil), value...), nil
}

func (f *snapshotRedisFake) Eval(_ context.Context, script string, keys []string, args ...any) (any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	// Mirrors the scripts exactly: ordered fields are compared as decimal
	// strings and stored verbatim, and the same-version check covers schema
	// and codec as well as the payload checksum.
	norm := func(v string) string {
		trimmed := strings.TrimLeft(v, "0")
		if trimmed == "" {
			return "0"
		}
		return trimmed
	}
	cmp := func(a, b string) int {
		a, b = norm(a), norm(b)
		if len(a) != len(b) {
			if len(a) < len(b) {
				return -1
			}
			return 1
		}
		return strings.Compare(a, b)
	}
	arg := func(value any) string { return fmt.Sprint(value) }
	if script == remoteSnapshotL2DeleteAtVersion {
		existing, held := f.values[keys[0]]
		if held && existing["version"] != nil && cmp(string(existing["version"]), arg(args[0])) > 0 {
			return int64(0), nil
		}
		tomb := arg(args[0])
		if held && existing["deleted_version"] != nil && cmp(string(existing["deleted_version"]), tomb) > 0 {
			tomb = string(existing["deleted_version"])
		}
		delete(f.values, keys[0])
		// The tombstone (RR-20260913-01 复核): only deleted_version, never
		// lowered, kept while a TTL is configured. The fake does not expire keys.
		if ttl, _ := args[1].(int64); ttl > 0 {
			f.values[keys[0]] = map[string][]byte{"deleted_version": []byte(tomb)}
		}
		return int64(1), nil
	}
	fields := f.values[keys[0]]
	if fields == nil {
		fields = make(map[string][]byte)
		f.values[keys[0]] = fields
	}
	deleted := fields["deleted_version"]
	if deleted != nil && cmp(arg(args[2]), string(deleted)) <= 0 {
		return int64(0), nil
	}
	oldMarker, oldRoute, oldVersion := string(fields["marker"]), string(fields["route"]), string(fields["version"])
	marker, route, version := arg(args[0]), arg(args[1]), arg(args[2])
	markerCmp, routeCmp := cmp(marker, oldMarker), cmp(route, oldRoute)
	if markerCmp < 0 || routeCmp < 0 {
		return int64(0), nil
	}
	sameEpoch := markerCmp == 0 && routeCmp == 0
	versionCmp := cmp(version, oldVersion)
	if sameEpoch && versionCmp < 0 {
		return int64(0), nil
	}
	checksum, schema, codec := arg(args[3]), arg(args[6]), arg(args[7])
	if sameEpoch && versionCmp == 0 && len(fields["checksum"]) > 0 &&
		(string(fields["checksum"]) != checksum || string(fields["schema"]) != schema || string(fields["codec"]) != codec) {
		return int64(-1), nil
	}
	fields["marker"] = []byte(marker)
	fields["route"] = []byte(route)
	fields["version"] = []byte(version)
	fields["checksum"] = []byte(checksum)
	fields["schema"] = []byte(schema)
	fields["codec"] = []byte(codec)
	fields["data"] = append([]byte(nil), args[4].([]byte)...)
	delete(fields, "deleted_version")
	return int64(1), nil
}

func (f *snapshotRedisFake) Del(_ context.Context, keys ...string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, key := range keys {
		delete(f.values, key)
	}
	return int64(len(keys)), nil
}

func TestRemoteSnapshotL2RejectsDelayedPublisher(t *testing.T) {
	const kind entity.EntityKind = 127
	entity.MustRegisterEntityKindDefs(entity.EntityKindDef{Kind: kind, Category: 1, RemotePolicy: entity.RemotePolicyManaged})
	id, err := entity.BuildEntityID(1901, kind)
	if err != nil {
		t.Fatal(err)
	}
	key := entity.RemoteSnapshotKey{EntityID: id, Kind: kind, Scope: 1}
	store := NewSnapshotL2Store(newSnapshotRedisFake(), time.Minute)
	newer := entity.RemoteSnapshotEnvelope{Key: key, StateVersion: 8, BaseVersion: 7, MarkerEpoch: 4, RouteEpoch: 2, Schema: 1, Full: true, Payload: entity.CopyFrozenRemoteSnapshotPayload([]byte("new"))}
	if err := store.Set(context.Background(), newer); err != nil {
		t.Fatal(err)
	}
	stale := newer
	stale.StateVersion = 7
	stale.Payload = entity.CopyFrozenRemoteSnapshotPayload([]byte("stale"))
	// RR-20261005-NC-130：被拒的旧写报 ErrStaleWrite，L2 仍是较新的那份。
	if err := store.Set(context.Background(), stale); !errors.Is(err, cache.ErrStaleWrite) {
		t.Fatalf("delayed publisher = %v, want cache.ErrStaleWrite", err)
	}
	got, ok, err := store.Get(context.Background(), key)
	if err != nil || !ok || got.StateVersion != 8 || string(got.Payload.BytesCopy()) != "new" {
		t.Fatalf("got=%+v ok=%v err=%v", got, ok, err)
	}
}

func TestRemoteSnapshotL2RejectsSameVersionDifferentContent(t *testing.T) {
	const kind entity.EntityKind = 129
	entity.MustRegisterEntityKindDefs(entity.EntityKindDef{Kind: kind, Category: 1, RemotePolicy: entity.RemotePolicyManaged})
	id, err := entity.BuildEntityID(1903, kind)
	if err != nil {
		t.Fatal(err)
	}
	key := entity.RemoteSnapshotKey{EntityID: id, Kind: kind, Scope: 1}
	store := NewSnapshotL2Store(newSnapshotRedisFake(), time.Minute)
	value := entity.RemoteSnapshotEnvelope{Key: key, StateVersion: 1, MarkerEpoch: 1, RouteEpoch: 1, Schema: 1, Full: true, Payload: entity.CopyFrozenRemoteSnapshotPayload([]byte("a"))}
	if err := store.Set(context.Background(), value); err != nil {
		t.Fatal(err)
	}
	value.Payload = entity.CopyFrozenRemoteSnapshotPayload([]byte("b"))
	if err := store.Set(context.Background(), value); !errors.Is(err, entity.ErrRemoteVersionConflict) {
		t.Fatalf("conflict error=%v", err)
	}
}

func l2CASKey(t *testing.T, kind entity.EntityKind, unique int64) entity.RemoteSnapshotKey {
	t.Helper()
	entity.MustRegisterEntityKindDefs(entity.EntityKindDef{Kind: kind, Category: 1, RemotePolicy: entity.RemotePolicyManaged})
	id, err := entity.BuildEntityID(unique, kind)
	if err != nil {
		t.Fatal(err)
	}
	return entity.RemoteSnapshotKey{EntityID: id, Kind: kind, Scope: 1}
}

// L2 的同版本冲突比较包含 schema 与 codec（checksum 只覆盖 payload 字节），与本地 RemoteSnapshotCache.Publish 同一规则。
// 历史：RR-20260913-07。
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

// L2 版本比较在 uint64 全域精确，不能过浮点（Lua 数值是 float64，2^53 之后相邻 uint64 会塌到同一个值）。
// 历史：RR-20260913-10。
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
		// RR-20261005-NC-130：被拒的旧写按 cache.Store 约定报 ErrStaleWrite（之前是 nil）；
		// 这里要证明的仍是比较在 2^53 之上精确、存下的版本不倒退。
		if err := store.Set(context.Background(), older); !errors.Is(err, cache.ErrStaleWrite) {
			t.Fatalf("writing the older version = %v, want cache.ErrStaleWrite", err)
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

// L2 的删除带版本，比较与 DEL 在一个脚本里：更旧的删除报 cache.ErrStaleWrite、不清掉更新的快照；版本按精确十进制字符串比较，
// 2^53 以上的相邻版本不能塌成同一个值；不存在的键删除是空操作。
// 历史：RR-20260913-01。
func TestRemoteSnapshotL2DeleteAtVersionKeepsNewerSnapshot(t *testing.T) {
	const kind entity.EntityKind = 249
	entity.MustRegisterEntityKindDefs(entity.EntityKindDef{Kind: kind, Category: 1, RemotePolicy: entity.RemotePolicyManaged})
	id, err := entity.BuildEntityID(1902, kind)
	if err != nil {
		t.Fatal(err)
	}
	key := entity.RemoteSnapshotKey{EntityID: id, Kind: kind, Scope: 1}
	store := NewSnapshotL2Store(newSnapshotRedisFake(), time.Minute)
	ctx := context.Background()
	put := func(version uint64) {
		t.Helper()
		if err := store.Set(ctx, entity.RemoteSnapshotEnvelope{Key: key, StateVersion: version, MarkerEpoch: 1, RouteEpoch: 1, Schema: 1, Full: true, Payload: entity.CopyFrozenRemoteSnapshotPayload([]byte("s"))}); err != nil {
			t.Fatal(err)
		}
	}
	put(3)
	// B2：被更新快照拒绝的删除报 cache.ErrStaleWrite（之前把脚本的 0 当成功），缓存据此改取 L2 的快照。
	if err := store.DeleteAtVersion(ctx, key, 2); !errors.Is(err, cache.ErrStaleWrite) {
		t.Fatalf("delete older than the stored snapshot = %v, want cache.ErrStaleWrite", err)
	}
	if got, ok, _ := store.Get(ctx, key); !ok || got.StateVersion != 3 {
		t.Fatalf("old delete removed newer L2: found=%v version=%d", ok, got.StateVersion)
	}
	if err := store.DeleteAtVersion(ctx, key, 3); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := store.Get(ctx, key); ok {
		t.Fatal("delete at the stored version must remove the key")
	}
	// 精度:2^53+1 存着,delete 2^53 不能删(float64 下两者相等)。
	put(1<<53 + 1)
	if err := store.DeleteAtVersion(ctx, key, 1<<53); !errors.Is(err, cache.ErrStaleWrite) {
		t.Fatalf("delete at 2^53 below 2^53+1 = %v, want cache.ErrStaleWrite", err)
	}
	if _, ok, _ := store.Get(ctx, key); !ok {
		t.Fatal("delete at 2^53 removed the snapshot at 2^53+1: version compared as float")
	}
	// 不存在的键:删除是空操作,不报错。
	if err := store.DeleteAtVersion(ctx, entity.RemoteSnapshotKey{EntityID: id, Kind: kind, Scope: 2}, 9); err != nil {
		t.Fatal(err)
	}
}

// 共享 L2 快照键带部署前缀：Config.SnapshotL2KeyPrefix（kit：remote_entity.snapshot_l2_key_prefix）非空时全部 L2 读写删都落在
// "<prefix>:remote_entity:snapshot:…"，两个前缀不同的部署互不可见；为空（默认）时键形状不变。
// 历史：RR-20260927-17。

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
	backend, err := NewBackend(newRemoteTestLoader(), newBoundMongoCommitter(newRemoteMongoFake(), "control", 1000, 0))
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
	if err := asm.Manager.snapshots.cache.Publish(context.Background(), l2Envelope(key, 3, "wired")); err != nil {
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

// O-M6-3（docs/feature/MIRROR-M6-OBSERVATIONS-2026-10-06.md §3）：L2 写墓碑之后在同一连接上 WAIT 副本确认。
//
// 承诺：只有带版本删除（墓碑脚本）走 WAIT，普通快照写入不变；WAIT 的结果（确认、不足、没有副本、出错、
// 不支持）只计数与记日志，DeleteAtVersion 的返回值仍只由脚本决定——墓碑已写在主节点上，不回滚、不把删除
// 变成结果未知；副本数为 0 时不调用新能力；设置非法（负副本数、要等副本却没有正超时）在构造时拒绝。
// 真实 Redis 上的切主红绿见 snapshot_l2_tombstone_wait_integration_test.go。

// replicatedRedisFake 在 snapshotRedisFake 上加同连接 WAIT 能力，结果由测试指定。
type replicatedRedisFake struct {
	*snapshotRedisFake
	outcome rediscore.ReplicatedEvalResult
	calls   []string // 每次 EvalReplicated 的脚本（只记是不是墓碑脚本）
	waitFor []int
}

func (f *replicatedRedisFake) EvalReplicated(ctx context.Context, script string, keys []string, numReplicas int, _ time.Duration, args ...any) (rediscore.ReplicatedEvalResult, error) {
	name := "other"
	if script == remoteSnapshotL2DeleteAtVersion {
		name = "tombstone"
	}
	f.calls = append(f.calls, name)
	f.waitFor = append(f.waitFor, numReplicas)
	result, err := f.snapshotRedisFake.Eval(ctx, script, keys, args...)
	if err != nil {
		return rediscore.ReplicatedEvalResult{}, err
	}
	outcome := f.outcome
	outcome.Result = result
	return outcome, nil
}

func tombstoneWaitStore(t *testing.T, redis remoteSnapshotRedis, replicas int) *remoteSnapshotL2Store {
	t.Helper()
	cfg := DefaultConfig()
	cfg.SnapshotL2TTL = time.Minute
	cfg.SnapshotL2TombstoneWaitReplicas = replicas
	store, err := NewSnapshotL2StoreFromConfig(redis, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func TestSnapshotL2TombstoneWaitIsObservedButNeverFailsTheDelete(t *testing.T) {
	ctx := context.Background()
	key := staleBackfillKey(t, b2WatermarkKind, 9910)
	for _, tc := range []struct {
		name    string
		outcome rediscore.ReplicatedEvalResult
		want    SnapshotL2TombstoneWaitStats
	}{
		{"confirmed", rediscore.ReplicatedEvalResult{Waited: true, Replicas: 1}, SnapshotL2TombstoneWaitStats{Confirmed: 1}},
		{"short", rediscore.ReplicatedEvalResult{Waited: true, Replicas: 0}, SnapshotL2TombstoneWaitStats{Short: 1}},
		{"no replicas", rediscore.ReplicatedEvalResult{Skipped: rediscore.ReplicatedSkipNoReplicas}, SnapshotL2TombstoneWaitStats{NoReplicas: 1}},
		{"wait error", rediscore.ReplicatedEvalResult{WaitErr: errors.New("EOF")}, SnapshotL2TombstoneWaitStats{Failed: 1}},
		{"redirected", rediscore.ReplicatedEvalResult{Skipped: rediscore.ReplicatedSkipRedirected}, SnapshotL2TombstoneWaitStats{Skipped: 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			redis := &replicatedRedisFake{snapshotRedisFake: newSnapshotRedisFake(), outcome: tc.outcome}
			store := tombstoneWaitStore(t, redis, 1)
			if err := store.Set(ctx, staleBackfillEnvelope(key, 3, 1, "v3")); err != nil {
				t.Fatal(err)
			}
			if len(redis.calls) != 0 {
				t.Fatalf("a snapshot write went through WAIT: %v", redis.calls)
			}
			if err := store.DeleteAtVersion(ctx, key, 4); err != nil {
				t.Fatalf("DeleteAtVersion = %v; the tombstone is on the primary whatever WAIT says", err)
			}
			if len(redis.calls) != 1 || redis.calls[0] != "tombstone" || redis.waitFor[0] != 1 {
				t.Fatalf("WAIT calls = %v for %v replicas, want one tombstone call for 1", redis.calls, redis.waitFor)
			}
			if got := store.TombstoneWaitStats(); got != tc.want {
				t.Fatalf("stats = %+v, want %+v", got, tc.want)
			}
			if _, found, err := store.Get(ctx, key); err != nil || found {
				t.Fatalf("after the delete: found=%v err=%v, want the tombstone", found, err)
			}
			// 被更新快照拒绝的删除仍报 ErrStaleWrite（脚本决定返回值）。
			if err := store.Set(ctx, staleBackfillEnvelope(key, 6, 1, "v6")); err != nil {
				t.Fatal(err)
			}
			if err := store.DeleteAtVersion(ctx, key, 5); !errors.Is(err, cache.ErrStaleWrite) {
				t.Fatalf("delete older than the stored snapshot = %v, want cache.ErrStaleWrite", err)
			}
		})
	}
}

func TestSnapshotL2TombstoneWaitDisabledAndUnsupported(t *testing.T) {
	ctx := context.Background()
	key := staleBackfillKey(t, b2WatermarkKind, 9911)
	// 副本数 0：不调用新能力。
	redis := &replicatedRedisFake{snapshotRedisFake: newSnapshotRedisFake()}
	store := tombstoneWaitStore(t, redis, 0)
	if err := store.DeleteAtVersion(ctx, key, 2); err != nil || len(redis.calls) != 0 {
		t.Fatalf("disabled: err=%v WAIT calls=%v", err, redis.calls)
	}
	if got := store.TombstoneWaitStats(); got != (SnapshotL2TombstoneWaitStats{}) {
		t.Fatalf("disabled counted %+v", got)
	}
	// 客户端没有同连接 WAIT：普通发送，计为 skipped。
	plain := tombstoneWaitStore(t, newSnapshotRedisFake(), 1)
	if err := plain.DeleteAtVersion(ctx, key, 2); err != nil {
		t.Fatal(err)
	}
	if got := plain.TombstoneWaitStats(); got != (SnapshotL2TombstoneWaitStats{Skipped: 1}) {
		t.Fatalf("unsupported client counted %+v, want one skipped", got)
	}
	// 旧构造不等（行为与修前相同）。
	legacy := NewSnapshotL2Store(redis, time.Minute)
	if err := legacy.DeleteAtVersion(ctx, key, 3); err != nil || len(redis.calls) != 0 {
		t.Fatalf("legacy constructor: err=%v WAIT calls=%v", err, redis.calls)
	}
}

func TestSnapshotL2TombstoneWaitSettingsAreValidated(t *testing.T) {
	for _, tc := range []struct {
		replicas int
		timeout  time.Duration
		ok       bool
	}{
		{1, 50 * time.Millisecond, true},
		{0, 0, true},
		{-1, time.Second, false},
		{1, 0, false},
		{1, time.Second, true},
		{1, 2 * time.Second, false},
	} {
		cfg := DefaultConfig()
		cfg.SnapshotL2TombstoneWaitReplicas, cfg.SnapshotL2TombstoneWaitTimeout = tc.replicas, tc.timeout
		_, err := NewSnapshotL2StoreFromConfig(newSnapshotRedisFake(), cfg)
		if (err == nil) != tc.ok {
			t.Errorf("replicas=%d timeout=%v: err=%v, want ok=%v", tc.replicas, tc.timeout, err, tc.ok)
		}
	}
	if _, err := Assemble(AssemblyDeps{Redis: refreshRedis{l2: newSnapshotRedisFake()}, Backend: &atomicTestBackend{newRemoteTestLoader()}},
		&Config{SnapshotL2TombstoneWaitReplicas: 1}, 7, MongoBackendConfig{}); err == nil {
		t.Error("Assemble accepted a tombstone wait without a timeout")
	}
}
