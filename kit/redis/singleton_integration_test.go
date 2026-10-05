//go:build integration

package redis

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/app"
)

// SingletonStore 在真实 Redis 上的语义（App 单实例锁方案 §8.1 kit 部分）：Acquire / Renew / Release /
// 丢键 / 他人持有 / Get 多键。键用每次运行随机的前缀，用例结束删除。
//
//	REDIS_ADDR=127.0.0.1:6379 go test -tags integration ./kit/redis/ -run Singleton
//
// Get 跨槽另在真实 Redis Cluster 上跑一次（ROOST_REVIEW_CLUSTER，入口 kit/scripts/integration/redis-cluster-suites.sh）。

func openSingletonStoreForTest(t *testing.T, cfg *viper.Viper) (app.SingletonStore, string) {
	t.Helper()
	store, err := SingletonStore(cfg)
	if err != nil {
		t.Fatalf("open singleton store: %v", err)
	}
	prefix := "roost:test:kitredis-singleton:" + rand.Text()
	t.Cleanup(func() { _ = store.Close() })
	return store, prefix
}

func singletonRedisStore(t *testing.T) (app.SingletonStore, string) {
	t.Helper()
	addr := os.Getenv("REDIS_ADDR")
	if addr == "" {
		t.Skip("REDIS_ADDR is not set")
	}
	cfg := viper.New()
	cfg.Set("redis.addr", addr)
	return openSingletonStoreForTest(t, cfg)
}

func cleanupSingletonKeys(t *testing.T, store app.SingletonStore, keys ...string) {
	t.Helper()
	t.Cleanup(func() {
		ctx := context.Background()
		values, err := store.Get(ctx, keys)
		if err != nil {
			return
		}
		for i, value := range values {
			if value != nil {
				_, _ = store.CompareAndDelete(ctx, keys[i], value)
			}
		}
	})
}

func TestSingletonStoreAcquireRenewRelease(t *testing.T) {
	store, prefix := singletonRedisStore(t)
	ctx := context.Background()
	key := prefix + ":game:1000"
	cleanupSingletonKeys(t, store, key)
	mine := []byte("token-a|host|1|1")

	applied, current, err := store.CompareAndSet(ctx, key, nil, mine, 15*time.Second)
	if err != nil || !applied || !bytes.Equal(current, mine) {
		t.Fatalf("acquire = %v %q %v, want applied", applied, current, err)
	}
	// 回复丢失后的重试：键已是自己的值 → Applied=false、current 等于自己（App 据此认领）。
	applied, current, err = store.CompareAndSet(ctx, key, nil, mine, 15*time.Second)
	if err != nil || applied || !bytes.Equal(current, mine) {
		t.Fatalf("re-acquire = %v %q %v, want not applied with current = own value", applied, current, err)
	}
	applied, _, err = store.CompareAndSet(ctx, key, mine, mine, 15*time.Second)
	if err != nil || !applied {
		t.Fatalf("renew = %v %v, want applied", applied, err)
	}
	applied, err = store.CompareAndDelete(ctx, key, mine)
	if err != nil || !applied {
		t.Fatalf("release = %v %v, want applied", applied, err)
	}
	values, err := store.Get(ctx, []string{key})
	if err != nil || values[0] != nil {
		t.Fatalf("after release Get = %q %v, want absent", values, err)
	}
}

func TestSingletonStoreRenewAfterTheKeyExpiredIsNotHeld(t *testing.T) {
	store, prefix := singletonRedisStore(t)
	ctx := context.Background()
	key := prefix + ":game:1000"
	cleanupSingletonKeys(t, store, key)
	mine := []byte("token-a|host|1|1")
	if applied, _, err := store.CompareAndSet(ctx, key, nil, mine, 50*time.Millisecond); err != nil || !applied {
		t.Fatalf("acquire = %v %v", applied, err)
	}
	time.Sleep(150 * time.Millisecond) // 真实 Redis 的 TTL，只能等它过期
	applied, current, err := store.CompareAndSet(ctx, key, mine, mine, 15*time.Second)
	if err != nil || applied || current != nil {
		t.Fatalf("renew after expiry = %v %q %v, want NotHeld with the key gone", applied, current, err)
	}
	if applied, err := store.CompareAndDelete(ctx, key, mine); err != nil || applied {
		t.Fatalf("release after expiry = %v %v, want not applied", applied, err)
	}
}

