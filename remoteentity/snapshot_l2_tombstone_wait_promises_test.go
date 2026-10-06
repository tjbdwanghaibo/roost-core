package remoteentity

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/cache"
	rediscore "github.com/tjbdwanghaibo/roost-core/redis"
)

// O-M6-3（docs/feature/MIRROR-M6-OBSERVATIONS-2026-10-06.md §3）：L2 写墓碑之后在同一连接上 WAIT 副本确认。
//
// 承诺：只有带版本删除（墓碑脚本）走 WAIT，普通快照写入不变；WAIT 的结果（确认、不足、没有副本、出错、
// 不支持）只计数与记日志，DeleteAtVersion 的返回值仍只由脚本决定——墓碑已写在主节点上，不回滚、不把删除
// 变成结果未知；副本数为 0 时不调用新能力；设置非法（负副本数、要等副本却没有正超时）在构造时拒绝。
// 真实 Redis 上的切主红绿见 snapshot_l2_tombstone_wait_integration_test.go。

// replicatedRedisFake 在 snapshotRedisFake 上加同连接 WAIT 能力，结果由测试指定。
type replicatedRedisFake struct {
	*snapshotRedisFake
	outcome rediscore.ReplicatedEvalResult
	calls   []string // 每次 EvalReplicated 的脚本（只记是不是墓碑脚本）
	waitFor []int
}

func (f *replicatedRedisFake) EvalReplicated(ctx context.Context, script string, keys []string, numReplicas int, _ time.Duration, args ...any) (rediscore.ReplicatedEvalResult, error) {
	name := "other"
	if script == remoteSnapshotL2DeleteAtVersion {
		name = "tombstone"
	}
	f.calls = append(f.calls, name)
	f.waitFor = append(f.waitFor, numReplicas)
	result, err := f.snapshotRedisFake.Eval(ctx, script, keys, args...)
	if err != nil {
		return rediscore.ReplicatedEvalResult{}, err
	}
	outcome := f.outcome
	outcome.Result = result
	return outcome, nil
}

