// 驱动不重放结果未知的写：写 / 脚本 / pipeline 回复丢失只执行一次，确定未执行的错误仍重发，复制等待的脚本连接与错误。

package driver

import (
	"bufio"
	"context"
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

	goredis "github.com/redis/go-redis/v9"
	fredis "github.com/tjbdwanghaibo/roost-core/infra/storage/redis"
)

// 驱动的默认重试与框架“结果未知交给调用方”的约定一致：
//   - 写命令（以及含写的 pipeline、分布式锁的 SETNX / 脚本）回复丢失时，一次调用至多让服务端执行一次，错误原样交回调用方；
//     真实 Redis + toxiproxy 的复现见 write_lost_reply_integration_test.go。
//   - 错误证明命令没有执行（LOADING、MASTERDOWN、TRYAGAIN、CLUSTERDOWN、max clients、READONLY、NOREPLICAS，以及取连接阶段的失败）
//     时仍然重发，包括脚本；读命令照常交给驱动重试。
// 用只会说 RESP 的本地替身服务端控制每条命令第几次怎样回答；驱动按生产构造（NewRedisClient）。

// standInAction 是替身对某条命令某一次调用的处理：drop=执行后断开不回复；reject=不执行、回这条错误。
type standInAction struct {
	drop   bool
	reject string
}

// respStandIn 是最小 RESP2 服务端：按命令名计数“执行”次数，先按计划处理，计划用完后正常回复。
type respStandIn struct {
	listener net.Listener
	mu       sync.Mutex
	plans    map[string][]standInAction
	executed map[string]int
}

