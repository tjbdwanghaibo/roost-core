package redis

import (
	"context"
	"crypto/rand"
	"errors"
	"os"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"
	rediscore "github.com/tjbdwanghaibo/roost-core/redis"
)

func TestDistLockExpiryAndReacquireIntegration(t *testing.T) {
	addr := os.Getenv("ROOST_REDIS_TEST_ADDR")
	if addr == "" {
		t.Skip("ROOST_REDIS_TEST_ADDR is not set")
	}
	ctx := context.Background()
	client := goredis.NewClient(&goredis.Options{Addr: addr})
	defer client.Close()
	if err := client.Ping(ctx).Err(); err != nil {
		t.Fatalf("redis ping: %v", err)
	}
	key := "roost:test:distlock:" + rand.Text()
	defer client.Del(ctx, key)

	factory := newDistLockFactory(client)
	first := factory.NewLock(key, 80*time.Millisecond).(*distLock)
	second := factory.NewLock(key, time.Second).(*distLock)
	if ok, err := first.Acquire(ctx); err != nil || !ok {
		t.Fatalf("first acquire: ok=%v err=%v", ok, err)
	}
	if ok, err := first.Acquire(ctx); !errors.Is(err, ErrDistLockAlreadyActive) || ok {
		t.Fatalf("duplicate acquire: ok=%v err=%v", ok, err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		ok, err := second.Acquire(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("second lock did not acquire after TTL")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := first.Release(ctx); !errors.Is(err, rediscore.ErrLockNotHeld) {
		t.Fatalf("stale release error=%v, want ErrLockNotHeld", err)
	}
	owner, err := client.Get(ctx, key).Result()
	if err != nil {
		t.Fatal(err)
	}
	if owner != second.value {
		t.Fatalf("stale release changed new owner: got=%q want=%q", owner, second.value)
	}
	if err := second.Release(ctx); err != nil {
		t.Fatal(err)
	}
}
