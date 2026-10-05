package driver

// RR-20261005-NC-100：一次 Eval 至多让服务端执行一次脚本。
//
// go-redis 的 MaxRetries（fredis.DefaultConfig 缺省 3）在连接 EOF / 读超时时会把同一条命令
// 换连接再发一遍。对 GET 这类读无害；对 Lua 脚本，“发出去了、回复丢了”时脚本可能已经执行，
// 再发一遍就是第二次执行。CompareAndSet 第二次执行时看到的是自己刚写的值，回答“没比上”，
// versionstore.Update 于是按“输给了别人”重读、把 mutate 叠在自己那次写上再写一次——一次
// Update 写了两次，返回成功（真实 Redis + toxiproxy 复现见 versionstore 的 integration 用例）。
// 结果未知只能由调用方按自己的语义裁决（集群恢复 hook 的注释同样写着“不重放结果不确定的写命令”），
// 所以脚本一律不交给驱动自动重放：回复丢了就把错误原样交回。
//
// 这里用一个只会说 RESP 的本地替身服务端：第一次收到 EVAL 时“执行”（计数）后直接断开连接、
// 不回复，之后的 EVAL 正常回复。驱动按生产构造（NewRedisClient，MaxRetries 缺省 3）。

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"
	fredis "github.com/tjbdwanghaibo/roost-core/redis"
)

// dropFirstScriptReply 是最小 RESP2 服务端：脚本命令计数，第一次执行后断开不回复。
type dropFirstScriptReply struct {
	listener net.Listener
	mu       sync.Mutex
	scripts  map[string]int // 命令名 → 服务端“执行”次数
	dropped  bool
}

func newDropFirstScriptReply(t *testing.T) *dropFirstScriptReply {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &dropFirstScriptReply{listener: listener, scripts: make(map[string]int)}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go server.serve(conn)
		}
	}()
	return server
}

func (s *dropFirstScriptReply) executions(name string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.scripts[name]
}

func (s *dropFirstScriptReply) serve(conn net.Conn) {
	defer conn.Close()
	reader := bufio.NewReader(conn)
	for {
		args, err := readRESPArray(reader)
		if err != nil {
			return
		}
		name := strings.ToLower(args[0])
		switch name {
		case "eval", "evalsha":
			s.mu.Lock()
			s.scripts[name]++
			drop := !s.dropped
			s.dropped = true
			s.mu.Unlock()
			if drop {
				return // 已执行，回复丢在网络上
			}
			_, _ = io.WriteString(conn, ":1\r\n")
		case "ping":
			_, _ = io.WriteString(conn, "+PONG\r\n")
		default:
			// HELLO / CLIENT SETINFO 等握手命令：按旧服务端回错误，驱动回落到 RESP2。
			_, _ = io.WriteString(conn, "-ERR unknown command '"+args[0]+"'\r\n")
		}
	}
}

func readRESPArray(reader *bufio.Reader) ([]string, error) {
	line, err := reader.ReadString('\n')
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(line, "*") {
		return nil, fmt.Errorf("not an array: %q", line)
	}
	count, err := strconv.Atoi(strings.TrimSpace(line[1:]))
	if err != nil {
		return nil, err
	}
	args := make([]string, 0, count)
	for range count {
		header, err := reader.ReadString('\n')
		if err != nil {
			return nil, err
		}
		size, err := strconv.Atoi(strings.TrimSpace(header[1:]))
		if err != nil {
			return nil, err
		}
		body := make([]byte, size+2)
		if _, err := io.ReadFull(reader, body); err != nil {
			return nil, err
		}
		args = append(args, string(body[:size]))
	}
	return args, nil
}

func TestAScriptWhoseReplyIsLostIsNotReplayedByTheDriver(t *testing.T) {
	for _, call := range []struct {
		name string
		run  func(ctx context.Context, c *Client) (any, error)
	}{
		{"eval", func(ctx context.Context, c *Client) (any, error) {
			return c.Eval(ctx, "return 1", []string{"k"}, "a")
		}},
		{"evalsha", func(ctx context.Context, c *Client) (any, error) {
			return c.EvalSha(ctx, "e0e1f9fabfc9d4800c877a703b823ac0578ff8db", []string{"k"}, "a")
		}},
	} {
		t.Run(call.name, func(t *testing.T) {
			server := newDropFirstScriptReply(t)
			cfg := fredis.DefaultConfig(server.listener.Addr().String())
			cfg.MinIdleConns = 0
			cfg.DialTimeout, cfg.ReadTimeout, cfg.WriteTimeout = time.Second, time.Second, time.Second
			if cfg.MaxRetries <= 0 {
				t.Fatalf("fixture needs the production default MaxRetries > 0, got %d", cfg.MaxRetries)
			}
			client := NewRedisClient(cfg)
			defer client.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			reply, err := call.run(ctx, client)
			if got := server.executions(call.name); got != 1 {
				t.Fatalf("one %s call executed the script %d times on the server (reply=%v err=%v); want exactly once", call.name, got, reply, err)
			}
			if err == nil {
				t.Fatalf("%s whose reply was lost returned %v with no error; the outcome is unknown and must surface", call.name, reply)
			}
			if errors.Is(err, goredis.Nil) {
				t.Fatalf("lost reply reported as a miss: %v", err)
			}
		})
	}
}

// 只有脚本命令带不可重放标记（克隆后仍带），普通命令保留驱动的自动重试。
func TestOnlyScriptCommandsOptOutOfTheDriverRetry(t *testing.T) {
	server := newDropFirstScriptReply(t)
	cfg := fredis.DefaultConfig(server.listener.Addr().String())
	cfg.MinIdleConns = 0
	client := NewRedisClient(cfg)
	defer client.Close()
	if err := client.Ping(context.Background()); err != nil {
		t.Fatalf("ping: %v", err)
	}
	cmd := newScriptCmd(context.Background(), "eval", "return 1", []string{"k"})
	if !cmd.NoRetry() || !cmd.Clone().NoRetry() {
		t.Fatal("script command (or its clone) is retryable")
	}
	if goredis.NewCmd(context.Background(), "get", "k").NoRetry() {
		t.Fatal("ordinary commands lost their retry")
	}
}
