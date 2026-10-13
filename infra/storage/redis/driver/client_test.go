// Redis 客户端：WAITAOF 与整数解析、缺失键的错误身份、上下文截止、集群恢复不重放、装配、pipeline 错误。

package driver

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"
	"github.com/tjbdwanghaibo/roost-core/infra/storage/redis"
	fredis "github.com/tjbdwanghaibo/roost-core/infra/storage/redis"
)

func TestRedisIntegerParsesWAITAOFReplyValues(t *testing.T) {
	tests := []struct {
		name  string
		value any
		want  int64
	}{
		{name: "int64", value: int64(1), want: 1},
		{name: "int", value: 2, want: 2},
		{name: "string", value: "3", want: 3},
		{name: "bytes", value: []byte("4"), want: 4},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := redisInteger(test.value)
			if err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Fatalf("redisInteger(%v) = %d, want %d", test.value, got, test.want)
			}
		})
	}
}

func TestRedisIntegerRejectsInvalidWAITAOFReplyValue(t *testing.T) {
	if _, err := redisInteger([]any{1}); err == nil {
		t.Fatal("redisInteger accepted an invalid reply type")
	}
}

func TestEvalDurableRejectsClusterTopologyBeforeIO(t *testing.T) {
	cluster := goredis.NewClusterClient(&goredis.ClusterOptions{Addrs: []string{"127.0.0.1:1"}})
	defer func() { _ = cluster.Close() }()
	client := &Client{rdb: cluster}

	_, _, _, err := client.EvalDurable(context.Background(), "return 1", []string{"{probe}:key"}, 1, 0, time.Second)
	if err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("EvalDurable cluster error = %v", err)
	}
}

// 错误身份：go-redis 的 redis.Nil 变成 fredis.ErrNil（七个读接口各有一处映射）；回复形状：WAITAOF 的回复、整数解析越界报错而不是越界
// 访问或静默截断。

// nilReplyRedis 对七个读命令一律回答 redis.Nil；其余方法保持 nil 接口指针，
// 未预期的调用直接 panic。
type nilReplyRedis struct {
	builderOnly
	wireErr error // 非 nil 时改为返回这个错误，验证非 Nil 错误不得被映射
}

// Process 接住写路径（LPop / RPop 经 sendOnce 发出）：一律回答同一个错误。
func (r nilReplyRedis) Process(_ context.Context, cmd goredis.Cmder) error {
	cmd.SetErr(r.reply())
	return r.reply()
}

func (r nilReplyRedis) reply() error {
	if r.wireErr != nil {
		return r.wireErr
	}
	return goredis.Nil
}
func (r nilReplyRedis) Get(context.Context, string) *goredis.StringCmd {
	return goredis.NewStringResult("", r.reply())
}
func (r nilReplyRedis) HGet(context.Context, string, string) *goredis.StringCmd {
	return goredis.NewStringResult("", r.reply())
}
func (r nilReplyRedis) LPop(context.Context, string) *goredis.StringCmd {
	return goredis.NewStringResult("", r.reply())
}
func (r nilReplyRedis) RPop(context.Context, string) *goredis.StringCmd {
	return goredis.NewStringResult("", r.reply())
}
func (r nilReplyRedis) ZScore(context.Context, string, string) *goredis.FloatCmd {
	return goredis.NewFloatResult(0, r.reply())
}
func (r nilReplyRedis) ZRank(context.Context, string, string) *goredis.IntCmd {
	return goredis.NewIntResult(0, r.reply())
}
func (r nilReplyRedis) ZRevRank(context.Context, string, string) *goredis.IntCmd {
	return goredis.NewIntResult(0, r.reply())
}

