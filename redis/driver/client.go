package driver

import (
	"context"
	"errors"
	"fmt"
	fredis "github.com/tjbdwanghaibo/roost-core/redis"
	"strconv"
	"sync"
	"time"

	goredis "github.com/redis/go-redis/v9"
)

// Client implements fredis.IRedis by wrapping go-redis.
//
// 读命令交给 go-redis 自动重试；写命令（含脚本、含写的 pipeline）只发一次，驱动不重放，
// 仅在错误证明命令没执行时重发（replay.go，A2）。行为契约表见 README.md。
type Client struct {
	rdb goredis.UniversalClient
	// resends 是写命令在“确定未执行”错误上的重发次数，由 Config.MaxRetries 换算（resendsFor）。
	resends int

	// closeOnce 让 Close 只关一次连接池，见 Close。
	closeOnce sync.Once
}

// Raw exposes the underlying go-redis client for assembly code (lock
// factories, health probes) that needs the driver handle; business code
// should stay on fredis.IRedis.
func (c *Client) Raw() goredis.UniversalClient {
	if c == nil {
		return nil
	}
	return c.rdb
}

// NewClient builds a client from a configuration, outside the Mod lifecycle.
//
// Production wiring goes through RedisMod, which owns config parsing, the
// registry capability and shutdown. This exists for the cases that have no
// registry: integration tests that must exercise real Redis semantics
// (Lua scripts, pipelines, WATCH) that an in-memory double reimplements
// rather than executes, and one-off operational tools. Callers own Close.
func NewClient(cfg *fredis.Config) (fredis.IRedis, error) {
	if cfg == nil {
		return nil, fmt.Errorf("redis: configuration is required")
	}
	if cfg.Addr == "" && !cfg.IsCluster() {
		return nil, fmt.Errorf("redis: addr or cluster addrs are required")
	}
	return NewRedisClient(cfg), nil
}

func NewRedisClient(cfg *fredis.Config) *Client {
	var rdb goredis.UniversalClient
	if cfg.IsCluster() {
		rdb = goredis.NewClusterClient(&goredis.ClusterOptions{
			Addrs:        cfg.ClusterAddrs,
			Password:     cfg.Password,
			PoolSize:     cfg.PoolSize,
			MinIdleConns: cfg.MinIdleConns,
			DialTimeout:  cfg.DialTimeout,
			ReadTimeout:  cfg.ReadTimeout,
			WriteTimeout: cfg.WriteTimeout,
			MaxRetries:   cfg.MaxRetries,
			// Without this go-redis ignores the caller's context deadline on
			// the wire and waits for ReadTimeout instead: a lock Acquire with a
			// 500ms budget sat for the full 2s read timeout under injected
			// latency. The caller's deadline is the contract; ReadTimeout is
			// only the backstop for callers that gave none.
			ContextTimeoutEnabled: true,
		})
	} else {
		rdb = goredis.NewClient(&goredis.Options{
			Addr:                  cfg.Addr,
			Password:              cfg.Password,
			DB:                    cfg.DB,
			PoolSize:              cfg.PoolSize,
			MinIdleConns:          cfg.MinIdleConns,
			DialTimeout:           cfg.DialTimeout,
			ReadTimeout:           cfg.ReadTimeout,
			WriteTimeout:          cfg.WriteTimeout,
			MaxRetries:            cfg.MaxRetries,
			ContextTimeoutEnabled: true,
		})
	}
	if cluster, ok := rdb.(*goredis.ClusterClient); ok {
		cluster.AddHook(clusterRecoveryHook{client: cluster})
	}
	return &Client{rdb: rdb, resends: resendsFor(cfg.MaxRetries)}
}

// --- String/KV ---

func (c *Client) Get(ctx context.Context, key string) ([]byte, error) {
	val, err := c.rdb.Get(ctx, key).Bytes()
	if err == goredis.Nil {
		return nil, fredis.ErrNil
	}
	return val, err
}

