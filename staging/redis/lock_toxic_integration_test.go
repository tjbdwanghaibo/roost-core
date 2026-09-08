//go:build integration

package redis

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"
	fredis "github.com/tjbdwanghaibo/roost-core/redis"
)

type toxiproxyClient struct{ base string }

func (c toxiproxyClient) do(t *testing.T, method, path string, body any) {
	t.Helper()
	var payload bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&payload).Encode(body); err != nil {
			t.Fatal(err)
		}
	}
	req, err := http.NewRequest(method, c.base+path, &payload)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("toxiproxy %s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		t.Fatalf("toxiproxy %s %s: status %d", method, path, resp.StatusCode)
	}
}

// toxicRedis returns a go-redis client that reaches the isolated Redis through
// toxiproxy, plus the toxiproxy API; it skips without toxiproxy and fails
// when ROOST_IT_TOXIPROXY=1 demands it.
func toxicRedis(t *testing.T) (goredis.UniversalClient, toxiproxyClient) {
	t.Helper()
	if os.Getenv("ROOST_DATAENGINE_IT") != "1" {
		t.Skip("set ROOST_DATAENGINE_IT=1 or use scripts/integration/dataengine-env.sh test")
	}
	api := os.Getenv("ROOST_DATAENGINE_IT_TOXIPROXY_URL")
	addr := os.Getenv("ROOST_DATAENGINE_IT_REDIS_PROXIED_ADDR")
	if api == "" || addr == "" {
		if os.Getenv("ROOST_IT_TOXIPROXY") == "1" {
			t.Fatal("ROOST_IT_TOXIPROXY=1 but the environment exported no proxied Redis; install toxiproxy-server and rerun dataengine-env.sh up")
		}
		t.Skip("toxiproxy-server not installed; network fault tests need it (brew install toxiproxy)")
	}
	proxy := toxiproxyClient{base: api}
	proxy.do(t, http.MethodPost, "/reset", nil)
	t.Cleanup(func() { proxy.do(t, http.MethodPost, "/reset", nil) })
	// Built through the kit's own constructor so the fixture carries the
	// production client options (context deadlines on the wire included),
	// not a hand-rolled approximation of them.
	rdb := newRedisClient(&fredis.Config{Addr: addr, DialTimeout: 2 * time.Second, ReadTimeout: 2 * time.Second, WriteTimeout: 2 * time.Second}).rdb
	t.Cleanup(func() { _ = rdb.Close() })
	if err := rdb.Ping(context.Background()).Err(); err != nil {
		t.Fatalf("proxied redis: %v", err)
	}
	return rdb, proxy
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
	factory := newDistLockFactory(rdb)
	lock := factory.NewLock("toxic:lock", 5*time.Second)
	if ok, err := lock.Acquire(ctx); err != nil || !ok {
		t.Fatalf("acquire: ok=%v err=%v", ok, err)
	}

	// Swallow every reply from Redis.
	proxy.do(t, http.MethodPost, "/proxies/redis/toxics", map[string]any{
		"name": "blackhole", "type": "timeout", "stream": "downstream", "toxicity": 1.0, "attributes": map[string]any{"timeout": 0},
	})
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

	proxy.do(t, http.MethodPost, "/reset", nil)
	// A second owner: the key is still held (the lost Release did not run) or
	// already gone (it did); either way the answer is honest, never a crash.
	other := factory.NewLock("toxic:lock", 5*time.Second)
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
		if val, err := rdb.Get(ctx, "toxic:lock").Result(); err != nil || val == "" {
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
	lock := newDistLockFactory(rdb).NewLock("toxic:acquire", 5*time.Second)
	proxy.do(t, http.MethodPost, "/proxies/redis/toxics", map[string]any{
		"name": "blackhole", "type": "timeout", "stream": "downstream", "toxicity": 1.0, "attributes": map[string]any{"timeout": 0},
	})
	acquireCtx, cancel := context.WithTimeout(ctx, 700*time.Millisecond)
	ok, err := lock.Acquire(acquireCtx)
	cancel()
	if err == nil || ok {
		t.Fatalf("Acquire with a swallowed reply returned ok=%v err=%v", ok, err)
	}
	if ok, err := lock.Acquire(ctx); !errors.Is(err, ErrDistLockStateUncertain) || ok {
		t.Fatalf("re-Acquire after a lost SETNX reply: ok=%v err=%v, want ErrDistLockStateUncertain", ok, err)
	}
	proxy.do(t, http.MethodPost, "/reset", nil)
	if err := lock.Release(ctx); err != nil && !errors.Is(err, fredis.ErrLockNotHeld) {
		t.Fatalf("reconciling Release: %v", err)
	}
	if held, err := rdb.Exists(ctx, "toxic:acquire").Result(); err != nil || held != 0 {
		t.Fatalf("key still held after reconciliation: exists=%d err=%v", held, err)
	}
	if ok, err := lock.Acquire(ctx); err != nil || !ok {
		t.Fatalf("lock unusable after reconciliation: ok=%v err=%v", ok, err)
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
	factory := newDistLockFactory(rdb)
	lock := factory.NewLock("toxic:slow", 5*time.Second)

	proxy.do(t, http.MethodPost, "/proxies/redis/toxics", map[string]any{
		"name": "slow", "type": "latency", "stream": "downstream", "toxicity": 1.0, "attributes": map[string]any{"latency": 3000, "jitter": 0},
	})
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

	proxy.do(t, http.MethodPost, "/reset", nil)
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
