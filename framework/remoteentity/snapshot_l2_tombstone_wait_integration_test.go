//go:build integration

package remoteentity

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	fredis "github.com/tjbdwanghaibo/roost-core/infra/storage/redis"
	redisdriver "github.com/tjbdwanghaibo/roost-core/infra/storage/redis/driver"
)

// O-M6-3 的真实 Redis 红绿（docs/feature/MIRROR-M6-OBSERVATIONS-2026-10-06.md §3、§6）。只在
// scripts/mirror-local.sh test-core 起的私有进程上运行（ROOST_MIRROR_LOCAL=1），不碰共享隔离环境。
//
// 确定性手段：单机副本改经私有 toxiproxy 的一个临时代理复制，代理在“主 → 副本”方向加延迟（复制滞后
// 由测试控制，不靠碰运气，0.5s）；写墓碑后立刻 REPLICAOF NO ONE 提升副本（复制连接被关掉，延迟中的数据丢弃）。
//   - 不 WAIT（修前的行为，旧构造 NewSnapshotL2StoreWithKeyPrefix）：删除立即返回，提升后的副本上没有墓碑，
//     L1 空的新只读方读到已删除的实体（红）；
//   - WAIT 1 个副本：删除等到副本确认（约 0.5s）才返回，提升后墓碑仍在，新只读方读到“不存在”（绿）。
// 副本被提升之后旧主没有副本：再删一次走 no_replicas，不等。
// Cluster：WAIT 只打到该键所在的主节点（INFO commandstats 的 cmdstat_wait 计数）；只 SIGSTOP 该主的副本、
// 不断开复制连接时 WAIT 按超时返回，计为 short，删除不被卡住。

const tombstoneWaitLatency = 500 * time.Millisecond

type tombstoneWaitEnv struct {
	master, replica, toxiproxy, script string
	cluster                            []string
	offset                             int
	prefix                             string
}

func newTombstoneWaitEnv(t *testing.T) *tombstoneWaitEnv {
	t.Helper()
	if os.Getenv("ROOST_DATAENGINE_IT") != "1" || os.Getenv("ROOST_MIRROR_LOCAL") != "1" {
		t.Skip("run scripts/mirror-local.sh test-core (private dependency processes)")
	}
	offset, err := strconv.Atoi(os.Getenv("ROOST_MIRROR_LOCAL_OFFSET"))
	if err != nil {
		t.Fatalf("ROOST_MIRROR_LOCAL_OFFSET: %v", err)
	}
	env := &tombstoneWaitEnv{
		master: os.Getenv("ROOST_DATAENGINE_IT_REDIS_ADDR"), replica: os.Getenv("ROOST_MIRROR_LOCAL_REDIS_REPLICA"),
		toxiproxy: os.Getenv("ROOST_DATAENGINE_IT_TOXIPROXY_URL"), script: os.Getenv("ROOST_MIRROR_LOCAL_SCRIPT"),
		offset: offset, prefix: fmt.Sprintf("m6obs.%d", time.Now().UnixNano()),
	}
	if c := os.Getenv("ROOST_MIRROR_LOCAL_REDIS_CLUSTER"); c != "" {
		env.cluster = strings.Split(c, ",")
	}
	if env.master == "" || env.replica == "" || env.toxiproxy == "" || env.script == "" {
		t.Fatal("the private environment does not export the Redis replica / toxiproxy / script")
	}
	return env
}

