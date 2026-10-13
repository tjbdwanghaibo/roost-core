// 失败日志列表：追加保留最新、列举与计数、原地裁剪与删除、指标、空 key 拒绝、结果未知不降级重放。

package failurelog

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
	fredis "github.com/tjbdwanghaibo/roost-core/infra/storage/redis"
)

func TestRedisListAppendRawKeepsNewestEntries(t *testing.T) {
	redis := newFakeRedis()
	log := NewRedisList(redis, Config{MaxEntries: 2, TTL: time.Hour})

	for _, item := range []string{"old", "middle", "new"} {
		if err := log.AppendRaw(context.Background(), "failure:{x}", []byte(item)); err != nil {
			t.Fatalf("AppendRaw %q: %v", item, err)
		}
	}

	got, err := log.ListRaw(context.Background(), "failure:{x}", 0, -1)
	if err != nil {
		t.Fatalf("ListRaw: %v", err)
	}
	if len(got) != 2 || got[0] != "middle" || got[1] != "new" {
		t.Fatalf("items = %+v, want newest two", got)
	}
	if redis.expire["failure:{x}"] != time.Hour {
		t.Fatalf("ttl = %v, want %v", redis.expire["failure:{x}"], time.Hour)
	}
}

func TestRedisListListRawSupportsSingleElementRange(t *testing.T) {
	redis := newFakeRedis()
	log := NewRedisList(redis, Config{MaxEntries: 10})
	for _, item := range []string{"one", "two", "three"} {
		if err := log.AppendRaw(context.Background(), "failure:{x}", []byte(item)); err != nil {
			t.Fatalf("AppendRaw %q: %v", item, err)
		}
	}

	got, err := log.ListRaw(context.Background(), "failure:{x}", 0, 0)
	if err != nil {
		t.Fatalf("ListRaw: %v", err)
	}
	if len(got) != 1 || got[0] != "one" {
		t.Fatalf("ListRaw(0,0) = %+v, want [one]", got)
	}
}