func newRESPStandIn(t *testing.T, plans map[string][]standInAction) *respStandIn {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &respStandIn{listener: listener, plans: plans, executed: map[string]int{}}
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

func (s *respStandIn) executions(name string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.executed[name]
}

// next 取出这次调用的处理方式，并在执行时计数。
func (s *respStandIn) next(name string) (standInAction, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var action standInAction
	if plan := s.plans[name]; len(plan) > 0 {
		action, s.plans[name] = plan[0], plan[1:]
	}
	if action.reject == "" {
		s.executed[name]++
	}
	return action, s.executed[name]
}

func (s *respStandIn) serve(conn net.Conn) {
	defer conn.Close()
	reader := bufio.NewReader(conn)
	for {
		args, err := readRESPArray(reader)
		if err != nil {
			return
		}
		name := strings.ToLower(args[0])
		var reply string
		switch name {
		case "ping":
			reply = "+PONG\r\n"
		case "hello", "client", "auth", "select":
			// 按旧服务端回错误，驱动回落到 RESP2。
			reply = "-ERR unknown command '" + args[0] + "'\r\n"
		default:
			action, count := s.next(name)
			switch {
			case action.reject != "":
				reply = "-" + action.reject + "\r\n"
			case action.drop:
				return // 已执行，回复丢在网络上
			default:
				reply = standInReply(name, count)
			}
		}
		if _, err := io.WriteString(conn, reply); err != nil {
			return
		}
	}
}

func standInReply(name string, count int) string {
	switch name {
	case "set":
		return "+OK\r\n"
	case "ltrim":
		return "+OK\r\n"
	case "get", "lpop", "rpop":
		return "$1\r\nv\r\n"
	case "waitaof":
		return "*2\r\n:1\r\n:0\r\n"
	default:
		return fmt.Sprintf(":%d\r\n", count)
	}
}

func standInClient(t *testing.T, server *respStandIn) *Client {
	t.Helper()
	cfg := fredis.DefaultConfig(server.listener.Addr().String())
	cfg.MinIdleConns = 0
	cfg.DialTimeout, cfg.ReadTimeout, cfg.WriteTimeout = time.Second, time.Second, time.Second
	if cfg.MaxRetries <= 0 {
		t.Fatalf("fixture needs the production default MaxRetries > 0, got %d", cfg.MaxRetries)
	}
	client := NewRedisClient(cfg)
	t.Cleanup(func() { _ = client.Close() })
	return client
}

type writeCall struct {
	name string // 服务端看到的命令名
	run  func(ctx context.Context, c *Client) error
}

func errOnly[T any](_ T, err error) error { return err }

var writeCalls = []writeCall{
	{"incr", func(ctx context.Context, c *Client) error { return errOnly(c.Incr(ctx, "k")) }},
	{"incrby", func(ctx context.Context, c *Client) error { return errOnly(c.IncrBy(ctx, "k", 5)) }},
	{"rpush", func(ctx context.Context, c *Client) error { return errOnly(c.RPush(ctx, "k", "a")) }},
	{"lpush", func(ctx context.Context, c *Client) error { return errOnly(c.LPush(ctx, "k", "a")) }},
	{"lpop", func(ctx context.Context, c *Client) error { return errOnly(c.LPop(ctx, "k")) }},
	{"rpop", func(ctx context.Context, c *Client) error { return errOnly(c.RPop(ctx, "k")) }},
	{"set", func(ctx context.Context, c *Client) error { return errOnly(c.SetNX(ctx, "k", "v", time.Second)) }},
	{"del", func(ctx context.Context, c *Client) error { return errOnly(c.Del(ctx, "k")) }},
	{"hset", func(ctx context.Context, c *Client) error { return c.HSet(ctx, "k", "f", "v") }},
	{"hdel", func(ctx context.Context, c *Client) error { return errOnly(c.HDel(ctx, "k", "f")) }},
	{"expire", func(ctx context.Context, c *Client) error { return errOnly(c.Expire(ctx, "k", time.Second)) }},
	{"zadd", func(ctx context.Context, c *Client) error {
		return errOnly(c.ZAdd(ctx, "k", fredis.Z{Score: 1, Member: "m"}))
	}},
	{"zrem", func(ctx context.Context, c *Client) error { return errOnly(c.ZRem(ctx, "k", "m")) }},
	{"sadd", func(ctx context.Context, c *Client) error { return errOnly(c.SAdd(ctx, "k", "m")) }},
	{"srem", func(ctx context.Context, c *Client) error { return errOnly(c.SRem(ctx, "k", "m")) }},
	{"ltrim", func(ctx context.Context, c *Client) error { return c.LTrim(ctx, "k", 0, 9) }},
	{"lrem", func(ctx context.Context, c *Client) error { return errOnly(c.LRem(ctx, "k", 1, "a")) }},
	{"publish", func(ctx context.Context, c *Client) error { return c.Publish(ctx, "ch", "m") }},
	{"eval", func(ctx context.Context, c *Client) error { return errOnly(c.Eval(ctx, "return 1", []string{"k"})) }},
	{"pipeline", func(ctx context.Context, c *Client) error {
		pipe := c.Pipeline()
		pipe.Incr(ctx, "counter")
		pipe.RPush(ctx, "k", "a")
		return pipe.Exec(ctx)
	}},
	{"distlock", func(ctx context.Context, c *Client) error {
		ok, err := NewDistLockFactory(c.rdb).NewLock("lock", time.Second).Acquire(ctx)
		if err == nil && !ok {
			return errors.New("Acquire reported the lock as taken by someone else")
		}
		return err
	}},
}

// serverName 是 writeCall 在服务端的命令名（pipeline 以 RPUSH 为丢回复点、INCR 为计数点）。
func (w writeCall) serverName() (dropAt, countAt string) {
	switch w.name {
	case "pipeline":
		return "rpush", "incr"
	case "distlock":
		return "set", "set"
	}
	return w.name, w.name
}

func TestAWriteWhoseReplyIsLostIsNotReplayedByTheDriver(t *testing.T) {
	for _, call := range writeCalls {
		t.Run(call.name, func(t *testing.T) {
			dropAt, countAt := call.serverName()
			server := newRESPStandIn(t, map[string][]standInAction{dropAt: {{drop: true}}})
			client := standInClient(t, server)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			err := call.run(ctx, client)
			if got := server.executions(countAt); got != 1 {
				t.Fatalf("one %s call executed %s %d times on the server (err=%v); want exactly once", call.name, countAt, got, err)
			}
			if err == nil {
				t.Fatalf("%s whose reply was lost returned no error; the outcome is unknown and must surface", call.name)
			}
			if IsDefinitelyNotExecuted(err) {
				t.Fatalf("%s lost reply classified as definitely not executed: %v", call.name, err)
			}
		})
	}
}

func TestNotExecutedErrorsAreStillResent(t *testing.T) {
	rejections := []string{
		"LOADING Redis is loading the dataset in memory",
		"MASTERDOWN Link with MASTER is down and replica-serve-stale-data is set to 'no'.",
		"TRYAGAIN Multiple keys request during rehashing of slot",
		"CLUSTERDOWN The cluster is down",
		"ERR max number of clients reached",
		"READONLY You can't write against a read only replica.",
		"NOREPLICAS Not enough good replicas to write.",
	}
	for _, rejection := range rejections {
		for _, call := range writeCalls {
			code := strings.Fields(rejection)[0]
			if code == "ERR" {
				code = "MAXCLIENTS"
			}
			t.Run(code+"/"+call.name, func(t *testing.T) {
				_, countAt := call.serverName()
				plans := map[string][]standInAction{countAt: {{reject: rejection}}}
				if call.name == "pipeline" {
					// 整条都被拒绝才是“整条未执行”。
					plans["rpush"] = []standInAction{{reject: rejection}}
				}
				server := newRESPStandIn(t, plans)
				client := standInClient(t, server)
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if err := call.run(ctx, client); err != nil {
					t.Fatalf("%s rejected once with %q (never executed) failed instead of being resent: %v", call.name, rejection, err)
				}
				if got := server.executions(countAt); got != 1 {
					t.Fatalf("%s executed %d times, want 1", call.name, got)
				}
			})
		}
	}
}

// 重发次数用完时把最后一次“确定未执行”的错误交回，分类不变。
func TestResendsStopAtTheConfiguredBudget(t *testing.T) {
	loading := standInAction{reject: "LOADING Redis is loading the dataset in memory"}
	server := newRESPStandIn(t, map[string][]standInAction{"incr": {loading, loading, loading, loading, loading}})
	client := standInClient(t, server)
	_, err := client.Incr(context.Background(), "k")
	if !goredis.IsLoadingError(err) || !IsDefinitelyNotExecuted(err) {
		t.Fatalf("err=%v, want the LOADING rejection after the budget", err)
	}
	if got := server.executions("incr"); got != 0 {
		t.Fatalf("incr executed %d times", got)
	}
	server.mu.Lock()
	left := len(server.plans["incr"])
	server.mu.Unlock()
	if left != 1 { // 1 次发送 + 3 次重发
		t.Fatalf("%d rejections left unused, want 1 (one send plus three resends)", left)
	}
}

// 部分命令确实执行过的 pipeline 不整条重发。
func TestAPipelineWithAnExecutedCommandIsNotResent(t *testing.T) {
	server := newRESPStandIn(t, map[string][]standInAction{"rpush": {{reject: "LOADING Redis is loading the dataset in memory"}}})
	client := standInClient(t, server)
	pipe := client.Pipeline()
	pipe.Incr(context.Background(), "counter")
	pipe.RPush(context.Background(), "k", "a")
	if err := pipe.Exec(context.Background()); !goredis.IsLoadingError(err) {
		t.Fatalf("Exec err=%v, want the RPUSH rejection", err)
	}
	if got := server.executions("incr"); got != 1 {
		t.Fatalf("INCR executed %d times; a pipeline with an executed command must not be resent", got)
	}
}

// 读命令照常交给驱动重试：回复丢失的 GET 换连接再读一次。
func TestReadsKeepTheDriverRetry(t *testing.T) {
	server := newRESPStandIn(t, map[string][]standInAction{"get": {{drop: true}}})
	client := standInClient(t, server)
	value, err := client.Get(context.Background(), "k")
	if err != nil || string(value) != "v" {
		t.Fatalf("Get = %q, %v; a read whose reply was lost should be retried by the driver", value, err)
	}
	if got := server.executions("get"); got != 2 {
		t.Fatalf("get executed %d times, want 2 (driver retry)", got)
	}
}

// EvalBatchDurable 的整批（脚本 + WAITAOF）都被拒绝时换新连接整批重发。
func TestDurableBatchIsResentWhenNothingExecuted(t *testing.T) {
	loading := standInAction{reject: "LOADING Redis is loading the dataset in memory"}
	server := newRESPStandIn(t, map[string][]standInAction{"eval": {loading}, "waitaof": {loading}})
	client := standInClient(t, server)
	results, local, _, err := client.EvalBatchDurable(context.Background(), "return 1", []fredis.EvalCall{{Keys: []string{"k"}}}, 1, 0, time.Second)
	if err != nil || len(results) != 1 || local != 1 {
		t.Fatalf("EvalBatchDurable = %v, local=%d, %v; want one result after one resend", results, local, err)
	}
	if got := server.executions("eval"); got != 1 {
		t.Fatalf("eval executed %d times, want 1", got)
	}
}

func TestIsDefinitelyNotExecuted(t *testing.T) {
	dial := &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connect: connection refused")}
	dialTimeout := &net.OpError{Op: "dial", Net: "tcp", Err: context.DeadlineExceeded}
	readTimeout := &net.OpError{Op: "read", Net: "tcp", Err: os.ErrDeadlineExceeded}
	for _, tc := range []struct {
		name   string
		err    error
		want   bool
		resend bool
	}{
		{"nil", nil, false, false},
		{"dial refused", dial, true, true},
		{"dial timeout", fmt.Errorf("redis: %w", dialTimeout), true, true},
		{"pool timeout", fmt.Errorf("get conn: %w", goredis.ErrPoolTimeout), true, true},
		{"pool exhausted", goredis.ErrPoolExhausted, true, true},
		{"client closed", goredis.ErrClosed, true, false},
		{"dial then deadline in backoff", errors.Join(dial, context.DeadlineExceeded), true, true},
		{"EOF", io.EOF, false, false},
		{"unexpected EOF", fmt.Errorf("read: %w", io.ErrUnexpectedEOF), false, false},
		{"read timeout", readTimeout, false, false},
		{"caller deadline", context.DeadlineExceeded, false, false},
		{"caller cancel", context.Canceled, false, false},
		{"script error reply", errors.New("ERR user_script:1: boom"), false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsDefinitelyNotExecuted(tc.err); got != tc.want {
				t.Fatalf("IsDefinitelyNotExecuted(%v) = %v, want %v", tc.err, got, tc.want)
			}
			if got := shouldResend(tc.err); got != tc.resend {
				t.Fatalf("shouldResend(%v) = %v, want %v", tc.err, got, tc.resend)
			}
		})
	}
}

