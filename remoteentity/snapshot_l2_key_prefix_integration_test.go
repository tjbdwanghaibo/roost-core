//go:build integration

package remoteentity

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"
	"github.com/tjbdwanghaibo/roost-core/cache"
	"github.com/tjbdwanghaibo/roost-core/entity"
	fredis "github.com/tjbdwanghaibo/roost-core/redis"
	redisdriver "github.com/tjbdwanghaibo/roost-core/redis/driver"
)

// RR-20260927-17 后续验证（B27 第 4 批）：L2 快照键前缀在真实 Redis 上的表现。修复记录的回归只用 snapshotRedisFake；
// 这里 CAS / DeleteAtVersion 的 Lua 由真实 Redis 执行，用原生命令核对：配置前缀后 Get / Set(CAS) / Delete / DeleteAtVersion
// 触到的键全部是 "<prefix>:remote_entity:snapshot:…"，缺省时逐字是旧格式。单机用例以 ROOST_DATAENGINE_IT 准入；
// Cluster 用例以 ROOST_REVIEW_CLUSTER（外部集群地址）准入，没有外部集群时 ROOST_REMOTE_CLUSTER_IT=1 用本包
// newRemoteTestCluster 自起一个（scripts/test-remote-matrix.sh 的做法）。键前缀 b27b4:<pid>:<ts>:…，结束逐键删除。

// b27L2Prefix 是本批用例的部署前缀（不带空白、不带 hash tag，能过 ValidateSnapshotL2KeyPrefix）。
func b27L2Prefix(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("b27b4:%d:%d:%s", os.Getpid(), time.Now().UnixNano(), strings.ReplaceAll(t.Name(), "/", "_"))
}

// b27L2EntityKey 给每个用例一个进程内唯一的实体：pid 与纳秒时间戳拼进 52 位 unique id，
// 缺省（无前缀）键因此也不会与同一 Redis 上其他会话的键相撞。
func b27L2EntityKey(t *testing.T, serial int64) entity.RemoteSnapshotKey {
	t.Helper()
	raw := (int64(os.Getpid())&0xffff)<<32 | (time.Now().UnixNano()>>8)&0xffffff00 | (serial & 0xff)
	return l2TestKey(t, raw)
}

func b27Exists(t *testing.T, r fredis.IRedis, key string) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	n, err := r.Exists(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	return n == 1
}

func b27HashField(t *testing.T, r fredis.IRedis, key, field string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	v, err := r.HGet(ctx, key, field)
	if errors.Is(err, fredis.ErrNil) {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(v)
}

// b27ScanKeys 用 SCAN 列出 pattern 下的全部键（单机用）。
func b27ScanKeys(t *testing.T, r fredis.IRedis, pattern string) []string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var keys []string
	cursor := "0"
	for {
		v, err := r.Eval(ctx, `return redis.call("SCAN", ARGV[1], "MATCH", ARGV[2], "COUNT", 1000)`, nil, cursor, pattern)
		if err != nil {
			t.Fatal(err)
		}
		reply, ok := v.([]any)
		if !ok || len(reply) != 2 {
			t.Fatalf("unexpected SCAN reply %#v", v)
		}
		for _, k := range reply[1].([]any) {
			keys = append(keys, fmt.Sprint(k))
		}
		if cursor = fmt.Sprint(reply[0]); cursor == "0" {
			return keys
		}
	}
}

func b27DeleteKeys(t *testing.T, r fredis.IRedis, keys ...string) {
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		for _, key := range keys {
			if _, err := r.Del(ctx, key); err != nil {
				t.Errorf("cleanup %s: %v", key, err)
			}
		}
	})
}

