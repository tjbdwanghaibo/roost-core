package redis

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"time"
)

var ErrCASInvalidCommand = errors.New("redis cas: invalid command")

// ScriptRunner is the one capability CompareAndSet needs. Taking the narrow
// interface lets a caller — or a test double — supply just this rather than a
// whole IRedis; IRedis satisfies it, so existing callers are unaffected.
type ScriptRunner interface {
	Eval(ctx context.Context, script string, keys []string, args ...any) (any, error)
}

type CompareAndSetCommand struct {
	Key      string
	Expected []byte
	Next     []byte
	TTL      time.Duration
	// Index, when set, is maintained in the SAME script as the value: the
	// sorted-set entry moves if and only if the compare-and-set applies.
	//
	// It exists because "the record" and "the record is in the pending set"
	// are one fact, and writing them as two operations leaves a window where
	// a crash produces a record nothing enumerates — a paid order no
	// background loop can ever find. Two keys in one script
	// means a Redis Cluster deployment must keep them in one hash slot; the
	// caller chooses the key layout, so the caller owns that.
	Index *CompareAndSetIndex
}

// CompareAndSetIndex is one sorted-set entry to write or retire alongside the
// value. Score is what the reader pages by — typically when the entry is next
// worth looking at.
type CompareAndSetIndex struct {
	Key    string
	Member string
	Score  float64
	// Remove retires the entry instead of writing it, for the write that
	// makes a record stop being interesting to the index's reader.
	Remove bool
}

type CompareAndSetResult struct {
	Applied bool
	Current []byte
}

const compareAndSetScript = `
local current = redis.call("GET", KEYS[1])
local expect_missing = ARGV[4]
if expect_missing == "1" then
  if current ~= false then
    return {0, current}
  end
else
  if current == false or current ~= ARGV[1] then
    return {0, current}
  end
end
if tonumber(ARGV[3]) > 0 then
  redis.call("PSETEX", KEYS[1], ARGV[3], ARGV[2])
else
  redis.call("SET", KEYS[1], ARGV[2])
end
return {1, ARGV[2]}
`

// compareAndSetIndexedScript is the same compare-and-set with one sorted-set
// entry maintained in the same call. The index is touched ONLY after the
// compare passed, which is the whole point: a write that did not happen must
// not change what the index says is waiting.
//
// ARGV[5] is the member, ARGV[6] the score, ARGV[7] "1" to retire the entry.
const compareAndSetIndexedScript = `
local current = redis.call("GET", KEYS[1])
local expect_missing = ARGV[4]
if expect_missing == "1" then
  if current ~= false then
    return {0, current}
  end
else
  if current == false or current ~= ARGV[1] then
    return {0, current}
  end
end
if tonumber(ARGV[3]) > 0 then
  redis.call("PSETEX", KEYS[1], ARGV[3], ARGV[2])
else
  redis.call("SET", KEYS[1], ARGV[2])
end
if ARGV[7] == "1" then
  redis.call("ZREM", KEYS[2], ARGV[5])
else
  redis.call("ZADD", KEYS[2], ARGV[6], ARGV[5])
end
return {1, ARGV[2]}
`