func tombstoneWaitStore(t *testing.T, redis remoteSnapshotRedis, replicas int) *remoteSnapshotL2Store {
	t.Helper()
	cfg := DefaultConfig()
	cfg.SnapshotL2TTL = time.Minute
	cfg.SnapshotL2TombstoneWaitReplicas = replicas
	store, err := NewSnapshotL2StoreFromConfig(redis, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func TestSnapshotL2TombstoneWaitIsObservedButNeverFailsTheDelete(t *testing.T) {
	ctx := context.Background()
	key := staleBackfillKey(t, b2WatermarkKind, 9910)
	for _, tc := range []struct {
		name    string
		outcome rediscore.ReplicatedEvalResult
		want    SnapshotL2TombstoneWaitStats
	}{
		{"confirmed", rediscore.ReplicatedEvalResult{Waited: true, Replicas: 1}, SnapshotL2TombstoneWaitStats{Confirmed: 1}},
		{"short", rediscore.ReplicatedEvalResult{Waited: true, Replicas: 0}, SnapshotL2TombstoneWaitStats{Short: 1}},
		{"no replicas", rediscore.ReplicatedEvalResult{Skipped: rediscore.ReplicatedSkipNoReplicas}, SnapshotL2TombstoneWaitStats{NoReplicas: 1}},
		{"wait error", rediscore.ReplicatedEvalResult{WaitErr: errors.New("EOF")}, SnapshotL2TombstoneWaitStats{Failed: 1}},
		{"redirected", rediscore.ReplicatedEvalResult{Skipped: rediscore.ReplicatedSkipRedirected}, SnapshotL2TombstoneWaitStats{Skipped: 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			redis := &replicatedRedisFake{snapshotRedisFake: newSnapshotRedisFake(), outcome: tc.outcome}
			store := tombstoneWaitStore(t, redis, 1)
			if err := store.Set(ctx, staleBackfillEnvelope(key, 3, 1, "v3")); err != nil {
				t.Fatal(err)
			}
			if len(redis.calls) != 0 {
				t.Fatalf("a snapshot write went through WAIT: %v", redis.calls)
			}
			if err := store.DeleteAtVersion(ctx, key, 4); err != nil {
				t.Fatalf("DeleteAtVersion = %v; the tombstone is on the primary whatever WAIT says", err)
			}
			if len(redis.calls) != 1 || redis.calls[0] != "tombstone" || redis.waitFor[0] != 1 {
				t.Fatalf("WAIT calls = %v for %v replicas, want one tombstone call for 1", redis.calls, redis.waitFor)
			}
			if got := store.TombstoneWaitStats(); got != tc.want {
				t.Fatalf("stats = %+v, want %+v", got, tc.want)
			}
			if _, found, err := store.Get(ctx, key); err != nil || found {
				t.Fatalf("after the delete: found=%v err=%v, want the tombstone", found, err)
			}
			// 被更新快照拒绝的删除仍报 ErrStaleWrite（脚本决定返回值）。
			if err := store.Set(ctx, staleBackfillEnvelope(key, 6, 1, "v6")); err != nil {
				t.Fatal(err)
			}
			if err := store.DeleteAtVersion(ctx, key, 5); !errors.Is(err, cache.ErrStaleWrite) {
				t.Fatalf("delete older than the stored snapshot = %v, want cache.ErrStaleWrite", err)
			}
		})
	}
}

func TestSnapshotL2TombstoneWaitDisabledAndUnsupported(t *testing.T) {
	ctx := context.Background()
	key := staleBackfillKey(t, b2WatermarkKind, 9911)
	// 副本数 0：不调用新能力。
	redis := &replicatedRedisFake{snapshotRedisFake: newSnapshotRedisFake()}
	store := tombstoneWaitStore(t, redis, 0)
	if err := store.DeleteAtVersion(ctx, key, 2); err != nil || len(redis.calls) != 0 {
		t.Fatalf("disabled: err=%v WAIT calls=%v", err, redis.calls)
	}
	if got := store.TombstoneWaitStats(); got != (SnapshotL2TombstoneWaitStats{}) {
		t.Fatalf("disabled counted %+v", got)
	}
	// 客户端没有同连接 WAIT：普通发送，计为 skipped。
	plain := tombstoneWaitStore(t, newSnapshotRedisFake(), 1)
	if err := plain.DeleteAtVersion(ctx, key, 2); err != nil {
		t.Fatal(err)
	}
	if got := plain.TombstoneWaitStats(); got != (SnapshotL2TombstoneWaitStats{Skipped: 1}) {
		t.Fatalf("unsupported client counted %+v, want one skipped", got)
	}
	// 旧构造不等（行为与修前相同）。
	legacy := NewSnapshotL2Store(redis, time.Minute)
	if err := legacy.DeleteAtVersion(ctx, key, 3); err != nil || len(redis.calls) != 0 {
		t.Fatalf("legacy constructor: err=%v WAIT calls=%v", err, redis.calls)
	}
}

func TestSnapshotL2TombstoneWaitSettingsAreValidated(t *testing.T) {
	for _, tc := range []struct {
		replicas int
		timeout  time.Duration
		ok       bool
	}{
		{1, 50 * time.Millisecond, true},
		{0, 0, true},
		{-1, time.Second, false},
		{1, 0, false},
		{1, time.Second, true},
		{1, 2 * time.Second, false},
	} {
		cfg := DefaultConfig()
		cfg.SnapshotL2TombstoneWaitReplicas, cfg.SnapshotL2TombstoneWaitTimeout = tc.replicas, tc.timeout
		_, err := NewSnapshotL2StoreFromConfig(newSnapshotRedisFake(), cfg)
		if (err == nil) != tc.ok {
			t.Errorf("replicas=%d timeout=%v: err=%v, want ok=%v", tc.replicas, tc.timeout, err, tc.ok)
		}
	}
	if _, err := Assemble(AssemblyDeps{Redis: refreshRedis{l2: newSnapshotRedisFake()}, Backend: &atomicTestBackend{newRemoteTestLoader()}},
		&Config{SnapshotL2TombstoneWaitReplicas: 1}, 7, MongoBackendConfig{}); err == nil {
		t.Error("Assemble accepted a tombstone wait without a timeout")
	}
}
