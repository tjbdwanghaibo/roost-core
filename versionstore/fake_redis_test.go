package versionstore

import (
	"context"
	"sort"
	"strconv"
	"strings"
	"sync"

	fredis "github.com/tjbdwanghaibo/roost-core/redis"
)

// fakeRedis implements the slice of IRedis that versionstore uses, and — the
// part that matters — actually evaluates the compare-and-set semantics under a
// mutex instead of pretending to.
//
// A fake that accepts every compare-and-set would make the concurrency tests
// pass while proving nothing; that is precisely how a service shipped a
// "uses atomic script" test that only asserted the script had been called.
type fakeRedis struct {
	mu     sync.Mutex
	values map[string][]byte
	// index mirrors the sorted set the indexed script maintains. It is a map
	// rather than a real zset because the only thing the store asks of it is
	// "the lowest scores at or below X"; the SEMANTICS of the Lua — that the
	// entry moves only when the compare passed — are asserted against a real
	// Redis in roost-core/redis's integration tests, because a Go fake of a
	// Lua script is exactly the kind of double that has hidden cross-language
	// defects here before.
	index map[string]map[string]float64

	// failEveryCAS makes every compare-and-set lose, as if another writer
	// always won the race, so the retry budget can be exercised.
	failEveryCAS bool
	casCalls     int
}

func newFakeRedis() *fakeRedis {
	return &fakeRedis{values: make(map[string][]byte), index: make(map[string]map[string]float64)}
}

func (f *fakeRedis) Get(_ context.Context, key string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	value, ok := f.values[key]
	if !ok {
		return nil, fredis.ErrNil
	}
	return append([]byte(nil), value...), nil
}

func (f *fakeRedis) Del(_ context.Context, keys ...string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	removed := int64(0)
	for _, key := range keys {
		if _, ok := f.values[key]; ok {
			delete(f.values, key)
			removed++
		}
	}
	return removed, nil
}

// Eval implements the one script versionstore relies on, with the same
// semantics as roost-core/redis's compareAndSetScript: ARGV[1] expected,
// ARGV[2] next, ARGV[3] ttl millis, ARGV[4] "1" when absence is expected.
func (f *fakeRedis) Eval(_ context.Context, script string, keys []string, args ...any) (any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if strings.Contains(script, "ZRANGEBYSCORE") {
		return f.indexDue(keys, args)
	}
	f.casCalls++
	if len(keys) == 0 || len(args) < 4 {
		return nil, fredis.ErrCASInvalidCommand
	}
	key := keys[0]
	expected, _ := args[0].(string)
	next, _ := args[1].(string)
	expectMissing, _ := args[3].(string)

	current, exists := f.values[key]
	if f.failEveryCAS {
		// Report a loss and hand back something that still decodes, the way a
		// real losing compare-and-set returns the winner's value.
		return []any{int64(0), string(current)}, nil
	}
	if expectMissing == "1" {
		if exists {
			return []any{int64(0), string(current)}, nil
		}
	} else if !exists || string(current) != expected {
		return []any{int64(0), string(current)}, nil
	}
	f.values[key] = []byte(next)
	// The index moves only now, after the compare passed — the property the
	// whole feature exists for.
	if len(keys) > 1 && len(args) >= 7 {
		member, _ := args[4].(string)
		scoreText, _ := args[5].(string)
		remove, _ := args[6].(string)
		entries := f.index[keys[1]]
		if entries == nil {
			entries = make(map[string]float64)
			f.index[keys[1]] = entries
		}
		if remove == "1" {
			delete(entries, member)
		} else {
			score, err := strconv.ParseFloat(scoreText, 64)
			if err != nil {
				return nil, err
			}
			entries[member] = score
		}
	}
	return []any{int64(1), next}, nil
}

// indexDue answers the index read: members at or below the score, lowest
// first, capped at the limit.
func (f *fakeRedis) indexDue(keys []string, args []any) (any, error) {
	if len(keys) != 1 || len(args) < 2 {
		return nil, fredis.ErrCASInvalidCommand
	}
	maxScore, err := strconv.ParseFloat(args[0].(string), 64)
	if err != nil {
		return nil, err
	}
	limit, err := strconv.Atoi(args[1].(string))
	if err != nil {
		return nil, err
	}
	type entry struct {
		member string
		score  float64
	}
	var due []entry
	for member, score := range f.index[keys[0]] {
		if score <= maxScore {
			due = append(due, entry{member: member, score: score})
		}
	}
	sort.Slice(due, func(i, j int) bool {
		if due[i].score != due[j].score {
			return due[i].score < due[j].score
		}
		return due[i].member < due[j].member
	})
	out := make([]any, 0, len(due))
	for i, item := range due {
		if i >= limit {
			break
		}
		out = append(out, item.member)
	}
	return out, nil
}