func CompareAndSet(ctx context.Context, client ScriptRunner, cmd CompareAndSetCommand) (CompareAndSetResult, error) {
	if client == nil || cmd.Key == "" || cmd.Next == nil {
		return CompareAndSetResult{}, ErrCASInvalidCommand
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ttlMillis := int64(0)
	if cmd.TTL > 0 {
		ttlMillis = int64(cmd.TTL / time.Millisecond)
		if ttlMillis <= 0 {
			ttlMillis = 1
		}
	}
	expectMissing := "0"
	if cmd.Expected == nil {
		expectMissing = "1"
	}
	if cmd.Index != nil {
		if cmd.Index.Key == "" || cmd.Index.Member == "" {
			return CompareAndSetResult{}, ErrCASInvalidCommand
		}
		// ZADD rejects NaN only after the script's SET has run, and a script is
		// not rolled back on error: the value would land without its index
		// entry while the caller gets an error. Refuse it
		// before anything is sent.
		if !cmd.Index.Remove && math.IsNaN(cmd.Index.Score) {
			return CompareAndSetResult{}, fmt.Errorf("%w: index score is NaN", ErrCASInvalidCommand)
		}
		remove := "0"
		if cmd.Index.Remove {
			remove = "1"
		}
		// The score goes over as a decimal string rather than as a float:
		// what Lua's tonumber makes of a serialized float is not always what
		// Go wrote, and a score is the number a reader pages by.
		score := strconv.FormatFloat(cmd.Index.Score, 'f', -1, 64)
		ret, err := client.Eval(ctx, compareAndSetIndexedScript,
			[]string{cmd.Key, cmd.Index.Key},
			string(cmd.Expected), string(cmd.Next), ttlMillis, expectMissing,
			cmd.Index.Member, score, remove)
		if err != nil {
			return CompareAndSetResult{}, err
		}
		return parseCompareAndSetResult(ret)
	}
	ret, err := client.Eval(ctx, compareAndSetScript, []string{cmd.Key}, string(cmd.Expected), string(cmd.Next), ttlMillis, expectMissing)
	if err != nil {
		return CompareAndSetResult{}, err
	}
	return parseCompareAndSetResult(ret)
}

func parseCompareAndSetResult(ret any) (CompareAndSetResult, error) {
	items, ok := ret.([]any)
	if !ok {
		if typed, ok := ret.([]interface{}); ok {
			items = typed
		} else {
			return CompareAndSetResult{}, fmt.Errorf("redis cas: unexpected result %T", ret)
		}
	}
	if len(items) != 2 {
		return CompareAndSetResult{}, fmt.Errorf("redis cas: invalid result length %d", len(items))
	}
	applied, err := parseCASApplied(items[0])
	if err != nil {
		return CompareAndSetResult{}, err
	}
	current, err := parseCASBytes(items[1])
	if err != nil {
		return CompareAndSetResult{}, err
	}
	return CompareAndSetResult{Applied: applied, Current: current}, nil
}

func parseCASApplied(raw any) (bool, error) {
	switch v := raw.(type) {
	case int64:
		return v != 0, nil
	case int:
		return v != 0, nil
	case uint64:
		return v != 0, nil
	case string:
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return false, fmt.Errorf("redis cas: invalid applied flag %q", v)
		}
		return n != 0, nil
	default:
		return false, fmt.Errorf("redis cas: invalid applied flag %T", raw)
	}
}

func parseCASBytes(raw any) ([]byte, error) {
	switch v := raw.(type) {
	case nil:
		return nil, nil
	case []byte:
		return append([]byte(nil), v...), nil
	case string:
		return []byte(v), nil
	default:
		return nil, fmt.Errorf("redis cas: invalid current value %T", raw)
	}
}

// compareAndDeleteScript removes a key only while it still holds the value
// the caller last saw. The compare and the delete are one round trip because
// the gap between them is the whole problem: a lease that expires there is
// taken by somebody else, and the delete that follows removes THEIR key.
const compareAndDeleteScript = `
local current = redis.call("GET", KEYS[1])
if current == false or current ~= ARGV[1] then
  return {0, current}
end
redis.call("DEL", KEYS[1])
return {1, current}
`

