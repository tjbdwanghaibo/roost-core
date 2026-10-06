package remoteentity

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/entity"
	fsyncbus "github.com/tjbdwanghaibo/roost-core/sync/syncbus"
	"github.com/tjbdwanghaibo/roost-core/sync/syncbus/mirror"
)

// O-M6-1（docs/feature/MIRROR-M6-OBSERVATIONS-2026-10-06.md）：owner 以相同 sid 重启后兴趣表是空的。
//
// 承诺：owner 的 Assembly.Start 订阅确认后广播“请重新续租”，只读方收到后立即把本机仍有效的兴趣续租一次——
// 重启后的 owner 在请求往返内（这里的进程内总线是同步的，即 Start 返回时）重新知道兴趣、新提交直接推送，
// 不等只读方的下一个续租周期（剩余不足一半，缺省最长约 15s）。续租走与读时续租同一入口：代际、配额、
// 撤销水位都不绕过；已 release 的 key 不被复活；请求合并、有间隔；过期 / 自己的 / 身份不符的请求不触发。
// 旧行为（修前）：重启后的 owner 不推送，只读方的 Cached 读一直停在旧版本，直到本机租约过半衰期后的下一次
// 读才续租、推送恢复。
//
// 确定性：总线是进程内同步投递（loopbackBus），遍历执行器换成同步执行（startInterestRefresh），续租周期
// 由测试直接把本机租约推到半衰期之后来模拟，不用 sleep。kind 253 的 EntityID 用 99xx 段。

// refreshRedis 给 Assemble 一个能跑 L2 脚本的 Redis 替身（其余方法不会被调用）。
type refreshRedis struct {
	assembleRedis
	l2 *snapshotRedisFake
}

func (r refreshRedis) HGet(ctx context.Context, key, field string) ([]byte, error) {
	return r.l2.HGet(ctx, key, field)
}

func (r refreshRedis) Eval(ctx context.Context, script string, keys []string, args ...any) (any, error) {
	return r.l2.Eval(ctx, script, keys, args...)
}

func (r refreshRedis) Del(ctx context.Context, keys ...string) (int64, error) {
	return r.l2.Del(ctx, keys...)
}

func newRefreshAssembly(t *testing.T, cfg *Config, sid int32) *Assembly {
	t.Helper()
	a, err := Assemble(AssemblyDeps{Redis: refreshRedis{l2: newSnapshotRedisFake()}, Backend: &atomicTestBackend{newRemoteTestLoader()}}, cfg, sid, MongoBackendConfig{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := a.Stop(ctx); err != nil {
			t.Errorf("stop owner %d: %v", sid, err)
		}
	})
	return a
}

// syncInterestRefresh 让续租遍历同步执行，间隔为 0；结束时恢复。
func syncInterestRefresh(t *testing.T) {
	t.Helper()
	previousStart, previousGap := startInterestRefresh, interestRefreshMinGap
	startInterestRefresh = func(run func()) { run() }
	interestRefreshMinGap = 0
	t.Cleanup(func() { startInterestRefresh, interestRefreshMinGap = previousStart, previousGap })
}

func newRefreshReader(t *testing.T, cfg *Config, sid int32, bus fsyncbus.ISyncBus) *SnapshotClient {
	t.Helper()
	reader, err := NewSnapshotClient(cfg, SnapshotClientDeps{ConsumerSID: sid})
	if err != nil {
		t.Fatal(err)
	}
	if err := reader.Start(bus); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reader.Stop(context.Background()) })
	return reader
}

func cachedVersion(t *testing.T, reader *SnapshotClient, key entity.RemoteSnapshotKey) uint64 {
	t.Helper()
	got, found, err := reader.ReadSnapshot(context.Background(), entity.RemoteSnapshotRead{Key: key, Consistency: entity.RemoteReadCached})
	if err != nil {
		t.Fatalf("cached read: %v", err)
	}
	if !found {
		return 0
	}
	return got.StateVersion
}