// 一次 Eval 至多让服务端执行一次脚本：回复丢失时脚本可能已经执行，驱动不自动重放，把错误原样交回调用方按自己的语义裁决。
// 用只会说 RESP 的本地替身服务端：第一次收到 EVAL 时“执行”（计数）后直接断开连接、不回复，之后的 EVAL 正常回复；驱动按生产构造。
// 历史：RR-20261005-NC-100。

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

// 不可重放标记（noReplay）克隆后仍在，没加标记的命令保留驱动的自动重试。原名 TestOnlyScriptCommandsOptOutOfTheDriverRetry：
// A2 之后写命令同样加标记（见本文件前面的写命令用例），“只有脚本”不再成立，按实际断言改名。
func TestNoReplayMarkSurvivesCloneAndUnmarkedCommandsKeepTheDriverRetry(t *testing.T) {
	server := newDropFirstScriptReply(t)
	cfg := fredis.DefaultConfig(server.listener.Addr().String())
	cfg.MinIdleConns = 0
	client := NewRedisClient(cfg)
	defer client.Close()
	if err := client.Ping(context.Background()); err != nil {
		t.Fatalf("ping: %v", err)
	}
	cmd := noReplay{newScriptCmd(context.Background(), "eval", "return 1", []string{"k"})}
	if !cmd.NoRetry() || !cmd.Clone().NoRetry() {
		t.Fatal("a noReplay-marked command (or its clone) is retryable")
	}
	if goredis.NewCmd(context.Background(), "get", "k").NoRetry() {
		t.Fatal("ordinary commands lost their retry")
	}
}

