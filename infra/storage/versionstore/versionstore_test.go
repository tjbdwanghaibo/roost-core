// 版本存储：版本从 1 递增、Update / Create / Delete 的语义、并发不丢写、冲突预算与重发共用、退避后重读、指标与入参守卫。

package versionstore

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
)

type counter struct {
	Name  string `json:"name"`
	Total int64  `json:"total"`
}

// Both implementations are driven through the same contract suite. A contract
// with one implementation drifts into that implementation's accidents, and the
// defect this package exists to prevent is exactly an implementation that
// satisfies the interface without honouring it.
func forEachStore(t *testing.T, run func(t *testing.T, store Store[string, counter])) {
	t.Helper()
	t.Run("memory", func(t *testing.T) {
		run(t, NewMemoryStore[string, counter]())
	})
	t.Run("redis", func(t *testing.T) {
		store, err := NewRedisStore[string, counter](newFakeRedis(), RedisConfig[string, counter]{
			Prefix: "test:",
			KeyOf:  func(key string) string { return key },
			Codec:  JSONCodec[counter]{},
		})
		if err != nil {
			t.Fatal(err)
		}
		run(t, store)
	})
}

// A stored value always has Version >= 1, so no caller can opt out of the
// comparison by leaving a version field at its zero value — the way a service
// that submitted Version: 0 skipped its guard entirely and double-counted
// every redelivered event.
func TestStoredVersionIsNeverZeroAndIncrementsPerWrite(t *testing.T) {
	forEachStore(t, func(t *testing.T, store Store[string, counter]) {
		ctx := context.Background()
		if got, found, err := store.Get(ctx, "a"); err != nil || found || got.Version != 0 {
			t.Fatalf("absent key: %+v found=%v err=%v", got, found, err)
		}
		for want := uint64(1); want <= 3; want++ {
			result, applied, err := store.Update(ctx, "a", func(current counter, _ bool) (counter, bool, error) {
				current.Total++
				return current, true, nil
			})
			if err != nil || !applied {
				t.Fatalf("update %d: applied=%v err=%v", want, applied, err)
			}
			if result.Version != want {
				t.Fatalf("version after %d writes = %d, want %d", want, result.Version, want)
			}
		}
		stored, found, err := store.Get(ctx, "a")
		if err != nil || !found {
			t.Fatalf("get: found=%v err=%v", found, err)
		}
		if stored.Value.Total != 3 || stored.Version != 3 {
			t.Fatalf("stored = %+v, want total 3 version 3", stored)
		}
	})
}

