package redis

import (
	"testing"
	"time"

	fredis "github.com/tjbdwanghaibo/roost-core/redis"

	goredis "github.com/redis/go-redis/v9"
)

// The caller's context deadline must reach the wire. go-redis only honours it
// with ContextTimeoutEnabled; without that flag a 500ms Acquire budget waits
// for the full ReadTimeout under latency (seen against toxiproxy).
func TestRedisClientsHonourContextDeadlinesOnTheWire(t *testing.T) {
	single := newRedisClient(&fredis.Config{Addr: "127.0.0.1:1", ReadTimeout: 2 * time.Second})
	client, ok := single.rdb.(*goredis.Client)
	if !ok {
		t.Fatalf("single-node client is %T", single.rdb)
	}
	if !client.Options().ContextTimeoutEnabled {
		t.Fatal("single-node client does not honour context deadlines")
	}
	cluster := newRedisClient(&fredis.Config{ClusterAddrs: []string{"127.0.0.1:1"}, ReadTimeout: 2 * time.Second})
	clusterClient, ok := cluster.rdb.(*goredis.ClusterClient)
	if !ok {
		t.Fatalf("cluster client is %T", cluster.rdb)
	}
	if !clusterClient.Options().ContextTimeoutEnabled {
		t.Fatal("cluster client does not honour context deadlines")
	}
}