// 单机：配置前缀后四个操作只触前缀键；缺省时键逐字是旧格式，且与前缀部署互不可见。
func TestRealSnapshotL2KeyPrefixOnRedis(t *testing.T) {
	r := realRemoteRedis(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	prefix := b27L2Prefix(t)
	key := b27L2EntityKey(t, 1)
	legacyKey := fmt.Sprintf("remote_entity:snapshot:%d:%d:%d:%d:%d", key.Tenant, key.Kind, key.EntityID, key.Scope, key.Policy)
	prefixedKey := prefix + ":" + legacyKey
	b27DeleteKeys(t, r, legacyKey, prefixedKey)

	store, err := NewSnapshotL2StoreWithKeyPrefix(r, time.Minute, prefix)
	if err != nil {
		t.Fatal(err)
	}
	legacy := NewSnapshotL2Store(r, time.Minute)

	// Set（真实 CAS Lua）落在前缀键上，且是该前缀下唯一的键；缺省键始终不出现。
	if err := store.Set(ctx, l2Envelope(key, 5, "a")); err != nil {
		t.Fatal(err)
	}
	if got := b27ScanKeys(t, r, prefix+":*"); len(got) != 1 || got[0] != prefixedKey {
		t.Fatalf("keys under the prefix = %v, want exactly %q", got, prefixedKey)
	}
	if b27Exists(t, r, legacyKey) {
		t.Fatalf("prefixed Set wrote the unprefixed key %q", legacyKey)
	}
	if v := b27HashField(t, r, prefixedKey, "version"); v != "5" {
		t.Fatalf("real key holds version %q, want 5", v)
	}
	if ttl := b27RedisPTTL(t, r, prefixedKey); ttl <= 0 || ttl > time.Minute.Milliseconds() {
		t.Fatalf("PTTL=%dms, want within the store TTL", ttl)
	}
	if got, ok, err := store.Get(ctx, key); err != nil || !ok || string(got.Payload.BytesCopy()) != "a" {
		t.Fatalf("prefixed Get: ok=%v err=%v payload=%q", ok, err, got.Payload.BytesCopy())
	}
	if _, ok, err := legacy.Get(ctx, key); err != nil || ok {
		t.Fatalf("unprefixed store sees the prefixed snapshot: ok=%v err=%v", ok, err)
	}
	// 真实 Lua 的 CAS 分支：更低版本被拒（键不变，RR-20261005-NC-130 起报 ErrStaleWrite）、同版本不同内容冲突。
	if err := store.Set(ctx, l2Envelope(key, 3, "stale")); !errors.Is(err, cache.ErrStaleWrite) {
		t.Fatalf("lower version: err=%v, want cache.ErrStaleWrite", err)
	}
	if v := b27HashField(t, r, prefixedKey, "version"); v != "5" {
		t.Fatalf("lower version overwrote the real key: version=%q", v)
	}
	if err := store.Set(ctx, l2Envelope(key, 5, "b")); !errors.Is(err, entity.ErrRemoteVersionConflict) {
		t.Fatalf("same version different content: err=%v", err)
	}
	// DeleteAtVersion 的 Lua 只看前缀键：更低版本不删、相等版本删。
	if err := store.DeleteAtVersion(ctx, key, 4); err != nil {
		t.Fatal(err)
	}
	if !b27Exists(t, r, prefixedKey) {
		t.Fatal("DeleteAtVersion(4) removed a version-5 snapshot")
	}
	if err := store.DeleteAtVersion(ctx, key, 5); err != nil {
		t.Fatal(err)
	}
	if b27Exists(t, r, prefixedKey) {
		t.Fatal("DeleteAtVersion(5) left the prefixed key")
	}
	// Delete 同样只触前缀键。
	if err := store.Set(ctx, l2Envelope(key, 6, "c")); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(ctx, key); err != nil {
		t.Fatal(err)
	}
	if b27Exists(t, r, prefixedKey) || len(b27ScanKeys(t, r, prefix+":*")) != 0 {
		t.Fatal("Delete left keys under the prefix")
	}
	if b27Exists(t, r, legacyKey) {
		t.Fatalf("some prefixed operation touched the unprefixed key %q", legacyKey)
	}

	// 缺省：键逐字是旧格式；空前缀构造的 store 与 NewSnapshotL2Store 互读；前缀部署看不见它。
	if err := legacy.Set(ctx, l2Envelope(key, 1, "legacy")); err != nil {
		t.Fatal(err)
	}
	if !b27Exists(t, r, legacyKey) {
		t.Fatalf("default store did not write the verbatim legacy key %q", legacyKey)
	}
	if len(b27ScanKeys(t, r, prefix+":*")) != 0 {
		t.Fatal("default store wrote under the deployment prefix")
	}
	empty, err := NewSnapshotL2StoreWithKeyPrefix(r, time.Minute, "")
	if err != nil {
		t.Fatal(err)
	}
	if got, ok, err := empty.Get(ctx, key); err != nil || !ok || string(got.Payload.BytesCopy()) != "legacy" {
		t.Fatalf("empty-prefix store does not read the legacy key: ok=%v err=%v", ok, err)
	}
	if _, ok, _ := store.Get(ctx, key); ok {
		t.Fatal("prefixed deployment sees the unprefixed snapshot")
	}
	if err := legacy.DeleteAtVersion(ctx, key, 1); err != nil {
		t.Fatal(err)
	}
	if b27Exists(t, r, legacyKey) {
		t.Fatal("legacy DeleteAtVersion left the legacy key")
	}
}

// b27ClusterAddresses 返回可用的 Redis Cluster 地址：优先 ROOST_REVIEW_CLUSTER（外部集群），
// 否则 ROOST_REMOTE_CLUSTER_IT=1 时用本包 harness 自起 3 主 3 从；都没有则跳过。
func b27ClusterAddresses(t *testing.T) []string {
	t.Helper()
	if addresses := os.Getenv("ROOST_REVIEW_CLUSTER"); addresses != "" {
		return strings.Split(addresses, ",")
	}
	if os.Getenv("ROOST_REMOTE_CLUSTER_IT") != "1" {
		t.Skip("set ROOST_REVIEW_CLUSTER=<addrs> or ROOST_REMOTE_CLUSTER_IT=1")
	}
	_, addresses := newRemoteTestCluster(t)
	return addresses
}

// Cluster：无 hash tag 的前缀让快照键像缺省一样散在多个槽主上，四个单键脚本无 CROSSSLOT；
// 带 hash tag 的前缀会把全部快照键钉在一个槽主上——这正是 ValidateSnapshotL2KeyPrefix 拒绝它的原因（用绕过校验的 store 实证）。
func TestRealSnapshotL2KeyPrefixOnRedisCluster(t *testing.T) {
	addresses := b27ClusterAddresses(t)
	cfg := fredis.DefaultConfig("")
	cfg.ClusterAddrs = addresses
	client, err := redisdriver.NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	raw := goredis.NewClusterClient(&goredis.ClusterOptions{Addrs: addresses})
	t.Cleanup(func() { _ = raw.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	slots, err := raw.ClusterSlots(ctx).Result()
	if err != nil {
		t.Fatal(err)
	}
	ownerOf := func(key string) string {
		slot, err := raw.ClusterKeySlot(ctx, key).Result()
		if err != nil {
			t.Fatal(err)
		}
		for _, partition := range slots {
			if slot >= int64(partition.Start) && slot <= int64(partition.End) {
				return partition.Nodes[0].Addr
			}
		}
		t.Fatalf("slot %d of %q has no owner", slot, key)
		return ""
	}
	const entities = 16
	keys := make([]entity.RemoteSnapshotKey, entities)
	for i := range keys {
		keys[i] = b27L2EntityKey(t, int64(i+1))
	}
	base := b27L2Prefix(t)
	for _, tc := range []struct {
		name, prefix string
		tagged       bool
	}{
		{name: "untagged", prefix: base},
		{name: "hash_tagged", prefix: "{" + base + "}", tagged: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := ValidateSnapshotL2KeyPrefix(tc.prefix); (err != nil) != tc.tagged {
				t.Fatalf("ValidateSnapshotL2KeyPrefix(%q) = %v, want rejected=%v", tc.prefix, err, tc.tagged)
			}
			var store *remoteSnapshotL2Store
			if tc.tagged {
				// 绕过校验的对照组：实证 hash tag 前缀会把全部快照键钉在一个槽主上。
				store = &remoteSnapshotL2Store{redis: client, ttl: time.Minute, keyPrefix: tc.prefix}
			} else {
				s, err := NewSnapshotL2StoreWithKeyPrefix(client, time.Minute, tc.prefix)
				if err != nil {
					t.Fatal(err)
				}
				store = s
			}
			realKeys := make([]string, entities)
			for i, key := range keys {
				realKeys[i] = store.key(key)
			}
			b27DeleteKeys(t, client, realKeys...)
			owners := map[string]bool{}
			for i, key := range keys {
				if err := store.Set(ctx, l2Envelope(key, 5, fmt.Sprintf("v%d", i))); err != nil {
					t.Fatalf("Set %d: %v", i, err)
				}
				if !strings.HasPrefix(realKeys[i], tc.prefix+":remote_entity:snapshot:") {
					t.Fatalf("key %q is not under the prefix", realKeys[i])
				}
				if n, err := client.Exists(ctx, realKeys[i]); err != nil || n != 1 {
					t.Fatalf("real key %q after Set: exists=%d err=%v", realKeys[i], n, err)
				}
				owners[ownerOf(realKeys[i])] = true
				if got, ok, err := store.Get(ctx, key); err != nil || !ok || string(got.Payload.BytesCopy()) != fmt.Sprintf("v%d", i) {
					t.Fatalf("Get %d: ok=%v err=%v", i, ok, err)
				}
				if err := store.Set(ctx, l2Envelope(key, 3, "stale")); !errors.Is(err, cache.ErrStaleWrite) {
					t.Fatalf("stale Set %d: %v, want cache.ErrStaleWrite", i, err)
				}
				if v := b27HashField(t, client, realKeys[i], "version"); v != "5" {
					t.Fatalf("stale Set %d overwrote version: %q", i, v)
				}
				if err := store.DeleteAtVersion(ctx, key, 4); err != nil {
					t.Fatalf("DeleteAtVersion(4) %d: %v", i, err)
				}
				if n, _ := client.Exists(ctx, realKeys[i]); n != 1 {
					t.Fatalf("DeleteAtVersion(4) removed key %d", i)
				}
				if err := store.DeleteAtVersion(ctx, key, 5); err != nil {
					t.Fatalf("DeleteAtVersion(5) %d: %v", i, err)
				}
				if n, _ := client.Exists(ctx, realKeys[i]); n != 0 {
					t.Fatalf("DeleteAtVersion(5) left key %d", i)
				}
				if err := store.Set(ctx, l2Envelope(key, 6, "again")); err != nil {
					t.Fatalf("second Set %d: %v", i, err)
				}
				if err := store.Delete(ctx, key); err != nil {
					t.Fatalf("Delete %d: %v", i, err)
				}
				if n, _ := client.Exists(ctx, realKeys[i]); n != 0 {
					t.Fatalf("Delete left key %d", i)
				}
			}
			if tc.tagged && len(owners) != 1 {
				t.Fatalf("hash-tagged prefix %q: %d snapshot keys landed on %d slot owners, want all on one (the reason the prefix is rejected)", tc.prefix, entities, len(owners))
			}
			if !tc.tagged && len(owners) < 2 {
				t.Fatalf("prefix %q: %d snapshot keys landed on %d slot owners, want them spread like the default keys", tc.prefix, entities, len(owners))
			}
			t.Logf("prefix=%q keys=%d owners=%d", tc.prefix, entities, len(owners))
		})
	}
}
