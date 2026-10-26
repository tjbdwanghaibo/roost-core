package storeservice

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"
	fredis "github.com/tjbdwanghaibo/roost-core/infra/storage/redis"
	redisdriver "github.com/tjbdwanghaibo/roost-core/infra/storage/redis/driver"
	"github.com/tjbdwanghaibo/roost-core/infra/storage/redis/redistest"
)

// memoryProjection 记下交给它的条目；fail 返回非 nil 时这一批失败。
type memoryProjection struct {
	prefix     string
	partitions int

	mu       sync.Mutex
	applied  map[int][]string
	advanced map[int]string
	fail     func(partition int, entries []Entry) error
}

func (p *memoryProjection) Name() string      { return "memory" }
func (p *memoryProjection) KeyPrefix() string { return p.prefix }
func (p *memoryProjection) Partitions() int   { return p.partitions }
func (p *memoryProjection) Apply(_ context.Context, partition int, entries []Entry) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.fail != nil {
		if err := p.fail(partition, entries); err != nil {
			return err
		}
	}
	for _, entry := range entries {
		p.applied[partition] = append(p.applied[partition], entry.Fields["n"])
	}
	return nil
}
func (p *memoryProjection) AfterAdvance(_ context.Context, partition int, upto string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.advanced[partition] = upto
	return nil
}
func (p *memoryProjection) snapshot(partition int) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.applied[partition])
}

type runnerEnv struct {
	client *redisdriver.Client
	raw    *goredis.Client
	prefix string
}

func newRunnerEnv(t *testing.T) runnerEnv {
	t.Helper()
	addr := redistest.AOFAddr(t)
	client := redisdriver.NewRedisClient(fredis.DefaultConfig(addr))
	raw := goredis.NewClient(&goredis.Options{Addr: addr})
	t.Cleanup(func() { _ = client.Close(); _ = raw.Close() })
	return runnerEnv{client: client, raw: raw, prefix: redistest.Prefix(t, addr, "storeservice")}
}

func (env runnerEnv) append(t *testing.T, partition, n int) {
	t.Helper()
	for i := range n {
		if err := env.raw.XAdd(context.Background(), &goredis.XAddArgs{Stream: fmt.Sprintf("%s:log:%d", env.prefix, partition), Values: map[string]any{"n": fmt.Sprint(i)}}).Err(); err != nil {
			t.Fatal(err)
		}
	}
}

func waitFor(t *testing.T, what string, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !done() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func startRunner(t *testing.T, env runnerEnv, options Options, projections ...Projection) *Runner {
	t.Helper()
	runner, err := NewRunner(env.client, options, projections...)
	if err != nil {
		t.Fatal(err)
	}
	if err := runner.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := runner.Stop(ctx); err != nil {
			t.Error(err)
		}
	})
	return runner
}

// 运行器按分区顺序把条目交给投影、Mongo 写完才推进进度并裁剪日志；新追加的条目在阻塞读里被接着处理。
func TestRunnerProjectsEachPartitionInOrderAndTrimsAfterAdvance(t *testing.T) {
	env := newRunnerEnv(t)
	projection := &memoryProjection{prefix: env.prefix, partitions: 3, applied: map[int][]string{}, advanced: map[int]string{}}
	for partition := range 3 {
		env.append(t, partition, 600)
	}
	startRunner(t, env, Options{BatchSize: 64, LeaseDuration: time.Second, Block: 20 * time.Millisecond}, projection)
	want := make([]string, 600)
	for i := range want {
		want[i] = fmt.Sprint(i)
	}
	for partition := range 3 {
		waitFor(t, fmt.Sprintf("partition %d drained", partition), func() bool { return len(projection.snapshot(partition)) == 600 })
		if got := projection.snapshot(partition); !slices.Equal(got, want) {
			t.Fatalf("partition %d applied %v..., want 0..599 in order exactly once", partition, got[:5])
		}
	}
	ctx := context.Background()
	waitFor(t, "logs trimmed", func() bool {
		for partition := range 3 {
			if n, _ := env.raw.XLen(ctx, fmt.Sprintf("%s:log:%d", env.prefix, partition)).Result(); n != 0 {
				return false
			}
		}
		return true
	})
	env.append(t, 1, 5)
	waitFor(t, "a later entry", func() bool { return len(projection.snapshot(1)) == 605 })
	progress, err := env.raw.Get(ctx, env.prefix+":proj:1").Result()
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "after advance", func() bool {
		projection.mu.Lock()
		defer projection.mu.Unlock()
		return projection.advanced[1] == progress
	})
}

