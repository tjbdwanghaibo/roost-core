//go:build integration

package driver

// A2：真实 Redis + 本用例自建、端口随机的 toxiproxy 代理上，写命令的回复丢失时，一次调用只让
// Redis 执行一次，结果未知原样交回调用方。
//
// 修前（go-redis MaxRetries 缺省 3）：回复只回来 1 个字节就断开，驱动换连接把同一条写再发一遍——
// INCR 之后计数器是 2、RPUSH 之后列表里两份、pipeline（INCR + RPUSH）整条重放、SETNX 第二次看到
// 自己的键回答“没设上”，自己的值留在 Redis 里直到 TTL。
//
// 代理只给本用例的连接加毒，新连接拨号前摘掉毒（重放若发生会落在干净连接上，才能观测到第二次执行）；
// 不动环境共享的 redis 代理，也不 /reset。键带唯一前缀，结束时删除。

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
)

// errMisreported marks a SETNX that answered "someone else holds it" for its own write.
var errMisreported = errors.New("reported as taken by someone else")

// toxiproxyClient 驱动本用例在环境 toxiproxy 上自建的代理。RR-20261005-NC-208：不用环境共享的
// "redis" 代理、不 POST /reset（它会摘掉同一 toxiproxy 上所有代理的毒，别的会话的故障也在内）；
// 每个用例建一个唯一命名、端口随机的代理，只给它加毒，结束时删掉它。
type toxiproxyClient struct{ base, name string }

func (c toxiproxyClient) call(t *testing.T, method, path string, body any) []byte {
	t.Helper()
	var payload bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&payload).Encode(body); err != nil {
			t.Fatal(err)
		}
	}
	req, err := http.NewRequest(method, strings.TrimRight(c.base, "/")+path, &payload)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("toxiproxy %s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	var out bytes.Buffer
	_, _ = out.ReadFrom(resp.Body)
	if resp.StatusCode >= 300 && !(method == http.MethodDelete && resp.StatusCode == http.StatusNotFound) {
		t.Fatalf("toxiproxy %s %s: status %d %s", method, path, resp.StatusCode, out.String())
	}
	return out.Bytes()
}

// newA2Proxy 在环境的 toxiproxy 上建一个本用例独占、端口随机的代理，返回代理、监听地址与上游地址。
func newA2Proxy(t *testing.T) (toxiproxyClient, string, string) {
	t.Helper()
	if os.Getenv("ROOST_DATAENGINE_IT") != "1" {
		t.Skip("set ROOST_DATAENGINE_IT=1 or use wiring/scripts/integration/dataengine-env.sh test")
	}
	api, upstream := os.Getenv("ROOST_DATAENGINE_IT_TOXIPROXY_URL"), os.Getenv("ROOST_DATAENGINE_IT_REDIS_ADDR")
	if api == "" || upstream == "" {
		t.Skip("toxiproxy or isolated Redis not exported by the integration environment")
	}
	proxy := toxiproxyClient{base: api, name: fmt.Sprintf("a2-%d-%d", os.Getpid(), time.Now().UnixNano())}
	var created struct {
		Listen string `json:"listen"`
	}
	raw := proxy.call(t, http.MethodPost, "/proxies", map[string]any{"name": proxy.name, "listen": "127.0.0.1:0", "upstream": upstream, "enabled": true})
	if err := json.Unmarshal(raw, &created); err != nil || created.Listen == "" {
		t.Fatalf("toxiproxy create: %v %s", err, raw)
	}
	t.Cleanup(func() { proxy.call(t, http.MethodDelete, "/proxies/"+proxy.name, nil) })
	return proxy, created.Listen, upstream
}

// beforeDial runs before every new connection.
type beforeDial func()

func (h beforeDial) DialHook(next goredis.DialHook) goredis.DialHook {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		h()
		return next(ctx, network, addr)
	}
}
func (beforeDial) ProcessHook(next goredis.ProcessHook) goredis.ProcessHook { return next }
func (beforeDial) ProcessPipelineHook(next goredis.ProcessPipelineHook) goredis.ProcessPipelineHook {
	return next
}

