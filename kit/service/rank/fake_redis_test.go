package rank

import (
	"context"
	"sort"
	"strconv"
	"strings"
	"sync"

	fredis "github.com/tjbdwanghaibo/roost-core/redis"
)

// fakeRedis implements the sorted-set and hash operations rank uses, and
// evaluates the two scripts' semantics rather than pretending to.
//
// A fake that accepted every swap would make the atomicity and idempotency
// tests pass while proving nothing — the failure mode of a rank test suite
// whose fake returned nil from Pipeline, so the production read path never
// ran and the bug that lived there went unnoticed for a release.
//
// Its limit, stated plainly: it reimplements the scripts' semantics in Go
// rather than executing them, so a defect in the Lua text itself is invisible
// here. Mutating a script string does not change this fake's behaviour. That
// surface is covered by redis_integration_test.go against a real Redis; keep
// the scripts small for exactly this reason.
type fakeRedis struct {
	mu sync.Mutex
	// zsets maps key -> members (score is always zero; ordering is the member).
	zsets map[string]map[string]bool
	// hashes maps key -> field -> value.
	hashes map[string]map[string]string
	// swapDelay, when set, runs before a swap commits so a test can interleave
	// two submits deterministically.
	swapDelay func()
	// swapAlwaysLoses makes every swap report a lost compare-and-swap, which
	// is how a test reaches the exhausted-retry path without racing.
	swapAlwaysLoses bool
	evalCalls       int
}

func newFakeRedis() *fakeRedis {
	return &fakeRedis{
		zsets:  make(map[string]map[string]bool),
		hashes: make(map[string]map[string]string),
	}
}

func (f *fakeRedis) sortedMembers(key string) []string {
	members := make([]string, 0, len(f.zsets[key]))
	for member := range f.zsets[key] {
		members = append(members, member)
	}
	// Descending lexicographic: exactly what ZREVRANGE gives for a set whose
	// scores are all equal.
	sort.Sort(sort.Reverse(sort.StringSlice(members)))
	return members
}

func (f *fakeRedis) ZCard(_ context.Context, key string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return int64(len(f.zsets[key])), nil
}

func (f *fakeRedis) ZRevRangeWithScores(_ context.Context, key string, start, stop int64) ([]fredis.Z, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	members := f.sortedMembers(key)
	if start < 0 {
		start = 0
	}
	if start >= int64(len(members)) {
		return nil, nil
	}
	if stop < 0 || stop >= int64(len(members)) {
		stop = int64(len(members)) - 1
	}
	if stop < start {
		return nil, nil
	}
	out := make([]fredis.Z, 0, stop-start+1)
	for _, member := range members[start : stop+1] {
		out = append(out, fredis.Z{Member: member})
	}
	return out, nil
}

func (f *fakeRedis) ZRevRank(_ context.Context, key string, member string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for index, candidate := range f.sortedMembers(key) {
		if candidate == member {
			return int64(index), nil
		}
	}
	return 0, fredis.ErrNil
}

func (f *fakeRedis) Del(_ context.Context, keys ...string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	removed := int64(0)
	for _, key := range keys {
		if _, ok := f.zsets[key]; ok {
			delete(f.zsets, key)
			removed++
		}
		if _, ok := f.hashes[key]; ok {
			delete(f.hashes, key)
			removed++
		}
	}
	return removed, nil
}

func (f *fakeRedis) hget(key, field string) (string, bool) {
	fields, ok := f.hashes[key]
	if !ok {
		return "", false
	}
	value, ok := fields[field]
	return value, ok
}

func (f *fakeRedis) hset(key, field, value string) {
	if f.hashes[key] == nil {
		f.hashes[key] = map[string]string{}
	}
	f.hashes[key][field] = value
}

// Eval evaluates the three scripts rank uses by matching on their content.
func (f *fakeRedis) Eval(_ context.Context, script string, keys []string, args ...any) (any, error) {
	strArgs := make([]string, 0, len(args))
	for _, arg := range args {
		strArgs = append(strArgs, toStr(arg))
	}

	switch {
	case strings.Contains(script, `ARGV[1] .. ":applied"`) && strings.Contains(script, "return {m or"):
		f.mu.Lock()
		defer f.mu.Unlock()
		f.evalCalls++
		member, _ := f.hget(keys[0], strArgs[0])
		ring, _ := f.hget(keys[0], strArgs[0]+":applied")
		return []any{member, ring}, nil

	case strings.Contains(script, "ZADD"):
		// swapScript
		if f.swapDelay != nil {
			f.swapDelay()
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		f.evalCalls++
		zkey, okey := keys[0], keys[1]
		owner, expected, next, requestID, ring := strArgs[0], strArgs[1], strArgs[2], strArgs[3], strArgs[4]
		stored, exists := f.hget(okey, owner)
		if f.swapAlwaysLoses {
			return []any{int64(0), stored}, nil
		}
		if expected == "" {
			if exists {
				return []any{int64(0), stored}, nil
			}
		} else {
			if !exists || stored != expected {
				return []any{int64(0), stored}, nil
			}
			if f.zsets[zkey] != nil {
				delete(f.zsets[zkey], stored)
			}
		}
		if f.zsets[zkey] == nil {
			f.zsets[zkey] = map[string]bool{}
		}
		f.zsets[zkey][next] = true
		f.hset(okey, owner, next)
		if requestID != "" {
			f.hset(okey, owner+":applied", ring)
		}
		return []any{int64(1), next}, nil

	case strings.Contains(script, "HDEL"):
		// removeScript
		f.mu.Lock()
		defer f.mu.Unlock()
		f.evalCalls++
		zkey, okey := keys[0], keys[1]
		owner := strArgs[0]
		stored, exists := f.hget(okey, owner)
		if !exists {
			return int64(0), nil
		}
		if f.zsets[zkey] != nil {
			delete(f.zsets[zkey], stored)
		}
		delete(f.hashes[okey], owner)
		delete(f.hashes[okey], owner+":applied")
		return int64(1), nil
	}
	return nil, fredis.ErrCASInvalidCommand
}

func toStr(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case int64:
		return strconv.FormatInt(typed, 10)
	case int:
		return strconv.Itoa(typed)
	}
	return ""
}
