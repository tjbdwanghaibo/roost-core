//go:build integration

package versionstore_test

// RR-20261005-NC-100：真实 Redis + toxiproxy 上，一次 Update 的 compare-and-set 回复丢失时，
// Update 不能把同一个 mutate 写两次。
//
// 修复前：go-redis 的 MaxRetries（缺省 3）把回复丢失的 EVAL 换连接重发；脚本第二次执行看到的
// 是本次 Update 自己刚写的值，回答“没比上”，Update 当成输给了别人，重读后把 mutate 叠在自己那次
// 写上再写一次，返回成功——["hello" "msg-1" "msg-1"] / v3。
//
// 承诺：回复丢失是结果未知，原样交给调用方（Redis 上恰好一次写入或零次写入），绝不在一次调用里
// 静默写两次。toxiproxy 用本用例自建、端口随机的代理，只给本用例的连接加毒，不动环境共享的
// redis 代理，也不 /reset 别人的代理。
//
// A2 ③ 之后：store 用信封里的一次性令牌核对，这种情况返回 applied / v2、err 为 nil
// （write_token_integration_test.go 断言这一点）；本用例只守“绝不写两次”。

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
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

type ownProxy struct {
	api, name, listen string
}

func (p ownProxy) call(t *testing.T, method, path string, body any) []byte {
	t.Helper()
	var payload bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&payload).Encode(body); err != nil {
			t.Fatal(err)
		}
	}
	req, err := http.NewRequest(method, strings.TrimRight(p.api, "/")+path, &payload)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("toxiproxy %s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	var out bytes.Buffer
	_, _ = out.ReadFrom(resp.Body)
	if resp.StatusCode >= 300 && !(method == http.MethodDelete && resp.StatusCode == http.StatusNotFound) {
		t.Fatalf("toxiproxy %s %s: status %d", method, path, resp.StatusCode)
	}
	return out.Bytes()
}

// newOwnProxy creates a uniquely named proxy on an ephemeral port in front of
// the isolated Redis and removes it at cleanup.
func newOwnProxy(t *testing.T) ownProxy {
	t.Helper()
	if os.Getenv("ROOST_DATAENGINE_IT") != "1" {
		t.Skip("set ROOST_DATAENGINE_IT=1 or use scripts/integration/dataengine-env.sh test")
	}
	api, upstream := os.Getenv("ROOST_DATAENGINE_IT_TOXIPROXY_URL"), os.Getenv("ROOST_DATAENGINE_IT_REDIS_ADDR")
	if api == "" || upstream == "" {
		t.Skip("toxiproxy or isolated Redis not exported by the integration environment")
	}
	proxy := ownProxy{api: api, name: fmt.Sprintf("nc100-%d-%d", os.Getpid(), time.Now().UnixNano())}
	var created struct {
		Listen string `json:"listen"`
	}
	raw := proxy.call(t, http.MethodPost, "/proxies", map[string]any{"name": proxy.name, "listen": "127.0.0.1:0", "upstream": upstream, "enabled": true})
	if err := json.Unmarshal(raw, &created); err != nil || created.Listen == "" {
		t.Fatalf("toxiproxy create: %v %s", err, raw)
	}
	proxy.listen = created.Listen
	t.Cleanup(func() { proxy.call(t, http.MethodDelete, "/proxies/"+proxy.name, nil) })
	return proxy
}

// onDial runs before every new connection, so the toxic that cut the first
// reply is gone before the driver could reconnect.
type onDial func()

func (h onDial) DialHook(next goredis.DialHook) goredis.DialHook {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		h()
		return next(ctx, network, addr)
	}
}
func (onDial) ProcessHook(next goredis.ProcessHook) goredis.ProcessHook { return next }
func (onDial) ProcessPipelineHook(next goredis.ProcessPipelineHook) goredis.ProcessPipelineHook {
	return next
}

func TestRealRedisUpdateWhoseReplyIsLostNeverWritesTwice(t *testing.T) {
	proxy := newOwnProxy(t)
	cfg := fredis.DefaultConfig(proxy.listen) // production default MaxRetries
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

	prefix := fmt.Sprintf("nc100:%d:%d:", os.Getpid(), time.Now().UnixNano())
	store, err := versionstore.NewRedisStore(client, versionstore.RedisConfig[string, []string]{
		Prefix: prefix, KeyOf: func(k string) string { return k }, Codec: versionstore.JSONCodec[[]string]{},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	t.Cleanup(func() { _, _ = client.Del(context.Background(), prefix+"chat") })
	if _, _, err := store.Create(ctx, "chat", []string{"hello"}); err != nil {
		t.Fatal(err)
	}

	calls := 0
	got, applied, err := store.Update(ctx, "chat", func(current []string, _ bool) ([]string, bool, error) {
		calls++
		if calls == 1 {
			// Between Update's read and its compare-and-set: the script reaches
			// Redis and runs; one byte of its reply comes back, then the
			// connection closes.
			armed.Store(true)
			proxy.call(t, http.MethodPost, "/proxies/"+proxy.name+"/toxics", map[string]any{
				"name": "cut", "type": "limit_data", "stream": "downstream", "attributes": map[string]any{"bytes": 1},
			})
		}
		return append(append([]string(nil), current...), "msg-1"), true, nil
	})
	removeCut()
	stored, _, readErr := store.Get(ctx, "chat")
	if readErr != nil {
		t.Fatal(readErr)
	}
	if n := strings.Count(strings.Join(stored.Value, ","), "msg-1"); n > 1 || stored.Version > 2 {
		t.Fatalf("one Update wrote its mutation %d times: stored %v / v%d (update returned %v / v%d applied=%v err=%v, mutate ran %d times)",
			n, stored.Value, stored.Version, got.Value, got.Version, applied, err, calls)
	}
	if err == nil && (!applied || got.Version != stored.Version) {
		t.Fatalf("Update reported success %v / v%d but Redis holds %v / v%d", got.Value, got.Version, stored.Value, stored.Version)
	}
	if err != nil && applied {
		t.Fatalf("Update returned both applied and %v", err)
	}
	t.Logf("lost reply surfaced as %v; Redis holds %v / v%d", err, stored.Value, stored.Version)
}
