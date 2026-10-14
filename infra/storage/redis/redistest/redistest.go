// Package redistest 让写真实 Redis 的测试在结束时删掉自己写的键。
//
// 真实 Redis 用例靠“每次运行唯一的键前缀”（含纳秒时间戳）彼此隔离。唯一前缀只保证互不影响，
// 不保证不留垃圾：2026-10-14 照 CI service-redis job 跑完一轮后，私有 Redis 上剩约 2000 个键，
// 这组用例因此不能指向别人也在用的 Redis。这里给每个唯一前缀注册清理，用例结束时删掉键名含该前缀
// 的键；不用 FLUSHALL / FLUSHDB，同一个 Redis 上不属于本测试的数据不动。
//
// 回退由 .github/workflows/ci.yml 的 service-redis job 末尾的残留键检查兜住（跑完整组后 Redis
// 必须为空），根包 ci_service_redis_test.go 钉住那一步。
package redistest

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"
)

// Prefix 返回 "<name>:<纳秒>" 形式的唯一前缀，并注册 Cleanup：用例结束时删掉 addrs 上键名含这个前缀的键。
// addrs 为空（Memory 变体）时不注册清理。
func Prefix(t testing.TB, addrs, name string) string {
	t.Helper()
	prefix := fmt.Sprintf("%s:%d", name, time.Now().UnixNano())
	Cleanup(t, addrs, prefix)
	return prefix
}

// Cleanup 为调用方自己生成的唯一前缀注册清理；prefix 必须只属于本测试（含纳秒时间戳或随机串），
// 否则会删到别人的键。
//
// 时序：t.Cleanup 后进先出，在拿到前缀之后才建的 Service / Mod 及其 Stop 清理先执行，停下后不再写，
// 所以应在建存储之前调用。清理失败只记 t.Logf，不让测试失败：残留由 CI 的残留键检查发现，清理自身的
// 故障不应把被测承诺的结果变红或掩盖它。
func Cleanup(t testing.TB, addrs, prefix string) {
	t.Helper()
	if strings.TrimSpace(addrs) == "" || prefix == "" {
		return
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if deleted, err := DeleteKeys(ctx, addrs, prefix); err != nil {
			t.Logf("redistest: cleanup of keys containing %q on %s stopped after %d deletions: %v", prefix, addrs, deleted, err)
		}
	})
}

// DeleteKeys 删掉 addrs 上键名含 prefix 的全部键，返回删除的键数。
//
// addrs 是逗号分隔的地址列表。多个地址、或唯一那个实例开着 cluster 时按集群处理：SCAN 只遍历所连
// 节点自己的槽，所以在每个主节点上分别 SCAN，并在该节点上逐键 DEL（同节点的键也可能分属不同槽，
// 多键 DEL 会 CROSSSLOT）。匹配式是 "*<prefix>*" 而不是 "<prefix>*"：有的存储把前缀包进 hash tag，
// 有的直接拼接业务名不加分隔符。
func DeleteKeys(ctx context.Context, addrs, prefix string) (int, error) {
	if prefix == "" {
		return 0, fmt.Errorf("redistest: empty prefix would match every key")
	}
	list := strings.Split(addrs, ",")
	for i := range list {
		list[i] = strings.TrimSpace(list[i])
	}
	list = slices.DeleteFunc(list, func(s string) bool { return s == "" })
	if len(list) == 0 {
		return 0, fmt.Errorf("redistest: no address")
	}
	pattern := "*" + escapeGlob(prefix) + "*"
	clustered := len(list) > 1
	if !clustered {
		single := goredis.NewClient(&goredis.Options{Addr: list[0]})
		info, err := single.Info(ctx, "cluster").Result()
		if err != nil {
			_ = single.Close()
			return 0, fmt.Errorf("probe %s: %w", list[0], err)
		}
		if !strings.Contains(info, "cluster_enabled:1") {
			defer single.Close()
			return deleteOnNode(ctx, single, pattern)
		}
		_ = single.Close()
	}
	cluster := goredis.NewClusterClient(&goredis.ClusterOptions{Addrs: list})
	defer cluster.Close()
	var deleted atomic.Int64
	err := cluster.ForEachMaster(ctx, func(ctx context.Context, node *goredis.Client) error {
		n, err := deleteOnNode(ctx, node, pattern)
		deleted.Add(int64(n))
		return err
	})
	return int(deleted.Load()), err
}

// deleteOnNode 在一个节点上 SCAN 出匹配的键，再按单键 DEL 管道删除（单键命令在集群节点上不会 CROSSSLOT）。
// 先收齐再删，避免边删边 SCAN 时游标语义带来的遗漏。
func deleteOnNode(ctx context.Context, node *goredis.Client, pattern string) (int, error) {
	var keys []string
	iter := node.Scan(ctx, 0, pattern, 512).Iterator()
	for iter.Next(ctx) {
		keys = append(keys, iter.Val())
	}
	if err := iter.Err(); err != nil {
		return 0, fmt.Errorf("scan %s on %s: %w", pattern, node.Options().Addr, err)
	}
	deleted := 0
	for chunk := range slices.Chunk(keys, 256) {
		_, err := node.Pipelined(ctx, func(pipe goredis.Pipeliner) error {
			for _, key := range chunk {
				pipe.Del(ctx, key)
			}
			return nil
		})
		if err != nil {
			return deleted, fmt.Errorf("delete on %s: %w", node.Options().Addr, err)
		}
		deleted += len(chunk)
	}
	return deleted, nil
}

// escapeGlob 转义 SCAN MATCH 的通配字符，让前缀里的 {、[ 等按字面匹配。
func escapeGlob(s string) string {
	return strings.NewReplacer(`\`, `\\`, "*", `\*`, "?", `\?`, "[", `\[`, "]", `\]`).Replace(s)
}