// mutate declining to save must leave the store untouched and report that
// nothing was applied — not report success for a write that did not happen.
func TestUpdateDecliningToSaveChangesNothing(t *testing.T) {
	forEachStore(t, func(t *testing.T, store Store[string, counter]) {
		ctx := context.Background()
		if _, _, err := store.Update(ctx, "a", func(c counter, _ bool) (counter, bool, error) {
			c.Total = 5
			return c, true, nil
		}); err != nil {
			t.Fatal(err)
		}
		result, applied, err := store.Update(ctx, "a", func(c counter, found bool) (counter, bool, error) {
			if !found {
				t.Fatal("mutate saw the key as absent")
			}
			c.Total = 99
			return c, false, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if applied {
			t.Fatal("Update reported applied for a mutate that declined to save")
		}
		if result.Value.Total != 5 || result.Version != 1 {
			t.Fatalf("declined update returned %+v, want the stored value", result)
		}
		stored, _, _ := store.Get(ctx, "a")
		if stored.Value.Total != 5 || stored.Version != 1 {
			t.Fatalf("store changed after a declined update: %+v", stored)
		}
	})
}

// An error from mutate must abort without writing.
func TestUpdatePropagatesMutateErrorWithoutWriting(t *testing.T) {
	forEachStore(t, func(t *testing.T, store Store[string, counter]) {
		ctx := context.Background()
		sentinel := errors.New("business rule")
		if _, _, err := store.Update(ctx, "a", func(c counter, _ bool) (counter, bool, error) {
			return c, true, sentinel
		}); !errors.Is(err, sentinel) {
			t.Fatalf("Update returned %v, want the mutate error", err)
		}
		if _, found, _ := store.Get(ctx, "a"); found {
			t.Fatal("a failed mutate wrote a value")
		}
	})
}

// Create is the insert-only path: it must refuse an existing key rather than
// overwrite it. Blind overwrite on create is how an ID collision silently
// destroys an existing record.
func TestCreateRefusesAnExistingKey(t *testing.T) {
	forEachStore(t, func(t *testing.T, store Store[string, counter]) {
		ctx := context.Background()
		first, created, err := store.Create(ctx, "a", counter{Name: "first"})
		if err != nil || !created {
			t.Fatalf("first Create: created=%v err=%v", created, err)
		}
		if first.Version != 1 {
			t.Fatalf("Create assigned version %d, want 1", first.Version)
		}
		second, created, err := store.Create(ctx, "a", counter{Name: "second"})
		if err != nil {
			t.Fatal(err)
		}
		if created {
			t.Fatal("Create overwrote an existing key")
		}
		if second.Version != 0 {
			t.Fatalf("refused Create returned version %d, want 0", second.Version)
		}
		// The refused Create hands back the ZERO value, not the value it
		// collided with. Every implementation must agree on this, because the
		// interface now promises it — and because assuming otherwise is a
		// quiet mistake: the zero value's fields read as empty, so a caller
		// inspecting them takes the branch meant for "absent" while the key
		// is occupied. That misreading turned an exclusive per-owner claim
		// into no exclusion at all in a consumer of this package.
		if second.Value != (counter{}) {
			t.Fatalf("refused Create returned the colliding value %+v; the contract says "+
				"the zero value, and a caller that needs the existing one must Get it",
				second.Value)
		}
		stored, _, _ := store.Get(ctx, "a")
		if stored.Value.Name != "first" {
			t.Fatalf("existing value was replaced: %+v", stored)
		}
	})
}

// Delete is version-checked: a caller holding a stale version must be refused
// rather than deleting whatever is there now.
func TestDeleteRequiresTheCallersVersion(t *testing.T) {
	forEachStore(t, func(t *testing.T, store Store[string, counter]) {
		ctx := context.Background()
		stored, _, err := store.Update(ctx, "a", func(c counter, _ bool) (counter, bool, error) {
			c.Total = 1
			return c, true, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Delete(ctx, "a", Versioned[counter]{Version: stored.Version + 1}); !errors.Is(err, ErrVersionMismatch) {
			t.Fatalf("Delete with a wrong version returned %v, want ErrVersionMismatch", err)
		}
		if _, found, _ := store.Get(ctx, "a"); !found {
			t.Fatal("a refused Delete removed the value")
		}
		if err := store.Delete(ctx, "a", stored); err != nil {
			t.Fatalf("Delete with the held version: %v", err)
		}
		if _, found, _ := store.Get(ctx, "a"); found {
			t.Fatal("Delete left the value in place")
		}
		// Deleting an absent key is a no-op only for a caller that holds no
		// version; a caller holding one is out of date and must be told.
		if err := store.Delete(ctx, "a", Versioned[counter]{}); err != nil {
			t.Fatalf("Delete of an absent key with no held version: %v", err)
		}
		if err := store.Delete(ctx, "a", Versioned[counter]{Version: 9}); !errors.Is(err, ErrVersionMismatch) {
			t.Fatalf("Delete of an absent key holding a version returned %v", err)
		}
	})
}

// The property the whole package exists for: concurrent read-modify-write must
// not lose an update. Every increment has to survive.
func TestConcurrentUpdatesLoseNothing(t *testing.T) {
	forEachStore(t, func(t *testing.T, store Store[string, counter]) {
		ctx := context.Background()
		const writers, perWriter = 8, 25
		// A caller-level retry ceiling, so a store that ALWAYS conflicts still
		// fails this test rather than spinning.
		const maxCallerRetries = 200

		var (
			wait      sync.WaitGroup
			conflicts atomic.Int64
		)
		errs := make([]error, writers)
		for writer := 0; writer < writers; writer++ {
			wait.Add(1)
			go func(index int) {
				defer wait.Done()
				for i := 0; i < perWriter; i++ {
					// Retry on ErrConflict, because that is what a caller does.
					//
					// The attempt budget inside Update is a BOUND, not a
					// guarantee, and giving up after it is a reported outcome —
					// a distinguishable error — not a lost update. So asserting
					// that no writer ever reaches the bound would be a
					// throughput claim wearing a correctness claim's name, and
					// it is one that does not hold under -race, where every
					// goroutine runs slowly enough for eight writers to lose
					// eight times in a row on a single key. That version of this
					// test failed 6 runs in 8 under -race and shipped in v1.11.0
					// doing so.
					//
					// What is actually under test is below: after all the work,
					// the total and the version are EXACT. A dropped update or
					// a double count fails that, whatever the timing.
					for attempt := 0; ; attempt++ {
						_, _, err := store.Update(ctx, "a", func(c counter, _ bool) (counter, bool, error) {
							c.Total++
							return c, true, nil
						})
						if err == nil {
							break
						}
						if !errors.Is(err, ErrConflict) {
							errs[index] = err
							return
						}
						conflicts.Add(1)
						if attempt >= maxCallerRetries {
							errs[index] = fmt.Errorf("gave up after %d caller retries: %w", attempt, err)
							return
						}
					}
				}
			}(writer)
		}
		wait.Wait()
		for writer, err := range errs {
			if err != nil {
				t.Fatalf("writer %d: %v", writer, err)
			}
		}
		stored, found, err := store.Get(ctx, "a")
		if err != nil || !found {
			t.Fatalf("get: found=%v err=%v", found, err)
		}
		if stored.Value.Total != writers*perWriter {
			t.Fatalf("total = %d, want %d: updates were lost (%d conflicts retried)",
				stored.Value.Total, writers*perWriter, conflicts.Load())
		}
		if stored.Version != uint64(writers*perWriter) {
			t.Fatalf("version = %d, want %d: a write did not bump the version",
				stored.Version, writers*perWriter)
		}
	})
}

// Only one concurrent Create may win.
func TestConcurrentCreateHasExactlyOneWinner(t *testing.T) {
	forEachStore(t, func(t *testing.T, store Store[string, counter]) {
		ctx := context.Background()
		const racers = 8
		var wait sync.WaitGroup
		var mu sync.Mutex
		wins := 0
		for racer := 0; racer < racers; racer++ {
			wait.Add(1)
			go func(index int) {
				defer wait.Done()
				_, created, err := store.Create(ctx, "a", counter{Name: fmt.Sprintf("racer-%d", index)})
				if err != nil {
					return
				}
				if created {
					mu.Lock()
					wins++
					mu.Unlock()
				}
			}(racer)
		}
		wait.Wait()
		if wins != 1 {
			t.Fatalf("%d creators reported success, want exactly 1", wins)
		}
	})
}

// Exhausting the retry budget must be reported as a conflict, distinguishable
// from a store failure: contention and an unreachable backend need different
// operational responses.
func TestUpdateReportsConflictWhenTheBudgetIsExhausted(t *testing.T) {
	fake := newFakeRedis()
	store, err := NewRedisStore[string, counter](fake, RedisConfig[string, counter]{
		KeyOf: func(key string) string { return key }, Codec: JSONCodec[counter]{}, MaxAttempts: 3, RetryBackoff: -1,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, _, err := store.Update(ctx, "a", func(c counter, _ bool) (counter, bool, error) {
		return c, true, nil
	}); err != nil {
		t.Fatal(err)
	}
	// Every compare-and-set now loses, as if another writer always won.
	fake.failEveryCAS = true
	_, applied, err := store.Update(ctx, "a", func(c counter, _ bool) (counter, bool, error) {
		c.Total++
		return c, true, nil
	})
	if applied {
		t.Fatal("Update reported applied despite never winning the compare-and-set")
	}
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("Update returned %v, want ErrConflict", err)
	}
	if !strings.Contains(err.Error(), "3 attempts") {
		t.Fatalf("the conflict error does not say how many attempts were made: %v", err)
	}
}

// The compare must be on the version, not on a re-encoded value: a codec whose
// output changes for the same value must not wedge writes. This is the failure
// that took down every role write in a service that compared marshalled JSON.
func TestUpdateSurvivesACodecWhoseOutputChanges(t *testing.T) {
	drifting := &driftingCodec{}
	store, err := NewRedisStore[string, counter](newFakeRedis(), RedisConfig[string, counter]{
		KeyOf: func(key string) string { return key }, Codec: drifting,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		if _, applied, err := store.Update(ctx, "a", func(c counter, _ bool) (counter, bool, error) {
			c.Total++
			return c, true, nil
		}); err != nil || !applied {
			t.Fatalf("write %d with a drifting codec: applied=%v err=%v", i, applied, err)
		}
	}
	stored, _, err := store.Get(ctx, "a")
	if err != nil {
		t.Fatal(err)
	}
	if stored.Value.Total != 5 {
		t.Fatalf("total = %d, want 5", stored.Value.Total)
	}
}

// driftingCodec appends a per-call suffix so that encoding the same value
// twice never produces the same bytes.
type driftingCodec struct{ calls int }

func (c *driftingCodec) Encode(value counter) ([]byte, error) {
	c.calls++
	return []byte(fmt.Sprintf(`{"name":%q,"total":%d,"nonce":%d}`, value.Name, value.Total, c.calls)), nil
}

func (c *driftingCodec) Decode(raw []byte) (counter, error) {
	return JSONCodec[counter]{}.Decode(raw)
}

func TestNewRedisStoreRejectsIncompleteConfig(t *testing.T) {
	if _, err := NewRedisStore[string, counter](nil, RedisConfig[string, counter]{}); err == nil {
		t.Fatal("nil client accepted")
	}
	if _, err := NewRedisStore[string, counter](newFakeRedis(), RedisConfig[string, counter]{Codec: JSONCodec[counter]{}}); err == nil {
		t.Fatal("nil key func accepted")
	}
	if _, err := NewRedisStore[string, counter](newFakeRedis(), RedisConfig[string, counter]{
		KeyOf: func(string) string { return "k" },
	}); !errors.Is(err, ErrCodecNil) {
		t.Fatal("nil codec accepted")
	}
}

func TestEmptyRenderedKeyIsRejected(t *testing.T) {
	store, err := NewRedisStore[string, counter](newFakeRedis(), RedisConfig[string, counter]{
		KeyOf: func(string) string { return "" }, Codec: JSONCodec[counter]{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Get(context.Background(), "a"); !errors.Is(err, ErrKeyEmpty) {
		t.Fatalf("empty rendered key returned %v, want ErrKeyEmpty", err)
	}
}

// 内存 / Redis 存储的 Update 拒绝 nil 变更函数；Redis 存储缺客户端不能构造；带期望版本的 Delete 在版本不符时报 ErrVersionMismatch 且不删。
func TestStoresRefuseNilMutatorsMissingClientsAndStaleDeletes(t *testing.T) {
	ctx := context.Background()
	if _, _, err := NewMemoryStore[string, counter]().Update(ctx, "k", nil); err == nil || !strings.Contains(err.Error(), "mutate is nil") {
		t.Fatalf("memory Update(nil) = %v", err)
	}
	cfg := RedisConfig[string, counter]{Prefix: "test:", KeyOf: func(key string) string { return key }, Codec: JSONCodec[counter]{}}
	if _, err := NewRedisStore[string, counter](nil, cfg); err == nil || !strings.Contains(err.Error(), "redis client is nil") {
		t.Fatalf("NewRedisStore(nil) = %v", err)
	}
	store, err := NewRedisStore[string, counter](newFakeRedis(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Update(ctx, "k", nil); err == nil || !strings.Contains(err.Error(), "mutate is nil") {
		t.Fatalf("redis Update(nil) = %v", err)
	}
	created, ok, err := store.Create(ctx, "k", counter{})
	if err != nil || !ok {
		t.Fatalf("Create = (%+v, %v, %v)", created, ok, err)
	}
	if err := store.Delete(ctx, "k", Versioned[counter]{Value: created.Value, Version: created.Version + 5}); !errors.Is(err, ErrVersionMismatch) {
		t.Fatalf("Delete with a stale version = %v, want ErrVersionMismatch", err)
	}
	if _, found, err := store.Get(ctx, "k"); err != nil || !found {
		t.Fatalf("a refused delete removed the value: found=%v err=%v", found, err)
	}
	if err := store.Delete(ctx, "k", created); err != nil {
		t.Fatalf("Delete with the current version = %v", err)
	}
}

// 每两次丢失发送后让竞争者写入下一版本，逼出「重发 × CAS 重试」的组合路径。
type contendedLostReplies struct {
	*fakeRedis
	sends int
}

func (f *contendedLostReplies) Eval(_ context.Context, _ string, keys []string, args ...any) (any, error) {
	f.sends++
	if f.sends%3 == 0 {
		header, err := parseEnvelope(f.values[keys[0]])
		if err != nil {
			return nil, err
		}
		f.values[keys[0]] = buildEnvelope(header.payload, header.version+1, newWriteToken(), header.tokens, 8)
	}
	return nil, errLostReply
}

func TestResendsAndCASRacesShareOneSendBudget(t *testing.T) {
	base := newFakeRedis()
	base.values["budget:a"] = buildEnvelope([]byte(`{"Name":"a","Total":0}`), 1, newWriteToken(), nil, 8)
	client := &contendedLostReplies{fakeRedis: base}
	store, err := NewRedisStore(client, RedisConfig[string, counter]{Prefix: "budget:", KeyOf: func(k string) string { return k }, Codec: JSONCodec[counter]{}, MaxAttempts: 3, RetryBackoff: -1})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = store.Update(context.Background(), "a", increment)
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("known lost race = %v", err)
	}
	if client.sends != 3 {
		t.Fatalf("one Update sent %d commands, budget=3", client.sends)
	}
}

func TestResumedUpdateCountsItsResolvedCAS(t *testing.T) {
	old := metrics.DefaultRegistry()
	reg := metrics.NewRegistry()
	metrics.SetDefaultRegistry(reg)
	t.Cleanup(func() { metrics.SetDefaultRegistry(old) })
	fake := newFakeRedis()
	store := newTokenTestStore(t, fake)
	ctx := context.Background()
	if _, _, err := store.Create(ctx, "a", counter{}); err != nil {
		t.Fatal(err)
	}
	loseReplyUnchecked(fake, false)
	_, _, err := store.Update(ctx, "a", increment)
	if !errors.Is(err, ErrOutcomeUnknown) {
		t.Fatalf("want unknown: %v", err)
	}
	if _, _, err = store.Update(Resume(ctx, err), "a", increment); err != nil {
		t.Fatal(err)
	}
	for _, metric := range reg.Snapshot() {
		if metric.Name == MetricCompareAndSet && metric.Labels["store"] == "tok:" && metric.Labels["result"] == "applied" && metric.Value == 1 {
			return
		}
	}
	t.Fatalf("resolved resumed CAS not counted: %v", reg.Snapshot())
}

// 维护者第十二轮决定（CAS 冲突率口径，N06 观察 2）：compare-and-set 冲突率在 versionstore 层统一
// 计数，不再由各服务各报一份。之前只有 chat 在拿到 ErrConflict 时报 Conflict，其他服务、account /
// activity 一处都没有，kit/service/README 第 6 条要求的“冲突率”没有统一口径。
//
// 承诺：RedisStore.Update 的每次 compare-and-set 计一次 versionstore.cas.total{store, result}，
// 赢了 result=applied、输了 result=lost；预算用尽返回 ErrConflict 时另计一次
// versionstore.conflict.total{store}。store 是该存储的键前缀（每个存储一个，低基数）。冲突率 =
// lost / (applied + lost)。

func TestUpdateCountsEveryCompareAndSetAndTheExhaustedConflict(t *testing.T) {
	old := metrics.DefaultRegistry()
	reg := metrics.NewRegistry()
	metrics.SetDefaultRegistry(reg)
	t.Cleanup(func() { metrics.SetDefaultRegistry(old) })

	fake := newFakeRedis()
	store, err := NewRedisStore[string, counter](fake, RedisConfig[string, counter]{
		Prefix: "roost:test:counter:", KeyOf: func(key string) string { return key }, Codec: JSONCodec[counter]{},
		MaxAttempts: 3, RetryBackoff: -1,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	bump := func(c counter, _ bool) (counter, bool, error) { c.Total++; return c, true, nil }
	for range 2 {
		if _, _, err := store.Update(ctx, "a", bump); err != nil {
			t.Fatal(err)
		}
	}
	fake.failEveryCAS = true
	if _, _, err := store.Update(ctx, "a", bump); !errors.Is(err, ErrConflict) {
		t.Fatalf("Update under permanent contention = %v, want ErrConflict", err)
	}
	// 不保存（save=false）不发 compare-and-set，也不计数。
	if _, _, err := store.Update(ctx, "a", func(c counter, _ bool) (counter, bool, error) { return c, false, nil }); err != nil {
		t.Fatal(err)
	}

	value := func(name string, labels metrics.Labels) int64 {
		for _, metric := range reg.Snapshot() {
			if metric.Name != name || len(metric.Labels) != len(labels) {
				continue
			}
			match := true
			for k, v := range labels {
				match = match && metric.Labels[k] == v
			}
			if match {
				return metric.Value
			}
		}
		return 0
	}
	const label = "roost:test:counter:"
	if got := value(MetricCompareAndSet, metrics.Labels{"store": label, "result": "applied"}); got != 2 {
		t.Errorf("%s{result=applied} = %d, want 2", MetricCompareAndSet, got)
	}
	if got := value(MetricCompareAndSet, metrics.Labels{"store": label, "result": "lost"}); got != 3 {
		t.Errorf("%s{result=lost} = %d, want 3 (every attempt of the exhausted Update)", MetricCompareAndSet, got)
	}
	if got := value(MetricConflict, metrics.Labels{"store": label}); got != 1 {
		t.Errorf("%s = %d, want 1", MetricConflict, got)
	}
}

// RedisStore.Update 输掉 compare-and-set 后先退避，重试基于退避之后的存储状态（不是输掉那一刻带回的旧值）：竞争者只在退避窗口里写时，
// 第二次尝试就成功，竞争写一个不丢。
// 历史：RR-20261005-NC-52。

func TestARetryAfterBackoffSeesWritesThatLandedDuringTheBackoff(t *testing.T) {
	ctx := context.Background()
	fake := newFakeRedis()
	newStore := func(sleep func(time.Duration)) *RedisStore[string, int] {
		store, err := NewRedisStore(fake, RedisConfig[string, int]{
			Prefix: "nc52:", KeyOf: func(k string) string { return k }, Codec: JSONCodec[int]{},
			RetryBackoff: time.Millisecond, Sleep: sleep,
		})
		if err != nil {
			t.Fatal(err)
		}
		return store
	}
	competitor := newStore(func(time.Duration) {})
	competitorWrites := 0
	compete := func() {
		if _, _, err := competitor.Update(ctx, "k", func(v int, _ bool) (int, bool, error) { return v + 1000, true, nil }); err != nil {
			t.Fatalf("competitor write: %v", err)
		}
		competitorWrites++
	}
	if _, _, err := competitor.Create(ctx, "k", 0); err != nil {
		t.Fatal(err)
	}

	// The competitor takes its turn while the loser backs off — which is
	// exactly what the backoff is for — once per backoff window.
	sleeps := 0
	loser := newStore(func(time.Duration) { sleeps++; compete() })
	mutates := 0
	stored, saved, err := loser.Update(ctx, "k", func(v int, _ bool) (int, bool, error) {
		mutates++
		if mutates == 1 {
			compete() // a write between this attempt's read and its compare-and-set
		}
		return v + 1, true, nil
	})
	if err != nil {
		t.Fatalf("Update after %d attempts (%d backoffs, %d competitor writes) failed: %v", mutates, sleeps, competitorWrites, err)
	}
	if !saved || mutates != 2 || sleeps != 1 {
		t.Fatalf("saved=%v after %d attempts and %d backoffs; want saved on the 2nd attempt after 1 backoff", saved, mutates, sleeps)
	}
	final, found, err := loser.Get(ctx, "k")
	if err != nil || !found {
		t.Fatalf("read back: found=%v err=%v", found, err)
	}
	if want := 1 + 1000*competitorWrites; final.Value != want || stored.Value != want || final.Version != stored.Version {
		t.Fatalf("final %+v, returned %+v; want value %d (one own increment, %d competitor writes)", final, stored, want, competitorWrites)
	}
}

// A lost compare-and-set with no write during the backoff still needs exactly
// one more attempt, and a deleted key is seen as absent on the retry.
func TestARetryAfterBackoffSeesADeletionAsAbsence(t *testing.T) {
	ctx := context.Background()
	fake := newFakeRedis()
	store, err := NewRedisStore(fake, RedisConfig[string, int]{
		Prefix: "nc52:", KeyOf: func(k string) string { return k }, Codec: JSONCodec[int]{},
		RetryBackoff: time.Millisecond,
		Sleep: func(time.Duration) {
			if _, err := fake.Del(ctx, "nc52:k"); err != nil {
				t.Fatal(err)
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Create(ctx, "k", 5); err != nil {
		t.Fatal(err)
	}
	other, err := NewRedisStore(fake, RedisConfig[string, int]{Prefix: "nc52:", KeyOf: func(k string) string { return k }, Codec: JSONCodec[int]{}})
	if err != nil {
		t.Fatal(err)
	}
	var seen []bool
	_, _, err = store.Update(ctx, "k", func(v int, found bool) (int, bool, error) {
		seen = append(seen, found)
		if len(seen) == 1 {
			if _, _, err := other.Update(ctx, "k", func(v int, _ bool) (int, bool, error) { return v + 1, true, nil }); err != nil {
				t.Fatal(err)
			}
		}
		return v + 1, true, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(seen) != 2 || !seen[0] || seen[1] {
		t.Fatalf("mutate saw found=%v; want present then absent (deleted during the backoff)", seen)
	}
	got, found, err := store.Get(ctx, "k")
	if err != nil || !found || got.Value != 1 || got.Version != 1 {
		t.Fatalf("recreated value %+v found=%v err=%v; want a fresh key at version 1", got, found, err)
	}
}
