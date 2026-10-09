package cache

// A2（驱动不重放写命令）：RedisJSONHashStore.Set 与 RedisRawSortedSetStore.SetScore 都是“写 + EXPIRE”
// 两步。驱动不再替调用方重放写命令之后，回复丢失的 HSET / ZADD 以错误返回——而它可能已经在服务端
// 建出了新键。修前两处都在写失败时直接返回、不发 EXPIRE，这个键就永不过期（缓存键的 TTL 承诺被打破）。
// 承诺：写的结果未知时仍补发 EXPIRE（键不存在时无副作用），返回的仍是写的错误。

import (
	"context"
	"errors"
	"testing"
	"time"

	fredis "github.com/tjbdwanghaibo/roost-core/infra/storage/redis"
)

// lostReplyRedis 让写命令在服务端生效后丢掉回复；记录每个键最后一次 EXPIRE。
type lostReplyRedis struct {
	fredis.IRedis
	applied map[string]bool
	ttl     map[string]time.Duration
}

var errReplyLost = errors.New("redis: reply lost (EOF)")

func newLostReplyRedis() *lostReplyRedis {
	return &lostReplyRedis{applied: map[string]bool{}, ttl: map[string]time.Duration{}}
}

func (r *lostReplyRedis) HGet(context.Context, string, string) ([]byte, error) {
	return nil, fredis.ErrNil
}
func (r *lostReplyRedis) HSet(_ context.Context, key string, _ ...any) error {
	r.applied[key] = true
	return errReplyLost
}
func (r *lostReplyRedis) ZAdd(_ context.Context, key string, _ ...fredis.Z) (int64, error) {
	r.applied[key] = true
	return 0, errReplyLost
}
func (r *lostReplyRedis) Expire(_ context.Context, key string, ttl time.Duration) (bool, error) {
	if !r.applied[key] {
		return false, nil
	}
	r.ttl[key] = ttl
	return true, nil
}

type lostReplyValue struct {
	ID string `json:"id"`
}

func TestAWriteWhoseReplyIsLostStillGetsItsTTL(t *testing.T) {
	ctx := context.Background()
	const ttl = time.Minute

	t.Run("hash", func(t *testing.T) {
		redis := newLostReplyRedis()
		store := NewRedisJSONHashStore(redis, ttl,
			func(id string) RedisHashKey { return RedisHashKey{Key: "h:" + id, Field: "v"} },
			StoreConfig[string, lostReplyValue]{KeyOf: func(v lostReplyValue) string { return v.ID }})
		err := store.Set(ctx, lostReplyValue{ID: "a"})
		if !errors.Is(err, errReplyLost) {
			t.Fatalf("Set err=%v, want the write's unknown-result error", err)
		}
		if got := redis.ttl["h:a"]; got != ttl {
			t.Fatalf("HSET reached Redis but the key carries TTL %v (want %v): it never expires", got, ttl)
		}
	})
	t.Run("sorted set", func(t *testing.T) {
		redis := newLostReplyRedis()
		store := NewRedisRawSortedSetStore[string](redis, "z:rank", ttl)
		err := store.SetScore(ctx, "m", 1)
		if !errors.Is(err, errReplyLost) {
			t.Fatalf("SetScore err=%v, want the write's unknown-result error", err)
		}
		if got := redis.ttl["z:rank"]; got != ttl {
			t.Fatalf("ZADD reached Redis but the key carries TTL %v (want %v): it never expires", got, ttl)
		}
	})
}
