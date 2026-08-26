package failurelog

import (
	"context"
	"testing"

	"github.com/tjbdwanghaibo/cube-core/obs"
)

// inPlaceFakeRedis adds the optional in-place list capabilities on top of
// the base fake, and counts Del calls so tests can assert the loss-window
// path (DEL + RPUSH) is no longer taken.
type inPlaceFakeRedis struct {
	*fakeRedis
	delCalls int
}

func (r *inPlaceFakeRedis) Del(ctx context.Context, keys ...string) (int64, error) {
	r.delCalls++
	return r.fakeRedis.Del(ctx, keys...)
}

func (r *inPlaceFakeRedis) LTrim(_ context.Context, key string, start, stop int64) error {
	items := r.lists[key]
	length := int64(len(items))
	if start < 0 {
		start += length
	}
	if stop < 0 {
		stop += length
	}
	if start < 0 {
		start = 0
	}
	if start > stop || start >= length {
		delete(r.lists, key)
		return nil
	}
	if stop >= length {
		stop = length - 1
	}
	r.lists[key] = append([]string(nil), items[start:stop+1]...)
	return nil
}

func (r *inPlaceFakeRedis) LRem(_ context.Context, key string, count int64, value any) (int64, error) {
	target, _ := value.(string)
	items := r.lists[key]
	kept := items[:0]
	var removed int64
	for _, item := range items {
		if item == target && (count <= 0 || removed < count) {
			removed++
			continue
		}
		kept = append(kept, item)
	}
	r.lists[key] = kept
	return removed, nil
}

func TestTrimUsesInPlaceLTrimWithoutDelete(t *testing.T) {
	redis := &inPlaceFakeRedis{fakeRedis: newFakeRedis()}
	log := NewRedisList(redis, Config{MaxEntries: 2})
	for _, item := range []string{"a", "b", "c", "d"} {
		if err := log.AppendRaw(context.Background(), "failure:{x}", []byte(item)); err != nil {
			t.Fatalf("AppendRaw %q: %v", item, err)
		}
	}
	if redis.delCalls != 0 {
		t.Fatalf("trim used DEL %d times; in-place LTRIM expected", redis.delCalls)
	}
	got, err := log.ListRaw(context.Background(), "failure:{x}", 0, -1)
	if err != nil || len(got) != 2 || got[0] != "c" || got[1] != "d" {
		t.Fatalf("items = %v err=%v, want newest two", got, err)
	}
}

func TestDeleteRawUsesInPlaceLRemWithoutDelete(t *testing.T) {
	redis := &inPlaceFakeRedis{fakeRedis: newFakeRedis()}
	log := NewRedisList(redis, Config{MaxEntries: 10})
	for _, item := range []string{"a", "dup", "b", "dup"} {
		if err := log.AppendRaw(context.Background(), "failure:{x}", []byte(item)); err != nil {
			t.Fatalf("AppendRaw: %v", err)
		}
	}
	// Multiset semantics: one copy removed leaves the other in place.
	n, err := log.DeleteRaw(context.Background(), "failure:{x}", [][]byte{[]byte("dup")})
	if err != nil || n != 1 {
		t.Fatalf("DeleteRaw = %d err=%v, want 1", n, err)
	}
	if redis.delCalls != 0 {
		t.Fatalf("delete used DEL %d times; in-place LREM expected", redis.delCalls)
	}
	got, _ := log.ListRaw(context.Background(), "failure:{x}", 0, -1)
	if len(got) != 3 || got[0] != "a" || got[1] != "b" || got[2] != "dup" {
		t.Fatalf("items = %v", got)
	}
}

func TestScriptFallbackIsCountedAsDegraded(t *testing.T) {
	obs.DefaultRegistry().Reset()
	redis := newFakeRedis() // Eval returns (nil, nil): the script path never engages
	log := NewRedisList(redis, Config{Namespace: "combat", MaxEntries: 10})
	if err := log.AppendRaw(context.Background(), "failure:{x}", []byte("one")); err != nil {
		t.Fatalf("AppendRaw: %v", err)
	}
	for _, metric := range obs.Snapshot() {
		if metric.Name == "failurelog_degraded_total" &&
			metric.Labels["namespace"] == "combat" &&
			metric.Labels["op"] == "append" &&
			metric.Value == 1 {
			return
		}
	}
	t.Fatalf("degraded metric missing: %+v", obs.Snapshot())
}
