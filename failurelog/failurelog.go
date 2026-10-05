package failurelog

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/tjbdwanghaibo/roost-core/metrics"
	fredis "github.com/tjbdwanghaibo/roost-core/redis"
)

const (
	defaultMaxEntries int64 = 10000
	appendTrimScript        = `
local len = redis.call("RPUSH", KEYS[1], ARGV[1])
local max = tonumber(ARGV[2])
if max and max > 0 then
	redis.call("LTRIM", KEYS[1], -max, -1)
end
local ttl = tonumber(ARGV[3])
if ttl and ttl > 0 then
	redis.call("PEXPIRE", KEYS[1], ttl)
end
return len
	`
	deleteRawScript = `
local key = KEYS[1]
local items = redis.call("LRANGE", key, 0, -1)
if #items == 0 then
	return 0
end
local ttl = redis.call("PTTL", key)
local remove = {}
for i = 1, #ARGV do
	local raw = ARGV[i]
	remove[raw] = (remove[raw] or 0) + 1
end
local kept = {}
local removed = 0
for i = 1, #items do
	local raw = items[i]
	local count = remove[raw]
	if count and count > 0 then
		remove[raw] = count - 1
		removed = removed + 1
	else
		kept[#kept + 1] = raw
	end
end
if removed > 0 then
	redis.call("DEL", key)
	if #kept > 0 then
		redis.call("RPUSH", key, unpack(kept))
		if ttl > 0 then
			redis.call("PEXPIRE", key, ttl)
		end
	end
end
return removed
	`
	purgeRawScript = `
local key = KEYS[1]
local count = redis.call("LLEN", key)
if count > 0 then
	redis.call("DEL", key)
end
return count
	`
)

var ErrKeyEmpty = errors.New("failurelog: key is empty")

type Config struct {
	Namespace  string
	TTL        time.Duration
	MaxEntries int64
}

func (c Config) normalize() Config {
	if c.MaxEntries == 0 {
		c.MaxEntries = defaultMaxEntries
	}
	return c
}

type RedisList struct {
	redis fredis.IRedis
	cfg   Config
}

func NewRedisList(redis fredis.IRedis, cfg Config) *RedisList {
	return &RedisList{redis: redis, cfg: cfg.normalize()}
}

func (l *RedisList) AppendRaw(ctx context.Context, key string, raw []byte) error {
	if l == nil || l.redis == nil {
		return nil
	}
	if key == "" {
		return ErrKeyEmpty
	}
	if ctx == nil {
		ctx = context.Background()
	}
	handled, err := l.tryAppendWithScript(ctx, key, raw)
	if err != nil {
		metrics.IncCounter("failurelog_append_total", l.labels("error"), 1)
		return err
	}
	if handled {
		metrics.IncCounter("failurelog_append_total", l.labels("ok"), 1)
		return nil
	}
	if _, err := l.redis.RPush(ctx, key, raw); err != nil {
		metrics.IncCounter("failurelog_append_total", l.labels("error"), 1)
		return err
	}
	if err := l.trim(ctx, key); err != nil {
		metrics.IncCounter("failurelog_append_total", l.labels("error"), 1)
		return err
	}
	if l.cfg.TTL > 0 {
		_, _ = l.redis.Expire(ctx, key, l.cfg.TTL)
	}
	metrics.IncCounter("failurelog_append_total", l.labels("ok"), 1)
	return nil
}

