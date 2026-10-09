//go:build integration

package versionstore_test

// A2 ③（真实 Redis）：写脚本已在服务端执行、回复丢失时，store 用信封里的一次性令牌认出自己的写，
// 调用方拿到那一次的结果；调用方“出错就再调一次”的重试不会让写生效两次，也不会把自己的
// Create 判成“键已被占用”。
//
// 单机：本用例自建、端口随机的 toxiproxy 代理（沿用 lost_reply_integration_test.go），回复只回来
// 1 个字节就断开，新连接拨号前摘掉毒。Cluster：scripts/mirror-local.sh 的私有 3 主 3 从，在
// go-redis 的 ProcessHook 里让 EVAL 真正执行之后把回复换成连接错误（toxiproxy 挡不住 MOVED 之后
// 的直连）。键带唯一前缀（Cluster 用 hash tag 让值与索引同槽），结束时删除。

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"
	fredis "github.com/tjbdwanghaibo/roost-core/infra/storage/redis"
	redisdriver "github.com/tjbdwanghaibo/roost-core/infra/storage/redis/driver"
	"github.com/tjbdwanghaibo/roost-core/infra/storage/versionstore"
)

type tokenItem struct {
	Messages []string `json:"messages"`
	Due      int64    `json:"due"`
}

func appendMessage(message string) versionstore.Mutate[tokenItem] {
	return func(current tokenItem, _ bool) (tokenItem, bool, error) {
		current.Messages = append(append([]string(nil), current.Messages...), message)
		return current, true, nil
	}
}

// singleNodeLostReply returns a client behind this test's own proxy and a
// function that cuts the reply of the next command after one byte.
func singleNodeLostReply(t *testing.T) (fredis.IRedis, func()) {
	t.Helper()
	proxy := newOwnProxy(t)
	cfg := fredis.DefaultConfig(proxy.listen)
	cfg.MinIdleConns = 0
	cfg.PoolSize = 1
	client, err := redisdriver.NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	var armed atomic.Bool
	removeCut := func() { proxy.call(t, http.MethodDelete, "/proxies/"+proxy.name+"/toxics/cut", nil) }
	client.(*redisdriver.Client).Raw().AddHook(onDial(func() {
		if armed.CompareAndSwap(true, false) {
			removeCut()
		}
	}))
	t.Cleanup(removeCut)
	cut := func() {
		armed.Store(true)
		proxy.call(t, http.MethodPost, "/proxies/"+proxy.name+"/toxics", map[string]any{
			"name": "cut", "type": "limit_data", "stream": "downstream", "attributes": map[string]any{"bytes": 1},
		})
	}
	return client, cut
}

func TestRealRedisAWriteWhoseReplyIsLostIsRecognisedByItsToken(t *testing.T) {
	client, cut := singleNodeLostReply(t)
	prefix := fmt.Sprintf("a2t:%d:%d:", os.Getpid(), time.Now().UnixNano())
	t.Cleanup(func() { _, _ = client.Del(context.Background(), prefix+"chat", prefix+"fresh") })
	runLostReplyScenarios(t, client, prefix, nil, cut)
}

// lostEvalHook runs the next armed EVAL for real and then reports a connection
// error instead of its reply.
type lostEvalHook struct{ armed *atomic.Bool }

func (lostEvalHook) DialHook(next goredis.DialHook) goredis.DialHook { return next }
func (h lostEvalHook) ProcessHook(next goredis.ProcessHook) goredis.ProcessHook {
	return func(ctx context.Context, cmd goredis.Cmder) error {
		err := next(ctx, cmd)
		if err == nil && strings.EqualFold(cmd.Name(), "eval") && h.armed.CompareAndSwap(true, false) {
			lost := errors.New("read tcp: connection reset by peer (injected after the script ran)")
			cmd.SetErr(lost)
			return lost
		}
		return err
	}
}
func (lostEvalHook) ProcessPipelineHook(next goredis.ProcessPipelineHook) goredis.ProcessPipelineHook {
	return next
}

func TestRealRedisClusterAWriteWhoseReplyIsLostIsRecognisedByItsToken(t *testing.T) {
	addrs := os.Getenv("ROOST_MIRROR_LOCAL_REDIS_CLUSTER")
	if addrs == "" {
		t.Skip("set ROOST_MIRROR_LOCAL_REDIS_CLUSTER (scripts/mirror-local.sh up)")
	}
	cfg := fredis.DefaultConfig("")
	cfg.ClusterAddrs = strings.Split(addrs, ",")
	client, err := redisdriver.NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	var armed atomic.Bool
	client.(*redisdriver.Client).Raw().AddHook(lostEvalHook{armed: &armed})
	// One hash tag for the values and their index: the indexed script touches
	// both keys, so they must share a slot.
	prefix := fmt.Sprintf("{a2t-%d-%d}:", os.Getpid(), time.Now().UnixNano())
	index := prefix + "due"
	t.Cleanup(func() { _, _ = client.Del(context.Background(), prefix+"chat", prefix+"fresh", index) })
	runLostReplyScenarios(t, client, prefix, &versionstore.RedisIndex[tokenItem]{
		Key: index, Entry: func(v tokenItem) (float64, bool) { return float64(v.Due), true },
	}, func() { armed.Store(true) })
}

