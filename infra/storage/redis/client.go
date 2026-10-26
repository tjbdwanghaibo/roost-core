package redis

import (
	"context"
	"errors"
	"time"
)

// IRedis is the abstraction for Redis operations.
type IRedis interface {
	// --- String/KV ---
	Get(ctx context.Context, key string) ([]byte, error)
	// MGet reads several keys in ONE round trip.
	//
	// The result is POSITIONAL: len(result) always equals len(keys), and a nil
	// element means that key is absent. It is a slice rather than a map on
	// purpose — a map would omit missing keys, and then a reply that came back
	// short for any other reason would be indistinguishable from "those keys
	// were not there". A caller that can compare lengths can detect a
	// truncated read; one holding a map cannot.
	//
	// Zero keys returns an empty result and makes NO round trip. This is part
	// of the contract because `MGET` with no arguments is an error in Redis,
	// so an implementation that passed the empty case through would fail on
	// an ordinary empty page.
	//
	// Unlike Get, an absent key is not ErrNil: with several keys, absence is a
	// per-element outcome and not a call-level one.
	MGet(ctx context.Context, keys ...string) ([][]byte, error)
	Set(ctx context.Context, key string, value any, expiration time.Duration) error
	SetNX(ctx context.Context, key string, value any, expiration time.Duration) (bool, error)
	Del(ctx context.Context, keys ...string) (int64, error)
	Exists(ctx context.Context, keys ...string) (int64, error)
	Expire(ctx context.Context, key string, expiration time.Duration) (bool, error)
	TTL(ctx context.Context, key string) (time.Duration, error)
	Incr(ctx context.Context, key string) (int64, error)
	IncrBy(ctx context.Context, key string, value int64) (int64, error)

	// --- Hash ---
	HGet(ctx context.Context, key, field string) ([]byte, error)
	HSet(ctx context.Context, key string, values ...any) error
	HGetAll(ctx context.Context, key string) (map[string]string, error)
	HDel(ctx context.Context, key string, fields ...string) (int64, error)
	HExists(ctx context.Context, key, field string) (bool, error)

	// --- List ---
	LPush(ctx context.Context, key string, values ...any) (int64, error)
	RPush(ctx context.Context, key string, values ...any) (int64, error)
	LPop(ctx context.Context, key string) ([]byte, error)
	RPop(ctx context.Context, key string) ([]byte, error)
	LLen(ctx context.Context, key string) (int64, error)
	LRange(ctx context.Context, key string, start, stop int64) ([]string, error)

	// --- Sorted Set ---
	ZAdd(ctx context.Context, key string, members ...Z) (int64, error)
	ZRem(ctx context.Context, key string, members ...any) (int64, error)
	ZScore(ctx context.Context, key string, member string) (float64, error)
	ZRank(ctx context.Context, key string, member string) (int64, error)
	ZRevRank(ctx context.Context, key string, member string) (int64, error)
	ZRangeWithScores(ctx context.Context, key string, start, stop int64) ([]Z, error)
	ZRevRangeWithScores(ctx context.Context, key string, start, stop int64) ([]Z, error)
	ZCard(ctx context.Context, key string) (int64, error)

	// --- Set ---
	SAdd(ctx context.Context, key string, members ...any) (int64, error)
	SRem(ctx context.Context, key string, members ...any) (int64, error)
	SMembers(ctx context.Context, key string) ([]string, error)
	SIsMember(ctx context.Context, key string, member any) (bool, error)

	// --- Pipeline / Script ---
	Pipeline() IPipeline
	Eval(ctx context.Context, script string, keys []string, args ...any) (any, error)
	EvalSha(ctx context.Context, sha string, keys []string, args ...any) (any, error)

	// --- PubSub ---
	Publish(ctx context.Context, channel string, message any) error
	Subscribe(ctx context.Context, channels ...string) IPubSub

	// --- Connection ---
	Ping(ctx context.Context) error
	Close() error
}

type EvalCall struct {
	Keys []string
	Args []any
}

// DurableEvaler executes a script and WAITAOF on the same physical Redis
// connection. Independent pooled calls do not satisfy this contract.
type DurableEvaler interface {
	EvalDurable(ctx context.Context, script string, keys []string, numLocal, numReplicas int, timeout time.Duration, args ...any) (result any, local, replicas int64, err error)
}

// DurableBatchEvaler pipelines all scripts and one trailing WAITAOF on one
// physical connection, amortizing the fsync across the admitted batch.
type DurableBatchEvaler interface {
	EvalBatchDurable(ctx context.Context, script string, calls []EvalCall, numLocal, numReplicas int, timeout time.Duration) (results []any, local, replicas int64, err error)
}

// ReplicatedEvaler 执行一个脚本，然后在同一条物理连接上对执行它的主节点发 WAIT。WAIT 只统计
// 本连接之前的写，所以必须与脚本同连接；独立的池化调用不满足这个约定。Cluster 下 WAIT 只发往脚本第一个键
// 所在槽位的主节点。契约（重放、结果分类）见 redis/driver/README.md。
//
// 返回的 error 只属于脚本（与 Eval 的分类相同）；脚本成功之后 WAIT 的情况都在结果里：副本确认数不足
// 不是错误，ROLE / WAIT 出错时脚本已执行、副本是否收到未知（WaitErr）。
type ReplicatedEvaler interface {
	EvalReplicated(ctx context.Context, script string, keys []string, numReplicas int, timeout time.Duration, args ...any) (ReplicatedEvalResult, error)
}