func TestMissingKeysSurfaceAsContractErrNil(t *testing.T) {
	ctx := context.Background()
	reads := []struct {
		name string
		call func(c *Client) error
	}{
		{"Get", func(c *Client) error { _, err := c.Get(ctx, "k"); return err }},
		{"HGet", func(c *Client) error { _, err := c.HGet(ctx, "k", "f"); return err }},
		{"LPop", func(c *Client) error { _, err := c.LPop(ctx, "k"); return err }},
		{"RPop", func(c *Client) error { _, err := c.RPop(ctx, "k"); return err }},
		{"ZScore", func(c *Client) error { _, err := c.ZScore(ctx, "k", "m"); return err }},
		{"ZRank", func(c *Client) error { _, err := c.ZRank(ctx, "k", "m"); return err }},
		{"ZRevRank", func(c *Client) error { _, err := c.ZRevRank(ctx, "k", "m"); return err }},
	}
	missing := &Client{rdb: nilReplyRedis{}}
	wireErr := errors.New("wire: connection reset")
	broken := &Client{rdb: nilReplyRedis{wireErr: wireErr}}
	for _, read := range reads {
		t.Run(read.name, func(t *testing.T) {
			err := read.call(missing)
			if !errors.Is(err, fredis.ErrNil) {
				t.Fatalf("missing key: err=%v, want fredis.ErrNil", err)
			}
			if errors.Is(err, goredis.Nil) {
				t.Fatalf("missing key: driver error leaked through the contract: %v", err)
			}
			if err := read.call(broken); !errors.Is(err, wireErr) || errors.Is(err, fredis.ErrNil) {
				t.Fatalf("wire error: err=%v, want the wire error untouched", err)
			}
		})
	}
}

// cannedRESPServer 是一个只会照本宣科的 RESP 服务端：按命令名回放固定回复。
// EvalBatchDurable 需要 *goredis.Client 的独占连接，接口替身走不到 WAITAOF
// 回复解析，所以这里用真实的 go-redis 连接对着假服务端。
func cannedRESPServer(t *testing.T, replies map[string]string) *goredis.Client {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer func() { _ = conn.Close() }()
				reader := bufio.NewReader(conn)
				for {
					args, err := readRESPCommand(reader)
					if err != nil {
						return
					}
					reply, ok := replies[strings.ToUpper(args[0])]
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
	return client
}

func readRESPCommand(reader *bufio.Reader) ([]string, error) {
	header, err := reader.ReadString('\n')
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(header, "*") {
		return nil, fmt.Errorf("resp: want array, got %q", header)
	}
	count, err := strconv.Atoi(strings.TrimSpace(header[1:]))
	if err != nil {
		return nil, err
	}
	args := make([]string, 0, count)
	for i := 0; i < count; i++ {
		sizeLine, err := reader.ReadString('\n')
		if err != nil {
			return nil, err
		}
		size, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(sizeLine, "$")))
		if err != nil {
			return nil, err
		}
		buf := make([]byte, size+2)
		if _, err := io.ReadFull(reader, buf); err != nil {
			return nil, err
		}
		args = append(args, string(buf[:size]))
	}
	return args, nil
}

func TestEvalDurableRejectsMalformedWAITAOFReply(t *testing.T) {
	ctx := context.Background()
	// 对照：形状正确的回复正常解析，证明假服务端与连接握手都工作。
	healthy := &Client{rdb: cannedRESPServer(t, map[string]string{
		"EVAL": ":7\r\n", "WAITAOF": "*2\r\n:1\r\n:0\r\n",
	})}
	result, local, replicas, err := healthy.EvalDurable(ctx, "return 7", []string{"k"}, 1, 0, time.Second)
	if err != nil || result != int64(7) || local != 1 || replicas != 0 {
		t.Fatalf("healthy EvalDurable = (%v, %d, %d, %v)", result, local, replicas, err)
	}

	short := &Client{rdb: cannedRESPServer(t, map[string]string{
		"EVAL": ":7\r\n", "WAITAOF": "*1\r\n:1\r\n",
	})}
	_, _, _, err = short.EvalDurable(ctx, "return 7", []string{"k"}, 1, 0, time.Second)
	if err == nil || !strings.Contains(err.Error(), "WAITAOF reply length") {
		t.Fatalf("one-element WAITAOF reply: err=%v, want a reply-length error", err)
	}
}

