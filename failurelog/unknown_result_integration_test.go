//go:build integration

package failurelog

// RR-20261005-NC-160 的真实 Redis 版本：生产驱动（redis/driver，MaxRetries 缺省 3）经一个本地
// TCP 代理连到真实 Redis；代理把第一条 EVAL 原样转给 Redis，Redis 执行完、回复回来时代理直接
// 断开客户端连接，不转发回复——“脚本执行了、回复丢了”。修前 AppendRaw 把这当成脚本没执行，
// 再走 RPUSH 降级，同一条死信在真实列表里出现两次。
//
// 环境：ROOST_DATAENGINE_IT_REDIS_ADDR（隔离环境 env.sh 导出）或 ROOST_REDIS_TEST_ADDR；键前缀
// roostrevn12:failurelog:，用例结束删除。

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	fredis "github.com/tjbdwanghaibo/roost-core/redis"
	"github.com/tjbdwanghaibo/roost-core/redis/driver"
)

// dropFirstEvalReply 转发所有字节，只把第一条 EVAL 的回复吞掉并断开那条客户端连接。
type dropFirstEvalReply struct {
	listener net.Listener
	target   string
	armed    atomic.Bool
	dropped  atomic.Int32
}

func newDropFirstEvalReply(t *testing.T, target string) *dropFirstEvalReply {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	proxy := &dropFirstEvalReply{listener: listener, target: target}
	proxy.armed.Store(true)
	t.Cleanup(func() { _ = listener.Close() })
	go proxy.accept()
	return proxy
}

func (p *dropFirstEvalReply) accept() {
	for {
		client, err := p.listener.Accept()
		if err != nil {
			return
		}
		server, err := net.Dial("tcp", p.target)
		if err != nil {
			_ = client.Close()
			continue
		}
		go p.pipe(client, server)
	}
}

func (p *dropFirstEvalReply) pipe(client, server net.Conn) {
	var swallow atomic.Bool
	var once sync.Once
	closeBoth := func() { once.Do(func() { _ = client.Close(); _ = server.Close() }) }
	go func() {
		defer closeBoth()
		buf := make([]byte, 64<<10)
		for {
			n, err := client.Read(buf)
			if n > 0 {
				if bytes.Contains(bytes.ToLower(buf[:n]), []byte("eval")) && p.armed.CompareAndSwap(true, false) {
					swallow.Store(true)
				}
				if _, werr := server.Write(buf[:n]); werr != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()
	defer closeBoth()
	buf := make([]byte, 64<<10)
	for {
		n, err := server.Read(buf)
		if n > 0 {
			if swallow.Load() {
				p.dropped.Add(1)
				return // 回复已由 Redis 产生，客户端永远收不到
			}
			if _, werr := client.Write(buf[:n]); werr != nil {
				return
			}
		}
		if err != nil {
			if err != io.EOF {
				return
			}
			return
		}
	}
}

func integrationRedisAddr(t *testing.T) string {
	t.Helper()
	for _, name := range []string{"ROOST_DATAENGINE_IT_REDIS_ADDR", "ROOST_REDIS_TEST_ADDR"} {
		if addr := os.Getenv(name); addr != "" {
			return addr
		}
	}
	t.Skip("set ROOST_DATAENGINE_IT_REDIS_ADDR or ROOST_REDIS_TEST_ADDR")
	return ""
}

func TestIntegrationLostScriptReplyDoesNotAppendTwice(t *testing.T) {
	addr := integrationRedisAddr(t)
	direct, err := driver.NewClient(fredis.DefaultConfig(addr))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = direct.Close() })

	proxy := newDropFirstEvalReply(t, addr)
	cfg := fredis.DefaultConfig(proxy.listener.Addr().String())
	cfg.ReadTimeout = 2 * time.Second
	viaProxy, err := driver.NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = viaProxy.Close() })

	key := fmt.Sprintf("roostrevn12:failurelog:{%d}", time.Now().UnixNano())
	t.Cleanup(func() { _, _ = direct.Del(context.Background(), key) })

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	list := NewRedisList(viaProxy, Config{Namespace: "bus_dlq", MaxEntries: 100, TTL: time.Minute})
	appendErr := list.AppendRaw(ctx, key, []byte(`{"msg_id":"m-1","reason":"handler failed"}`))
	if proxy.dropped.Load() != 1 {
		t.Fatalf("proxy dropped %d EVAL replies, want 1 (fixture did not engage)", proxy.dropped.Load())
	}
	items, err := direct.LRange(ctx, key, 0, -1)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("AppendRaw err=%v; real list now holds %d entries", appendErr, len(items))
	if len(items) != 1 {
		t.Fatalf("one AppendRaw stored %d copies in real Redis %v (err=%v); want 1", len(items), items, appendErr)
	}
	if appendErr == nil {
		t.Fatal("AppendRaw reported success although its result is unknown")
	}
}
