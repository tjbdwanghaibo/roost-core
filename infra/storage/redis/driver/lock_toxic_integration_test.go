//go:build integration

package driver

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	fredis "github.com/tjbdwanghaibo/roost-core/infra/storage/redis"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"
)

// toxiproxyClient drives one proxy this test created on the environment's
// toxiproxy. RR-20261005-NC-208: the suite used the environment's shared
// "redis" proxy and POST /reset, which removes every toxic on every proxy of
// that toxiproxy — other sessions' faults included. Each test now owns a
// uniquely named proxy on an ephemeral port, adds toxics only to it, heals by
// deleting only its own toxics and deletes the proxy at cleanup (the pattern
// versionstore and mongo/driver already use).
type toxiproxyClient struct{ base, name string }

func (c toxiproxyClient) call(t *testing.T, method, path string, body any) []byte {
	t.Helper()
	var payload bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&payload).Encode(body); err != nil {
			t.Fatal(err)
		}
	}
	req, err := http.NewRequest(method, strings.TrimRight(c.base, "/")+path, &payload)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("toxiproxy %s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	var out bytes.Buffer
	_, _ = out.ReadFrom(resp.Body)
	if resp.StatusCode >= 300 && !(method == http.MethodDelete && resp.StatusCode == http.StatusNotFound) {
		t.Fatalf("toxiproxy %s %s: status %d %s", method, path, resp.StatusCode, out.String())
	}
	return out.Bytes()
}

// addToxic adds a downstream toxic to this test's own proxy.
func (c toxiproxyClient) addToxic(t *testing.T, name, kind string, attributes map[string]any) {
	t.Helper()
	c.call(t, http.MethodPost, "/proxies/"+c.name+"/toxics", map[string]any{
		"name": name, "type": kind, "stream": "downstream", "toxicity": 1.0, "attributes": attributes,
	})
}

// heal removes every toxic on this test's own proxy and nothing else.
func (c toxiproxyClient) heal(t *testing.T) {
	t.Helper()
	var toxics []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(c.call(t, http.MethodGet, "/proxies/"+c.name+"/toxics", nil), &toxics); err != nil {
		t.Fatalf("toxiproxy list toxics of %s: %v", c.name, err)
	}
	for _, toxic := range toxics {
		c.call(t, http.MethodDelete, "/proxies/"+c.name+"/toxics/"+toxic.Name, nil)
	}
}

// toxicRedis returns a go-redis client that reaches the isolated Redis through
// a proxy of its own on the environment's toxiproxy, plus that proxy; it skips
// without toxiproxy and fails when ROOST_IT_TOXIPROXY=1 demands it.
func toxicRedis(t *testing.T) (goredis.UniversalClient, toxiproxyClient) {
	t.Helper()
	if os.Getenv("ROOST_DATAENGINE_IT") != "1" {
		t.Skip("set ROOST_DATAENGINE_IT=1 or use scripts/integration/dataengine-env.sh test")
	}
	api := os.Getenv("ROOST_DATAENGINE_IT_TOXIPROXY_URL")
	upstream := os.Getenv("ROOST_DATAENGINE_IT_REDIS_ADDR")
	if api == "" || upstream == "" {
		if os.Getenv("ROOST_IT_TOXIPROXY") == "1" {
			t.Fatal("ROOST_IT_TOXIPROXY=1 but the environment exported no toxiproxy / isolated Redis; install toxiproxy-server and rerun dataengine-env.sh up")
		}
		t.Skip("toxiproxy-server not installed; network fault tests need it (brew install toxiproxy)")
	}
	proxy := toxiproxyClient{base: api, name: fmt.Sprintf("redis-driver-toxic-%d-%s", os.Getpid(), rand.Text())}
	var created struct {
		Listen string `json:"listen"`
	}
	raw := proxy.call(t, http.MethodPost, "/proxies", map[string]any{"name": proxy.name, "listen": "127.0.0.1:0", "upstream": upstream, "enabled": true})
	if err := json.Unmarshal(raw, &created); err != nil || created.Listen == "" {
		t.Fatalf("toxiproxy create %s: %v %s", proxy.name, err, raw)
	}
	t.Cleanup(func() { proxy.call(t, http.MethodDelete, "/proxies/"+proxy.name, nil) })
	// Built through the kit's own constructor so the fixture carries the
	// production client options (context deadlines on the wire included),
	// not a hand-rolled approximation of them.
	rdb := NewRedisClient(&fredis.Config{Addr: created.Listen, DialTimeout: 2 * time.Second, ReadTimeout: 2 * time.Second, WriteTimeout: 2 * time.Second}).rdb
	t.Cleanup(func() { _ = rdb.Close() })
	if err := rdb.Ping(context.Background()).Err(); err != nil {
		t.Fatalf("proxied redis: %v", err)
	}
	return rdb, proxy
}