func TestRedisIntegerRejectsUnsignedOverflow(t *testing.T) {
	if got, err := redisInteger(uint64(1<<63 - 1)); err != nil || got != 1<<63-1 {
		t.Fatalf("max int64 as uint64: (%d, %v)", got, err)
	}
	got, err := redisInteger(uint64(1 << 63))
	if err == nil {
		t.Fatalf("uint64 above MaxInt64 was accepted as %d", got)
	}
}

// pipelineStub 只实现 pipeline 包装器会调用的 LPop 与 Exec。
// pipelineStub 收下入队的命令；Exec 像 go-redis 一样把错误写到每条命令上并返回它。
type pipelineStub struct {
	goredis.Pipeliner
	execErr error
	queued  []goredis.Cmder
}

func (p *pipelineStub) Process(_ context.Context, cmd goredis.Cmder) error {
	p.queued = append(p.queued, cmd)
	return nil
}
func (p *pipelineStub) Exec(context.Context) ([]goredis.Cmder, error) {
	cmds := p.queued
	p.queued = nil
	for _, cmd := range cmds {
		cmd.SetErr(p.execErr)
	}
	return cmds, p.execErr
}

func stubPipeline(execErr error) *pipeline {
	return &pipeline{rdb: builderOnly{}, pipe: &pipelineStub{execErr: execErr}, resends: 3}
}

// go-redis 的 Pipeline.Exec 会把第一条命令的 redis.Nil 当作整条 pipeline 的错误
// 返回。对调用方而言"某个键不存在"不是 pipeline 失败：Exec 必须返回 nil，
// 缺失只体现在对应 future 的 ErrNil 上；而真正的传输错误必须原样上抛。
func TestPipelineExecToleratesNilButPropagatesRealErrors(t *testing.T) {
	ctx := context.Background()

	missing := stubPipeline(goredis.Nil)
	future := missing.LPop(ctx, "queue")
	if err := missing.Exec(ctx); err != nil {
		t.Fatalf("Exec with a Nil reply: err=%v, want nil", err)
	}
	if _, err := future.Result(); !errors.Is(err, fredis.ErrNil) {
		t.Fatalf("LPop future: err=%v, want fredis.ErrNil", err)
	}

	wireErr := errors.New("wire: broken pipe")
	broken := stubPipeline(wireErr)
	broken.LPop(ctx, "queue")
	if err := broken.Exec(ctx); !errors.Is(err, wireErr) {
		t.Fatalf("Exec with a wire error: err=%v, want %v", err, wireErr)
	}
}

// commandBuilder 是一个从不连接的 go-redis 客户端，只用来给替身提供 Pipeline()：
// 驱动用它组装写命令（参数展开与类型化方法一致），真正发送走替身的 Process。
var commandBuilder = goredis.NewClient(&goredis.Options{Addr: "127.0.0.1:1"})

// builderOnly 是只会组装命令的 UniversalClient 替身，其余方法保持 nil 接口指针。
type builderOnly struct{ goredis.UniversalClient }

func (builderOnly) Pipeline() goredis.Pipeliner { return commandBuilder.Pipeline() }

// NewClient exists so a caller outside the kit can exercise real Redis
// semantics — Lua, pipelines — that an in-memory double reimplements rather
// than executes. It must refuse a configuration that cannot connect anywhere
// rather than hand back a client that fails on first use.
func TestNewClientRequiresAnAddress(t *testing.T) {
	if _, err := NewClient(nil); err == nil {
		t.Fatal("a nil configuration was accepted")
	}
	if _, err := NewClient(&fredis.Config{}); err == nil {
		t.Fatal("a configuration with no address was accepted")
	}
	client, err := NewClient(fredis.DefaultConfig("127.0.0.1:6379"))
	if err != nil {
		t.Fatalf("a single-node configuration was rejected: %v", err)
	}
	if client == nil {
		t.Fatal("NewClient returned a nil client with no error")
	}
	_ = client.Close()

	cluster, err := NewClient(&fredis.Config{ClusterAddrs: []string{"127.0.0.1:7000"}})
	if err != nil {
		t.Fatalf("a cluster configuration was rejected: %v", err)
	}
	_ = cluster.Close()
}