func rawRedis(t *testing.T, addr string) *goredis.Client {
	t.Helper()
	client := goredis.NewClient(&goredis.Options{Addr: addr, MaxRetries: -1})
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func replicationField(t *testing.T, client *goredis.Client, field string) string {
	t.Helper()
	info, err := client.Info(context.Background(), "replication").Result()
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(info, "\n") {
		if value, ok := strings.CutPrefix(strings.TrimSpace(line), field+":"); ok {
			return value
		}
	}
	return ""
}

// waitReplicaInSync 等副本的复制连接 up 且偏移量追平主（环境就绪的等待，不是被测行为）。
func waitReplicaInSync(t *testing.T, master, replica *goredis.Client) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if replicationField(t, replica, "master_link_status") == "up" &&
			replicationField(t, master, "master_repl_offset") == replicationField(t, replica, "slave_repl_offset") {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("replica did not catch up with the primary within 30s")
}

func (env *tombstoneWaitEnv) tox(t *testing.T, method, path, body string) {
	t.Helper()
	req, err := http.NewRequest(method, env.toxiproxy+path, bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode >= 300 {
		t.Fatalf("toxiproxy %s %s: %s", method, path, resp.Status)
	}
}

func (env *tombstoneWaitEnv) fault(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command(env.script, append([]string{"fault"}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("fault %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// readerSees 用一个 L1 空的新只读方（只有 L2，没有权威）做 Cached 读：found 即读到了快照。
func readerSees(t *testing.T, l2 *remoteSnapshotL2Store, key entity.RemoteSnapshotKey, sid int32) (uint64, bool) {
	t.Helper()
	reader, err := NewSnapshotClient(DefaultConfig(), SnapshotClientDeps{ConsumerSID: sid, L2: l2})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Stop(context.Background()) }()
	got, found, err := reader.ReadSnapshot(context.Background(), entity.RemoteSnapshotRead{Key: key, Consistency: entity.RemoteReadCached})
	if err != nil {
		t.Fatalf("fresh reader: %v", err)
	}
	return got.StateVersion, found
}

func TestMirrorLocalTombstoneSurvivesAFailoverOnlyWithWait(t *testing.T) {
	env := newTombstoneWaitEnv(t)
	ctx := context.Background()
	master, replica := rawRedis(t, env.master), rawRedis(t, env.replica)
	if role := replicationField(t, master, "role"); role != "master" {
		t.Fatalf("%s is %q; run on a fresh environment (the primary must be the original node)", env.master, role)
	}
	masterPort := strings.Split(env.master, ":")[1]
	proxyListen := fmt.Sprintf("127.0.0.1:%d", env.offset+26390)
	env.tox(t, http.MethodPost, "/proxies", fmt.Sprintf(`{"name":"m6obs-replication","listen":%q,"upstream":%q,"enabled":true}`, proxyListen, env.master))
	t.Cleanup(func() {
		// 恢复：副本直接复制主、删掉临时代理、等追平。
		_ = replica.Do(ctx, "replicaof", "127.0.0.1", masterPort).Err()
		env.tox(t, http.MethodDelete, "/proxies/m6obs-replication", "")
		waitReplicaInSync(t, master, replica)
		keys, _ := master.Keys(ctx, env.prefix+":*").Result()
		if len(keys) > 0 {
			_ = master.Del(ctx, keys...).Err()
		}
	})

	driver, err := redisdriver.NewClient(fredis.DefaultConfig(env.master))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = driver.Close() })
	replicaDriver, err := redisdriver.NewClient(fredis.DefaultConfig(env.replica))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = replicaDriver.Close() })

	for i, tc := range []struct {
		name string
		wait bool
	}{
		{"without_wait", false},
		{"with_wait", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			proxyHost, proxyPort, _ := strings.Cut(proxyListen, ":")
			if err := replica.Do(ctx, "replicaof", proxyHost, proxyPort).Err(); err != nil {
				t.Fatal(err)
			}
			waitReplicaInSync(t, master, replica)
			cfg := DefaultConfig()
			cfg.SnapshotL2KeyPrefix = env.prefix
			cfg.SnapshotL2TombstoneWaitTimeout = MaxSnapshotL2TombstoneWaitTimeout
			var store *remoteSnapshotL2Store
			if tc.wait {
				store, err = NewSnapshotL2StoreFromConfig(driver, cfg)
			} else {
				store, err = NewSnapshotL2StoreWithKeyPrefix(driver, cfg.SnapshotL2TTL, cfg.SnapshotL2KeyPrefix) // 修前的构造，不等副本
			}
			if err != nil {
				t.Fatal(err)
			}
			key := staleBackfillKey(t, b2WatermarkKind, int64(9920+i))
			if err := store.Set(ctx, staleBackfillEnvelope(key, 1, 1, "v1")); err != nil {
				t.Fatal(err)
			}
			waitReplicaInSync(t, master, replica)

			env.tox(t, http.MethodPost, "/proxies/m6obs-replication/toxics",
				fmt.Sprintf(`{"name":"lag","type":"latency","stream":"downstream","attributes":{"latency":%d}}`, tombstoneWaitLatency.Milliseconds()))
			started := time.Now()
			if err := store.DeleteAtVersion(ctx, key, 2); err != nil {
				t.Fatal(err)
			}
			elapsed := time.Since(started)
			// 立刻切主：提升副本，复制连接关闭，代理里还在延迟的数据随之丢弃。
			if err := replica.Do(ctx, "replicaof", "no", "one").Err(); err != nil {
				t.Fatal(err)
			}
			env.tox(t, http.MethodDelete, "/proxies/m6obs-replication/toxics/lag", "")

			promoted, err := NewSnapshotL2StoreWithKeyPrefix(replicaDriver, cfg.SnapshotL2TTL, cfg.SnapshotL2KeyPrefix)
			if err != nil {
				t.Fatal(err)
			}
			tomb, _ := replica.HGet(ctx, promoted.key(key), "deleted_version").Result()
			version, found := readerSees(t, promoted, key, int32(2700+i))
			t.Logf("MIRROR O-M6-3 %s: delete took %v; after promoting the replica tombstone=%q fresh reader found=%v version=%d; wait stats %+v",
				tc.name, elapsed.Round(time.Millisecond), tomb, found, version, store.TombstoneWaitStats())
			if !tc.wait {
				// 修前：墓碑没复制到副本就切主，新只读方把已删除的实体读成存在（O-M6-3 的现象，确定性复现）。
				if !found || tomb != "" {
					t.Fatalf("without WAIT the deleted snapshot should come back after the failover (found=%v tombstone=%q); the lag injection did not work", found, tomb)
				}
				return
			}
			if found || tomb != "2" {
				t.Fatalf("with WAIT the tombstone must survive the failover: fresh reader found=%v version=%d, tombstone=%q", found, version, tomb)
			}
			if stats := store.TombstoneWaitStats(); stats.Confirmed != 1 || elapsed < tombstoneWaitLatency/2 {
				t.Fatalf("with WAIT: stats=%+v elapsed=%v, want one confirmed wait that actually waited for the lagging replica", stats, elapsed)
			}
			// 副本已被提升：旧主没有副本，再删一次不等（no_replicas），不被卡住。先等旧主看到复制连接关闭。
			deadline := time.Now().Add(10 * time.Second)
			for replicationField(t, master, "connected_slaves") != "0" {
				if time.Now().After(deadline) {
					t.Fatal("the old primary still lists a replica 10s after the promotion")
				}
				time.Sleep(10 * time.Millisecond)
			}
			other := staleBackfillKey(t, b2WatermarkKind, 9925)
			started = time.Now()
			if err := store.DeleteAtVersion(ctx, other, 1); err != nil {
				t.Fatal(err)
			}
			if stats := store.TombstoneWaitStats(); stats.NoReplicas != 1 || time.Since(started) > cfg.SnapshotL2TombstoneWaitTimeout/2 {
				t.Fatalf("delete on a primary without replicas: stats=%+v took %v, want no wait", stats, time.Since(started))
			}
		})
	}
}

