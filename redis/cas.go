package redis

import (
	"context"
	"errors"
	"fmt"
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
	// background loop can ever find (RR-20260919-04). Two keys in one script
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