// ReplicatedEvalResult 是 EvalReplicated 在脚本成功时的结果。
type ReplicatedEvalResult struct {
	// Result 是脚本的回复。
	Result any
	// Waited 报告是否发了 WAIT；Replicas 是 WAIT 回复的副本确认数（没发时为 0）。
	Waited   bool
	Replicas int64
	// Skipped 是没有发 WAIT 的原因（ReplicatedSkip*）；Waited 为 true 或 WaitErr 非空时为空。
	Skipped string
	// WaitErr 是 ROLE / WAIT 的错误：脚本已执行，副本是否收到未知；不重放。
	WaitErr error
}

const (
	// ReplicatedSkipNoReplicas：主节点当前没有连着的副本（例如单机开发环境），不等。
	ReplicatedSkipNoReplicas = "no_replicas"
	// ReplicatedSkipRedirected：Cluster 拓扑刚变（MOVED / ASK 或取不到槽位主节点），脚本改经集群客户端
	// 普通发送（跟随重定向），没有同连接可等。
	ReplicatedSkipRedirected = "redirected"
	// ReplicatedSkipUnsupported：客户端类型不支持同连接 WAIT，脚本普通发送。
	ReplicatedSkipUnsupported = "unsupported"
	// ReplicatedSkipDisabled：numReplicas 或 timeout 不为正，没有要等的。
	ReplicatedSkipDisabled = "disabled"
)

// ScriptCall 是 DurableScriptBatch 里的一次脚本调用：SHA 是 SCRIPT LOAD 得到的摘要，Source 是脚本原文
// （服务端回 NOSCRIPT 时用它在同一连接上以 EVAL 重发）。
type ScriptCall struct {
	SHA    string
	Source string
	Keys   []string
	Args   []any
}

// ScriptResult 是一次脚本调用的结果：Value 是脚本回复（Lua 的 nil / false 为 nil），Err 是服务端对这一条的错误回复。
// 某一条的 Err 不影响同批其他调用的结果。
type ScriptResult struct {
	Value any
	Err   error
}

// DurableScriptBatcher 在同一条物理连接上流水线发出一批 EVALSHA 与一条 WAITAOF：一次往返、服务端一次 fsync 摊给整批。
//
// 结果分类（驱动契约见 redis/driver/README.md）：
//   - 返回 err == nil：每条调用的结果在 results 里（按调用顺序）；local / replicas 是 WAITAOF 的确认数，覆盖本批全部写。
//     某条回 NOSCRIPT（服务端重启后脚本缓存清空，属于“确定没执行”）时，驱动已在同一连接上用 EVAL 重发这些调用并补发 WAITAOF，
//     调用方看到的是重发后的结果。
//   - err 满足 errors.Is(err, ErrWaitAOFFailed)：全部脚本已执行、results 有效，但服务端拒绝了 WAITAOF（例如 appendonly 被关）；
//     按“已执行、持久未确认”处理，与 local 不足相同。
//   - 其余 err：整批结果未知（连接中断、超时），不重放；只有每条命令的错误都证明没执行时驱动才换连接整批重发。
//
// 写脚本的调用方必须能在结果未知后安全重试（请求摘要、版本 CAS）。Cluster 客户端一律拒绝。
type DurableScriptBatcher interface {
	DurableScriptBatch(ctx context.Context, calls []ScriptCall, numLocal, numReplicas int, timeout time.Duration) (results []ScriptResult, local, replicas int64, err error)
}

// ErrWaitAOFFailed：DurableScriptBatch 的脚本都已执行，WAITAOF 本身被服务端拒绝（appendonly 关闭等），持久未确认。
var ErrWaitAOFFailed = errors.New("redis: WAITAOF rejected; scripts executed, durability unconfirmed")

// StreamEntry 是流里的一条记录。
type StreamEntry struct {
	ID     string
	Fields map[string]string
}

// StreamReader 读流（XRANGE / XREAD / XLEN），都是读命令，按驱动的读重试规则处理。
type StreamReader interface {
	// XRangeAfter 返回 ID 严格大于 afterID 的至多 count 条（XRANGE key (afterID + COUNT count）。
	XRangeAfter(ctx context.Context, key, afterID string, count int64) ([]StreamEntry, error)
	// XReadAfter 阻塞至多 block 等 afterID 之后的新条目（XREAD BLOCK）；超时返回空切片、nil 错误。
	XReadAfter(ctx context.Context, key, afterID string, count int64, block time.Duration) ([]StreamEntry, error)
	XLen(ctx context.Context, key string) (int64, error)
}

// HashFieldsReader 一次往返读一个哈希的多个字段（HMGET）。结果按 fields 的位置给出，nil 元素表示字段不存在；
// 键不存在时全部为 nil。
type HashFieldsReader interface {
	HMGet(ctx context.Context, key string, fields ...string) ([][]byte, error)
}

// ScriptLoader 把脚本装进服务端缓存，返回 SHA1。
type ScriptLoader interface {
	ScriptLoad(ctx context.Context, source string) (string, error)
}

// ServerInspector 读服务端状态与配置，供启动探针核对部署契约。托管 Redis 可能禁用 CONFIG，调用方要能处理它的错误。
type ServerInspector interface {
	Info(ctx context.Context, section string) (string, error)
	ConfigGet(ctx context.Context, parameter string) (map[string]string, error)
}

// ListTrimmer trims a list in place (LTRIM). Optional capability: callers
// that would otherwise emulate a trim with DEL+RPUSH (a window in which a
// crash loses the whole list) should prefer this when the client provides it.
type ListTrimmer interface {
	LTrim(ctx context.Context, key string, start, stop int64) error
}

// ListRemover removes occurrences of a value from a list in place (LREM).
// Optional capability with the same motivation as ListTrimmer.
type ListRemover interface {
	LRem(ctx context.Context, key string, count int64, value any) (int64, error)
}

// Z represents a sorted set member with score.
type Z struct {
	Score  float64
	Member string
}