func TestRealRedisAWriteWhoseReplyIsLostRunsOnce(t *testing.T) {
	type outcome struct {
		executions int64
		detail     string
	}
	for _, tc := range []struct {
		name string
		// run issues the write; inspect reads what Redis holds through a clean client.
		run     func(ctx context.Context, c *Client, key string) error
		inspect func(ctx context.Context, direct *goredis.Client, key string) outcome
	}{
		{"incr", func(ctx context.Context, c *Client, key string) error {
			_, err := c.Incr(ctx, key)
			return err
		}, func(ctx context.Context, d *goredis.Client, key string) outcome {
			n, _ := d.Get(ctx, key).Int64()
			return outcome{n, fmt.Sprintf("counter=%d", n)}
		}},
		{"rpush", func(ctx context.Context, c *Client, key string) error {
			_, err := c.RPush(ctx, key, "msg-1")
			return err
		}, func(ctx context.Context, d *goredis.Client, key string) outcome {
			items, _ := d.LRange(ctx, key, 0, -1).Result()
			return outcome{int64(len(items)), fmt.Sprintf("list=%v", items)}
		}},
		{"pipeline", func(ctx context.Context, c *Client, key string) error {
			pipe := c.Pipeline()
			pipe.Incr(ctx, key)
			pipe.RPush(ctx, key+":list", "msg-1")
			return pipe.Exec(ctx)
		}, func(ctx context.Context, d *goredis.Client, key string) outcome {
			n, _ := d.Get(ctx, key).Int64()
			items, _ := d.LRange(ctx, key+":list", 0, -1).Result()
			return outcome{n, fmt.Sprintf("counter=%d list=%v", n, items)}
		}},
		{"setnx", func(ctx context.Context, c *Client, key string) error {
			ok, err := c.SetNX(ctx, key, "mine", time.Minute)
			if err == nil && !ok {
				return errMisreported
			}
			return err
		}, func(ctx context.Context, d *goredis.Client, key string) outcome {
			v, _ := d.Get(ctx, key).Result()
			return outcome{1, "value=" + v}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			proxy, listen, upstream := newA2Proxy(t)
			cfg := fredis.DefaultConfig(listen) // production default MaxRetries
			cfg.MinIdleConns = 0
			cfg.PoolSize = 1
			client := NewRedisClient(cfg)
			t.Cleanup(func() { _ = client.Close() })
			direct := goredis.NewClient(&goredis.Options{Addr: upstream})
			t.Cleanup(func() { _ = direct.Close() })

			key := fmt.Sprintf("a2:%d:%d:%s", os.Getpid(), time.Now().UnixNano(), tc.name)
			t.Cleanup(func() { direct.Del(context.Background(), key, key+":list") })
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := client.Ping(ctx); err != nil { // establish the one pooled connection
				t.Fatal(err)
			}
			var armed atomic.Bool
			removeCut := func() { proxy.call(t, http.MethodDelete, "/proxies/"+proxy.name+"/toxics/cut", nil) }
			client.rdb.AddHook(beforeDial(func() {
				if armed.CompareAndSwap(true, false) {
					removeCut()
				}
			}))
			// The write reaches Redis and runs; one byte of its reply comes back,
			// then the connection closes.
			armed.Store(true)
			proxy.call(t, http.MethodPost, "/proxies/"+proxy.name+"/toxics", map[string]any{
				"name": "cut", "type": "limit_data", "stream": "downstream", "attributes": map[string]any{"bytes": 1},
			})
			err := tc.run(ctx, client, key)
			removeCut()
			got := tc.inspect(context.Background(), direct, key)
			t.Logf("%s: err=%v; Redis holds %s", tc.name, err, got.detail)
			if errors.Is(err, errMisreported) {
				t.Fatalf("%s: the replayed SET NX saw this call's own key and reported it as taken (%s)", tc.name, got.detail)
			}
			if got.executions != 1 {
				t.Fatalf("one %s call was applied %d times (%s, err=%v); want exactly once", tc.name, got.executions, got.detail, err)
			}
			if err == nil {
				t.Fatalf("%s whose reply was lost returned success (%s); the outcome is unknown and must surface", tc.name, got.detail)
			}
			if IsDefinitelyNotExecuted(err) {
				t.Fatalf("lost reply classified as definitely not executed: %v", err)
			}
		})
	}
}
