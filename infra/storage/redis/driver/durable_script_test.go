package driver

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"
	fredis "github.com/tjbdwanghaibo/roost-core/infra/storage/redis"
	"github.com/tjbdwanghaibo/roost-core/infra/storage/redis/redistest"
)

func scriptCall(source string, keys []string, args ...any) fredis.ScriptCall {
	sum := sha1.Sum([]byte(source))
	return fredis.ScriptCall{SHA: hex.EncodeToString(sum[:]), Source: source, Keys: keys, Args: args}
}

func newAOFClient(t *testing.T) (*Client, *redistest.AOFServer) {
	t.Helper()
	server := redistest.StartAOF(t)
	client := NewRedisClient(fredis.DefaultConfig(server.Addr))
	t.Cleanup(func() { _ = client.Close() })
	return client, server
}

// DurableScriptBatch：一批里混合不同脚本，各自的结果按调用顺序给出，WAITAOF 覆盖整批；某条的错误回复只属于那一条。
// 服务端没有缓存脚本（NOSCRIPT）时，只把这些调用用原文在同一连接重发——每个脚本恰好执行一次。
func TestDurableScriptBatchMixesScriptsAndResendsOnNoScript(t *testing.T) {
	ctx := context.Background()
	client, _ := newAOFClient(t)
	incr := scriptCall("return redis.call('INCR', KEYS[1])", []string{"batch:counter"})
	set := scriptCall("redis.call('SET', KEYS[1], ARGV[1]); return 'set'", []string{"batch:value"}, "v1")
	broken := scriptCall("return redis.call('NOT_A_COMMAND')", nil)
	if err := client.Raw().ScriptFlush(ctx).Err(); err != nil {
		t.Fatal(err)
	}
	results, local, _, err := client.DurableScriptBatch(ctx, []fredis.ScriptCall{incr, set, broken, incr}, 1, 0, time.Second)
	if err != nil || local != 1 {
		t.Fatalf("batch = %v local=%d", err, local)
	}
	if results[0].Value != int64(1) || results[1].Value != "set" || results[2].Err == nil || results[3].Value != int64(2) {
		t.Fatalf("results = %+v; want 1, set, an error, 2", results)
	}
	if got, err := client.Raw().Get(ctx, "batch:counter").Int(); err != nil || got != 2 {
		t.Fatalf("counter = %d, %v; each INCR call must run exactly once across the NOSCRIPT resend", got, err)
	}
	// 脚本已在缓存里：一轮 EVALSHA 完成。
	results, _, _, err = client.DurableScriptBatch(ctx, []fredis.ScriptCall{incr}, 1, 0, time.Second)
	if err != nil || results[0].Value != int64(3) {
		t.Fatalf("cached batch = %+v, %v", results, err)
	}
}

// WAITAOF 被服务端拒绝（appendonly 被关）时：脚本已执行、结果有效，错误是 ErrWaitAOFFailed，不是“结果未知”。
func TestDurableScriptBatchReportsARejectedWaitAOF(t *testing.T) {
	ctx := context.Background()
	client, _ := newAOFClient(t)
	if err := client.Raw().ConfigSet(ctx, "appendonly", "no").Err(); err != nil {
		t.Fatal(err)
	}
	results, local, _, err := client.DurableScriptBatch(ctx, []fredis.ScriptCall{scriptCall("return redis.call('INCR', KEYS[1])", []string{"aof:counter"})}, 1, 0, time.Second)
	if !errors.Is(err, fredis.ErrWaitAOFFailed) || local != 0 || len(results) != 1 || results[0].Value != int64(1) {
		t.Fatalf("batch with appendonly off = %+v local=%d err=%v; want the script result and ErrWaitAOFFailed", results, local, err)
	}
	if got, _ := client.Raw().Get(ctx, "aof:counter").Int(); got != 1 {
		t.Fatalf("counter = %d, the script executed once", got)
	}
}

func TestDurableScriptBatchRefusesIncompleteCalls(t *testing.T) {
	client := NewRedisClient(fredis.DefaultConfig("127.0.0.1:1"))
	defer client.Close()
	if _, _, _, err := client.DurableScriptBatch(context.Background(), []fredis.ScriptCall{{Source: "return 1"}}, 1, 0, time.Second); err == nil {
		t.Fatal("a call without SHA was accepted")
	}
	cluster := NewRedisClient(&fredis.Config{ClusterAddrs: []string{"127.0.0.1:1"}})
	defer cluster.Close()
	if _, _, _, err := cluster.DurableScriptBatch(context.Background(), []fredis.ScriptCall{scriptCall("return 1", nil)}, 1, 0, time.Second); err == nil {
		t.Fatal("a cluster client accepted a same-connection WAITAOF batch")
	}
}