// CompareAndDelete deletes Key if and only if it currently holds Expected.
//
// It is the other half of CompareAndSet for owner-style keys: CompareAndSet
// covers "extend what is mine" (compare, then write the same value with a new
// TTL), and this covers "give up what is mine". Applied reports whether the
// delete happened; Current is what was there when it did not, so a caller can
// tell "somebody else owns it now" from "it was already gone" (Current nil).
//
// Expected must be non-nil: deleting a key whose value you have not compared
// is just DEL, and calling it through here would suggest a guarantee that is
// not being asked for.
func CompareAndDelete(ctx context.Context, client ScriptRunner, key string, expected []byte) (CompareAndSetResult, error) {
	if client == nil || key == "" || expected == nil {
		return CompareAndSetResult{}, ErrCASInvalidCommand
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ret, err := client.Eval(ctx, compareAndDeleteScript, []string{key}, string(expected))
	if err != nil {
		return CompareAndSetResult{}, err
	}
	return parseCompareAndSetResult(ret)
}

// compareAndSetAllScript 是多键版的 compareAndSetScript：先逐键比较，全部通过才逐键写入，
// 任何一个键不符就一个都不写。每个键占 ARGV 里连续四格：期望值、新值、TTL 毫秒、"1" 表示期望键不存在。
//
// 比较与写入在同一个脚本里，Redis 单线程执行脚本，所以“全有或全无”不靠回滚：写入阶段开始时所有比较
// 都已通过，SET / PSETEX 对字符串值不会半途失败（内存不足时 Redis 在脚本开始前就拒绝可能写入的脚本）。
const compareAndSetAllScript = `
local n = #KEYS
for i = 1, n do
  local at = (i - 1) * 4
  local current = redis.call("GET", KEYS[i])
  if ARGV[at + 4] == "1" then
    if current ~= false then
      return 0
    end
  elseif current == false or current ~= ARGV[at + 1] then
    return 0
  end
end
for i = 1, n do
  local at = (i - 1) * 4
  if tonumber(ARGV[at + 3]) > 0 then
    redis.call("PSETEX", KEYS[i], ARGV[at + 3], ARGV[at + 2])
  else
    redis.call("SET", KEYS[i], ARGV[at + 2])
  end
end
return 1
`

// CompareAndSetAll 对多个键做一次全有或全无的比较并设置：每个键都仍是调用方读到的字节（Expected 为 nil 时
// 要求键不存在）才把全部键写成各自的 Next，返回 true；任何一个键不符返回 false，一个键也不改。
//
// 它服务于“一个事实横跨几个版本化值”的写，例如 service/match 的跨分区成局：几张票分属不同分区的队列状态，
// 成局要么在所有分区同时生效，要么都不生效。
//
// 键不能重复，也不支持 Index（多键写目前没有需要同步维护的索引）。脚本一次带多个键，Redis Cluster 要求它们
// 落在同一个 hash slot（键名用同一个 hash tag），否则服务端回 CROSSSLOT 错误；键的布局由调用方决定，
// 这个约束也由调用方负责。
func CompareAndSetAll(ctx context.Context, client ScriptRunner, cmds []CompareAndSetCommand) (bool, error) {
	if client == nil || len(cmds) == 0 {
		return false, ErrCASInvalidCommand
	}
	if ctx == nil {
		ctx = context.Background()
	}
	keys := make([]string, 0, len(cmds))
	args := make([]any, 0, 4*len(cmds))
	seen := make(map[string]bool, len(cmds))
	for _, cmd := range cmds {
		if cmd.Key == "" || cmd.Next == nil || cmd.Index != nil || seen[cmd.Key] {
			return false, ErrCASInvalidCommand
		}
		seen[cmd.Key] = true
		ttlMillis := int64(0)
		if cmd.TTL > 0 {
			ttlMillis = int64(cmd.TTL / time.Millisecond)
			if ttlMillis <= 0 {
				ttlMillis = 1
			}
		}
		expectMissing := "0"
		if cmd.Expected == nil {
			expectMissing = "1"
		}
		keys = append(keys, cmd.Key)
		args = append(args, string(cmd.Expected), string(cmd.Next), ttlMillis, expectMissing)
	}
	ret, err := client.Eval(ctx, compareAndSetAllScript, keys, args...)
	if err != nil {
		return false, err
	}
	return parseCASApplied(ret)
}