func TestSingletonStoreDoesNotTouchAnotherHoldersKey(t *testing.T) {
	store, prefix := singletonRedisStore(t)
	ctx := context.Background()
	key := prefix + ":game:1000"
	cleanupSingletonKeys(t, store, key)
	other := []byte("token-b|other|2|1")
	mine := []byte("token-a|host|1|1")
	if applied, _, err := store.CompareAndSet(ctx, key, nil, other, 15*time.Second); err != nil || !applied {
		t.Fatalf("other acquire = %v %v", applied, err)
	}
	applied, current, err := store.CompareAndSet(ctx, key, nil, mine, 15*time.Second)
	if err != nil || applied || !bytes.Equal(current, other) {
		t.Fatalf("acquire held key = %v %q %v, want not applied with the holder's value", applied, current, err)
	}
	applied, current, err = store.CompareAndSet(ctx, key, mine, mine, 15*time.Second)
	if err != nil || applied || !bytes.Equal(current, other) {
		t.Fatalf("renew someone else's key = %v %q %v, want NotHeld", applied, current, err)
	}
	if applied, err := store.CompareAndDelete(ctx, key, mine); err != nil || applied {
		t.Fatalf("release someone else's key = %v %v, want not applied", applied, err)
	}
	values, err := store.Get(ctx, []string{key})
	if err != nil || !bytes.Equal(values[0], other) {
		t.Fatalf("holder's key = %q %v, want untouched", values, err)
	}
}

func TestSingletonStoreGetReadsEveryKeyInOrder(t *testing.T) {
	store, prefix := singletonRedisStore(t)
	assertSingletonGetAcrossKeys(t, store, prefix)
}

// Cluster 下各 sid 的键落在不同槽：Get 不能用单条 MGET（CROSSSLOT）。
func TestSingletonStoreGetAcrossClusterSlots(t *testing.T) {
	addrs := os.Getenv("ROOST_REVIEW_CLUSTER")
	if addrs == "" {
		t.Skip("ROOST_REVIEW_CLUSTER is not set")
	}
	cfg := viper.New()
	cfg.Set("redis.cluster_addrs", addrs)
	store, prefix := openSingletonStoreForTest(t, cfg)
	assertSingletonGetAcrossKeys(t, store, prefix)
}

func assertSingletonGetAcrossKeys(t *testing.T, store app.SingletonStore, prefix string) {
	t.Helper()
	ctx := context.Background()
	keys := make([]string, 8)
	for i := range keys {
		keys[i] = fmt.Sprintf("%s:game:%d", prefix, 1000+i)
	}
	cleanupSingletonKeys(t, store, keys...)
	for _, i := range []int{1, 4, 6} {
		if applied, _, err := store.CompareAndSet(ctx, keys[i], nil, []byte(fmt.Sprintf("v%d", i)), 15*time.Second); err != nil || !applied {
			t.Fatalf("set %s = %v %v", keys[i], applied, err)
		}
	}
	values, err := store.Get(ctx, keys)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(values) != len(keys) {
		t.Fatalf("Get returned %d values for %d keys", len(values), len(keys))
	}
	for i, value := range values {
		want := ""
		if i == 1 || i == 4 || i == 6 {
			want = fmt.Sprintf("v%d", i)
		}
		if string(value) != want || (want == "" && value != nil) {
			t.Fatalf("Get[%d] = %q, want %q", i, value, want)
		}
	}
}

