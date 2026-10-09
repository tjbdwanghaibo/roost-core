package rank

import (
	"context"
	"strings"
	"testing"
	"time"
)

// shapedRedis answers the owner-read or swap script with an arbitrary value
// so the store's shape checks can be exercised; everything else goes to the
// real fake.
type shapedRedis struct {
	*fakeRedis
	ownerShape any
	swapShape  any
}

func (r *shapedRedis) Eval(ctx context.Context, script string, keys []string, args ...any) (any, error) {
	if r.swapShape != nil && script == swapScript {
		return r.swapShape, nil
	}
	if r.ownerShape != nil && strings.Contains(script, `ARGV[1] .. ":applied"`) {
		return r.ownerShape, nil
	}
	return r.fakeRedis.Eval(ctx, script, keys, args...)
}

// U-0152 · C2 · rank redis_store.go:211 / 224（U-0150 留待）：Lua 脚本返回的不是两元素数组时，
// 单读与换分都要报"unexpected ... result"，而不是对错误形状做索引。
func TestStoreRefusesLuaResultsOfTheWrongShape(t *testing.T) {
	ctx := context.Background()
	now := time.Unix(1_700_000_000, 0)
	build := func(t *testing.T, redis *shapedRedis) *RedisStore {
		t.Helper()
		store, err := NewRedisStore(redis, RedisConfig{Prefix: "test:rank", Now: func() time.Time { return now }})
		if err != nil {
			t.Fatal(err)
		}
		return store
	}
	ownerStore := build(t, &shapedRedis{fakeRedis: newFakeRedis(), ownerShape: "bogus"})
	if _, _, err := ownerStore.Rank(ctx, arena(), 7); err == nil || !strings.Contains(err.Error(), "unexpected owner read result string") {
		t.Fatalf("Rank with a malformed owner read = %v", err)
	}
	swapStore := build(t, &shapedRedis{fakeRedis: newFakeRedis(), swapShape: []any{int64(1)}})
	if _, err := swapStore.Submit(ctx, arena(), Score{OwnerID: 7, Value: 10}, UpdateMax, "r-1"); err == nil || !strings.Contains(err.Error(), "unexpected swap result") {
		t.Fatalf("Submit with a malformed swap result = %v", err)
	}
	healthy := build(t, &shapedRedis{fakeRedis: newFakeRedis()})
	if _, err := healthy.Submit(ctx, arena(), Score{OwnerID: 7, Value: 10}, UpdateMax, "r-1"); err != nil {
		t.Fatalf("Submit through the untouched fake = %v", err)
	}
}