func (l *RedisList) ListRaw(ctx context.Context, key string, start, stop int64) ([]string, error) {
	if l == nil || l.redis == nil {
		return nil, nil
	}
	if key == "" {
		return nil, ErrKeyEmpty
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return l.redis.LRange(ctx, key, start, stop)
}

func (l *RedisList) Purge(ctx context.Context, key string) (int64, error) {
	if l == nil || l.redis == nil {
		return 0, nil
	}
	if key == "" {
		return 0, ErrKeyEmpty
	}
	if ctx == nil {
		ctx = context.Background()
	}
	n, err := l.purgeRaw(ctx, key)
	if err == nil {
		metrics.IncCounter("failurelog_purge_total", l.labels("ok"), n)
	}
	if err != nil {
		metrics.IncCounter("failurelog_purge_total", l.labels("error"), 1)
	}
	return n, err
}

func (l *RedisList) CountRaw(ctx context.Context, key string) (int64, error) {
	if l == nil || l.redis == nil {
		return 0, nil
	}
	if key == "" {
		return 0, ErrKeyEmpty
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return l.redis.LLen(ctx, key)
}

func (l *RedisList) DeleteRaw(ctx context.Context, key string, raws [][]byte) (int64, error) {
	if l == nil || l.redis == nil || len(raws) == 0 {
		return 0, nil
	}
	if key == "" {
		return 0, ErrKeyEmpty
	}
	if ctx == nil {
		ctx = context.Background()
	}
	args := make([]any, 0, len(raws))
	for _, raw := range raws {
		if len(raw) == 0 {
			continue
		}
		args = append(args, string(raw))
	}
	if len(args) == 0 {
		return 0, nil
	}
	n, handled, err := l.tryDeleteWithScript(ctx, key, args)
	if err != nil {
		metrics.IncCounter("failurelog_delete_total", l.labels("error"), 1)
		return 0, err
	}
	if handled {
		metrics.IncCounter("failurelog_delete_total", l.labels("ok"), n)
		return n, nil
	}
	n, err = l.deleteRawFallback(ctx, key, args)
	if err != nil {
		metrics.IncCounter("failurelog_delete_total", l.labels("error"), 1)
		return 0, err
	}
	metrics.IncCounter("failurelog_delete_total", l.labels("ok"), n)
	return n, nil
}

func (l *RedisList) labels(result string) metrics.Labels {
	labels := metrics.Labels{}
	if l != nil && l.cfg.Namespace != "" {
		labels["namespace"] = l.cfg.Namespace
	}
	if result != "" {
		labels["result"] = result
	}
	if len(labels) == 0 {
		return nil
	}
	return labels
}

// 原子脚本与非原子降级（RR-20261005-NC-160）。
//
// 每个写操作先发一条 Lua 脚本。降级路径（RPUSH+LTRIM、LREM、LLEN+DEL）只为“适配器没有 Lua”
// 准备：替身的 Eval 返回 (nil, nil)，表示脚本根本没有执行。生产驱动的 Eval 不会这样返回
// （脚本回复为空时驱动报 redis.Nil 错误）。
//
// Eval 返回错误时不再降级：连接断开、读超时、ctx 到期都可能发生在服务端已经执行完脚本之后，
// 再走降级就是第二次执行——追加两次、多删同值记录、把清空之后新到的死信也删掉。结果未知原样交给
// 调用方（bus 的 DeadLetter 会返回错误，由消息重投决定下一步），与 cache RefHMap（NC-21）和
// 驱动脚本不重放（NC-100）同一契约。脚本执行了但返回值解析不了，同样按“已执行、结果未知”报错。

// errUnexpectedScriptResult 表示脚本已执行，但返回值不是约定的整数。
var errUnexpectedScriptResult = errors.New("failurelog: unexpected script result")

// degraded 记录一次“适配器没有 Lua、走了非原子降级”。降级仍保留（可用性优先），但计数可见，
// 沿用 failurelog_degraded_total。
func (l *RedisList) degraded(op string) {
	labels := l.labels("")
	if labels == nil {
		labels = metrics.Labels{}
	}
	labels["op"] = op
	metrics.IncCounter("failurelog_degraded_total", labels, 1)
}

// scriptError 给脚本错误补上操作与命名空间，保留 errors.Is。
func (l *RedisList) scriptError(op string, err error) error {
	slog.Warn("failurelog: script result unknown, not falling back to non-atomic commands",
		"op", op, "namespace", l.cfg.Namespace, "err", err)
	return fmt.Errorf("failurelog: %s script: %w", op, err)
}

// tryAppendWithScript 返回 handled=false 且 err=nil 表示适配器没有 Lua，调用方走降级。
func (l *RedisList) tryAppendWithScript(ctx context.Context, key string, raw []byte) (bool, error) {
	if l == nil || l.redis == nil {
		return false, nil
	}
	ttlMillis := int64(0)
	if l.cfg.TTL > 0 {
		ttlMillis = l.cfg.TTL.Milliseconds()
		if ttlMillis <= 0 {
			ttlMillis = 1
		}
	}
	ret, err := l.redis.Eval(ctx, appendTrimScript, []string{key}, string(raw), l.cfg.MaxEntries, ttlMillis)
	if err != nil {
		return false, l.scriptError("append", err)
	}
	if ret == nil {
		l.degraded("append")
		return false, nil
	}
	return true, nil
}

func (l *RedisList) tryDeleteWithScript(ctx context.Context, key string, args []any) (int64, bool, error) {
	if l == nil || l.redis == nil {
		return 0, false, nil
	}
	ret, err := l.redis.Eval(ctx, deleteRawScript, []string{key}, args...)
	if err != nil {
		return 0, false, l.scriptError("delete", err)
	}
	if ret == nil {
		l.degraded("delete")
		return 0, false, nil
	}
	n, err := redisInt64(ret)
	if err != nil {
		return 0, false, l.scriptError("delete", fmt.Errorf("%w: %v", errUnexpectedScriptResult, err))
	}
	return n, true, nil
}

func (l *RedisList) purgeRaw(ctx context.Context, key string) (int64, error) {
	n, handled, err := l.tryPurgeWithScript(ctx, key)
	if err != nil {
		return 0, err
	}
	if handled {
		return n, nil
	}
	count, err := l.redis.LLen(ctx, key)
	if err != nil {
		return 0, err
	}
	if count == 0 {
		return 0, nil
	}
	if _, err := l.redis.Del(ctx, key); err != nil {
		return 0, err
	}
	return count, nil
}

func (l *RedisList) tryPurgeWithScript(ctx context.Context, key string) (int64, bool, error) {
	if l == nil || l.redis == nil {
		return 0, false, nil
	}
	ret, err := l.redis.Eval(ctx, purgeRawScript, []string{key})
	if err != nil {
		return 0, false, l.scriptError("purge", err)
	}
	if ret == nil {
		l.degraded("purge")
		return 0, false, nil
	}
	n, err := redisInt64(ret)
	if err != nil {
		return 0, false, l.scriptError("purge", fmt.Errorf("%w: %v", errUnexpectedScriptResult, err))
	}
	return n, true, nil
}

func (l *RedisList) deleteRawFallback(ctx context.Context, key string, args []any) (int64, error) {
	// LREM removes in place; only clients without it take the legacy
	// LRANGE+DEL+RPUSH path, whose DEL..RPUSH window can lose the whole
	// list on a crash.
	if remover, ok := l.redis.(fredis.ListRemover); ok {
		var removed int64
		counts := make(map[string]int64, len(args))
		order := make([]string, 0, len(args))
		for _, arg := range args {
			value := fmt.Sprint(arg)
			if counts[value] == 0 {
				order = append(order, value)
			}
			counts[value]++
		}
		for _, value := range order {
			n, err := remover.LRem(ctx, key, counts[value], value)
			if err != nil {
				return removed, err
			}
			removed += n
		}
		return removed, nil
	}
	items, err := l.redis.LRange(ctx, key, 0, -1)
	if err != nil {
		return 0, err
	}
	if len(items) == 0 {
		return 0, nil
	}
	ttl, _ := l.redis.TTL(ctx, key)
	remove := make(map[string]int, len(args))
	for _, arg := range args {
		remove[fmt.Sprint(arg)]++
	}
	kept := make([]any, 0, len(items))
	var removed int64
	for _, item := range items {
		if count := remove[item]; count > 0 {
			remove[item] = count - 1
			removed++
			continue
		}
		kept = append(kept, item)
	}
	if removed == 0 {
		return 0, nil
	}
	if _, err := l.redis.Del(ctx, key); err != nil {
		return 0, err
	}
	if len(kept) > 0 {
		if _, err := l.redis.RPush(ctx, key, kept...); err != nil {
			return 0, err
		}
		if ttl > 0 {
			_, _ = l.redis.Expire(ctx, key, ttl)
		}
	}
	return removed, nil
}

func (l *RedisList) trim(ctx context.Context, key string) error {
	if l == nil || l.redis == nil || l.cfg.MaxEntries <= 0 {
		return nil
	}
	count, err := l.redis.LLen(ctx, key)
	if err != nil {
		return err
	}
	if count <= l.cfg.MaxEntries {
		return nil
	}
	metrics.IncCounter("failurelog_trim_total", l.labels(""), 1)
	if trimmer, ok := l.redis.(fredis.ListTrimmer); ok {
		return trimmer.LTrim(ctx, key, -l.cfg.MaxEntries, -1)
	}
	items, err := l.redis.LRange(ctx, key, count-l.cfg.MaxEntries, -1)
	if err != nil {
		return err
	}
	if _, err := l.redis.Del(ctx, key); err != nil {
		return err
	}
	if len(items) > 0 {
		values := make([]any, 0, len(items))
		for _, item := range items {
			values = append(values, item)
		}
		if _, err := l.redis.RPush(ctx, key, values...); err != nil {
			return err
		}
	}
	return nil
}

func redisInt64(v any) (int64, error) {
	switch typed := v.(type) {
	case int64:
		return typed, nil
	case int:
		return int64(typed), nil
	case int32:
		return int64(typed), nil
	case uint64:
		return int64(typed), nil
	case string:
		return strconv.ParseInt(typed, 10, 64)
	case []byte:
		return strconv.ParseInt(string(typed), 10, 64)
	default:
		return 0, fmt.Errorf("failurelog: unexpected redis integer %T", v)
	}
}
