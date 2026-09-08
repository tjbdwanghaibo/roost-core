package redis

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"
	fredis "github.com/tjbdwanghaibo/roost-core/redis"
)

// scriptedRedis stands in for the UniversalClient for the two commands distLock
// issues. Embedding the interface leaves every other method a nil-pointer
// panic, which is what a double should do for a call the test did not expect.
// It evaluates SETNX and the release script against one map, and can lose the
// reply of a SETNX that the server did or did not apply — the ambiguity the
// uncertain state exists for.
type scriptedRedis struct {
	goredis.UniversalClient
	mu   sync.Mutex
	held map[string]string
	// loseNextSetNX makes the next SETNX return an error. applyBeforeLosing
	// says whether the server applied it first (reply lost) or not (never
	// reached the server).
	loseNextSetNX     bool
	applyBeforeLosing bool
	evalErr           error
}

func newScriptedRedis() *scriptedRedis { return &scriptedRedis{held: map[string]string{}} }

func (r *scriptedRedis) SetNX(_ context.Context, key string, value any, _ time.Duration) *goredis.BoolCmd {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.loseNextSetNX {
		r.loseNextSetNX = false
		if r.applyBeforeLosing {
			if _, taken := r.held[key]; !taken {
				r.held[key] = fmt.Sprint(value)
			}
		}
		return goredis.NewBoolResult(false, errors.New("scripted redis: reply lost"))
	}
	if _, taken := r.held[key]; taken {
		return goredis.NewBoolResult(false, nil)
	}
	r.held[key] = fmt.Sprint(value)
	return goredis.NewBoolResult(true, nil)
}

func (r *scriptedRedis) Eval(_ context.Context, script string, keys []string, args ...any) *goredis.Cmd {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.evalErr != nil {
		return goredis.NewCmdResult(nil, r.evalErr)
	}
	if script != releaseLockScript {
		return goredis.NewCmdResult(nil, fmt.Errorf("scripted redis: unexpected script"))
	}
	if r.held[keys[0]] == fmt.Sprint(args[0]) {
		delete(r.held, keys[0])
		return goredis.NewCmdResult(int64(1), nil)
	}
	return goredis.NewCmdResult(int64(0), nil)
}

func (r *scriptedRedis) owner(key string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.held[key]
}

// FEATURE_LOGIC M7 item 1: a TTL below one millisecond is a configuration
// error, never a lock that lasts forever. Reverting the check made no test red
// before this one existed.
func TestDistLockRefusesATTLBelowOneMillisecond(t *testing.T) {
	factory := newDistLockFactory(newScriptedRedis())
	for _, ttl := range []time.Duration{0, -time.Second, 500 * time.Microsecond} {
		lock := factory.NewLock("k", ttl)
		if ok, err := lock.Acquire(context.Background()); ok || !errors.Is(err, ErrDistLockConfig) {
			t.Fatalf("ttl %s: ok=%v err=%v, want ErrDistLockConfig", ttl, ok, err)
		}
	}
	if ok, err := factory.NewLock("k", time.Millisecond).Acquire(context.Background()); err != nil || !ok {
		t.Fatalf("a 1ms ttl was refused: ok=%v err=%v", ok, err)
	}
}

// M7 item 2: the owner token is minted per acquisition, so a stale holder
// from an earlier acquisition cannot present the current one's identity.
func TestDistLockOwnerTokenIsPerAcquisition(t *testing.T) {
	redis := newScriptedRedis()
	lock := newDistLockFactory(redis).NewLock("k", time.Second)
	ctx := context.Background()
	if ok, err := lock.Acquire(ctx); err != nil || !ok {
		t.Fatalf("acquire: ok=%v err=%v", ok, err)
	}
	first := redis.owner("k")
	if err := lock.Release(ctx); err != nil {
		t.Fatal(err)
	}
	if ok, err := lock.Acquire(ctx); err != nil || !ok {
		t.Fatalf("reacquire: ok=%v err=%v", ok, err)
	}
	second := redis.owner("k")
	if first == "" || second == "" || first == second {
		t.Fatalf("owner token reused across acquisitions: %q then %q", first, second)
	}
}

// M7 item 3: a lost SETNX reply leaves ownership uncertain. The same object
// must not simply acquire again — that could stack a second lease on top of
// one the server already granted — until a value-guarded Release reconciles.
// Both outcomes of the ambiguity are exercised: the server applied the SETNX
// (Release frees it) and it never arrived (Release reports nothing held).
func TestDistLockUncertainStateBlocksReuseUntilReleaseReconciles(t *testing.T) {
	for _, applied := range []bool{true, false} {
		t.Run(fmt.Sprintf("applied=%v", applied), func(t *testing.T) {
			redis := newScriptedRedis()
			redis.loseNextSetNX, redis.applyBeforeLosing = true, applied
			lock := newDistLockFactory(redis).NewLock("k", time.Second)
			ctx := context.Background()

			if ok, err := lock.Acquire(ctx); ok || err == nil {
				t.Fatalf("a lost reply was reported as ok=%v err=%v", ok, err)
			}
			if ok, err := lock.Acquire(ctx); ok || !errors.Is(err, ErrDistLockStateUncertain) {
				t.Fatalf("acquire while uncertain: ok=%v err=%v, want ErrDistLockStateUncertain", ok, err)
			}
			err := lock.Release(ctx)
			switch {
			case applied && err != nil:
				t.Fatalf("release of a lease the server did grant: %v", err)
			case !applied && !errors.Is(err, fredis.ErrLockNotHeld):
				t.Fatalf("release of a lease that never reached the server: %v, want ErrLockNotHeld", err)
			}
			if owner := redis.owner("k"); owner != "" {
				t.Fatalf("the server still holds %q after reconciliation", owner)
			}
			if ok, err := lock.Acquire(ctx); err != nil || !ok {
				t.Fatalf("acquire after reconciliation: ok=%v err=%v", ok, err)
			}
		})
	}
}

// A Release whose reply is lost is equally uncertain: the lease may or may not
// be gone, so the object stays unusable until a later Release settles it.
func TestDistLockReleaseErrorLeavesTheLockUncertain(t *testing.T) {
	redis := newScriptedRedis()
	lock := newDistLockFactory(redis).NewLock("k", time.Second)
	ctx := context.Background()
	if ok, err := lock.Acquire(ctx); err != nil || !ok {
		t.Fatalf("acquire: ok=%v err=%v", ok, err)
	}
	redis.evalErr = errors.New("scripted redis: reply lost")
	if err := lock.Release(ctx); err == nil {
		t.Fatal("a release whose reply was lost reported success")
	}
	if ok, err := lock.Acquire(ctx); ok || !errors.Is(err, ErrDistLockStateUncertain) {
		t.Fatalf("acquire after an uncertain release: ok=%v err=%v", ok, err)
	}
	redis.evalErr = nil
	if err := lock.Release(ctx); err != nil {
		t.Fatalf("reconciling release: %v", err)
	}
	if ok, err := lock.Acquire(ctx); err != nil || !ok {
		t.Fatalf("acquire after reconciliation: ok=%v err=%v", ok, err)
	}
}