// The caller's context deadline must reach the wire. go-redis only honours it
// with ContextTimeoutEnabled; without that flag a 500ms Acquire budget waits
// for the full ReadTimeout under latency (seen against toxiproxy).
func TestRedisClientsHonourContextDeadlinesOnTheWire(t *testing.T) {
	single := NewRedisClient(&fredis.Config{Addr: "127.0.0.1:1", ReadTimeout: 2 * time.Second})
	client, ok := single.rdb.(*goredis.Client)
	if !ok {
		t.Fatalf("single-node client is %T", single.rdb)
	}
	if !client.Options().ContextTimeoutEnabled {
		t.Fatal("single-node client does not honour context deadlines")
	}
	cluster := NewRedisClient(&fredis.Config{ClusterAddrs: []string{"127.0.0.1:1"}, ReadTimeout: 2 * time.Second})
	clusterClient, ok := cluster.rdb.(*goredis.ClusterClient)
	if !ok {
		t.Fatalf("cluster client is %T", cluster.rdb)
	}
	if !clusterClient.Options().ContextTimeoutEnabled {
		t.Fatal("cluster client does not honour context deadlines")
	}
}

func TestClusterRecoveryPreservesErrorsWithoutReplayingCommands(t *testing.T) {
	// 已关闭客户端让异步刷新立即退出，断言只关注 hook 的业务契约；真实选主由集成测试覆盖。
	client := goredis.NewClusterClient(&goredis.ClusterOptions{Addrs: []string{"127.0.0.1:1"}})
	_ = client.Close()
	h := clusterRecoveryHook{client: client}
	for _, failure := range []error{&net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}, context.Canceled, context.DeadlineExceeded, errors.New("business error")} {
		calls := 0
		err := h.ProcessHook(func(context.Context, goredis.Cmder) error { calls++; return failure })(context.Background(), goredis.NewCmd(context.Background(), "eval"))
		if err != failure || calls != 1 {
			t.Fatalf("command calls=%d error=%v want=%v", calls, err, failure)
		}
		calls = 0
		err = h.ProcessPipelineHook(func(context.Context, []goredis.Cmder) error { calls++; return failure })(context.Background(), nil)
		if err != failure || calls != 1 {
			t.Fatalf("pipeline calls=%d error=%v want=%v", calls, err, failure)
		}
	}
}

// P3b: the Mod publishes exactly what Assemble returns, so Assemble must
// refuse what the Mod used to refuse (no address) and must put the client and
// the lock factory on one connection pool — a lock factory on a second pool
// would double the connection count and make the Mod's Ping meaningless for
// locks.
func TestAssembleBuildsClientAndLocksOnOnePool(t *testing.T) {
	if _, err := Assemble(nil); err == nil {
		t.Fatal("nil configuration was assembled")
	}
	if _, err := Assemble(&fredis.Config{}); err == nil {
		t.Fatal("configuration without an address was assembled")
	}
	asm, err := Assemble(fredis.DefaultConfig("127.0.0.1:6379"))
	if err != nil {
		t.Fatal(err)
	}
	if asm.Client == nil || asm.Locks == nil {
		t.Fatalf("assembly incomplete: %+v", asm)
	}
	if asm.Locks.rdb != asm.Client.rdb {
		t.Fatal("lock factory does not share the client's connection pool")
	}
	var _ fredis.IRedis = asm.Client
	var _ fredis.IDistLockFactory = asm.Locks
	if err := asm.Close(); err != nil {
		t.Fatalf("Close = %v", err)
	}

	var none *Assembly
	if err := none.Ping(context.Background()); err == nil {
		t.Fatal("nil assembly Ping returned no error")
	}
	if err := none.Close(); err != nil {
		t.Fatalf("nil assembly Close = %v", err)
	}
}