// 主用例（先红后绿）：owner 同 sid 重启，Start 返回后的第一笔提交就推送到只读方。
func TestOwnerRestartWithTheSameSidGetsInterestBackWithoutWaitingForRenewal(t *testing.T) {
	syncInterestRefresh(t)
	ctx := context.Background()
	bus := newLoopbackBus()
	cfg := DefaultConfig() // 兴趣 TTL 30s：只读方读时续租要等剩余不足 15s
	key := staleBackfillKey(t, b2WatermarkKind, 9901)

	first := newRefreshAssembly(t, cfg, 2601)
	if err := first.Start(ctx, bus); err != nil {
		t.Fatal(err)
	}
	reader := newRefreshReader(t, cfg, 2602, bus)
	if v := cachedVersion(t, reader, key); v != 0 {
		t.Fatalf("first read found v%d before any publish", v)
	}
	if !first.Manager.snapshots.interests.interested(key) {
		t.Fatal("the reader's interest did not reach the owner")
	}
	if err := first.Manager.snapshots.publishCommitted(ctx, clientCommit(key, 1, "v1")); err != nil {
		t.Fatal(err)
	}
	if v := cachedVersion(t, reader, key); v != 1 {
		t.Fatalf("reader read v%d after the owner pushed v1", v)
	}

	// owner 重启：同一 sid，新进程（新 Assembly），兴趣表从空开始。
	if err := first.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	restarted := newRefreshAssembly(t, cfg, 2601)
	if err := restarted.Start(ctx, bus); err != nil {
		t.Fatal(err)
	}
	if err := restarted.Manager.snapshots.publishCommitted(ctx, clientCommit(key, 2, "v2")); err != nil {
		t.Fatal(err)
	}
	if v := cachedVersion(t, reader, key); v != 2 {
		t.Errorf("after the owner restarted with the same sid the reader still reads v%d, want v2 pushed right away: "+
			"the restarted owner's interest table is empty (interested=%v) and the reader renews only past half its lease",
			v, restarted.Manager.snapshots.interests.interested(key))
	}

	// 对照：下一个续租周期（本机租约过了半衰期）之后的读一定续租，推送恢复——修前只能等到这里。
	reader.localInterestMu.Lock()
	reader.localInterests[key] = time.Now().Add(time.Millisecond).UnixNano()
	reader.localInterestMu.Unlock()
	_ = cachedVersion(t, reader, key)
	if !restarted.Manager.snapshots.interests.interested(key) {
		t.Fatal("the regular renewal did not reach the restarted owner either")
	}
	if err := restarted.Manager.snapshots.publishCommitted(ctx, clientCommit(key, 3, "v3")); err != nil {
		t.Fatal(err)
	}
	if v := cachedVersion(t, reader, key); v != 3 {
		t.Fatalf("after the regular renewal the reader reads v%d, want v3", v)
	}
}

// 遍历不复活已 release 的 key，也不续过期的 key；续租带新的代际。
func TestInterestRefreshRenewsOnlyLiveInterests(t *testing.T) {
	syncInterestRefresh(t)
	ctx := context.Background()
	bus := newLoopbackBus()
	cfg := DefaultConfig()
	live, released, expired := staleBackfillKey(t, b2WatermarkKind, 9902), staleBackfillKey(t, b2WatermarkKind, 9903), staleBackfillKey(t, b2WatermarkKind, 9904)

	first := newRefreshAssembly(t, cfg, 2611)
	if err := first.Start(ctx, bus); err != nil {
		t.Fatal(err)
	}
	reader := newRefreshReader(t, cfg, 2612, bus)
	for _, key := range []entity.RemoteSnapshotKey{live, released, expired} {
		_ = cachedVersion(t, reader, key)
	}
	if err := reader.ReleaseInterest(ctx, released); err != nil {
		t.Fatal(err)
	}
	reader.localInterestMu.Lock()
	reader.localInterests[expired] = time.Now().Add(-time.Millisecond).UnixNano()
	reader.localInterestMu.Unlock()
	before := reader.interestGeneration.Load()

	if err := first.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	restarted := newRefreshAssembly(t, cfg, 2611)
	if err := restarted.Start(ctx, bus); err != nil {
		t.Fatal(err)
	}
	table := restarted.Manager.snapshots.interests
	if !table.interested(live) {
		t.Error("the live interest was not renewed to the restarted owner")
	}
	if table.interested(released) {
		t.Error("the refresh brought back an interest the reader had released")
	}
	if table.interested(expired) {
		t.Error("the refresh renewed an interest whose local lease had expired")
	}
	if got := reader.interestGeneration.Load(); got <= before {
		t.Errorf("refresh renewals reused generation %d (before %d); they must take new ones", got, before)
	}
	// 直接调用：本机表里没有的 key 不续租、不广播。
	if published, err := reader.renewInterest(ctx, released, true); published || err != nil {
		t.Errorf("refresh renewal of a released key: published=%v err=%v", published, err)
	}
}