// MGet reads several keys in one round trip.
//
// Two details of the contract are handled here rather than left to callers.
//
// Zero keys returns early WITHOUT a round trip, because `MGET` with no
// arguments is an error in Redis — a caller paging an empty result should not
// have to special-case that, and one that forgot to would fail on an ordinary
// empty page.
//
// The result is positional and always as long as keys: go-redis returns
// `[]any` with a nil for each absent key, and that nil is preserved as a nil
// element rather than dropped. Dropping it would shorten the result, and a
// short result is indistinguishable from a truncated read — which is the
// silent-loss failure this shape exists to make detectable.
func (c *Client) MGet(ctx context.Context, keys ...string) ([][]byte, error) {
	if len(keys) == 0 {
		return nil, nil
	}
	values, err := c.rdb.MGet(ctx, keys...).Result()
	if err != nil {
		return nil, err
	}
	if len(values) != len(keys) {
		// An assertion, not a branch: a conforming driver always returns one
		// element per key, so no test can reach this against a real Redis.
		// It is here because the alternative to failing on a driver bug is
		// returning a result whose positions no longer line up with the keys
		// the caller asked for — misaligned data rather than an error.
		return nil, fmt.Errorf("redis: MGET returned %d values for %d keys", len(values), len(keys))
	}
	out := make([][]byte, len(keys))
	for index, value := range values {
		switch typed := value.(type) {
		case nil:
			// Absent. Left as a nil element, deliberately.
		case string:
			out[index] = []byte(typed)
		case []byte:
			out[index] = append([]byte(nil), typed...)
		default:
			return nil, fmt.Errorf("redis: MGET element %d is %T, want a string", index, value)
		}
	}
	return out, nil
}

func (c *Client) Set(ctx context.Context, key string, value any, expiration time.Duration) error {
	return write(ctx, c, func(p goredis.Pipeliner) *goredis.StatusCmd { return p.Set(ctx, key, value, expiration) }).Err()
}

func (c *Client) SetNX(ctx context.Context, key string, value any, expiration time.Duration) (bool, error) {
	return write(ctx, c, func(p goredis.Pipeliner) *goredis.BoolCmd { return p.SetNX(ctx, key, value, expiration) }).Result()
}

func (c *Client) Del(ctx context.Context, keys ...string) (int64, error) {
	return write(ctx, c, func(p goredis.Pipeliner) *goredis.IntCmd { return p.Del(ctx, keys...) }).Result()
}

func (c *Client) Exists(ctx context.Context, keys ...string) (int64, error) {
	return c.rdb.Exists(ctx, keys...).Result()
}

func (c *Client) Expire(ctx context.Context, key string, expiration time.Duration) (bool, error) {
	return write(ctx, c, func(p goredis.Pipeliner) *goredis.BoolCmd { return p.Expire(ctx, key, expiration) }).Result()
}

func (c *Client) TTL(ctx context.Context, key string) (time.Duration, error) {
	return c.rdb.TTL(ctx, key).Result()
}

func (c *Client) Incr(ctx context.Context, key string) (int64, error) {
	return write(ctx, c, func(p goredis.Pipeliner) *goredis.IntCmd { return p.Incr(ctx, key) }).Result()
}

func (c *Client) IncrBy(ctx context.Context, key string, value int64) (int64, error) {
	return write(ctx, c, func(p goredis.Pipeliner) *goredis.IntCmd { return p.IncrBy(ctx, key, value) }).Result()
}

// --- Hash ---

func (c *Client) HGet(ctx context.Context, key, field string) ([]byte, error) {
	val, err := c.rdb.HGet(ctx, key, field).Bytes()
	if err == goredis.Nil {
		return nil, fredis.ErrNil
	}
	return val, err
}

func (c *Client) HSet(ctx context.Context, key string, values ...any) error {
	return write(ctx, c, func(p goredis.Pipeliner) *goredis.IntCmd { return p.HSet(ctx, key, values...) }).Err()
}

func (c *Client) HGetAll(ctx context.Context, key string) (map[string]string, error) {
	return c.rdb.HGetAll(ctx, key).Result()
}

func (c *Client) HDel(ctx context.Context, key string, fields ...string) (int64, error) {
	return write(ctx, c, func(p goredis.Pipeliner) *goredis.IntCmd { return p.HDel(ctx, key, fields...) }).Result()
}

func (c *Client) HExists(ctx context.Context, key, field string) (bool, error) {
	return c.rdb.HExists(ctx, key, field).Result()
}

// --- List ---

func (c *Client) LPush(ctx context.Context, key string, values ...any) (int64, error) {
	return write(ctx, c, func(p goredis.Pipeliner) *goredis.IntCmd { return p.LPush(ctx, key, values...) }).Result()
}

func (c *Client) RPush(ctx context.Context, key string, values ...any) (int64, error) {
	return write(ctx, c, func(p goredis.Pipeliner) *goredis.IntCmd { return p.RPush(ctx, key, values...) }).Result()
}