func TestRedisListPurgeReturnsEntryCount(t *testing.T) {
	redis := newFakeRedis()
	log := NewRedisList(redis, Config{})
	for _, item := range []string{"one", "two", "three"} {
		if err := log.AppendRaw(context.Background(), "failure:{x}", []byte(item)); err != nil {
			t.Fatalf("AppendRaw %q: %v", item, err)
		}
	}

	n, err := log.Purge(context.Background(), "failure:{x}")
	if err != nil {
		t.Fatalf("Purge: %v", err)
	}
	if n != 3 {
		t.Fatalf("purged = %d, want entry count 3", n)
	}
	got, err := log.ListRaw(context.Background(), "failure:{x}", 0, -1)
	if err != nil {
		t.Fatalf("ListRaw: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("items after purge = %+v", got)
	}
}

func TestRedisListCountRawReturnsListLength(t *testing.T) {
	redis := newFakeRedis()
	log := NewRedisList(redis, Config{Namespace: "dlq", MaxEntries: 10})
	for _, item := range []string{"one", "two"} {
		if err := log.AppendRaw(context.Background(), "failure:{x}", []byte(item)); err != nil {
			t.Fatalf("AppendRaw %q: %v", item, err)
		}
	}

	got, err := log.CountRaw(context.Background(), "failure:{x}")
	if err != nil {
		t.Fatalf("CountRaw: %v", err)
	}
	if got != 2 {
		t.Fatalf("CountRaw = %d, want 2", got)
	}
}

func TestRedisListMetricsIncludeNamespace(t *testing.T) {
	metrics.DefaultRegistry().Reset()
	redis := newFakeRedis()
	log := NewRedisList(redis, Config{Namespace: "bus_dlq", MaxEntries: 10})

	if err := log.AppendRaw(context.Background(), "failure:{x}", []byte("one")); err != nil {
		t.Fatalf("AppendRaw: %v", err)
	}
	if _, err := log.DeleteRaw(context.Background(), "failure:{x}", [][]byte{[]byte("one")}); err != nil {
		t.Fatalf("DeleteRaw: %v", err)
	}

	var appendSeen, deleteSeen bool
	for _, metric := range metrics.Snapshot() {
		if metric.Labels["namespace"] != "bus_dlq" {
			continue
		}
		switch metric.Name {
		case "failurelog_append_total":
			appendSeen = metric.Labels["result"] == "ok" && metric.Value == 1
		case "failurelog_delete_total":
			deleteSeen = metric.Labels["result"] == "ok" && metric.Value == 1
		}
	}
	if !appendSeen || !deleteSeen {
		t.Fatalf("namespace metrics missing append=%v delete=%v snapshot=%+v", appendSeen, deleteSeen, metrics.Snapshot())
	}
}

func TestRedisListMetricsRecordAppendErrors(t *testing.T) {
	metrics.DefaultRegistry().Reset()
	redis := newFakeRedis()
	redis.rpushErr = errors.New("redis unavailable")
	log := NewRedisList(redis, Config{Namespace: "entitysync_failed", MaxEntries: 10})

	if err := log.AppendRaw(context.Background(), "failure:{x}", []byte("one")); err == nil {
		t.Fatalf("AppendRaw should return redis error")
	}

	for _, metric := range metrics.Snapshot() {
		if metric.Name == "failurelog_append_total" &&
			metric.Labels["namespace"] == "entitysync_failed" &&
			metric.Labels["result"] == "error" &&
			metric.Value == 1 {
			return
		}
	}
	t.Fatalf("append error metric missing: %+v", metrics.Snapshot())
}

type fakeRedis struct {
	lists    map[string][]string
	expire   map[string]time.Duration
	rpushErr error
}

func newFakeRedis() *fakeRedis {
	return &fakeRedis{
		lists:  make(map[string][]string),
		expire: make(map[string]time.Duration),
	}
}

func (r *fakeRedis) Get(context.Context, string) ([]byte, error) { return nil, fredis.ErrNil }

// This double stores lists only, so every key is absent to a value read. MGet
// still returns one element per key, because the contract is positional and a
// caller comparing lengths must get a usable answer.
func (r *fakeRedis) MGet(_ context.Context, keys ...string) ([][]byte, error) {
	if len(keys) == 0 {
		return nil, nil
	}
	return make([][]byte, len(keys)), nil
}
func (r *fakeRedis) Set(context.Context, string, any, time.Duration) error { return nil }
func (r *fakeRedis) SetNX(context.Context, string, any, time.Duration) (bool, error) {
	return false, nil
}
func (r *fakeRedis) Del(_ context.Context, keys ...string) (int64, error) {
	var n int64
	for _, key := range keys {
		if _, ok := r.lists[key]; ok {
			delete(r.lists, key)
			n++
		}
		delete(r.expire, key)
	}
	return n, nil
}
func (r *fakeRedis) Exists(context.Context, ...string) (int64, error) { return 0, nil }
func (r *fakeRedis) Expire(_ context.Context, key string, expiration time.Duration) (bool, error) {
	r.expire[key] = expiration
	return true, nil
}
func (r *fakeRedis) TTL(context.Context, string) (time.Duration, error)         { return 0, nil }
func (r *fakeRedis) Incr(context.Context, string) (int64, error)                { return 0, nil }
func (r *fakeRedis) IncrBy(context.Context, string, int64) (int64, error)       { return 0, nil }
func (r *fakeRedis) HGet(context.Context, string, string) ([]byte, error)       { return nil, nil }
func (r *fakeRedis) HSet(context.Context, string, ...any) error                 { return nil }
func (r *fakeRedis) HGetAll(context.Context, string) (map[string]string, error) { return nil, nil }
func (r *fakeRedis) HDel(context.Context, string, ...string) (int64, error)     { return 0, nil }
func (r *fakeRedis) HExists(context.Context, string, string) (bool, error)      { return false, nil }
func (r *fakeRedis) LPush(context.Context, string, ...any) (int64, error)       { return 0, nil }
func (r *fakeRedis) RPush(_ context.Context, key string, values ...any) (int64, error) {
	if r.rpushErr != nil {
		return 0, r.rpushErr
	}
	for _, value := range values {
		switch typed := value.(type) {
		case []byte:
			r.lists[key] = append(r.lists[key], string(typed))
		case string:
			r.lists[key] = append(r.lists[key], typed)
		default:
			r.lists[key] = append(r.lists[key], "")
		}
	}
	return int64(len(r.lists[key])), nil
}
func (r *fakeRedis) LPop(context.Context, string) ([]byte, error) { return nil, nil }
func (r *fakeRedis) RPop(context.Context, string) ([]byte, error) { return nil, nil }
func (r *fakeRedis) LLen(_ context.Context, key string) (int64, error) {
	return int64(len(r.lists[key])), nil
}
func (r *fakeRedis) LRange(_ context.Context, key string, start, stop int64) ([]string, error) {
	items := r.lists[key]
	if stop < 0 || stop >= int64(len(items)) {
		stop = int64(len(items)) - 1
	}
	if start < 0 {
		start = 0
	}
	if start > stop || start >= int64(len(items)) {
		return nil, nil
	}
	return append([]string(nil), items[start:stop+1]...), nil
}
func (r *fakeRedis) ZAdd(context.Context, string, ...fredis.Z) (int64, error) { return 0, nil }
func (r *fakeRedis) ZRem(context.Context, string, ...any) (int64, error)      { return 0, nil }
func (r *fakeRedis) ZScore(context.Context, string, string) (float64, error)  { return 0, nil }
func (r *fakeRedis) ZRank(context.Context, string, string) (int64, error)     { return 0, nil }
func (r *fakeRedis) ZRevRank(context.Context, string, string) (int64, error)  { return 0, nil }
func (r *fakeRedis) ZRangeWithScores(context.Context, string, int64, int64) ([]fredis.Z, error) {
	return nil, nil
}
func (r *fakeRedis) ZRevRangeWithScores(context.Context, string, int64, int64) ([]fredis.Z, error) {
	return nil, nil
}
func (r *fakeRedis) ZCard(context.Context, string) (int64, error)        { return 0, nil }
func (r *fakeRedis) SAdd(context.Context, string, ...any) (int64, error) { return 0, nil }
func (r *fakeRedis) SRem(context.Context, string, ...any) (int64, error) { return 0, nil }
func (r *fakeRedis) SMembers(context.Context, string) ([]string, error)  { return nil, nil }
func (r *fakeRedis) SIsMember(context.Context, string, any) (bool, error) {
	return false, nil
}
func (r *fakeRedis) Pipeline() fredis.IPipeline { return nil }
func (r *fakeRedis) Eval(context.Context, string, []string, ...any) (any, error) {
	return nil, nil
}
func (r *fakeRedis) EvalSha(context.Context, string, []string, ...any) (any, error) {
	return nil, nil
}
func (r *fakeRedis) Publish(context.Context, string, any) error          { return nil }
func (r *fakeRedis) Subscribe(context.Context, ...string) fredis.IPubSub { return nil }
func (r *fakeRedis) Ping(context.Context) error                          { return nil }
func (r *fakeRedis) Close() error                                        { return nil }

var _ fredis.IRedis = (*fakeRedis)(nil)

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
	metrics.DefaultRegistry().Reset()
	redis := newFakeRedis() // Eval returns (nil, nil): the script path never engages
	log := NewRedisList(redis, Config{Namespace: "combat", MaxEntries: 10})
	if err := log.AppendRaw(context.Background(), "failure:{x}", []byte("one")); err != nil {
		t.Fatalf("AppendRaw: %v", err)
	}
	for _, metric := range metrics.Snapshot() {
		if metric.Name == "failurelog_degraded_total" &&
			metric.Labels["namespace"] == "combat" &&
			metric.Labels["op"] == "append" &&
			metric.Value == 1 {
			return
		}
	}
	t.Fatalf("degraded metric missing: %+v", metrics.Snapshot())
}

