package versionstore

// RR-20261006-35：带索引的写，索引分数是 NaN 时 Redis 的 ZADD 报错，但同一脚本里的 SET 已经
// 执行（脚本出错不回滚）：值写进去了、索引没动，调用方却拿到错误。“值与索引同进退”的承诺
// （RR-20260919-04）被打破；A2 ③ 之后服务端错误回复被当作“命令没改键”，这次失败还会被当成确定的。
//
// 承诺：分数在发出前校验，NaN 的写不发出、什么都不改，返回 ErrCASInvalidCommand。
// 替身先改忠实（ZADD 拒绝 NaN 时 SET 已生效），再改实现；真实 Redis 见 write_token_integration_test.go。

import (
	"context"
	"errors"
	"math"
	"testing"

	fredis "github.com/tjbdwanghaibo/roost-core/infra/storage/redis"
)

func TestAnIndexedWriteWithANaNScoreChangesNothing(t *testing.T) {
	fake := newFakeRedis()
	store, err := NewRedisStore[string, counter](fake, RedisConfig[string, counter]{
		Prefix: "nan:", KeyOf: func(key string) string { return key }, Codec: JSONCodec[counter]{},
		Index: &RedisIndex[counter]{Key: "nan:due", Entry: func(c counter) (float64, bool) {
			if c.Total == 1 {
				return math.NaN(), true
			}
			return float64(c.Total), true
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, _, err := store.Create(ctx, "a", counter{Name: "a"}); err != nil {
		t.Fatal(err)
	}
	_, applied, err := store.Update(ctx, "a", increment)
	stored, _, _ := store.Get(ctx, "a")
	if applied || !errors.Is(err, fredis.ErrCASInvalidCommand) || stored.Version != 1 || stored.Value.Total != 0 {
		t.Fatalf("an update whose index score is NaN returned applied=%v err=%v and the store holds %+v (want nothing written, ErrCASInvalidCommand)",
			applied, err, stored)
	}
	if score := fake.index["nan:due"]["a"]; score != 0 {
		t.Fatalf("index entry moved to %v", score)
	}
}
