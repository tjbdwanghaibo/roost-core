package remoteentity

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/tjbdwanghaibo/roost-core/entity"
	fsyncbus "github.com/tjbdwanghaibo/roost-core/sync/syncbus"
	"github.com/tjbdwanghaibo/roost-core/sync/syncbus/mirror"
)

// Mirror 第 4 步与 O4（docs/feature/MIRROR-STEP-4-AND-O4-2026-10-06.md）。
//
// 承诺：
//   - 首载缓冲：一个 key 的权威加载在途时，它的复制消息先缓冲，加载结果装入后再按到达顺序经同一准入重放；
//     增量消息不因“缺基”再触发一次权威加载，也不被丢掉。
//   - 兴趣代际：owner 的兴趣表里，release 之后迟到的更旧 renew 不能把租约复活（renew / release 全交错）。
//   - O4：兴趣容量按 consumer 计：一个 consumer 占满不会让别的 consumer 的兴趣被拒。
//
// kind 253 与 B2 用例共用（同一注册定义），EntityID 用 98xx 段。

// step4DeltaSchema 是本文件增量用例的 schema。增量解码表是进程级的，只登记一次（-count>1 安全）；
// 本包其他用例不用这个 schema。
const step4DeltaSchema uint32 = 41

var registerStep4Delta sync.Once

func step4RegisterDelta(t *testing.T) {
	t.Helper()
	registerStep4Delta.Do(func() {
		if err := entity.RegisterRemoteSnapshotDelta(step4DeltaSchema, func(base, delta []byte) ([]byte, error) {
			return append(base, delta...), nil
		}); err != nil {
			t.Fatal(err)
		}
	})
}

func step4Full(key entity.RemoteSnapshotKey, version uint64, payload string) entity.RemoteSnapshotEnvelope {
	data := []byte(payload)
	return entity.RemoteSnapshotEnvelope{
		Key: key, StateVersion: version, BaseVersion: version - 1, MarkerEpoch: 1, RouteEpoch: 1,
		Schema: step4DeltaSchema, Codec: 1, Full: true, Checksum: entity.RemoteSnapshotChecksum(data),
		Payload: entity.CopyFrozenRemoteSnapshotPayload(data),
	}
}

func step4DeltaReplica(t *testing.T, key entity.RemoteSnapshotKey, base, version uint64, delta string) mirror.Envelope {
	t.Helper()
	data := []byte(delta)
	record := entity.RemoteSnapshotRecord{
		Key: key, StateVersion: version, BaseVersion: base, MarkerEpoch: 1, RouteEpoch: 1,
		Schema: step4DeltaSchema, Codec: 1, Full: false, Data: data, Checksum: entity.RemoteSnapshotChecksum(data),
	}
	raw, err := json.Marshal(remoteSnapshotWire{Key: key, Update: record, PublishedAt: time.Now().UnixNano()})
	if err != nil {
		t.Fatal(err)
	}
	return mirror.Envelope{Key: remoteSnapshotReplicaKey(key), Version: int64(version), Op: mirror.OpUpsert, Payload: raw}
}