// 每个操作对空 key 报 ErrKeyEmpty，不把空 key 当成一个真的列表去碰 Redis。
func TestRedisListRefusesAnEmptyKeyOnEveryOperation(t *testing.T) {
	ctx := context.Background()
	list := NewRedisList(newFakeRedis(), Config{Namespace: "test"})
	if err := list.AppendRaw(ctx, "", []byte("x")); !errors.Is(err, ErrKeyEmpty) {
		t.Fatalf("AppendRaw = %v", err)
	}
	if _, err := list.ListRaw(ctx, "", 0, -1); !errors.Is(err, ErrKeyEmpty) {
		t.Fatalf("ListRaw = %v", err)
	}
	if _, err := list.Purge(ctx, ""); !errors.Is(err, ErrKeyEmpty) {
		t.Fatalf("Purge = %v", err)
	}
	if _, err := list.CountRaw(ctx, ""); !errors.Is(err, ErrKeyEmpty) {
		t.Fatalf("CountRaw = %v", err)
	}
	if _, err := list.DeleteRaw(ctx, "", [][]byte{[]byte("x")}); !errors.Is(err, ErrKeyEmpty) {
		t.Fatalf("DeleteRaw = %v", err)
	}
}

type trimResultRedis struct {
	*fakeRedis
	result any
}

func (r trimResultRedis) Eval(context.Context, string, []string, ...any) (any, error) {
	return r.result, nil
}
func TestLuaAppendCountsTrimmedRecords(t *testing.T) {
	metrics.DefaultRegistry().Reset()
	l := NewRedisList(trimResultRedis{newFakeRedis(), int64(5)}, Config{Namespace: "b7_trim", MaxEntries: 2})
	if err := l.AppendRaw(context.Background(), "b7", []byte("x")); err != nil {
		t.Fatal(err)
	}
	for _, s := range metrics.Snapshot() {
		if s.Name == "failurelog_trim_total" && s.Labels["namespace"] == "b7_trim" && s.Value == 3 {
			return
		}
	}
	t.Fatal("Lua removed three records without the trim counter")
}