func waitCalls(t *testing.T, addr string) int64 {
	t.Helper()
	client := rawRedis(t, addr)
	info, err := client.Info(context.Background(), "commandstats").Result()
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(info, "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "cmdstat_wait:calls="); ok {
			n, _ := strconv.ParseInt(strings.SplitN(rest, ",", 2)[0], 10, 64)
			return n
		}
	}
	return 0
}

func TestMirrorLocalTombstoneWaitOnClusterGoesToTheKeysPrimary(t *testing.T) {
	env := newTombstoneWaitEnv(t)
	if len(env.cluster) == 0 {
		t.Fatal("the private environment does not export the Redis Cluster")
	}
	ctx := context.Background()
	rcfg := fredis.DefaultConfig("")
	rcfg.ClusterAddrs = env.cluster
	client := redisdriver.NewRedisClient(rcfg)
	t.Cleanup(func() { _ = client.Close() })
	cfg := DefaultConfig()
	cfg.SnapshotL2KeyPrefix = env.prefix
	cfg.SnapshotL2TombstoneWaitTimeout = 200 * time.Millisecond
	store, err := NewSnapshotL2StoreFromConfig(client, cfg)
	if err != nil {
		t.Fatal(err)
	}
	key := staleBackfillKey(t, b2WatermarkKind, 9930)
	redisKey := store.key(key)
	t.Cleanup(func() { _ = client.Raw().Del(context.Background(), redisKey).Err() })
	cluster := client.Raw().(*goredis.ClusterClient)
	owner, err := cluster.MasterForKey(ctx, redisKey)
	if err != nil {
		t.Fatal(err)
	}
	before := map[string]int64{}
	for _, addr := range env.cluster {
		before[addr] = waitCalls(t, addr)
	}
	if err := store.Set(ctx, staleBackfillEnvelope(key, 1, 1, "v1")); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteAtVersion(ctx, key, 2); err != nil {
		t.Fatal(err)
	}
	for _, addr := range env.cluster {
		delta := waitCalls(t, addr) - before[addr]
		want := int64(0)
		if addr == owner.Options().Addr {
			want = 1
		}
		if delta != want {
			t.Fatalf("WAIT calls on %s went up by %d, want %d (the key's primary is %s)", addr, delta, want, owner.Options().Addr)
		}
	}
	if stats := store.TombstoneWaitStats(); stats.Confirmed != 1 {
		t.Fatalf("cluster tombstone wait stats %+v, want one confirmed", stats)
	}

	// 该主节点的副本被 SIGSTOP（复制连接仍在）：WAIT 按超时返回，删除照常成功、不被卡住，计为 short。
	t.Log(env.fault(t, "redis-cluster-stop-replica", redisKey))
	resumed := false
	resume := func() {
		if !resumed {
			resumed = true
			env.fault(t, "redis-cluster-cont")
		}
	}
	t.Cleanup(resume)
	if err := store.Set(ctx, staleBackfillEnvelope(key, 3, 1, "v3")); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	if err := store.DeleteAtVersion(ctx, key, 4); err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(started)
	resume()
	stats := store.TombstoneWaitStats()
	t.Logf("MIRROR O-M6-3 cluster: stopped replica, delete took %v, wait stats %+v", elapsed.Round(time.Millisecond), stats)
	if stats.Short != 1 || elapsed < cfg.SnapshotL2TombstoneWaitTimeout || elapsed > cfg.SnapshotL2TombstoneWaitTimeout+time.Second {
		t.Fatalf("with the replica stopped: stats=%+v elapsed=%v, want one short wait bounded by the %v timeout", stats, elapsed, cfg.SnapshotL2TombstoneWaitTimeout)
	}
}