// refreshEnvelope 造一条请求信封。
func refreshEnvelope(t *testing.T, sid int32, at int64) mirror.Envelope {
	t.Helper()
	raw, err := json.Marshal(remoteInterestRefreshWire{RequesterSID: sid, RequestedAt: at})
	if err != nil {
		t.Fatal(err)
	}
	return mirror.Envelope{Topic: SyncTopicInterestRefresh, Key: remoteInterestRefreshReplicaKey(sid), Version: at, Op: mirror.OpUpsert, Payload: raw}
}

// 有界：遍历进行中到达的请求只合并成之后的一次遍历；过期的、自己的请求不触发，身份不符的被拒。
func TestInterestRefreshRequestsCoalesceAndAreValidated(t *testing.T) {
	previousStart, previousGap := startInterestRefresh, interestRefreshMinGap
	t.Cleanup(func() { startInterestRefresh, interestRefreshMinGap = previousStart, previousGap })
	interestRefreshMinGap = 0
	var runs []func()
	startInterestRefresh = func(run func()) { runs = append(runs, run) }

	ctx := context.Background()
	bus := newLoopbackBus()
	cfg := DefaultConfig()
	reader := newRefreshReader(t, cfg, 2622, bus)
	keys := []entity.RemoteSnapshotKey{staleBackfillKey(t, b2WatermarkKind, 9905), staleBackfillKey(t, b2WatermarkKind, 9906)}
	for _, key := range keys {
		_ = cachedVersion(t, reader, key)
	}
	store := InterestRefreshStore{client: reader}
	var mu sync.Mutex
	renewals := 0
	// 第一次遍历广播第一条续租时再到达 3 个请求：它们合并成之后的一次遍历。
	unsub, err := bus.Subscribe(SyncTopicInterest, func(*fsyncbus.SyncMsg) error {
		mu.Lock()
		renewals++
		first := renewals == 1
		mu.Unlock()
		if first {
			for i := range 3 {
				if err := store.ApplyReplica(ctx, refreshEnvelope(t, 2623, time.Now().UnixNano()+int64(i))); err != nil {
					t.Error(err)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer unsub()
	now := time.Now().UnixNano()
	for i := range 5 {
		if err := store.ApplyReplica(ctx, refreshEnvelope(t, 2621, now+int64(i))); err != nil {
			t.Fatal(err)
		}
	}
	if len(runs) != 1 {
		t.Fatalf("5 requests started %d refresh runs, want 1 (the rest coalesce)", len(runs))
	}
	// 遍历开始之前到达的 4 个请求被这次遍历覆盖；遍历中途到达的 3 个合并成之后的一次：共两次遍历。
	runs[0]()
	if want := 2 * len(keys); renewals != want {
		t.Fatalf("one run with requests arriving mid-pass published %d renewals, want %d (two passes)", renewals, want)
	}
	// 跑完之后的新请求开新的一次。
	if err := store.ApplyReplica(ctx, refreshEnvelope(t, 2621, time.Now().UnixNano())); err != nil || len(runs) != 2 {
		t.Fatalf("a request after the run: err=%v runs=%d, want a new run", err, len(runs))
	}
	runs[1]()

	// 过期的、自己的请求不触发；身份不符的被拒。
	for _, env := range []mirror.Envelope{
		refreshEnvelope(t, 2621, time.Now().Add(-2*cfg.SnapshotInterestTTL).UnixNano()),
		refreshEnvelope(t, 2622, time.Now().UnixNano()),
	} {
		if err := store.ApplyReplica(ctx, env); err != nil {
			t.Fatal(err)
		}
	}
	if len(runs) != 2 {
		t.Fatalf("a historic or own request started a run (runs=%d)", len(runs))
	}
	bad := refreshEnvelope(t, 2621, time.Now().UnixNano())
	bad.Key = remoteInterestRefreshReplicaKey(2699)
	if err := store.ApplyReplica(ctx, bad); err == nil {
		t.Fatal("a request whose envelope key does not match its requester was accepted")
	}
	bad = refreshEnvelope(t, 2621, time.Now().UnixNano())
	bad.Version++
	if err := store.ApplyReplica(ctx, bad); err == nil {
		t.Fatal("a request whose envelope version does not match its time was accepted")
	}
}

// 间隔与停机：上一次遍历刚开始过，新的遍历在间隔里等；Stop 取消等待并等它退出（不挂住）。
func TestInterestRefreshGapWaitEndsOnStop(t *testing.T) {
	previousStart, previousGap := startInterestRefresh, interestRefreshMinGap
	t.Cleanup(func() { startInterestRefresh, interestRefreshMinGap = previousStart, previousGap })
	interestRefreshMinGap = time.Hour
	done := make(chan struct{})
	startInterestRefresh = func(run func()) { go func() { defer close(done); run() }() }

	reader, err := NewSnapshotClient(DefaultConfig(), SnapshotClientDeps{ConsumerSID: 2632})
	if err != nil {
		t.Fatal(err)
	}
	if err := reader.Start(newLoopbackBus()); err != nil {
		t.Fatal(err)
	}
	reader.refreshMu.Lock()
	reader.refreshLastStart = time.Now()
	reader.refreshMu.Unlock()
	if got := reader.acceptInterestRefresh(); got != "accepted" {
		t.Fatalf("accept = %q", got)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := reader.Stop(ctx); err != nil {
		t.Fatalf("Stop with a refresh waiting on its gap: %v", err)
	}
	select {
	case <-done:
	default:
		t.Fatal("Stop returned while the refresh run was still running")
	}
	if got := reader.acceptInterestRefresh(); got != "stopped" {
		t.Fatalf("accept after Stop = %q, want stopped", got)
	}
}

// 混跑与退化：推送关着（普通 NATS）时只读方不订阅请求主题、owner 不发请求；没有任何订阅者（旧只读方）时
// owner 照常启动。
func TestInterestRefreshNeedsPushAndToleratesOldConsumers(t *testing.T) {
	ctx := context.Background()
	loop := newLoopbackBus()
	var mu sync.Mutex
	requests := 0
	count := func(*fsyncbus.SyncMsg) error { mu.Lock(); requests++; mu.Unlock(); return nil }
	cfg := DefaultConfig()

	plainReader := newRefreshReader(t, cfg, 2642, plainBus{inner: loop})
	if plainReader.Stats().PushEnabled || loop.active(SyncTopicInterestRefresh) != 0 {
		t.Fatalf("without push: refresh subscriptions=%d", loop.active(SyncTopicInterestRefresh))
	}
	unsub, err := loop.Subscribe(SyncTopicInterestRefresh, count)
	if err != nil {
		t.Fatal(err)
	}
	plainOwner := newRefreshAssembly(t, cfg, 2641)
	if err := plainOwner.Start(ctx, plainBus{inner: loop}); err != nil {
		t.Fatal(err)
	}
	if requests != 0 {
		t.Fatalf("an owner without push sent %d refresh requests", requests)
	}
	unsub()

	// 没有订阅者（旧版本只读方不订阅这个主题）：请求无人收，owner 启动不受影响。
	other := newLoopbackBus()
	owner := newRefreshAssembly(t, cfg, 2643)
	if err := owner.Start(ctx, other); err != nil {
		t.Fatalf("owner start with no refresh subscriber: %v", err)
	}
	if other.active(SyncTopicInterestRefresh) != 1 {
		t.Fatalf("the owner's own client should subscribe once (it is a consumer too): %d", other.active(SyncTopicInterestRefresh))
	}
}

// 启动失败逐步回收：请求主题的订阅失败时，快照与兴趣两个订阅已退掉；重试后三个主题各恰好一个订阅，Stop 后为零。
func TestSnapshotClientRefreshSubscriptionFailureLeavesNoSubscription(t *testing.T) {
	bus := newLoopbackBus()
	bus.failSubscribe = SyncTopicInterestRefresh
	client, err := NewSnapshotClient(nil, SnapshotClientDeps{ConsumerSID: 2651})
	if err != nil {
		t.Fatal(err)
	}
	topics := []string{SyncTopicSnapshot, SyncTopicInterest, SyncTopicInterestRefresh}
	counts := func() []int {
		out := make([]int, len(topics))
		for i, topic := range topics {
			out[i] = bus.active(topic)
		}
		return out
	}
	if err := client.Start(bus); err == nil {
		t.Fatal("Start succeeded although the refresh subscription failed")
	}
	if got := counts(); got[0] != 0 || got[1] != 0 || got[2] != 0 {
		t.Fatalf("a failed Start left subscriptions %v", got)
	}
	for range 2 {
		if err := client.Start(bus); err != nil {
			t.Fatalf("retry: %v", err)
		}
	}
	if got := counts(); got[0] != 1 || got[1] != 1 || got[2] != 1 {
		t.Fatalf("after a retry and a repeated Start: %v, want one per topic", got)
	}
	if err := client.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := counts(); got[0] != 0 || got[1] != 0 || got[2] != 0 {
		t.Fatalf("after Stop: %v", got)
	}
}