func TestStreamReadsAndHashFields(t *testing.T) {
	ctx := context.Background()
	client, _ := newAOFClient(t)
	raw := client.Raw()
	first, err := raw.XAdd(ctx, &goredis.XAddArgs{Stream: "log", Values: map[string]any{"id": "a", "v": "1"}}).Result()
	if err != nil {
		t.Fatal(err)
	}
	second, err := raw.XAdd(ctx, &goredis.XAddArgs{Stream: "log", Values: map[string]any{"id": "b", "v": "2"}}).Result()
	if err != nil {
		t.Fatal(err)
	}
	if n, err := client.XLen(ctx, "log"); err != nil || n != 2 {
		t.Fatalf("XLen = %d, %v", n, err)
	}
	all, err := client.XRangeAfter(ctx, "log", "0-0", 10)
	if err != nil || len(all) != 2 || all[0].ID != first || all[1].Fields["id"] != "b" {
		t.Fatalf("XRangeAfter(0-0) = %+v, %v", all, err)
	}
	after, err := client.XRangeAfter(ctx, "log", first, 10)
	if err != nil || len(after) != 1 || after[0].ID != second {
		t.Fatalf("XRangeAfter(first) = %+v, %v; want only the second entry", after, err)
	}
	empty, err := client.XReadAfter(ctx, "log", second, 10, 20*time.Millisecond)
	if err != nil || len(empty) != 0 {
		t.Fatalf("XReadAfter past the end = %+v, %v; want an empty timeout", empty, err)
	}
	go func() {
		time.Sleep(20 * time.Millisecond)
		_ = raw.XAdd(context.Background(), &goredis.XAddArgs{Stream: "log", Values: map[string]any{"id": "c"}}).Err()
	}()
	woke, err := client.XReadAfter(ctx, "log", second, 10, 2*time.Second)
	if err != nil || len(woke) != 1 || woke[0].Fields["id"] != "c" {
		t.Fatalf("blocking XReadAfter = %+v, %v; want the new entry", woke, err)
	}
	if err := raw.HSet(ctx, "h", "a", "1", "b", "2").Err(); err != nil {
		t.Fatal(err)
	}
	values, err := client.HMGet(ctx, "h", "a", "missing", "b")
	if err != nil || string(values[0]) != "1" || values[1] != nil || string(values[2]) != "2" {
		t.Fatalf("HMGet = %q, %v", values, err)
	}
	if values, err := client.HMGet(ctx, "absent", "a"); err != nil || len(values) != 1 || values[0] != nil {
		t.Fatalf("HMGet on a missing key = %q, %v", values, err)
	}
}

// CheckAuthority 是权威 Redis 的启动探针：持久配置、淘汰策略、角色任何一项不符都拒绝，满足时探针写经 WAITAOF 确认。
func TestCheckAuthorityRefusesBrokenDeployments(t *testing.T) {
	ctx := context.Background()
	client, _ := newAOFClient(t)
	raw := client.Raw()
	if err := fredis.CheckAuthority(ctx, client, "authority:probe", time.Second); err != nil {
		t.Fatalf("a conforming instance was refused: %v", err)
	}
	for _, c := range []struct{ name, value, restore string }{
		{"appendfsync", "everysec", "always"},
		{"maxmemory-policy", "allkeys-lru", "noeviction"},
		{"no-appendfsync-on-rewrite", "yes", "no"},
	} {
		if err := raw.ConfigSet(ctx, c.name, c.value).Err(); err != nil {
			t.Fatal(err)
		}
		err := fredis.CheckAuthority(ctx, client, "authority:probe", time.Second)
		if !errors.Is(err, fredis.ErrAuthorityContract) {
			t.Fatalf("%s=%s accepted: %v", c.name, c.value, err)
		}
		if err := raw.ConfigSet(ctx, c.name, c.restore).Err(); err != nil {
			t.Fatal(err)
		}
	}
	if err := raw.Del(ctx, "authority:probe").Err(); err != nil {
		t.Fatal(err)
	}
	if err := raw.RPush(ctx, "authority:probe", "x").Err(); err != nil {
		t.Fatal(err)
	}
	if err := fredis.CheckAuthority(ctx, client, "authority:probe", time.Second); !errors.Is(err, fredis.ErrAuthorityContract) {
		t.Fatalf("a wrong-type probe key was accepted: %v", err)
	}
	// 副本（role:slave）上不能做权威写。
	if err := raw.Do(ctx, "REPLICAOF", "127.0.0.1", "1").Err(); err != nil {
		t.Fatal(err)
	}
	defer raw.Do(ctx, "REPLICAOF", "NO", "ONE")
	if err := fredis.CheckAuthority(ctx, client, "authority:probe2", time.Second); !errors.Is(err, fredis.ErrAuthorityContract) {
		t.Fatalf("a replica was accepted: %v", err)
	}
}
