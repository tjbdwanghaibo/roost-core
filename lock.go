package redis

import (
	"context"
	"crypto/rand"
	fredis "github.com/tjbdwanghaibo/cube-core/redis"
	"encoding/hex"
	"time"

	goredis "github.com/redis/go-redis/v9"
)

// Lua scripts for atomic lock operations
const (
	releaseLockScript = `
if redis.call("get", KEYS[1]) == ARGV[1] then
    return redis.call("del", KEYS[1])
else
    return 0
end`

	extendLockScript = `
if redis.call("get", KEYS[1]) == ARGV[1] then
    return redis.call("pexpire", KEYS[1], ARGV[2])
else
    return 0
end`
)

// distLockFactory implements fredis.IDistLockFactory.
type distLockFactory struct {
	rdb goredis.UniversalClient
}

func newDistLockFactory(rdb goredis.UniversalClient) *distLockFactory {
	return &distLockFactory{rdb: rdb}
}

func (f *distLockFactory) NewLock(key string, ttl time.Duration) fredis.IDistLock {
	return &distLock{
		rdb:   f.rdb,
		key:   key,
		ttl:   ttl,
		value: generateLockValue(),
	}
}

var _ fredis.IDistLockFactory = (*distLockFactory)(nil)

// distLock implements fredis.IDistLock.
type distLock struct {
	rdb   goredis.UniversalClient
	key   string
	ttl   time.Duration
	value string // unique value to identify lock owner
}

func (l *distLock) Acquire(ctx context.Context) (bool, error) {
	ok, err := l.rdb.SetNX(ctx, l.key, l.value, l.ttl).Result()
	if err != nil {
		return false, err
	}
	if !ok {
		return false, nil
	}
	return true, nil
}

func (l *distLock) Release(ctx context.Context) error {
	result, err := l.rdb.Eval(ctx, releaseLockScript, []string{l.key}, l.value).Int64()
	if err != nil {
		return err
	}
	if result == 0 {
		return fredis.ErrLockNotHeld
	}
	return nil
}

func (l *distLock) Extend(ctx context.Context, ttl time.Duration) (bool, error) {
	result, err := l.rdb.Eval(ctx, extendLockScript, []string{l.key}, l.value, ttl.Milliseconds()).Int64()
	if err != nil {
		return false, err
	}
	return result == 1, nil
}

var _ fredis.IDistLock = (*distLock)(nil)

func generateLockValue() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}
