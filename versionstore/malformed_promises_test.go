package versionstore

import (
	"context"
	"errors"
	"testing"

	fredis "github.com/tjbdwanghaibo/roost-core/redis"
)

// RR-20260920-05 的使能件：一条读不回来的记录，必须和"基础设施出问题了"区分开。
//
// 两者要的反应正相反：超时值得立刻重试，而字节解不开的记录会永远以同样的方式失败。
// 分不开的重试循环只有两个结局——要么对瞬时错误放弃，要么让一条坏记录永久占住队头。
func TestAnUnreadableRecordIsReportedAsMalformed(t *testing.T) {
	for name, stored := range map[string]string{
		"no version separator": "not-an-envelope",
		"version not a number": "abc\n{}",
		"version is zero":      "0\n{}",
		"payload does not decode": "1\n{not json",
	} {
		t.Run(name, func(t *testing.T) {
			store := newMalformedTestStore(t)
			store.client.values["bad"] = []byte(stored)
			_, _, err := store.store.Get(context.Background(), "bad")
			if !errors.Is(err, ErrMalformedRecord) {
				t.Fatalf("an unreadable record reported %v, want ErrMalformedRecord", err)
			}
		})
	}
}

// 反面：基础设施错误**不能**被归成 malformed，否则调用方会把一次超时当成永久损坏。
func TestATransportFailureIsNotMalformed(t *testing.T) {
	store := newMalformedTestStore(t)
	store.client.fail = errors.New("connection reset")
	_, _, err := store.store.Get(context.Background(), "anything")
	if err == nil {
		t.Fatal("a failing transport returned no error")
	}
	if errors.Is(err, ErrMalformedRecord) {
		t.Fatalf("a transport failure was classified as an unreadable record: %v", err)
	}
}

type malformedTestStore struct {
	client *malformedFakeRedis
	store  *RedisStore[string, map[string]any]
}

func newMalformedTestStore(t *testing.T) malformedTestStore {
	t.Helper()
	client := &malformedFakeRedis{values: map[string][]byte{}}
	store, err := NewRedisStore(client, RedisConfig[string, map[string]any]{
		Prefix: "orders",
		KeyOf:  func(k string) string { return k },
		Codec:  JSONCodec[map[string]any]{},
	})
	if err != nil {
		t.Fatal(err)
	}
	return malformedTestStore{client: client, store: store}
}

type malformedFakeRedis struct {
	values map[string][]byte
	fail   error
}

func (f *malformedFakeRedis) Get(_ context.Context, key string) ([]byte, error) {
	if f.fail != nil {
		return nil, f.fail
	}
	// Keyed by nothing on purpose: this fake answers for whatever key the
	// store renders, so the test is about decoding rather than key format.
	if len(f.values) == 0 {
		return nil, fredis.ErrNil
	}
	for _, value := range f.values {
		return value, nil
	}
	return nil, fredis.ErrNil
}

func (f *malformedFakeRedis) Del(_ context.Context, keys ...string) (int64, error) {
	if f.fail != nil {
		return 0, f.fail
	}
	var removed int64
	for _, key := range keys {
		if _, ok := f.values[key]; ok {
			delete(f.values, key)
			removed++
		}
	}
	return removed, nil
}

func (f *malformedFakeRedis) Eval(_ context.Context, _ string, _ []string, _ ...any) (any, error) {
	if f.fail != nil {
		return nil, f.fail
	}
	return []any{int64(1), ""}, nil
}