// O-M6-3（docs/feature/MIRROR-M6-OBSERVATIONS-2026-10-06.md）：EvalReplicated 的契约。
//
//   - WAIT 与脚本在同一条连接上（WAIT 只统计本连接之前的写）；
//   - 主节点没有连着的副本时不发 WAIT（单机开发环境不被卡到超时）；
//   - 副本确认不足不是错误；ROLE / WAIT 出错时脚本结果照常返回，错误只在 WaitErr；
//   - 脚本自己的错误照 Eval 的分类返回，不发 WAIT；
//   - numReplicas / timeout 不为正时普通发送脚本。

// recordingRESPServer 与 cannedRESPServer 相同，另按连接记录收到的命令名。
func recordingRESPServer(t *testing.T, replies map[string]string) (*goredis.Client, func() [][]string) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	var mu sync.Mutex
	var conns [][]string
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			index := len(conns)
			conns = append(conns, nil)
			mu.Unlock()
			go func(conn net.Conn) {
				defer func() { _ = conn.Close() }()
				reader := bufio.NewReader(conn)
				for {
					args, err := readRESPCommand(reader)
					if err != nil {
						return
					}
					name := strings.ToUpper(args[0])
					if name != "HELLO" { // 连接握手，不计
						mu.Lock()
						conns[index] = append(conns[index], name)
						mu.Unlock()
					}
					reply, ok := replies[name]
					if !ok {
						reply = "-ERR unknown command '" + args[0] + "'\r\n"
					}
					if _, err := io.WriteString(conn, reply); err != nil {
						return
					}
				}
			}(conn)
		}
	}()
	client := goredis.NewClient(&goredis.Options{
		Addr: listener.Addr().String(), Protocol: 2, DisableIdentity: true,
		MaxRetries: -1, DialTimeout: time.Second, ReadTimeout: time.Second, WriteTimeout: time.Second,
	})
	t.Cleanup(func() { _ = client.Close() })
	return client, func() [][]string {
		mu.Lock()
		defer mu.Unlock()
		out := make([][]string, len(conns))
		for i, c := range conns {
			out[i] = append([]string(nil), c...)
		}
		return out
	}
}

