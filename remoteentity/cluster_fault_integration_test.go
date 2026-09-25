//go:build integration

package remoteentity

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"
	"github.com/tjbdwanghaibo/roost-core/entity"
	fmongo "github.com/tjbdwanghaibo/roost-core/mongo"
	mongodriver "github.com/tjbdwanghaibo/roost-core/mongo/driver"
	fredis "github.com/tjbdwanghaibo/roost-core/redis"
	redisdriver "github.com/tjbdwanghaibo/roost-core/redis/driver"
)

type remoteClusterNode struct {
	port    int
	dir     string
	command *exec.Cmd
	done    chan error
}

func (n *remoteClusterNode) start(t *testing.T) {
	t.Helper()
	log, err := os.OpenFile(filepath.Join(n.dir, "redis.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	n.command = exec.Command("redis-server", "--port", strconv.Itoa(n.port), "--bind", "127.0.0.1", "--dir", n.dir, "--cluster-enabled", "yes", "--cluster-config-file", "nodes.conf", "--cluster-node-timeout", "1000", "--appendonly", "yes", "--appendfsync", "always", "--save", "", "--set-proc-title", "no")
	n.command.Stdout = log
	n.command.Stderr = log
	if err := n.command.Start(); err != nil {
		log.Close()
		t.Fatal(err)
	}
	n.done = make(chan error, 1)
	go func() { n.done <- n.command.Wait(); log.Close() }()
}
func (n *remoteClusterNode) stop(t *testing.T) {
	t.Helper()
	if n.command == nil {
		return
	}
	select {
	case <-n.done:
	default:
		if err := n.command.Process.Kill(); err != nil {
			t.Error(err)
		}
		<-n.done
	}
	n.command = nil
}
func remoteClusterCLI(t *testing.T, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "redis-cli", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("redis-cli %v: %v %s", args, err, out)
	}
	return string(out)
}
func awaitRemoteCluster(t *testing.T, nodes []*remoteClusterNode) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		healthy := true
		for _, n := range nodes {
			if n.command == nil {
				continue
			}
			c := goredis.NewClient(&goredis.Options{Addr: fmt.Sprintf("127.0.0.1:%d", n.port), MaxRetries: -1})
			ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
			info, err := c.ClusterInfo(ctx).Result()
			cancel()
			c.Close()
			if err != nil || !strings.Contains(info, "cluster_state:ok") {
				healthy = false
				break
			}
		}
		if healthy {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("cluster not healthy")
}

// 专属 3 主 3 从集群，AOF always；只杀本测试创建的进程，完全独立于长稳依赖。
func newRemoteTestCluster(t *testing.T) ([]*remoteClusterNode, []string) {
	t.Helper()
	if os.Getenv("ROOST_REMOTE_CLUSTER_IT") != "1" {
		t.Skip("set ROOST_REMOTE_CLUSTER_IT=1")
	}
	for _, binary := range []string{"redis-server", "redis-cli"} {
		if _, err := exec.LookPath(binary); err != nil {
			t.Fatal(err)
		}
	}
	root := t.TempDir()
	var nodes []*remoteClusterNode
	var addresses []string
	t.Cleanup(func() {
		if t.Failed() {
			for _, n := range nodes {
				data, _ := os.ReadFile(filepath.Join(n.dir, "redis.log"))
				if len(data) > 5000 {
					data = data[len(data)-5000:]
				}
				t.Logf("node %d: %s", n.port, data)
			}
		}
		for _, n := range nodes {
			n.stop(t)
		}
	})
	for i := 0; i < 6; i++ {
		port := 17380 + i
		for _, probe := range []int{port, port + 10000} {
			l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", probe))
			if err != nil {
				t.Fatalf("dedicated port unavailable: %v", err)
			}
			l.Close()
		}
		dir := filepath.Join(root, strconv.Itoa(port))
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
		n := &remoteClusterNode{port: port, dir: dir}
		nodes = append(nodes, n)
		n.start(t)
		addresses = append(addresses, fmt.Sprintf("127.0.0.1:%d", port))
	}
	for _, addr := range addresses {
		c := goredis.NewClient(&goredis.Options{Addr: addr, MaxRetries: -1})
		deadline := time.Now().Add(5 * time.Second)
		for {
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			err := c.Ping(ctx).Err()
			cancel()
			if err == nil {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal(err)
			}
			time.Sleep(20 * time.Millisecond)
		}
		c.Close()
	}
	args := append([]string{"--cluster", "create"}, addresses...)
	args = append(args, "--cluster-replicas", "1", "--cluster-yes")
	remoteClusterCLI(t, args...)
	awaitRemoteCluster(t, nodes)
	return nodes, addresses
}

func TestRealRemoteRedisClusterFailover(t *testing.T) {
	nodes, addresses := newRemoteTestCluster(t)
	cfg := fredis.DefaultConfig("")
	cfg.ClusterAddrs = addresses
	cfg.DialTimeout = 200 * time.Millisecond
	cfg.ReadTimeout = 200 * time.Millisecond
	cfg.WriteTimeout = 200 * time.Millisecond
	client := redisdriver.NewRedisClient(cfg)
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	// 当前多 key Lua 要求显式 hash tag；先把默认配置的限制钉成可见证据。
	unsupported := newVersionedLock(client, 42, fredis.VersionedLockOptions{Key: "e", TTL: time.Second})
	if err := unsupported.TryLock(ctx); err == nil || !strings.Contains(err.Error(), "CROSSSLOT") {
		t.Fatalf("default untagged key should expose cluster limitation: %v", err)
	}
	t.Log("default LockKey=e rejected with CROSSSLOT; cluster scenario explicitly uses {remote-cluster-it}")
	opts := fredis.VersionedLockOptions{Key: "{remote-cluster-it}", TTL: 1200 * time.Millisecond}
	first := newVersionedLock(client, 42, opts)
	defer first.Close()
	raw := client.Raw().(*goredis.ClusterClient)
	slot, err := raw.ClusterKeySlot(ctx, first.key).Result()
	if err != nil {
		t.Fatal(err)
	}
	slots, err := raw.ClusterSlots(ctx).Result()
	if err != nil {
		t.Fatal(err)
	}
	var primary string
	for _, s := range slots {
		if int64(s.Start) <= slot && slot <= int64(s.End) {
			primary = s.Nodes[0].Addr
			break
		}
	}
	if primary == "" {
		t.Fatal("primary not found")
	}
	// WAIT 与锁写在不同连接不能证明锁已复制；读取 replica 的实际 fence/owner 后才杀主。
	var replica string
	replicaDeadline := time.Now().Add(15 * time.Second)
	for replica == "" {
		slots, err = raw.ClusterSlots(ctx).Result()
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range slots {
			if int64(s.Start) <= slot && slot <= int64(s.End) && len(s.Nodes) > 1 {
				replica = s.Nodes[1].Addr
				break
			}
		}
		if replica == "" {
			if time.Now().After(replicaDeadline) {
				t.Fatal("replica not found")
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	replicaClient := goredis.NewClient(&goredis.Options{Addr: replica})
	defer replicaClient.Close()
	if err := replicaClient.ReadOnly(ctx).Err(); err != nil {
		t.Fatal(err)
	}
	if err := first.TryLock(ctx); err != nil {
		t.Fatal(err)
	}
	oldFence := first.Fence()
	deadline := time.Now().Add(10 * time.Second)
	for {
		fence, err := replicaClient.Get(ctx, first.key+":fence").Uint64()
		owner, ownerErr := replicaClient.HGet(ctx, first.key, "owner").Result()
		if err == nil && ownerErr == nil && fence == oldFence && owner != "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("replica not caught up: fence=%d err=%v owner=%q err=%v", fence, err, owner, ownerErr)
		}
		time.Sleep(20 * time.Millisecond)
	}
	started := time.Now()
	for _, n := range nodes {
		if primary == fmt.Sprintf("127.0.0.1:%d", n.port) {
			n.stop(t)
		}
	}
	second := newVersionedLock(client, 42, opts)
	defer second.Close()
	deadline = time.Now().Add(20 * time.Second)
	for {
		err = second.TryLock(ctx)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal(err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if second.Fence() <= oldFence {
		t.Fatalf("fence rolled back %d -> %d", oldFence, second.Fence())
	}
	if err := first.Unlock(ctx, 1, time.Minute); !errors.Is(err, ErrVersionedLockNotOwned) {
		t.Fatalf("old owner unlocked: %v", err)
	}
	if err := second.Unlock(ctx, 2, time.Minute); err != nil {
		t.Fatal(err)
	}
	t.Logf("replicated primary kill recovered in %v, fence %d -> %d", time.Since(started), oldFence, second.Fence())
	for _, n := range nodes {
		if n.command == nil {
			n.start(t)
		}
	}
	awaitRemoteCluster(t, nodes)
	for _, n := range nodes {
		n.stop(t)
	}
	outage := newVersionedLock(client, 42, opts)
	defer outage.Close()
	limited, stop := context.WithTimeout(context.Background(), 300*time.Millisecond)
	started = time.Now()
	err = outage.TryLock(limited)
	stop()
	if err == nil || time.Since(started) > time.Second {
		t.Fatalf("whole cluster outage not bounded: %v %v", time.Since(started), err)
	}
	for _, n := range nodes {
		n.start(t)
	}
	awaitRemoteCluster(t, nodes)
	restored := newVersionedLock(client, 42, opts)
	defer restored.Close()
	if err := restored.TryLock(ctx); err != nil {
		t.Fatal(err)
	}
	if restored.Fence() <= second.Fence() || restored.Version() != 2 {
		t.Fatalf("restart fence/version=%d/%d", restored.Fence(), restored.Version())
	}
	if err := restored.Unlock(ctx, 3, time.Minute); err != nil {
		t.Fatal(err)
	}
	t.Logf("whole cluster restart retained version=2 and advanced fence to %d", restored.Fence())
}

// 这是生产准入测试，不把“已知 Redis 异步复制会丢写”转换成绿色预期。
// 暂停唯一副本并切断复制连接，再获取锁、强杀主节点，检查已确认 fence 是否重用。
func TestRealRemoteRedisClusterUnreplicatedFence(t *testing.T) {
	uri := os.Getenv("ROOST_DATAENGINE_IT_MONGO_URI")
	if uri == "" {
		t.Skip("isolated Mongo replica set required")
	}
	mongo, err := mongodriver.NewClient(fmongo.DefaultConfig(uri), mongodriver.IndexMigrationPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	defer mongo.Close(context.Background())
	database := fmt.Sprintf("roost_remote_fence_%d", time.Now().UnixNano())
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := mongo.Database(database).Drop(ctx); err != nil {
			t.Error(err)
		}
	}()
	store := NewMongoCommitter(mongo, database, 7, 0)
	authority := store.WriteAuthority()
	entity.MustRegisterEntityKindDefs(entity.EntityKindDef{Kind: 198, Category: 1, RemotePolicy: entity.RemotePolicyManaged})
	id, err := entity.BuildEntityID(997, 198)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := authority.ClaimOwnership(context.Background(), id, 7)
	if err != nil {
		t.Fatal(err)
	}
	shared, err := authority.EnterSharedExpected(context.Background(), id, owner)
	if err != nil {
		t.Fatal(err)
	}
	nodes, addresses := newRemoteTestCluster(t)
	cfg := fredis.DefaultConfig("")
	cfg.ClusterAddrs = addresses
	client := redisdriver.NewRedisClient(cfg)
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	opts := fredis.VersionedLockOptions{Key: "{remote-cluster-loss}", TTL: time.Second}
	factory := NewVersionedLockFactory(client, authority)
	first := factory.NewVersionedLock(id, opts).(*versionedLock)
	defer first.Close()
	raw := client.Raw().(*goredis.ClusterClient)
	slot, err := raw.ClusterKeySlot(ctx, first.key).Result()
	if err != nil {
		t.Fatal(err)
	}
	var primary, replica string
	deadline := time.Now().Add(15 * time.Second)
	for replica == "" {
		slots, err := raw.ClusterSlots(ctx).Result()
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range slots {
			if int64(s.Start) <= slot && slot <= int64(s.End) && len(s.Nodes) > 1 {
				primary, replica = s.Nodes[0].Addr, s.Nodes[1].Addr
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("replica not ready")
		}
		time.Sleep(50 * time.Millisecond)
	}
	var replicaNode, primaryNode *remoteClusterNode
	for _, n := range nodes {
		addr := fmt.Sprintf("127.0.0.1:%d", n.port)
		if addr == replica {
			replicaNode = n
		}
		if addr == primary {
			primaryNode = n
		}
	}
	if replicaNode == nil || primaryNode == nil {
		t.Fatal("owned nodes not found")
	}
	// wait for initial sync, not merely the replica role announcement
	replicaClient := goredis.NewClient(&goredis.Options{Addr: replica})
	defer replicaClient.Close()
	deadline = time.Now().Add(15 * time.Second)
	for {
		info, err := replicaClient.Info(ctx, "replication").Result()
		if err == nil && strings.Contains(info, "master_link_status:up") && !strings.Contains(info, "master_sync_in_progress:1") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("initial replication not ready", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	signal := func(name string) {
		if output, err := exec.Command("kill", name, strconv.Itoa(replicaNode.command.Process.Pid)).CombinedOutput(); err != nil {
			t.Fatalf("signal owned replica: %v %s", err, output)
		}
	}
	signal("-STOP")
	defer signal("-CONT")
	direct := goredis.NewClient(&goredis.Options{Addr: primary})
	defer direct.Close()
	killed, err := direct.ClientKillByFilter(ctx, "TYPE", "replica").Result()
	if err != nil || killed < 1 {
		t.Fatalf("replication cut: %d %v", killed, err)
	}
	if err := first.TryLock(ctx); err != nil {
		t.Fatal(err)
	}
	acknowledgedFence := first.Fence()
	redisFence, err := raw.Get(ctx, first.key+":fence").Int64()
	if err != nil {
		t.Fatal(err)
	}
	stale := authorityCommit(t, id, 1, WriteGrant{Version: first.Version(), Fence: first.Fence(), Ownership: shared})
	primaryNode.stop(t)
	signal("-CONT")
	next := factory.NewVersionedLock(id, opts).(*versionedLock)
	defer next.Close()
	deadline = time.Now().Add(20 * time.Second)
	for {
		err = next.TryLock(ctx)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal(err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if next.Fence() <= acknowledgedFence {
		t.Fatalf("PRODUCTION_GATE_FAILED: acknowledged fence %d reused after unreplicated primary failure: new fence=%d", acknowledgedFence, next.Fence())
	}
	rolledBack, err := raw.Get(ctx, next.key+":fence").Int64()
	if err != nil {
		t.Fatal(err)
	}
	if rolledBack > redisFence {
		t.Fatalf("fault did not reproduce Redis counter reuse: %d -> %d", redisFence, rolledBack)
	}
	if _, err := store.CommitRemote(ctx, stale); !errors.Is(err, entity.ErrRemoteVersionConflict) {
		t.Fatalf("old business commit accepted: %v", err)
	}
	fresh := authorityCommit(t, id, 2, WriteGrant{Version: next.Version(), Fence: next.Fence(), Ownership: shared})
	if _, err := store.CommitRemote(ctx, fresh); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CommitRemote(ctx, stale); !errors.Is(err, entity.ErrRemoteVersionConflict) {
		t.Fatalf("old business replay accepted: %v", err)
	}
	t.Logf("Redis counter reused %d -> %d; durable fence %d -> %d; stale commit rejected, new commit persisted", redisFence, rolledBack, acknowledgedFence, next.Fence())

}
