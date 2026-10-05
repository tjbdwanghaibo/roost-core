package redis

import (
	"strings"
	"testing"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/kit/mods"
)

// RR-20261005-NC-190：redis.cluster_addrs 写成 YAML 列表时同样是 Cluster，逗号后的空格不进地址。
// 旧行为：列表读成空串，Redis Mod 退回 localhost:6379 单机、服务 Mod 的 Cluster hash tag 校验被
// 跳过；`a:1, b:2` 拆出带前导空格的 " b:2"。
func TestClusterAddrsAcceptAYAMLListAndTrimEntries(t *testing.T) {
	for _, text := range []string{"a:1,b:2", "a:1, b:2", "[a:1, b:2]", "\n    - a:1\n    - b:2"} {
		cfg := viper.New()
		cfg.SetConfigType("yaml")
		if err := cfg.ReadConfig(strings.NewReader("redis:\n  cluster_addrs: " + text + "\n")); err != nil {
			t.Fatal(err)
		}
		conn := redisConfig(cfg)
		if !conn.IsCluster() || len(conn.ClusterAddrs) != 2 || conn.ClusterAddrs[0] != "a:1" || conn.ClusterAddrs[1] != "b:2" {
			t.Fatalf("cluster_addrs: %s -> cluster=%v addrs=%q, want Cluster [a:1 b:2]", text, conn.IsCluster(), conn.ClusterAddrs)
		}
		if err := mods.ValidateClusterKeyPrefix(cfg, "mail", "roost:mail"); err == nil {
			t.Fatalf("cluster_addrs: %s: a key prefix without a hash tag passed the Cluster check", text)
		}
	}
}
