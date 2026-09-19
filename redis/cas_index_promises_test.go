package redis

import (
	"context"
	"errors"
	"os"
	"strconv"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"
)

// RR-20260919-04：一个记录和"它在不在待办集合里"必须是同一次写。
//
// platform 的订单先持久化、索引后写，两次写之间进程退出或第二次写失败，
// 就留下一笔后台循环永远枚举不到的已付款订单。修法是让 CAS 脚本同时维护
// 一个有序集合索引：记录写成功才动索引，记录没写成功索引一动不动。
//
// 下面两组：命令校验走替身，**语义走真 Redis**。这里必须用真 Redis——
// Lua 的 tonumber 与 Go 的数值解析不是一回事（kit 的两个跨语言缺陷就是这样
// 长期不可见的），而索引的分数正是一个数。

func TestCompareAndSetRefusesAnIncompleteIndex(t *testing.T) {
	ctx := context.Background()
	runner := &recordingScriptRunner{reply: []any{int64(1), "v2"}}
	for name, index := range map[string]*CompareAndSetIndex{
		"no key":    {Member: "m"},
		"no member": {Key: "idx"},
	} {
		cmd := CompareAndSetCommand{Key: "k", Next: []byte("v"), Index: index}
		if _, err := CompareAndSet(ctx, runner, cmd); !errors.Is(err, ErrCASInvalidCommand) {
			t.Errorf("%s: error = %v, want ErrCASInvalidCommand", name, err)
		}
	}
	if runner.calls != 0 {
		t.Errorf("an incomplete index reached Redis %d times", runner.calls)
	}
}

func integrationRedis(t *testing.T) *goredis.Client {
	t.Helper()
	addr := os.Getenv("ROOST_REDIS_TEST_ADDR")
	if addr == "" {
		t.Skip("ROOST_REDIS_TEST_ADDR is not set")
	}
	client := goredis.NewClient(&goredis.Options{Addr: addr})
	if err := client.Ping(context.Background()).Err(); err != nil {
		t.Skipf("redis at %s is not reachable: %v", addr, err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// evalRunner adapts a go-redis client to ScriptRunner, so the test drives the
// same script the product does.
type evalRunner struct{ client *goredis.Client }

func (r evalRunner) Eval(ctx context.Context, script string, keys []string, args ...any) (any, error) {
	return r.client.Eval(ctx, script, keys, args...).Result()
}

func TestIndexIsMaintainedInTheSameWriteIntegration(t *testing.T) {
	client := integrationRedis(t)
	ctx := context.Background()
	unique := strconv.FormatInt(time.Now().UnixNano(), 36)
	valueKey := "roost:test:cas:" + unique + ":value"
	indexKey := "roost:test:cas:" + unique + ":index"
	t.Cleanup(func() { client.Del(ctx, valueKey, indexKey) })
	runner := evalRunner{client: client}

	// Create with an index entry: both or neither.
	result, err := CompareAndSet(ctx, runner, CompareAndSetCommand{
		Key: valueKey, Next: []byte("v1"),
		Index: &CompareAndSetIndex{Key: indexKey, Member: "order-1", Score: 1700},
	})
	if err != nil || !result.Applied {
		t.Fatalf("create: applied=%v err=%v", result.Applied, err)
	}
	score, err := client.ZScore(ctx, indexKey, "order-1").Result()
	if err != nil || score != 1700 {
		t.Fatalf("index score = %v (err %v), want 1700", score, err)
	}

	// A write that loses the compare must not touch the index.
	lost, err := CompareAndSet(ctx, runner, CompareAndSetCommand{
		Key: valueKey, Expected: []byte("not what is stored"), Next: []byte("v2"),
		Index: &CompareAndSetIndex{Key: indexKey, Member: "order-1", Remove: true},
	})
	if err != nil {
		t.Fatalf("losing write: %v", err)
	}
	if lost.Applied {
		t.Fatal("a compare against the wrong value was applied")
	}
	if exists, _ := client.ZScore(ctx, indexKey, "order-1").Result(); exists != 1700 {
		t.Fatal("a write that did not happen still changed the index")
	}

	// A successful write can move the score...
	if _, err := CompareAndSet(ctx, runner, CompareAndSetCommand{
		Key: valueKey, Expected: []byte("v1"), Next: []byte("v2"),
		Index: &CompareAndSetIndex{Key: indexKey, Member: "order-1", Score: 1800},
	}); err != nil {
		t.Fatalf("update: %v", err)
	}
	if score, _ := client.ZScore(ctx, indexKey, "order-1").Result(); score != 1800 {
		t.Fatalf("index score = %v after an update, want 1800", score)
	}

	// ...and retire the entry when the record stops being pending.
	if _, err := CompareAndSet(ctx, runner, CompareAndSetCommand{
		Key: valueKey, Expected: []byte("v2"), Next: []byte("v3"),
		Index: &CompareAndSetIndex{Key: indexKey, Member: "order-1", Remove: true},
	}); err != nil {
		t.Fatalf("retire: %v", err)
	}
	if _, err := client.ZScore(ctx, indexKey, "order-1").Result(); !errors.Is(err, goredis.Nil) {
		t.Fatalf("the entry was not retired: %v", err)
	}
	if stored, _ := client.Get(ctx, valueKey).Result(); stored != "v3" {
		t.Fatalf("the value is %q, want the last write", stored)
	}
}

// A score is a number on the Lua side too: one that arrives as a float with
// many digits must come back as the same number, not as a rounded string.
func TestIndexScorePrecisionSurvivesLuaIntegration(t *testing.T) {
	client := integrationRedis(t)
	ctx := context.Background()
	unique := strconv.FormatInt(time.Now().UnixNano(), 36)
	valueKey := "roost:test:cas:" + unique + ":value"
	indexKey := "roost:test:cas:" + unique + ":index"
	t.Cleanup(func() { client.Del(ctx, valueKey, indexKey) })

	const due = float64(1789781700)
	if _, err := CompareAndSet(ctx, evalRunner{client: client}, CompareAndSetCommand{
		Key: valueKey, Next: []byte("v"),
		Index: &CompareAndSetIndex{Key: indexKey, Member: "order", Score: due},
	}); err != nil {
		t.Fatal(err)
	}
	score, err := client.ZScore(ctx, indexKey, "order").Result()
	if err != nil {
		t.Fatal(err)
	}
	if score != due {
		t.Fatalf("score came back as %f, want %f: a unix second must survive the script", score, due)
	}
}