// Fixed command results isolate wrapper error selection from network routing.
// Real-backend tests separately execute the buffered Redis commands.
type pipelineResultsStub struct {
	goredis.Pipeliner
	commands []goredis.Cmder
	execErr  error
}

func (s *pipelineResultsStub) Exec(context.Context) ([]goredis.Cmder, error) {
	return s.commands, s.execErr
}

func TestPipelineExecChecksEveryCommandError(t *testing.T) {
	commandErr := errors.New("command failed")
	wireErr := fmt.Errorf("reply lost: %w", context.DeadlineExceeded)
	for _, tc := range []struct {
		name      string
		aggregate error
		command   error
		want      error
	}{
		{"missing_before_failure", goredis.Nil, commandErr, commandErr},
		{"nil_aggregate_with_failure", nil, commandErr, commandErr},
		{"only_missing", goredis.Nil, goredis.Nil, nil},
		{"all_success", nil, nil, nil},
		{"transport_error_preserved", wireErr, commandErr, context.DeadlineExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stub := &pipelineResultsStub{execErr: tc.aggregate, commands: []goredis.Cmder{
				goredis.NewStringResult("", goredis.Nil),
				goredis.NewStatusResult("", tc.command),
			}}
			err := (&pipeline{pipe: stub}).Exec(context.Background())
			if !errors.Is(err, tc.want) {
				t.Fatalf("Exec=%v, want %v", err, tc.want)
			}
		})
	}
}

func TestPipelineExecPopulatesAllFuturesOnFailure(t *testing.T) {
	backendErr := errors.New("write rejected")
	str := goredis.NewStringResult("value", nil)
	missing := goredis.NewStringResult("", goredis.Nil)
	bad := goredis.NewStringResult("", backendErr)
	integer := goredis.NewIntResult(42, nil)
	hash := goredis.NewMapStringStringResult(map[string]string{"field": "value"}, nil)
	stub := &pipelineResultsStub{execErr: goredis.Nil, commands: []goredis.Cmder{missing, str, bad, integer, hash}}
	pipe := &pipeline{pipe: stub}
	// Use the same tracked command/future pairs Get/Incr/HGetAll establish.
	goodFuture, missingFuture, badFuture := &redis.FutureBytes{}, &redis.FutureBytes{}, &redis.FutureBytes{}
	intFuture, mapFuture := &redis.FutureInt64{}, &redis.FutureStringMap{}
	pipe.bytesFutures = []*pipelineBytesCmd{{cmd: str, future: goodFuture}, {cmd: missing, future: missingFuture}, {cmd: bad, future: badFuture}}
	pipe.int64Futures = []*pipelineInt64Cmd{{cmd: integer, future: intFuture}}
	pipe.stringMapFutures = []*pipelineStringMapCmd{{cmd: hash, future: mapFuture}}
	if err := pipe.Exec(context.Background()); !errors.Is(err, backendErr) {
		t.Fatalf("Exec: %v", err)
	}
	if value, err := goodFuture.Result(); err != nil || string(value) != "value" {
		t.Fatalf("good: %q %v", value, err)
	}
	if _, err := missingFuture.Result(); !errors.Is(err, redis.ErrNil) {
		t.Fatalf("missing: %v", err)
	}
	if _, err := badFuture.Result(); !errors.Is(err, backendErr) {
		t.Fatalf("bad: %v", err)
	}
	if value, err := intFuture.Result(); err != nil || value != 42 {
		t.Fatalf("int: %d %v", value, err)
	}
	if value, err := mapFuture.Result(); err != nil || value["field"] != "value" {
		t.Fatalf("map: %v %v", value, err)
	}
}
