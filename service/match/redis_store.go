package match

import (
	"fmt"
	"strings"

	"github.com/tjbdwanghaibo/roost-core/infra/storage/versionstore"
)

// NewRedisStore builds a queue Store over Redis.
//
// No storage logic: each queue partition is one versioned entry under
// RedisKey(prefix, queue), which is what makes a group commit a single
// compare-and-set, and versionstore.RedisStore already implements that
// contract. A cross-partition commit (CommitAcross) is one multi-key
// compare-and-set over the partitions involved (versionstore.MultiUpdater).
//
// One key per partition is deliberate and is the cost of that design:
// throughput on a single partition is bounded by contention on one key. Two
// things keep that bound out of the way. Partitioning a large queue (by rank
// band, region or sid group) splits it into several keys; and
// servicerpc.KeyAffinityPicker routes a partition's traffic to one replica, so
// the contention is in-process rather than cross-replica. Round-robin routing
// over a shared key is the design cause of the contention the implementation
// this replaces suffered, not a tuning problem.
//
// 键名 "<prefix>:queue:{<Mode>:<GroupSize>}:<Partition>"：花括号里的逻辑队列是 Redis Cluster 的 hash tag，
// 同一逻辑队列的分区同槽，CommitAcross 的多键脚本才能执行。prefix 自己也可以带 hash tag（那样整个匹配服务
// 落在一个 slot），但不能让第一对花括号为空：Redis 遇到空的第一对花括号会对整个键求槽，同一逻辑队列的分区
// 就会散到不同 slot，这里在构造时拒绝。
//
// It returns a ready Store rather than the state store, because the state's
// value type is unexported: handing back a versionstore.Store over an
// unnameable type would be a value a caller can hold and cannot declare.
func NewRedisStore(client versionstore.RedisClient, prefix string, cfg Config) (Store, error) {
	if strings.TrimSpace(prefix) == "" {
		return nil, fmt.Errorf("match: redis key prefix is required")
	}
	probe := Queue{Mode: "m", GroupSize: 2}
	east, west := probe, probe
	east.Partition, west.Partition = "east", "west"
	if clusterHashTag(RedisKey(prefix, east)) != clusterHashTag(RedisKey(prefix, west)) {
		return nil, fmt.Errorf("match: redis key prefix %q puts the partitions of one logical queue in different Redis Cluster "+
			"slots (its first hash tag is empty: {}); remove the braces or give them content", prefix)
	}
	state, err := versionstore.NewRedisStore(client, versionstore.RedisConfig[string, queueState]{
		Prefix: redisQueuePrefix(prefix),
		KeyOf:  func(key string) string { return key },
		Codec:  versionstore.JSONCodec[queueState]{},
	})
	if err != nil {
		return nil, fmt.Errorf("match: queue state: %w", err)
	}
	return NewStore(state, cfg)
}

// RedisKey 是 queue 的状态在 NewRedisStore(prefix) 下的 Redis 键："<prefix>:queue:" + queue.Key()。
// 运维排查或测试核对键与槽时用它，不要自己拼。
func RedisKey(prefix string, queue Queue) string { return redisQueuePrefix(prefix) + queue.Key() }

func redisQueuePrefix(prefix string) string { return prefix + ":queue:" }

// clusterHashTag 返回 Redis Cluster 对 key 求槽时实际哈希的部分：第一个 '{' 与其后第一个 '}' 之间的内容；
// 没有 '{'、其后没有 '}'、或两者之间为空时是整个键（Redis Cluster 规范 “Hash tags” 一节）。
func clusterHashTag(key string) string {
	open := strings.IndexByte(key, '{')
	if open < 0 {
		return key
	}
	closing := strings.IndexByte(key[open+1:], '}')
	if closing <= 0 {
		return key
	}
	return key[open+1 : open+1+closing]
}