// toxicLockKey 给每次运行一个独立的锁键，并在用例结束时删除它。
// OPEN-ITEMS B01 / W-2026-09-22-01：原来三条用例共用固定键（toxic:lock / toxic:acquire / toxic:slow），
// 上一次运行结尾 Acquire 成功后留下的键（TTL 5s）会被下一次运行撞上——和解用的 Release 只删自己 token 的键，
// 删不掉上一轮的，于是 `-count>1` 或相邻两次运行报 "key still held after reconciliation"，把用例污染显成锁缺陷。
func toxicLockKey(t *testing.T, rdb goredis.UniversalClient, name string) string {
	t.Helper()
	key := fmt.Sprintf("toxic:%s:%d:%s", name, os.Getpid(), rand.Text())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = rdb.Del(ctx, key).Err()
	})
	return key
}

// Invariant 2 at the network level: a Release whose reply is swallowed by the
// network (toxiproxy `timeout`: data dropped, connection held open) leaves the
// lock UNCERTAIN — Redis may or may not have executed it. Until a Release has
// reconciled, the same lock object refuses to Acquire, and a second owner is
// still kept out while the key lives. Once the network heals, Release
// reconciles with the value-guarded delete and the lock is reusable. This
// is the U-0012 contract driven by a real dropped reply rather than a scripted
// client.
func TestToxicRedisDroppedReleaseReplyLeavesTheLockUncertainUntilReconciled(t *testing.T) {
	rdb, proxy := toxicRedis(t)
	ctx := context.Background()
	factory := NewDistLockFactory(rdb)
	key := toxicLockKey(t, rdb, "lock")
	lock := factory.NewLock(key, 5*time.Second)
	if ok, err := lock.Acquire(ctx); err != nil || !ok {
		t.Fatalf("acquire: ok=%v err=%v", ok, err)
	}

	// Swallow every reply from Redis.
	proxy.addToxic(t, "blackhole", "timeout", map[string]any{"timeout": 0})
	releaseCtx, cancel := context.WithTimeout(ctx, 700*time.Millisecond)
	err := lock.Release(releaseCtx)
	cancel()
	t.Logf("Release with swallowed reply: err=%v state=%d", err, lock.(*distLock).state)
	if err == nil {
		t.Fatal("Release reported success although its reply never arrived")
	}
	if ok, err := lock.Acquire(ctx); !errors.Is(err, ErrDistLockStateUncertain) || ok {
		t.Fatalf("Acquire after a lost Release reply: ok=%v err=%v, want ErrDistLockStateUncertain", ok, err)
	}

	proxy.heal(t)
	// A second owner: the key is still held (the lost Release did not run) or
	// already gone (it did); either way the answer is honest, never a crash.
	other := factory.NewLock(key, 5*time.Second)
	otherOK, err := other.Acquire(ctx)
	if err != nil {
		t.Fatalf("second owner acquire after heal: %v", err)
	}
	// Reconcile: a value-guarded delete that only removes our own token.
	err = lock.Release(ctx)
	if err != nil && !errors.Is(err, fredis.ErrLockNotHeld) {
		t.Fatalf("reconciling Release after heal: %v", err)
	}
	if otherOK {
		// Our reconcile must not have removed the other owner's lock.
		if val, err := rdb.Get(ctx, key).Result(); err != nil || val == "" {
			t.Fatalf("reconcile deleted another owner's lock: val=%q err=%v", val, err)
		}
		if err := other.Release(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if ok, err := lock.Acquire(ctx); err != nil || !ok {
		t.Fatalf("lock is not reusable after reconciliation: ok=%v err=%v", ok, err)
	}
	if err := lock.Release(ctx); err != nil {
		t.Fatal(err)
	}
}

// A lost SETNX reply is the mirror image: the lock may be held in Redis with
// a token only this object knows. It must not be re-acquired (that would mint
// a second token and orphan the first until TTL), and reconciliation through
// Release must free it.
func TestToxicRedisDroppedAcquireReplyIsReconciledNotRetried(t *testing.T) {
	rdb, proxy := toxicRedis(t)
	ctx := context.Background()
	key := toxicLockKey(t, rdb, "acquire")
	lock := NewDistLockFactory(rdb).NewLock(key, 5*time.Second)
	proxy.addToxic(t, "blackhole", "timeout", map[string]any{"timeout": 0})
	acquireCtx, cancel := context.WithTimeout(ctx, 700*time.Millisecond)
	ok, err := lock.Acquire(acquireCtx)
	cancel()
	if err == nil || ok {
		t.Fatalf("Acquire with a swallowed reply returned ok=%v err=%v", ok, err)
	}
	if ok, err := lock.Acquire(ctx); !errors.Is(err, ErrDistLockStateUncertain) || ok {
		t.Fatalf("re-Acquire after a lost SETNX reply: ok=%v err=%v, want ErrDistLockStateUncertain", ok, err)
	}
	proxy.heal(t)
	if err := lock.Release(ctx); err != nil && !errors.Is(err, fredis.ErrLockNotHeld) {
		t.Fatalf("reconciling Release: %v", err)
	}
	if held, err := rdb.Exists(ctx, key).Result(); err != nil || held != 0 {
		t.Fatalf("key still held after reconciliation: exists=%d err=%v", held, err)
	}
	if ok, err := lock.Acquire(ctx); err != nil || !ok {
		t.Fatalf("lock unusable after reconciliation: ok=%v err=%v", ok, err)
	}
	if err := lock.Release(ctx); err != nil {
		t.Fatal(err)
	}
}

// Latency, not loss: with three seconds added to every Redis reply, Acquire
// must honour the caller's deadline and come back within it — a lock that
// waits for the slow reply past its deadline is a lock that stalls every
// handler behind it. The lock must also not be left in a state that refuses
// the next Acquire once the network is fast again: a timed-out SETNX is
// uncertain, and reconciliation through Release clears it.
func TestToxicRedisLatencyKeepsAcquireWithinItsDeadline(t *testing.T) {
	rdb, proxy := toxicRedis(t)
	ctx := context.Background()
	factory := NewDistLockFactory(rdb)
	lock := factory.NewLock(toxicLockKey(t, rdb, "slow"), 5*time.Second)

	proxy.addToxic(t, "slow", "latency", map[string]any{"latency": 3000, "jitter": 0})
	acquireCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	started := time.Now()
	ok, err := lock.Acquire(acquireCtx)
	cancel()
	elapsed := time.Since(started)
	t.Logf("Acquire under 3s latency: ok=%v err=%v elapsed=%s", ok, err, elapsed)
	if ok || err == nil {
		t.Fatalf("Acquire reported success (ok=%v err=%v) although the reply could not have arrived within the deadline", ok, err)
	}
	if elapsed > 1500*time.Millisecond {
		t.Fatalf("Acquire took %s under a 500ms deadline; it waited for the slow reply instead of honouring the caller", elapsed)
	}

	proxy.heal(t)
	// The timed-out SETNX may or may not have been executed; the lock object
	// says so and is reconciled through Release, never by a blind retry.
	if ok, err := lock.Acquire(ctx); ok && err == nil {
		t.Log("Acquire after heal succeeded directly: the timed-out SETNX had not reached Redis")
	} else if errors.Is(err, ErrDistLockStateUncertain) {
		if err := lock.Release(ctx); err != nil && !errors.Is(err, fredis.ErrLockNotHeld) {
			t.Fatalf("reconciling Release after heal: %v", err)
		}
		if ok, err := lock.Acquire(ctx); err != nil || !ok {
			t.Fatalf("lock not reusable after reconciliation: ok=%v err=%v", ok, err)
		}
	} else {
		t.Fatalf("Acquire after heal: ok=%v err=%v", ok, err)
	}
	if err := lock.Release(ctx); err != nil {
		t.Fatal(err)
	}
}