// 脚本结果未知时不走非原子降级：AppendRaw / DeleteRaw / Purge 的 Lua 脚本回复丢失时脚本可能已经执行，再降级到 RPUSH / LREM / LLEN+DEL
// 就是第二次执行；结果未知原样交给调用方。降级只给“适配器没有 Lua”用。
// 历史：RR-20261005-NC-160。

var errReplyLost = errors.New("read tcp: i/o timeout (reply lost)")

// lostReplyRedis 在 fakeRedis 上模拟“脚本已在服务端执行、回复丢了”：Eval 先按脚本语义改动列表，
// 再返回传输错误。afterScript 用来插入“脚本执行后、调用方收到错误前”另一写者的动作。
type lostReplyRedis struct {
	*fakeRedis
	evals       int
	afterScript func()
}

func (r *lostReplyRedis) Eval(ctx context.Context, script string, keys []string, args ...any) (any, error) {
	r.evals++
	key := keys[0]
	switch script {
	case appendTrimScript:
		_, _ = r.fakeRedis.RPush(ctx, key, args[0])
	case deleteRawScript:
		remove := map[string]int{}
		for _, arg := range args {
			remove[arg.(string)]++
		}
		kept := r.lists[key][:0]
		for _, item := range r.lists[key] {
			if remove[item] > 0 {
				remove[item]--
				continue
			}
			kept = append(kept, item)
		}
		r.lists[key] = kept
	case purgeRawScript:
		delete(r.lists, key)
	}
	if r.afterScript != nil {
		r.afterScript()
	}
	return nil, errReplyLost
}

func TestUnknownScriptResultIsReturnedWithoutReplayingTheWrite(t *testing.T) {
	const key = "failure:{x}"
	ctx := context.Background()

	t.Run("append", func(t *testing.T) {
		redis := &lostReplyRedis{fakeRedis: newFakeRedis()}
		list := NewRedisList(redis, Config{Namespace: "dlq", MaxEntries: 10})
		err := list.AppendRaw(ctx, key, []byte("dead-letter"))
		if got := redis.lists[key]; len(got) != 1 {
			t.Fatalf("one AppendRaw whose script ran but whose reply was lost stored %d entries %v (err=%v); want exactly the script's one", len(got), got, err)
		}
		if !errors.Is(err, errReplyLost) {
			t.Fatalf("AppendRaw err = %v, want the unknown-result error kept for the caller", err)
		}
	})

	t.Run("delete", func(t *testing.T) {
		redis := &lostReplyRedis{fakeRedis: newFakeRedis()}
		redis.lists[key] = []string{"same", "same"}
		list := NewRedisList(redis, Config{Namespace: "dlq", MaxEntries: 10})
		_, err := list.DeleteRaw(ctx, key, [][]byte{[]byte("same")})
		if got := redis.lists[key]; len(got) != 1 {
			t.Fatalf("deleting one copy whose script ran but whose reply was lost left %v (err=%v); want one copy kept", got, err)
		}
		if !errors.Is(err, errReplyLost) {
			t.Fatalf("DeleteRaw err = %v, want the unknown-result error", err)
		}
	})

	t.Run("purge", func(t *testing.T) {
		redis := &lostReplyRedis{fakeRedis: newFakeRedis()}
		redis.lists[key] = []string{"old-1", "old-2"}
		// 脚本清空之后、调用方收到错误之前，另一个消费者写进一条新死信。
		redis.afterScript = func() { redis.lists[key] = []string{"arrived-after-purge"} }
		list := NewRedisList(redis, Config{Namespace: "dlq", MaxEntries: 10})
		_, err := list.Purge(ctx, key)
		if got := redis.lists[key]; len(got) != 1 || got[0] != "arrived-after-purge" {
			t.Fatalf("Purge whose script ran but whose reply was lost left %v (err=%v); the dead letter written after the purge must survive", got, err)
		}
		if !errors.Is(err, errReplyLost) {
			t.Fatalf("Purge err = %v, want the unknown-result error", err)
		}
	})
}

// 降级路径仍然留给真的没有 Lua 的适配器（Eval 返回 nil, nil 的替身）：行为不变。
func TestAdapterWithoutLuaStillUsesTheFallback(t *testing.T) {
	redis := newFakeRedis()
	list := NewRedisList(redis, Config{MaxEntries: 10})
	if err := list.AppendRaw(context.Background(), "failure:{x}", []byte("one")); err != nil {
		t.Fatal(err)
	}
	if got := redis.lists["failure:{x}"]; len(got) != 1 {
		t.Fatalf("fallback append stored %v", got)
	}
}