// 暂时错误退避后重试同一批；非暂时错误写暂停键、暂停分区、不跳过任何条目；运维删除暂停键后从原位置继续。
func TestRunnerPausesOnPermanentErrorAndResumesWhenTheKeyIsDeleted(t *testing.T) {
	env := newRunnerEnv(t)
	var mu sync.Mutex
	failures := map[string]int{"temporary": 2, "permanent": 1}
	projection := &memoryProjection{prefix: env.prefix, partitions: 1, applied: map[int][]string{}, advanced: map[int]string{},
		fail: func(_ int, entries []Entry) error {
			mu.Lock()
			defer mu.Unlock()
			if failures["temporary"] > 0 {
				failures["temporary"]--
				return errors.New("mongo is briefly unavailable")
			}
			if failures["permanent"] > 0 && entries[0].Fields["n"] == "0" {
				failures["permanent"]--
				return Permanent(errors.New("unique business key conflict"))
			}
			return nil
		}}
	env.append(t, 0, 10)
	runner := startRunner(t, env, Options{BatchSize: 4, LeaseDuration: time.Second, Block: 20 * time.Millisecond, PausedRecheck: 20 * time.Millisecond, RetryMin: time.Millisecond}, projection)
	ctx := context.Background()
	waitFor(t, "the pause key", func() bool { n, _ := env.raw.Exists(ctx, env.prefix+":paused:0").Result(); return n == 1 })
	waitFor(t, "Paused() to report partition 0", func() bool { return len(runner.Paused()["memory"]) == 1 })
	time.Sleep(100 * time.Millisecond)
	if got := projection.snapshot(0); len(got) != 0 {
		t.Fatalf("a paused partition applied %v; nothing may be skipped or applied past the failing batch", got)
	}
	if n, _ := env.raw.XLen(ctx, env.prefix+":log:0").Result(); n != 10 {
		t.Fatalf("a paused partition trimmed its log to %d entries", n)
	}
	if err := env.raw.Del(ctx, env.prefix+":paused:0").Err(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the partition resumed", func() bool { return len(projection.snapshot(0)) == 10 })
	if got := projection.snapshot(0); got[0] != "0" || got[9] != "9" {
		t.Fatalf("after resuming: %v, want 0..9 from the failed batch on", got)
	}
	waitFor(t, "paused cleared", func() bool { return len(runner.Paused()) == 0 })
}

// TestProjectionLostLeaseCannotAdvance：租约过期、被另一个实例接管之后，旧持有者的进度推进返回 LOST，不改进度、不裁剪日志；
// 新持有者照常推进。
func TestProjectionLostLeaseCannotAdvance(t *testing.T) {
	env := newRunnerEnv(t)
	ctx := context.Background()
	projection := &memoryProjection{prefix: env.prefix, partitions: 1, applied: map[int][]string{}, advanced: map[int]string{}}
	env.append(t, 0, 3)
	old, err := NewRunner(env.client, Options{Owner: "old", LeaseDuration: 50 * time.Millisecond}, projection)
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := NewRunner(env.client, Options{Owner: "new", LeaseDuration: time.Second}, projection)
	if err != nil {
		t.Fatal(err)
	}
	oldToken, held := old.lease(ctx, projection, 0)
	if !held {
		t.Fatal("the first runner did not get the lease")
	}
	if _, held := fresh.lease(ctx, projection, 0); held {
		t.Fatal("a second runner took a live lease")
	}
	time.Sleep(80 * time.Millisecond)
	newToken, held := fresh.lease(ctx, projection, 0)
	if !held || newToken == oldToken {
		t.Fatalf("takeover after expiry: held=%v token %s (old %s)", held, newToken, oldToken)
	}
	entries, err := fresh.read(ctx, projection, 0, "0-0")
	if err != nil || len(entries) != 3 {
		t.Fatalf("read = %v, %v", entries, err)
	}
	keys := []string{fresh.key(projection, "plock", 0), fresh.key(projection, "proj", 0), fresh.key(projection, "log", 0)}
	if code, _, err := old.run(ctx, old.lua.advance, keys, "old", oldToken, entries[2].ID); err != nil || code != "LOST" {
		t.Fatalf("advance by the previous holder = %q, %v; want LOST", code, err)
	}
	if n, _ := env.raw.XLen(ctx, keys[2]).Result(); n != 3 {
		t.Fatalf("a LOST advance trimmed the log to %d", n)
	}
	if code, _, err := fresh.run(ctx, fresh.lua.advance, keys, "new", newToken, entries[1].ID); err != nil || code != "OK" {
		t.Fatalf("advance by the holder = %q, %v; want OK", code, err)
	}
	if code, _, err := fresh.run(ctx, fresh.lua.advance, keys, "new", newToken, entries[0].ID); err != nil || code != "NOOP" {
		t.Fatalf("advance backwards = %q, %v; want NOOP", code, err)
	}
	if n, _ := env.raw.XLen(ctx, keys[2]).Result(); n != 1 {
		t.Fatalf("log after advancing past two entries has %d, want 1", n)
	}
	if done, _ := env.raw.Get(ctx, keys[1]).Result(); done != entries[1].ID {
		t.Fatalf("progress = %s, want %s", done, entries[1].ID)
	}
	if err := env.raw.Del(ctx, keys[0]).Err(); err != nil {
		t.Fatal(err)
	}
	if err := env.raw.RPush(ctx, keys[0], "x").Err(); err != nil {
		t.Fatal(err)
	}
	// 键类型不对：脚本在第一条写之前返回 CORRUPT（业务结论），不是脚本运行时错误。
	if code, _, err := fresh.run(ctx, fresh.lua.advance, keys, "new", newToken, entries[2].ID); code != "CORRUPT" || err != nil {
		t.Fatalf("advance with a wrong-type lease key = %q, %v; want CORRUPT", code, err)
	}
	if code, _, err := fresh.run(ctx, fresh.lua.plock, []string{keys[0]}, "new", 1000); code != "CORRUPT" || err != nil {
		t.Fatalf("plock on a wrong-type key = %q, %v; want CORRUPT", code, err)
	}
}

// 两个运行器同时跑：分区靠租约分摊，每个条目至少投影一次，进度最终越过全部条目；MaxPartitions 限制一个进程持有的分区数。
func TestTwoRunnersShareThePartitions(t *testing.T) {
	env := newRunnerEnv(t)
	projection := &memoryProjection{prefix: env.prefix, partitions: 4, applied: map[int][]string{}, advanced: map[int]string{}}
	for partition := range 4 {
		env.append(t, partition, 50)
	}
	a := startRunner(t, env, Options{Owner: "a", BatchSize: 8, LeaseDuration: time.Second, Block: 20 * time.Millisecond, MaxPartitions: 2}, projection)
	startRunner(t, env, Options{Owner: "b", BatchSize: 8, LeaseDuration: time.Second, Block: 20 * time.Millisecond, MaxPartitions: 2}, projection)
	if a.Concurrency() != 2 {
		t.Fatalf("Concurrency with max_partitions 2 = %d", a.Concurrency())
	}
	for partition := range 4 {
		waitFor(t, fmt.Sprintf("partition %d", partition), func() bool { return len(projection.snapshot(partition)) >= 50 })
	}
	ctx := context.Background()
	owners := map[string]int{}
	for partition := range 4 {
		owner, err := env.raw.HGet(ctx, fmt.Sprintf("%s:plock:%d", env.prefix, partition), "owner").Result()
		if err != nil {
			t.Fatal(err)
		}
		owners[owner]++
	}
	if owners["a"] != 2 || owners["b"] != 2 {
		t.Fatalf("partition owners %v, want two each (max_partitions 2)", owners)
	}
}
