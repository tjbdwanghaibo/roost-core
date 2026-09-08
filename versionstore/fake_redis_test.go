package versionstore

import (
	"context"
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

	// failEveryCAS makes every compare-and-set lose, as if another writer
	// always won the race, so the retry budget can be exercised.
	failEveryCAS bool
	casCalls     int
}

func newFakeRedis() *fakeRedis {
	return &fakeRedis{values: make(map[string][]byte)}
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
func (f *fakeRedis) Eval(_ context.Context, _ string, keys []string, args ...any) (any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.casCalls++
	if len(keys) != 1 || len(args) < 4 {
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
	return []any{int64(1), next}, nil
}