// 修前：读者的首次权威加载在途时到达一条增量（基 = 权威上的 v5）。复制接收在 L1 找不到基，返回缺基，
// 再直接回源一次（不经合并），装上 v5 后增量被丢掉；读者的加载随后也装上 v5。权威被读两次，L1 停在 v5。
// 承诺：增量进首载缓冲，加载装上 v5 之后重放，L1 是 v6；权威只读一次。
func TestSnapshotBootstrapBuffersDeltaDuringFirstLoad(t *testing.T) {
	step4RegisterDelta(t)
	ctx := context.Background()
	key := staleBackfillKey(t, b2WatermarkKind, 9801)
	var loads atomic.Int32
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	client, err := NewSnapshotClient(nil, SnapshotClientDeps{ConsumerSID: 2401,
		Loader: func(context.Context, entity.RemoteSnapshotKey, entity.RemoteReadConsistency, uint64) (entity.RemoteSnapshotEnvelope, bool, error) {
			if loads.Add(1) == 1 {
				entered <- struct{}{}
				<-release
			}
			return step4Full(key, 5, "base"), true, nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	read := make(chan error, 1)
	go func() {
		_, _, err := client.ReadSnapshot(ctx, entity.RemoteSnapshotRead{Key: key, Consistency: entity.RemoteReadMonotonic})
		read <- err
	}()
	<-entered
	// 复制接收在加载在途时到达；等它返回（修前它自己回源一次，修后进缓冲）再放行读者的加载。
	replicaErr := (SnapshotReplicaStore{client: client}).ApplyReplica(ctx, step4DeltaReplica(t, key, 5, 6, "+delta"))
	close(release)
	if err := <-read; err != nil {
		t.Fatal(err)
	}
	got, found, err := client.ReadSnapshot(ctx, entity.RemoteSnapshotRead{Key: key, Consistency: entity.RemoteReadCached})
	if replicaErr != nil || err != nil || !found || got.StateVersion != 6 || string(got.Payload.BytesCopy()) != "base+delta" || loads.Load() != 1 {
		t.Fatalf("after the first load: version=%d payload=%q found=%v err=%v authority loads=%d replica err=%v; want v6 \"base+delta\" from one load (the delta buffered and replayed on top of the load)",
			got.StateVersion, got.Payload.BytesCopy(), found, err, loads.Load(), replicaErr)
	}
}

// 修前：release 只撤销已登记的租约；release 先到、更旧的 renew 后到时，renew 建起一份新租约，consumer 已
// 撤销的兴趣在 owner 处复活到租期结束（owner 继续为它发布）。
// 承诺：owner 的兴趣表按 generation 收敛到最后一次操作的意图，与投递顺序无关。
func TestInterestRenewReleaseConvergesInEveryDeliveryOrder(t *testing.T) {
	key := staleBackfillKey(t, b2WatermarkKind, 9802)
	const consumer int32 = 2402
	type op struct {
		release    bool
		generation uint64
	}
	for _, tc := range []struct {
		name string
		ops  []op // 按 consumer 发出的顺序（generation 递增）
		want bool // 最后一次操作的意图
	}{
		{"renew then release", []op{{false, 1}, {true, 2}}, false},
		{"release then renew", []op{{true, 1}, {false, 2}}, true},
		{"renew release renew", []op{{false, 1}, {true, 2}, {false, 3}}, true},
		{"renew renew release", []op{{false, 1}, {false, 2}, {true, 3}}, false},
		{"release renew release", []op{{true, 1}, {false, 2}, {true, 3}}, false},
	} {
		for _, order := range permutations(len(tc.ops)) {
			owner, err := NewSnapshotClient(nil, SnapshotClientDeps{ConsumerSID: 2403})
			if err != nil {
				t.Fatal(err)
			}
			for _, i := range order {
				o := tc.ops[i]
				if o.release {
					owner.interests.release(key, consumer, o.generation)
					continue
				}
				_ = owner.interests.renew(entity.RemoteSnapshotInterest{ConsumerSID: consumer, Key: key,
					ExpiresAt: time.Now().Add(time.Minute).UnixNano(), Generation: o.generation})
			}
			if got := owner.interests.interested(key); got != tc.want {
				t.Errorf("%s delivered in order %v: interested=%v, want %v (the last operation issued decides)", tc.name, order, got, tc.want)
			}
		}
	}
}

func permutations(n int) [][]int {
	if n == 1 {
		return [][]int{{0}}
	}
	var out [][]int
	for _, p := range permutations(n - 1) {
		for at := 0; at <= len(p); at++ {
			next := append(append(append([]int(nil), p[:at]...), n-1), p[at:]...)
			out = append(out, next)
		}
	}
	return out
}

// O4 修前：兴趣表按全集群合计限额（每个节点存所有 consumer 的租约）。consumer A 读了足够多的 key 占满
// 全表之后，consumer B 的第一份兴趣在 owner 处被拒（只在 owner 记一条日志），B 的读路径吞掉续租错误，
// 推送停止而 B 不知道。
// 承诺：每个 consumer 各有配额；A 占满自己的配额不影响 B。
func TestInterestCapacityIsPerConsumer(t *testing.T) {
	ctx := context.Background()
	bus := newLoopbackBus()
	cfg := DefaultConfig()
	cfg.SnapshotInterestSubs = 16
	newClient := func(sid int32) *SnapshotClient {
		client, err := NewSnapshotClient(cfg, SnapshotClientDeps{ConsumerSID: sid})
		if err != nil {
			t.Fatal(err)
		}
		if err := client.Start(bus); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = client.Stop(context.Background()) })
		return client
	}
	owner, a, b := newClient(2404), newClient(2405), newClient(2406)
	for i := range int64(cfg.SnapshotInterestSubs) {
		_ = a.RenewInterest(ctx, staleBackfillKey(t, b2WatermarkKind, 9810+i))
	}
	keyB := staleBackfillKey(t, b2WatermarkKind, 9809)
	_ = b.RenewInterest(ctx, keyB)
	if !owner.interests.interested(keyB) {
		t.Fatal("consumer B's interest was refused at the owner because consumer A filled the cluster-wide table; capacity must be counted per consumer")
	}
}

func step4DeleteReplica(t *testing.T, key entity.RemoteSnapshotKey, version uint64) mirror.Envelope {
	t.Helper()
	raw, err := json.Marshal(remoteSnapshotWire{Delete: true, Key: key, PublishedAt: time.Now().UnixNano()})
	if err != nil {
		t.Fatal(err)
	}
	return mirror.Envelope{Key: remoteSnapshotReplicaKey(key), Version: int64(version), Op: mirror.OpUpsert, Payload: raw}
}

// step4BlockedLoad 起一次 Monotonic 读，让它的权威加载停在 loader 里，交回放行函数与读的结果。
func step4BlockedLoad(t *testing.T, client *SnapshotClient, key entity.RemoteSnapshotKey, entered <-chan struct{}) <-chan error {
	t.Helper()
	read := make(chan error, 1)
	go func() {
		_, _, err := client.ReadSnapshot(context.Background(), entity.RemoteSnapshotRead{Key: key, Consistency: entity.RemoteReadMonotonic})
		read <- err
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the read never reached the loader")
	}
	return read
}

// 首载缓冲的重放矩阵（方案第 4 行：旧删除 / 旧 upsert / 重建）。权威加载返回 v6，加载在途时到达的复制
// 消息按到达顺序缓冲，装入 v6 之后经同一准入重放：更旧的 upsert 与删除是过去；更新的删除生效；删除后
// 重建（不论到达顺序）留下重建的版本。权威只读一次。
func TestSnapshotBootstrapReplayMatrix(t *testing.T) {
	ctx := context.Background()
	type msg struct {
		delete  bool
		version uint64
	}
	for i, tc := range []struct {
		name      string
		arrivals  []msg
		wantFound bool
		want      uint64
	}{
		{"old upsert", []msg{{false, 4}}, true, 6},
		{"old delete", []msg{{true, 5}}, true, 6},
		{"same-version delete", []msg{{true, 6}}, false, 0},
		{"newer delete", []msg{{true, 7}}, false, 0},
		{"delete then recreate", []msg{{true, 7}, {false, 8}}, true, 8},
		{"recreate overtakes its delete", []msg{{false, 8}, {true, 7}}, true, 8},
		{"newer upsert then an older one", []msg{{false, 9}, {false, 7}}, true, 9},
	} {
		t.Run(tc.name, func(t *testing.T) {
			key := staleBackfillKey(t, b2WatermarkKind, 9830+int64(i))
			var loads atomic.Int32
			entered := make(chan struct{}, 1)
			release := make(chan struct{})
			client, err := NewSnapshotClient(nil, SnapshotClientDeps{ConsumerSID: 2410,
				Loader: func(context.Context, entity.RemoteSnapshotKey, entity.RemoteReadConsistency, uint64) (entity.RemoteSnapshotEnvelope, bool, error) {
					if loads.Add(1) == 1 {
						entered <- struct{}{}
						<-release
					}
					return staleBackfillEnvelope(key, 6, 1, "v6"), true, nil
				}})
			if err != nil {
				t.Fatal(err)
			}
			read := step4BlockedLoad(t, client, key, entered)
			store := SnapshotReplicaStore{client: client}
			for _, m := range tc.arrivals {
				env := staleBackfillReplica(t, key, m.version, 1, fmt.Sprintf("v%d", m.version))
				if m.delete {
					env = step4DeleteReplica(t, key, m.version)
				}
				if err := store.ApplyReplica(ctx, env); err != nil {
					t.Fatalf("replica %+v during the load: %v", m, err)
				}
			}
			close(release)
			if err := <-read; err != nil {
				t.Fatal(err)
			}
			got, found, err := client.ReadSnapshot(ctx, entity.RemoteSnapshotRead{Key: key, Consistency: entity.RemoteReadCached})
			if err != nil || found != tc.wantFound || (found && got.StateVersion != tc.want) || loads.Load() != 1 {
				t.Fatalf("after replay: found=%v version=%d err=%v loads=%d; want found=%v version=%d from one load",
					found, got.StateVersion, err, loads.Load(), tc.wantFound, tc.want)
			}
			stats := client.Stats().Bootstrap
			if stats.Buffered != uint64(len(tc.arrivals)) || stats.Replayed+stats.ReplayFailed != stats.Buffered {
				t.Fatalf("bootstrap stats %+v; want %d buffered and all of them replayed", stats, len(tc.arrivals))
			}
		})
	}
}

// 缓冲溢出的明确行为：丢弃缓冲（缺了中间消息，部分重放可能装上错位的增量），计数并记日志，加载装入后
// 整体回源一次；回源之后到达的消息直接准入。
func TestSnapshotBootstrapOverflowDropsTheBufferAndReloads(t *testing.T) {
	ctx := context.Background()
	key := staleBackfillKey(t, b2WatermarkKind, 9840)
	cfg := DefaultConfig()
	cfg.SnapshotReplicaBuffer = 2
	var loads atomic.Int32
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	client, err := NewSnapshotClient(cfg, SnapshotClientDeps{ConsumerSID: 2411,
		Loader: func(context.Context, entity.RemoteSnapshotKey, entity.RemoteReadConsistency, uint64) (entity.RemoteSnapshotEnvelope, bool, error) {
			if loads.Add(1) == 1 {
				entered <- struct{}{}
				<-release
				return staleBackfillEnvelope(key, 5, 1, "v5"), true, nil
			}
			return staleBackfillEnvelope(key, 9, 1, "v9"), true, nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	read := step4BlockedLoad(t, client, key, entered)
	store := SnapshotReplicaStore{client: client}
	for v := uint64(6); v <= 8; v++ {
		if err := store.ApplyReplica(ctx, staleBackfillReplica(t, key, v, 1, fmt.Sprintf("v%d", v))); err != nil {
			t.Fatal(err)
		}
	}
	close(release)
	if err := <-read; err != nil {
		t.Fatal(err)
	}
	got, found, err := client.ReadSnapshot(ctx, entity.RemoteSnapshotRead{Key: key, Consistency: entity.RemoteReadCached})
	stats := client.Stats().Bootstrap
	if err != nil || !found || got.StateVersion != 9 || loads.Load() != 2 || stats.Overflows != 1 || stats.Reloads != 1 || stats.Buffered != 2 || stats.Replayed != 0 {
		t.Fatalf("after an overflow: version=%d found=%v err=%v loads=%d stats=%+v; want v9 from one reload, nothing replayed",
			got.StateVersion, found, err, loads.Load(), stats)
	}
	// 首载结束后到达的消息直接准入。
	if err := store.ApplyReplica(ctx, staleBackfillReplica(t, key, 10, 1, "v10")); err != nil {
		t.Fatal(err)
	}
	if got, found, err := client.ReadSnapshot(ctx, entity.RemoteSnapshotRead{Key: key, Consistency: entity.RemoteReadCached}); err != nil || !found || got.StateVersion != 10 {
		t.Fatalf("after the bootstrap: version=%d found=%v err=%v", got.StateVersion, found, err)
	}
}

// O4 修后：超出配额是可识别的错误，consumer 在本地就拿到（本机兴趣表按同一配额判定），计入 Stats；
// 读取照常经权威回源（按需读取）；owner 不为超出配额的 key 登记兴趣，配额内的照旧。
func TestInterestQuotaRefusalIsVisibleAndReadsGoOnDemand(t *testing.T) {
	ctx := context.Background()
	bus := newLoopbackBus()
	cfg := DefaultConfig()
	cfg.SnapshotInterestSubs = 16
	cfg.SnapshotInterestPerConsumer = 1
	owner, err := NewSnapshotClient(cfg, SnapshotClientDeps{ConsumerSID: 2412})
	if err != nil {
		t.Fatal(err)
	}
	consumer, err := NewSnapshotClient(cfg, SnapshotClientDeps{ConsumerSID: 2413,
		Loader: func(_ context.Context, k entity.RemoteSnapshotKey, _ entity.RemoteReadConsistency, _ uint64) (entity.RemoteSnapshotEnvelope, bool, error) {
			return staleBackfillEnvelope(k, 3, 1, "v3"), true, nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []*SnapshotClient{owner, consumer} {
		if err := c.Start(bus); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = c.Stop(context.Background()) })
	}
	within, over := staleBackfillKey(t, b2WatermarkKind, 9841), staleBackfillKey(t, b2WatermarkKind, 9842)
	for _, key := range []entity.RemoteSnapshotKey{within, over} {
		if got, found, err := consumer.ReadSnapshot(ctx, entity.RemoteSnapshotRead{Key: key, Consistency: entity.RemoteReadMonotonic}); err != nil || !found || got.StateVersion != 3 {
			t.Fatalf("read %v: version=%d found=%v err=%v; the read must not depend on the interest", key.EntityID, got.StateVersion, found, err)
		}
	}
	if err := consumer.RenewInterest(ctx, over); !errors.Is(err, ErrInterestQuotaExceeded) {
		t.Fatalf("renewal over the quota = %v, want ErrInterestQuotaExceeded", err)
	}
	if got := consumer.Stats().InterestRejected; got < 2 {
		t.Fatalf("InterestRejected=%d; the refused renewals were not counted", got)
	}
	if !owner.interests.interested(within) || owner.interests.interested(over) {
		t.Fatalf("owner interest within=%v over=%v; want only the key within the quota", owner.interests.interested(within), owner.interests.interested(over))
	}
	if cfg.SnapshotInterestPerConsumer = 17; true {
		if _, err := NewSnapshotClient(cfg, SnapshotClientDeps{ConsumerSID: 2414}); err == nil {
			t.Fatal("a per-consumer quota above snapshot_interest_subs was accepted")
		}
	}
}

// plainBus 去掉 SubscribeLive：像普通 NATS 一样不能确认订阅。
type plainBus struct{ inner *loopbackBus }

func (b plainBus) Publish(msg *fsyncbus.SyncMsg) error { return b.inner.Publish(msg) }
func (b plainBus) Subscribe(topic string, h fsyncbus.Handler) (*fsyncbus.Subscription, error) {
	return b.inner.Subscribe(topic, h)
}

// 推送依赖可确认订阅：总线不能确认时显式退化——不订阅快照主题、记 Warn、Stats().PushEnabled=false；
// 读取按陈旧上限经共享 L2 回源，看到 owner 的新提交而不需要推送。
func TestSnapshotClientWithoutConfirmedSubscriptionsReadsOnDemand(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		var logs bytes.Buffer
		previous := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
		t.Cleanup(func() { slog.SetDefault(previous) })

		redis := newSnapshotRedisFake()
		cfg := DefaultConfig()
		cfg.CachedMaxStaleness = 10 * time.Millisecond // RR-20261008-41：用虚拟时间跨过陈旧上限，不猜测真实时钟的分辨率。
		owner := NewManager(newMockVersionedLockFactory(), cfg, 2415, NewSnapshotL2Store(redis, cfg.SnapshotL2TTL))
		loop := newLoopbackBus()
		consumer, err := NewSnapshotClient(cfg, SnapshotClientDeps{ConsumerSID: 2416, L2: NewSnapshotL2Store(redis, cfg.SnapshotL2TTL)})
		if err != nil {
			t.Fatal(err)
		}
		if err := consumer.Start(plainBus{inner: loop}); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = consumer.Stop(context.Background()) })
		if consumer.Stats().PushEnabled || loop.active(SyncTopicSnapshot) != 0 || loop.active(SyncTopicInterest) != 1 {
			t.Fatalf("push=%v snapshot subscriptions=%d interest subscriptions=%d; want push off, no snapshot subscription, the interest one kept",
				consumer.Stats().PushEnabled, loop.active(SyncTopicSnapshot), loop.active(SyncTopicInterest))
		}
		if !strings.Contains(logs.String(), "snapshot push disabled") {
			t.Fatalf("the degraded mode was not logged: %q", logs.String())
		}
		key := staleBackfillKey(t, b2WatermarkKind, 9843)
		for v := uint64(1); v <= 2; v++ {
			if err := owner.snapshots.publishCommitted(ctx, clientCommit(key, v, fmt.Sprintf("v%d", v))); err != nil {
				t.Fatal(err)
			}
			if v == 2 {
				// 陈旧上限内（含边界）允许命中已确认的 v1；跨过上限后才必须回源取 v2。
				for _, advance := range []time.Duration{0, cfg.CachedMaxStaleness} {
					time.Sleep(advance)
					got, found, err := consumer.ReadSnapshot(ctx, entity.RemoteSnapshotRead{Key: key, Consistency: entity.RemoteReadCached})
					if err != nil || !found || got.StateVersion != 1 {
						t.Fatalf("within staleness bound: version=%d found=%v err=%v, want v1", got.StateVersion, found, err)
					}
				}
				time.Sleep(time.Nanosecond)
			}
			got, found, err := consumer.ReadSnapshot(ctx, entity.RemoteSnapshotRead{Key: key, Consistency: entity.RemoteReadCached})
			if err != nil || !found || got.StateVersion != v {
				t.Fatalf("on-demand Cached read after commit v%d: version=%d found=%v err=%v", v, got.StateVersion, found, err)
			}
		}
		live, err := NewSnapshotClient(cfg, SnapshotClientDeps{ConsumerSID: 2417})
		if err != nil {
			t.Fatal(err)
		}
		if err := live.Start(loop); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = live.Stop(context.Background()) })
		if !live.Stats().PushEnabled || loop.active(SyncTopicSnapshot) != 1 {
			t.Fatalf("on a bus that confirms subscriptions: push=%v snapshot subscriptions=%d", live.Stats().PushEnabled, loop.active(SyncTopicSnapshot))
		}
	})
}