func (c *Client) LPop(ctx context.Context, key string) ([]byte, error) {
	val, err := write(ctx, c, func(p goredis.Pipeliner) *goredis.StringCmd { return p.LPop(ctx, key) }).Bytes()
	if err == goredis.Nil {
		return nil, fredis.ErrNil
	}
	return val, err
}

func (c *Client) RPop(ctx context.Context, key string) ([]byte, error) {
	val, err := write(ctx, c, func(p goredis.Pipeliner) *goredis.StringCmd { return p.RPop(ctx, key) }).Bytes()
	if err == goredis.Nil {
		return nil, fredis.ErrNil
	}
	return val, err
}

func (c *Client) LLen(ctx context.Context, key string) (int64, error) {
	return c.rdb.LLen(ctx, key).Result()
}

func (c *Client) LRange(ctx context.Context, key string, start, stop int64) ([]string, error) {
	return c.rdb.LRange(ctx, key, start, stop).Result()
}

// LTrim implements ListTrimmer: in-place trim without the DEL+RPUSH
// loss window of the emulated fallback.
func (c *Client) LTrim(ctx context.Context, key string, start, stop int64) error {
	return write(ctx, c, func(p goredis.Pipeliner) *goredis.StatusCmd { return p.LTrim(ctx, key, start, stop) }).Err()
}

// LRem implements fredis.ListRemover.
func (c *Client) LRem(ctx context.Context, key string, count int64, value any) (int64, error) {
	return write(ctx, c, func(p goredis.Pipeliner) *goredis.IntCmd { return p.LRem(ctx, key, count, value) }).Result()
}

// --- Sorted Set ---

func (c *Client) ZAdd(ctx context.Context, key string, members ...fredis.Z) (int64, error) {
	zs := make([]goredis.Z, len(members))
	for i, m := range members {
		zs[i] = goredis.Z{Score: m.Score, Member: m.Member}
	}
	return write(ctx, c, func(p goredis.Pipeliner) *goredis.IntCmd { return p.ZAdd(ctx, key, zs...) }).Result()
}

func (c *Client) ZRem(ctx context.Context, key string, members ...any) (int64, error) {
	return write(ctx, c, func(p goredis.Pipeliner) *goredis.IntCmd { return p.ZRem(ctx, key, members...) }).Result()
}

func (c *Client) ZScore(ctx context.Context, key string, member string) (float64, error) {
	score, err := c.rdb.ZScore(ctx, key, member).Result()
	if err == goredis.Nil {
		return 0, fredis.ErrNil
	}
	return score, err
}

func (c *Client) ZRank(ctx context.Context, key string, member string) (int64, error) {
	rank, err := c.rdb.ZRank(ctx, key, member).Result()
	if err == goredis.Nil {
		return 0, fredis.ErrNil
	}
	return rank, err
}

func (c *Client) ZRevRank(ctx context.Context, key string, member string) (int64, error) {
	rank, err := c.rdb.ZRevRank(ctx, key, member).Result()
	if err == goredis.Nil {
		return 0, fredis.ErrNil
	}
	return rank, err
}

func (c *Client) ZRangeWithScores(ctx context.Context, key string, start, stop int64) ([]fredis.Z, error) {
	result, err := c.rdb.ZRangeWithScores(ctx, key, start, stop).Result()
	if err != nil {
		return nil, err
	}
	return convertZSlice(result), nil
}

func (c *Client) ZRevRangeWithScores(ctx context.Context, key string, start, stop int64) ([]fredis.Z, error) {
	result, err := c.rdb.ZRevRangeWithScores(ctx, key, start, stop).Result()
	if err != nil {
		return nil, err
	}
	return convertZSlice(result), nil
}

func (c *Client) ZCard(ctx context.Context, key string) (int64, error) {
	return c.rdb.ZCard(ctx, key).Result()
}

// --- Set ---

func (c *Client) SAdd(ctx context.Context, key string, members ...any) (int64, error) {
	return write(ctx, c, func(p goredis.Pipeliner) *goredis.IntCmd { return p.SAdd(ctx, key, members...) }).Result()
}

func (c *Client) SRem(ctx context.Context, key string, members ...any) (int64, error) {
	return write(ctx, c, func(p goredis.Pipeliner) *goredis.IntCmd { return p.SRem(ctx, key, members...) }).Result()
}

func (c *Client) SMembers(ctx context.Context, key string) ([]string, error) {
	return c.rdb.SMembers(ctx, key).Result()
}

func (c *Client) SIsMember(ctx context.Context, key string, member any) (bool, error) {
	return c.rdb.SIsMember(ctx, key, member).Result()
}

// --- Pipeline / Script ---

