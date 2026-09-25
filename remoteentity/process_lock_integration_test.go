//go:build integration

package remoteentity

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/entity"
	fmongo "github.com/tjbdwanghaibo/roost-core/mongo"
	mongodriver "github.com/tjbdwanghaibo/roost-core/mongo/driver"
	fredis "github.com/tjbdwanghaibo/roost-core/redis"
	redisdriver "github.com/tjbdwanghaibo/roost-core/redis/driver"
)

const processLeaseTTL = 1200 * time.Millisecond

type processLeaseStatus struct {
	Fence             uint64
	Held              bool
	Touches, Failures int64
	Error             string
	Rejected          bool
}
type observedLeaseRedis struct {
	fredis.IRedis
	touches, failures atomic.Int64
}

func (r *observedLeaseRedis) Eval(ctx context.Context, script string, keys []string, args ...any) (any, error) {
	value, err := r.IRedis.Eval(ctx, script, keys, args...)
	if script == versionedTouchLua {
		if err != nil {
			r.failures.Add(1)
		} else {
			r.touches.Add(1)
		}
	}
	return value, err
}

// 子进程使用正式锁实现与真实 Redis；管道只控制观测/退出，不模拟租约状态。
func TestRemoteLeaseProcessHelper(t *testing.T) {
	if os.Getenv("ROOST_REMOTE_CHILD") != "1" {
		t.Skip("subprocess only")
	}
	key := os.Getenv("ROOST_REMOTE_LOCK_KEY")
	if !strings.HasPrefix(key, "roost-remote-it:") {
		t.Fatal("isolated key required")
	}
	client, err := redisdriver.NewClient(fredis.DefaultConfig(os.Getenv("ROOST_REMOTE_CHILD_REDIS")))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	observed := &observedLeaseRedis{IRedis: client}
	lock := newVersionedLock(observed, 42, fredis.VersionedLockOptions{TTL: processLeaseTTL, AutoAsyncTouch: true, AsyncTouchInterval: 200 * time.Millisecond, AsyncTouchExtend: 600 * time.Millisecond})
	var store *MongoCommitter
	var ownership entity.RemoteEntityMarkerLease
	if database := os.Getenv("ROOST_REMOTE_CHILD_AUTHORITY_DB"); database != "" {
		if !strings.HasPrefix(database, "roost_remote_process_") {
			t.Fatal("isolated authority database required")
		}
		mongo, err := mongodriver.NewClient(fmongo.DefaultConfig(os.Getenv("ROOST_DATAENGINE_IT_MONGO_URI")), mongodriver.IndexMigrationPolicy{})
		if err != nil {
			t.Fatal(err)
		}
		defer mongo.Close(context.Background())
		store = NewMongoCommitter(mongo, database, 7, 0)
		lock.authority = store.WriteAuthority()
		entity.MustRegisterEntityKindDefs(entity.EntityKindDef{Kind: 198, Category: 1, RemotePolicy: entity.RemotePolicyManaged})
		lock.id, err = entity.BuildEntityID(998, 198)
		if err != nil {
			t.Fatal(err)
		}
		var found bool
		ownership, found, err = store.GetOwnership(context.Background(), lock.id)
		if err != nil || !found {
			t.Fatalf("ownership found=%v err=%v", found, err)
		}
	}
	lock.key = key
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		err = lock.TryLock(ctx)
		if err == nil {
			break
		}
		if !errors.Is(err, ErrVersionedLockNotAcquired) {
			t.Fatal(err)
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	defer lock.Close()
	reply := func(err error) {
		s := processLeaseStatus{Fence: lock.Fence(), Held: lock.IsAcquired(), Touches: observed.touches.Load(), Failures: observed.failures.Load()}
		if err != nil {
			s.Error = err.Error()
			s.Rejected = errors.Is(err, entity.ErrRemoteVersionConflict)
		}
		if err := json.NewEncoder(os.Stdout).Encode(s); err != nil {
			t.Fatal(err)
		}
	}
	reply(nil)
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		switch scanner.Text() {
		case "commit":
			if store == nil {
				t.Fatal("authority is required")
			}
			commit := authorityCommit(t, lock.id, byte(lock.Fence()), WriteGrant{Version: lock.Version(), Fence: lock.Fence(), Ownership: ownership})
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_, err := store.CommitRemote(ctx, commit)
			cancel()
			reply(err)
		case "status":
			reply(nil)
		case "release":
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			err := lock.Unlock(ctx, 7, time.Second)
			cancel()
			reply(err)
		case "quit":
			return
		default:
			t.Fatal("unknown child command")
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
}

type leaseProcess struct {
	cmd     *exec.Cmd
	input   io.WriteCloser
	replies chan processLeaseStatus
	done    chan struct{}
	err     error
}

func startLeaseProcess(t *testing.T, key, addr string, extraEnv ...string) *leaseProcess {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^TestRemoteLeaseProcessHelper$", "-test.timeout=30s")
	cmd.Env = append(os.Environ(), "ROOST_REMOTE_CHILD=1", "ROOST_REMOTE_LOCK_KEY="+key, "ROOST_REMOTE_CHILD_REDIS="+addr)
	cmd.Env = append(cmd.Env, extraEnv...)
	input, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	p := &leaseProcess{cmd: cmd, input: input, replies: make(chan processLeaseStatus, 4), done: make(chan struct{})}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() {
		defer close(p.replies)
		decoder := json.NewDecoder(output)
		for {
			var s processLeaseStatus
			if err := decoder.Decode(&s); err != nil {
				return
			}
			p.replies <- s
		}
	}()
	go func() { p.err = cmd.Wait(); close(p.done) }()
	t.Cleanup(func() {
		_ = input.Close()
		select {
		case <-p.done:
		default:
			_ = cmd.Process.Kill()
			<-p.done
		}
	})
	return p
}
func (p *leaseProcess) receive(t *testing.T) processLeaseStatus {
	t.Helper()
	select {
	case s, ok := <-p.replies:
		if !ok {
			t.Fatal("child exited without reply")
		}
		return s
	case <-time.After(12 * time.Second):
		t.Fatal("child reply timeout")
	}
	return processLeaseStatus{}
}
func (p *leaseProcess) command(t *testing.T, command string) processLeaseStatus {
	t.Helper()
	if _, err := fmt.Fprintln(p.input, command); err != nil {
		t.Fatal(err)
	}
	return p.receive(t)
}
func (p *leaseProcess) kill(t *testing.T) {
	t.Helper()
	if err := p.cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	<-p.done
	if p.err == nil {
		t.Fatal("killed child exited successfully")
	}
}
func (p *leaseProcess) quit(t *testing.T) {
	t.Helper()
	if _, err := fmt.Fprintln(p.input, "quit"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-p.done:
		if p.err != nil {
			t.Fatal(p.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("child did not stop")
	}
}

func TestRealRemoteProcessKillRecoversLease(t *testing.T) {
	r := realRemoteRedis(t)
	key := remoteRedisKey(t, r)
	addr := os.Getenv("ROOST_DATAENGINE_IT_REDIS_ADDR")
	first := startLeaseProcess(t, key, addr)
	initial := first.receive(t)
	// 故意跨两个初始 TTL 观测；这是验证续租的时间窗口，不是制造并发顺序的 sleep。
	until := time.Now().Add(2 * processLeaseTTL)
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	current := initial
	for time.Now().Before(until) {
		<-ticker.C
		current = first.command(t, "status")
		if !current.Held {
			t.Fatal("lease lost while healthy")
		}
	}
	if current.Touches < 3 {
		t.Fatalf("auto touch did not run: %+v", current)
	}
	first.kill(t)
	started := time.Now()
	second := startLeaseProcess(t, key, addr)
	next := second.receive(t)
	if !next.Held || next.Fence <= initial.Fence {
		t.Fatalf("handoff initial=%+v next=%+v", initial, next)
	}
	if elapsed := time.Since(started); elapsed > 4*processLeaseTTL {
		t.Fatalf("handoff too slow: %v", elapsed)
	} else {
		t.Logf("kill -> new owner=%v fence=%d->%d touches=%d", elapsed, initial.Fence, next.Fence, current.Touches)
	}
	second.quit(t)
}

// 每个测试创建自己的代理，不调用全局 /reset，不影响已有 Mongo/NATS/Redis 测试代理。
type leaseProxy struct{ base, name, addr string }

func proxyRequest(t *testing.T, method, url string, body any) json.RawMessage {
	t.Helper()
	var payload bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&payload).Encode(body); err != nil {
			t.Fatal(err)
		}
	}
	req, err := http.NewRequest(method, url, &payload)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 3 * time.Second}
	response, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode >= 300 {
		t.Fatalf("proxy HTTP %d: %s", response.StatusCode, data)
	}
	return data
}
func newLeaseProxy(t *testing.T) *leaseProxy {
	t.Helper()
	base := os.Getenv("ROOST_DATAENGINE_IT_TOXIPROXY_URL")
	if base == "" {
		t.Fatal("isolated toxiproxy required")
	}
	name := "remote-lease-" + generateToken()
	raw := proxyRequest(t, http.MethodPost, base+"/proxies", map[string]any{"name": name, "listen": "127.0.0.1:0", "upstream": os.Getenv("ROOST_DATAENGINE_IT_REDIS_ADDR"), "enabled": true})
	var created struct {
		Listen string `json:"listen"`
	}
	if err := json.Unmarshal(raw, &created); err != nil {
		t.Fatal(err)
	}
	p := &leaseProxy{base: base, name: name, addr: created.Listen}
	t.Cleanup(func() { proxyRequest(t, http.MethodDelete, base+"/proxies/"+name, nil) })
	if created.Listen == "" || strings.HasSuffix(created.Listen, ":0") {
		t.Fatalf("proxy did not allocate listen address: %s", raw)
	}
	return p
}
func (p *leaseProxy) enable(t *testing.T, enabled bool) {
	// 首次用 :0 分配端口后固定它；否则再次开启会换端口，无法验证原客户端重连。
	proxyRequest(t, http.MethodPost, p.base+"/proxies/"+p.name, map[string]any{"enabled": enabled, "listen": p.addr})
}
func TestRealRemoteProcessPartitionFencesOldOwner(t *testing.T) {
	r := realRemoteRedis(t)
	key := remoteRedisKey(t, r)
	proxy := newLeaseProxy(t)
	first := startLeaseProcess(t, key, proxy.addr)
	initial := first.receive(t)
	proxy.enable(t, false)
	started := time.Now()
	second := startLeaseProcess(t, key, os.Getenv("ROOST_DATAENGINE_IT_REDIS_ADDR"))
	next := second.receive(t)
	if next.Fence <= initial.Fence {
		t.Fatal("partition did not advance fence")
	}
	partitioned := first.command(t, "status")
	if partitioned.Failures == 0 {
		t.Fatalf("no actual renewal errors observed: %+v", partitioned)
	}
	proxy.enable(t, true)
	released := first.command(t, "release")
	deadline := time.Now().Add(5 * time.Second)
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for released.Error != "" && !strings.Contains(released.Error, "not owned") && time.Now().Before(deadline) {
		<-ticker.C
		released = first.command(t, "release")
	}
	if !strings.Contains(released.Error, "not owned") || released.Held {
		t.Fatalf("old release=%+v", released)
	}
	// 旧进程恢复后不能清掉新 owner；新进程仍能续租并正常释放。
	if current := second.command(t, "status"); !current.Held || current.Fence != next.Fence {
		t.Fatalf("new holder=%+v", current)
	}
	if result := second.command(t, "release"); result.Error != "" {
		t.Fatalf("new release=%+v", result)
	}
	t.Logf("partition -> new owner=%v fence=%d->%d renewal_errors=%d", time.Since(started), initial.Fence, next.Fence, partitioned.Failures)
	first.quit(t)
	second.quit(t)
}

func TestRealRemoteColdAdmissionDeadlineUnderLatency(t *testing.T) {
	r := realRemoteRedis(t)
	key := remoteRedisKey(t, r)
	proxy := newLeaseProxy(t)
	client, err := redisdriver.NewClient(fredis.DefaultConfig(proxy.addr))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	warm, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	_, err = client.Eval(warm, `return 1`, nil)
	cancel()
	if err != nil {
		t.Fatal(err)
	}
	proxyRequest(t, http.MethodPost, proxy.base+"/proxies/"+proxy.name+"/toxics", map[string]any{"name": "slow-reply", "type": "latency", "stream": "downstream", "toxicity": 1, "attributes": map[string]any{"latency": 1500, "jitter": 0}})
	registerAdmissionKind()
	cfg := DefaultConfig()
	cfg.OpTimeout = time.Second
	manager := NewManager(NewVersionedLockFactory(client), cfg, 1000)
	loader := newRemoteTestLoader()
	live := newTestRemoteEntity(7399, 1, 236)
	loader.add(live)
	manager.SetBackend(loader)
	manager.SetOwnershipStore(NewRedisMarker(client, key))
	defer manager.StopFinalizer(context.Background())
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err = manager.PrepareRemoteWriteBatch(ctx, []int64{live.GUId()})
	elapsed := time.Since(started)
	if err == nil {
		t.Fatal("delayed authority admitted a write")
	}
	if elapsed > 400*time.Millisecond {
		t.Fatalf("50ms caller deadline bypassed: %v err=%v", elapsed, err)
	}
	t.Logf("cold admission: caller=50ms injected latency=1500ms elapsed=%v err=%v", elapsed, err)
}

// 真正两个独立业务进程：旧进程断 Redis 后仍能访问 Mongo，新进程接管后拒绝其提交。
func TestRealRemoteDurableAuthorityProcessPartition(t *testing.T) {
	r := realRemoteRedis(t)
	uri := os.Getenv("ROOST_DATAENGINE_IT_MONGO_URI")
	if uri == "" {
		t.Fatal("isolated Mongo required")
	}
	mongo, err := mongodriver.NewClient(fmongo.DefaultConfig(uri), mongodriver.IndexMigrationPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	defer mongo.Close(context.Background())
	database := fmt.Sprintf("roost_remote_process_%d", time.Now().UnixNano())
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := mongo.Database(database).Drop(ctx); err != nil {
			t.Error(err)
		}
	}()
	store := NewMongoCommitter(mongo, database, 7, 0)
	entity.MustRegisterEntityKindDefs(entity.EntityKindDef{Kind: 198, Category: 1, RemotePolicy: entity.RemotePolicyManaged})
	id, _ := entity.BuildEntityID(998, 198)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	lease, err := store.ClaimOwnership(ctx, id, 7)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.EnterSharedExpected(ctx, id, lease); err != nil {
		t.Fatal(err)
	}
	key := remoteRedisKey(t, r)
	proxy := newLeaseProxy(t)
	env := "ROOST_REMOTE_CHILD_AUTHORITY_DB=" + database
	first := startLeaseProcess(t, key, proxy.addr, env)
	initial := first.receive(t)
	proxy.enable(t, false)
	second := startLeaseProcess(t, key, os.Getenv("ROOST_DATAENGINE_IT_REDIS_ADDR"), env)
	next := second.receive(t)
	if next.Fence <= initial.Fence {
		t.Fatal("durable fence did not advance")
	}
	if stale := first.command(t, "commit"); !stale.Rejected {
		t.Fatalf("stale process commit: %+v", stale)
	}
	if fresh := second.command(t, "commit"); fresh.Error != "" {
		t.Fatalf("new process commit: %+v", fresh)
	}
	proxy.enable(t, true)
	if stale := first.command(t, "commit"); !stale.Rejected {
		t.Fatalf("reconnected stale process commit: %+v", stale)
	}
	first.quit(t)
	second.quit(t)
	t.Logf("two independent writers: durable fence %d -> %d, old rejected before/after reconnect, new committed", initial.Fence, next.Fence)
}