// Live 与锁操作不共用连接（收尾审查 1）。Redis 变慢时并发的 Live 查询可能占满连接，续期等不到连接
// 只能超时（Unknown），一直如此会被误判 Lost、进程 fail-stop。用一个只扣住 GET 的代理模拟“Live 的
// 命令迟迟没有回复”：Live 的连接全部卡住之后，续期仍要在自己的超时内拿到连接并 Applied。
func TestSingletonStoreRenewalDoesNotWaitBehindStalledLiveQueries(t *testing.T) {
	direct, prefix := singletonRedisStore(t)
	key := prefix + ":game:1000"
	cleanupSingletonKeys(t, direct, key)
	proxy := newGetHoldingProxy(t, os.Getenv("REDIS_ADDR"))
	cfg := viper.New()
	cfg.Set("redis.addr", proxy.addr)
	store, err := SingletonStore(cfg)
	if err != nil {
		t.Fatalf("open singleton store through the proxy: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	t.Cleanup(proxy.releaseGets) // 先于 Close 放行，卡住的 Live 才能返回

	mine := []byte("token-a|host|1|1")
	if applied, _, err := store.CompareAndSet(context.Background(), key, nil, mine, 15*time.Second); err != nil || !applied {
		t.Fatalf("acquire = %v %v", applied, err)
	}
	const stalledLive = 8 // 多于 Live 的连接数：排队等连接的 Live 也不能挤占续期
	liveDone := make(chan struct{}, stalledLive)
	for i := range stalledLive {
		go func() {
			defer func() { liveDone <- struct{}{} }()
			_, _ = store.Get(context.Background(), []string{fmt.Sprintf("%s:game:%d", prefix, 2000+i)})
		}()
	}
	for range singletonLivePoolSize { // Live 的连接全部卡住
		select {
		case <-proxy.held:
		case <-time.After(5 * time.Second):
			t.Fatal("Live queries never reached Redis")
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	applied, current, err := store.CompareAndSet(ctx, key, mine, mine, 15*time.Second)
	if err != nil || !applied {
		t.Fatalf("renew while every Live connection is stalled = %v %q %v, want applied", applied, current, err)
	}

	proxy.releaseGets()
	for range stalledLive {
		select {
		case <-liveDone:
		case <-time.After(10 * time.Second):
			t.Fatal("stalled Live queries did not return after the proxy let them through")
		}
	}
}

// getHoldingProxy 是一个 TCP 代理：转发所有命令，但把 GET 扣住直到 releaseGets；每扣住一条 GET
// 往 held 发一个信号。只解析客户端发往服务端方向的 RESP 数组（go-redis 只发这种格式）。
type getHoldingProxy struct {
	addr        string
	held        chan struct{}
	release     chan struct{}
	releaseOnce sync.Once

	mu    sync.Mutex
	conns []net.Conn
}

func newGetHoldingProxy(t *testing.T, upstream string) *getHoldingProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("proxy listen: %v", err)
	}
	p := &getHoldingProxy{addr: ln.Addr().String(), held: make(chan struct{}, 64), release: make(chan struct{})}
	t.Cleanup(func() {
		p.releaseGets()
		_ = ln.Close()
		p.mu.Lock()
		defer p.mu.Unlock()
		for _, conn := range p.conns {
			_ = conn.Close()
		}
	})
	go func() {
		for {
			client, err := ln.Accept()
			if err != nil {
				return
			}
			server, err := net.Dial("tcp", upstream)
			if err != nil {
				_ = client.Close()
				continue
			}
			p.mu.Lock()
			p.conns = append(p.conns, client, server)
			p.mu.Unlock()
			go func() { _, _ = io.Copy(client, server) }()
			go p.forward(client, server)
		}
	}()
	return p
}

func (p *getHoldingProxy) releaseGets() { p.releaseOnce.Do(func() { close(p.release) }) }

func (p *getHoldingProxy) forward(client, server net.Conn) {
	defer server.Close()
	reader := bufio.NewReader(client)
	for {
		raw, name, err := readRESPCommand(reader)
		if err != nil {
			return
		}
		if strings.EqualFold(name, "GET") {
			select {
			case p.held <- struct{}{}:
			default:
			}
			<-p.release
		}
		if _, err := server.Write(raw); err != nil {
			return
		}
	}
}

// readRESPCommand 读一条 RESP 数组命令，返回原始字节与命令名。
func readRESPCommand(r *bufio.Reader) ([]byte, string, error) {
	header, err := r.ReadBytes('\n')
	if err != nil {
		return nil, "", err
	}
	if len(header) < 4 || header[0] != '*' {
		return nil, "", errors.New("proxy: expected a RESP array")
	}
	count, err := strconv.Atoi(string(header[1 : len(header)-2]))
	if err != nil {
		return nil, "", err
	}
	raw := append([]byte(nil), header...)
	var name string
	for i := range count {
		line, err := r.ReadBytes('\n')
		if err != nil {
			return nil, "", err
		}
		if len(line) < 4 || line[0] != '$' {
			return nil, "", errors.New("proxy: expected a RESP bulk string")
		}
		size, err := strconv.Atoi(string(line[1 : len(line)-2]))
		if err != nil {
			return nil, "", err
		}
		data := make([]byte, size+2)
		if _, err := io.ReadFull(r, data); err != nil {
			return nil, "", err
		}
		if i == 0 {
			name = string(data[:size])
		}
		raw = append(raw, line...)
		raw = append(raw, data...)
	}
	return raw, name, nil
}