func (c *Client) Pipeline() fredis.IPipeline {
	return newPipeline(c.rdb, c.resends)
}

// newScriptCmd 与 go-redis 的 cmdable.eval 组装相同的参数与首键位置（集群槽位计算不变）。
func newScriptCmd(ctx context.Context, name, payload string, keys []string, args ...any) *goredis.Cmd {
	cmdArgs := make([]any, 0, 3+len(keys)+len(args))
	cmdArgs = append(cmdArgs, name, payload, len(keys))
	for _, key := range keys {
		cmdArgs = append(cmdArgs, key)
	}
	cmdArgs = append(cmdArgs, args...)
	cmd := goredis.NewCmd(ctx, cmdArgs...)
	if len(keys) > 0 {
		cmd.SetFirstKeyPos(3)
	}
	return cmd
}

// runScript 发一次脚本：驱动不重放（回复丢失时脚本可能已执行，第二次执行会把自己的写当成别人的写，
// RR-20261005-NC-100）；错误证明脚本没执行（LOADING、拨号失败等）时在这里重发，补回 NC-100
// 关掉驱动重试后丢掉的那部分可用性（NC-101 复审“应改”，A2）。
func runScript(ctx context.Context, rdb goredis.UniversalClient, resends int, name, payload string, keys []string, args ...any) *goredis.Cmd {
	cmd := newScriptCmd(ctx, name, payload, keys, args...)
	_ = sendOnce(ctx, rdb, resends, cmd)
	return cmd
}

func (c *Client) Eval(ctx context.Context, script string, keys []string, args ...any) (any, error) {
	return runScript(ctx, c.rdb, c.resends, "eval", script, keys, args...).Result()
}

func (c *Client) EvalSha(ctx context.Context, sha string, keys []string, args ...any) (any, error) {
	return runScript(ctx, c.rdb, c.resends, "evalsha", sha, keys, args...).Result()
}

// EvalDurable pins a physical connection so WAITAOF observes the replication
// offset produced by the immediately preceding script. Redis Cluster cannot
// safely provide this through go-redis's keyless-command routing, so it is
// rejected instead of silently weakening the durability contract.
func (c *Client) EvalDurable(ctx context.Context, script string, keys []string, numLocal, numReplicas int, timeout time.Duration, args ...any) (any, int64, int64, error) {
	results, local, replicas, err := c.EvalBatchDurable(ctx, script, []fredis.EvalCall{{Keys: keys, Args: args}}, numLocal, numReplicas, timeout)
	if err != nil {
		return nil, 0, 0, err
	}
	if len(results) != 1 {
		return nil, 0, 0, fmt.Errorf("redis: durable eval returned %d results, want 1", len(results))
	}
	return results[0], local, replicas, nil
}

// EvalBatchDurable 把全部脚本与一条 WAITAOF 放在同一条物理连接上流水线发出。整条不经驱动重放；
// 每条命令的错误都证明没执行（例如取连接时拨号失败）时换一条新连接整条重发。
func (c *Client) EvalBatchDurable(ctx context.Context, script string, calls []fredis.EvalCall, numLocal, numReplicas int, timeout time.Duration) ([]any, int64, int64, error) {
	if len(calls) == 0 {
		return nil, 0, 0, nil
	}
	client, ok := c.rdb.(*goredis.Client)
	if !ok {
		return nil, 0, 0, fmt.Errorf("redis: same-connection WAITAOF is unsupported for %T; use a single-primary or Sentinel endpoint", c.rdb)
	}
	for attempt := 0; ; attempt++ {
		commands, waitCommand, execErr := evalBatchOnConn(ctx, client, script, calls, numLocal, numReplicas, timeout)
		if execErr == nil {
			return durableBatchResults(commands, waitCommand)
		}
		sent := append(append([]goredis.Cmder(nil), commandsAsCmders(commands)...), waitCommand)
		if attempt >= c.resends || !allNotExecuted(sent) {
			return nil, 0, 0, execErr
		}
		if waitErr := waitBeforeResend(ctx, attempt); waitErr != nil {
			return nil, 0, 0, errors.Join(execErr, waitErr)
		}
	}
}