const (
	roleOneReplica = "*3\r\n$6\r\nmaster\r\n:100\r\n*1\r\n*3\r\n$9\r\n127.0.0.1\r\n$4\r\n6380\r\n$3\r\n100\r\n"
	roleNoReplica  = "*3\r\n$6\r\nmaster\r\n:100\r\n*0\r\n"
)

// connectionWith 返回含 command 的那条连接上的命令序列。
func connectionWith(conns [][]string, command string) []string {
	for _, c := range conns {
		for _, name := range c {
			if name == command {
				return c
			}
		}
	}
	return nil
}

func TestEvalReplicatedWaitsOnTheScriptsConnection(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name     string
		role     string
		wait     string
		waited   bool
		replicas int64
		skipped  string
		waitErr  bool
	}{
		{name: "confirmed", role: roleOneReplica, wait: ":1\r\n", waited: true, replicas: 1},
		{name: "short", role: roleOneReplica, wait: ":0\r\n", waited: true, replicas: 0},
		{name: "no replicas", role: roleNoReplica, skipped: fredis.ReplicatedSkipNoReplicas},
		{name: "wait error", role: roleOneReplica, wait: "-ERR wait failed\r\n", waitErr: true},
		{name: "not a master", role: "*5\r\n$5\r\nslave\r\n$9\r\n127.0.0.1\r\n:6379\r\n$9\r\nconnected\r\n:10\r\n", waitErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			replies := map[string]string{"EVAL": ":1\r\n", "ROLE": tc.role}
			if tc.wait != "" {
				replies["WAIT"] = tc.wait
			}
			rdb, commands := recordingRESPServer(t, replies)
			client := &Client{rdb: rdb}
			got, err := client.EvalReplicated(ctx, "return 1", []string{"k"}, 1, 50*time.Millisecond)
			if err != nil {
				t.Fatalf("script error %v; the script succeeded", err)
			}
			if got.Result != int64(1) || got.Waited != tc.waited || got.Replicas != tc.replicas || got.Skipped != tc.skipped || (got.WaitErr != nil) != tc.waitErr {
				t.Fatalf("result=%+v, want waited=%v replicas=%d skipped=%q waitErr=%v", got, tc.waited, tc.replicas, tc.skipped, tc.waitErr)
			}
			sequence := connectionWith(commands(), "EVAL")
			wantWait := tc.wait != ""
			if wantWait && strings.Join(sequence, ",") != "EVAL,ROLE,WAIT" {
				t.Fatalf("commands on the script's connection = %v, want EVAL,ROLE,WAIT", sequence)
			}
			if !wantWait && strings.Join(sequence, ",") != "EVAL,ROLE" {
				t.Fatalf("commands on the script's connection = %v, want EVAL,ROLE (no WAIT)", sequence)
			}
		})
	}
}

func TestEvalReplicatedScriptErrorsAndDisabledWait(t *testing.T) {
	ctx := context.Background()
	rdb, commands := recordingRESPServer(t, map[string]string{"EVAL": "-ERR user_script failed\r\n", "ROLE": roleOneReplica, "WAIT": ":1\r\n"})
	client := &Client{rdb: rdb}
	if _, err := client.EvalReplicated(ctx, "return 1", []string{"k"}, 1, 50*time.Millisecond); err == nil || !strings.Contains(err.Error(), "user_script") {
		t.Fatalf("script error = %v, want the script's own error", err)
	}
	if seq := connectionWith(commands(), "WAIT"); seq != nil {
		t.Fatalf("WAIT was sent after a failed script: %v", seq)
	}

	rdb, commands = recordingRESPServer(t, map[string]string{"EVAL": ":1\r\n"})
	client = &Client{rdb: rdb}
	got, err := client.EvalReplicated(ctx, "return 1", []string{"k"}, 0, 50*time.Millisecond)
	if err != nil || got.Result != int64(1) || got.Skipped != fredis.ReplicatedSkipDisabled || got.Waited {
		t.Fatalf("disabled: result=%+v err=%v", got, err)
	}
	if seq := connectionWith(commands(), "EVAL"); strings.Join(seq, ",") != "EVAL" {
		t.Fatalf("disabled sent %v, want only EVAL", seq)
	}
}
