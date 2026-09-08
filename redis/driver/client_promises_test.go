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
	fredis "github.com/tjbdwanghaibo/roost-core/redis"
)

// U-0105 · C2（空洞测试）· B-19。
//
// 驱动包对契约包的第一条承诺是错误身份：go-redis 的 redis.Nil 必须变成
// fredis.ErrNil，否则 cache / dataengine / service 里所有 errors.Is(err, redis.ErrNil)
// 的"不存在"分支都会把 miss 当故障。七个读接口各有一处映射，此前没有任何
// 测试触达（gap map 19/24 无覆盖）。第二条是回复形状：WAITAOF 的回复、整数
// 解析越界要报错而不是越界访问或静默截断。

// nilReplyRedis 对七个读命令一律回答 redis.Nil；其余方法保持 nil 接口指针，
// 未预期的调用直接 panic。
type nilReplyRedis struct {
	goredis.UniversalClient
	wireErr error // 非 nil 时改为返回这个错误，验证非 Nil 错误不得被映射
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
type pipelineStub struct {
	goredis.Pipeliner
	execErr error
}

func (p pipelineStub) LPop(context.Context, string) *goredis.StringCmd {
	return goredis.NewStringResult("", goredis.Nil)
}
func (p pipelineStub) Exec(context.Context) ([]goredis.Cmder, error) { return nil, p.execErr }

// go-redis 的 Pipeline.Exec 会把第一条命令的 redis.Nil 当作整条 pipeline 的错误
// 返回。对调用方而言"某个键不存在"不是 pipeline 失败：Exec 必须返回 nil，
// 缺失只体现在对应 future 的 ErrNil 上；而真正的传输错误必须原样上抛。
func TestPipelineExecToleratesNilButPropagatesRealErrors(t *testing.T) {
	ctx := context.Background()

	missing := newPipeline(pipelineStub{execErr: goredis.Nil})
	future := missing.LPop(ctx, "queue")
	if err := missing.Exec(ctx); err != nil {
		t.Fatalf("Exec with a Nil reply: err=%v, want nil", err)
	}
	if _, err := future.Result(); !errors.Is(err, fredis.ErrNil) {
		t.Fatalf("LPop future: err=%v, want fredis.ErrNil", err)
	}

	wireErr := errors.New("wire: broken pipe")
	broken := newPipeline(pipelineStub{execErr: wireErr})
	broken.LPop(ctx, "queue")
	if err := broken.Exec(ctx); !errors.Is(err, wireErr) {
		t.Fatalf("Exec with a wire error: err=%v, want %v", err, wireErr)
	}
}