// evalBatchOnConn 在一条新的独占连接上发一次脚本批次与 WAITAOF。
func evalBatchOnConn(ctx context.Context, client *goredis.Client, script string, calls []fredis.EvalCall, numLocal, numReplicas int, timeout time.Duration) ([]*goredis.Cmd, *goredis.Cmd, error) {
	conn := client.Conn()
	defer func() { _ = conn.Close() }()
	pipe := conn.Pipeline()
	commands := make([]*goredis.Cmd, 0, len(calls))
	for _, call := range calls {
		// 与 Eval 相同，脚本不经驱动重放（RR-20261005-NC-100）：含 NoRetry 命令的流水线整体不重发。
		command := newScriptCmd(ctx, "eval", script, call.Keys, call.Args...)
		_ = pipe.Process(ctx, noReplay{command})
		commands = append(commands, command)
	}
	waitCommand := goredis.NewCmd(ctx, "WAITAOF", numLocal, numReplicas, timeout.Milliseconds())
	_ = pipe.Process(ctx, noReplay{waitCommand})
	_, err := pipe.Exec(ctx)
	return commands, waitCommand, err
}

func commandsAsCmders(commands []*goredis.Cmd) []goredis.Cmder {
	out := make([]goredis.Cmder, len(commands))
	for i, command := range commands {
		out[i] = command
	}
	return out
}

// durableBatchResults 解析 WAITAOF 回复与每条脚本的结果。
func durableBatchResults(commands []*goredis.Cmd, waitCommand *goredis.Cmd) ([]any, int64, int64, error) {
	reply, err := waitCommand.Slice()
	if err != nil {
		return nil, 0, 0, err
	}
	if len(reply) != 2 {
		return nil, 0, 0, fmt.Errorf("redis: invalid WAITAOF reply length %d", len(reply))
	}
	local, err := redisInteger(reply[0])
	if err != nil {
		return nil, 0, 0, fmt.Errorf("redis: invalid WAITAOF local reply: %w", err)
	}
	replicas, err := redisInteger(reply[1])
	if err != nil {
		return nil, 0, 0, fmt.Errorf("redis: invalid WAITAOF replica reply: %w", err)
	}
	results := make([]any, 0, len(commands))
	for _, command := range commands {
		result, err := command.Result()
		if err != nil {
			return nil, 0, 0, err
		}
		results = append(results, result)
	}
	return results, local, replicas, nil
}

// --- PubSub ---

func (c *Client) Publish(ctx context.Context, channel string, message any) error {
	return write(ctx, c, func(p goredis.Pipeliner) *goredis.IntCmd { return p.Publish(ctx, channel, message) }).Err()
}

func (c *Client) Subscribe(ctx context.Context, channels ...string) fredis.IPubSub {
	return newPubSub(c.rdb.Subscribe(ctx, channels...))
}

// --- Connection ---

func (c *Client) Ping(ctx context.Context) error {
	return c.rdb.Ping(ctx).Err()
}

// Close 关闭连接池，幂等：只有第一次真正关闭，第一次的错误只报给那一次调用，之后（含并发的后到者，
// 它们等第一次做完）都返回 nil。Close 之后的命令返回 goredis.ErrClosed（RR-20261006-10）。
//
// go-redis 单机的 Close 不幂等（第二次返回 ErrClosed），Cluster 的幂等（返回 nil）；旧实现直接透传，
// 重复 Close 的结果随部署形态变化，并发时单机的后到者不等第一个关完就返回。go-redis 遇到错误也会
// 关完全部连接，所以第一次出错之后同样没有可以重试的资源。
func (c *Client) Close() error {
	var err error
	c.closeOnce.Do(func() { err = c.rdb.Close() })
	return err
}

// --- helpers ---

func convertZSlice(zs []goredis.Z) []fredis.Z {
	result := make([]fredis.Z, len(zs))
	for i, z := range zs {
		member := ""
		if s, ok := z.Member.(string); ok {
			member = s
		}
		result[i] = fredis.Z{Score: z.Score, Member: member}
	}
	return result
}

func redisInteger(value any) (int64, error) {
	switch typed := value.(type) {
	case int64:
		return typed, nil
	case int:
		return int64(typed), nil
	case uint64:
		if typed > uint64(^uint64(0)>>1) {
			return 0, fmt.Errorf("integer overflow: %d", typed)
		}
		return int64(typed), nil
	case string:
		return strconv.ParseInt(typed, 10, 64)
	case []byte:
		return strconv.ParseInt(string(typed), 10, 64)
	default:
		return 0, fmt.Errorf("unexpected integer type %T", value)
	}
}

var _ fredis.IRedis = (*Client)(nil)
var _ fredis.DurableEvaler = (*Client)(nil)
var _ fredis.DurableBatchEvaler = (*Client)(nil)
var _ fredis.ListTrimmer = (*Client)(nil)
var _ fredis.ListRemover = (*Client)(nil)