func runLostReplyScenarios(t *testing.T, client fredis.IRedis, prefix string, index *versionstore.RedisIndex[tokenItem], loseNextReply func()) {
	t.Helper()
	store, err := versionstore.NewRedisStore(client, versionstore.RedisConfig[string, tokenItem]{
		Prefix: prefix, KeyOf: func(k string) string { return k }, Codec: versionstore.JSONCodec[tokenItem]{}, Index: index,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	t.Run("update", func(t *testing.T) {
		if _, _, err := store.Create(ctx, "chat", tokenItem{Messages: []string{"hello"}}); err != nil {
			t.Fatal(err)
		}
		calls := 0
		update := func() (versionstore.Versioned[tokenItem], bool, error) {
			return store.Update(ctx, "chat", func(current tokenItem, found bool) (tokenItem, bool, error) {
				calls++
				if calls == 1 {
					// Between Update's read and its compare-and-set.
					loseNextReply()
				}
				return appendMessage("msg-1")(current, found)
			})
		}
		got, applied, err := update()
		if err != nil {
			got, applied, err = update() // the caller's plain retry
		}
		stored, _, readErr := store.Get(ctx, "chat")
		if readErr != nil {
			t.Fatal(readErr)
		}
		if err != nil || !applied || got.Version != 2 || stored.Version != 2 || strings.Count(strings.Join(stored.Value.Messages, ","), "msg-1") != 1 {
			t.Fatalf("lost reply: update returned %v / v%d applied=%v err=%v; Redis holds %v / v%d (want msg-1 once at v2)",
				got.Value.Messages, got.Version, applied, err, stored.Value.Messages, stored.Version)
		}
		if index != nil {
			due, err := store.IndexDue(ctx, 0, 10)
			if err != nil || len(due) != 1 || due[0] != "chat" {
				t.Fatalf("index after the recognised write: %v %v", due, err)
			}
		}
	})

	t.Run("create", func(t *testing.T) {
		create := func() (versionstore.Versioned[tokenItem], bool, error) {
			loseNextReply()
			return store.Create(ctx, "fresh", tokenItem{Messages: []string{"first"}})
		}
		got, created, err := create()
		if err != nil {
			got, created, err = store.Create(ctx, "fresh", tokenItem{Messages: []string{"first"}})
		}
		if err != nil || !created || got.Version != 1 {
			t.Fatalf("lost reply: create returned created=%v %v / v%d err=%v; it is this caller's own write", created, got.Value.Messages, got.Version, err)
		}
	})
}

// RR-20261006-35（真实 Redis）：索引分数是 NaN 的带索引写不能“值写进去、索引没动、调用方拿到错误”。
func TestRealRedisAnIndexedWriteWithANaNScoreChangesNothing(t *testing.T) {
	if os.Getenv("ROOST_DATAENGINE_IT") != "1" || os.Getenv("ROOST_DATAENGINE_IT_REDIS_ADDR") == "" {
		t.Skip("set ROOST_DATAENGINE_IT=1 and ROOST_DATAENGINE_IT_REDIS_ADDR")
	}
	client, err := redisdriver.NewClient(fredis.DefaultConfig(os.Getenv("ROOST_DATAENGINE_IT_REDIS_ADDR")))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	prefix := fmt.Sprintf("a2tnan:%d:%d:", os.Getpid(), time.Now().UnixNano())
	t.Cleanup(func() { _, _ = client.Del(context.Background(), prefix+"a", prefix+"due") })
	store, err := versionstore.NewRedisStore(client, versionstore.RedisConfig[string, tokenItem]{
		Prefix: prefix, KeyOf: func(k string) string { return k }, Codec: versionstore.JSONCodec[tokenItem]{},
		Index: &versionstore.RedisIndex[tokenItem]{Key: prefix + "due", Entry: func(v tokenItem) (float64, bool) {
			if len(v.Messages) > 1 {
				return math.NaN(), true
			}
			return 1, true
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, _, err := store.Create(ctx, "a", tokenItem{Messages: []string{"hello"}}); err != nil {
		t.Fatal(err)
	}
	_, applied, err := store.Update(ctx, "a", appendMessage("nan"))
	stored, _, readErr := store.Get(ctx, "a")
	if readErr != nil {
		t.Fatal(readErr)
	}
	if applied || !errors.Is(err, fredis.ErrCASInvalidCommand) || stored.Version != 1 {
		t.Fatalf("a NaN index score: update applied=%v err=%v; Redis holds %v / v%d (want nothing written)", applied, err, stored.Value.Messages, stored.Version)
	}
}
